// Command escapediff compares the Go compiler's escape analysis and inlining
// decisions between two builds, such as a base branch and a pull request.
// It reads `go build -gcflags=-m=2` output, or builds two git revisions
// itself, and reports new heap escapes, functions that are no longer
// inlined, new parameter leaks and lost devirtualizations.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const version = "0.1.0"

// defaultBudget is the compiler's inlining budget when a log doesn't print one.
const defaultBudget = 80

// nearBudget is how close to the budget a grown cost has to be to get a warning.
const nearBudget = 10

// Kinds of diagnostics.
const (
	KindMoved  = "moved"  // moved to heap: x
	KindEscape = "escape" // x escapes to heap
	KindLeak   = "leak"   // leaking param: p
	KindDevirt = "devirt" // devirtualizing x.M to T
	KindCall   = "call"   // inlining call to f
)

// Diag is one compiler diagnostic that escapediff tracks.
type Diag struct {
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Col      int      `json:"col,omitempty"`
	Func     string   `json:"func,omitempty"`
	Kind     string   `json:"kind"`
	Text     string   `json:"text"`
	Detail   string   `json:"detail,omitempty"`
	Why      []string `json:"-"` // -m=2 flow lines, printed by --why
	funcLine int
}

// FuncInfo is one function's inlining decision.
type FuncInfo struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Name      string `json:"name"`
	Inlinable bool   `json:"inlinable"`
	Cost      int    `json:"cost,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Build is everything parsed from one build's output.
type Build struct {
	Diags    []Diag
	Funcs    map[string]*FuncInfo // key: package dir + "|" + name
	Packages int
	Detailed bool // true when the log came from -m=2, with costs and "cannot inline" lines
	Budget   int
	decls    map[string][]decl
	headers  map[string][]string   // -m=2 "X escapes to heap in F:" expressions by position
	why      map[string][]whyBlock // -m=2 flow lines by position and header
}

// whyBlock is the flow lines printed under one -m=2 header. Several escapes can
// share a position, so the subject keeps their blocks apart.
type whyBlock struct {
	subject string
	lines   []string
}

type decl struct {
	line int
	name string
}

var (
	posRe        = regexp.MustCompile(`^(.+?\.go):(\d+)(?::(\d+))?: (.*)$`)
	stampRe      = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z `)
	canCostRe    = regexp.MustCompile(`^can inline (.+?) with cost (\d+) as: `)
	canRe        = regexp.MustCompile(`^can inline (.+)$`)
	cannotRe     = regexp.MustCompile(`^cannot inline (.+?): (.*)$`)
	exceedsRe    = regexp.MustCompile(`cost (\d+) exceeds budget (\d+)`)
	callRe       = regexp.MustCompile(`^inlining call to (.+)$`)
	devirtRe     = regexp.MustCompile(`^devirtualizing (.+) to (.+)$`)
	movedRe      = regexp.MustCompile(`^moved to heap: (.+)$`)
	leakRe       = regexp.MustCompile(`^leaking (param|param content|closure reference): `)
	headerRe     = regexp.MustCompile(`^(.+) escapes to heap in .+:$`)
	closureRe    = regexp.MustCompile(`\.func\d+`)
	typeParamsRe = regexp.MustCompile(`\[[^\[\]]*\]`)
)

// normName removes type arguments, so Map[go.shape.int] and Map[int] are both Map.
func normName(name string) string {
	for {
		n := typeParamsRe.ReplaceAllString(name, "")
		if n == name {
			return n
		}
		name = n
	}
}

// Parse reads compiler output. Lines that aren't diagnostics are ignored.
func Parse(r io.Reader) (*Build, error) {
	b := &Build{Funcs: map[string]*FuncInfo{}, decls: map[string][]decl{}, headers: map[string][]string{}, why: map[string][]whyBlock{}, Budget: defaultBudget}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := stampRe.ReplaceAllString(strings.TrimRight(sc.Text(), "\r"), "")
		if strings.HasPrefix(line, "# ") {
			b.Packages++
			continue
		}
		m := posRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		msg := m[4]
		// -m=2 explains each decision on indented lines and with headers
		// ending in a colon. The plain one-line form follows, so skip these,
		// but keep the full expression from escape headers and the flow lines
		// that --why prints.
		if h := headerRe.FindStringSubmatch(msg); h != nil {
			p := m[1] + ":" + m[2] + ":" + m[3]
			b.headers[p] = append(b.headers[p], h[1])
			b.why[p] = append(b.why[p], whyBlock{subject: h[1]})
			continue
		}
		if strings.HasPrefix(msg, " ") {
			p := m[1] + ":" + m[2] + ":" + m[3]
			blocks := b.why[p]
			if len(blocks) == 0 {
				blocks = append(blocks, whyBlock{})
			}
			blocks[len(blocks)-1].lines = append(blocks[len(blocks)-1].lines, msg)
			b.why[p] = blocks
			continue
		}
		if strings.HasSuffix(msg, ":") {
			// A non-escape header ends the previous block, so its flow lines
			// can't be picked up by an escape at the same position.
			p := m[1] + ":" + m[2] + ":" + m[3]
			b.why[p] = append(b.why[p], whyBlock{})
			continue
		}
		file := strings.TrimPrefix(filepath.ToSlash(m[1]), "./")
		ln, _ := strconv.Atoi(m[2])
		col, _ := strconv.Atoi(m[3])
		b.add(file, ln, col, msg)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	b.attribute()
	return b, nil
}

func (b *Build) add(file string, ln, col int, msg string) {
	dk := func(kind, text string) {
		b.Diags = append(b.Diags, Diag{File: file, Line: ln, Col: col, Kind: kind, Text: text})
	}
	switch {
	case strings.HasPrefix(msg, "can inline "):
		var name string
		cost := 0
		if m := canCostRe.FindStringSubmatch(msg); m != nil {
			name = m[1]
			cost, _ = strconv.Atoi(m[2])
			b.Detailed = true
		} else {
			name = canRe.FindStringSubmatch(msg)[1]
		}
		b.addFunc(file, ln, name, true, cost, "")
	case strings.HasPrefix(msg, "cannot inline "):
		m := cannotRe.FindStringSubmatch(msg)
		if m == nil {
			return
		}
		b.Detailed = true
		cost := 0
		if e := exceedsRe.FindStringSubmatch(m[2]); e != nil {
			cost, _ = strconv.Atoi(e[1])
			b.Budget, _ = strconv.Atoi(e[2])
		}
		b.addFunc(file, ln, m[1], false, cost, m[2])
	case callRe.MatchString(msg):
		dk(KindCall, normName(callRe.FindStringSubmatch(msg)[1]))
	case devirtRe.MatchString(msg):
		dk(KindDevirt, msg)
	case movedRe.MatchString(msg):
		dk(KindMoved, msg)
		d := &b.Diags[len(b.Diags)-1]
		subject := strings.TrimPrefix(msg, "moved to heap: ")
		d.Detail = b.takeHeader(file, ln, col, subject)
		d.Why = b.takeWhy(file, ln, col, subject)
	case strings.HasSuffix(msg, " escapes to heap"):
		dk(KindEscape, msg)
		d := &b.Diags[len(b.Diags)-1]
		subject := strings.TrimSuffix(msg, " escapes to heap")
		d.Detail = b.takeHeader(file, ln, col, subject)
		d.Why = b.takeWhy(file, ln, col, subject)
	case leakRe.MatchString(msg):
		dk(KindLeak, msg)
	}
}

// takeWhy returns and removes the -m=2 flow lines for one escape at a
// position. Several escapes can share a position, so the block whose header
// matches the subject is used.
func (b *Build) takeWhy(file string, ln, col int, subject string) []string {
	for _, p := range []string{file, "./" + file} {
		key := fmt.Sprintf("%s:%d:%d", p, ln, col)
		if col == 0 {
			key = fmt.Sprintf("%s:%d:", p, ln)
		}
		blocks := b.why[key]
		for i, blk := range blocks {
			if blk.subject == "" || !subjectMatches(blk.subject, subject) {
				continue
			}
			b.why[key] = append(blocks[:i:i], blocks[i+1:]...)
			return blk.lines
		}
	}
	return nil
}

// subjectMatches reports whether a -m=2 header describes the plain subject:
// the same text, or a call that starts with it.
func subjectMatches(header, subject string) bool {
	return header == subject || strings.HasPrefix(header, subject+"(") || strings.HasPrefix(header, subject+" ")
}

// takeHeader returns and removes the -m=2 header expression at a position
// that matches the plain subject: the same text, or a call that starts with it.
func (b *Build) takeHeader(file string, ln, col int, subject string) string {
	for _, p := range []string{file, "./" + file} {
		key := fmt.Sprintf("%s:%d:%d", p, ln, col)
		if col == 0 {
			key = fmt.Sprintf("%s:%d:", p, ln)
		}
		hs := b.headers[key]
		for i, h := range hs {
			if subjectMatches(h, subject) {
				b.headers[key] = append(hs[:i:i], hs[i+1:]...)
				if h == subject {
					return ""
				}
				return h + " escapes to heap"
			}
		}
	}
	return ""
}

func (b *Build) addFunc(file string, ln int, name string, inlinable bool, cost int, reason string) {
	n := normName(name)
	if !closureRe.MatchString(n) {
		b.decls[file] = append(b.decls[file], decl{ln, n})
	}
	key := path.Dir(file) + "|" + n
	f := b.Funcs[key]
	if f == nil {
		f = &FuncInfo{File: file, Line: ln, Name: n}
		b.Funcs[key] = f
	}
	// Generic functions print one line per shape and per instantiation.
	// Count the function as inlinable if any of them is.
	if inlinable && !f.Inlinable {
		f.Inlinable, f.Reason, f.Cost = true, "", cost
	} else if inlinable == f.Inlinable && cost > f.Cost {
		f.Cost = cost
	}
	if !f.Inlinable && reason != "" && f.Reason == "" {
		f.Reason = reason
	}
}

// attribute sets each diagnostic's enclosing function from the "can inline"
// and "cannot inline" lines, which sit on each function's first line. Only
// -m=2 prints a line for every function, so -m=1 output is matched by file.
func (b *Build) attribute() {
	if !b.Detailed {
		return
	}
	for f, ds := range b.decls {
		sort.Slice(ds, func(i, j int) bool { return ds[i].line < ds[j].line })
		b.decls[f] = ds
	}
	for i := range b.Diags {
		d := &b.Diags[i]
		ds := b.decls[d.File]
		k := sort.Search(len(ds), func(j int) bool { return ds[j].line > d.Line }) - 1
		if k >= 0 {
			d.Func, d.funcLine = ds[k].name, ds[k].line
		}
	}
}

// Outside reports whether a file is outside the packages that were built:
// an absolute path, such as Go's own source or the module cache. These show
// up when a generic function from another package is compiled into this one.
func Outside(file string) bool {
	return path.IsAbs(file) || filepath.IsAbs(file) || filepath.VolumeName(file) != "" ||
		strings.Contains(file, "/pkg/mod/")
}

// HeapCount is the number of heap escapes and moves in a build.
func (b *Build) HeapCount() int {
	n := 0
	for _, d := range b.Diags {
		if (d.Kind == KindMoved || d.Kind == KindEscape) && !Outside(d.File) {
			n++
		}
	}
	return n
}

// InlinableCount is the number of functions the compiler can inline.
func (b *Build) InlinableCount() int {
	n := 0
	for _, f := range b.Funcs {
		if f.Inlinable && !Outside(f.File) {
			n++
		}
	}
	return n
}

// LostInline is a function that was inlinable in base and isn't in head.
type LostInline struct {
	FuncInfo
	BaseCost  int      `json:"baseCost,omitempty"`
	CallSites int      `json:"callSitesInBase"`
	Removed   bool     `json:"maybeRemoved,omitempty"`
	Grown     []Callee `json:"grownCallees,omitempty"`
}

// Callee is a function inlined into another whose inline cost grew. Its
// body is copied into each caller, so it adds to the caller's cost.
type Callee struct {
	Name     string `json:"name"`
	BaseCost int    `json:"baseCost"`
	Cost     int    `json:"cost"`
}

// CostChange is an inlinable function whose cost grew close to the budget.
type CostChange struct {
	FuncInfo
	BaseCost int `json:"baseCost"`
	Budget   int `json:"budget"`
}

// Result is the comparison of two builds.
type Result struct {
	NewHeap       []Diag       `json:"newHeap"`
	NewLeaks      []Diag       `json:"newLeaks"`
	LostDevirt    []Diag       `json:"lostDevirtualization"`
	LostInline    []LostInline `json:"noLongerInlined"`
	NearBudget    []CostChange `json:"nearBudget"`
	GoneHeap      []Diag       `json:"goneHeap"`
	GoneLeaks     []Diag       `json:"goneLeaks"`
	NewInline     []FuncInfo   `json:"nowInlined"`
	BaseHeap      int          `json:"baseHeap"`
	HeadHeap      int          `json:"headHeap"`
	BaseInlinable int          `json:"baseInlinable"`
	HeadInlinable int          `json:"headInlinable"`
	Packages      int          `json:"packages"`
	Outside       int          `json:"hiddenOutside,omitempty"`
	ByFile        bool         `json:"matchedByFileOnly,omitempty"`
}

// Regressions counts the changes that --fail reports.
func (r *Result) Regressions() int {
	return len(r.NewHeap) + len(r.NewLeaks) + len(r.LostDevirt) + len(r.LostInline)
}

func diagKey(d Diag) string {
	return d.Kind + "|" + d.File + "|" + d.Func + "|" + d.Text
}

// matchDiags pairs base and head diagnostics with the same key, preferring
// pairs at the same offset from the start of their function, and returns
// the ones left over on each side.
func matchDiags(base, head []Diag) (added, removed []Diag) {
	group := func(ds []Diag) map[string][]Diag {
		m := map[string][]Diag{}
		for _, d := range ds {
			m[diagKey(d)] = append(m[diagKey(d)], d)
		}
		return m
	}
	bg, hg := group(base), group(head)
	keys := map[string]bool{}
	for k := range bg {
		keys[k] = true
	}
	for k := range hg {
		keys[k] = true
	}
	for k := range keys {
		bs, hs := bg[k], hg[k]
		type pair struct{ i, j, dist int }
		var pairs []pair
		for i, b := range bs {
			for j, h := range hs {
				dist := (h.Line - h.funcLine) - (b.Line - b.funcLine)
				if dist < 0 {
					dist = -dist
				}
				pairs = append(pairs, pair{i, j, dist*1000 + abs(h.Col-b.Col)})
			}
		}
		sort.Slice(pairs, func(a, c int) bool {
			if pairs[a].dist != pairs[c].dist {
				return pairs[a].dist < pairs[c].dist
			}
			if pairs[a].i != pairs[c].i {
				return pairs[a].i < pairs[c].i
			}
			return pairs[a].j < pairs[c].j
		})
		usedB, usedH := map[int]bool{}, map[int]bool{}
		for _, p := range pairs {
			if !usedB[p.i] && !usedH[p.j] {
				usedB[p.i], usedH[p.j] = true, true
			}
		}
		for i, b := range bs {
			if !usedB[i] {
				removed = append(removed, b)
			}
		}
		for j, h := range hs {
			if !usedH[j] {
				added = append(added, h)
			}
		}
	}
	sortDiags(added)
	sortDiags(removed)
	return added, removed
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func sortDiags(ds []Diag) {
	sort.Slice(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Col != b.Col {
			return a.Col < b.Col
		}
		return a.Text < b.Text
	})
}

// Compare finds what changed from base to head. Unless all is set, changes
// in files outside the built packages are counted in Outside, not listed.
func Compare(base, head *Build, all bool) *Result {
	r := &Result{
		BaseHeap: base.HeapCount(), HeadHeap: head.HeapCount(),
		BaseInlinable: base.InlinableCount(), HeadInlinable: head.InlinableCount(),
		Packages: head.Packages,
	}
	// Function names only line up when both logs have them.
	if !base.Detailed || !head.Detailed {
		r.ByFile = true
		for _, b := range []*Build{base, head} {
			for i := range b.Diags {
				b.Diags[i].Func, b.Diags[i].funcLine = "", 0
			}
		}
	}
	split := func(ds []Diag, kinds ...string) []Diag {
		var out []Diag
		for _, d := range ds {
			for _, k := range kinds {
				if d.Kind == k {
					out = append(out, d)
				}
			}
		}
		return out
	}
	r.NewHeap, r.GoneHeap = matchDiags(split(base.Diags, KindMoved, KindEscape), split(head.Diags, KindMoved, KindEscape))
	r.NewLeaks, r.GoneLeaks = matchDiags(split(base.Diags, KindLeak), split(head.Diags, KindLeak))
	_, r.LostDevirt = matchDiags(split(base.Diags, KindDevirt), split(head.Diags, KindDevirt))

	budget := head.Budget
	for key, bf := range base.Funcs {
		hf := head.Funcs[key]
		switch {
		case bf.Inlinable && hf == nil:
			// -m=2 prints every function, so a missing one was removed or renamed.
			if head.Detailed {
				continue
			}
			r.LostInline = append(r.LostInline, LostInline{FuncInfo: *bf, BaseCost: bf.Cost, CallSites: callSites(base, bf), Removed: true})
		case bf.Inlinable && !hf.Inlinable:
			r.LostInline = append(r.LostInline, LostInline{FuncInfo: *hf, BaseCost: bf.Cost, CallSites: callSites(base, bf), Grown: grownCallees(base, head, hf)})
		case !bf.Inlinable && hf != nil && hf.Inlinable:
			r.NewInline = append(r.NewInline, *hf)
		case bf.Inlinable && hf.Inlinable && hf.Cost > bf.Cost && hf.Cost >= budget-nearBudget:
			r.NearBudget = append(r.NearBudget, CostChange{FuncInfo: *hf, BaseCost: bf.Cost, Budget: budget})
		}
	}
	if !all {
		r.hideOutside()
	}
	sort.Slice(r.LostInline, func(i, j int) bool { return funcLess(r.LostInline[i].FuncInfo, r.LostInline[j].FuncInfo) })
	sort.Slice(r.NewInline, func(i, j int) bool { return funcLess(r.NewInline[i], r.NewInline[j]) })
	sort.Slice(r.NearBudget, func(i, j int) bool { return funcLess(r.NearBudget[i].FuncInfo, r.NearBudget[j].FuncInfo) })
	return r
}

func (r *Result) hideOutside() {
	diags := func(ds []Diag) []Diag {
		var keep []Diag
		for _, d := range ds {
			if Outside(d.File) {
				r.Outside++
			} else {
				keep = append(keep, d)
			}
		}
		return keep
	}
	r.NewHeap, r.GoneHeap = diags(r.NewHeap), diags(r.GoneHeap)
	r.NewLeaks, r.GoneLeaks = diags(r.NewLeaks), diags(r.GoneLeaks)
	r.LostDevirt = diags(r.LostDevirt)
	var lost []LostInline
	for _, f := range r.LostInline {
		if Outside(f.File) {
			r.Outside++
		} else {
			lost = append(lost, f)
		}
	}
	r.LostInline = lost
	var now []FuncInfo
	for _, f := range r.NewInline {
		if Outside(f.File) {
			r.Outside++
		} else {
			now = append(now, f)
		}
	}
	r.NewInline = now
	var near []CostChange
	for _, f := range r.NearBudget {
		if Outside(f.File) {
			r.Outside++
		} else {
			near = append(near, f)
		}
	}
	r.NearBudget = near
}

// grownCallees lists functions from f's package that head inlines into f
// and whose inline cost grew since base.
func grownCallees(base, head *Build, f *FuncInfo) []Callee {
	dir := path.Dir(f.File)
	seen := map[string]bool{}
	var out []Callee
	for _, d := range head.Diags {
		if d.Kind != KindCall || d.Func != f.Name || path.Dir(d.File) != dir || seen[d.Text] {
			continue
		}
		seen[d.Text] = true
		key := dir + "|" + d.Text
		bc, hc := base.Funcs[key], head.Funcs[key]
		if bc != nil && hc != nil && bc.Inlinable && hc.Inlinable && hc.Cost > bc.Cost {
			out = append(out, Callee{Name: d.Text, BaseCost: bc.Cost, Cost: hc.Cost})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func funcLess(a, b FuncInfo) bool {
	if a.File != b.File {
		return a.File < b.File
	}
	return a.Line < b.Line
}

// callSites counts calls to f that base inlined inside f's package.
func callSites(b *Build, f *FuncInfo) int {
	n := 0
	dir := path.Dir(f.File)
	for _, d := range b.Diags {
		if d.Kind == KindCall && d.Text == f.Name && path.Dir(d.File) == dir {
			n++
		}
	}
	return n
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func pos(file string, line, col int) string {
	if col > 0 {
		return fmt.Sprintf("%s:%d:%d", file, line, col)
	}
	return fmt.Sprintf("%s:%d", file, line)
}

func writeDiags(w io.Writer, ds []Diag, why bool) {
	width := 0
	for _, d := range ds {
		if n := len(pos(d.File, d.Line, d.Col)); n > width {
			width = n
		}
	}
	for _, d := range ds {
		fn := ""
		if d.Func != "" {
			fn = "  (in " + d.Func + ")"
		}
		text := d.Text
		if d.Detail != "" {
			text = d.Detail
		}
		fmt.Fprintf(w, "  %-*s  %s%s\n", width, pos(d.File, d.Line, d.Col), text, fn)
		if why {
			for _, line := range d.Why {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
	}
}

// summaryParts counts the regressions and improvements shown at the top of a
// report.
func summaryParts(r *Result) ([]string, int) {
	var parts []string
	if n := len(r.NewHeap); n > 0 {
		parts = append(parts, plural(n, "new heap escape", "new heap escapes"))
	}
	if n := len(r.LostInline); n > 0 {
		parts = append(parts, plural(n, "function no longer inlined", "functions no longer inlined"))
	}
	if n := len(r.NewLeaks); n > 0 {
		parts = append(parts, plural(n, "new parameter leak", "new parameter leaks"))
	}
	if n := len(r.LostDevirt); n > 0 {
		parts = append(parts, plural(n, "call no longer devirtualized", "calls no longer devirtualized"))
	}
	improved := len(r.GoneHeap) + len(r.GoneLeaks) + len(r.NewInline)
	return parts, improved
}

// WriteText prints the result for people. When why is set, each new heap
// escape or move is followed by the compiler's -m=2 flow lines for it.
func WriteText(w io.Writer, r *Result, why bool) {
	if r.ByFile {
		fmt.Fprintln(w, "Note: a log has no -m=2 detail, so changes are matched by file and function names aren't shown.")
	}
	parts, improved := summaryParts(r)
	if len(parts) == 0 && improved == 0 && len(r.NearBudget) == 0 {
		fmt.Fprintf(w, "No changes in heap escapes, parameter leaks, inlining or devirtualization (%s, %s).\n",
			plural(r.HeadHeap, "heap escape", "heap escapes"), plural(r.HeadInlinable, "inlinable function", "inlinable functions"))
		writeOutside(w, r)
		return
	}
	if len(parts) == 0 {
		parts = append(parts, "no regressions")
	}
	if improved > 0 {
		parts = append(parts, plural(improved, "improvement", "improvements"))
	}
	fmt.Fprintln(w, strings.Join(parts, ", "))
	fmt.Fprintf(w, "Heap escapes: %d -> %d. Inlinable functions: %d -> %d.\n", r.BaseHeap, r.HeadHeap, r.BaseInlinable, r.HeadInlinable)

	if len(r.NewHeap) > 0 {
		fmt.Fprintln(w, "\nNew heap escapes:")
		writeDiags(w, r.NewHeap, why)
	}
	if len(r.LostInline) > 0 {
		fmt.Fprintln(w, "\nNo longer inlined:")
		for _, f := range r.LostInline {
			why := f.Reason
			if f.Removed {
				why = "not in head's log (removed, renamed or not inlinable)"
			}
			was := ""
			if f.BaseCost > 0 {
				was = fmt.Sprintf(" (base cost %d)", f.BaseCost)
			}
			fmt.Fprintf(w, "  %s  %s: %s%s\n", pos(f.File, f.Line, 0), f.Name, why, was)
			if f.CallSites > 0 {
				fmt.Fprintf(w, "      %s inlined in base\n", plural(f.CallSites, "call site was", "call sites were"))
			}
			for _, c := range f.Grown {
				fmt.Fprintf(w, "      inlines %s, whose cost grew %d -> %d\n", c.Name, c.BaseCost, c.Cost)
			}
		}
	}
	if len(r.NewLeaks) > 0 {
		fmt.Fprintln(w, "\nNew parameter leaks (callers may now allocate):")
		writeDiags(w, r.NewLeaks, false)
	}
	if len(r.LostDevirt) > 0 {
		fmt.Fprintln(w, "\nNo longer devirtualized (base positions):")
		writeDiags(w, r.LostDevirt, false)
	}
	if len(r.NearBudget) > 0 {
		fmt.Fprintf(w, "\nStill inlined, but close to the budget of %d:\n", r.NearBudget[0].Budget)
		for _, f := range r.NearBudget {
			fmt.Fprintf(w, "  %s  %s: cost %d -> %d\n", pos(f.File, f.Line, 0), f.Name, f.BaseCost, f.Cost)
		}
	}
	if len(r.GoneHeap) > 0 {
		fmt.Fprintln(w, "\nHeap escapes gone (base positions):")
		writeDiags(w, r.GoneHeap, false)
	}
	if len(r.GoneLeaks) > 0 {
		fmt.Fprintln(w, "\nParameter leaks gone (base positions):")
		writeDiags(w, r.GoneLeaks, false)
	}
	if r.Outside > 0 {
		fmt.Fprintln(w)
		writeOutside(w, r)
	}
	if len(r.NewInline) > 0 {
		fmt.Fprintln(w, "\nNow inlined:")
		for _, f := range r.NewInline {
			cost := ""
			if f.Cost > 0 {
				cost = fmt.Sprintf(" (cost %d)", f.Cost)
			}
			fmt.Fprintf(w, "  %s  %s%s\n", pos(f.File, f.Line, 0), f.Name, cost)
		}
	}
}

// mdCell escapes text for a Markdown table cell.
func mdCell(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"|", `\|`,
		"\n", "<br>",
		"\r", "",
	).Replace(s)
}

// mdCode formats s as an inline code span that survives a Markdown table,
// choosing a delimiter longer than any backtick run in s.
func mdCode(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	delim := "`"
	for strings.Contains(s, delim) {
		delim += "`"
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		return delim + " " + s + " " + delim
	}
	return delim + s + delim
}

func writeDiagTable(w io.Writer, ds []Diag) {
	fmt.Fprintln(w, "| Position | Change | In |")
	fmt.Fprintln(w, "| --- | --- | --- |")
	for _, d := range ds {
		fn := "—"
		if d.Func != "" {
			fn = mdCode(d.Func)
		}
		text := d.Text
		if d.Detail != "" {
			text = d.Detail
		}
		fmt.Fprintf(w, "| %s | %s | %s |\n", mdCode(pos(d.File, d.Line, d.Col)), mdCode(text), fn)
	}
}

// WriteMarkdown prints the result as Markdown tables, one per section, so CI
// can post it as a pull request comment.
func WriteMarkdown(w io.Writer, r *Result) {
	if r.ByFile {
		fmt.Fprintln(w, "> A log has no -m=2 detail, so changes are matched by file and function names aren't shown.")
		fmt.Fprintln(w)
	}

	parts, improved := summaryParts(r)
	if len(parts) == 0 && improved == 0 && len(r.NearBudget) == 0 {
		fmt.Fprintf(w, "No changes in heap escapes, parameter leaks, inlining or devirtualization (%s, %s).\n\n",
			plural(r.HeadHeap, "heap escape", "heap escapes"), plural(r.HeadInlinable, "inlinable function", "inlinable functions"))
		writeOutside(w, r)
		return
	}
	if len(parts) == 0 {
		parts = append(parts, "no regressions")
	}
	if improved > 0 {
		parts = append(parts, plural(improved, "improvement", "improvements"))
	}
	fmt.Fprintf(w, "**%s**\n\n", mdCell(strings.Join(parts, ", ")))
	fmt.Fprintf(w, "Heap escapes: %d -> %d. Inlinable functions: %d -> %d.\n", r.BaseHeap, r.HeadHeap, r.BaseInlinable, r.HeadInlinable)

	if len(r.NewHeap) > 0 {
		fmt.Fprint(w, "\n### New heap escapes\n\n")
		writeDiagTable(w, r.NewHeap)
	}
	if len(r.LostInline) > 0 {
		fmt.Fprint(w, "\n### No longer inlined\n\n")
		fmt.Fprintln(w, "| Position | Function | Reason | Detail |")
		fmt.Fprintln(w, "| --- | --- | --- | --- |")
		for _, f := range r.LostInline {
			why := f.Reason
			if f.Removed {
				why = "not in head's log (removed, renamed or not inlinable)"
			}
			var detail []string
			if f.BaseCost > 0 {
				detail = append(detail, fmt.Sprintf("base cost %d", f.BaseCost))
			}
			if f.CallSites > 0 {
				was := "call sites were"
				if f.CallSites == 1 {
					was = "call site was"
				}
				detail = append(detail, fmt.Sprintf("%d %s inlined in base", f.CallSites, was))
			}
			for _, c := range f.Grown {
				detail = append(detail, fmt.Sprintf("inlines %s, whose cost grew %d -> %d", mdCode(c.Name), c.BaseCost, c.Cost))
			}
			if len(detail) == 0 {
				detail = append(detail, "—")
			}
			fmt.Fprintf(w, "| %s | %s | %s | %s |\n", mdCode(pos(f.File, f.Line, 0)), mdCode(f.Name), mdCode(why), strings.Join(detail, "<br>"))
		}
	}
	if len(r.NewLeaks) > 0 {
		fmt.Fprint(w, "\n### New parameter leaks (callers may now allocate)\n\n")
		writeDiagTable(w, r.NewLeaks)
	}
	if len(r.LostDevirt) > 0 {
		fmt.Fprint(w, "\n### No longer devirtualized (base positions)\n\n")
		writeDiagTable(w, r.LostDevirt)
	}
	if len(r.NearBudget) > 0 {
		fmt.Fprintf(w, "\n### Still inlined, but close to the budget of %d\n\n", r.NearBudget[0].Budget)
		fmt.Fprintln(w, "| Position | Function | Cost |")
		fmt.Fprintln(w, "| --- | --- | --- |")
		for _, f := range r.NearBudget {
			fmt.Fprintf(w, "| %s | %s | %s |\n", mdCode(pos(f.File, f.Line, 0)), mdCode(f.Name), mdCode(fmt.Sprintf("%d -> %d", f.BaseCost, f.Cost)))
		}
	}
	if len(r.GoneHeap) > 0 {
		fmt.Fprint(w, "\n### Heap escapes gone (base positions)\n\n")
		writeDiagTable(w, r.GoneHeap)
	}
	if len(r.GoneLeaks) > 0 {
		fmt.Fprint(w, "\n### Parameter leaks gone (base positions)\n\n")
		writeDiagTable(w, r.GoneLeaks)
	}
	if r.Outside > 0 {
		fmt.Fprintln(w)
		writeOutside(w, r)
	}
	if len(r.NewInline) > 0 {
		fmt.Fprint(w, "\n### Now inlined\n\n")
		fmt.Fprintln(w, "| Position | Function | Cost |")
		fmt.Fprintln(w, "| --- | --- | --- |")
		for _, f := range r.NewInline {
			cost := "—"
			if f.Cost > 0 {
				cost = mdCode(strconv.Itoa(f.Cost))
			}
			fmt.Fprintf(w, "| %s | %s | %s |\n", mdCode(pos(f.File, f.Line, 0)), mdCode(f.Name), cost)
		}
	}
}

func writeOutside(w io.Writer, r *Result) {
	if r.Outside > 0 {
		fmt.Fprintf(w, "Not listed: %s in files outside these packages, such as generic code from the standard library. Use --all to list them.\n",
			plural(r.Outside, "change", "changes"))
	}
}

func parseFile(name string) (*Build, error) {
	var r io.Reader = os.Stdin
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	return Parse(r)
}

const usage = `escapediff compares Go escape analysis and inlining between two builds.

Usage:
  escapediff compare [--why] [--json|--markdown] [--fail] [--all] BASE.log HEAD.log
  escapediff git [--head REF] [--save DIR] [--why] [--json|--markdown] [--fail] [--all] BASE [PACKAGES...]
  escapediff version

compare reads two saved outputs of: go build -gcflags=-m=2 ./... 2> FILE
git builds BASE (a commit, branch or tag) and the working tree, or --head REF,
with -gcflags=-m=2 and compares them. PACKAGES defaults to ./...

--fail exits 1 when there are new heap escapes, new parameter leaks,
functions no longer inlined or calls no longer devirtualized.
--all also lists changes in files outside the built packages, such as
generic functions from the standard library compiled into them.
--why prints the compiler's flow lines under each new heap escape or move in
the text report. It needs -m=2 logs.
--markdown prints one Markdown table per section, for a pull request comment.
Exit code 2 means bad usage, a failed build or an unreadable file.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "escapediff", version)
		return 0
	case "help", "--help", "-h", "-help":
		fmt.Fprint(stdout, usage)
		return 0
	case "compare", "git":
	default:
		fmt.Fprintf(stderr, "escapediff: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	asMarkdown := fs.Bool("markdown", false, "print Markdown tables for a pull request comment")
	fail := fs.Bool("fail", false, "exit 1 on regressions")
	head := fs.String("head", "", "git: build this ref instead of the working tree")
	save := fs.String("save", "", "git: write base.log and head.log to this directory")
	all := fs.Bool("all", false, "also list changes in files outside the built packages")
	why := fs.Bool("why", false, "print the compiler's flow lines under each new heap escape or move")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *asJSON && *asMarkdown {
		fmt.Fprintln(stderr, "escapediff: choose one of --json and --markdown")
		return 2
	}
	if *why && (*asJSON || *asMarkdown) {
		fmt.Fprintln(stderr, "escapediff: --why needs the text report, so it can't be combined with --json or --markdown")
		return 2
	}
	var base, headB *Build
	var err error
	switch args[0] {
	case "compare":
		if fs.NArg() != 2 {
			fmt.Fprintf(stderr, "escapediff: compare needs BASE and HEAD files\n\n%s", usage)
			return 2
		}
		if base, err = parseFile(fs.Arg(0)); err == nil {
			headB, err = parseFile(fs.Arg(1))
		}
	case "git":
		if fs.NArg() < 1 {
			fmt.Fprintf(stderr, "escapediff: git needs a BASE ref\n\n%s", usage)
			return 2
		}
		base, headB, err = gitBuilds(fs.Arg(0), *head, fs.Args()[1:], *save)
	}
	if err != nil {
		fmt.Fprintf(stderr, "escapediff: %v\n", err)
		return 2
	}
	blank := func(b *Build) bool { return len(b.Diags) == 0 && len(b.Funcs) == 0 }
	// A saved log with nothing in it is usually a failed build, and comparing with it
	// would report every escape as an improvement. In git mode the build itself succeeded.
	if args[0] == "compare" && (blank(base) || blank(headB)) {
		fmt.Fprintln(stderr, "escapediff: a log has no compiler diagnostics, so it may be from a failed build. Check that build, then save stderr from go build -gcflags=-m=2.")
		return 2
	}
	if blank(base) && blank(headB) && base.Packages == 0 && headB.Packages == 0 {
		fmt.Fprintln(stderr, "escapediff: neither log has compiler diagnostics. Build with -gcflags=-m=2 and save stderr.")
		return 2
	}
	r := Compare(base, headB, *all)
	switch {
	case *asJSON:
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(r)
	case *asMarkdown:
		WriteMarkdown(stdout, r)
	default:
		WriteText(stdout, r, *why)
	}
	if *fail && r.Regressions() > 0 {
		return 1
	}
	return 0
}

// gitBuilds builds base and head with -gcflags=-m=2 and parses the output.
// Revisions are unpacked with git archive into a temporary directory, so
// the repository and working tree aren't changed.
func gitBuilds(baseRef, headRef string, pkgs []string, save string) (*Build, *Build, error) {
	if len(pkgs) == 0 {
		pkgs = []string{"./..."}
	}
	prefix, err := gitOut("rev-parse", "--show-prefix")
	if err != nil {
		return nil, nil, err
	}
	prefix = strings.TrimSpace(prefix)
	top, err := gitOut("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, nil, err
	}
	top = strings.TrimSpace(top)
	tmp, err := os.MkdirTemp("", "escapediff")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)

	build := func(ref, name string) (*Build, error) {
		dir := "."
		if ref != "" {
			root := filepath.Join(tmp, name)
			if err := extract(top, ref, root); err != nil {
				return nil, err
			}
			dir = filepath.Join(root, filepath.FromSlash(prefix))
		}
		out, err := goBuild(dir, pkgs)
		if err != nil {
			label := ref
			if label == "" {
				label = "the working tree"
			}
			return nil, fmt.Errorf("building %s failed: %v\n%s", label, err, tail(out, 20))
		}
		if save != "" {
			if err := os.MkdirAll(save, 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(save, name+".log"), out, 0o644); err != nil {
				return nil, err
			}
		}
		return Parse(bytes.NewReader(out))
	}
	base, err := build(baseRef, "base")
	if err != nil {
		return nil, nil, err
	}
	head, err := build(headRef, "head")
	if err != nil {
		return nil, nil, err
	}
	return base, head, nil
}

func gitOut(args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func goBuild(dir string, pkgs []string) ([]byte, error) {
	// -o os.DevNull compiles every package, main or not, and writes nothing.
	args := append([]string{"build", "-gcflags=-m=2", "-o", os.DevNull}, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func tail(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	var keep []string
	for _, l := range lines {
		// Skip the diagnostics themselves and keep the errors.
		if m := posRe.FindStringSubmatch(l); m != nil && isDiag(m[4]) {
			continue
		}
		keep = append(keep, l)
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, "\n")
}

func isDiag(msg string) bool {
	if strings.HasPrefix(msg, " ") || strings.HasSuffix(msg, ":") {
		return true
	}
	for _, p := range []string{"can inline", "cannot inline", "inlining call", "devirtualizing", "moved to heap", "leaking ", "parameter "} {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return strings.HasSuffix(msg, "escape") || strings.HasSuffix(msg, "escapes to heap")
}

// extract writes the whole tree of ref into dir with git archive, run from
// the repository root top. Symlinks are skipped so nothing can be written
// outside dir.
func extract(top, ref, dir string) error {
	cmd := exec.Command("git", "-C", top, "archive", "--format=tar", ref)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	terr := untar(out, dir)
	io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive %s: %v: %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	return terr
}

func untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path in archive: %s", h.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
}

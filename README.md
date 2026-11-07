# escapediff: Go escape analysis and inlining diff for pull requests

[![CI](https://github.com/GillMacedo/escapediff/actions/workflows/ci.yml/badge.svg)](https://github.com/GillMacedo/escapediff/actions/workflows/ci.yml) [![MIT license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**A Claude Code skill and Go CLI for Go developers who want to know whether a change adds heap allocations or stops a function from being inlined. It builds your base branch and your change with `go build -gcflags=-m=2` and lists only what changed: new heap escapes, functions no longer inlined with the compiler's reason and cost, new parameter leaks, and calls no longer devirtualized.**

`-gcflags=-m` output is thousands of lines for a real package, and the line numbers move with every edit, so a plain `diff` of two runs is mostly noise. `escapediff` matches each decision by file, function and text, so moved lines don't count as changes. The skill then has Claude read each new escape in the code, connect it to its cause, and say whether it's on a hot path.

## What you can ask Claude

- "Does my branch add any heap allocations compared to main?"
- "Is `parseHeader` still inlined after my change?"
- "This benchmark got slower after the last commit. Did escape analysis change?"
- "Review this Go PR for performance regressions."

## Install

As a Claude Code plugin:

    claude plugin marketplace add GillMacedo/escapediff
    claude plugin install escapediff@escapediff-marketplace

Or as a CLI with Go 1.22 or newer and git. It has no dependencies. From inside your module:

    go run github.com/GillMacedo/escapediff/plugin/scripts@latest git main ./...

## Example

`examples/shop` holds three versions of a made-up order service. `after` makes typical pull request changes: it adds negative rounding to a helper, stores the last order in a package variable for a debug endpoint, wraps a parse error, and moves an encoder into a package variable. The logs are real `go build -gcflags=-m=2` output from Go 1.27.3. From a clone of this repo:

    $ go run ./plugin/scripts compare examples/logs/shop-before.log examples/logs/shop-after.log
    3 new heap escapes, 2 functions no longer inlined, 4 new parameter leaks, 1 call no longer devirtualized, 1 improvement
    Heap escapes: 2 -> 4. Inlinable functions: 8 -> 6.

    New heap escapes:
      orders/orders.go:68:16  "ord-" + ~r0 escapes to heap  (in FormatID)
      orders/orders.go:75:23  &errors.errorString{...} escapes to heap  (in ParseQuantity)
      orders/orders.go:75:43  s escapes to heap  (in ParseQuantity)

    No longer inlined:
      orders/orders.go:55  (*Order).Total: function too complex: cost 116 exceeds budget 80 (base cost 53)
          2 call sites were inlined in base
          inlines (*Order).Subtotal, whose cost grew 30 -> 60
          inlines roundCents, whose cost grew 6 -> 36
      orders/orders.go:72  ParseQuantity: function too complex: cost 173 exceeds budget 80 (base cost 72)

    New parameter leaks (callers may now allocate):
      orders/encode.go:22:14  leaking param: o  (in Receipt)
      orders/encode.go:28:12  leaking param content: orders  (in Batch)
      orders/orders.go:55:7   leaking param: o  (in (*Order).Total)
      orders/orders.go:72:20  leaking param: s  (in ParseQuantity)

    No longer devirtualized (base positions):
      orders/encode.go:22:17  devirtualizing e.Encode to PlainEncoder  (in Receipt)

    Still inlined, but close to the budget of 80:
      orders/encode.go:28  Batch: cost 69 -> 73
      orders/orders.go:67  FormatID: cost 65 -> 72

    Heap escapes gone (base positions):
      orders/orders.go:50:31  id escapes to heap  (in FormatID)

Two findings are hard to spot by reading the diff. `roundCents` is still inlined, but its cost went from 6 to 36, and it's copied into `Total` both directly and through `Subtotal`, so `Total` is now too big to inline at its two call sites. And the one line `lastOrder = o` makes `Total` leak `o`, so `Receipt` and `Batch`, which call it, now leak their arguments too.

The `comments` version only adds comments and renames a local variable, which moves lines in both files. It stays quiet:

    $ go run ./plugin/scripts compare examples/logs/shop-before.log examples/logs/shop-comments.log
    No changes in heap escapes, parameter leaks, inlining or devirtualization (2 heap escapes, 8 inlinable functions).

### Real projects

zerolog commit `14dcf38`, "Make AppendKey in json inlinable". The tool confirms the goal, and also shows that the same commit replaced an in-place write with an `append` that escapes:

    $ go run ./plugin/scripts compare examples/logs/real/zerolog-14dcf38-base.log examples/logs/real/zerolog-14dcf38-head.log
    1 new heap escape, 1 improvement
    Heap escapes: 131 -> 132. Inlinable functions: 23 -> 24.

    New heap escapes:
      internal/json/types.go:383:16  append(dst, byte(44)) escapes to heap  (in Encoder.AppendObjectData)

    Now inlined:
      internal/json/base.go:6  Encoder.AppendKey (cost 78)

google/uuid commit `85223fa`, "Reduce custom error allocation", changed `&invalidLengthError{...}` to `invalidLengthError{...}`. The compiler reports both forms as escaping, so the count stays at 39:

    $ go run ./plugin/scripts compare examples/logs/real/uuid-85223fa-base.log examples/logs/real/uuid-85223fa-head.log
    3 new heap escapes, 3 improvements
    Heap escapes: 39 -> 39. Inlinable functions: 29 -> 29.

    New heap escapes:
      uuid.go:41:51   err.len escapes to heap  (in invalidLengthError.Error)
      uuid.go:77:34   invalidLengthError{...} escapes to heap  (in Parse)
      uuid.go:121:34  invalidLengthError{...} escapes to heap  (in ParseBytes)

    Heap escapes gone (base positions):
      uuid.go:41:51   err.len escapes to heap  (in (*invalidLengthError).Error)
      uuid.go:77:16   &invalidLengthError{...} escapes to heap  (in Parse)
      uuid.go:121:16  &invalidLengthError{...} escapes to heap  (in ParseBytes)

chi commit `aba8618` added a panic message to `Mount`. That's a new escape on a path that only runs on misuse, which is the kind of finding the skill tells Claude to call harmless:

    $ go run ./plugin/scripts compare examples/logs/real/chi-aba8618-base.log examples/logs/real/chi-aba8618-head.log
    2 new heap escapes
    Heap escapes: 353 -> 355. Inlinable functions: 167 -> 167.

    New heap escapes:
      mux.go:313:20  fmt.Sprintf("chi: attempting to Mount() a router onto itself on '%s'", ... argument...) escapes to heap  (in (*Mux).Mount)
      mux.go:313:80  pattern escapes to heap  (in (*Mux).Mount)

chi commit `cf537d4` changed the throttle middleware's logic without changing any compiler decision:

    $ go run ./plugin/scripts compare examples/logs/real/chi-cf537d4-base.log examples/logs/real/chi-cf537d4-head.log
    No changes in heap escapes, parameter leaks, inlining or devirtualization (310 heap escapes, 154 inlinable functions).

When a change starts using a generic function from another package, such as `slices.Concat` in chi commit `be60b2e`, the compiler also reports on that function's body under Go's own source path. Those are counted, not listed, unless you pass `--all`:

    $ go run ./plugin/scripts compare examples/logs/real/chi-be60b2e-base.log examples/logs/real/chi-be60b2e-head.log | tail -n 1
    Not listed: 10 changes in files outside these packages, such as generic code from the standard library. Use --all to list them.

## Usage

    escapediff git [--head REF] [--save DIR] [--why] [--json|--markdown] [--fail] [--all] BASE [PACKAGES...]
    escapediff compare [--why] [--json|--markdown] [--fail] [--all] BASE.log HEAD.log

- `git` builds `BASE` and your working tree, uncommitted edits included, with `go build -gcflags=-m=2 -o /dev/null`, then compares them. `--head REF` builds a commit instead of the working tree. Revisions are unpacked with `git archive` into a temporary directory, so your checkout isn't touched. `PACKAGES` defaults to `./...`. Run it from inside the module.
- `compare` reads two saved outputs of `go build -gcflags=-m=2 ./... 2> FILE`. Both should come from the same Go version. `-m=1` output works too, but then changes are matched by file only, function names aren't shown, and a function missing from head's log can't be told apart from one that was removed.
- `--save DIR` writes `base.log` and `head.log`, which hold the compiler's full reasoning (`flow:` lines) for each escape.
- `--why` prints those `flow:` lines under each new heap escape or move in the text report. It needs `-m=2` logs, and it can't be combined with `--json` or `--markdown`.
- `--fail` exits 1 when there are new heap escapes, new parameter leaks, functions no longer inlined, or calls no longer devirtualized. "Close to the budget" doesn't fail. Exit code 2 means bad usage, a failed build, or a file it couldn't read.
- `--json` prints the full result.
- `--markdown` prints one Markdown table per section, so CI can post the report as a pull request comment. It can't be combined with `--json`.

### In CI

    - run: go run github.com/GillMacedo/escapediff/plugin/scripts@v0.1.0 git --fail origin/${{ github.base_ref }} ./...

The base branch has to be fetched, for example with `fetch-depth: 0` in `actions/checkout`. This hasn't been run in GitHub Actions yet.

## What it reports

| Section | From compiler lines like | Counts for `--fail` |
|---|---|---|
| New heap escapes | `moved to heap: x`, `x escapes to heap` | yes |
| No longer inlined | `can inline F` in base, `cannot inline F: ...` in head | yes |
| New parameter leaks | `leaking param: p`, `leaking param content: p` | yes |
| No longer devirtualized | `devirtualizing i.M to T` in base only | yes |
| Close to the budget | inline cost grew to within 10 of the budget | no |
| Gone, now inlined | the reverse of the above | no |

When a function is no longer inlined, the report also names functions inlined into it whose own cost grew, since their bodies are copied into it.

Matching: each decision is keyed by kind, file, enclosing function and message text. When a key appears more than once, base and head entries are paired by their line offset from the start of the function, so a new escape among several identical ones still shows at the right line. The enclosing function comes from the `can inline` and `cannot inline` lines, which `-m=2` prints on every function's first line.

## Limits

- **Compile-time decisions, not measurements.** "Escapes to heap" means the compiler can't keep the value on the stack. Some of these don't allocate at run time, such as a constant string converted to an interface, and a new escape on a cold path costs nothing in practice. Measure with `go test -bench . -benchmem` and `benchstat` before acting.
- **A build that failed partway.** `compare` refuses a log with no diagnostics at all, but one package failing among several still leaves a log it can read, and that package's changes will look like improvements. Check the build's exit code first. `git` mode stops on any failed build.
- **Same compiler on both sides.** Cost numbers and decisions change between Go versions. `git` mode uses one toolchain for both builds. For `compare`, make both logs with the same version.
- **Only packages you build.** `-gcflags=-m=2` applies to the packages on the command line. Test files aren't compiled by `go build`, so test-only code isn't covered.
- **Line-based function attribution.** A diagnostic belongs to the last function declared above it. Code in a package-level variable initializer is attributed to the function above it. Closures count as part of their enclosing function.
- **Renames show as gone and new.** A function that's renamed or moved to another file shows its escapes as gone under the old name and new under the new one.
- **`git` mode limits:** it needs a module at both revisions, so commits from before a project added `go.mod` fail to build. `git archive` leaves out submodules, files marked `export-ignore`, and symlinks, which are skipped on purpose. Builds that need to download modules need the network.
- `escapediff` compiles code but never runs it, and never uses the network itself. Claude sends what it reads to its model.

## FAQ

**How do I check if my Go change adds heap allocations?** Run `escapediff git main` from your module. It lists escapes that are new compared with `main`. Then benchmark with `-benchmem` to see whether they matter at run time.

**How do I see why a variable escapes to the heap?** Run with `--why` to print the compiler's indented `flow:` lines under each new heap escape. For any escape, run with `--save DIR` and find the same `file:line:col` in `DIR/head.log`.

**How do I know if a Go function is still inlined?** "No longer inlined" lists functions that were inlinable in base and aren't in head, with the reason, such as `function too complex: cost 116 exceeds budget 80`.

**Why does `leaking param` matter?** A leaking parameter means the function may keep a reference, so callers can't keep the argument on their stack. A new leak often explains new heap escapes in other functions.

**How is this different from gcassert?** `gcassert` checks functions you annotate with comments. `escapediff` needs no annotations and reports every change between two builds.

**Does it work with PGO?** It hasn't been tested with `-pgo`. PGO raises the inline budget for hot functions, so pass the same profile to both builds if you use one.

**Is it an MCP server?** No. It's a skill with one Go source file, so `go run` is all it needs.

## Tested on

- **Real compiler output** from `examples/shop` on Go 1.27.3 (linux/arm64), saved in `examples/logs/`, including `-m=1` logs and mixed `-m=1` and `-m=2` comparisons.
- **Real projects**, built with `escapediff git --head C --save DIR C^` on Go 1.27.3: zerolog `14dcf38`, google/uuid `85223fa`, and chi `aba8618`, `be60b2e` and `cf537d4`. Each result was checked against the commit's diff. chi `cf537d4` is a real code change with no compiler-visible effect, and it stays quiet. Older chi and gjson commits from before their `go.mod` failed to build, as described in Limits.
- **Integration tests** in `tests/` create a git repository from the shop example and run `git` mode on the working tree, on two commits, from a subdirectory, on a broken build and on a bad ref. They check that the working tree isn't changed.
- **Go 1.22.12** runs the unit and integration tests, so the integration tests also check fresh Go 1.22 compiler output.

It hasn't been tested in a live Claude Code session, on Windows or macOS, in GitHub Actions, with PGO, with cgo packages, or through `go run ...@latest`, which needs the published repo.

## Contributing

Contributions are welcome, from people and from people working with AI agents. See [CONTRIBUTING.md](CONTRIBUTING.md). Good first issues are [listed here](https://github.com/GillMacedo/escapediff/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).

### How pull requests work

1. **Open a PR.** Fork the repo, work on a branch, and keep it to one change. Write a title that says what changes for users, because it becomes the changelog line: `feat:` goes under Added and `fix:` under Fixed.
2. **Automatic check.** Code changes need tests, and `make test` must pass. Changes over 30 lines need a linked open issue (`Closes #<number>`), and a PR can have at most 800 changed lines. If something is missing, a comment tells you what to fix.
3. **Review.** Claude reviews each PR automatically. Code PRs also get a security review and are tested in a sandbox. The review then merges the PR or asks for changes. PRs that change CI, agent settings, npm or Python dependencies, vendored code or build files can't be merged this way, and a comment says so. The same goes for anything the security review doesn't clear.
4. **Release.** A merge that changes what users install becomes a new version within about two hours, with a GitHub Release and, if the project has one, its npm package. Docs and CI changes wait for the next release.

## AI disclosure

This project was largely generated with Claude Code and reviewed by its maintainer.

## About me

I'm Giulliano Macedo, and I build developer tools at GoTech Solutions. Follow along or get in touch:

- GitHub: [GillMacedo](https://github.com/GillMacedo)
- LinkedIn: [giulliano](https://www.linkedin.com/in/giulliano/)
- X: [@giumacedo](https://x.com/giumacedo)

If `escapediff` saved you some prompts, a star helps others find it.

---

Built by [Giulliano Macedo](https://github.com/GillMacedo), GoTech Solutions · [LinkedIn](https://www.linkedin.com/in/giulliano/) · [X](https://x.com/giumacedo)

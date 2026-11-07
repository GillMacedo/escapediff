# Architecture

    git mode:  git archive BASE --> temp dir --> go build -gcflags=-m=2 -o /dev/null PACKAGES --> base log
               working tree (or git archive HEAD) --> same build                              --> head log
    compare:   base log, head log --> Parse --> Build (diagnostics + per-function inlining) --> Compare --> Result --> text or JSON

- **Parse** keeps the one-line form of each decision. From `X escapes to heap in F:` headers it keeps the full expression, because the one-line form often says only `append escapes to heap`. For `--why` it also groups the indented `flow:` lines under the header they explain, keyed by position and subject so several escapes at one position keep their own flows. It tracks `moved to heap`, `escapes to heap`, `leaking param`, `devirtualizing` and `inlining call to`. `does not escape` lines are ignored.
- **Functions:** `can inline F` and `cannot inline F: reason` lines sit on each function's first line. `-m=2` prints one for every function, so a diagnostic belongs to the last non-closure function declared above it. Type arguments are removed, so `Map[go.shape.int]` and `Map[int]` are one function, inlinable if any instance is. `-m=1` only prints inlinable functions, so function attribution is turned off for it and matching falls back to file.
- **Compare** keys each diagnostic by kind, file, function and text. When a key has several entries, base and head are paired greedily by the closest offset from the function's first line, then by column. What's left in head is new, what's left in base is gone. Functions are keyed by package directory and name.
- **Grown callees:** for a function no longer inlined, the functions head inlines into it from the same package whose cost grew are listed, since an inlined body adds its cost to the caller.
- **Outside files:** absolute paths and module cache paths, which appear when generic code from another package is compiled into the built one, are counted and hidden unless `--all` is set.
- **git mode** never touches the checkout. `git archive` runs from the repository root and is unpacked into a temporary directory with regular files only: symlinks are skipped and paths that leave the directory are rejected. The build runs in the same subdirectory the user ran from, so relative paths in both logs line up. `-o /dev/null` compiles main and non-main packages without writing binaries.

Key choices are in `decisions/`.

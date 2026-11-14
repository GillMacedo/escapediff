# Changelog

All notable changes are documented here, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. This project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.3.1] - 2026-11-14

### Fixed
- Polish markdown output (#10)

## [0.3.0] - 2026-11-08

### Added
- Add --why for new heap escapes (#11)

## [0.2.0] - 2026-11-03

### Added
- Add markdown output for pull request comments (#8)

## [0.1.1] - 2026-11-02

### Fixed
- Refuse a compare log with no compiler diagnostics (#7)

## [0.1.0] - 2026-11-02
### Added
- `compare` command: compares two saved `go build -gcflags=-m=2` outputs
- `git` command: builds a base revision and the working tree, or `--head REF`, and compares them without changing the checkout
- Reports new heap escapes, functions no longer inlined with the compiler's reason and cost, inlined callees whose cost grew, new parameter leaks, calls no longer devirtualized, and functions close to the inline budget, plus what improved
- Matches by file, enclosing function and message text, so moved lines don't count as changes
- Hides changes in files outside the built packages, such as generic standard library code, unless `--all` is set
- `--json`, `--fail` and `--save` options
- Claude Code plugin with the `comparing-go-escapes-and-inlining` skill

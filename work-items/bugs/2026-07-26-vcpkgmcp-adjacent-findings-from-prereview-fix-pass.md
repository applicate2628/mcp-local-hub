---
title: Three adjacent findings surfaced while closing the vcpkg-mcp pre-submission review (discovery / lastfailure / patchesapply)
severity: low
found-by: backend-engineer, closing the 14 pre-submission review findings on feat/vcpkg-mcp
found-in-phase: vcpkg-mcp pre-submission review fix pass (F1-F15)
affected-surface: internal/vcpkgmcp/patchesapply/patchesapply.go, internal/vcpkgmcp/patchesapply/walk.go, internal/vcpkgmcp/{discovery,lastfailure,patchesapply}/*.go (doc comments)
context: adjacent-finding
status: open
---

## Scope note

None of these is one of the 14 reviewed findings, and none was fixed — each
would have widened the approved change surface. Filed per the adjacent-findings
protocol so the orchestrator decides priority.

## 1. `patchesapply.Args.PortDir` has the same CWD-binding class as F3 (relative root)

F3 established that a relative root-like parameter silently binds to the hub
daemon's working directory, so a confident answer describes an unrelated tree.
That was fixed in `lastfailure` for `root` and `buildtrees_root`, and the same
guard was applied to the NEW `patchesapply` parameters `vcpkg_root` and
`overlay_triplets` (a relative one is skipped for triplet lookup rather than
bound to the daemon CWD).

`patchesapply.Args.PortDir` was left as-is. Its doc comment already says
"absolute path to the port directory", but nothing enforces it:
`internal/vcpkgmcp/patchesapply/patchesapply.go` `applyOrder` passes it
straight to `deps.Stat` / `filepath.Join`. A relative `port_dir` therefore
analyses whatever port happens to sit under the daemon's CWD and reports
`ok` with applied/missing/orphan buckets for the wrong port.

Suggested fix: reject a non-absolute `PortDir` with a `relative_port_dir`
reason (a new closed enum value + a wire-schema/description update), mirroring
`lastfailure.ReasonRelativeRoot`.

## 2. `walkPortfile` matches CMake command names case-sensitively

CMake command names are case-insensitive (cmake-language(7); `IF(...)`,
`SET(...)`, `LIST(APPEND ...)` are all legal and appear in older portfiles).

`internal/vcpkgmcp/patchesapply/lexer.go:195` stores the command name verbatim
(`statement{Name: name, ...}`) with no case folding — there is no `ToLower`
anywhere in `lexer.go`. `internal/vcpkgmcp/patchesapply/walk.go:90` then
dispatches on exact lowercase literals (`case "if":`, `case "set":`,
`case "list":`, `case "endif":`, ...).

Consequence for an uppercase-spelled portfile: `IF(...)` / `ENDIF()` never
open or close a guard frame, so every guarded `list(APPEND PATCHES ...)` is
attributed to the wrong (or no) guard, and `SET(...)` never establishes a
variable. The entries are still collected, so the tool answers `ok` with
wrong guard attribution rather than failing — the same "confident verdict from
misread evidence" shape as the reviewed findings.

The new `parseTripletFacts` in `triplet.go` DOES fold case
(`strings.ToLower(st.Name)`), so triplet files are unaffected; only the
portfile walk is.

Suggested fix: fold the command name once, at the single owner — either in
`splitStatementsChecked` when building the `statement`, or via one
`strings.ToLower(st.Name)` at the head of `walkPortfile`'s switch. Add a
portfile fixture spelled in uppercase.

## 3. Every package in `internal/vcpkgmcp` cites decision docs that do not exist

The package doc comments of `discovery`, `lastfailure` and `patchesapply` (and
the already-filed `2026-07-26-vcpkg-mcp-move-adjacent-*` bug) cite:

- `work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md`
- `work-items/decisions/2026-07-25-vcpkg-ground-truth-measured.md`
- `work-items/decisions/2026-07-26-vcpkg-mcp-must-follow-the-in-hub-server-pattern.md`

None of the three exists in the working tree, and none was ever added in any
commit on any ref (`git log --all -- <path>` returns zero commits for each).

These are the documents the code names as the OWNER of the tri-state contract,
the measured traps, and the in-hub placement decision — i.e. the canonical
sources a reviewer would consult to check the code against its spec. As
things stand the only enumeration of the tool contract that exists is the Go
source itself plus the tool descriptions in
`internal/vcpkgmcp/vcpkgserver/tools.go`.

Suggested fix: either land the decision records, or rewrite the citations to
point at whatever is actually canonical. Not done here because it is a
documentation-ownership call, not an implementation one.

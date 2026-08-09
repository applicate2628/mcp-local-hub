---
title: Pre-existing gofmt drift + stale tool-count comment surfaced while moving vcpkg-mcp in-hub
severity: low
found-by: backend-engineer, during the cmd/vcpkg-mcp -> internal/vcpkgmcp house-pattern move
found-in-phase: vcpkg-mcp in-hub migration (feat/vcpkg-mcp)
affected-surface: internal/vcpkgmcp/discovery/discovery_test.go, internal/vcpkgmcp/lastfailure/types.go, internal/cli/root.go, internal/vcpkgmcp/vcpkgserver/server.go
context: adjacent-finding
status: open
---

## Symptom

While moving `cmd/vcpkg-mcp/internal/*` to `internal/vcpkgmcp/*` (decision
`work-items/decisions/2026-07-26-vcpkg-mcp-must-follow-the-in-hub-server-pattern.md`),
`gofmt -l` on the touched tree surfaced three findings that predate the move and
are out of scope for a move+wiring commit:

1. **`internal/vcpkgmcp/discovery/discovery_test.go`** — a map-literal key
   alignment in `TestDiscoverRoot_ManifestWalkup` is not gofmt-canonical.
   Verified pre-existing via `git show HEAD:cmd/vcpkg-mcp/internal/discovery/discovery_test.go | gofmt -d`
   before any of my edits touched the file.
2. **`internal/vcpkgmcp/lastfailure/types.go`** — the `Note` const block's
   column alignment is not gofmt-canonical (same verification method, same
   result: pre-existing).
3. **`internal/cli/root.go`** — an unrelated one-liner-stub alignment block
   (`newAdoptCmd`/`newUpgradeCmd`/etc., lines ~176-190) is not gofmt-canonical.
   My own diff to this file is exactly 2 lines (one import, one
   `AddCommand` entry); the misalignment is elsewhere in the file and predates
   my change.

None of these affect behavior — `go build`/`go vet`/tests are clean regardless
— they are pure formatting drift, most likely from an editor or gofmt-version
mismatch on a prior commit. (Two import-order violations I DID cause in this
same move — `cmakewrap/tool.go` and `cmakewrap/tool_test.go`, where rewriting
`cmd/vcpkg-mcp/internal/evidence` -> `internal/vcpkgmcp/evidence` changed the
two imports' relative alphabetical order — were fixed in the same commit via
`gofmt -w`, since I caused those.)

## Separate stale-comment finding

`internal/vcpkgmcp/vcpkgserver/server.go`'s `VcpkgServer` struct doc comment
(originally written at "increment 1") says *"All three increment-1 tools are
currently pure functions..."* — but `registerTools` in the same package's
`tools.go` registers **seven** tools (`vcpkg_discover_root`,
`vcpkg_last_failure`, `vcpkg_port_resolution`, `vcpkg_pin_status`,
`vcpkg_patches_apply`, `vcpkg_cmake_trace`, `cmake_include_graph`). This
staleness predates the move (increments 2-7 landed without updating this
comment) and is unrelated to the in-hub migration itself, so it was left
untouched to keep this move+wiring diff scoped to the placement change.

## Suggested fix (not done here — scope discipline)

- `gofmt -w` the two drifted files (discovery_test.go, types.go) as a
  standalone formatting-only commit.
- Update the `VcpkgServer` doc comment in server.go to say "all seven tools"
  (or drop the count entirely to avoid re-staling on tool #8).

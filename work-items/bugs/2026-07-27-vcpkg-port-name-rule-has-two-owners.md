# vcpkg port-name validation + root-containment rule now has two owners

- **Status:** open
- **Context:** adjacent-finding
- **Severity:** P3 (maintainability / drift risk; no current defect)
- **Found:** 2026-07-27, while closing PR #591 bot finding
  "portresolution.go:278 — validate the port before joining"
- **Owner packages:** `internal/vcpkgmcp/lastfailure`, `internal/vcpkgmcp/portresolution`

## What

The documented vcpkg port-name rule ("lowercase ASCII letters, digits, or
hyphens; must not start nor end with a hyphen" — Microsoft Learn, vcpkg.json
Reference) plus the `filepath.Rel`-based root-containment check now exists in
TWO places with byte-identical logic:

- `internal/vcpkgmcp/lastfailure/buildtrees.go` — `portNameRE`,
  `errPortEscapesRoot`, `portDirWithin` (validates a port under the buildtrees
  root).
- `internal/vcpkgmcp/portresolution/portresolution.go` — the same three
  identifiers, added by the PR #591 fix (validates a port under each overlay
  root and under `<vcpkg-root>/ports`).

This is a cross-cutting invariant with two owners, which the layering rule C1
("one owner per cross-cutting invariant") calls out: if vcpkg ever loosens or
tightens the port-name rule, or a new path-normalisation escape is discovered,
the two copies can drift and one tool will accept what the other refuses.

## Why it was not fixed in place

The PR #591 lane that found it was scoped OUT of both candidate homes:

- `internal/vcpkgmcp/lastfailure` was owned by a concurrently-running parallel
  lane and explicitly off-limits.
- `internal/vcpkgmcp/evidence` — the natural shared leaf, and already the single
  owner of the sibling `Presence` tri-state — was explicitly READ-ONLY for that
  lane.

Duplicating was therefore the only way to close the actual security finding
(a traversal port name was being normalised by `filepath.Join` and then probed
outside the granted roots) without editing another lane's package mid-flight.

## Proposed fix

Hoist the rule into `internal/vcpkgmcp/evidence` (or a new sibling leaf, e.g.
`internal/vcpkgmcp/portname`) as the single owner:

```go
// evidence (or portname).PortDirWithin validates port as ONE legal vcpkg
// port-name segment and returns its directory under root, guaranteeing
// containment.
func PortDirWithin(root, port string) (string, error)
```

Then have both `lastfailure` and `portresolution` call it and delete their
local copies. Dependency direction is unchanged — both already import
`evidence`, and `evidence` imports neither.

Keep BOTH checks in the shared owner: the charset rule rejects the input shape,
and the `filepath.Rel` containment check is the actual security boundary (it
holds even if the charset rule is loosened, and catches platform-specific
normalisation a regex cannot reason about).

## Verification when fixed

- `internal/vcpkgmcp/portresolution/pr591_fixes_test.go`
  (`TestTraversalPortNameIsRefusedBeforeTheJoin`,
  `TestPortDirWithinRejectsAnEscapeIndependentlyOfTheCharsetRule`) and the
  equivalent lastfailure buildtrees-root tests must both still pass against the
  shared owner.
- A drift guard (one test asserting both packages resolve the same verdict for
  a shared table of names) becomes unnecessary once there is one owner.

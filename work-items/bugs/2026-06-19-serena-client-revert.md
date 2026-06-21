---
status: resolved
severity: P2
date: 2026-06-19
slug: serena-client-revert
decision: work-items/decisions/2026-06-21-serena-router-client-url-single-owner.md
---

# Serena client URL reverts to legacy daemon port during install

## Symptom

After serena is migrated to the dynamic-pool router shape, an install plan can
rewrite client configuration back to the legacy per-daemon URL
`http://127.0.0.1:9121/mcp`. Clients then bypass the live GUI router URL
`http://127.0.0.1:<gui-port>/serena/mcp` and hit a dead daemon port.

## Root Cause

`BuildPlanWithOpts` previously built every native HTTP client binding from
`clients.HubLoopbackURL(daemon.Port, urlPath)` in `internal/api/install.go:1589`.
That made install the remaining client-URL writer that did not consume
`SerenaRouterClientURL(guiPort)`, while migrate already used that owner in
`internal/api/migrate.go:233`.

## Resolution

Install now receives the live GUI port through the hermetic `BuildPlanOpts.GUIPort`
field (`internal/api/install.go:1479`). For serena, `BuildPlanWithOpts` routes
client writes through `SerenaRouterClientURL(opts.GUIPort)` when the port is
known, and skips the serena client write with a notice when it is not
resolvable (`internal/api/install.go:1591`). CLI install entry points resolve the
GUI pidport in `internal/cli/install.go:1198`; GUI handlers pass their already
bound process port in `internal/gui/install.go:156` and `internal/gui/install.go:212`.

## Decision

[`work-items/decisions/2026-06-21-serena-router-client-url-single-owner.md`](../decisions/2026-06-21-serena-router-client-url-single-owner.md)
records `SerenaRouterClientURL` as the single owner consumed by both client URL
writers and the follow-up to make the serena manifest router-native.

## Verification

- `TestBuildPlanWithOpts_SerenaInstallWritePlaneUsesRouterURL` in `internal/api/install_test.go:53`.
- `TestBuildPlanWithOpts_SerenaInstallWritePlaneSkipsWhenGUIPortUnknown` in `internal/api/install_test.go:77`.
- `TestBuildPlanWithOpts_NonSerenaIgnoresGUIPort` in `internal/api/install_test.go:95`.
- `TestBuildPlanWithOpts_SerenaRouterEntryNotRevertedToLegacyURL` in `internal/api/install_test.go:120`.

## Terms and Abbreviations

- CLI: Command-line interface.
- GUI: Graphical user interface.
- HTTP: Hypertext Transfer Protocol.
- URL: Uniform Resource Locator.

## Resolution (2026-06-21)

FIXED by #400 (install-write-plane single-owner wiring): BuildPlanWithOpts now consults SerenaRouterClientURL (the /serena/mcp router single owner) for serena, with a live-GUI-port liveness gate, so install/manifest-sync no longer reverts serena to the dead 9121/mcp. Verified live after redeploy: serena on http://127.0.0.1:9125/serena/mcp, 42 MCP healthy. Duplicate record 2026-06-19-serena-client-revert-on-manifest-sync.md (mostly-resolved via #401) describes the same defect.

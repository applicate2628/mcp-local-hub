# Plan: deep-review P2 scan per-client isolation

Date: 2026-06-30
Owner: main conversation
Participants: main conversation, backend-engineer discipline, qa-engineer discipline

## Scope

Fix deep-review P2 in `mcp-local-hub`: one malformed readable client config must not abort the entire global `ScanFrom` result. Preserve existing `ScanResult` fields and add an additive per-client error surface.

## Steps

1. Map `ScanFrom` result shape and consumers: `internal/gui/server.go`, `internal/gui/scan.go`, `internal/cli/scan.go`, project scan routes, and hub bind-adjacent scan usage.
2. Add a failing `internal/api` regression test with valid Claude/Zed configs plus malformed Codex TOML.
3. Implement per-client isolation by scanning into a temporary per-client entry map, recording `client_scan_errors`, downgrading that client's config presence to `error`, and merging only successful client scans.
4. Verify with the requested scan-focused `internal/api` test gate, `go build ./...`, `go vet ./...`, Linux/Darwin cross-builds, and publication-safety scan.
5. Commit with a message referencing deep-review P2 and push `fix/p2-scan-per-client-isolation`.

## Acceptance Criteria

- A malformed client config records a per-client error and does not return a whole-scan error.
- Valid sibling clients still contribute scan entries.
- Existing `ScanResult` fields remain present and compatible.
- Fatal global errors, such as manifest loading errors, still return from `ScanFrom`.

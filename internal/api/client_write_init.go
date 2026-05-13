// client_write_init.go — wire the production secure-write pipeline
// into the client-adapter writer hook.
//
// `internal/clients/write.go` declares `WriteConfigFile` defaulted to a
// plain os.WriteFile wrapper so in-package adapter tests continue to
// work against `t.TempDir()` (which on Windows lives under %TEMP%'s
// Authenticated Users-readable DACL and would fail the
// SecureWriteClientConfig parent-dir gate).
//
// Production wires it to the secure writer so every token-bearing
// rewrite — including the Phase 5 install reconciler's `mcphub-hub`
// aggregate entry — flows through the handle-relative, DACL-bound
// pipeline. The swap is a one-way override: package `api` is in the
// import graph of every production entry point (cmd/mcphub, internal/cli,
// internal/gui), so this init() always runs before any adapter call.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"SecureWriteClientConfig sequence" + §"Bidirectional install
// reconciler".
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 5.1
// step 6 ("Route ALL adapter writes through SecureWriteClientConfig").

package api

import "mcp-local-hub/internal/clients"

func init() {
	clients.WriteConfigFile = SecureWriteClientConfig
}

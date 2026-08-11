# Bug: LSP and Serena ordering tests search a stale assignment spelling

- id: 2026-08-10-resolver-source-anchor-test-rot
- context: adjacent-finding
- status: open
- severity: low
- area: internal/api/lsp_routing and internal/api/serena_routing
- found-by: qa-engineer

Both `TestRefreshCapturesEntriesBeforeRegistryRelease` rows fail identically on
the candidate and immutable `HEAD`: the test searches `entries =` while the
source uses `entries :=`. This is baseline test-rot, not an A-D regression.
Replace the lexical proxy with a behavior or syntax-aware oracle.

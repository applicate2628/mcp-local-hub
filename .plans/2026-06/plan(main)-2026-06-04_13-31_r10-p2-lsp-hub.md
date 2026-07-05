# Plan Snapshot: R10 P2 LSP Hub Fixes

1. Inspect current LSP routing and status code and capture evidence for all three findings.
2. Add targeted failing tests for relative `filePath`, multi-path `files`, and registry-only status liveness.
3. Implement the smallest owning-boundary fixes in `internal/gui`, `internal/api/lsp_routing`, and `internal/api`.
4. Run targeted tests, `go build ./...`, and scoped `go vet`; record exact results.

Status: PASS. All plan steps were completed in this session.

## Terms and Abbreviations

- LSP: Language Server Protocol.
- P2: priority 2 finding.
- PASS: the requested implementation and verification completed successfully.

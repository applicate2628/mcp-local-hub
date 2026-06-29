# Plan: LSP pool exclusion-aware allocation

Created: 2026-06-29
Owner: main conversation
Status: complete

## Steps

1. Inspect allocator, manifest, and current tests with `rg` and file reads. Status: done.
2. Add a failing Windows-tagged allocator test for OS-excluded TCP ranges. Status: done.
3. Implement Windows exclusion-aware allocation and capacity diagnostics while keeping non-Windows behavior unchanged. Status: done.
4. Relocate/widen the shipped `mcp-language-server` workspace pool to maintain capacity on Windows hosts with dynamic exclusions. Status: done.
5. Run requested verification: focused API tests, build, vet, Linux/Darwin cross-builds, publication-safety scan. Status: done.
6. Commit and push `fix/lsp-pool-exclusion-aware`. Status: pending at snapshot time.

## Verification

- `go test -count=1 -run 'Port|Alloc|Register' ./internal/api/`: PASS.
- `go build ./...`: PASS.
- `go vet ./internal/api/...`: PASS.
- `GOOS=linux go build ./...`: PASS.
- `GOOS=darwin go build ./...`: PASS.
- Git Bash publication-safety scan: PASS.

## Notes

The allocator subtracts Windows TCP excluded ranges before bind probing and reports total, OS-excluded, usable, registry, and process-bound counts on exhaustion. The shipped LSP pool moved from `9200-9299` to `9400-9599` so a typical 100-port Windows exclusion still leaves enough capacity for the 9-language registration set.

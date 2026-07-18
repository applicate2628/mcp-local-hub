# PR 562 Round 3 Backend Plan

## Scope

Fix the two admitted Option B review findings without changing the accepted P1-1/P1-2 residuals or the existing rotation safety gates.

## Completed Steps

- [x] Orient to the live repository workflow and governing decision record.
- [x] Reproduce the missing durable reconcile marker and overly broad initial-bind classification with failing tests.
- [x] Persist `reconcile_pending` through the existing hardened hub endpoint writer, restore it into hub health on startup, and clear it after successful `--reconcile-hub-mode` application.
- [x] Classify only an actual refusal while binding the persisted hub port as an initial-bind failure eligible for owner probing and adversarial rotation.
- [x] Verify focused regressions, related API/GUI/CLI tests, the repository build and vet gates, touched-file formatting, and the required tagged API/GUI suites.
- [x] Reconcile the implementation against the requested invariants and preserved behavior.

## Verification

- `go build ./...`
- `go vet ./...`
- `gofmt -l` on all touched Go files
- `go test -tags=test_state_path_env -count=1 -timeout 15m ./internal/api/ ./internal/gui/`

All checks passed on 2026-07-18. No commit or publication action was performed.

## Terms and Abbreviations

- API: Application Programming Interface.
- CLI: Command-Line Interface.
- GUI: Graphical User Interface.
- PR: Pull Request.

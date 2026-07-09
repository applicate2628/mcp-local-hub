# Backend Engineer Plan Snapshot - Absent-Only Watermark Rollback

Plan created for the scoped P3 fix in `internal/api/register_supervisor.go` and `internal/api/register_supervisor_rollback_test.go`.

1. Verify current code shape and invariant owner - completed.
2. Add failing rollback race regression - completed.
3. Patch restore watermark branch - completed.
4. Run scoped gates - completed.
5. Write mandatory session log and summarize - completed.

Acceptance criteria:

- Preserve fail-toward-stop behavior by never mutating `Stops` in the watermark-only restore branch.
- Restore prior watermark only when `desired.Stops[key]` is still absent under the rollback helper's fresh read.
- Add a deterministic regression that fails before the guard and passes after it.
- Run only the requested scoped test suite for `./internal/api/`, plus `go build ./...` and `go vet ./internal/api/`.

Gate results:

- PASS: `go test ./internal/api/ -run 'IntentCollapse|StopIntent|SupervisorIntent|RegisterSupervisor|ParsedManifest|PhaseF|OwnershipDisambiguator|Watermark' -count=1`
- PASS: `go build ./...`
- PASS: `go vet ./internal/api/`

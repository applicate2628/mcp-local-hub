# Brief: Cursor explicit opt-in consistency

## Goal

Fix the functional register fallback and all stale user-facing default-client restatements so Cursor is touched only when explicitly selected.

## Scope

- Derive `internal/api/register.go` fallback bindings from the existing client registry default-install descriptor.
- Add or update narrowly scoped tests that prove Cursor is excluded from fallback registration while registry-default clients remain included.
- Correct the sweep-proven stale test comment at `internal/cli/install_legacy_upgrade_classification_test.go:181-185`; preserve its explicitly bound three-client fixture data.
- Update all stale Go, TypeScript, Markdown, fixture, golden, count, and expected-output statements found by a repository-wide sweep.
- Regenerate `internal/gui/assets/` through `go generate ./internal/gui/...`.
- Run `go build ./...`, `go vet ./...`, safe narrow tests for changed packages, frontend typecheck/tests, and end-to-end diff/sweep review.
- Create one local commit. Do not push or create/update a pull request.

## Out of scope

- Starting, installing, registering, setting up, supervising, or otherwise running `mcphub`.
- Unscoped `go test ./...`, broad `internal/api`/`internal/gui` tests, GUI end-to-end tests, Task Scheduler changes, process termination, push, release, or pull-request operations.
- Repairing unrelated pre-existing `work-items/` drift.

## Constraints

- The live installed fleet is protected and must remain undisturbed.
- `go build ./...` and `go vet ./...` are required and safe.
- Every Go test command must name only touched packages and a narrow `-run` pattern where `internal/api` or `internal/gui` is involved.
- Frontend source changes require generated embedded assets; generated assets may not be hand-edited.
- The commit message must name the duplicated-policy drift root cause, the single-owner symbol, and any unverified item.

## Acceptance criteria

1. Workspace registration fallback contains exactly the client descriptors marked default-install by the registry owner, without a second literal policy list.
2. Cursor is absent from bare/default registration and installation claims, and every relevant surface states `--clients cursor` opt-in.
3. Repository-wide default-client/count/expected-output/fixture sweep has no stale live-tree occurrence.
4. Frontend source and generated embedded assets are synchronized.
5. Required build/vet, safe scoped tests, frontend checks, and independent review pass with exact evidence.
6. One focused local commit exists; no push or pull-request mutation occurs.

## Roles and ownership

- Lead/orchestration: main conversation.
- Facts and exhaustive inventory: `$analyst`.
- Owner/seam design: `$architect`.
- Delivery phases and safe gates: `$planner`.
- Go behavior, Go/CLI tests, integration owner: `$backend-engineer`.
- React text and generated asset phase: `$frontend-engineer`.
- Canonical documentation consistency: `$knowledge-archivist`.
- Verification: `$qa-engineer` through the configured external-reviewer route.
- Maintainability/single-owner gate: `$architecture-reviewer` through the configured external-reviewer route.

## Critical risks

- Live fleet / scheduler disturbance: Lead owns command allowlisting.
- Split default-client policy: Architect and backend engineer own the single-owner seam.
- Stale user-facing residue: Analyst and knowledge archivist own exhaustive inventory and documentation synchronization.
- Generated bundle drift: Frontend engineer owns `go generate` and frontend gates.
- Regression or incomplete verification: QA and architecture-reviewer own independent gates.

## Current stage

Research and inventory.

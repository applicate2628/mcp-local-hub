# Brief

## Primary task

Close the 34 Codex-bot findings reported against the remote head of PR #591 while preserving the four unpushed local commits.

## Scope

- Classify every supplied finding against the local branch and the four named local commits.
- Sweep every cited defect class across all sibling sites.
- Fix only findings still real and open.
- Add regression tests that fail when the fix is removed.
- Run the required mutation proof, scoped package tests, `go build ./...`, and `go vet ./...`.
- Commit the task-owned fix without pushing.

## Out of scope

- GitHub review-thread resolution.
- Push, reset, stash, checkout-based restoration, or edits outside this worktree.
- GUI, tray, or supervisor launch.
- Any untagged test run of `internal/api` or `internal/cli`.
- Any unscoped `go test ./...`.

## Acceptance criteria

- All 34 findings have evidence-backed classifications.
- Every real finding is fixed at the owning boundary and its full class is swept.
- Every added regression test has preserved red-before-green mutation evidence.
- Scoped tests pass with the user-mandated state isolation; build and vet pass.
- One focused local commit names the findings it closes.

## Required roles

- Analyst: local-commit classification and class inventories.
- Security engineer: path-containment and credential-boundary constraints.
- Performance engineer: bounded-read and allocation-cap constraints.
- Backend engineer: implement only accepted still-open fixes.
- QA engineer: independent mutation and verification gate.

## Critical risks and owners

- Live Windows fleet exposure: Lead owns command-envelope enforcement.
- False-positive reviewer appeasement: Analyst owns classification evidence.
- Path traversal or credential leakage: Security engineer owns constraints.
- Unbounded allocation or incomplete-evidence semantics: Performance engineer owns constraints.
- Cross-package regression and mutation proof: QA engineer owns verification.
- Integration across packages: Backend engineer is the integration owner.

## Expected change boundary

Only the packages and tests implicated by genuinely open findings may change. Existing local commits are preserved.

## Terms and Abbreviations

- PR: Pull Request.
- QA: Quality Assurance.

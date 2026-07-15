# E2E Status Seam Implementation Plan

**Goal:** Repair the stale `TestE2E_LazyRegisterFullLifecycle` Status assertion without weakening the production fail-loud contract.

**Scope:** `internal/e2e/lazy_register_test.go` only unless evidence proves an e2e seam-assignment leak. Do not change `internal/api/health.go`, commit to `master`, or push.

## Steps

- [ ] Verify the isolated failing test and trace `API.Status` plus every `SupervisorIPCStatusFn` assignment.
- [ ] Inspect the e2e test binary's seam state at the failure boundary and distinguish it from `statusInternalDialFn`.
- [ ] Update the test comment and assertion to the verified current Status contract; retain only necessary cleanup.
- [ ] Run the specified build, vet, targeted API, and two order-independence e2e gates.
- [ ] Write the session report with evidence, outcome, branch, and residual risk.

## Execution Record

Execution role: backend-engineer
Assigned / replaced internal role: none
Requested provider: none
Resolved provider: none
Actual execution path: local Codex session
Model / profile used: unspecified by runtime
Deviation reason: none

# T13 correction cycle 1 — publication fixture and EnsureAlive boot-grace

Gate: PASS

Execution role: `$backend-engineer` integration owner. Scope is the two T13 QA findings only; no production, Git/index, live process, service, CST, hub, or deployment mutation occurred.

## Verified diagnosis

| Finding | Evidence | Conclusion |
|---|---|---|
| Publication fixture | Fresh installed scanner returned `PS-FINDING-CONTENT kind=path-blob line=95 class=machine-path` for `internal/api/supervisor_cst_identity_test.go`; the value was the test image at lines 93-107. | RED was caused by a machine-specific synthetic fixture, not production data. |
| EnsureAlive full-suite failure | Accepted T13 QA artifact SHA `299B3916687AFDE17A9597F7CE626F356ED93EAF4896D6E100806516D272D02A` records supervisor age about 160 s, one relaunch, exact-test PASS, and 20/20 isolated PASS. | Failure depends on package-process age, not nondeterministic relaunch behavior. |
| Causal owner | Current candidate diff at `internal/api/supervisor_lock.go:90-100` intentionally changed `StartedAt` from wall-clock acquisition time to kernel process creation time for exact PID-generation binding. The old test premise remained at `internal/cli/supervise_ensure_alive_test.go:571-572`; the production decision uses the explicit 45 s threshold at `internal/cli/supervise_ensure_alive.go:1099-1104`. | Current security binding exposed a stale test-time premise. Reverting production would break identity binding; the correct owner-level fix is deterministic test time. |
| Adjacent alternatives | No candidate diff exists in `internal/cli/supervise_ensure_alive.go`; focused exact and repeated tests below exercise the real decision seam. | No production EnsureAlive edit is authorized or needed. Known routing stale anchors remain out of scope. |

## Change surface

| Path | Change |
|---|---|
| `internal/api/supervisor_cst_identity_test.go:93-107` | Replace the user-profile and temp absolute image fixtures with distinct portable synthetic paths while preserving exact-match/mismatch assertions. |
| `internal/cli/supervise_ensure_alive_test.go:563-596` | Write a fixed `StartedAt` and inject `observedAt = StartedAt + 1s`; this engineers a deterministic point inside boot grace regardless of package age. |
| `work-items/active/2026-08-11-cst-saved-field-sampler/implementation-t13-correction.md` | This receipt. |
| `work-items/active/2026-08-11-cst-saved-field-sampler/status.md` | Return the corrected T13 candidate to QA. |

No compatibility alias, timeout widening, symptom suppression, or production fallback was added.

## RED to GREEN receipts

| Oracle | RED | GREEN |
|---|---|---|
| Installed publication scanner | `python .../check-publication-safety.py --path internal/api/supervisor_cst_identity_test.go` -> exit 1, line-95 machine path. | Same current installed scanner and path -> exit 0, `publication-safety: clean (path, examined 1 file)`. |
| EnsureAlive boot-grace | Full T13 package: age about 160 s, relaunch occurred; exact and 20-repeat isolated runs passed because their process remained younger than 45 s. | `go test ./internal/cli -run '^(TestEnsureAlive_HeadlessFleet_BootGraceSuppresses|TestEnsureAliveHeadlessFleetSupervisorAge_DomainMatrix)$' -count=20 -timeout 4m` -> exit 0, `ok`, 1.577 s. |
| Supervisor status image binding | Publication RED identified the only bad fixture. | `go test ./internal/api -run '^TestSupervisorStatusAuthorizationKernelBinding$' -count=1 -timeout 2m` -> exit 0, `ok`, 0.040 s. |
| Formatting/diff | Not applicable before edit. | `gofmt -d` on both changed test files -> empty; `git diff --check` -> exit 0. |

## CodeGraph evidence

Pre-edit and post-edit exact-path queries were issued for `TestSupervisorStatusAuthorizationKernelBinding`, `TestEnsureAlive_HeadlessFleet_BootGraceSuppresses`, `AcquireSupervisorLock`, and the EnsureAlive clock seam. Both responses reported `CodeGraph auto-sync is DISABLED` because another process holds the index lock. The post-edit response still resolved the expected call edges (`BootGraceSuppresses -> AcquireSupervisorLock`, rewrite helper, and authorization validator), but returned irrelevant stale files instead of the current test bodies. This is an explicit index gap, not claimed as current source evidence; exact file reads and fresh compiler/tests are authoritative for this correction.

## Owner invariants and falsifiers

| Invariant | Owner | Falsifying probe |
|---|---|---|
| Supervisor `StartedAt` remains kernel PID-generation time when available. | `internal/api/supervisor_lock.go:90-100` | Revert to acquisition `time.Now` and the security identity binding no longer equals kernel creation time. |
| Boot-grace test outcome is independent of package duration. | `TestEnsureAlive_HeadlessFleet_BootGraceSuppresses` | Run the two-test command with `-count=20`; any relaunch or missing `boot-grace` assertion fails. |
| Image mismatch remains rejected without machine-local fixture content. | `TestSupervisorStatusAuthorizationKernelBinding` | Replace the synthetic mismatch with the expected synthetic image; the mismatch case must fail its test assertion. |

## Rollback

Restore only the two fixture hunks above and delete this correction receipt; do not revert the kernel process-creation binding in `supervisor_lock.go`. No runtime state needs recovery.

## Gate

PASS — both T13 findings have deterministic, focused GREEN evidence. Return the candidate to `$qa-engineer` for the T13 broad QA rerun; this correction does not claim T13 itself is complete.

## Terms and Abbreviations

- PID: process identifier.
- QA: quality assurance.
- RED/GREEN: failing diagnostic before correction / passing verification after correction.

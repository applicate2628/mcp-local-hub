# T15 architecture correction cycle 1

Gate: PASS

Execution role: `$backend-engineer` integration owner. Baseline candidate: `43fee019d46c69522ebe79be952d5f139bd4854f`. Scope is AR-T15-01/02/03 only; no commit, index, live Service Control Manager, CST, hub, fleet, registration, or deployment mutation occurred.

## Verified root cause and correction

| Finding | Verified cause | Owner-level correction |
|---|---|---|
| AR-T15-01 | Daemon and broker `main()` raised unavailable; worker selected a synthetic `_unavailable` application. | Add explicit provisioning-owned `DaemonServiceCompositionV1`, `BrokerServiceCompositionV1`, and `WorkerCompositionV1`. Each root builds the real owner graph and runs the supplied listener/worker seam. Missing composition remains fail-closed; no environment/path policy fallback exists. |
| AR-T15-02 | Containment erased its kernel receipt to bytes; broker reconstructed seven kernel facts with literal `True`; `_unavailable` fabricated worker settlement. | `ContainedInvocationReceiptV1` carries response plus the seven containment-owned facts. Broker rejects incomplete receipts and copies only those facts plus the actual worker transaction receipt. The synthetic worker success path was deleted. |
| AR-T15-03 | Dead `UnavailableBrokerTransport` remained and fabricated complete cancellation. | Delete it; production residue scan is zero. |

## Exact change surface

| Path | Change |
|---|---|
| `src/mcphub_em_mcp/cst_saved_field_containment_windows.py` | Typed containment receipt and preservation through existing runner byte-compatible seams. |
| `src/mcphub_em_mcp/cst_saved_field_broker_service_windows.py` | Receipt-only aggregate settlement; real broker composition root. |
| `src/mcphub_em_mcp/cst_saved_field_daemon_service_windows.py` | Real daemon composition root with provisioning-supplied listener. |
| `src/mcphub_em_mcp/cst_saved_field_broker_worker.py` | Real worker application composition; remove synthetic default. |
| `src/mcphub_em_mcp/cst_saved_field_broker_client_windows.py` | Remove stale unavailable transport. |
| `tests/test_cst_saved_field_t15_production_composition.py` | Persistent production-root, per-fact receipt, and residue oracles. |
| `tests/test_cst_saved_field_t08_containment.py` | Assert the typed response field. |
| `tests/test_cst_saved_field_integration.py` | Synthetic containment owner returns the production receipt type. |

## RED to GREEN

| Oracle | RED | GREEN |
|---|---|---|
| T15 persistent tests | Exit 1: 4 failed — missing receipt and three composition types; unavailable residue present. | `pytest ...test_cst_saved_field_t15_production_composition.py` -> 12 passed. Seven parametrized cases independently falsify each kernel fact. |
| T03-T12 plus route surface | Typed-return fixture initially exposed two expected callers. | Focused phase command -> 89 passed in 2.5 s. |
| Static gates | Literal-success and unavailable class present before correction. | Ruff PASS; Ruff format-check PASS; `git diff --check` PASS; production scan for unavailable transport and literal-success settlement -> ZERO. |

One adjacent direct Win32 kernel probe, `test_saved_field_worker_reference_order`, returned `containment_startup_invalid` when run separately with the current Python executable. It does not traverse any corrected composition or typed-return caller and was not changed or used as completion evidence. The accepted T03-T12 phase set and production route are green; the target-only containment gate remains later-plan work.

## CodeGraph and impact evidence

CodeGraph was queried before and after edits for the exact entrypoints, containment receipt, broker application, and stale transport. Both calls reported global auto-sync disabled because another process holds the file lock. The post-edit call nevertheless read the current on-disk new symbols and reported one local caller for each `compose_service`/`compose_application`; the disabled banner is recorded as an index-health gap, not silently promoted to fresh graph evidence. Fresh tests and exact current-file static scans are the correctness oracles.

## Invariants, falsifiers, rollback

| Invariant | Owner | Falsifier |
|---|---|---|
| No production root can succeed without explicit provisioning dependencies. | Three composition roots | Call `main()` without composition; daemon/broker fail unavailable and worker exits 78. |
| Every broker kernel fact originates in containment. | `ContainedInvocationReceiptV1` | Set any one fact false; each parametrized test rejects with `containment_settle_failed`. |
| Existing byte callers retain their byte contract. | `ContainedSamplerRunner` | T08 and integration phase tests fail if a receipt leaks through the legacy runner seam. |
| No unavailable/synthetic-success route remains. | Broker client/worker production modules | Production residue scan finds any named class or literal-success constructor. |

Rollback: restore these eight code/test paths to candidate `43fee019...` bytes and remove this receipt; there is no runtime state to recover. Do not touch unrelated dirty paths.

## Gate

PASS — all three T15 blockers are corrected with persistent RED-first oracles and focused GREEN evidence. Return to T13 re-verification, create a new immutable T14 candidate, then re-run independent T15 review.

## Terms and Abbreviations

- SCM: Windows Service Control Manager.
- RED/GREEN: failing diagnostic before correction / passing verification after correction.
- Receipt: typed evidence emitted by the resource owner for observed settlement facts.

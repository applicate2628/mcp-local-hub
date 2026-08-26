# T13 Verification Cycle 3 — T15 Architecture Correction

Date: 2026-08-12

Execution role: `$qa-engineer`

Correction input: `implementation-t15-correction.md` SHA-256 `9C0AE75C01F0750E2CFE34B7465EB3544130EF7BA87C01700A88B7D39B977C4F`.

Baseline candidate: `43fee019d46c69522ebe79be952d5f139bd4854f`. Accepted plan: `D1C1137D062DF4652902657696D6EE488D4DAD90D6ED33CFBC8B856C7C03E99A`.

This cycle was read-only except for this canonical QA artifact and `status.md`. No source, test, Git index, Service Control Manager, CST, hub, or fleet state was mutated.

## Receiving-side echo and acceptance falsifiers

| Criterion | What would incorrectly pass? | Required oracle | Result |
|---|---|---|---|
| Corrected production route and receipts | Synthetic composition tests pass while the real child entrypoint cannot emit its startup proof. | Direct Win32 child-process containment test plus full suite. | FAIL |
| T15 persistent guards | Composition dataclasses and typed receipts exist but are never usable across `CreateProcessW`. | 12 persistent T15 tests plus the direct-child guard. | Partial: 12 PASS, direct child FAIL |
| Affected T03-T12 route matrix | In-process fakes bypass the child module entrypoint. | Focused phase matrix followed by direct Win32 test. | Partial: 71 PASS, direct child FAIL |
| Full Python regression | An affected failure is dismissed because the same symbols have unit coverage. | One bounded full frozen Python run. | FAIL: 585 PASS, 1 FAIL |
| Win32 differential | A standalone rerun is used to exonerate a correction-caused route break. | Exact old-vs-current symptom or deterministic affected-path diagnosis. | FAIL: deterministic current-path diagnosis binds the failure to the correction |
| Static/publication/Go/state gates | Green secondary checks conceal a failing required runtime invariant. | Run only if the primary affected route remains viable; otherwise report unverified and return owner. | Not advanced after decisive affected regression |

## CodeGraph and exact impact

CodeGraph exact exploration read the current corrected `WindowsContainedInvocation`, `ContainedWorkerBrokerApplicationV1`, broker composition, worker composition, and `test_saved_field_worker_reference_order` sources without a stale or pending banner for any referenced file. Pending notices named unrelated files only.

The correction artifact's admitted source/test surface is Python-only. The worktree also contains an unrelated modified Go file, `internal/api/port_alloc_excluded_windows.go`, which is outside this correction and was not treated as correction impact. No Go source belongs to the T15 correction.

## Fresh command receipts

| Command | Exit | Counts / duration | Interpretation |
|---|---:|---|---|
| `uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_containment_windows.py::test_saved_field_worker_reference_order` | 1 | 0 passed, 1 failed; 5.8 s | Exact required Win32 symptom reproduced: `ContainmentFailure: cst_saved_field.containment_startup_invalid` while waiting for the first-instruction proof. |
| `uv run --frozen --python 3.13 pytest -q --tb=no tests/test_cst_saved_field_t15_production_composition.py` | 0 | 12 passed; 0.8 s | Persistent T15 tests are green but do not launch the production child module. |
| `uv run --frozen --python 3.13 pytest -q --tb=no` over T03-T10 plus integration | 0 | 71 passed; 1.5 s | In-process affected matrix is green; it does not falsify the direct-child failure. |
| `uv run --frozen --python 3.13 pytest -q --tb=no` | 1 | 585 passed, 1 failed; 14.9 s | Full suite reproduces the same sole Win32 failure. No timeout, crash, skip, or xfail delta was observed. |
| Exact process-survivor check after full run | 0 | 0 recent owned pytest/Python processes | No cleanup was required; foreign CST/hub/Python processes were not touched. |
| `git diff --cached --name-only`; `git rev-parse HEAD` | 0 | index empty; HEAD `43fee019d46c69522ebe79be952d5f139bd4854f` | Candidate baseline and index preserved. |

Raw outputs were returned directly by the bounded current-session commands. No raw transcript was persisted because the allowed write surface is limited to `implementation-t13.md` and `status.md`.

## Deterministic root-cause diagnosis

The failure is an affected regression, not an unrelated baseline condition:

1. Containment builds the real child command as `"<python>" -I -s -E -m mcphub_em_mcp.cst_saved_field_broker_worker` (`cst_saved_field_containment_windows.py:506-522`).
2. The corrected worker changed `run_worker` to require an explicit application and changed `main(composition=None)` to return exit 78 when no Python `WorkerCompositionV1` object is supplied (`cst_saved_field_broker_worker.py:208-215,251-270`).
3. `CreateProcessW` conveys only the executable, command line, environment and three inherited standard handles. The broker's `WorkerCompositionV1` Python object is not serialized into any of those production channels.
4. Therefore the child exits from `main()` before entering `run_worker`, before writing `WorkerStartupProofV1` to stderr, and before reading the request. The parent reaches its five-second startup deadline and raises the observed `containment_startup_invalid` (`cst_saved_field_containment_windows.py:1129-1173`).

This same path is constructed by corrected broker `compose_service`, which passes only `worker_executable` to `WindowsContainedInvocation` (`cst_saved_field_broker_service_windows.py:497-519`). The direct Win32 test therefore exercises the production child boundary that the synthetic T15 and integration tests bypass.

## Failure classification and owner return

| Test / invariant | Classification | Owner action |
|---|---|---|
| `test_saved_field_worker_reference_order` / production child startup | regression | `$backend-engineer` must provide a production-serializable provisioning/transaction composition mechanism that preserves the accepted broker-worker authority boundary and emits startup proof before request read. |
| T15 synthetic composition coverage | coverage gap | Add a persistent test that launches the exact production command and proves startup proof, request processing, typed containment receipt, and settlement without injecting a Python composition object in-process. |

Falsifying guard for the correction: start the exact `build_create_process_spec()` command under the real Win32 Job/HANDLE_LIST path, observe a complete startup proof before request bytes, receive a broker-worker response from the actual transaction adapter, and settle all kernel and worker receipt facts. The guard must fail against the present correction.

The QA skill requires a bug-registry record for `REVISE`, but the dispatch explicitly restricts writes to `implementation-t13.md` and `status.md`. No unauthorized bug file was created; the lead must route this recorded defect into the registry if the correction cycle does not immediately own it.

## Unverified secondary gates

Ruff, format, publication scan, complete T03-T12 matrix, and appropriate Go gates were not extended after the required affected Win32 route and the full Python suite failed deterministically. Their earlier green evidence cannot promote this cycle. A corrected cycle must rerun them as dispatched.

## Gate

REVISE — the T15 correction breaks the exact production Win32 child startup path. Return to `$backend-engineer`; do not create a new T14 candidate.

## Terms and Abbreviations

- Win32: native Windows process and handle application programming interface.
- RED/GREEN: failing pre-correction or falsifying result / passing corrected result.
- SCM: Windows Service Control Manager.
- Receipt: typed evidence emitted by the resource owner for observed settlement facts.

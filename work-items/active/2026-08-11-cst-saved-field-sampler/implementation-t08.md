# T08 — Worker launch, Job containment and all-return cleanup

Gate: **PASS**

## Scope and invariant

`WindowsContainedInvocation` owns one atomic, non-shell worker creation using exact `PROC_THREAD_ATTRIBUTE_JOB_LIST` plus `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` and no suspended/post-create assignment gap. Before request construction or user code, it verifies exact Job membership and two identical kernel reads of the `CreateProcessW` process-handle identity: process identifier (PID), creation time, token user security identifier, session, canonical image, package identity and parent broker PID.

The immutable Query Performance Counter (QPC) deadline received from the SCM daemon is the only execution budget. The containment owner validates its frequency/tick, translates its remaining interval once for native waits, retains the original triple for every QPC recheck, and reserves a separate ten seconds only for cleanup. `KernelContainmentEvidenceV1` requires the exact three inherited standard-handle roles, zero foreign-process operations and the ordered worker-signal/exit/process-close/Job-zero/readers/handles settlement sequence. Missing or unequal evidence fails closed and quarantines.

Authorization: only the exact process handle returned by the broker's `CreateProcessW`, its matching broker token/session/package/parent and its exact invocation Job may reach request work. No PID enumeration, foreign/pre-existing CST observation, retry, detached spawn, alternate Job owner, `jobs.py`, or `internal/process` edge exists.

## Evidence

| Gate | Receipt |
|---|---|
| MCP before edit | Exact query resolved `_invoke_atomic_job_process -> validate_create_process_spec -> build_create_process_spec`, `WindowsContainedInvocation`, callers and existing coverage. No stale banner. |
| RED | `uv run pytest tests/test_cst_saved_field_t08_containment.py -q --tb=short` → exit 1: atomic tuple oracle passed; six tests failed on absent `WorkerIdentityV1`, containment evidence and injected QPC deadline. |
| T08 GREEN | New T08 synthetic matrix: 7 passed. |
| Containment/worker GREEN | Containment owner, worker and worker-protocol tests: 47 passed, including the safe real Win32 worker/Job/handle probe. |
| Focused regression | T03–T08 plus broker/worker/containment protocol, pipe and client surfaces: 103 passed. No caller regression remains. |
| Static | Ruff check PASS; Ruff format check PASS (`3 files already formatted`); scoped `git diff --check` PASS. |
| MCP after edit | Exact query resolved current `WorkerIdentityV1`, `KernelContainmentEvidenceV1`, `WindowsContainedInvocation`, validation callers and T08 tests. No stale/disabled banner. Irrelevant Go blast-radius candidates were rejected. |

## Changed paths

- `src/mcphub_em_mcp/cst_saved_field_containment_windows.py`
- `tests/test_cst_saved_field_containment_windows.py`
- `tests/test_cst_saved_field_t08_containment.py`

Wire-level effect: no broker-worker JSON field changes. The internal containment call now requires the unchanged `QpcDeadlineV1` instead of constructing a local float deadline, and `KernelInvocationResult` carries broker-local kernel evidence. Named consumers are `ContainedSamplerRunner`, synthetic kernels and containment result validation. Failures remain fixed `ContainmentFailure` identifiers; there is no retry.

Rollback: reverse these three T08 path deltas together. No live CST, SCM service, hub, fleet, configuration, index, commit, push, deployment or registration was touched.

## Terms and Abbreviations

- **PID** — process identifier.
- **QPC** — Windows Query Performance Counter.
- **SCM** — Windows Service Control Manager.
- **PASS** — all phase acceptance criteria and focused regression checks are green.

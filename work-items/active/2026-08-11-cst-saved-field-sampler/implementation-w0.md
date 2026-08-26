# W0 Backend Package — Dirty Candidate and Executable Gap Baseline

Execution role: `$backend-engineer`

Plan: `484883EDBAD02333162C61FAF78B99AA56C402FD64D5955F0E6B65BDDEC82E14`

Accepted design: `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`

Accepted decision: `8EAD15D041781A05ED192107D997693E27B79175C0D0125BB09F1C0A6DE8696A`

## Receiving-side echo

- Task understood: execute W0 only; preserve and classify the entire dirty candidate, capture a recoverable binary/hash inventory, prove the current Go and Python ownership paths with fresh CodeGraph evidence, record the missing native/runtime/composition/receipt behavior as deterministic RED tests, and capture the existing-six baseline.
- Inputs accepted: the immutable plan, design, decision, and source candidate named above.
- Output: one focused RED test file, this implementation package, and raw local evidence under `/.scratch/cst-w0-20260813/`.
- Constraints preserved: no production implementation, no Git index mutation, no reset/checkout/stash, no broad formatter, no live CST, Service Control Manager (SCM), App Control, Virtual Hard Disk (VHDX), Code Integrity tool (CiTool), registration, deployment, publication, or push.
- Assumptions: none drive the RED verdict. Every asserted gap was reproduced against the current candidate.

## Candidate identity and recovery inventory

| Fact | Current W0 evidence |
|---|---|
| Git commit | `43fee019d46c69522ebe79be952d5f139bd4854f` |
| Staged paths | `0` |
| Unstaged paths | `17` |
| Untracked paths | `46` |
| Total pre-edit dirty paths | `63` |
| Binary unstaged patch | `/.scratch/cst-w0-20260813/tracked-unstaged.patch`; SHA-256 `6E6C1C269E073B4C5B90663BB0234A39EC78B923AC9E31D66AA3BFBB9D84A4FC` |
| Binary staged patch | empty by construction; SHA-256 `E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855` |
| Per-file inventory | `/.scratch/cst-w0-20260813/inventory.json`; SHA-256 `C531CAE9456F7DBC1C24C75B0B7ABCBC16BE9ACBD1EE1F524FD5569AE322D907` |
| Preservation recheck | All 63 inventoried pre-edit files rehashed after RED creation; changed count `0` |

The inventory records each exact path, state, byte length, and SHA-256. It is the W6 byte-for-byte comparison oracle.

## Complete dirty-path classification

Classification is path-level. Some `owned-current` Python files contain a superseded injected-production hunk retained as evidence; W0 does not delete or rewrite it. W1-W6 must replace that hunk through the accepted native/composition route without discarding the current containment receipt corrections in the same file.

### `owned-current`

- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_broker_client_windows.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_broker_service_windows.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_broker_worker.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_containment_windows.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_daemon_service_windows.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_integration.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_t08_containment.py`
- `work-items/active/2026-08-11-cst-saved-field-sampler/architecture-review.md`
- `work-items/active/2026-08-11-cst-saved-field-sampler/design.md`
- `work-items/active/2026-08-11-cst-saved-field-sampler/plan.md`
- `work-items/active/2026-08-11-cst-saved-field-sampler/security-constraints.md`
- `work-items/active/2026-08-11-cst-saved-field-sampler/security-review-design.md`
- `work-items/active/2026-08-11-cst-saved-field-sampler/status.md`
- `work-items/decisions/2026-08-12-cst-saved-field-authority-containment.md`

### `owned-superseded`

- `servers/electromagnetics-mcp/tests/test_cst_saved_field_t15_production_composition.py`
- `work-items/active/2026-08-11-cst-saved-field-sampler/implementation-architecture-review.md`
- `work-items/active/2026-08-11-cst-saved-field-sampler/implementation-t13.md`
- `work-items/active/2026-08-11-cst-saved-field-sampler/implementation-t14.md`
- `work-items/active/2026-08-11-cst-saved-field-sampler/implementation-t15-correction.md`

These remain historical/bounded evidence. In particular, the T15 composition test proves Python object injection, which the accepted native production route supersedes; it is preserved and is not accepted as deployment evidence.

### `unrelated-preserve`

- `internal/api/port_alloc_excluded_windows.go`
- `work-items/README.md`
- all five files in `work-items/active/2026-08-11-pr598-review-fix/`: `agent-runs.jsonl`, `closure.md`, `implementation.md`, `qa.md`, `status.md`
- all five files in `work-items/active/2026-08-11-pr600-review-fix/`: `agent-runs.jsonl`, `closure.md`, `implementation.md`, `qa.md`, `status.md`
- all five files in `work-items/active/2026-08-12-work-items-registry-reconciliation/`: `bug-dispositions.md`, `decision-dispositions.md`, `portfolio-dispositions.md`, `registry-audit.md`, `status.md`
- all seventeen files in `work-items/archive/2026-08/2026-08-10-windows-console-opt-in/`: `agent-runs.jsonl`, `architecture-review.md`, `brief.md`, `closure.md`, `design.md`, `implementation-ab.md`, `implementation-cd.md`, `integration-e-design-correction.md`, `integration-e-security-correction.md`, `integration-e.md`, `plan.md`, `qa.md`, `research.md`, `roadmap.md`, `runner-bootstrap.md`, `status.md`, `task-memory-audit.md`
- all five files in `work-items/archive/2026-08/2026-08-12-supervisor-liveness-recovery/`: `bug-dispositions-receipt.json`, `bug-dispositions.json`, `closure.md`, `qa.md`, `status.md`
- `work-items/bugs/2026-08-12-autostart-status-misses-exported-task-path-drift.md`
- `work-items/bugs/2026-08-12-cst-frontend-capability-lands-on-uvx-wrapper.md`
- `work-items/bugs/2026-08-12-cst-native-bootstrap-loadtime-dll-precedes-revocation.md`
- `work-items/bugs/2026-08-12-cst-worker-capability-prebootstrap-site-execution.md`
- `work-items/bugs/2026-08-12-scheduler-upgrade-cannot-own-maintenance-tasks.md`

The exact inventory is authoritative for counts and path spelling.

## Fresh CodeGraph ownership evidence

`codegraph status`, `codegraph sync`, then `codegraph status` reported the index current. A subsequent exact query returned the current source/call path:

1. `StdioHost.Start` constructs the existing `exec.Cmd`, creates its three standard pipes, calls `prepareLaunchCapability`, then `windowsLaunchCapabilityPipe.apply`, and finally `exec.Cmd.Start` (`internal/daemon/host.go:276-365`, `internal/daemon/launch_capability.go:45-105`, `internal/daemon/launch_capability_windows.go:103-112`). No new raw Go launch owner was found.
2. The CLI composition remains `cstLaunchCapabilityConfig` (`internal/cli/daemon.go:411-433`).
3. Current daemon and broker `main` functions take optional Python composition objects and reject `None` (`cst_saved_field_daemon_service_windows.py:295-314`, `cst_saved_field_broker_service_windows.py:497-526`). The worker has the same injected-only shape. This is evidence, not an accepted production route.
4. The existing containment correction returns a typed receipt and the broker maps its observed fields, but the accepted direct-image/native pre-main/worker-capability/broker-transport receipt chain is absent.

CodeGraph briefly emitted pending-file banners caused by stale watcher metadata. The orchestrator independently proved the named files byte-stable over two snapshots; a forced sync/status remained fresh. No stale response drives a W0 conclusion.

## RED package

Added only `servers/electromagnetics-mcp/tests/test_cst_saved_field_w0_gap_baseline.py`, SHA-256 `065CE479AF32B82B564AA46E8A7E8CFBEED8E7AF0C3DC97B76BF38C62457B75D`.

Focused command:

```powershell
uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_w0_gap_baseline.py
```

Result: **9 failed**, exit `1`, in `7.1 s`; every failure is the named assertion, with no import or fixture error. Raw evidence: `/.scratch/cst-w0-20260813/w0-red.txt`, SHA-256 `FFF78C80D6CCF13AF293CA41B3974DD453A070F54C8E1692EC9B0B10BE19FC92`.

| RED ID | Reproduced gap |
|---|---|
| `test_w00_red_package_owned_native_runtime_is_materialized` | `native/cst-runtime/` and `mcphub-cst-runtime.exe` absent |
| `test_w00_red_production_roots_do_not_require_python_object_injection` | daemon, broker and worker roots each expose an injected composition parameter |
| `test_w00_red_cross_process_receipt_owner_types_exist` | `BrokerExchangeReceiptV1`, `WorkerPreMainReceiptV1`, `WorkerCapabilityReceiptV1`, and `BrokerCapabilityReceiptV1` absent from their owners |
| `test_w00_red_go_launch_owner_requires_exact_direct_image_receipt` | current Go launch owner contains no `cst-direct-v1` exact-image receipt |

## Existing-six and baseline checks

| Check | Result |
|---|---|
| `uv run --frozen --python 3.13 pytest -q tests/test_servers.py tests/test_stdio.py` | PASS, 6 tests in 39.2 s; raw SHA-256 `117A6225D62E441CEF027BD1EDC25EC818896235E4F96A08DE806E7CEC7A1E21` |
| Existing-six semantic oracle | CodeGraph confirmed the separate frozen oracle names all three HFSS and three CST schema hashes plus CST body hashes in `tests/test_cst_saved_field_contract.py:13-25,81-108`; W0 made no production mutation |
| `git diff --check` | PASS before W0 test addition |
| `go test ./internal/daemon ./internal/api ./internal/cli -count=1` | PARTIAL: daemon PASS in 76.248 s; combined command timed out at 124.1 s before api/cli terminal results |
| `go test ./internal/api -count=1` | UNVERIFIED: timeout at 124.0 s, no terminal output |
| `go test ./internal/cli -count=1` | UNVERIFIED: timeout at 124.0 s, no terminal output |
| Owned-process settlement | Fresh process probe after both timeouts found zero matching Go/test/Python processes |

The Go timeout is an adjacent verification finding, not hidden as PASS and not patched in W0. It does not contradict any W0 RED assertion; W1 or the integration owner must choose a bounded package/test selector or diagnose the long-running package before relying on a broad Go green claim.

## Contract and behavior statement

No HTTP, remote procedure call (RPC), database, queue, cache, command-line, schema, status-code, authorization, pagination, retry, or persistence contract changed. No production code changed. The only behavioral addition is a deliberately RED test oracle.

## Acceptance reconciliation

| Criterion | Result |
|---|---|
| `W00-AC01` | PASS — exact commit and all pre-edit staged/unstaged/untracked paths captured and classified |
| `W00-AC02` | PASS — binary patch plus per-file SHA inventory captured; post-RED rehash changed count is zero |
| `W00-AC03` | PASS — fresh CodeGraph bound the single Go spawn owner and current Python intended topology; no duplicate raw Go launcher admitted |
| `W00-AC04` | PASS — 9 deterministic assertion failures reproduce native runtime, production-root, direct-image and receipt-chain gaps |
| `W00-AC05` | PASS — prescribed six-test stdio/server baseline is green and the frozen six schema/body oracle remains identified and untouched |

## Risks, adjacent findings, rollback and next gate

- Residual risk: broad Go `internal/api` and `internal/cli` baselines lack terminal evidence because both exceeded the explicit 120-second bound. Treat them as UNVERIFIED until a bounded diagnostic run completes.
- Adjacent findings: none beyond that verification latency.
- Rollback: remove only `tests/test_cst_saved_field_w0_gap_baseline.py` and this artifact. Scratch evidence is local-only. No production or unrelated path needs restoration.
- First W1 step: extend the focused RED surface with the independent native PE verifier contract from `W01-AC01`; do not implement the runtime until the named test demonstrably fails for a structural PE assertion.

Gate: PASS

## Terms and Abbreviations

- CodeGraph: repository code-intelligence index used for current source and call-path evidence.
- RED: a test deliberately demonstrated failing before implementation.
- SCM: Windows Service Control Manager.
- VHDX: Virtual Hard Disk version 2 format.
- CiTool: Windows Code Integrity policy management tool.
- PE: Windows Portable Executable image format.
- RPC: Remote Procedure Call.

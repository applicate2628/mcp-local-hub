# Delivery Plan — CST Saved-Field Sampler Working Result and Target Promotion

Status: implementation-ready, default-off
Plan owner: `$planner`
Integration owner: `$backend-engineer` through W6; `$lead` owns handoffs and acceptance
First implementation owner: `$backend-engineer`
Working-result boundary: executable local candidate, real process/pipe/handle lifecycle, synthetic authority and vendor ports, no live registration
Target-promotion boundary: HSM signing, App Control, VHDX, CiTool, live SCM, installed CST, Line10, manifest pin, publication and deployment

## Immutable accepted inputs

| Artifact | SHA-256 | Authority |
|---|---|---|
| `design.md` | `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED` | Normative architecture and Change-Surface Contract |
| `work-items/decisions/2026-08-12-cst-saved-field-authority-containment.md` | `8EAD15D041781A05ED192107D997693E27B79175C0D0125BB09F1C0A6DE8696A` | Accepted decision |
| `architecture-review.md` | `0A64428AA74EA930A0630341C768B0001D45865263CB2D4336D8A16D59D053DC` | Independent architecture acceptance |
| `security-constraints.md` | `FDE2B842E41C9F771109DC233C34F9C3E5A767EDF40700FBCADEAF7416D4A845` | Accepted security constraints |
| `security-review-design.md` | `6FB1D54E980A4DEAE0861F94AD81DE1B7B9E0583A401CC2F7C5E2C3267CE7488` | Independent design-security acceptance |
| Source candidate | Git `43fee019d46c69522ebe79be952d5f139bd4854f` plus explicitly reconciled in-scope uncommitted corrections | Starting implementation evidence, not an accepted implementation |

Any material change to an accepted artifact invalidates this plan. CodeGraph was queried after recovery and was fresh for the actual Go launch owner (`HostConfig`, `StdioHost.Start`, `prepareLaunchCapability`, `windowsLaunchCapabilityPipe.apply`, `cstLaunchCapabilityConfig`). It showed that the Python service and worker `main` functions still require injected composition and that no package-owned native runtime exists. Those are implementation gaps, not permission to change the accepted architecture.

## Outcome split

### Working result

W0–W10 produce a review-bound local candidate with all of these observable facts:

1. `mcphub-cst-runtime.exe` is built from the admitted no-CRT, KERNEL32-only source and both fixed roles execute far enough to prove real pre-entry handle revocation and fail-closed package admission.
2. The existing Go `exec.Cmd` owner can launch the exact frontend image with exactly three standard handles plus one capability handle; no wrapper, PATH lookup, raw parallel launcher, or extra inherited handle exists.
3. Enrollment, frontend, daemon, broker and worker composition uses real local process/pipe/handle closure and locally derived receipts. No hardcoded, caller-supplied, or test-fabricated settlement receipt is accepted by the production path.
4. Policy absent, disabled, invalid, or target-enforcement-unsettled keeps `cst_sample_saved_field` absent and performs zero source, workspace, vendor, CST, persistence, network, or background work.
5. Synthetic authorized tests traverse the exact four schemas and three sampler endpoints through injected non-live authority/vendor ports; they prove protocol, deadline, containment, transfer, receipt and cleanup behavior without claiming App Control, SCM, CST, HSM or Line10 acceptance.
6. The current live manifest pin and installed fleet remain unchanged. Existing-six compatibility is proved against test/A-B fixtures but direct-bootstrap promotion remains target-dependent.

This is a working, shippable code result, not enterprise registration or release authorization. A local green result cannot stand in for the later target evidence.

### Target promotion and release

X1–X6 add and verify the enterprise signing/provisioning chain, exercise disposable live target state, prove installed CST and Line10 claims, migrate the immutable pin, publish with fresh human approval, deploy default-off, and verify rollback. No target-only claim is promoted earlier.

## Fixed change surface

| Owner | Allowed files/modules |
|---|---|
| Native runtime and package closure | New `servers/electromagnetics-mcp/native/cst-runtime/` entity-owned subtree: freestanding entry/prelude, checked post-revocation loader, independent PE/closure manifest tooling, deterministic build and verifier. Root `build.ps1` and existing package/release materialization only where required to include the reviewed bundle. |
| Existing Go launch owner | `internal/daemon/host.go`, `internal/daemon/launch_capability.go`, `internal/daemon/launch_capability_windows.go`, `internal/daemon/launch_capability_other.go`, `internal/cli/daemon.go`, accepted bounded `internal/api` enrollment/supervisor-status owners and their focused tests. Preserve `exec.Cmd`; do not create a second process owner. |
| Python frontend and services | `cst.py`; accepted new `cst_saved_field_frontend_protocol.py`, `cst_saved_field_daemon_client_windows.py`, `cst_saved_field_hub_enrollment_windows.py`, `cst_saved_field_daemon_service_windows.py`, `cst_saved_field_broker_protocol.py`, `cst_saved_field_broker_client_windows.py`, `cst_saved_field_broker_service_windows.py`. |
| Python worker/domain | Accepted saved-field policy, broker-worker protocol, containment, worker, transfer/path safety, application, neutral port, vendor and vendor-isolation modules; sampler-only additive changes in `safety.py` and `strict_fastmcp.py`. |
| Target provisioning | Accepted `policy/windows/author_exact_appid_policy.ps1`; accepted Windows-only P18 verifier/import/provisioning owner under `internal/cli`; manifest schema/materialization/provisioning; exact tests. |
| Final docs/pin | `servers/electromagnetics-mcp/README.md`, focused API/operator docs, and `servers/cst/manifest.yaml` only after X4. |
| Protected | Existing six CST names, schemas, behavior, errors, outputs and application calls; `jobs.py`; `cst_results.py`; HFSS; solve/history/mesh/1D; hub routing/filter and transport/port/client/daemon shape; `internal/process`; retained bundles; foreign processes; unrelated dirty worktree. |

No phase may add a third-party package dependency, wrapper fallback, direct frontend-to-broker or daemon-to-CST route, test-only production branch, fourth sampler pipe, second Job owner, same-principal write fallback, or fake settlement.

## Execution law

- Phases are serial. A phase starts only after its predecessor's acceptance criteria and fresh evidence are recorded.
- Every implementation phase is RED-first: the named falsifier must demonstrably fail before the behavior is implemented, then pass. A pre-existing green test is not RED evidence.
- W0 records the dirty baseline; no reset, checkout, stash-pop, broad formatter, or overwrite of unrelated changes is permitted. `internal/api/port_alloc_excluded_windows.go`, `work-items/README.md`, other work-items and unrelated archive/bug records are excluded unless separately admitted.
- Go/Python/native source and any test that executes or packages them are shared observed surfaces; they do not run in parallel with mutations to those surfaces.
- W6 is the single working-result integration phase. Failures return to the owning phase; integration does not layer fixes.
- W7 creates one immutable candidate. W8–W10 are independent, sequential checks of that exact candidate. Any change invalidates all later results and restarts at the owning phase.
- X1–X6 are later target phases and create a successor immutable promotion candidate; all W8–W10 checks rerun after target-owned source changes.
- No live SCM, App Control, VHDX, CiTool, installed CST, manifest pin, fleet, publication or deployment mutation occurs before its named X phase.
- Raw logs and proprietary evidence stay under `/.scratch/`; canonical artifacts contain only bounded summaries, hashes, counts and verdicts.

## Retired plan identity

| Retired surface | Disposition |
|---|---|
| Prior `plan.md` SHA-256 `3A0CB9AB98447A7A8ED63B2115F68007A9E…` | Superseded because it predates accepted native pre-entry/package-load closure and the current dirty-candidate gap. The full old text remains in Git/session history, not as live instructions. |
| Prior phases `T00`–`T25` and every `Txx-ACyy` | Retired permanently. They are not executable and their AC identifiers must never be reused. Historical implementation artifacts named `implementation-t*.md` remain bounded evidence only. |
| New identifiers | `Wxx-ACyy` cover the working result; `Xxx-ACyy` cover later target promotion. They are append-only within this plan revision. |

## Working-result phases

### W0 — Dirty-candidate reconciliation and executable gap baseline

Owner: `$backend-engineer`. Depends on immutable inputs only. Mutation scope: focused tests and in-scope source hunks only; no production-behavior change until RED evidence exists.

Endangered diff-invisible invariants: unrelated worktree survives byte-for-byte; accepted seam remains one Go spawn owner and one Python topology; existing six remain unchanged.

Named regression guards: **Existing-wire compatibility guard**, **No-job-edge guard**, **Protocol drift guard**, **Publication guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| W00-AC01 | Record `HEAD=43fee019d46c69522ebe79be952d5f139bd4854f`, staged-name set, unstaged-name set and untracked-name set; every path is classified `owned-current`, `owned-superseded`, or `unrelated-preserve` against the accepted Change-Surface Contract. |
| W00-AC02 | Preserve a binary patch/hash inventory under `/.scratch/` before editing. Every `unrelated-preserve` path has the same SHA-256 after W6 as at W0. |
| W00-AC03 | CodeGraph identifies the current `StdioHost.Start -> prepareLaunchCapability -> windowsLaunchCapabilityPipe.apply -> exec.Cmd.Start` path and `cst.py -> daemon client -> SCM daemon -> broker -> contained worker` intended path; any duplicate raw launch path is marked for deletion, not retention. |
| W00-AC04 | RED probes fail for the actual gaps: native runtime/bundle absent; production daemon/broker/worker entrypoints unavailable without injected composition; no exact direct-image package receipt; no real end-to-end local receipt chain. |
| W00-AC05 | Existing-six schema/stdio/error fixture baseline is captured before source mutation, and no empty/no-op result can satisfy it. |

Verification:

```powershell
git rev-parse HEAD
git status --short
git diff --check
go test ./internal/daemon ./internal/api ./internal/cli -count=1
Push-Location servers/electromagnetics-mcp
uv run --frozen --python 3.13 pytest -q tests/test_servers.py tests/test_stdio.py
Pop-Location
```

Expected fresh evidence: exact commit, classified path table, SHA inventory, CodeGraph query result, named RED tests and existing-six baseline counts. Rollback: standalone test/evidence changes; source remains at captured W0 state.

### W1 — RED contracts for native runtime, production composition and receipts

Owner: `$backend-engineer`. Depends on W0. Scope: focused Go/Python/native verifier tests only.

Endangered diff-invisible invariants: no fake settlement; no production path accepts injected composition as deployment; no capability bytes in environment/argv/log; exactly three endpoints/four schemas.

Named regression guards: **Enrollment lifecycle/handle-inventory guard**, **Split-receipt event-order guard**, **Protocol drift guard**, **Canary-redaction guard**, **Atomic-inheritance guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| W01-AC01 | RED native test requires AMD64 custom entry, only KERNEL32 direct imports, zero delay/TLS/CLR/bound-import/CRT/package imports, exact mitigations, deterministic unsigned bytes and disassembly through role-required revocation. |
| W01-AC02 | RED Go test requires an exact `cst-direct-v1` image receipt, otherwise-empty compatible `SysProcAttr`, exactly one `AdditionalInheritedHandles` capability and rejection of every conflicting inheritance/token/parent/security field. |
| W01-AC03 | RED production-composition test invokes fixed frontend/daemon/broker/worker entrypoints without passing Python objects and requires real local transport endpoints; injected-object-only `main` behavior fails. |
| W01-AC04 | RED receipt test rejects hardcoded/all-true/caller-supplied receipts and requires locally observed write, terminal frame, flush, ACK, disconnect/close and handle settlement in the owner that observed each fact. |
| W01-AC05 | RED default-off test proves absent/disabled/invalid/unsettled policy yields six tools, zero extra process/pipe/source/workspace/vendor/CST/persistence calls, and no background listener. |

Verification:

```powershell
go test ./internal/daemon ./internal/api ./internal/cli -run 'Test.*(CstDirect|LaunchCapability|Enrollment|InheritedHandle)' -count=1
Push-Location servers/electromagnetics-mcp
uv run --frozen --python 3.13 pytest -q tests/test_cst_native_runtime.py tests/test_cst_saved_field_t15_production_composition.py tests/test_cst_saved_field_integration.py
Pop-Location
```

Expected fresh evidence: named tests fail for the asserted missing behavior, not import errors or missing fixtures. Rollback: standalone W1 tests.

### W2 — Native runtime and immutable package-load closure

Owner: `$toolchain-engineer`. Depends on W1. Scope: `servers/electromagnetics-mcp/native/cst-runtime/`, its tests, and the minimum product/package build materialization seam; no manifest pin.

Endangered diff-invisible invariants: no package/user code before handle revocation; no DLL/TLS callback before policy admission; exact package namespace/content continuity; no ambient Python path.

Named regression guards: **Pre-entry loader-closure guard**, **Package code-admission and namespace guard**, **Atomic-inheritance guard**, **Canary-redaction guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| W02-AC01 | The pinned MSVC build emits one no-CRT `mcphub-cst-runtime.exe` with fixed `frontend` and `worker` roles and the exact flags/import/section/mitigation contract from the design; independent parser and `dumpbin` agree. |
| W02-AC02 | First executable instructions clear and read back frontend four handles or worker standard three, then worker capabilities two, before parsing authority, loading DLLs, initializing Python, creating threads/children or calling package code. |
| W02-AC03 | Independent PE traversal emits the closed ordered CPython/package/System32 manifest; missing, extra, reordered, ambiguous or dynamically discovered package modules fail closed. |
| W02-AC04 | `NativePackageLoadOwner` admits only an already committed exact App Control/VHDX package-set receipt. Without it the executable returns `native_loader_invalid`, performs zero Python/source/vendor/CST/descendant work, closes/zeros all resources, and never weakens search/share/DACL policy. |
| W02-AC05 | `PyConfig` is isolated with `site_import=0`, `user_site_directory=0`, `safe_path=1`, `use_environment=0`, `parse_argv=0`, and exact manifest paths; hostile `PYTHON*`, cwd, PATH, registry, `.pth`, sitecustomize and usercustomize inputs have zero influence. |
| W02-AC06 | Two clean unsigned builds are byte-identical; signed-structure verification is defined but remains target-unfulfilled until X1/X2. |

Verification:

```powershell
pwsh ./servers/electromagnetics-mcp/native/cst-runtime/build.ps1 -Clean -Unsigned
pwsh ./servers/electromagnetics-mcp/native/cst-runtime/verify.ps1 -Unsigned
Push-Location servers/electromagnetics-mcp
uv run --frozen --python 3.13 pytest -q tests/test_cst_native_runtime.py -k 'pe_loader_closure or preentry or postrevocation or deterministic'
Pop-Location
```

Expected fresh evidence: two build hashes, PE/parser agreement, disassembly markers, hostile-search matrix, exact fail-closed exit/resource receipt. Rollback: W2 is one atomic native-runtime/build-materialization group.

### W3 — Existing Go launch-owner integration

Owner: `$backend-engineer` (Go). Depends on W2. Scope: accepted Go launch/enrollment/status files and tests only.

Endangered diff-invisible invariants: `exec.Cmd` remains the sole frontend creator; installed Go produces exact HANDLE_LIST; non-CST launch behavior remains identical; capability cancellation covers all returns.

Named regression guards: **Supervisor kernel-binding/status-only authorization guard**, **Enrollment lifecycle/handle-inventory guard**, **Atomic-inheritance guard**, **Existing-wire compatibility guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| W03-AC01 | `cstLaunchCapabilityConfig` admits only exact Windows CST/default identity and resolves a protected absolute `cst-direct-v1` runtime/image receipt; absence or mismatch remains default-off/fail-closed with no wrapper substitution. |
| W03-AC02 | `windowsLaunchCapabilityPipe.apply` rejects non-empty/conflicting process attributes and results in exactly stdin, stdout, stderr and one capability handle in the child HANDLE_LIST; environment contains only the decimal locator. |
| W03-AC03 | Capability is CNG-generated, digest-enrolled, written exactly 32 bytes plus EOF, zeroed, cancelled/consumed once and every parent handle closes on create/enroll/write/start/cancel/timeout/shutdown/exit paths. |
| W03-AC04 | Parent verifies the held runtime identity immediately before `exec.Cmd.Start`; the child proves four flags cleared before capability use. |
| W03-AC05 | Non-Windows and non-CST paths preserve prior behavior; no change reaches `internal/process`, HTTPHost or unrelated daemons. |

Verification:

```powershell
go test ./internal/daemon ./internal/api ./internal/cli -run 'Test.*(LaunchCapability|Enrollment|CstDirect|HandleList|StdioHost)' -count=1
go test ./internal/daemon ./internal/api ./internal/cli -count=1
go test ./...
go vet ./...
```

Expected fresh evidence: exact handle inventory, all-return cancellation table, direct-image identity, non-CST baseline. Rollback: atomic W3 Go integration group; W2 remains inert/unselected.

### W4 — Production frontend, enrollment, daemon and broker composition

Owner: `$backend-engineer` (Python services). Depends on W3. Scope: accepted frontend/enrollment/daemon/broker protocol, client/service and `cst.py` files; no live SCM.

Endangered diff-invisible invariants: exactly three sampler endpoints/four schemas; daemon owns admission; broker alone owns source/workspace authority; each receipt asserts only locally observed facts.

Named regression guards: **In-server authority guard**, **Protocol drift guard**, **Split-receipt event-order guard**, **Three-descriptor readback guard**, **Quarantine linearization guard**, **Validation-channel guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| W04-AC01 | Fixed entrypoints construct their dependencies from closed local configuration and native handles, not caller-injected Python objects; missing production dependency returns a stable unavailable failure and no partial listener. |
| W04-AC02 | Enrollment authenticates current supervisor/current frontend, consumes one pending digest/challenge pair, and exposes no reusable secret or generic control/status authority. |
| W04-AC03 | Frontend registers the seventh tool only after restart-loaded enabled inventory and endpoint/package proof; absent/invalid/disabled/unsettled state leaves exact six-tool inventory with zero side effects. |
| W04-AC04 | SCM daemon alone owns the unchanged QPC triple and `SamplerAdmissionGate`; broker independently reauthorizes exact revision/entry/manifest and alone opens source/workspace authority. |
| W04-AC05 | Real local transport derives `DaemonResponseReceiptV1`, `FrontendTransportReceiptV1` and broker receipts from observed framing/flush/ACK/disconnect/handle events in exact order; fake or cross-owner facts fail and quarantine. |
| W04-AC06 | Deterministic queued-waiter, disconnect, timeout, crash, shutdown and restart matrices settle once and cannot return success early. |

Verification:

```powershell
Push-Location servers/electromagnetics-mcp
uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_t03_contracts.py tests/test_cst_saved_field_integration.py tests/test_cst_saved_field_t15_production_composition.py
uv run --frozen --python 3.13 ruff check src tests
uv run --frozen --python 3.13 ruff format --check src tests
Pop-Location
```

Expected fresh evidence: real local endpoint trace, schema snapshots, receipt event order, disabled side-effect counters and deterministic quarantine schedule. Rollback: atomic W4 frontend/service group; W3 sees enrollment unavailable and remains fail-closed.

### W5 — Worker containment, transfer, vendor boundary and settlement

Owner: `$backend-engineer` (Windows/Python runtime). Depends on W4. Scope: accepted worker protocol, containment, worker, transfer/path, policy, application, port, vendor/isolation and sampler-only safety files.

Endangered diff-invisible invariants: worker Job membership is atomic; source bytes remain immutable; write-capable CST work uses distinct principal; foreign CST survives; success follows complete settlement.

Named regression guards: **Sole-Job-handle guard**, **Contained-duration guard**, **Complete-manifest transfer guard**, **Trusted-root capability guard**, **Workspace-transaction guard**, **Namespace identity guard**, **Vendor-byte capability-continuity guard**, **Foreign-process guard**, **Settlement-order guard**, **Neutral-port guard**, **Vendor-record guard**, **Finite-budget guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| W05-AC01 | Broker creates one exact native worker atomically in its preconfigured kill-on-close Job with ordered five-handle list, no breakaway, no console, bounded streams and one exclusive inheritance epoch. |
| W05-AC02 | Worker proves all five inherited flags cleared before request/source/vendor/CST/descendant work; failed proof yields zero privileged work and quarantine. |
| W05-AC03 | Shared closed Windows path grammar rejects reserved/superscript aliases, ADS, remote/device/8.3/reparse/hardlink/case/NFC/stream ambiguity before CST; canonical drive-colon input remains admissible. |
| W05-AC04 | Every manifest-v2 row is copied from held source capabilities into a protected workspace and exact destination equality is proved; drift/add/remove/rename/replace fails before vendor start. |
| W05-AC05 | `open_owned_sampler_session` and `AuthorizedVendorPathLease` retain exact ownership through output share-zero seal, cache/session close, worker settlement and broker root deletion; no default receipt field or path fallback exists. |
| W05-AC06 | Timeout/cancel/broker crash/service stop closes worker references, proves Job active zero, joins readers and closes handles; foreign/pre-existing CST remains live and never joins the Job. |
| W05-AC07 | Vendor call order remains Result3D/header/ResultTree/sample/cache-clear/close; solve/history/mesh/1D calls remain zero. |

Verification:

```powershell
Push-Location servers/electromagnetics-mcp
uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_t08_containment.py tests/test_cst_saved_field_containment_windows.py tests/test_cst_saved_field_integration.py tests/test_cst_saved_field_vendor.py
uv run --frozen --python 3.13 ruff check src tests
uv run --frozen --python 3.13 ruff format --check src tests
Pop-Location
```

Expected fresh evidence: exact Job/handle trace, transfer drift matrix, namespace property matrix, vendor/lease receipt ordering and foreign-process sentinel. Rollback: atomic W5 worker/containment/domain group; W4 must stay disabled when W5 is absent.

### W6 — Executable default-off local integration

Owner: `$backend-engineer` as named integration owner. Depends on W5. Scope: cross-owner integration tests and minimum composition wiring within accepted files; no live target state.

Endangered diff-invisible invariants: one route only; no fake settlement; default-off has zero side effects; existing six and hub topology are unchanged; final MCP text is bounded and redacted.

Named regression guards: **Existing-wire compatibility guard**, **Validation-channel guard**, **Protocol drift guard**, **MCP-boundary budget guard**, **Canary-redaction guard**, **Publication guard**, plus every W2–W5 guard at the integrated boundary.

| AC ID | Observable acceptance criterion |
|---|---|
| W06-AC01 | Actual native frontend child and actual local pipes traverse Go capability delivery and fixed production entrypoints; process exit, handle closure and transport receipts are observed, not injected. |
| W06-AC02 | With policy absent/disabled/invalid or App Control/VHDX receipt absent, exact six-tool inventory remains and the native route fails closed before package/source/vendor/CST work; current manifest pin is untouched. |
| W06-AC03 | With synthetic, non-live policy/App-Control/vendor ports, exactly one request traverses four schemas and three endpoints and produces one bounded canonical `TextContent`; the evidence explicitly does not claim target policy or CST acceptance. |
| W06-AC04 | Removing any receipt bit, adding an inherited handle/module/schema field, changing the QPC triple, disconnecting at any frame, or leaving any worker/reader/handle active turns the result into stable failure/quarantine with no success publication. |
| W06-AC05 | Existing six schema/error/stdio fixtures equal W0 exactly; no production import/string/config edge references Line10/VFEM, and no solver action occurs. |

Verification:

```powershell
go test ./internal/daemon ./internal/api ./internal/cli -count=1
Push-Location servers/electromagnetics-mcp
uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_integration.py tests/test_cst_saved_field_t15_production_composition.py tests/test_servers.py tests/test_stdio.py
Pop-Location
```

Expected fresh evidence: native child identity/exit, real pipe/handle/receipt trace, disabled zero-side-effect counters, synthetic exact-route result and W0 six-tool equality. Rollback: W2–W6 are an atomic integration revert group; current manifest/live installation never selected them.

### W7 — Full regression and immutable working candidate

Owner: `$qa-engineer` for verification, then `$backend-engineer` for candidate assembly under Lead supervision. Depends on W6. Scope: tests, hygiene and exact admitted source only.

Endangered diff-invisible invariants: all design guards; protected-path zero drift; unrelated worktree hashes preserved; candidate has no secrets/machine paths/raw evidence.

Named regression guards: all named guards allocated in W0–W6, especially **Publication guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| W07-AC01 | Full frozen Python suite, Ruff, format, full Go suite, Go vet, Windows native verifier and non-Windows compile/import checks pass fresh. |
| W07-AC02 | Every named guard has one observed RED and GREEN falsifier; null/missing sub-verdict is failure. |
| W07-AC03 | Candidate diff is exactly the accepted surface; unrelated W0 path hashes match; staged index is assembled only from explicit admitted paths. |
| W07-AC04 | Superseded T15 composition/fake-receipt routes and compatibility aliases are absent from live code; history remains only in the bounded work-item ledger/Git. |
| W07-AC05 | One immutable candidate commit binds source, tests, native build manifest and accepted input SHAs; no manifest pin, live registration, install, push or deployment occurs. |

Verification:

```powershell
go test ./...
go vet ./...
Push-Location servers/electromagnetics-mcp
uv run --frozen --python 3.13 pytest -q
uv run --frozen --python 3.13 ruff check .
uv run --frozen --python 3.13 ruff format --check .
Pop-Location
pwsh ./servers/electromagnetics-mcp/native/cst-runtime/verify.ps1 -Unsigned
git diff --check
python "$env:USERPROFILE\.codex\skills\lead\scripts\check-publication-safety.py"
```

Expected fresh evidence: command/exit/count summary, RED/GREEN matrix, exact candidate SHA, explicit staged/unrelated inventory and clean safety result. Rollback: reset only the candidate commits after confirming no unrelated work depends on them; never reset the dirty user paths.

### W8 — Independent implementation architecture review

Owner: `$architecture-reviewer`. Depends on immutable W7 candidate. Read-only.

Endangered diff-invisible invariants: all 34 accepted architecture claims; exact owner/dependency/route topology; no stale fallback.

Named regression guards: every design guard; reviewer must map each claim to source plus executable falsifier.

| AC ID | Observable acceptance criterion |
|---|---|
| W08-AC01 | Reviewer binds verdict to exact candidate and immutable input SHAs and verifies actual Go/native/Python call paths with fresh CodeGraph. |
| W08-AC02 | Reviewer reconciles every numbered architecture claim, marking Claims 7 and 15 target-only rather than passing them from synthetic evidence. |
| W08-AC03 | Reviewer verifies one spawn owner, exact endpoints/schemas, native load closure, dependency direction, all-return settlement, existing-six protection and zero stale route. |
| W08-AC04 | Any finding returns to its owning W phase and invalidates W8 onward; no advisory wording can authorize advancement. |

Verification: reviewer re-runs the smallest named falsifiers needed to validate source claims and records exact commands/results. Expected fresh evidence: exact-candidate claim matrix and independent verdict. Rollback: none; read-only.

### W9 — Security reconciliation and independent security review

Owner sequence: `$security-engineer`, then independent `$security-reviewer`. Depends on W8 acceptance. Read-only except correction returns to owner.

Endangered diff-invisible invariants: pre-entry and package-code execution barriers; capability/nonce secrecy; principal/path/receipt authority; no control weakening or fake settlement.

Named regression guards: **Pre-entry loader-closure guard**, **Package code-admission and namespace guard**, **Enrollment lifecycle/handle-inventory guard**, **Atomic-inheritance guard**, **Vendor-byte capability-continuity guard**, **Canary-redaction guard**, **Publication guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| W09-AC01 | Security engineer maps every accepted constraint to a concrete owner, source site, test and failure mode on the exact candidate. |
| W09-AC02 | Independent reviewer adversarially tests malicious DLL/TLS/search inputs, extra handles, replay, spoofed identity, pipe squatting, namespace races, writer conflicts, receipt forgery, resource exhaustion and cleanup ambiguity. |
| W09-AC03 | App Control/HSM/VHDX/CiTool/SCM/CST claims remain explicitly unfulfilled target obligations; local fakes cannot waive them. |
| W09-AC04 | No exception or fallback weakens signature/hash/AppID, share mode, DACL, Job, handle-list, QPC, quarantine, receipt or default-off behavior. |

Verification: exact focused tests named in W1–W6 plus independent source/call-path inspection. Expected fresh evidence: constraint matrix and exact-candidate independent verdict. Rollback: none; findings return to owner and invalidate W8–W9.

### W10 — Independent QA and working-result acceptance

Owner: `$qa-engineer`. Depends on W9 acceptance. Read-only against exact candidate.

Endangered diff-invisible invariants: all accepted local claims; target-only claims remain unclaimed; current live state is untouched.

Named regression guards: all W0–W6 guards.

| AC ID | Observable acceptance criterion |
|---|---|
| W10-AC01 | QA independently reruns W7 commands and obtains fresh success bound to the exact candidate. |
| W10-AC02 | QA runs the actual local native/pipe/process default-off route and verifies exact child identity, required revocation markers, stable fail-closed outcome, complete real receipts and zero residue. |
| W10-AC03 | QA proves synthetic authorized integration sensitivity by breaking each boundary once; fake/no-op settlement and both-modes-broken cannot pass. |
| W10-AC04 | QA confirms current manifest pin, SCM, App Control, VHDX, CiTool, installed CST and fleet were not mutated and existing six remain W0-compatible. |
| W10-AC05 | Working-result acceptance statement says exactly: code candidate is executable and default-off locally; enterprise registration, target CST correctness, Line10, publication and deployment remain open. |

Verification: W7 commands plus one recorded local runtime invocation and post-process/handle/artifact residue probe. Expected fresh evidence: exact-candidate QA matrix and bounded working-result acceptance. Rollback: none; a defect returns to owner and invalidates W8–W10.

## Later target-promotion phases

### X1 — Exact AppID policy artifact and offline HSM signing chain

Owner sequence: `$toolchain-engineer`, `$security-engineer`, independent `$security-reviewer`. Depends on W10. Scope: accepted policy author, P18 request/export/import verifier and isolated ceremony interfaces; no target deployment.

Endangered diff-invisible invariants: exact hash/AppID policy semantics; non-exportable HSM key; non-persistent OCS loaded interval; request-bound vendor audit; target has no signing authority.

Named regression guards: **Policy-signing authenticity and loaded-interval guard**, **Package code-admission and namespace guard**, **Canary-redaction guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| X01-AC01 | Pinned Windows PowerShell/ConfigCI/XSD tooling deterministically emits canonical exact-hash/AppID XML and unsigned CIP; direct broken Hash-plus-AppID cmdlet path and broad rules are rejected. |
| X01-AC02 | Isolated HSM ceremony proves exact non-exportable key, K-of-N non-persistent OCS, single signer operation, request-bound vendor-verified audit range and final-card unload before handoff. |
| X01-AC03 | Independent verifier proves exact PKCS #7 content/signature/current chain/revocation, artifact semantics and receipt equality; mismatch yields zero deployment artifact. |
| X01-AC04 | No key/provider/credential/PIN/raw audit/private identifier enters repo, target, logs or canonical artifacts. |

Verification: deterministic two-run artifact hashes, vendor-supported HSM audit export verification, independent signed-content/chain/revocation parser and negative matrices. Expected fresh evidence: sanitized request/artifact/receipt hashes and exact vendor/tool identities. Rollback: unsigned outputs are standalone; signed handoff is quarantined and never deleted until independent verification settles.

### X2 — App Control, read-only VHDX and CiTool lifecycle on disposable target

Owner: `$platform-engineer`; mandatory `$security-engineer` and `$security-reviewer`. Depends on X1. Scope: P18 Windows-only owner and disposable target only.

Endangered diff-invisible invariants: exact package bytes cannot change during mapping/DllMain; supplemental policy never changes ambient policy authority; CiTool consumes one retained read-only identity; reboot-observed state owns settlement.

Named regression guards: **Package code-admission and namespace guard**, **Policy-signing authenticity and loaded-interval guard**, **Publication guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| X02-AC01 | With services stopped and absence proved, P18 builds/verifies one complete VHDX, attaches it read-only/no-drive-letter, retains backing/volume/mount/ancestor/version/file identity and denies write/delete/rename/reparse/ADS races through mapped callbacks. |
| X02-AC02 | P18 independently inventories complete base/supplemental policy family, proves no broader allow for runtime AppID, and applies only the fixed sampler supplemental PolicyID. |
| X02-AC03 | Exact held staged CIP identity remains continuous through one serialized `CiTool --update-policy`; reboot and independent inventory prove committed policy before runtime admission. |
| X02-AC04 | Crash/replay/ambiguity yields quarantine and reconciliation, never a second CiTool call or weakened fallback. Removal uses only sampler PolicyID, reboot, absence proof, then VHDX detach. |

Verification: target pre/post inventory, slow-callback race matrix, malicious DLL denial-before-entry, CiTool/reboot state journal and full rollback absence. Expected fresh evidence: sanitized identities/hashes/events. Safe fallback: leave policy/VHDX attached and sampler quarantined if absence or detach is ambiguous.

### X3 — Disposable SCM, descriptors and installed CST containment

Owner: `$platform-engineer` for SCM plus `$qa-engineer` for target execution. Depends on X2. Scope: disposable services/identities/pipes and copied target project only; no catalog pin.

Endangered diff-invisible invariants: three exact descriptors; current PID/token/session/image authentication; exact native worker tree; foreign CST preservation; no solver.

Named regression guards: **Three-descriptor readback guard**, **Supervisor kernel-binding/status-only authorization guard**, **Sole-Job-handle guard**, **Foreign-process guard**, **Vendor-byte capability-continuity guard**, **Settlement-order guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| X03-AC01 | Disposable services resolve real numeric service SIDs and exact ordered DACL/SACL readback for enrollment/frontend/broker; wrong identity/opcode/generation fails before work. |
| X03-AC02 | Installed CST proves Claim 7 exact activation/header/ResultTree/status path in one fresh contained descendant, no console, no solve/history/mesh/1D. |
| X03-AC03 | Timeout/cancel/service stop/broker crash proves worker/CST disappearance and complete handle/workspace/output settlement while a held foreign CST sentinel remains untouched. |
| X03-AC04 | Pre-state is restored byte-for-byte; service, pipe, policy-output and process residue is zero. |

Verification: disposable SCM descriptor/auth matrix, real CST copied-project smoke, Job accounting and rollback script. Expected fresh evidence: sanitized target trace and exact pre/post digest. Safe fallback: stop services, keep sampler default-off/quarantined, preserve held evidence until absence is proven.

### X4 — Independent native provider, Line10 and existing-six target regression

Owner sequence: independent provider owner, `$qa-engineer`. Depends on X3. Scope: test-only provider/comparator and disposable target; production imports remain absent.

Endangered diff-invisible invariants: production has no Line10/VFEM edge; Claim 15 requires independent producer; existing six preserve exact behavior through direct bootstrap.

Named regression guards: **Existing-wire compatibility guard**, **Solve-path preservation guard**, **Foreign-process guard**, **Publication guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| X04-AC01 | Independent native provider identity/path is proved and does not import/call sampler/vendor implementation; missing or ambiguous provider is failure. |
| X04-AC02 | Four calls cover port 1 E, port 2 E, port 1 H, port 2 H; each field proves 96 local/90 unique rows, required materials/classes, six finite components, units and exact accepted equality without fitting. |
| X04-AC03 | Production dependency scan contains zero Line10/VFEM/provider edge; raw proprietary outputs remain only under `/.scratch/`. |
| X04-AC04 | Existing six direct-bootstrap A/B schema, bytes, errors, stdio, restart and shutdown behavior equals W0/current published behavior; uvx is absent only on the candidate direct route, not silently retained as fallback. |

Verification: independent provider trace, mechanical comparator, real stdio catalogue and representative six-tool success/error corpus. Expected fresh evidence: sanitized hashes/counts/verdicts. Rollback: target-only artifacts and disposable state; production candidate remains default-off.

### X5 — Promotion candidate, documentation, immutable pin and publication

Owner sequence: `$backend-engineer` integration, `$qa-engineer`, `$architecture-reviewer`, `$security-engineer`, independent `$security-reviewer`; publication only by `$lead` after human approval. Depends on X4.

Endangered diff-invisible invariants: pin binds exact reviewed native bundle/policy; docs describe exact topology and rollback; candidate range is clean and leak-free.

Named regression guards: **Publication guard**, all design guards.

| AC ID | Observable acceptance criterion |
|---|---|
| X05-AC01 | Docs describe exact three endpoints/four schemas, default-off behavior, HSM/App Control/VHDX/CiTool/SCM provisioning, enable/restart, quarantine, diagnostics and rollback without stale topology. |
| X05-AC02 | `servers/cst/manifest.yaml` changes only to the accepted immutable `cst-direct-v1` pin/description and binds reviewed package/policy identities while transport/port/client/daemon shape remains equal. |
| X05-AC03 | `pwsh ./build.ps1` produces the admitted Windows product; full W7 checks and independent reviews rerun on one successor immutable promotion candidate. |
| X05-AC04 | Fresh range publication-safety scan, protected-path review, clean index/history and target evidence hashes bind exactly the pending publication range. |
| X05-AC05 | `git push` occurs only after a new user message containing current publication approval; the earlier approval marker is not reusable. |

Verification:

```powershell
pwsh ./build.ps1
go test ./...
go vet ./...
Push-Location servers/electromagnetics-mcp
uv run --frozen --python 3.13 pytest -q
uv run --frozen --python 3.13 ruff check .
uv run --frozen --python 3.13 ruff format --check .
Pop-Location
git diff --check
python "$env:USERPROFILE\.codex\skills\lead\scripts\check-publication-safety.py"
```

Expected fresh evidence: product hash, exact candidate/range, independent review results, safety receipt and current human approval. Rollback: before push, reset only owned candidate commits after dependency check; after publication, use repository release rollback policy without rewriting shared history.

### X6 — Default-off deployment, controlled enablement and rollback proof

Owner: `$platform-engineer`; QA witness; Lead lifecycle close after evidence. Depends on X5 publication.

Endangered diff-invisible invariants: deployment begins default-off; enabled route is exact; disable removes only seventh tool; existing six/fleet/foreign CST remain healthy.

Named regression guards: **Publication guard**, **Existing-wire compatibility guard**, **In-server authority guard**, **Settlement-order guard**, **Foreign-process guard**.

| AC ID | Observable acceptance criterion |
|---|---|
| X06-AC01 | Install starts with sampler policy absent/disabled and seventh tool absent; existing six and hub remain healthy. |
| X06-AC02 | Before enablement, exact services/SIDs/descriptors/native bundle/VHDX/policy/runtime identities are read back and equal reviewed receipts. |
| X06-AC03 | Controlled enable plus required restart exposes exactly the seventh tool and exact accepted route; one target smoke reproduces complete receipts within 60 seconds. |
| X06-AC04 | Disable plus restart removes only the seventh tool, settles active work, stops services in order and preserves existing six/foreign CST. |
| X06-AC05 | Rollback restores prior pin/config/services/descriptors/policy/package state and proves zero sampler process/handle/workspace/pipe/catalog residue; ambiguity is operational failure, not success. |

Verification: installed binary/version/hash, live MCP catalogue and exact route smoke, service/policy/VHDX/CiTool inventory, existing-six checks, disable/rollback post-state. Expected fresh evidence: bounded deployment and rollback receipts. Safe fallback: keep feature disabled/quarantined; never detach/remove/reenable while settlement or absence is ambiguous.

## Risk and rollback summary

| Risk | Stop condition | Recovery |
|---|---|---|
| Dirty-tree collision | Any unrelated hash changes or another lane edits the same admitted source during implementation | Stop, preserve both diffs, return ownership to Lead; never reset/stash-pop/overwrite. |
| Native pre-entry ambiguity | Import/TLS/disassembly/mapped-module/handle-clear evidence is missing or extra | Stop before Python/child/source; close/zero; fix W2 rather than add a fallback. |
| Receipt ambiguity | A success fact is unobserved, caller-supplied, defaulted or owned by another endpoint | Fail and quarantine; fix producing/receiving owner, never synthesize settlement. |
| Target control unavailable | HSM/App Control/VHDX/CiTool/SCM/CST provider is missing, mismatched or cannot prove state | Working candidate remains accepted default-off; target promotion stops without weakening controls. |
| Target cleanup ambiguity | Process/Job/handle/policy/VHDX/service absence is not proved | Keep sampler disabled/quarantined and preserve state/evidence; no retry/detach/re-enable. |
| Review correction | Candidate changes after W7 or X5 | Invalidate downstream reviews and QA; rerun from owning phase on a new immutable SHA. |

## Recommended role sequence

`$backend-engineer` W0–W1 -> `$toolchain-engineer` W2 -> `$backend-engineer` W3–W6 -> `$qa-engineer` W7 -> `$architecture-reviewer` W8 -> `$security-engineer` then `$security-reviewer` W9 -> `$qa-engineer` W10 -> `$toolchain-engineer`/security roles X1 -> `$platform-engineer`/security roles X2 -> `$platform-engineer`/`$qa-engineer` X3 -> independent provider/`$qa-engineer` X4 -> integration plus all independent checks X5 -> `$platform-engineer`/`$qa-engineer` X6 -> `$knowledge-archivist` lifecycle closure.

The immediate next action is W0 under `$backend-engineer`: classify and preserve the dirty candidate, capture RED evidence for the missing native runtime and production composition, then proceed directly to W1.

Gate: PASS

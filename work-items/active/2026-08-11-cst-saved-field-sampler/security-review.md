# C5 Independent Implementation Security Review — CST Saved-Field Sampler

Date: 2026-08-12

## Review Identity and Boundary

| Item | Exact reviewed value |
|---|---|
| Execution role | Independent `$security-reviewer`; the upstream author verdict was treated only as a claim source |
| Immutable candidate | `e5228a305fd0d196faa5d4042d04769e04c3f1bf` |
| Accepted security constraints | `security-constraints.md`; SHA-256 `E3FF52C6F35D617BDA3E774838C4E88441C5195CBB32A11E113ECE33EF17715C` |
| Security-engineer implementation gate | `security-implementation.md`; SHA-256 `DE0E50685F38A9B6AD1859C506FCC7C6E2B3B297A858C458CB381A501520AB0C` |
| Architecture gate | `implementation-architecture-review.md`; SHA-256 `02B0B3C69F6FFE15F0BA6B0BAAB4B70C41E982ED370AD50EE4F83C8088E5A058` |
| QA gate | `qa-implementation.md`; SHA-256 `FBFC4CFE462FA0C1C8918370AAD653E920D72F0F304DE45D4C525A1D5B029092` |
| Reviewed source | Saved-field composition, schema/publisher, policy/ACL, transfer/capability, helper, helper protocol, Windows containment, vendor adapter, and their tests |
| Permitted evidence | Read-only source/static inspection and safe synthetic/unit falsifiers |
| Prohibited and not performed | Source/test/Git/index mutation; live CST, hub, fleet, solver, deployment, publication, or foreign-process action |

The review independently attempted to falsify all 18 upstream security claims.
Two implementation defects reproduced despite the existing 547-test suite
passing. No publication-safety exception is requested or granted.

## Blocking Findings

### SR-C5-01 — ordinary vendor paths break retained-capability authority

| Field | Finding |
|---|---|
| Severity | **high** — authenticated same-user namespace replacement reaches the external CST parser as unauthorized bytes |
| Fix class | **design-decision** |
| Defect class | Trust-boundary capability discontinuity / time-of-check-to-time-of-use authorization bypass |
| Attacker position | Authenticated local tenant or insider able to rename and recreate entries under the owner-controlled workspace namespace |
| Security owner violated | `AuthorizedWorkspaceSnapshot`, which must be the sole capability from which vendor inputs and generated outputs derive |
| Concrete instance | `activate_and_sample` calls `api.open_result3d(handle, copied_payload)` with an ordinary path before capability-backed identity revalidation (`cst_saved_field_vendor.py:468-506`). |
|  | The helper likewise derives `copied_payload` as a path (`cst_saved_field_helper.py:325-355`). |
|  | Capability-backed hashing/copying later uses the retained object, but that cannot undo bytes already consumed by CST through the replaceable path. |
| Reproduction | Deterministic safe falsifier renamed the authorized workspace after transfer, recreated the same name with `attacker-field`, and retained the original as `owned-moved`. |
|  | Result: `api.open_result3d` read `attacker-field`; the retained capability still read `authorized-field`; only a later generated-header check failed closed. |
| Contract impact | SEC-C01, C02, C03, C04, C06 and C14 fail; design Claims 4 and 26 are not implemented. |
| Required WHAT | No external vendor call may open/read/write a name-replaceable object that is not continuously bound to the authorized workspace capability. The deterministic swap must cause zero vendor use of replacement bytes or paths. |
| ADVISORY HOW | Route through `$security-engineer` and the design owner. Candidate mechanisms include non-delete-share handle leases across every path-only vendor call, an immutable read-only snapshot plus separately capability-locked generated-output namespace, or another Windows primitive that makes the external path identity unreplaceable for the complete call. |
|  | Material alternatives differ in CST compatibility, generated-header creation, cleanup, and ownership; reviewer HOW is therefore non-binding. |
| Falsifying guard | A real Windows deterministic swap at `open_result3d`, header generation, and ResultTree registration must prove zero vendor read/write/use of replacement identities while the owned capability remains settleable. |

This finding was missed by prior gates because existing swap tests stop at
source/transfer/component/cleanup owners. The vendor tests pass ordinary paths
and do not swap the workspace at the helper-to-CST boundary.

### SR-C5-02 — successful breakaway is published as breakaway denial

| Field | Finding |
|---|---|
| Severity | **medium** — false containment attestation with the current parent Job configuration as a compensating control |
| Fix class | **inline-sufficient** |
| Defect class | Inverted security predicate / false startup proof |
| Security owner violated | First-instruction helper proof consumed by `WindowsContainedInvocation._validate_startup` |
| Concrete instance | `_synthetic_breakaway_observation` returns `(False, True)` when `CreateProcessW` with `CREATE_BREAKAWAY_FROM_JOB` succeeds (`cst_saved_field_helper.py:72-146`). |
|  | `run_helper` publishes `breakaway_denied = observed_denied OR breakaway_created` (`cst_saved_field_helper.py:482-509`). |
|  | Parent validation accepts any true `breakaway_denied` (`cst_saved_field_containment_windows.py:445-476`). |
| Reproduction | Safe in-memory proof with `breakaway_denied=false`, `breakaway_created=true` emitted `StartupProofV1(..., breakaway_denied=True)`; parent validation returned `accepted`. |
| Official semantics | Microsoft documents that a child created with `CREATE_BREAKAWAY_FROM_JOB` is not associated with the calling process's Job when breakaway is allowed: https://learn.microsoft.com/windows/win32/procthread/process-creation-flags and https://learn.microsoft.com/windows/win32/procthread/job-objects |
| Compensating control | C5's parent Job sets active-process and kill-on-close limits but neither breakaway limit (`cst_saved_field_containment_windows.py:801-810`), so normal C5 creation is intended to deny the probe. |
| Contract impact | SEC-C09, C10 and C17 fail at the startup-proof layer. Architecture Claim 27's proof is false even though target evidence remains separately open. |
| Required WHAT | An observed created breakaway must make startup proof false and quarantine before descriptor construction or source/vendor work. |
| ADVISORY HOW | Keep the existing owner and encode denial only when the breakaway attempt failed for the exact denial reason and no process was created. Add the inverse regression case to the current startup-proof suite. |
| Falsifying guard | Inject `breakaway_denied=false`, `breakaway_created=true`; emitted proof must be false and `WindowsContainedInvocation` must reject with quarantine. |

## Required Fixes Before Merge, Target Admission, or Release

1. Route SR-C5-01 through `$security-engineer` plus the design owner and approve
   one capability-continuous contract for every path-only CST operation.
2. Implement the accepted SR-C5-01 contract and add the deterministic vendor-use
   swap matrix for copied payload, generated header, clean copies, and ResultTree
   registration.
3. Correct SR-C5-02 in the existing helper startup-proof owner and add its inverse
   behavioral guard.
4. Re-run security-engineer, architecture, QA, and this independent
   security-reviewer against a new immutable candidate.
5. File both findings in `work-items/bugs/` before accepting this verdict. The
   dispatch explicitly allowed this reviewer to write only `security-review.md`,
   so the mandatory registry writes remain Lead-owned and were not made here.

## Fresh Independent Evidence

| Probe | Result |
|---|---|
| Candidate and input binding | HEAD exact `e5228a305fd0d196faa5d4042d04769e04c3f1bf`; upstream hashes matched; Git index empty before and after review |
| Full safe package | `uv run --frozen --python 3.13 pytest -q -p no:cacheprovider`: PASS; independent collection: **547 tests** |
| Static and format | `ruff check src tests`: PASS; `ruff format --check src tests`: PASS, 45 files |
| SR-C5-01 falsifier | `VENDOR_OPENED_BYTES=attacker-field`; `AUTHORIZED_BYTES_STILL=authorized-field`; later result: `activation_failed:result3d_header` |
| SR-C5-02 falsifier | `OBSERVED_BREAKAWAY_CREATED=true`; `EMITTED_BREAKAWAY_DENIED=true`; parent validation: `accepted` |
| Dependency/CI surface | Candidate adds no package manifest, lock, or CI path |
| Runtime residue | Zero saved-field helper orphans after checks |
| Foreign CST preservation | Existing PIDs `7624`, `7636`, `10032` retained their 2026-08-09 creation times and were not opened, signaled, or stopped |

The green package is evidence for unaffected controls, not evidence against the
two new falsifiers. Both reproduce on paths absent from the current suite.

## S4 Per-Claim Verdict Matrix

The canonical vocabulary is `verified`, `failed`, or `not-verifiable (with
reason)`. Each upstream claim receives exactly one verdict.

| Claim | Verdict | Independent evidence |
|---|---|---|
| SEC-C01 | **failed** | Same-name replacement not authorized by the retained snapshot reaches `open_result3d`. |
| SEC-C02 | **failed** | Deterministic workspace-name swap supplies replacement bytes to vendor before capability validation. |
| SEC-C03 | **failed** | Vendor write-capable operations receive replaceable paths that can cease to designate the exact lease. |
| SEC-C04 | **failed** | Exact copied identities do not remain the identities consumed by the external vendor path. |
| SEC-C05 | **verified** | Default-off restart snapshot and all-route policy mediation remain covered by the green composition/policy suite. |
| SEC-C06 | **failed** | The admitted method sequence can begin on replacement rather than copied `Result3D`; installed CST semantics remain additionally unverified. |
| SEC-C07 | **verified** | Closed vendor record, finite/type/status and count/metadata matrices pass before selection/allocation. |
| SEC-C08 | **not-verifiable (installed acquisition P12 was not run)** | Synthetic ownership/rollback receipts pass; exact installed CST creation semantics remain a target gate. |
| SEC-C09 | **failed** | Successful breakaway is falsely attested as denied; interleaved foreign-CST target trace remains additionally open. |
| SEC-C10 | **failed** | Startup settlement accepts a false breakaway bit, so complete containment proof is not truthful. Workspace receipt/quarantine matrices otherwise pass. |
| SEC-C11 | **verified** | One `TextContent`, null structured result and exact UTF-8 ceiling tests pass. |
| SEC-C12 | **verified** | Entry/file/byte/point/I/O/deadline/process/waiter/output ceilings pass exact and one-over tests. |
| SEC-C13 | **verified** | Closed error allowlist and caller/vendor/path/environment/security/process canary tests pass synthetic channels. |
| SEC-C14 | **failed** | A replaceable untrusted path selects attacker bytes for external vendor use despite the retained capability. |
| SEC-C15 | **verified** | Literal-false solve control and forbidden shell/solve/remesh/history/save/process-discovery graph guards pass. |
| SEC-C16 | **not-verifiable (release provenance P16-P18 was not run)** | No dependency/lock/CI delta is verified; exact package and publication provenance remain later gates. |
| SEC-C17 | **failed** | False breakaway attestation and replaceable vendor inputs violate implementation preconditions; installed CST/Windows target behavior remains additionally unverified. |
| SEC-C18 | **verified** | Real NTFS owner/DACL/ABI/held-root tests and locality/reparse/ADS/foreign-access denial matrices pass. |

Architecture Claims 7 and 15 remain `not-verifiable`: no installed CST target
activation or independent Line10/native comparison was run. This REVISE does
not upgrade, waive, or replace either target gate.

## Security Surface Checklist

| Surface | Result | Evidence and disposition |
|---|---|---|
| Untrusted input: injection, deserialization, traversal, SSRF | **found** | Path traversal/namespace grammar and closed framing pass, and SSRF is not applicable to this local sampler. SR-C5-01 is a post-admission namespace replacement bypass at the vendor boundary. |
| Object/resource authorization | **found** | SR-C5-01 substitutes object B at the same path after object A was authorized. |
| New or updated dependency | **not-applicable** | No manifest, lock, service, or CI dependency delta. |
| New configuration/default polarity | **verified** | Exact `enabled=true` is required; absent/false/invalid policy does not register the tool. `allow_solve` is literal false. |
| Agent-facing prompt/tool misuse | **not found on reviewed channels** | Prompt-like strings remain bounded data and cannot select a tool/method; path identity bypass is separately SR-C5-01. |
| Secrets and diagnostics | **not found on synthetic channels** | No secret introduced; fixed errors and canary tests pass. Installed CST logs remain target-only and were not run. |
| Process containment and resource cleanup | **found** | SR-C5-02 falsifies breakaway proof. Native ledger, deadline, cancellation, settlement, quarantine, no-console and orphan checks otherwise pass safe probes. |
| Bounded public output | **not found** | Exact/one-over response tests and closed failure identifiers pass. |

## Layering and Residual Risk

| Check | Verdict |
|---|---|
| Single-owner security predicates | **REVISE**: snapshot authority ends before path-only external opens; helper breakaway predicate inverts one outcome. |
| Injected policy | **PASS**: policy/output root are read at composition and typed values are injected downward. |
| Fail-closed publication manifest | **not-verifiable**: release/publication P16-P18 remain open. |
| Resource lifetime | **REVISE** for breakaway proof; other helper/Job/handle/reader/workspace settlement paths passed. |
| Anti-layering | **CLEAN-SINGLE-OWNER for SR-C5-02**; **design decision required for SR-C5-01** because the external path-only seam has multiple security-sensitive owner/lifetime alternatives. |

Residual target risks remain unchanged: installed CST metadata/time/status and
hidden actions, exact acquisition/rollback, descendant/broker Job membership,
parent-crash cleanup, no visible windows, foreign-CST preservation, Line10,
release pinning, publication safety, registration, and deployment all remain
fail-closed future gates.

## Gate

**REVISE — immutable C5 `e5228a305fd0d196faa5d4042d04769e04c3f1bf` fails independent security review. SR-C5-01 permits a same-user workspace-name replacement to supply unauthorized bytes to the external vendor before capability-backed validation, and SR-C5-02 publishes successful breakaway creation as breakaway denial. The 547-test package, static checks, settlement checks, and unrelated controls are green, but they do not exercise either falsifier. No merge, target admission, release, publication, or deployment is authorized.**

## Terms and Abbreviations

- ACL / DACL: Access-control list / discretionary access-control list.
- CST: Computer Simulation Technology electromagnetic solver suite.
- Job: Windows Job Object used to contain helper and descendant processes.
- MCP: Model Context Protocol.
- PID: Process identifier.
- S4: Per-claim verdict discipline using verified, failed, or not-verifiable.
- SSRF: Server-Side Request Forgery.
- TOCTOU: Time of check to time of use.

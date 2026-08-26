# T04 Backend Implementation — Hub Enrollment State and Descriptor

Gate: **PASS** — strict RED/GREEN, the focused authentication/state/descriptor matrix, Ruff, format, diff check, and current CodeGraph evidence satisfy T04 without live service control.

Execution role: `$backend-engineer` under `$lead`. Scope: T04 only. Baseline: `5ff268dc13b2be9ca9500b5441634f0594538b94`.

## Receiving-side echo and invariant

Accepted design `AFABC3C...`, decision `18307E...`, architecture/security reviews `475606E...`, `A0F0D2...`, `238059A...`, and plan `8DD78E...` remain the immutable inputs. `HubEnrollmentServerV1` authenticates kernel-observed peer facts against one independently queried current supervisor CST row before parsing or comparing a digest. It owns separate one-use channel-nonce and capability ledgers. Every accepted or rejected exchange reaches `CONSUMED` or `CANCELLED`; no digest alone grants authority. The endpoint descriptor accepts numeric security identifiers only and preserves exact owner, protected access-control list order, High-integrity audit label, local-only/first-instance/message-mode flags, and exact readback equality.

| Owner | T04 surface |
|---|---|
| Enrollment server | New `cst_saved_field_hub_enrollment_windows.py`: injected supervisor-status query with an explicit five-second timeout and no retry; typed fail-closed mapping; peer/status identity matrix; bounded one-use nonce and digest ledgers; enroll/cancel/consume/expiry/exit/shutdown lifecycle. |
| Endpoint descriptor | Same module: exact first policy endpoint, runtime-numeric service/policy-owner security identifiers, ordered access-control entries, High-integrity no-write-up plus success/failure audit flags, and exact typed readback comparison. |
| Tests | New `test_cst_saved_field_t04_hub_enrollment.py`: Win32-safe synthetic identities, clock and cryptographic-random source; no live Service Control Manager, hub, CST, fleet, registration, or pipe mutation. |

Authorization: enrollment is not public. A request is authorized only after the injected status-only supervisor query returns exactly one current `cst` identity whose process identifier, kernel creation time, canonical image/package, parent, token user, session, and positive generation match the independently observed peer. The query has a five-second timeout, no retry, and maps unavailability/mismatch to typed enrollment failure before frame/digest work.

## RED/GREEN and MCP evidence

| Stage | Receipt |
|---|---|
| RED | `.venv\\Scripts\\python.exe -m pytest tests/test_cst_saved_field_t04_hub_enrollment.py -q` exited 1: 13 failures, all anchored on the absent exact T04 module. The prior system-Python run was discarded because it was outside the repo environment. |
| GREEN focused | `.venv\\Scripts\\python.exe -m pytest tests/test_cst_saved_field_t03_contracts.py tests/test_cst_saved_field_t04_hub_enrollment.py -q` exited 0: **18 passed**. The T04-only subset is 13 passed. |
| Static | Ruff check passed; both T04 files are already formatted; `git diff --check` passed. |
| T00 boundary | The topology scaffold remains intentionally RED on T05/T06 frontend/daemon/service owners plus existing broker descriptor duplication; this is unchanged and outside T04. |
| CodeGraph before edit | Three exact-path queries failed to resolve the missing Python target and returned irrelevant GUI material; those results were rejected. The earlier exact Go client result was retained only for the actual V1 wire fields: challenge, enroll, cancel, and terminal receipt. |
| CodeGraph after edit | Exact-path query resolved current `HubEnrollmentServerV1`, `build_enrollment_descriptor`, `issue_challenge`, `exchange`, `consume_frontend`, and their T04 callers. Blast radius is the new focused test; the similarly named broker `issue_challenge` is a separate untouched symbol. No stale or disabled-index banner appeared. |
|  | Final exact freshness query returned `_authenticate` at current lines 208-215, including the five-second query port and `except Exception`; no stale banner appeared. Its extra GUI match was irrelevant and not used. |

## AC state, wire contract and rollback

| AC | State |
|---|---|
| T04-AC01 | PASS: mismatch matrix rejects peer process identifier, creation time, image, package, parent, token user, session, task, and generation before frame/digest authority. |
| T04-AC02 | PASS: channel nonce and capability ledgers are distinct; enroll consumes the former and creates `ENROLLED`; replay/duplicate cannot admit; exact 32-byte capability plus frontend challenge consumes once. |
| T04-AC03 | PASS within the T04 state owner: ACK loss, post-ACK failure, fresh authenticated cancel, expiry, child exit, disconnect, service stop, shutdown, and restart leave terminal state with zero outstanding nonces/capabilities. This pure phase allocates no operating-system handle; pipe-handle ownership is composed by the planned T06 service layer. |
| T04-AC04 | PASS: descriptor is exact endpoint one of three, numeric-SID only, protected, first-instance, local-only, message-mode, High-integrity audited, and order-sensitive on readback. |
| T04-AC05 | PASS: source guard excludes bypass/detach/direct-frontend enrollment paths; peer/status authorization precedes digest comparison. |

Wire before T04: Go emitted V1 challenge/enroll/cancel/receipt frames, but no Python owner accepted them. Wire after T04: the Python owner accepts the same closed fields and maximum 4096-byte frame, returns `{version,correlation,state,channel_settled}`, and introduces no additional wire field or caller-selected identity. Malformed, duplicate-key, trailing/unknown-field, replay, stale, ambiguous, or identity-mismatched exchanges return a typed local failure and zero admission.

Rollback is one T04 group: remove the new enrollment module and its focused test. No earlier T00-T03 or unrelated dirty path belongs to this group.

## Terms and Abbreviations

- CST: Computer Simulation Technology.
- SCM: Windows Service Control Manager.
- SID: Windows security identifier.
- V1: version one of a closed protocol.

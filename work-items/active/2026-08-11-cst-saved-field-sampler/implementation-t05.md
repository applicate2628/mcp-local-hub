# T05 Backend Implementation — Thin CST Frontend and Daemon Protocol

Gate: **PASS** — strict RED/GREEN, affected six-tool/server/stdio checks, receipt/deadline/redaction tests, Ruff/format/diff checks, and fresh CodeGraph evidence satisfy the frontend-only boundary.

Execution role: `$backend-engineer` under `$lead`. Scope: T05 only. Baseline: `5ff268dc13b2be9ca9500b5441634f0594538b94`.

## Receiving-side echo and invariant

Accepted design `AFABC3C...`, decision `18307E...`, reviews `475606E...`, `A0F0D2...`, `238059A...`, plan `8DD78E...`, and accepted T04 artifact `1F1412...` are the immutable inputs. Existing six tool registrations and local call paths remain unchanged. Only the seventh tool reads one inherited capability, resolves the caller path to a non-authoritative policy `entry_id`, removes `project_bundle` before IPC, and uses `WindowsDaemonClient`. The frontend owns only challenge/correlation/capability proof, client-observed transport receipt, absolute-deadline publication, safe failure text and final `TextContent`; it imports no broker, worker, containment or vendor owner.

| Owner | T05 surface |
|---|---|
| Frontend protocol | `cst_saved_field_frontend_protocol.py`: canonical bounded frames, capability-bearing closed request, entry/budget-bearing result, closed safe failure identifiers, and locally split receipts. |
| Daemon client | New `cst_saved_field_daemon_client_windows.py`: exact 32-byte-plus-EOF inherited-handle intake, one-use challenge ledger, five-second no-retry transport calls, cancel-on-every-post-challenge error, receipt/deadline validation and capability zeroization. |
| CST frontend | `cst.py`: replaces direct broker composition/imports with daemon-only composition while preserving existing six tools and the public seventh-tool schema. |
| Tests | New T05 frontend matrix plus the T03 capability-field contract update required by the accepted request schema. |

Authorization: the frontend grants none. The external `project_bundle` selects one already loaded policy inventory entry; only its `entry_id` crosses IPC. Daemon authentication/entry resolution is T06-owned. All client calls use an explicit five-second timeout with no retry; failures map to closed safe identifiers, never raw exceptions.

## RED/GREEN and MCP evidence

| Stage | Receipt |
|---|---|
| RED | `.venv\\Scripts\\python.exe -m pytest tests/test_cst_saved_field_t05_frontend.py -q` exited 1: 10/10 failures on absent capability/result fields, daemon client/framing and daemon-only `cst.py` edge. |
| GREEN focused | T05 alone passed 11 tests. Combined T03/T04/T05, composition, legacy server/error inventory and real stdio handshake passed **47 tests**. One dependency warning about an unresolved FastMCP lifespan forward reference is unchanged and non-failing. |
| Static | Ruff passed; five affected files are formatted; `git diff --check` passed before the final format-only change. |
| CodeGraph before edit | Initial exact query resolved `strict_fastmcp` and old composition tests but missed `cst.py`/client and was rejected. Repeated exact queries resolved current direct-broker `cst.py` imports, `_register_saved_field_tool`, `_compose_saved_field_tool`, publisher and callers; the missing daemon-client path was confirmed. An irrelevant request-type response was rejected and only that uncovered detail used direct exact-source fallback. |
| CodeGraph after edit | Exact query resolved current `WindowsDaemonClient`, inherited intake, challenge ledger, daemon-only `cst.py` imports/composition and their call edges. A second exact query resolved capability-bearing request, budget-bearing result, safe failure set and frame functions. No stale/disabled-index banner appeared; unrelated GUI/broker matches were ignored. |

## AC state, wire change and rollback

| AC | State |
|---|---|
| T05-AC01 | PASS: legacy server catalogue/error corpus and real stdio handshake keep the three HFSS plus three CST names and local paths; affected combined surface is green. |
| T05-AC02 | PASS: seventh tool alone consumes inherited handle locator/capability, uses policy inventory only to select `entry_id`, removes `project_bundle` before IPC, and imports only daemon frontend owners. |
| T05-AC03 | PASS: per-client challenge/correlation/capability sequence is one-use; every post-challenge failure attempts bounded cancel, terminalizes locally and zeroes capability. Partial, short and overlong handle reads close fail-closed. |
| T05-AC04 | PASS: publication requires exact correlation/entry/request hash, structurally valid unchanged absolute budget, current frequency/deadline, and all four frontend-local receipt observations. |
| T05-AC05 | PASS: sampler-only validation policy remains fixed; frontend result accepts only a closed failure set; raw path/SID/canary failure text is rejected; existing tool error routes are unchanged. |

Wire before T05 lacked launch capability, resolved entry identity and budget, and `cst.py` called the broker directly. Wire after T05 request is exact `{schema,correlation_id,challenge_nonce,launch_capability,entry_id,request_sha256,request}` with no source locator/manifest/policy authority. Result adds exact `entry_id` and immutable budget and retains `{ok,text,failure_id}`. Consumer is the planned T06 daemon; publisher is only `cst.py` after complete `FrontendTransportReceiptV1`.

Rollback is one T05 group: remove the daemon client/T05 tests; revert frontend protocol/T03 contract changes and restore the prior `cst.py` composition hunks. No T00-T04 or unrelated dirty path belongs to this group.

## Terms and Abbreviations

- CST: Computer Simulation Technology.
- IPC: inter-process communication.
- MCP: Model Context Protocol.
- QPC: Windows QueryPerformanceCounter.

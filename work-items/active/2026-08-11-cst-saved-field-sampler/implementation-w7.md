# W7 Verification Rerun — Candidate Assembly Pending

Execution role: `$qa-engineer`

Plan: `484883EDBAD02333162C61FAF78B99AA56C402FD64D5955F0E6B65BDDEC82E14`

Candidate base: `43fee019d46c69522ebe79be952d5f139bd4854f`

Accepted correction: `5F0E0540B68C04229B04DF0C5A8E49F30E820C0E5801E2E1EBD153BC6799C361`

Accepted full-Go differential: `23D594C2E4F4FC1B8905DD2144EBAF8A08DB70631EA3D92A720C56B53A62ECB5`

Superseded W7 verification: `05C62691BC8210ECF5B35CB8EC2C56DC782BEB382D175F6FAA3F242C463AAB3A` (`REVISE`)

## Receiving-side echo

- Rerun the complete W7 verification after the accepted in-scope correction and differential.
- Accept only the exact nine established clean-HEAD Go failure signatures; any drift, new failure, timeout or survivor is `REVISE`.
- Do not edit source or tests, stage files, commit, push, install, register, or mutate Service Control Manager, CST, App Control, Hardware Security Module or virtual-disk state.
- W7 ownership remains split exactly as the plan states: QA verifies first; `$backend-engineer` then assembles the immutable candidate under Lead supervision.

## Fresh CodeGraph and process preflight

- CodeGraph MCP was queried before file search and again after verification. Relevant CST and Go files were current; pending banners named unrelated GUI files only.
- The canonical fixed-endpoint owner is `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_endpoints.py`; policy, enrollment, broker and daemon consume that owner.
- A narrow text search filled one explicit CodeGraph query gap for `WorkerCapabilityReceiptV1`: the type, strict wire parser, producer and consumer are live in the broker-worker protocol and worker transaction path. The full Python suite exercises the route.
- No `go.exe`, `*.test.exe` or native runtime process existed at preflight. Every QA command ran serially.
- HEAD was `43fee019d46c69522ebe79be952d5f139bd4854f`; the Git index was empty; `git diff --check` was clean.

## Fresh verification evidence

| Check | Result | Counts / wall time | Raw output |
|---|---|---|---|
| Full Go, `go test ./... -count=1 -json` | `PASS:differential` for the CST candidate | 11,903 test PASS, 9 exact accepted baseline FAIL, 114 SKIP; 41 package PASS, 3 baseline-fail, 11 package SKIP; 350.960 s | `/.scratch/w7-rerun-20260813/go-test-all.jsonl` |
| Full frozen Python | PASS | 635/635 PASS, 0 fail/skip/xfail; 10.015 s; one known Pydantic warning | `/.scratch/w7-rerun-20260813/python-full.txt` |
| Ruff check | PASS | 0 findings; 0.098 s | `/.scratch/w7-rerun-20260813/ruff-check.txt` |
| Ruff format check | PASS | 78 files already formatted; 0.113 s | `/.scratch/w7-rerun-20260813/ruff-format.txt` |
| Go vet | PASS | 0 findings; 2.594 s | `/.scratch/w7-rerun-20260813/go-vet.txt` |
| Native clean unsigned build 1 | PASS | SHA-256 `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3`; 1.463 s | `/.scratch/w7-rerun-20260813/native-build-1.txt` |
| Native clean unsigned build 2 | PASS | byte-identical SHA-256; 1.492 s | `/.scratch/w7-rerun-20260813/native-build-2.txt` |
| Native PE/manifest verifier | PASS | KERNEL32-only structure and manifest binding; 0.480 s | `/.scratch/w7-rerun-20260813/native-verify.txt` |
| Real Win32 five-handle native worker | PASS | 1/1 selected test; 0.660 s wrapper | `/.scratch/w7-rerun-20260813/native-five-handle.txt` |
| Real Go frontend/local-pipe E2E | PASS | 1/1; test 1.92 s, wrapper 2.597 s | `/.scratch/w7-rerun-20260813/frontend-go-e2e.txt` |
| Linux amd64 full-package compile-only matrix | PASS: compile only | all packages compiled; 4.020 s; no test-execution claim | `/.scratch/w7-rerun-20260813/go-linux-compile.txt` |
| Diff hygiene | PASS | 0 findings | `/.scratch/w7-rerun-20260813/git-diff-check.txt` |
| Manual staged publication-safety scan | PASS but non-authorizing | clean; 0 staged files examined | `/.scratch/w7-rerun-20260813/publication-safety.txt` |

The real frontend test crosses the existing Go `StdioHost`/`exec.Cmd` owner, a Go-generated enrollment capability, the scratch-built native frontend, and one unique fixed local pipe. The receiver observes 32 delivered bytes and matches their digest. This is the missing real-process evidence for W06-AC01; it is not a proxy or injected settlement result.

## Full-Go differential reconciliation

The full Go command terminated normally. Its nine failures match the accepted clean-HEAD differential exactly by package, test and semantic error; no new failure, signature drift, timeout or survivor occurred.

| Failure group | Fresh signature | Differential disposition |
|---|---|---|
| LSP and Serena refresh ordering | `capture=-1`, release anchor present | Exact clean-HEAD baseline, 2 tests |
| V5 unreadable intent | expected PE subsystem 2, actual 3 before intent read | Exact clean-HEAD baseline, 1 test |
| MCP-front rollback | malformed or unbound pin binding | Exact clean-HEAD baseline, 3 tests |
| Cross-volume staging | truncated DOS header, size 10 | Exact clean-HEAD baseline, 1 test |
| Self-replace operator text | test expects obsolete `go build` hint; production says `pwsh ./build.ps1` | Exact clean-HEAD baseline, 1 test |
| Dev-build operator text | test expects obsolete `build.sh`/console wording; production reports `build.ps1`/PE admission | Exact clean-HEAD baseline, 1 test |

These failures remain repository defects for their owning routing/CLI lanes; this result proves only that none is caused by the CST candidate. The rerun added one Go PASS relative to the superseded W7 result: the new real frontend end-to-end test.

## Guard and preservation reconciliation

| Verification surface | Result |
|---|---|
| W0-W6 named RED/GREEN guards | PASS. Accepted W0-W6 artifacts plus the correction now include the endpoint-owner, worker-receipt, formatting and real frontend/local-pipe GREEN falsifiers; full Python and focused real-process checks pass fresh. |
| Existing-six and default-off behavior | PASS through the full frozen Python suite, including the existing server/stdio compatibility and W0-W6 guard tests. |
| Endpoint and receipt topology | PASS. One canonical fixed-endpoint literal owner remains in production source; `WorkerCapabilityReceiptV1` has one live owner, strict parser, producer and consumer. No superseded fake-receipt or injected production route is accepted as settlement. |
| Non-Windows behavior | PASS for Go compile-only coverage. Python import behavior is covered by the full 635-test suite; no alternate-OS runtime claim is made. |
| Unrelated W0 preservation | PASS: exact rehash of the 44 `unrelated-preserve` inventory rows is 44/44 equal, 0 changed. |
| Git/index safety | PASS for QA preservation: index remained exactly empty and HEAD remained `43fee019d46c69522ebe79be952d5f139bd4854f`. QA did not stage or commit. |
| Publication safety | PASS only as a manual zero-staged-file QA check. It is non-authorizing and cannot replace candidate assembly, human review, the later fresh range scanner or explicit publication approval. |
| Process/resource cleanup | PASS. Post-run probes found no QA-owned Go test, native runtime or Python/uv survivor. One observed Python PID 33188 belonged to a foreign vcpkg-builds unittest outside this repository and was not touched. |

## Acceptance reconciliation

| Criterion | Verification result |
|---|---|
| W07-AC01 | `PASS:verification`. Python, Ruff, format, vet, native two-build/verifier, real Win32 integrations and Linux compile-only checks pass. Full Go has only the exact nine clean-HEAD baseline signatures accepted by the immutable differential. |
| W07-AC02 | `PASS:verification`. Every W0-W6 named guard has accepted RED/GREEN evidence; the four prior W7 gaps now have fresh GREEN evidence. No null or missing result was treated as clean. |
| W07-AC03 | `PASS` for unrelated-hash and diff-hygiene verification; **candidate assembly pending**. The index is intentionally empty because the plan assigns explicit-path staging to `$backend-engineer`, not QA. |
| W07-AC04 | `PASS:verification`. Current source has one endpoint owner and one worker-receipt route; full tests pass and no superseded fake settlement route is live. |
| W07-AC05 | **Pending candidate assembly**. No immutable candidate commit exists yet; therefore no W8 candidate SHA is claimed. QA correctly performed no manifest-pin, registration, install, push or deployment mutation. |

## Gate and handoff

Gate: `PASS:verification; candidate assembly pending`

The technical findings recorded by `work-items/bugs/2026-08-13-cst-w7-candidate-regression-gate-fails.md` are closed by fresh evidence. The bug record remains physically current/open because QA may not mark it fixed or archive it without user approval and lifecycle-owner action.

Next owner: `$backend-engineer` under Lead supervision must assemble the index from only the explicit admitted W0-W7 source, test, native-manifest and accepted-input artifact paths; exclude `servers/electromagnetics-mcp/.scratch/` and every `unrelated-preserve` path. After human review of the staged leak-check, create one immutable local candidate commit without push, registration, install or deployment. Then resume QA to bind this report to that exact commit SHA and re-probe index/HEAD drift before W8.

W8 remains blocked until that immutable candidate commit exists. No working-tree hash or empty-index state is represented as an immutable candidate.

## Terms and Abbreviations

- CST: Computer Simulation Technology electromagnetic solver.
- E2E: end-to-end.
- HSM: Hardware Security Module.
- MCP: Model Context Protocol.
- PE: Portable Executable.
- W7/W8: working-candidate verification and independent architecture-review phases.

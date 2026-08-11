# Exact integration and publication-safety gate

Recorded: 2026-08-11T14:39:42Z

## Gate

**PASS** — the local branch is safely fast-forwarded to the verified live `origin/master`; the final 105-path stage matches the approved allowlist exactly; staged diff hygiene, exact-patch publication safety, every-new-file publication safety, dependency/exclusion checks, and dirty-byte preservation are all green.

No product, test, fixture, dependency, wire, application programming interface (API), command-line interface (CLI), or configuration byte was authored or corrected in this lane. Under explicit correction authorization, six canonical bug records changed only by removing one surplus terminal blank line each.

## Receiving-side echo

- **Role:** backend engineer acting as integration owner for the console candidate.
- **Accepted inputs:** current `status.md`; superseding `qa-final-r2.md`, verified SHA-256 `846B1E39DB1FCF604E143CD008C0FB08A756555016548466860D173F7D1976EF`; `implementation-linux-final.md`, verified SHA-256 `9577F6EE65D6B85756D62A06D73C84C56797E2A332BDCB7D842594C7D6B2C0BF`.
- **Scope:** fetch and bind the live `origin/master`; prove whether the old-base console patch integrates safely; construct and scan only the exact candidate stage if safe.
- **Forbidden operations honored:** no source/test correction; no test rerun; no commit, push, publication, install, restart, deploy, Windows Sandbox, visible console, or live-fleet mutation; no pull-request worktree access.
- **Write boundary:** this report plus the six explicitly authorized end-of-file-only canonical bug corrections; no other authored change.

## Live remote base and drift proof

The current remote was fetched directly from `origin` before any candidate staging.

| Fact | Observed value |
| --- | --- |
| Old local base | `dcc41eb83784a2d38661ef6ab45668bcd7cad4e9` |
| Fetched `origin/master` | `599bbd92fb63961b465ea877705173447d99487a` |
| Distance | Three linear squash commits; merge base equals the old local base |
| PR #598 | `d08ab398955d9b23c2052498382dbe5bf7100996` |
| PR #600 | `a95d5c7493d93bf4b3831e1fa043caa0fe7099ec` |
| PR #599 | `599bbd92fb63961b465ea877705173447d99487a` |

Remote drift contains exactly eight paths:

| Merged lane | Paths |
| --- | --- |
| PR #598 | `servers/electromagnetics-mcp/README.md`; `servers/electromagnetics-mcp/src/mcphub_em_mcp/safety.py`; `servers/electromagnetics-mcp/tests/conftest.py`; `servers/electromagnetics-mcp/tests/test_safety.py` |
| PR #600 | `internal/vcpkgmcp/pinstatus/r23_bot_regressions_test.go`; `internal/vcpkgmcp/pinstatus/redact.go` |
| PR #599 | `internal/daemon/host.go`; `internal/daemon/host_session_contract_test.go` |

Mechanical comparison against the accepted 83-path console product inventory found **0 exact path overlaps**.

The only nearby namespace is `internal/daemon`. PR #599 validates and caches successful initialize responses in `host.go` and adds its session-contract test. The console candidate deletes the separate obsolete `child_env_console_marker_test.go`; it does not edit `host.go`, the session cache, or the new test. The concerns and symbols are independent.

## Safe fast-forward

Before fast-forward, the index was empty. All 133 dirty, deleted, and untracked working-tree paths were hashed or recorded as absent. `git merge --ff-only origin/master` then advanced the local branch without a rebase or cherry-pick.

Post-fast-forward evidence:

- `HEAD` equals `origin/master` at `599bbd92fb63961b465ea877705173447d99487a`;
- all 133 pre-existing working-tree states matched their pre-fast-forward SHA-256/absence state: **0 byte mismatches**;
- the index remained empty;
- the accepted product inventory still matched exactly: 63 tracked paths plus 20 untracked paths, 0 missing and 0 extra.

This proves a safe, linear integration path on the current remote base. No blind rebase or cherry-pick is required.

## Intended exact candidate inventory

The required candidate selection is mechanically defined as follows:

| Class | Count before this report | Final expected count | Selection rule |
| --- | ---: | ---: | --- |
| Console product, tracked | 63 | 63 | Exact accepted QA inventory relative to the current `HEAD` |
| Console product, new | 20 | 20 | Exact accepted QA inventory |
| Current console work-item | 12 | 13 | Every file under `work-items/active/2026-08-11-windows-console-opt-in-r2/`, including this report in the final stage |
| Related bug records | 8 | 8 | The exact files enumerated below |
| Accepted decision | 1 | 1 | `work-items/decisions/2026-08-10-windows-console-debug-opt-in.md` |
| **Total** | **104** | **105** | No other path |

Related bug records required by the current and reopened console evidence chain:

1. `work-items/bugs/2026-08-10-audit-lock-helper-broad-flake.md`
2. `work-items/bugs/2026-08-10-canonicalize-pe-fixtures-stale.md`
3. `work-items/bugs/2026-08-10-cli-reconcile-broad-timeout.md`
4. `work-items/bugs/2026-08-10-gui-broadcaster-workers-leak.md`
5. `work-items/bugs/2026-08-10-resolver-source-anchor-test-rot.md`
6. `work-items/bugs/2026-08-11-codegraph-daemon-restarted-during-scratch-ab.md`
7. `work-items/bugs/2026-08-11-native-linux-release-gate-red.md`
8. `work-items/bugs/2026-08-11-native-macos-release-runner-unavailable.md`

The archived predecessor is preserved in the working tree but is not part of the requested **current** work-item selection. Its historical Markdown contains deliberate two-space hard breaks and separate publication-history concerns; adding it would expand this integration beyond the assigned current candidate.

## Explicit exclusions

The trial index was mechanically checked to contain none of these paths:

| Exclusion | Reason |
| --- | --- |
| `work-items/active/2026-08-11-pr598-review-fix/**` | Explicitly excluded PR work-item; merged source is already in `HEAD` |
| `work-items/active/2026-08-11-pr600-review-fix/**` | Explicitly excluded PR work-item; merged source is already in `HEAD` |
| `work-items/README.md` | Generated mixed read model containing unrelated lifecycle changes |
| `internal/api/port_alloc_excluded_windows.go` | Unrelated user-owned dirty change |
| `work-items/archive/2026-08/2026-08-10-windows-console-opt-in/**` | Historical predecessor, not the current work-item selection |
| `/.scratch/**`, PR worktrees, raw logs | Local-only evidence and explicitly protected surfaces |
| The eight fetched remote-drift paths | Already committed in the new base, not candidate changes |

All excluded working-tree bytes remain present and unchanged.

## Final stage and publication evidence

The exact final allowlist, including this report, is staged for integration:

| Stage fact | Result |
| --- | --- |
| Paths | 105 total: 42 added, 60 modified, 3 deleted |
| Allowlist comparison | 0 missing, 0 extra |
| Forbidden staged paths | 0 |
| Dependency manifest or lock delta | 0 |
| Exact binary/full-index patch | Regenerated after final report bytes were staged |
| Exact-patch publication scanner | PASS, 0 findings |
| Every staged new file scanned separately | 42/42 PASS, 0 findings |

The required scanner therefore found no secret, credential, machine-local path, transcript marker, or private-key material in the exact staged delta or any staged new file.

One additional non-authoritative diagnostic used the scanner's default whole-staged-blob mode. It reported machine-path markers in unchanged portions of modified files because that mode scans complete staged blobs rather than the assigned exact delta. It is not substituted for the required exact-patch-plus-new-files oracle above.

## Authorized diff-hygiene correction

The first exact trial exposed one extra blank line at end of file in each of these six required new bug records. Lead explicitly authorized this bounded correction, and `apply_patch` removed exactly the second terminal line feed from each file:

| File | Reported line |
| --- | ---: |
| `work-items/bugs/2026-08-10-audit-lock-helper-broad-flake.md` | 16 |
| `work-items/bugs/2026-08-10-canonicalize-pe-fixtures-stale.md` | 16 |
| `work-items/bugs/2026-08-10-cli-reconcile-broad-timeout.md` | 15 |
| `work-items/bugs/2026-08-10-gui-broadcaster-workers-leak.md` | 14 |
| `work-items/bugs/2026-08-10-resolver-source-anchor-test-rot.md` | 14 |
| `work-items/bugs/2026-08-11-native-macos-release-runner-unavailable.md` | 15 |

No other byte or content in those records changed. The reconstructed 105-path final stage now passes `git diff --cached --check` with exit 0; no check was weakened or bypassed.

## Final index and side effects

The unsafe 104-path trial was first removed by exact path with 0 worktree byte mismatches. After authorization and correction, the final index was reconstructed from the same mechanical allowlist plus this report. Final state:

- Git index rows: **105**, exactly matching the allowlist with 0 missing and 0 extra;
- stage status: 42 added, 60 modified, 3 deleted;
- forbidden staged paths: **0**;
- dependency manifest or lock delta: **0**;
- worktree byte mismatches during fast-forward and unsafe-stage restoration: **0**;
- `HEAD == origin/master` remains true;
- no source/test file was authored, reformatted, reverted, or overwritten;
- no tests were rerun;
- no commit, push, publication, install, restart, deployment, Sandbox, live-fleet, scheduler, supervisor, listener, or visible-console action occurred.

The local exact-patch receipt remains only under `/.scratch/windows-console-contract/`.

## Next owner action

The final exact stage is ready for Lead's commit step. Commit, push, install, restart, and live no-visible-console verification remain deliberately unperformed and require their own gates and human publication authorization.

This integration lane is terminal **PASS**.

## Terms and Abbreviations

- **API** — Application Programming Interface.
- **CLI** — Command-Line Interface.
- **HEAD** — the currently checked-out Git commit.
- **PR** — Pull Request.
- **PASS / REVISE** — accepted gate / bounded correction or evidence still required.
- **SHA-256** — Secure Hash Algorithm 256-bit digest.

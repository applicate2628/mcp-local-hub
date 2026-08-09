# Plan snapshot — PR #588 live findings R3 correction

Date: 2026-07-27  
Role: `$planner`  
Gate: **PASS plan; implementation BLOCKED at R3-0**

Canonical plan:
`work-items/active/2026-07-25-mcp-front-daemon/plan-live-findings-2026-07-27.md`

Canonical SHA-256:
`65DE5A6EC80E2A1E61986137B59AB5B6A4C3E425D2C616DEA3AF13615DD674D1`

Accepted inputs:

- ADR:
  `6DC6DC4478F05044DBD5E58E08F7DF0E4D4A87AA4161BE20E02B46B93330028F`;
- design:
  `A42A6CDF0BDEEF1502640269E305D1F6D3F09E05F72789810A125FA6CD6C106B`;
- reliability PASS:
  `7237D8CFE8CE6A9C535BAC0EAA885E015EAD52F9941207874C8705EA6F890B34`.

## Active R3 snapshot

| Phase | Owner | Gate |
| --- | --- | --- |
| R3-0 — foreign-overlap admission | `$lead` plus foreign owner/user | BLOCKED until overlapping API projection/`TEMPDEBUG` changes are resolved or explicitly admitted; `.codegraph*` excluded |
| R3-A — multi-entry authorization | `$backend-engineer` | Wrapper same-lock forward/rollback dependency races invoke zero unsafe writes |
| R3-B — classifier and caller policies | `$backend-engineer` | Full F2/conflict/prior-receipt/forward-admission matrices plus independent rollback progress |
| R3-C — secure pins | `$backend-engineer` | Windows/POSIX root-relative no-follow, size, cleanup, malformed-pin, and retained-byte tests |
| R3-D — integration | `$backend-engineer`, integration owner | I1-I12, R3-1..11, v1/v2 command owners, C1-C10/protected guards |
| R3-E — independent QA | `$qa-engineer` | 11 controlled mutations fail, exact SHA restoration, scoped target-platform tests, tagged build/vet |
| R3-F — review/commit | two external `$architecture-reviewer` lanes, then `$lead` | Native completion-verified PASS ×2; publication scan; new local correction commit; no push |

Exact production surface:
`internal/clients/config_lock.go`,
`internal/api/lsp_client_router.go`,
`internal/api/lsp_client_router_snapshot.go`,
`internal/api/serena_client_reconcile.go`,
`internal/cli/install_reconcile_mcp_front.go`,
`internal/api/pin_windows.go`, and
`internal/api/pin_posix.go`.

Exact test surface:
`internal/clients/config_lock_wrapped_test.go`,
`internal/api/lsp_client_router_plan_test.go`,
`internal/api/lsp_client_router_snapshot_review_test.go`,
`internal/api/serena_client_reconcile_test.go`,
`internal/cli/install_reconcile_mcp_front_v3_test.go`,
`internal/cli/install_reconcile_mcp_front_pr588_r2_test.go`,
`internal/api/pin_windows_test.go`, and
`internal/api/pin_posix_test.go`.

Atomic revert group: `RG-PR588-V3-R3`.

Every API/CLI command uses `-tags=test_state_path_env` and a distinct fresh
`MCPHUB_STATE_DIR_OVERRIDE`. A whole CLI package test, `go test ./...`,
GUI/tray/supervisor/scheduler/daemon launch, image-name process kill, checkout,
reset, stash, worktree creation, history rewrite, and push are forbidden.

External review must use direct native `codex.exe` or a PowerShell process owner
that waits and records the native `ExitCode`. The failed `.cmd` parent oracle is
forbidden.

No provider-backed execution was used for this planner stage.

## Terms and Abbreviations

- CAS: compare-and-set.
- CLI: Command-Line Interface.
- LSP: Language Server Protocol.
- POSIX: portable operating-system interface for Unix-like targets.
- RG: atomic revert group.
- R3: third correction round.

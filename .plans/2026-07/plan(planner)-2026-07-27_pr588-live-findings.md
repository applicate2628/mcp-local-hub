# Plan snapshot — PR #588 live findings R2 correction

Date: 2026-07-27  
Role: `$planner`  
Gate: **PASS — RETURN(lead); planner-eligible**

Canonical plan:
`work-items/active/2026-07-25-mcp-front-daemon/plan-live-findings-2026-07-27.md`

Canonical SHA-256:
`A9A21F1DCC9E80D0D6CC5FAF67D68C64CC815350721883AB3DCD8C8A3DBD9DE8`

R1 plan SHA-256 retained as completed implementation/QA provenance:
`15B42A0E60CB2730D4C9CD77F6D49792049B9F66B500CB82D4BF05BA677ACB7B`

Decision input:
`work-items/decisions/2026-07-27-mcp-front-reconcile-v3-row-journal.md`
at
`42A3FBE24E4E87EA7EF1D5A2E59BEC895DF05C310C740222492E8A8AC3776B62`.

## Active R2 snapshot

| Phase | Owner | Gate |
| --- | --- | --- |
| R2-A — wrapper-owned conditional mutation | `$backend-engineer` | F1 and real-seam prepare-order guards green; every factory conditional; CAS allowlist unchanged |
| R2-B — sole row authority and causal receipts | `$backend-engineer` | F2/A-01 guards green; compatibility projections and stale helper families absent |
| R2-C — exact Serena inverse and durable LSP groups | `$backend-engineer` | F3/F4 two-call, baseline-only, C4, and C9 guards green |
| R2-D — integration and handoff | `$backend-engineer`, integration owner | Complete scoped sets green; protected owners unchanged; exact handoff hashes |
| R2-E — independent QA | `$qa-engineer` | Seven current-source defect mutations fail at named assertions; exact restoration; tagged build/vet PASS |
| R2-F — review and commit | two independent external `$architecture-reviewer` lanes, then `$lead` | Both completion-verified PASS; leak scan; one local commit; no push |

Production change surface is limited to
`internal/clients/config_lock.go`,
`internal/clients/cas_mutator.go`,
`internal/api/serena_client_reconcile.go`,
`internal/cli/install_reconcile_mcp_front.go`,
`internal/api/lsp_client_router.go`,
and `internal/api/lsp_client_router_snapshot.go`.

Exact focused test surface:
`internal/clients/config_lock_wrapped_test.go`,
`internal/clients/cas_mutator_test.go`,
`internal/api/serena_client_reconcile_test.go`,
`internal/api/lsp_client_router_plan_test.go`,
`internal/api/lsp_client_router_snapshot_review_test.go`,
`internal/cli/install_reconcile_mcp_front_v3_test.go`,
`internal/cli/install_reconcile_mcp_front_review_test.go`,
`internal/cli/install_reconcile_mcp_front_pr588_test.go`, and
`internal/cli/install_reconcile_mcp_front_pr588_r2_test.go`.

All R2 implementation/test phases are atomic revert group `RG-PR588-V3-R2`.
Every API/CLI-inclusive Go invocation uses a fresh `.scratch` state directory,
`MCPHUB_STATE_DIR_OVERRIDE`, and `-tags=test_state_path_env`. Unscoped
`go test ./...`, a whole CLI package test without the exact R2 regex,
GUI/tray/supervisor/scheduler/daemon launch, process killing by image name,
checkout, hard reset, stash, another worktree, and push are forbidden.

No provider-backed execution was used for this planner stage.

## Terms and Abbreviations

- **CAS**: compare-and-set.
- **CLI**: command-line interface.
- **LSP**: Language Server Protocol.
- **RG**: atomic revert group.

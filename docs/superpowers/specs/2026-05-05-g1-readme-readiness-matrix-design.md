# G1 — README Feature/Readiness Matrix Design

## Goal

Add a single-table readiness matrix to the project README that lets a first-time visitor scan the project's actual maturity in 10 seconds without overclaiming. Replaces the existing one-paragraph `## Platform support` section with a structured surface-vs-status grid that distinguishes "works manually" from "scheduler-managed lifetime works".

## Source

Tracked as G1 in `docs/superpowers/plans/phase-3b-ii-backlog.md` Section G (ravitemer adoption). Codex xhigh plan-review verdict ("Scenario D PASS") sequenced this immediately after G10 governance docs and before G2 unified health endpoint.

## Architecture

Pure documentation change. No code, no tests, no API, no schema. Single Markdown table in `README.md`.

## Layout

### Position in README

Replaces the existing `## Platform support` paragraph (the new matrix subsumes its content into two rows: "Run on Linux (manual)" and "Auto-start on logon — Linux/macOS").

Stays between `## Current status` (prose summary, kept) and `## License`. The matrix functions as the structured complement to the prose summary.

### Section heading

`## Feature & readiness matrix`

### Table structure

Three columns:

1. **Surface** — user-visible capability or platform target. Rows describe what a visitor would naturally ask "does this work?" about.
2. **Status** — one of three states (see below).
3. **Notes** — one-line qualifier; cites the relevant code path, commit, or backlog item where useful.

### Status states

Three states, fixed, with explicit promotion rules so a future maintainer can correctly classify a new row:

- **✅ Stable** — fresh automated test coverage OR a recent live-smoke pass for the exact user-visible surface, AND no open critical caveat (no open work-item bug, no missing manual-smoke matrix entry). "Stable" does not mean "production-ready"; it means "works as advertised in this preview, with evidence".
- **⚠ Preview** — feature is shipped and reachable, but live-smoke coverage is partial, an open caveat exists in `work-items/bugs/` or the backlog, OR the surface may change in incompatible ways. Backups and dry-run before applying.
- **🚧 Roadmap** — feature is acknowledged in the backlog but not yet shipped, OR currently exists only as a cross-compile path with no runtime evidence. The row exists so visitors stop guessing whether it's planned.

No fourth "❌ Not planned" state in the matrix. Items we explicitly decline (e.g., "Mandatory single `/mcp` endpoint", "Remote access as default") live in `SECURITY.md` Out-of-scope section, not the readiness matrix.

### Initial rows (18)

```text
| Surface                                       | Status     | Notes                                                                              |
|-----------------------------------------------|------------|------------------------------------------------------------------------------------|
| Run on Windows                                | ✅ Stable  | Tested on Windows 11 (10.0.26100); primary platform                                |
| Run on Linux                                  | 🚧 Roadmap | Ubuntu CI builds/tests; install/scheduler not implemented                          |
| Run on macOS                                  | 🚧 Roadmap | darwin cross-build only; scheduler + force-kill probe stubbed                      |
| Auto-start on logon — Windows                 | ✅ Stable  | Task Scheduler with restart-on-failure                                             |
| Auto-start on logon — Linux                   | 🚧 Roadmap | systemd user units (F2) + `mcphub setup --server` with `loginctl enable-linger` (F3) tracked in backlog |
| Auto-start on logon — macOS                   | 🚧 Roadmap | launchd auto-start is not currently tracked in the backlog F-tier; manual launch only |
| Default client install                        | ⚠ Preview  | Claude Code, Codex CLI, Cursor; Cursor live-smoke pending in verification matrix   |
| Opt-in client install                         | ⚠ Preview  | VS Code, Gemini-CLI, Qwen-CLI, Antigravity (stdio-relay); manual smoke pending     |
| GUI dashboard (`mcphub gui`)                  | ⚠ Preview  | Loopback-only; CSRF/DNS-rebind hardened (PR #51); manual GUI browser smoke pending |
| GUI logs viewer (`/api/logs/:server`)         | ⚠ Preview  | SSE tail follow + filter + ERROR/WARN highlight + Open folder all shipped          |
| Workspace-scoped LSP lazy proxies             | ⚠ Preview  | `mcphub register` + per-language proxy; D3 manual multi-language smoke pending     |
| Encrypted secrets vault                       | ⚠ Preview  | age-encrypted; argv-leak removed (PR #128); open cross-process last-write-wins limitation tracked in `work-items/bugs/a3a-vault-concurrent-edit-lww.md` |
| Local manifest authoring (GUI Add server / `mcphub manifest create`) | ⚠ Preview | Form + `Paste YAML` import; YAML smuggling hardened (PR #51) but still surface-may-change before 1.0 |
| Backups, rollback, migration                  | ⚠ Preview  | `backups.keep_n` enforced + per-write timestamped; tracked race in interleaved migrate/demigrate (`work-items/bugs/b1-backup-file-race.md`) |
| Per-server HTTP API (`/mcp` per daemon)       | ⚠ Preview  | DNS-rebind + Content-Type + body-size guards; GET/SSE server-notification semantics still being reconciled |
| Unified health/status snapshot                | 🚧 Roadmap | G2, immediately ahead of preview tag — combines ping/status/version + probes      |
| Capability browser (tools/resources/prompts)  | 🚧 Roadmap | G3, post preview-tag                                                               |
| Marketplace / remote manifests                | 🚧 Roadmap | G5/G6/G7 — Phase 3C/3D                                                             |
```

18 rows in the actual table (2 Stable, 9 Preview, 7 Roadmap). The honest distribution skews toward Preview because the project is mid-stabilization after the 2026-05-04 audit wave; rows promote to Stable as live-smoke evidence lands and open caveats close.

## Out of scope

- Per-server status (the `## Ten shipped servers` table at the top of README already lists them by name).
- Per-CLI-command status (the `## CLI surface` section already enumerates them).
- Per-screen GUI status (the `## Supported clients` and architecture sections already describe per-screen state).
- Threat model / "do not copy" decisions (those live in `SECURITY.md`, not the readiness matrix).

## Maintenance contract

- New row added when a new user-visible capability is introduced. Rows are not added for internal refactors.
- Status flip from 🚧 → ⚠ → ✅ happens **as a commit on the same PR** that earns the promotion (e.g., the PR that adds live-smoke evidence is the PR that flips the status). PR descriptions are not commit content and cannot enforce this; instead the project's PR review checklist (in `CONTRIBUTING.md`) names the matrix as one of the items reviewers verify.
- Removing a row requires an explicit "deprecated in vX" CHANGELOG entry.
- A future-iteration `make docs-check` target (or equivalent CI step) MAY be added to lint the matrix shape (column count, status emoji set), but is not required for this initial G1 rollout. Until then the gate is human review per the `CONTRIBUTING.md` checklist row.

## Testing

No automated test. Manual review acceptance: a developer unfamiliar with the project can read the matrix and correctly answer "what platforms does this run on?", "is the GUI safe to expose?", "does it have a marketplace?" without further reading.

## Implementation effort

~½ day. Single-file edit to `README.md`. No code review required beyond the standard PR review.

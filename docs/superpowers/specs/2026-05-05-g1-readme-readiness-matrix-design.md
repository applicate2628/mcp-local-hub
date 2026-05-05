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

Three states, fixed:

- **✅ Stable** — feature exists, tested, expected to keep working between preview tags. Not "production-ready" — just "works as advertised in this preview".
- **⚠ Preview** — feature exists and is reachable, but live-smoke coverage is partial or the surface may change in incompatible ways. Backups and dry-run before applying.
- **🚧 Roadmap** — feature is acknowledged in the backlog but not yet shipped. The row exists so visitors can stop guessing whether it's planned.

No fourth "❌ Not planned" state in the matrix. Items we explicitly decline (e.g., "Mandatory single `/mcp` endpoint", "Remote access as default") live in `SECURITY.md` Out-of-scope section, not the readiness matrix.

### Initial rows (12)

```
| Surface                                       | Status     | Notes                                                                  |
|-----------------------------------------------|------------|------------------------------------------------------------------------|
| Run on Windows                                | ✅ Stable  | Tested on Windows 11 (10.0.26100); primary platform                    |
| Run on Linux (manual launch)                  | ⚠ Preview  | Cross-compiles and runs; no live smoke matrix yet                      |
| Run on macOS (manual launch)                  | ⚠ Preview  | Cross-compiles and runs; `--force --kill` identity probe is Linux/Win  |
| Auto-start on logon — Windows                 | ✅ Stable  | Task Scheduler with restart-on-failure                                 |
| Auto-start on logon — Linux/macOS             | 🚧 Roadmap | systemd user units (F2) and launchd (F3) tracked in backlog            |
| Default client install                        | ✅ Stable  | Claude Code, Codex CLI, Cursor (`mcphub install --server X`)           |
| Opt-in client install                         | ⚠ Preview  | VS Code, Gemini-CLI, Qwen-CLI, Antigravity (`--clients ...`)           |
| GUI dashboard (`mcphub gui`)                  | ✅ Stable  | Loopback-only; CSRF + DNS-rebind hardened (PR #51)                     |
| Encrypted secrets vault                       | ✅ Stable  | age-encrypted; argv-leak removed (`secrets set --value` deleted)       |
| Backups, rollback, migration                  | ✅ Stable  | Per-write timestamped + `backups.keep_n` enforced + `mcphub migrate`   |
| Per-server HTTP API                           | ⚠ Preview  | `/mcp` per daemon; unified `/api/health` is G2 (next)                  |
| Capability browser (tools/resources/prompts)  | 🚧 Roadmap | G3, post preview-tag                                                   |
| Marketplace / remote manifests                | 🚧 Roadmap | G5/G6/G7 — Phase 3C/3D                                                 |
```

13 rows in the actual table (one over the proposed 12 — the marketplace/remote-manifest row was added to close the obvious "is this thing connected to a registry?" question).

## Out of scope

- Per-server status (the `## Ten shipped servers` table at the top of README already lists them by name).
- Per-CLI-command status (the `## CLI surface` section already enumerates them).
- Per-screen GUI status (the `## Supported clients` and architecture sections already describe per-screen state).
- Threat model / "do not copy" decisions (those live in `SECURITY.md`, not the readiness matrix).

## Maintenance contract

- New row added when a new user-visible capability is introduced. Rows are not added for internal refactors.
- Status flip from 🚧 → ⚠ → ✅ happens at PR-merge time; the PR description must update the matrix row in the same commit.
- Removing a row requires an explicit "deprecated in vX" comment plus a CHANGELOG entry.

## Testing

No automated test. Manual review acceptance: a developer unfamiliar with the project can read the matrix and correctly answer "what platforms does this run on?", "is the GUI safe to expose?", "does it have a marketplace?" without further reading.

## Implementation effort

~½ day. Single-file edit to `README.md`. No code review required beyond the standard PR review.

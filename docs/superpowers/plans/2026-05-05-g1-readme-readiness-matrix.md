# G1 README Feature/Readiness Matrix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the existing one-paragraph `## Platform support` section in `README.md` with a structured `## Feature & readiness matrix` section containing an 18-row × 3-column table that lets a first-time visitor scan project maturity in 10 seconds without overclaiming.

**Architecture:** Pure documentation change. One file modified (`README.md`). No code, no tests, no API, no schema. The matrix uses three fixed status states (`✅ Stable` / `⚠ Preview` / `🚧 Roadmap`) with explicit promotion rules defined in the spec.

**Tech Stack:** Markdown only. The repo uses GitHub-flavored Markdown for `README.md`; no Markdown linter is enforced in CI for this file.

**Spec:** [`docs/superpowers/specs/2026-05-05-g1-readme-readiness-matrix-design.md`](../specs/2026-05-05-g1-readme-readiness-matrix-design.md) at commit `2e161a5`.

**Branch:** `feat/g10-g1-g2-preview-prep` (already exists; G10 community docs already committed on it).

---

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `README.md` | Modify lines `262-264` | Replace `## Platform support` paragraph with `## Feature & readiness matrix` section (heading + intro line + 16-row table) |

No new files. No test files. No CI changes.

---

## Task 1: Replace `## Platform support` with `## Feature & readiness matrix` in README.md

**Files:**
- Modify: `README.md:262-264` (the entire `## Platform support` section: heading, blank line, paragraph)

- [ ] **Step 1: Confirm the exact lines to replace**

Run:
```bash
sed -n '260,266p' README.md
```

Expected output (3 lines of section + surrounding context):
```
[line 260 blank or end of Current status]
[line 261 blank]
## Platform support

**Windows 11** is first-class (tested on 10.0.26100). Linux and macOS cross-compile but `mcphub install` fails with "not yet implemented" — the scheduler backend for those platforms is Phase 4 scope. The embedded stdio-bridge / godbolt / perftools servers themselves run fine on Linux and macOS; you just can't yet wire them up as persistent daemons through the OS scheduler.

## License
```

If the section is not at lines 262-264 anymore (someone else moved it), update the Edit `old_string` accordingly before applying.

- [ ] **Step 2: Apply the Edit replacing the section**

Use the Edit tool with `file_path: README.md`.

`old_string` (4 lines, including the heading and the paragraph):

```
## Platform support

**Windows 11** is first-class (tested on 10.0.26100). Linux and macOS cross-compile but `mcphub install` fails with "not yet implemented" — the scheduler backend for those platforms is Phase 4 scope. The embedded stdio-bridge / godbolt / perftools servers themselves run fine on Linux and macOS; you just can't yet wire them up as persistent daemons through the OS scheduler.
```

`new_string` (the new section — heading + intro line + 3 status-state legend lines + table; 18 data rows + 1 header row + 1 separator row inside the table):

```
## Feature & readiness matrix

A surface-by-surface map of what this project actually does today, with explicit honesty about preview-state coverage. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the promotion rules a row follows.

- **✅ Stable** — fresh automated test coverage OR a recent live-smoke pass for the exact user-visible surface, AND no open critical caveat. Not "production-ready"; "works as advertised in this preview, with evidence."
- **⚠ Preview** — feature is shipped and reachable, but live-smoke coverage is partial, an open caveat exists in `work-items/bugs/` or the backlog, OR the surface may change in incompatible ways. Backups and dry-run before applying.
- **🚧 Roadmap** — feature is acknowledged in the backlog but not yet shipped, OR currently exists only as a cross-compile path with no runtime evidence.

| Surface | Status | Notes |
|---|---|---|
| Run on Windows | ✅ Stable | Tested on Windows 11 (10.0.26100); primary platform |
| Run on Linux | 🚧 Roadmap | Ubuntu CI builds/tests; install/scheduler not implemented |
| Run on macOS | 🚧 Roadmap | darwin cross-build only; scheduler + force-kill probe stubbed |
| Auto-start on logon — Windows | ✅ Stable | Task Scheduler with restart-on-failure |
| Auto-start on logon — Linux | 🚧 Roadmap | systemd user units (F2) + `mcphub setup --server` with `loginctl enable-linger` (F3) tracked in backlog |
| Auto-start on logon — macOS | 🚧 Roadmap | launchd auto-start is not currently tracked in the backlog F-tier; manual launch only |
| Default client install | ⚠ Preview | Claude Code, Codex CLI, Cursor; Cursor live-smoke pending in verification matrix |
| Opt-in client install | ⚠ Preview | VS Code, Gemini-CLI, Qwen-CLI, Antigravity (stdio-relay); manual smoke pending |
| GUI dashboard (`mcphub gui`) | ⚠ Preview | Loopback-only; CSRF/DNS-rebind hardened (PR #51); manual GUI browser smoke pending |
| GUI logs viewer (`/api/logs/:server`) | ⚠ Preview | SSE tail follow + filter + ERROR/WARN highlight + Open folder all shipped |
| Workspace-scoped LSP lazy proxies | ⚠ Preview | `mcphub register` + per-language proxy; D3 manual multi-language smoke pending |
| Encrypted secrets vault | ⚠ Preview | age-encrypted; argv-leak removed (PR #128); open cross-process last-write-wins limitation tracked in `work-items/bugs/a3a-vault-concurrent-edit-lww.md` |
| Local manifest authoring (GUI Add server / `mcphub manifest create`) | ⚠ Preview | Form + `Paste YAML` import; YAML smuggling hardened (PR #51) but still surface-may-change before 1.0 |
| Backups, rollback, migration | ⚠ Preview | `backups.keep_n` enforced + per-write timestamped; tracked race in interleaved migrate/demigrate (`work-items/bugs/b1-backup-file-race.md`) |
| Per-server HTTP API (`/mcp` per daemon) | ⚠ Preview | DNS-rebind + Content-Type + body-size guards; GET/SSE server-notification semantics still being reconciled |
| Unified health/status snapshot | 🚧 Roadmap | G2, immediately ahead of preview tag — combines ping/status/version + probes |
| Capability browser (tools/resources/prompts) | 🚧 Roadmap | G3, post preview-tag |
| Marketplace / remote manifests | 🚧 Roadmap | G5/G6/G7 — Phase 3C/3D |
```

Note: the table-render-friendly compact format `|---|---|---|` is used for the column-separator row. Each cell is single-line (no embedded newlines) so GitHub renders it as a clean grid.

- [ ] **Step 3: Verify the table has 18 data rows + 1 header + 1 separator**

Run the authoritative isolated-table row-count assertion:
```bash
awk '/^## Feature & readiness matrix/,/^## License/' README.md | grep -c '^| '
```

Expected output: `19` (1 header + 18 data rows; the separator `|---|...|` does not match `^| ` because the second char after `|` is `-`, not space).

If the count is off, the new table is malformed — re-check the Edit.

- [ ] **Step 4: Verify each status emoji renders correctly**

Run the per-state distribution check:
```bash
awk '/^## Feature & readiness matrix/,/^## License/' README.md | grep -c '✅ Stable'
awk '/^## Feature & readiness matrix/,/^## License/' README.md | grep -c '⚠ Preview'
awk '/^## Feature & readiness matrix/,/^## License/' README.md | grep -c '🚧 Roadmap'
```

Expected outputs:
- `3` Stable (1 legend + 2 data rows: Run on Windows, Auto-start on logon — Windows)
- `10` Preview (1 legend + 9 data rows: Default client install, Opt-in client install, GUI dashboard, GUI logs viewer, Workspace LSP lazy proxies, Encrypted secrets vault, Local manifest authoring, Backups/rollback/migration, Per-server HTTP API)
- `8` Roadmap (1 legend + 7 data rows: Run on Linux, Run on macOS, Auto-start Linux, Auto-start macOS, Unified health, Capability browser, Marketplace)

If any of those numbers is off, the status distribution drifted from the planned 2 / 9 / 7 split — re-inspect the table content.

- [ ] **Step 5: Confirm no other section was accidentally damaged**

Run:
```bash
grep -c '^## ' README.md
```

Expected output: `12`. The original README had 12 `##` sections (`## The problem`, `## What this tool does`, `## Quick start`, `## Ten shipped servers`, `## Supported clients`, `## CLI surface`, `## Architecture highlights`, `## Current status`, `## Platform support`, `## License`, `## Terms and Abbreviations`). Replacing one section with one section keeps the count at `12` (renamed: `Platform support` → `Feature & readiness matrix`). Wait — that's 11 originally + the title `# mcp-local-hub`. Let me recount.

Recount: `^## ` (level-2 only, not level-1):
1. The problem
2. What this tool does
3. Quick start
4. Ten shipped servers
5. Supported clients
6. CLI surface
7. Architecture highlights
8. Current status
9. Platform support → renamed to Feature & readiness matrix
10. License
11. Terms and Abbreviations

So `grep -c '^## '` should return `11` both before and after the edit (rename is in-place).

If your pre-edit count was `11` and post-edit count is also `11`, the structural integrity of the README is preserved. If it changed, something was duplicated or deleted; re-inspect.

- [ ] **Step 6: Verify the markdown table renders by spot-checking with `cmark` or visual inspection**

If `cmark` is available locally:
```bash
which cmark && cmark README.md > /tmp/readme.html && grep -A 2 'Feature &' /tmp/readme.html | head -20
```

Expected: HTML `<table>` element with `<thead>` containing 3 `<th>` cells (`Surface`, `Status`, `Notes`).

If `cmark` is not available, skip this step and rely on GitHub's renderer once the PR is opened.

- [ ] **Step 7: Visual diff review**

Run:
```bash
git diff README.md | head -50
```

Expected: a clean diff showing the 4-line `## Platform support` block removed and the new 26-line block inserted (1 heading + 1 blank + 1 intro + 3 legend bullets + 1 blank + 1 header + 1 separator + 18 data rows + 1 blank or end-of-section).

If the diff shows changes elsewhere in the file, the Edit affected something it shouldn't have — abort with `git checkout README.md` and retry.

- [ ] **Step 8: Stage and commit**

Run:
```bash
git add README.md
git commit -m "docs(readme): replace Platform support paragraph with Feature & readiness matrix" -m "Per G1 spec docs/superpowers/specs/2026-05-05-g1-readme-readiness-matrix-design.md (commit 2e161a5)." -m "Single-table 16-row map of user-visible surfaces against fixed status states (Stable / Preview / Roadmap). Replaces the prior one-paragraph platform claim with a structured grid that distinguishes 'works manually' from 'scheduler-managed lifetime works' and acknowledges open caveats." -m "Status distribution: 3 Stable, 8 Preview, 7 Roadmap. The Preview-heavy distribution reflects honest mid-stabilization state after the 2026-05-04 audit wave; rows promote to Stable as live-smoke evidence lands per the CONTRIBUTING.md review checklist." -m "Closes G1 in docs/superpowers/plans/phase-3b-ii-backlog.md Section G."
```

Expected output: `[feat/g10-g1-g2-preview-prep <SHA>] docs(readme): replace Platform support paragraph with Feature & readiness matrix` followed by `1 file changed, 23 insertions(+), 1 deletion(-)` (or similar — exact line count depends on whether the original paragraph wrapped in your editor).

---

## Self-Review checklist (run mentally before handing off to subagent-driven-development)

**1. Spec coverage:** Walk through each spec section:

- [x] `## Goal` — Task 1 produces the matrix as described.
- [x] `### Position in README` — Task 1 Step 2 replaces the exact `## Platform support` section.
- [x] `### Section heading` — Task 1 Step 2 uses `## Feature & readiness matrix`.
- [x] `### Table structure` — Task 1 Step 2 has Surface / Status / Notes as the 3 columns.
- [x] `### Status states` — Task 1 Step 2 includes the 3-bullet legend before the table.
- [x] `### Initial rows (18)` — Task 1 Step 2 has all 18 data rows verbatim from the spec.
- [x] `## Out of scope` — n/a, no implementation needed.
- [x] `## Maintenance contract` — already addressed in CONTRIBUTING.md (commit `2e161a5` on this branch).
- [x] `## Testing` — Task 1 Steps 3-5 cover the manual review acceptance.
- [x] `## Implementation effort` — half-day, single task; matches the spec estimate.

No spec section is uncovered.

**2. Placeholder scan:** No "TBD", no "TODO", no "implement later", no "add appropriate error handling". Each step has the exact command, the exact expected output, or the exact `old_string`/`new_string`.

**3. Type consistency:** No types or function names are involved (docs-only). The status emojis are used consistently across the legend and the data rows (`✅ Stable`, `⚠ Preview`, `🚧 Roadmap`).

**4. Idempotence:** Re-running Task 1 on a clean working tree is safe — the Edit's `old_string` precisely identifies the original section, so the second invocation would fail (string not found), giving the engineer a clear error rather than silently double-applying.

---

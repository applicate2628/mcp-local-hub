# Recovery index audit

## Scope and outcome

Reconciled only the `work-items/index.md` Active table against the directory
basenames under `work-items/active/`. Four missing rows were added at
`work-items/index.md:14-18`; the existing matched row was left unchanged. No
status, brief, roadmap, board, archive, or bug-truth semantics were rewritten.

## Defect-class inventory

| Active folder | Before | Grounding | Action |
|---|---|---|---|
| `2026-07-14-cbuild-mcp` | missing | The first State section says `DESIGN ACCEPTED ... IMPLEMENTING` (`work-items/active/2026-07-14-cbuild-mcp/status.md:6-7`). | Added an Active row. |
|  |  | A later section says `DEFERRED` and PR #541 `PARKED` (`work-items/active/2026-07-14-cbuild-mcp/status.md:38-55`). | Preserved the confirmed ambiguity in the row; no lifecycle decision was invented. |
| `2026-07-16-productization-gui-solidify` | matched | Its Active row already existed and still targets its live status file (`work-items/index.md:15`). | Unchanged. |
| `2026-07-20-cli-first-run-ux` | missing | The first stage log records the revision delivered and awaiting the lead gate (`work-items/active/2026-07-20-cli-first-run-ux/status.md:40-46`). | Added an Active row. |
| `2026-07-20-supervisor-never-crash-reliability` | missing | The status header records investigation complete and design lanes dispatched (`work-items/active/2026-07-20-supervisor-never-crash-reliability/status.md:3-6`). | Added an Active row. |
| `2026-07-25-mcp-front-daemon` | missing | The current-state section marks the re-review active at recovery-state repair with four open obligations (`work-items/active/2026-07-25-mcp-front-daemon/status.md:8-18`). | Added an Active row. |

There were no stale Active rows after reconciliation. The five resulting rows
are visible together at `work-items/index.md:14-18`.

## Exact-set regression guard

A current-session PowerShell guard enumerated directory basenames with
`Get-ChildItem work-items\active -Directory`, parsed only
`active/<basename>/status.md` targets from the Active section, sorted both sets,
and computed both set differences.

Observed active-directory set:

```text
2026-07-14-cbuild-mcp
2026-07-16-productization-gui-solidify
2026-07-20-cli-first-run-ux
2026-07-20-supervisor-never-crash-reliability
2026-07-25-mcp-front-daemon
```

Observed Active-index target set:

```text
2026-07-14-cbuild-mcp
2026-07-16-productization-gui-solidify
2026-07-20-cli-first-run-ux
2026-07-20-supervisor-never-crash-reliability
2026-07-25-mcp-front-daemon
```

Guard result:

```text
missing=[]
stale=[]
exact_set_equal=true
active_links_use_active_prefix=true
```

A second current-session guard checked all five active folders for `closure.md`
and for terminal `status` / `state` / `stage` / `outcome` lines beginning with
`closed`, `done`, `complete`, `completed`, or `archived`; it found no
violations.

No rename, move, merge, or consolidation occurred, so an old-name/path sweep is
not applicable.

## Gate

**PASS** — the active-folder and Active-index target sets are exactly equal, no
Active row points at an archive path, and no active folder is terminal-marked.
The cbuild status prose remains internally ambiguous, but the ambiguity is now
visible and does not leave the active recovery index incomplete.

## Terms and Abbreviations

- **Active index**: the `## Active (work-items/active/)` table in
  `work-items/index.md`.
- **Exact-set guard**: a comparison requiring no missing and no stale member in
  either set.

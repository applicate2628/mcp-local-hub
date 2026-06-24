---
status: triage
---

# OpenPath is a second SeparateProcess=1 explorer-window generator (adjacent to the flood reaper)

Surfaced by the $architect during the explorer-flood drop-A/keep-C review (2026-06-23). NOT in the flood-reaper
scope; filed separately per scope discipline.

## Finding
`internal/gui/openpath.go:25` spawns `explorer.exe <path>` (no `/select`) via `spawnProcess`
(`internal/gui/browser.go:15-19` — fire-and-forget, no `Release()`). Reachable from:
- tray **Open-data-folder / Open-logs-folder** (`internal/cli/gui.go:838,852`),
- Settings/Logs **"Open app-data folder" POST** (`internal/gui/settings.go:225`, `internal/gui/logs.go:181`).

Under HKCU `SeparateProcess=1` these ALSO spawn persistent handed-off `explorer.exe` windows the hub cannot reap
(same handoff mechanism proven for `/select`: the launched process exits ~2s, a different handed-off PID owns the
window). So OpenPath is a second, untouched flood generator.

## Why not folded into the reaper PR
- Different trigger profile: OpenPath fires on **deliberate, low-frequency user clicks** (one window per explicit
  "Open folder" action) vs the `--force` recovery loop's high-frequency auto-reveal. One window per explicit click
  is arguably acceptable; the unbounded `--force`-loop accumulation is not.
- The same print-only/opt-in mitigation does NOT obviously apply (the user explicitly asked to open the folder).
- No reliable reap exists (architect verdict: handed-off windows are indistinguishable from the user's own).

## Decision needed (tracked)
Is one persistent window per explicit Open-folder click acceptable (do nothing — document), or should the tray/
settings affordances also go print-path-only / opt-in? Route to a decision; do not silently expand the reaper PR.

## Related
- Flood reaper: `work-items/bugs/2026-06-22-explorer-folder-window-orphan-flood.md` (drop-A/keep-C).
- The flood bug-doc's own root-cause snippet (it claims tray/settings flow through `OpenFolderAt`) is STALE —
  those flow through `OpenPath`. Update that doc on close.

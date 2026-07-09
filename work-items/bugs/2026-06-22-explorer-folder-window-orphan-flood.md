---
severity: high
context: orphan-reaping (the hub's core mission — stale processes from agentic+MCP usage)
---

- **status:** fixed
- **fixed-by:** PR #423 (`260eaa47`) - bare `mcphub gui --force` is print-only; reveal is opt-in.
- **HEAD reconciliation (2026-07-09):** Verified against master `63b6a008`; see `TRIAGE-2026-07-09.md` for code/test evidence.

# Hub spawns unbounded zombie `explorer.exe` folder windows it never reaps

## Resolution (2026-06-23 — print-only `--force`, drop the reaper)

Fixed by bounding the **generator at the source**, not by reaping after the fact.
**Measurement (decisive, this `SeparateProcess=1` host):** `explorer.exe /select,<path>`
HANDS OFF — the launched process EXITS in ~2s and the persistent window is a DIFFERENT,
handed-off `explorer.exe` PID the hub never observes. So a track-and-close reaper that
records the spawned (launcher) PID is fundamentally ineffective (and was dead code — zero
production callers). `$architect` PASS: no reliable reap exists for a handed-off window
that would not also risk closing the user's own folder windows.

**Fix:** bare `mcphub gui --force` is now PRINT-ONLY; the lock-folder open is behind an
opt-in `--force --reveal`. The high-frequency `--force` recovery loop spawns ZERO reveals
→ no handed-off-zombie accumulation. A one-time `SeparateProcess=1` warn is kept. RESIDUAL
(documented, accepted): one explicit `--force --reveal` leaves a single handed-off window
the hub cannot reap under `SeparateProcess=1`. The `OpenPath` tray/Settings generator is a
SEPARATE class — `2026-06-23-openpath-second-explorer-generator.md`. Close on PR merge.

## Symptom (observed 2026-06-22, live host)

System + Windows Explorer hung. Diagnosis found **32 `explorer.exe` processes**
(normal: 1–2), ~6.35 GB RAM and ~64K handles across them. All re-parented to
`svchost`, empty `MainWindowTitle` (zombie folder-window processes), start times
spanning 20–22.06 (accumulated over ~3 days of GUI restarts / `--force` recoveries).
Immediate mitigation: killed the 31 non-shell `explorer.exe` (kept the shell via
`GetShellWindow`), freed ~6.1 GB; system recovered.

## Root cause (confirmed in source)

`internal/gui/openfolder_windows.go`:
```go
func openFolderImpl(path string) error {
    _ = openFolderSpawn("explorer.exe", "/select,"+path) // fire-and-forget
    return nil
}
```
**Correction (codegraph-verified 2026-06-23):** the `/select` reveal flood path
(`OpenFolderAt` → `openFolderImpl`) has exactly ONE production caller — the
`mcphub gui --force` lock-folder reveal (`runForceDiagnostic`). The tray
**Open data folder** / **Open logs folder** items and the GUI Settings open-folder
POST flow through the SEPARATE `OpenPath` seam (`explorer.exe <path>`, no `/select`) —
a distinct generator tracked in `2026-06-23-openpath-second-explorer-generator.md`,
not this `/select` path. The original attribution above conflated the two.

The `--force` reveal did `explorer.exe /select,<path>` **fire-and-forget** with NO
tracking, dedup, or close.

The Windows HKCU setting `...\Explorer\Advanced\SeparateProcess = 1` ("Launch folder
windows in a separate process") is ON on this host, so every `/select` spawns a
**separate, persistent** `explorer.exe` process (instead of delegating to the one
shell instance). The hub generates one per open and reaps none → unbounded.

## Why this is in-scope for the hub (not "user's Windows setting")

The hub's raison d'être is taming the process/orphan explosion from codex/claude +
MCP usage (see `project_mcphub_process_tails_motivation`). Here the hub is itself an
**orphan generator** for a class it does not reap. The supervisor reconcile owns
daemon lifecycle but has no awareness of the file-manager windows the GUI/tray spawn.

## Orphan inventory found this session (the broader class)

1. **Zombie `explorer.exe` folder windows** — 31 reaped. THIS bug. Entirely unhandled.
2. **Stale `workspace-proxy` daemons** for removed worktrees (wtdw:9203, wtprune:9202,
   wtp3:9211) — manually `mcphub workspace unregister`-ed. Partially covered by the
   #418 dead-worktree auto-shed signal, but that binary is **merged, not yet deployed**;
   the deployed `workspace prune` classifier misses the dir-exists-but-`.git`-gone case.
3. **Duplicate `serena` daemon** (two `serena-proxy serena` under the supervisor) — no
   dedup on the workspace-proxy spawn path.

## Proposed fix (hub-side, options)

- **A. Single-window track-and-close (preferred):** record the PID of the hub-opened
  `explorer.exe`; before opening a new folder window, terminate the prior hub-owned one
  (and/or on supervisor shutdown). Bounds the count to 1 regardless of `SeparateProcess`.
- **B. Reuse instead of spawn:** open via a shell verb that delegates to the existing
  shell instance even under `SeparateProcess=1` (needs verification it actually reuses;
  `/select` does not).
- **C. Don't auto-open on `--force`:** print the path only (the diagnostic already does);
  make the reveal opt-in. Removes the highest-frequency trigger.
- **D. Supervisor orphan sweep:** the reconcile loop reaps hub-spawned `explorer.exe`
  whose owning GUI/tray is gone, alongside the dead-worktree daemon shed (deploy #418).

Recommend **A + C** for the generator, **D + deploy #418** for the reaper, plus a
`SeparateProcess=1` detection that warns once.

## Repro

On a host with `SeparateProcess=1`, invoke `mcphub gui --force` (or tray Open-data-folder)
N times; observe N persistent `explorer.exe` processes that never exit.

---
status: proposed
date: 2026-07-19
---
# The config-absence-gated auto-reaper reaps ONLY config-absent orphans (by design)

## Decision
The automatic orphan-reaper (with ScanClientConfigs) reaps a client-direct MCP-server process ONLY when its
signature is ABSENT from every readable client config. A currently-configured server is nominated then SPARED
(`ReapVerdictSparedConfigReferenced`) via the nomination⊆spare symmetry (`internal/api/cleanup.go` strict
nomination :642 ⊆ inclusive reference-spare :788). This is the shipped safety invariant (H5: a live client may
hold stdio pipes through a broken process chain, indistinguishable from a true orphan), NOT a bug.

## Consequence
- Rank-2 (wire ScanClientConfigs into the auto path) closes the config-ABSENT dead-parent orphan gap only.
- Config-PRESENT duplicate accumulation (N stale children of a still-configured server — the 2026-07-19
  incident) is UNREACHABLE by this reaper by construction. Clearing it requires:
  - Rank-1: `mcphub adopt <server>` → one shared Job-owned backend behind the hub URL (operator action, today).
  - Rank-3: automating the `AggressiveCleanup` path (kills live-rooted dups) — a SEPARATE work-item needing a
    safe duplicate-detector + its own $security-reviewer gate + the dangerous-class deny-list. Do NOT fold into Rank-2.

## Constrains future work
The Rank-3 aggressive-auto item must NOT reuse the config-absence gate (which would spare the very dups it targets);
it needs its own preview/confirm-equivalent + dup-detector.

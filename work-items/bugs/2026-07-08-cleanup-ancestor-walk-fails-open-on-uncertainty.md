---
id: 2026-07-08-cleanup-ancestor-walk-fails-open-on-uncertainty
status: open
severity: high
area: internal/api/cleanup.go (parseOrphans ancestor walk)
found-by: codex deep-security lane A + lane B (A2 PR5 adversarial re-verify)
context: adjacent to 2026-07-05-adopt-npx-orphans (A2 PR5)
---

## Summary

The orphan reaper's 16-deep ancestor walk in `parseOrphans`
([internal/api/cleanup.go](../../internal/api/cleanup.go)) **fails OPEN on
classification uncertainty**: two distinct cases end the walk without proving
the candidate is an orphan, yet both fall through to "orphan → reap-eligible",
so on the unattended 5-min `apply:true` ticker a still-live process can be
force-killed. This is PRE-EXISTING (master has it); A2 PR5 (#520) improved reaper
safety (config-absence gate, scanner-error snapshot-fail-closed, age floor) but
did NOT close these two walk fail-opens. They are grouped here as ONE coherent
change because both are the same defect class — *the walk must fail CLOSED
(spare) when it cannot prove ancestry* — and fixing them piecemeal across two
PRs would touch the same walk twice (edge-mining).

## The two fail-open cases

**Case A — dropped/absent ancestor row (codex lane A P0, lane B must-have-1).**
The walk breaks on a `byPID` miss (`parent, ok := byPID[cur]; if !ok { break }`,
cleanup.go ~1224). A `byPID` miss is the signature of BOTH a real orphan (dead
parent, genuinely absent) AND a false orphan (a live parent whose census row was
DROPPED — a malformed row silently skipped by `parseProcessSnapshotRows`
([processes.go](../../internal/api/processes.go) ~143, `ok=false` → dropped, no
degraded signal — OR a per-process WMI property race). PR #520's snapshot-
fail-closed only catches a *scanner* error (`bufio.ErrTooLong`) / a failed census
COMMAND; a syntactically-valid census that merely drops one malformed row returns
`snapErr == nil`, so `snapshotDegraded` never fires and the child is killed.

Because a `byPID` miss is ALSO the normal real-orphan case, a blunt
"byPID-miss → spare" would make the reaper permanently inert. The correct fix is
a **parent-liveness probe**: at the walk break, call `process.IsPidAlive(cur)`
([internal/process/pid_alive_*.go](../../internal/process/) — already exists,
per-OS). Alive → the census merely lost the row → SPARE (fail closed). Dead →
genuine orphan → proceed. The probe is fail-safe (a recycled PID reads alive →
spare; a since-exited parent reads dead → correct orphan).

**Case B — depth-cap exhaustion (codex lane A P1).** The walk caps at 16 levels
(`for cur, depth := r.ppid, 0; depth < 16; depth++`). A candidate that IS a real
descendant of a live `mcphub.exe daemon` / client at depth ≥17 exhausts the loop
without setting `ourDescendant`, then falls through to orphan → kill. Fix:
depth-cap-reached-while-unresolved (no protected ancestor AND no genuine root
`ppid==0`) → SPARE (fail closed). This one is cheap and does NOT risk inertness —
real orphans resolve via byPID-miss or `ppid==0` in a few levels; 16 present live
parents in a chain is exactly a deep live tree we WANT to spare.

## Correct fix (one coherent change)

Make the walk return a 3-state verdict — `protected` (found our-daemon/client
ancestor → spare), `genuine-orphan` (resolved to `ppid==0` root with no protected
ancestor → reap-eligible), `uncertain` (byPID-miss with a LIVE parent, OR
depth-cap exhausted, OR self-loop → spare, fail closed) — replacing today's
2-state `ourDescendant bool`. The `IsPidAlive` probe needs an **injectable seam**
(`var orphanParentAliveFn = process.IsPidAlive`) because it queries real OS PIDs;
without it the existing `parseOrphans` fixture tests (ppids 4000/7777) would go
flaky if such a PID happened to be live on the test host. Existing tests set the
seam to a controlled map.

## Also fold in (codex lane A P2, secondary)

The CLI aggressive kill path recomputes candidates and kills with
`ExpectPIDs == nil` (unlike the GUI, which binds `ExpectPIDs` to the
token-validated set), so drift between validation and kill is unbounded there.
Set `opts.ExpectPIDs = pidsOf(candidates)` in the CLI aggressive kill call. (PR
#520 already wired the aggressive path to fail closed on a *scanner* snapshot
error via `parseAggressiveCandidates` returning an error; this is the separate
validated-set-binding gap.)

## Why not in PR #520

PR #520 owns the config-absence gate + the fixes for what it changed (3 bot P2s,
the dry-run KillErr consent-surface P1 it introduced, and the all-return-paths
aggressive snapshot-scanner-error fail-closed). Cases A/B are pre-existing walk
fail-opens whose correct fix is a self-contained walk-classification refactor with
its own OS-probe seam + test updates; bundling it would grow #520 unboundedly and
mix "fix my regression" with "refactor a pre-existing walk". Severity is real
(P0 on the unattended ticker) but the pre-existing exposure is unchanged by #520,
which is a strict safety improvement.

## Repro sketch (Case A)

Census CSV with a candidate row (old, config-absent) whose live `mcphub.exe
daemon` ancestor row is malformed (wrong column count → `parseProcessSnapshotRow`
`ok=false` → dropped). `snapErr == nil`; `applyReapEligibilityGate(...,
snapshotDegraded=false)` stamps the child reap-eligible; apply kills a live
hub-managed descendant.

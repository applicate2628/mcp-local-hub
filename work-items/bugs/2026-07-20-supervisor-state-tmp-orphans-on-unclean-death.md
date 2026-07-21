---
title: supervisor-state.json atomic temp+rename leaks its temp file when the writer dies mid-write
severity: low
found-by: backend-engineer
found-in-phase: supervisor death-forensics lane (stderr sink + goroutine panic capture + heartbeat)
affected-surface: <state-dir>/.supervisor-state.json.tmp.<pid>.<hash> (writer in internal/cli/supervisor_runtime_tracker.go PersistTo)
context: adjacent-finding
status: open
related-branch: feat/supervisor-death-forensics
---

## Symptom

The live state dir accumulates orphaned atomic-write temp files that no
process ever renamed into place or cleaned up.

Verified read-only on the operator's host, 2026-07-20 15:xx local:

```
count        = 22
zero-length  = 4
oldest       = 2026-05-19 02:17:02
newest       = 2026-07-18 23:54:43
total bytes  = 73249
```

Sample names (the shape is `.supervisor-state.json.tmp.<pid>.<hash>`):

```
.supervisor-state.json.tmp.58464.b9ef0570daf8b704    1922 B  2026-05-19 02:17:02
.supervisor-state.json.tmp.61640.ad6e8a75166c3fa4    1877 B  2026-05-19 04:52:34
.supervisor-state.json.tmp.183904.f6df43bb2b56feb5      0 B  2026-06-09 13:04:21
```

A sibling forensic lane independently reported 21; this lane counted 22, so
the set is still growing.

## Why it matters beyond disk usage

These files are themselves **weak evidence of past unclean supervisor
deaths**. The write path is temp-create → write → rename; a temp file that
was never renamed means the writing process stopped between those steps.
The 4 zero-length ones mean death between create and first write.

That makes the orphan set a (lossy, unattributed) shadow record of exactly
the death class the death-forensics lane exists to close — which is why it
is recorded here rather than discarded. It is NOT a substitute for the
stderr sink / goroutine-panic events / heartbeat: it carries no timestamp of
death cause, no stack, and no distinction between a crash and a kill.

**Correlation is suggestive, not established.** The oldest orphans (May)
predate the 42-hour forensic window, and an external `Stop-Process -Force`
name-matched sweep would produce the same residue as an internal panic. Do
not treat orphan count as a crash count.

## Not fixed here — deliberately

A reaper was explicitly out of scope for the death-forensics branch. Scope
discipline: this branch's mandate is that a future death leaves evidence,
not that historical residue is swept. Two further reasons to keep it
separate:

1. A reaper must not delete a temp file belonging to a **live** writer, so
   it needs a liveness/age gate — the same class of fail-closed identity
   reasoning as the port-squatter reap, not a blind glob-delete.
2. Deleting them now would destroy the only physical trace of the deaths
   under investigation while the investigation is still open.

## Suggested fix (for whoever picks this up)

Bounded, age-gated reap on supervisor startup, mirroring the existing
`mcphub.exe.old-<ts>` sweep already documented in CLAUDE.md ("`.old-<ts>`
cleanup"): glob `<state-dir>/.supervisor-state.json.tmp.*`, and remove only
entries whose embedded `<pid>` is not alive AND whose mtime is older than a
threshold. Failures non-fatal + logged warn. Emit a count so the reap is
itself observable rather than silent.

Do NOT reap purely by age without the liveness check — a slow write on a
loaded host would then race the reaper.

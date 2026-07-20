# Bug: stderr sink loses MAIN-goroutine panics — deferred release() restores stderr before the runtime prints the traceback

- id: 2026-07-20-stderr-sink-misses-main-goroutine-panic
- context: feat/supervisor-death-forensics (branch, pre-merge)
- status: open
- severity: high
- area: internal/cli/supervise.go (defer stderrSink.release() after openSupervisorStderrSink)
- found-by: qa-engineer

## Reproduction

Empirically proven 2026-07-20 with a standalone probe replicating the exact
mechanism (SetStdHandle + `os.Stderr` swap, deferred restore-then-close, on
windows-amd64):

| mode | shape | panic in sink? | panic in original stderr? |
| --- | --- | --- | --- |
| main-defer | `defer release()` then panic on the SAME goroutine — exactly `runSupervise`'s shape | **NO** | YES |
| main-nodefer | no defer, panic on main | YES | NO |
| goroutine-defer | defer + panic on a background goroutine — the shape the branch's subprocess test covers | YES | NO |

Go semantics: an unrecovered panic runs the panicking goroutine's deferred
functions FIRST, and only then does the runtime print `panic: ...` + traceback
and exit 2. `defer stderrSink.release()` in `runSupervise` therefore restores
the original (detached, unbound) stderr binding and closes the sink file
BEFORE the traceback is written, for any panic raised on `runSupervise`'s own
goroutine.

## Expected vs actual

- Expected (per the in-code comment at the sink install site: "Placed BEFORE
  the event log so a panic during the remaining startup is still captured"):
  a panic anywhere after sink install lands in `supervisor-stderr.log`.
- Actual: only panics on OTHER goroutines are captured. All of
  `runSupervise`'s remaining startup body, the final select loop, and every
  shutdown/signal/IPC-exit handler run on the main goroutine — a panic there
  is lost exactly as before the branch. The 19s/59s death sessions in the
  motivating forensic window are plausibly startup-window shapes, i.e. the
  class this placement comment claims to cover. The comment asserts the
  opposite of actual behavior.

## Suggested direction (implementer decides)

Make the release defer panic-aware, e.g.
`defer func() { if r := recover(); r != nil { panic(r) }; stderrSink.release() }()`
(recover-then-repanic leaves the sink installed during the final unwind, so
the runtime's writer still hits the sink; test-mode handle cleanup on normal
returns is preserved). Add a subprocess test variant that panics on the MAIN
goroutine through `runSupervise`'s actual defer shape — the existing
`TestSupervisorStderrSink_CapturesRuntimePanic` panics on a background
goroutine and cannot catch this.

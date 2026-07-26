---
title: Serena interlock release-before-start swallows its Unlock, so the migrate starts a supervisor that cannot acquire the lock
severity: high
found-by: backend-engineer
found-in-phase: adjacent sweep while fixing the SupervisorRunningUnderStateDir probe-release swallow
affected-surface: internal/cli/migrate_serena_restart_windows.go; internal/cli/migrate_serena.go; internal/api/supervisor_lock.go
context: adjacent-finding
status: open
related-work-item: 2026-07-25-liveness-headless-gui-recovery
---

## Defect

Fourth instance of the class established by `2d68690f` (GUI single-instance
lease + supervisor event-log flock), `b9db5208` (serena removal fence), and the
`SupervisorRunningUnderStateDir` probe-release fix this finding was spun out of:
**an unconfirmed flock release reported as success, where the caller's very next
step depends on the lock actually being free.**

`(*api.SupervisorLock).Release()` (`internal/api/supervisor_lock.go:178-192`)
ends in `_ = l.fl.Unlock()` and has **no return type**, so its failure is
structurally unreportable. That is harmless at most of its call sites, but not
at this one:

`defaultAcquireSupervisorInterlock` (`internal/cli/migrate_serena_restart_windows.go:154-166`)
hands the migrate driver a release closure typed `func()`:

```go
var once sync.Once
release := func() { once.Do(lock.Release) }
```

The driver calls `releaseInterlock()` and then IMMEDIATELY starts a supervisor
(`internal/cli/migrate_serena.go:1153-1160` on the verify-failure branch, and
`:1175-1181` on the normal branch). The comment at `:1170-1173` states the
dependency explicitly: *"Release the interlock before the (normal or no-op)
start. The just-started supervisor must AcquireSupervisorLock itself; holding it
here would block the child's own acquire."*

If `Unlock()` fails, the release is a no-op, the migrate process keeps the
`supervisor.lock` flock until it exits, and the supervisor it then starts fails
its own `AcquireSupervisorLock` and exits. The migrate reports the start as
attempted (or succeeded, on the branches that do not poll), leaving a committed
`runtime_spec`-bearing intent on disk with no supervisor reconciling it — the
exact half-state the `willStart` preflight gate exists to prevent.

The same shape applies to the serena auto-register cutover, which takes the same
interlock through `autoRegisterAcquireInterlockFn func() (*SupervisorLock, func(), error)`
(`internal/api/serena_auto_register.go:1044`).

## Why it was not fixed in the same change

Out of the approved change surface (that change owned `SupervisorRunningUnderStateDir`
only), and the fix is not the one-line signature widening it first appears to be:

1. **Widening `Release()` to `Release() error` is a silent no-op at 6 of its 7
   production call sites.** Go permits discarding a returned value at a
   statement call, so `lk.Release()` and `defer lk.Release()` keep compiling and
   keep discarding. Verified empirically this session: widening the signature
   and running `go build ./...` broke exactly ONE site —
   `migrate_serena_restart_windows.go:164`, and only because `lock.Release` is
   passed there as a method VALUE to `sync.Once.Do`, which requires `func()`:

   ```
   internal\cli\migrate_serena_restart_windows.go:164:30: cannot use lock.Release
   (value of type func() error) as func() value in argument to once.Do
   ```

   So the signature change alone would create the *illusion* of reportability
   while changing no behavior anywhere.

2. **The real fix ripples into an exported API.** The release closure is typed
   `func()` inside the exported `SetSerenaAutoRegisterCutoverPrimitives(...)`
   signature and the `autoRegisterAcquireInterlockFn` seam, and
   `StateDirLocks.Release()` (`internal/api/state_dir_locks.go:70-79`) wraps two
   `SupervisorLock.Release()` calls behind its own no-return `Release()`,
   consumed as `defer locks.Release()` at `internal/cli/strict_mode.go:308,777`.
   Making the failure reportable end-to-end means widening that cascade.

3. **The established precedent is to ADD, not widen.** `2d68690f` introduced
   `ReleaseErr() error` alongside an unchanged `Release()` precisely so the many
   harmless call sites stay untouched, and then fixed the ONE caller that had to
   fail closed. The same shape applies here.

## Suggested fix

Add `(*SupervisorLock).ReleaseErr() error` alongside the unchanged `Release()`,
mirroring `internal/gui.SingleInstanceLock`. Change the interlock release closure
type to `func() error` (dropping the `sync.Once.Do` method-value form for
`sync.OnceValue`), and make the migrate/auto-register drivers treat an
unconfirmed release as a hard failure BEFORE attempting the supervisor start —
reporting that the intent is committed but no supervisor could be started,
rather than starting one that is guaranteed to lose its acquire.

## Verification that would close it

A mutation-proven test in the shape of
`TestSupervisorRunningUnderStateDir_UnconfirmedReleaseStopsAutostartOwnerStart`:
inject an interlock lease whose `Unlock` fails, and assert the driver does NOT
call `migrateSerenaStartFn` and surfaces the committed-intent-no-supervisor
error. Restoring the swallow must make that specific test fail.

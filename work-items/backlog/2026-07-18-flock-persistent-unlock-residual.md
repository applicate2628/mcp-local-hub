# gofrs/flock: a persistent Unlock failure leaves the OS lock held (definitive release residual)

Status: candidate
Severity: P3 (near-impossible trigger + process-exit backstop; pre-existing, not a Phase-E regression)
Filed: 2026-07-18
Found: item-3 Unit B Phase E commission (Sol P1 + Terra confirm — the deterministic-lease-release property).

## The residual
`SingleInstanceLock.release()` (`internal/gui/single_instance.go`) uses `github.com/gofrs/flock` v0.13.0.
Its `Close()` merely delegates to `Unlock()` (`flock.go:99`), and on a real `UnlockFileEx` / `flock`
syscall error the library returns BEFORE `reset()` → `resetFh()` → `fh.Close()` (`flock_windows.go:97`,
`flock_unix.go:105`), so the underlying descriptor is never closed and the OS lock stays held. gofrs/flock
exposes no public file-handle accessor (`fh` is private), so `release()` cannot close the descriptor
itself. `release()` therefore: (1) retries `Unlock()` a bounded number of times (recovers a TRANSIENT
failure), (2) calls `Close()` as a last attempt, (3) unconditionally clears `l.fl` (so a discarded lease
never double-frees). On a PERSISTENT failure the OS lock remains held until process exit.

## Why it is accepted (not blocking Phase E)
- **Near-impossible trigger:** `UnlockFileEx` failing on a lock this process legitimately holds requires
  handle corruption / an OS bug; it does not occur in normal operation.
- **Process-exit backstop:** every current tentative-lease caller (a GUI entrant, a one-shot
  `mcphub supervise --ensure-alive` tick) is short-lived, and the OS frees all file locks on process exit.
- **Pre-existing:** the legacy `Release` path has always had this limitation; Phase E's new tentative-lease
  usage adds no new reachability (its callers are short-lived too).
- Fixing it requires vendoring/patching gofrs/flock or replacing it with a raw-handle
  (`LockFileEx`/`flock(2)` on an owned `*os.File`) lock — disproportionate to the near-zero practical risk.

## The definitive fix (deferred)
Replace the gofrs/flock usage in the GUI single-instance lock with a raw-`*os.File` lock so `release()` can
`Close()` the owned descriptor directly (closing the handle drops the OS lock even when the unlock syscall
errors), returning the unlock/close error. Then a genuine fault-injection seam on the real handle can pin
the persistent-failure path. Until then, `release()` documents the residual inline and the honest tests
cover transient-recovery (`TestSingleInstanceLock_ReleaseRecoversTransientUnlockFailure`) + the
handle-cleared/reported/idempotent persistent path
(`TestSingleInstanceLock_ReleasePersistentUnlockFailureClearsHandleAndReports`).

## Do NOT re-file as a Phase-E blocker
This is a general `single_instance.go` + dependency limitation, tracked here so Phase E (and the whole
gated D–J group) ships on its bounded, documented residual.

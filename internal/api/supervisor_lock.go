package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gofrs/flock"
)

// SupervisorLock is the singleton lock primitive used by `mcphub supervise`
// to enforce single-supervisor concurrency. Generalized from
// internal/gui/single_instance.go:39-62 so internal/cli/supervise.go
// does not need to import internal/gui (preserves dependency direction
// per spec §"Package ownership").
//
// The lock has two parts:
//   - <path>.lock: flock-managed advisory lock (gofrs/flock) — source of
//     truth for ownership. Released on Unlock OR process exit.
//   - <path>.owner.json: pidport sidecar JSON ({pid, started_at}) —
//     metadata for liveness probes; the next acquirer overwrites it
//     atomically via WriteStateFileAtomic.
//
// Contention diagnostics: when flock TryLock fails (existing holder), the
// caller reads .owner.json and probes the recorded PID only to produce a
// precise error. The sidecar is never repaired by a losing contender; flock is
// the ownership source of truth, and deleting metadata before owning the lock
// corrupts the live holder's IPC handshake surface.
type SupervisorLock struct {
	path  string
	fl    *flock.Flock
	owner SupervisorLockOwner
}

var (
	supervisorLockOwnerMissingRetryWindow = 2 * time.Second
	supervisorLockOwnerMissingRetryDelay  = 25 * time.Millisecond
)

// SupervisorLockOwner is the pidport sidecar describing the current holder.
type SupervisorLockOwner struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// AcquireSupervisorLock takes <path>.lock via flock and writes
// <path>.owner.json with current PID + start_time (RFC3339Nano UTC).
// If the flock is already held, owner metadata is diagnostic only. A missing,
// corrupt, or stale sidecar while the lock is held fails closed; the next real
// acquirer overwrites the sidecar only after TryLock succeeds.
func AcquireSupervisorLock(path string) (*SupervisorLock, error) {
	lk := flock.New(path + ".lock")
	deadline := time.Now().Add(supervisorLockOwnerMissingRetryWindow)
	for {
		got, err := lk.TryLock()
		if err != nil {
			return nil, fmt.Errorf("flock: %w", err)
		}
		if got {
			break
		}
		owner, ownerErr := ReadSupervisorLockOwner(path)
		if ownerErr != nil {
			if errors.Is(ownerErr, os.ErrNotExist) && time.Now().Before(deadline) {
				time.Sleep(supervisorLockOwnerMissingRetryDelay)
				continue
			}
			return nil, fmt.Errorf("supervisor.lock held but owner metadata invalid: %w", ownerErr)
		}
		if !isOwnerLive(owner) {
			return nil, fmt.Errorf("supervisor.lock held but owner metadata stale or unobservable: pid=%d", owner.PID)
		}
		return nil, fmt.Errorf("supervisor.lock held by live PID %d", owner.PID)
	}

	// Write owner sidecar.
	owner := SupervisorLockOwner{
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := WriteStateFileAtomic(path+".owner.json", owner); err != nil {
		_ = lk.Unlock()
		return nil, fmt.Errorf("owner sidecar: %w", err)
	}
	return &SupervisorLock{path: path, fl: lk, owner: owner}, nil
}

// Owner returns the exact sidecar identity written when the lock was acquired.
// IPC listeners use this as the single source of truth for hello frames.
func (l *SupervisorLock) Owner() SupervisorLockOwner {
	if l == nil {
		return SupervisorLockOwner{}
	}
	return l.owner
}

// ReadSupervisorLockOwner reads <path>.owner.json and returns the
// parsed sidecar. Returns the zero owner + error on missing/corrupt
// file.
func ReadSupervisorLockOwner(path string) (SupervisorLockOwner, error) {
	var o SupervisorLockOwner
	raw, err := os.ReadFile(path + ".owner.json")
	if err != nil {
		return o, err
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, err
	}
	return o, nil
}

// Release removes the owner sidecar and unlocks the flock. Idempotent.
//
// The owner sidecar IS removed here (unlike internal/gui's pidport
// which is intentionally retained — see single_instance.go:81 for the
// rationale). The supervisor lock has no second-instance handshake
// (no port to probe), so the sidecar is purely identity metadata; a
// successor will overwrite it on its next acquire anyway, and removing
// it on graceful release keeps the state directory tidy.
func (l *SupervisorLock) Release() {
	if l == nil || l.fl == nil {
		return
	}
	os.Remove(l.path + ".owner.json")
	_ = l.fl.Unlock()
	l.fl = nil
}

// isOwnerLive probes whether the recorded PID is still running by
// sending signal 0. PID 0 is treated as not-live (canonical "unset"
// sentinel; any owner sidecar with PID 0 is malformed).
//
// IMPORTANT — this probe is POSIX-effective only. On POSIX, kill(pid, 0)
// returns ESRCH for a dead PID and EPERM when the PID is alive but owned by a
// different user — both cases this function treats as "not ours to assume
// live", returning false to allow reclaim. On Windows os.FindProcess always
// succeeds, but Go's Process.Signal implements ONLY os.Kill and returns an
// error for signal 0 (empirically verified — see TestSupervisorRunningUnderStateDir),
// so this returns false even for a LIVE PID. Callers that need a cross-platform
// "is the supervisor running" answer must use the flock (the authoritative
// ownership signal), as SupervisorRunningUnderStateDir does; isOwnerLive here is
// only the diagnostic refinement AcquireSupervisorLock applies AFTER its flock
// TryLock has already failed, so its Windows weakness degrades a message, not a
// decision. The repo ALREADY ships the correct cross-platform primitive —
// internal/process.IsPidAlive (Windows OpenProcess + WaitForSingleObject(0));
// migrating isOwnerLive onto it is a cheap, contained follow-up tracked
// separately (it would change only the AcquireSupervisorLock diagnostic string).
//
// Pattern intentionally simpler than internal/gui/single_instance.go's
// processID probe — that one needs image basename, argv, and start time
// for the kill identity gate; the supervisor lock only needs the
// liveness signal.
func isOwnerLive(o SupervisorLockOwner) bool {
	if o.PID == 0 {
		return false
	}
	p, err := os.FindProcess(o.PID)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// SupervisorRunningUnderStateDir reports whether a LIVE supervisor process
// currently owns the singleton lock under stateDir — i.e. its intent watcher is
// actively re-reading <stateDir>/supervisor-intent.json. It returns the owner
// PID (best-effort, from the sidecar) for diagnostics, plus a TRI-STATE result:
//
//   - (false, 0, nil)    — definitively NOT running (lock acquirable).
//   - (true,  pid, nil)  — definitively running (lock held by another process).
//   - (false, 0, err)    — UNDETERMINABLE (the lock probe itself errored).
//
// The authoritative signal is the gofrs/flock itself, NOT the owner sidecar +
// isOwnerLive: isOwnerLive's PID-signal probe is POSIX-effective only (Go's
// Windows Process.Signal supports only Kill and errors on signal 0, so it
// reports a LIVE PID as not-live — see isOwnerLive's note), which would make
// this probe a no-op on the Windows GA platform. Instead we attempt a
// non-blocking TryLock on the supervisor's own flock file: if it is held we
// could not acquire (a live supervisor holds it → running); if we acquire it we
// release immediately (no holder → not running). flock ownership is released by
// the kernel on holder exit, so a crashed supervisor frees the lock and reads as
// not running — exactly when the NEXT supervisor start is a fresh process on the
// current binary.
//
// FAIL-CLOSED CONTRACT (consultant PR #246 r2 #1): a non-nil err means liveness
// could not be determined (a rare open/lock syscall failure — e.g. a
// locked-down or AV-instrumented Windows host where LockFileEx errors instead of
// cleanly reporting "held"). A caller gating a destructive/unsafe operation MUST
// treat err != nil as "assume running" and refuse. Returning "not running" on a
// probe error (the naive polarity) would SILENTLY disable the gate on exactly
// the hardened hosts where split-brain protection matters most.
//
// Used by the §7.1 spec-bearing supervisor-intent write gate (bot PR #246 r2):
// an OLD running supervisor binary's ReadSupervisorIntent uses
// DisallowUnknownFields and would reject a newly written runtime_spec field, so
// the gate refuses the spec-bearing write while a supervisor is live (or while
// liveness is undeterminable).
func SupervisorRunningUnderStateDir(stateDir string) (running bool, pid int, err error) {
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	// Mirror AcquireSupervisorLock's flock leaf: it locks `path + ".lock"`.
	lk := flock.New(lockPath + ".lock")
	got, lerr := lk.TryLock()
	if lerr != nil {
		// Probe error → UNDETERMINABLE. Surface it so the gate fails closed.
		return false, 0, fmt.Errorf("probe supervisor lock %s: %w", lockPath+".lock", lerr)
	}
	if got {
		// We acquired it → no supervisor held it → not running. Release at once.
		_ = lk.Unlock()
		return false, 0, nil
	}
	// Held by another process → a supervisor is running. The PID is best-effort
	// diagnostic from the sidecar (absent/corrupt → 0).
	owner, _ := ReadSupervisorLockOwner(lockPath)
	return true, owner.PID, nil
}

package api

import (
	"encoding/json"
	"fmt"
	"os"
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
// Stale-PID detection: when flock TryLock fails (existing holder), the
// caller reads .owner.json and probes the recorded PID. If the PID is
// not running (signal 0 fails), the lock is considered stale; the
// sidecar is removed and TryLock retried. This handles the case where
// the previous holder crashed without releasing the flock (Windows: the
// kernel releases the flock on process exit, so this branch primarily
// covers degraded/unobservable cases).
type SupervisorLock struct {
	path string
	fl   *flock.Flock
}

// SupervisorLockOwner is the pidport sidecar describing the current holder.
type SupervisorLockOwner struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// AcquireSupervisorLock takes <path>.lock via flock and writes
// <path>.owner.json with current PID + start_time (RFC3339Nano UTC).
// On stale-holder detection (flock held but recorded PID is not
// running), the owner sidecar is removed and the flock is retried.
func AcquireSupervisorLock(path string) (*SupervisorLock, error) {
	lk := flock.New(path + ".lock")
	got, err := lk.TryLock()
	if err != nil {
		return nil, fmt.Errorf("flock: %w", err)
	}
	if !got {
		// Lock held — check if stale.
		owner, _ := ReadSupervisorLockOwner(path)
		if !isOwnerLive(owner) {
			// Stale — force reclaim by deleting owner sidecar and retrying.
			os.Remove(path + ".owner.json")
			got, err = lk.TryLock()
			if err != nil || !got {
				return nil, fmt.Errorf("flock reclaim: %v / got=%v", err, got)
			}
		} else {
			return nil, fmt.Errorf("supervisor.lock held by live PID %d", owner.PID)
		}
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
	return &SupervisorLock{path: path, fl: lk}, nil
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
// On Windows os.FindProcess always succeeds (the OS does not gate
// process-handle creation on existence); signal 0 still probes via
// OpenProcess + GetExitCodeProcess and fails on a dead/recycled PID.
// On POSIX, kill(pid, 0) returns ESRCH for a dead PID and EPERM when
// the PID is alive but owned by a different user — both cases this
// function treats as "not ours to assume live", returning false to
// allow reclaim.
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

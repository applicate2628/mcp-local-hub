package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/process"
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
	// quiet marks a flock-only acquire (AcquireSupervisorLockQuiet) that did NOT
	// write <path>.owner.json. A quiet holder borrows the flock as a pure mutex
	// (the serena reap→write→start interlock) and must NOT touch the sidecar: the
	// sidecar is the REAP's PID source (ForceKillSupervisor /
	// QuiesceTimers / ExitGraceful read it to target the OLD supervisor), so
	// overwriting it with the interlock-holder's own CLI PID would make the reap
	// kill the caller instead of the old supervisor. Release() therefore skips the
	// sidecar removal for a quiet lock — it never owned that file.
	quiet bool
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

// AcquireSupervisorLockQuiet takes <path>.lock via flock WITHOUT writing (or
// touching) <path>.owner.json. It is the variant the serena migrate / serena
// auto-register cutovers use to borrow supervisor.lock as a pure reap→write→start
// mutex (Phase 2 of .plans/2026-06/plan-serena-lock-interlock-2026-06-09.md).
//
// Why no sidecar write: the owner sidecar is the REAP's PID source — the reap
// primitive (ReapSupervisorForRestart → ForceKillSupervisor /
// QuiesceTimers / ExitGraceful, internal/cli/install_migration_wiring_windows.go)
// reads <path>.owner.json to pick the PID it taskkills / IPC-handshakes against.
// The migrate and auto-register reap the OLD supervisor while (or just before)
// holding this interlock; if the interlock acquire OVERWROTE the sidecar with the
// CLI holder's own os.Getpid() (as the full AcquireSupervisorLock does,
// supervisor_lock.go), the reap would read that and force-kill the migrate /
// router process instead of the old supervisor (bot PR #276 finding 1), and the
// post-reap acquire-too-late gap would let a foreign supervisor slip in (finding
// 2). A QUIET acquire leaves the sidecar pointing at the old supervisor (or
// absent), so the reap targets the correct PID, while the flock still provides the
// mutual exclusion the interlock needs.
//
// The returned *SupervisorLock has .path and .fl set (Owner() is the zero value —
// a quiet holder never recorded one), so it STILL mints a valid §7.1 bypass token:
// AllowSpecBearingWriteBypass()'s gate identity check verifies only
// lk.fl != nil (still held) AND lk.path == the gate's supervisor.lock path
// (install_parsed_manifest.go) — neither needs the sidecar. The interlock callers
// are CLI processes that never serve supervisor IPC, so they have no legitimate
// reason to own the sidecar that feeds the supervisor's IPC hello frame.
//
// Contention diagnostics are weaker here than AcquireSupervisorLock by design:
// since this never wrote a sidecar, on a failed TryLock it reports a generic
// "held" error rather than probing a PID. The interlock callers map that to a
// fail-loud / defer-and-retry path; a precise PID is not needed.
func AcquireSupervisorLockQuiet(path string) (*SupervisorLock, error) {
	lk := flock.New(path + ".lock")
	got, err := lk.TryLock()
	if err != nil {
		return nil, fmt.Errorf("flock: %w", err)
	}
	if !got {
		return nil, fmt.Errorf("supervisor.lock held (quiet acquire could not take the flock at %s)", path+".lock")
	}
	// Deliberately NO owner-sidecar write — see the doc comment.
	return &SupervisorLock{path: path, fl: lk, quiet: true}, nil
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
	raw, err := readStateFileInodeAnchored(path + ".owner.json")
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
	// A quiet holder (AcquireSupervisorLockQuiet) never wrote the sidecar — it
	// belongs to the OLD supervisor the interlock caller reaps — so the quiet
	// Release must NOT remove it (removing the old supervisor's sidecar would
	// strip the reap's PID source for any concurrent reader). Only a full
	// AcquireSupervisorLock holder owns the sidecar and tidies it here.
	if !l.quiet {
		os.Remove(l.path + ".owner.json")
	}
	_ = l.fl.Unlock()
	l.fl = nil
}

// isOwnerLive probes whether the recorded PID is still running. It delegates
// to internal/process.IsPidAlive — the single cross-platform owner of PID
// liveness (Windows OpenProcess + WaitForSingleObject; Linux kill(0) with
// /proc zombie exclusion; other POSIX kill(0)) — rather than re-implementing a
// per-platform probe here (deep-review P4: the old inline probe used Go's
// Process.Signal(0), a no-op on Windows that reported every live PID as dead
// and wrongly degraded the AcquireSupervisorLock contention diagnostic).
// IsPidAlive treats a non-positive PID as dead, so the canonical "unset"
// sentinel PID 0 in a malformed owner sidecar reads as not-live.
//
// This is the diagnostic refinement AcquireSupervisorLock applies AFTER its
// flock TryLock has already failed; it never gates a decision by itself, and a
// PID-liveness check is inherently racy against PID reuse. Callers needing an
// authoritative cross-platform "is the supervisor running" answer must use the
// flock, as SupervisorRunningUnderStateDir does (only liveness is needed here,
// not the full image/argv/start-time identity gate single_instance.go uses).
func isOwnerLive(o SupervisorLockOwner) bool {
	return process.IsPidAlive(o.PID)
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
// isOwnerLive: even though isOwnerLive now has a real per-platform liveness
// probe (supervisor_lock_liveness_posix.go / _windows.go), a PID-liveness
// check is inherently racy against PID reuse and gives no exclusion
// guarantee — it can only ever be a diagnostic hint, never the ownership
// decision. Instead we attempt a non-blocking TryLock on the supervisor's own
// flock file: if it is held we could not acquire (a live supervisor holds it
// → running); if we acquire it we release immediately (no holder → not
// running). flock ownership is released by the kernel on holder exit, so a
// crashed supervisor frees the lock and reads as not running — exactly when
// the NEXT supervisor start is a fresh process on the current binary.
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
//
// RELEASING THE PROBE LEASE IS PART OF THE ANSWER, NOT CLEANUP. Acquiring the
// flock only proves nobody ELSE held it at that instant; returning (false, nil)
// additionally asserts the lock is free NOW — which is FALSE if this probe still
// owns it. gofrs/flock exposes no file handle and its Close() is literally
// `return f.Unlock()` (v0.13.0 flock.go:99-101), so on a persistent Unlock
// syscall failure there is nothing left to try: the descriptor stays open and
// the OS lock stays held until THIS process exits
// (work-items/backlog/2026-07-18-flock-persistent-unlock-residual.md). The old
// code discarded that error via `_ = lk.Unlock()` and still reported a DEFINITE
// "not running", violating the fail-closed contract stated directly above. The
// concrete harm is self-defeat in the fleet's most load-bearing liveness
// primitive: `mcphub supervise --ensure-alive` (the \mcp-local-hub-liveness
// task) reads "not running" and starts a supervisor, whose AcquireSupervisorLock
// then blocks on the lock the probing process itself is still holding.
//
// Fail closed instead: an unconfirmed release is reported as UNDETERMINABLE
// through the SAME error return every consumer already treats as "assume
// running" — no consumer needs to change. This matches the invariant
// internal/gui.ProbeSingleInstanceLockUnheld already states for the GUI
// single-instance flock (single_instance.go:239-260) and the serena removal
// fence, whose self-releasing probes are the same class.
func SupervisorRunningUnderStateDir(stateDir string) (running bool, pid int, err error) {
	return supervisorRunningUnderStateDir(stateDir, func(lockFilePath string) supervisorLockProbeLease {
		return flock.New(lockFilePath)
	})
}

// supervisorLockProbeLease is the minimal flock surface the probe needs from its
// TENTATIVE lease. *flock.Flock satisfies it.
type supervisorLockProbeLease interface {
	TryLock() (bool, error)
	Unlock() error
}

// supervisorRunningUnderStateDir is SupervisorRunningUnderStateDir with the
// probe-lease constructor passed in, so a test can supply a lease whose Unlock
// fails persistently and exercise the fail-closed release branch without needing
// a real UnlockFileEx failure (which is unreachable synthetically). Taking the
// seam as a PARAMETER rather than a package-level var keeps it off production
// global state — the same shape internal/gui.probeSingleInstanceLockUnheld uses
// for the identical probe on the GUI single-instance lock.
func supervisorRunningUnderStateDir(
	stateDir string,
	newProbeLease func(lockFilePath string) supervisorLockProbeLease,
) (running bool, pid int, err error) {
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	// Mirror AcquireSupervisorLock's flock leaf: it locks `path + ".lock"`.
	lk := newProbeLease(lockPath + ".lock")
	got, lerr := lk.TryLock()
	if lerr != nil {
		// Probe error → UNDETERMINABLE. Surface it so the gate fails closed.
		return false, 0, fmt.Errorf("probe supervisor lock %s: %w", lockPath+".lock", lerr)
	}
	if got {
		// We acquired it → no supervisor held it at that instant. Release at
		// once — and report a failed release as UNDETERMINABLE rather than
		// swallowing it, because an unreleased probe lease makes "not running"
		// a claim this process itself falsifies (see the doc comment).
		if uerr := lk.Unlock(); uerr != nil {
			return false, 0, fmt.Errorf("release supervisor lock probe lease %s: %w", lockPath+".lock", uerr)
		}
		return false, 0, nil
	}
	// Held by another process → a supervisor is running. The PID is best-effort
	// diagnostic from the sidecar (absent/corrupt → 0).
	owner, _ := ReadSupervisorLockOwner(lockPath)
	return true, owner.PID, nil
}

// InstallParsedManifestBypass is an opaque, constructor-enforced capability
// token that authorizes InstallParsedManifest to SKIP its §7.1 spec-bearing
// supervisor-intent write gate (install_parsed_manifest.go) — the gate that
// otherwise refuses a runtime_spec write while a supervisor holds its singleton
// lock. The token exists because the migrate / serena auto-register flows
// (Phase 2) acquire that VERY lock around their reap+rewrite: to those callers
// the held lock is THEIR OWN handle, not a foreign supervisor, so the gate's
// fail-closed refuse is a false positive for them specifically.
//
// The single field is UNEXPORTED, so no code outside package api can forge a
// non-nil token: the ONLY way to obtain one with a non-nil lk is to hold a real
// *SupervisorLock (returned by AcquireSupervisorLock) and call
// AllowSpecBearingWriteBypass on it. The zero value (lk == nil) is "no bypass"
// and is the default for every existing call site. The gate re-verifies
// IDENTITY at use time (lk still held AND lk.path == the gate's own
// supervisor.lock path), so even a real-but-mismatched or already-released token
// is rejected — see InstallParsedManifestOpts.SupervisorLockBypass.
//
// It wraps a *POINTER* to the lock (not a value): SupervisorLock holds a
// *flock.Flock, never a value lock, so copying this token (e.g. via an opts
// copy) is safe and go vet's copylocks does not fire.
type InstallParsedManifestBypass struct{ lk *SupervisorLock }

// AllowSpecBearingWriteBypass mints the capability token that lets the holder of
// THIS live lock bypass InstallParsedManifest's §7.1 spec-bearing write gate.
// Calling it on a released lock yields a token whose lk.fl is nil, which the
// gate's identity check rejects (the bypass requires the lock to be STILL held
// at use time), so a stale token can never re-open the split-brain the gate
// prevents.
func (l *SupervisorLock) AllowSpecBearingWriteBypass() InstallParsedManifestBypass {
	return InstallParsedManifestBypass{lk: l}
}

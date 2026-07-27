package gui

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// ErrSingleInstanceBusy is returned by AcquireSingleInstance when another
// mcphub gui process already holds the lock. Callers should read the
// pidport file, probe the incumbent's /api/ping, and POST
// /api/activate-window before giving up.
var ErrSingleInstanceBusy = errors.New("another mcphub gui is already running")

// ErrHandoffReserved reports that a fresh restart-v3 reservation protects the
// single-instance lease for its designated child. An ordinary entrant, a
// third entrant with the wrong nonce, or a late designated child with no
// matching reservation must release any tentative lease before returning it.
var ErrHandoffReserved = errors.New("GUI handoff lease is reserved for the designated child")

// ErrGUIOwnerLeaseUnknown classifies path, marker, DACL, clock, or flock
// uncertainty. It is deliberately distinct from ErrSingleInstanceBusy: an
// unknown observation is never proof that the GUI owner is dead or that a
// takeover is safe.
var ErrGUIOwnerLeaseUnknown = errors.New("GUI owner lease state is unknown")

// HandoffMarkerReader is the read-only Phase-E view of HandoffMarkerStore.
// Reservation-aware acquisition and probing may inspect the marker, but this
// seam grants them no marker mutation authority.
type HandoffMarkerReader interface {
	Read() (*HandoffMarkerRecord, error)
}

// SingleInstanceLease is the ownership seam for the GUI flock. A returned
// lease is an owned resource and Release must run on every success, error,
// cancellation, and timeout path.
type SingleInstanceLease interface {
	Release()
}

// OwnedSingleInstanceLease is a lease whose release outcome is OBSERVABLE.
//
// ProbeGUIOwnerLease hands a GUIOwnerLeaseStateFree caller an OWNED lease, and
// that caller's contract is fail-closed on release, exactly like
// ProbeSingleInstanceLockUnheld's: an UNCONFIRMED release may leave the flock
// held by this process until it EXITS (see release()'s persistent-failure
// residual note), so the caller must not go on to act as though the GUI
// single-instance flock were free. Release() alone cannot express that — it
// discards release()'s error — so the owned-probe seam requires ReleaseErr.
type OwnedSingleInstanceLease interface {
	SingleInstanceLease

	// ReleaseErr releases the flock and REPORTS the outcome instead of
	// discarding it. A non-nil error means the OS lock may still be held by
	// this process, and the caller MUST classify its observation as Unknown.
	// Like Release it is idempotent: a second call after a successful release
	// is a no-op returning nil.
	ReleaseErr() error
}

// SingleInstanceAcquireOptions enables the restart-v3 reservation check for a
// single acquisition. Existing callers pass no options and retain the legacy
// single-shot behavior without reading the marker.
type SingleInstanceAcquireOptions struct {
	RestartV3Enabled     bool
	MarkerStore          HandoffMarkerReader
	DesignatedChildNonce []byte
	Deadlines            RestartDeadlines
}

// GUIOwnerLeaseState is the tri-state owner-lease probe result.
type GUIOwnerLeaseState uint8

const (
	// GUIOwnerLeaseStateUnknown is the fail-closed zero value.
	GUIOwnerLeaseStateUnknown GUIOwnerLeaseState = iota
	GUIOwnerLeaseStateHeld
	GUIOwnerLeaseStateFree
)

// GUIOwnerLeaseLifecycleState is the monotonic acquisition/release state
// shared by the GUI-owner probe and its timeout-owning caller.
type GUIOwnerLeaseLifecycleState uint32

const (
	// GUIOwnerLeaseLifecycleOpen is the zero value: no flock acquisition has
	// been admitted.
	GUIOwnerLeaseLifecycleOpen GUIOwnerLeaseLifecycleState = iota
	// GUIOwnerLeaseLifecycleClosedBeforeExposure means the timeout owner
	// closed the gate before the probe could attempt the flock.
	GUIOwnerLeaseLifecycleClosedBeforeExposure
	// GUIOwnerLeaseLifecycleExposed means the probe won the gate immediately
	// before attempting the flock. Until a terminal outcome is published, the
	// current process may retain the lease.
	GUIOwnerLeaseLifecycleExposed
	// GUIOwnerLeaseLifecycleNotAcquired means the admitted flock attempt
	// returned busy or failed without acquiring a lease.
	GUIOwnerLeaseLifecycleNotAcquired
	// GUIOwnerLeaseLifecycleReleased means an acquired lease reported a
	// successful ReleaseErr.
	GUIOwnerLeaseLifecycleReleased
	// GUIOwnerLeaseLifecycleReleaseUnconfirmed means ReleaseErr failed, so
	// process exit is the remaining release boundary.
	GUIOwnerLeaseLifecycleReleaseUnconfirmed
)

// GUIOwnerLeaseDisposition is the tick-local relaunch capability derived from
// the lifecycle. It deliberately says only whether this process may still own
// the GUI flock; GUI liveness remains a separate classifier.
type GUIOwnerLeaseDisposition uint8

const (
	GUIOwnerLeaseNoRetainedLease GUIOwnerLeaseDisposition = iota
	GUIOwnerLeaseMayRetainLease
)

// GUIOwnerLeaseLifecycle atomically arbitrates the outer timeout against the
// probe's first flock attempt, then publishes exactly one terminal outcome.
type GUIOwnerLeaseLifecycle struct {
	state atomic.Uint32
}

// NewGUIOwnerLeaseLifecycle creates an Open lifecycle.
func NewGUIOwnerLeaseLifecycle() *GUIOwnerLeaseLifecycle {
	return &GUIOwnerLeaseLifecycle{}
}

// TryExpose admits one flock attempt. False means the timeout already closed
// the gate and the caller must not touch the flock.
func (l *GUIOwnerLeaseLifecycle) TryExpose() bool {
	return l != nil && l.state.CompareAndSwap(
		uint32(GUIOwnerLeaseLifecycleOpen),
		uint32(GUIOwnerLeaseLifecycleExposed),
	)
}

// CloseBeforeExposure prevents a not-yet-admitted probe from touching the
// flock after the outer deadline.
func (l *GUIOwnerLeaseLifecycle) CloseBeforeExposure() bool {
	return l != nil && l.state.CompareAndSwap(
		uint32(GUIOwnerLeaseLifecycleOpen),
		uint32(GUIOwnerLeaseLifecycleClosedBeforeExposure),
	)
}

// PublishNotAcquired closes an admitted attempt that returned without a lease.
func (l *GUIOwnerLeaseLifecycle) PublishNotAcquired() bool {
	return l != nil && l.state.CompareAndSwap(
		uint32(GUIOwnerLeaseLifecycleExposed),
		uint32(GUIOwnerLeaseLifecycleNotAcquired),
	)
}

// PublishRelease records the observed ReleaseErr outcome without replacing
// any earlier terminal evidence.
func (l *GUIOwnerLeaseLifecycle) PublishRelease(err error) bool {
	if l == nil {
		return false
	}
	next := GUIOwnerLeaseLifecycleReleased
	if err != nil {
		next = GUIOwnerLeaseLifecycleReleaseUnconfirmed
	}
	return l.state.CompareAndSwap(
		uint32(GUIOwnerLeaseLifecycleExposed),
		uint32(next),
	)
}

// Disposition fails closed for an exposed, release-unconfirmed, nil, or
// numerically-invalid lifecycle.
func (l *GUIOwnerLeaseLifecycle) Disposition() GUIOwnerLeaseDisposition {
	if l == nil {
		return GUIOwnerLeaseMayRetainLease
	}
	switch GUIOwnerLeaseLifecycleState(l.state.Load()) {
	case GUIOwnerLeaseLifecycleOpen,
		GUIOwnerLeaseLifecycleClosedBeforeExposure,
		GUIOwnerLeaseLifecycleNotAcquired,
		GUIOwnerLeaseLifecycleReleased:
		return GUIOwnerLeaseNoRetainedLease
	case GUIOwnerLeaseLifecycleExposed,
		GUIOwnerLeaseLifecycleReleaseUnconfirmed:
		return GUIOwnerLeaseMayRetainLease
	default:
		return GUIOwnerLeaseMayRetainLease
	}
}

// GUIOwnerLeaseProbeRequest carries the previously-read record plus the
// read-only store needed to revalidate it after tentatively acquiring a free
// flock. That re-read closes the marker-change window without introducing a
// probe-release-reacquire gap.
type GUIOwnerLeaseProbeRequest struct {
	PidportPath string
	Record      *HandoffMarkerRecord
	MarkerStore HandoffMarkerReader
	Deadlines   RestartDeadlines
	Lifecycle   *GUIOwnerLeaseLifecycle
}

// GUIOwnerLeaseProbeResult represents Held(reason),
// Free(owned_probe_lease), or Unknown(error). Lease is populated only for Free
// and remains held until the caller releases it.
//
// Lease is an OwnedSingleInstanceLease, not a bare SingleInstanceLease: the
// Free caller owns the flock and its release is load-bearing, so the seam must
// let it observe whether the release actually happened.
type GUIOwnerLeaseProbeResult struct {
	State  GUIOwnerLeaseState
	Reason error
	Lease  OwnedSingleInstanceLease
	Record *HandoffMarkerRecord
	// Lifecycle is the exact lifecycle supplied by the request.
	Lifecycle *GUIOwnerLeaseLifecycle
}

// GUIOwnerLeaseUnknownError preserves the concrete uncertainty while exposing
// the stable ErrGUIOwnerLeaseUnknown classifier.
type GUIOwnerLeaseUnknownError struct {
	Operation string
	Cause     error
}

func (e *GUIOwnerLeaseUnknownError) Error() string {
	if e == nil {
		return ErrGUIOwnerLeaseUnknown.Error()
	}
	return fmt.Sprintf("%s: %v: %v", ErrGUIOwnerLeaseUnknown, e.Operation, e.Cause)
}

func (e *GUIOwnerLeaseUnknownError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *GUIOwnerLeaseUnknownError) Is(target error) bool {
	if e == nil {
		return target == ErrGUIOwnerLeaseUnknown
	}
	return target == ErrGUIOwnerLeaseUnknown || errors.Is(e.Cause, target)
}

type singleInstanceFlock interface {
	TryLock() (bool, error)
	Unlock() error
	Close() error
}

const (
	singleInstanceUnlockAttempts   = 3
	singleInstanceUnlockRetryDelay = time.Millisecond
)

// SingleInstanceLock represents the acquired single-instance ownership.
// Release must be called on shutdown (or by an errdefer immediately after
// acquisition) to free the flock. The pidport file is intentionally NOT
// removed on Release — see Release() for the rationale.
type SingleInstanceLock struct {
	pidport string
	fl      singleInstanceFlock
}

// AcquireSingleInstance tries to become the sole mcphub gui process for
// this user. On success it writes a pidport record at PidportPath() and
// returns a lock the caller must Release on shutdown.
//
// The lock is a flock-managed adjacent .lock file — the same pattern
// workspace-registry uses elsewhere in the codebase. It is NOT a Windows
// named kernel mutex; portability across Linux/macOS was favored over
// the tiny-but-theoretical advantage of kernel-level serialization on
// Windows alone.
func AcquireSingleInstance(port int) (*SingleInstanceLock, error) {
	p, err := PidportPath()
	if err != nil {
		return nil, err
	}
	return acquireSingleInstanceAt(p, port)
}

// acquireSingleInstanceAt is the injectable form used by tests.
func acquireSingleInstanceAt(pidportPath string, port int) (*SingleInstanceLock, error) {
	lease, err := tryAcquireSingleInstanceLockAt(pidportPath)
	if err != nil {
		return nil, err
	}
	if err := api.WriteStateFileBytesLockHeld(pidportPath, []byte(formatPidport(os.Getpid(), port))); err != nil {
		lease.Release()
		return nil, fmt.Errorf("write pidport: %w", err)
	}
	return lease, nil
}

// tryAcquireSingleInstanceLockAt performs one non-blocking flock attempt. The
// underlying TryLock is deliberately retained instead of Lock: both legacy
// acquisition and restart-v3 probing remain bounded and never wait forever on
// another process.
func tryAcquireSingleInstanceLockAt(pidportPath string) (*SingleInstanceLock, error) {
	fl := flock.New(pidportPath + ".lock")
	ok, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("flock %s: %w", pidportPath+".lock", err)
	}
	if !ok {
		return nil, ErrSingleInstanceBusy
	}
	return &SingleInstanceLock{pidport: pidportPath, fl: fl}, nil
}

// ProbeSingleInstanceLockUnheld reports whether the GUI's own single-instance
// flock (pidportPath + ".lock") is currently held by ANY process — a signal
// that is INDEPENDENT of whatever CONTENT sits in the pidport file itself
// (missing, corrupt, or out of range does not matter: the flock is a
// kernel-enforced exclusivity primitive tied to an open file descriptor, not
// to the bytes on disk, and the OS releases it automatically when the
// holding process exits or the descriptor is closed).
//
// This mirrors api.SupervisorRunningUnderStateDir's exact idiom for the
// supervisor's own lock: a non-blocking TryLock that is released immediately
// on success (proving no holder existed) or refused (proving a live holder
// exists). Residual 1(b)'s bounded confirmation path
// (internal/cli/supervise_ensure_alive.go) uses this to establish GUI-owner
// death when pidport metadata itself cannot be trusted (VerdictMalformed /
// VerdictIndeterminate / an unresolvable pidport path).
//
// Returns:
//   - (true, nil)  — definitively unheld: no process currently owns the GUI
//     single-instance lock.
//   - (false, nil) — definitively held: a live process owns it.
//   - (false, err) — UNDETERMINABLE (the flock probe itself errored, e.g. a
//     locked-down filesystem, OR the probe's own tentative lease could not be
//     released — see below). Callers gating a destructive/unsafe decision
//     on this MUST treat err != nil the same as "held" (fail closed) — the
//     same contract SupervisorRunningUnderStateDir documents.
//
// RELEASING THE PROBE LEASE IS PART OF THE ANSWER, NOT CLEANUP (review
// finding). Acquiring the flock only proves nobody ELSE held it at that
// instant; returning (true, nil) additionally asserts that the lock is unheld
// NOW — which is false if this probe still owns it. release()'s bounded Unlock
// retries and its Close() fallback can both fail (gofrs/flock's Close()
// delegates to Unlock(), so a persistent syscall error leaves the descriptor
// open and the OS lock held until this process exits — see release()'s comment
// and work-items/backlog/2026-07-18-flock-persistent-unlock-residual.md). The
// old code discarded that error via Release() and still reported "definitively
// unheld", so residual 1(b)'s confirmation window
// (runEnsureAliveGUIOwnerUnknownEscalation) could consume its marker and launch
// a replacement GUI while THIS process still owned the single-instance lock —
// the replacement then loses acquisition and exits, while the tick reports a
// successful relaunch.
//
// Fail closed instead: any release failure is reported as UNDETERMINABLE. This
// matches every other release() call site in this file, all of which classify a
// non-nil release() as Unknown. Note release() deliberately reports the FIRST
// Unlock error even when a later retry succeeded (pinned by
// TestSingleInstanceLock_ReleaseRecoversTransientUnlockFailure), so a recovered
// transient also lands here — conservative in the safe direction: a missed
// recovery tick, never a relaunch against a lock this process still holds.
func ProbeSingleInstanceLockUnheld(pidportPath string) (unheld bool, err error) {
	return probeSingleInstanceLockUnheld(pidportPath, tryAcquireSingleInstanceLockAt)
}

// probeSingleInstanceLockUnheld is ProbeSingleInstanceLockUnheld with the
// acquire step passed in, so a test can supply a lease whose Unlock/Close fail
// persistently and exercise the fail-closed release branch without needing a
// real locked-down filesystem. Taking the seam as a PARAMETER (rather than a
// package-level var) keeps it off the production global state.
func probeSingleInstanceLockUnheld(
	pidportPath string,
	acquire func(string) (*SingleInstanceLock, error),
) (unheld bool, err error) {
	lease, acquireErr := acquire(pidportPath)
	if acquireErr != nil {
		if errors.Is(acquireErr, ErrSingleInstanceBusy) {
			return false, nil
		}
		return false, acquireErr
	}
	if releaseErr := lease.release(); releaseErr != nil {
		return false, fmt.Errorf("release single-instance probe lease %s: %w", pidportPath+".lock", releaseErr)
	}
	return true, nil
}

// Release releases ONLY the flock — it does NOT remove the pidport file.
// Idempotent.
//
// Removing the pidport on Release is unsafe: a racing successor that
// acquires the flock (between our Unlock and Remove) and writes its own
// pidport would have its file deleted. Round 7 (unlock-first) and round
// 8 (ownership PID check before Remove) both left a TOCTOU window
// between the read and the remove. The flock is the source of truth for
// ownership; the pidport file is metadata that the next acquirer
// overwrites atomically via os.WriteFile in acquireSingleInstanceAt.
//
// Stale-file harmless because:
//   - No flock holder + listener gone → TryActivateIncumbent probes the
//     port → connection-refused → "incumbent unreachable" error surfaces
//     correctly to the caller.
//   - Next acquirer overwrites the file before any second-instance
//     handshake can read it.
func (l *SingleInstanceLock) Release() {
	_ = l.release()
}

// ReleaseErr is Release with the outcome REPORTED rather than discarded, so a
// caller whose next step depends on the flock actually being free can fail
// closed instead of guessing (review finding 1).
//
// Release() is retained for the many call sites that release on a path where
// nothing downstream re-acquires this flock, and where a discarded error is
// therefore harmless. ReleaseErr is for the owned-probe callers — the ones this
// file's own invariant already binds: "any release failure is reported as
// UNDETERMINABLE", which every release() call site in this file upholds. Before
// this method existed, the cross-package Phase-I caller in internal/cli could
// not uphold it, because release() is unexported and Release() drops the error.
//
// A non-nil result does NOT mean the caller should retry — release() has
// already exhausted its bounded Unlock retries and gofrs/flock leaves nothing
// further to try (see the residual note in release()). It means "this process
// may still hold the lock until it exits": treat the observation as Unknown.
func (l *SingleInstanceLock) ReleaseErr() error {
	return l.release()
}

func (l *SingleInstanceLock) release() error {
	if l == nil || l.fl == nil {
		return nil
	}

	fl := l.fl
	var firstErr error
	for attempt := 0; attempt < singleInstanceUnlockAttempts; attempt++ {
		if err := fl.Unlock(); err == nil {
			l.fl = nil
			return firstErr
		} else if firstErr == nil {
			firstErr = err
		}
		if attempt+1 < singleInstanceUnlockAttempts {
			time.Sleep(singleInstanceUnlockRetryDelay)
		}
	}

	// The bounded retry above recovers a TRANSIENT Unlock failure. A PERSISTENT
	// failure is an accepted residual: gofrs/flock exposes no file-handle
	// accessor and its Close() delegates to Unlock() (flock.go:99), so on a real
	// persistent UnlockFileEx/flock syscall error Close() fails identically and
	// the descriptor is NOT closed — the OS lock stays held until process exit.
	// This is bounded and pre-existing (the legacy Release path has always had
	// it, so Phase E adds no new reachability): UnlockFileEx failing on a lock
	// this process legitimately holds is near-impossible, and every tentative-
	// lease caller (entrant, one-shot ensure-alive tick) is short-lived so
	// process exit frees the lock. We still clear l.fl unconditionally so a
	// discarded lease never double-frees and the caller classifies Unknown.
	// Definitive release (raw-handle lock or a gofrs/flock patch) is tracked:
	// work-items/backlog/2026-07-18-flock-persistent-unlock-residual.md.
	_ = fl.Close()
	l.fl = nil
	return firstErr
}

// ReadPidport reads "<PID> <PORT>\n" format. Returns (0,0,err) on parse
// failure or missing file. Second-instance callers use it to probe the
// incumbent.
func ReadPidport(path string) (pid, port int, err error) {
	b, err := api.ReadStateFileInodeAnchored(path)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed pidport %q", string(b))
	}
	pid, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse pid: %w", err)
	}
	port, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse port: %w", err)
	}
	return pid, port, nil
}

func formatPidport(pid, port int) string {
	return fmt.Sprintf("%d %d\n", pid, port)
}

// AcquireSingleInstanceAt is the exported form of acquireSingleInstanceAt so
// callers outside the gui package (cli) can share the same path.
//
// With no options, or with RestartV3Enabled false, this calls the legacy
// single-shot implementation directly and never reads the handoff marker.
// Phase F will pass one enabled option for a restart child; current CLI and
// force-kill callers intentionally remain unwired in Phase E.
func AcquireSingleInstanceAt(pidportPath string, port int, options ...SingleInstanceAcquireOptions) (*SingleInstanceLock, error) {
	if len(options) == 0 || !options[0].RestartV3Enabled {
		return acquireSingleInstanceAt(pidportPath, port)
	}
	if len(options) != 1 {
		return nil, newGUIOwnerLeaseUnknown("acquire options", fmt.Errorf("got %d option sets, want exactly one", len(options)))
	}
	return acquireReservationAwareSingleInstanceAt(pidportPath, port, options[0])
}

func acquireReservationAwareSingleInstanceAt(pidportPath string, port int, options SingleInstanceAcquireOptions) (*SingleInstanceLock, error) {
	if err := validateGUIOwnerLeasePath(pidportPath); err != nil {
		return nil, newGUIOwnerLeaseUnknown("validate pidport path", err)
	}

	lease, err := tryAcquireSingleInstanceLockAt(pidportPath)
	if err != nil {
		if errors.Is(err, ErrSingleInstanceBusy) {
			return nil, err
		}
		return nil, newGUIOwnerLeaseUnknown("acquire tentative lease", err)
	}

	record, err := readValidatedHandoffMarker(options.MarkerStore)
	if err != nil {
		return nil, releaseTentativeLeaseUnknown(lease, "read handoff marker", err)
	}
	now, err := restartDeadlineNow(options.Deadlines)
	if err != nil {
		return nil, releaseTentativeLeaseUnknown(lease, "read restart clock", err)
	}

	reservationOpen := reservationWindowOpen(record, now)
	if len(options.DesignatedChildNonce) > 0 {
		if !reservationOpen || !designatedChildMatches(record, options.DesignatedChildNonce) {
			return nil, releaseTentativeLeaseWithReason(lease, ErrHandoffReserved)
		}
	} else if reservationOpen {
		return nil, releaseTentativeLeaseWithReason(lease, ErrHandoffReserved)
	}

	if err := api.WriteStateFileBytesLockHeld(pidportPath, []byte(formatPidport(os.Getpid(), port))); err != nil {
		if releaseErr := lease.release(); releaseErr != nil {
			return nil, newGUIOwnerLeaseUnknown("release tentative lease after pidport write failure", errors.Join(err, releaseErr))
		}
		return nil, fmt.Errorf("write pidport: %w", err)
	}
	return lease, nil
}

// ProbeGUIOwnerLease returns a tri-state owner verdict. A Free result owns the
// flock and does not write pidport metadata; the caller must retain that exact
// lease through its guarded operation and release it on every exit path.
func ProbeGUIOwnerLease(ctx context.Context, request GUIOwnerLeaseProbeRequest) GUIOwnerLeaseProbeResult {
	lifecycle := request.Lifecycle
	withLifecycle := func(result GUIOwnerLeaseProbeResult) GUIOwnerLeaseProbeResult {
		result.Lifecycle = lifecycle
		return result
	}
	if lifecycle == nil {
		return withLifecycle(unknownGUIOwnerLease(nil, "probe lifecycle", errors.New("GUI owner lease lifecycle is nil")))
	}
	if ctx == nil {
		return withLifecycle(unknownGUIOwnerLease(nil, "probe context", errors.New("context is nil")))
	}
	if err := validateGUIOwnerLeasePath(request.PidportPath); err != nil {
		return withLifecycle(unknownGUIOwnerLease(request.Record, "validate pidport path", err))
	}
	if request.Record != nil {
		if err := validateHandoffMarker(request.Record); err != nil {
			return withLifecycle(unknownGUIOwnerLease(nil, "validate observed handoff marker", err))
		}
	}
	if err := ctx.Err(); err != nil {
		return withLifecycle(unknownGUIOwnerLease(request.Record, "probe cancelled before marker read", err))
	}
	observed, err := readValidatedHandoffMarker(request.MarkerStore)
	if err != nil {
		return withLifecycle(unknownGUIOwnerLease(request.Record, "read handoff marker", err))
	}
	if request.Record != nil && !sameHandoffMarkerObservation(request.Record, observed) {
		return withLifecycle(unknownGUIOwnerLease(observed, "validate observed handoff marker", errors.New("handoff marker changed before owner probe")))
	}
	now, err := restartDeadlineNow(request.Deadlines)
	if err != nil {
		return withLifecycle(unknownGUIOwnerLease(observed, "read restart clock", err))
	}
	// A raw reservation is authoritative throughout its protection window.
	// Returning Held before touching a momentarily-free OS flock prevents a
	// third entrant or ensure-alive from entering the healthy release gap.
	if reservationWindowOpen(observed, now) {
		return withLifecycle(heldGUIOwnerLease(observed, ErrHandoffReserved))
	}

	if !lifecycle.TryExpose() {
		return withLifecycle(unknownGUIOwnerLease(observed, "probe flock exposure", errors.New("GUI owner lease probe closed before flock exposure")))
	}
	lease, err := tryAcquireSingleInstanceLockAt(request.PidportPath)
	if err != nil {
		lifecycle.PublishNotAcquired()
		if errors.Is(err, ErrSingleInstanceBusy) {
			return withLifecycle(heldGUIOwnerLease(observed, ErrSingleInstanceBusy))
		}
		return withLifecycle(unknownGUIOwnerLease(observed, "probe flock", err))
	}
	if err := ctx.Err(); err != nil {
		return withLifecycle(unknownAfterTentativeLease(lifecycle, lease, observed, "probe cancelled after acquire", err))
	}

	current, err := readValidatedHandoffMarker(request.MarkerStore)
	if err != nil {
		return withLifecycle(unknownAfterTentativeLease(lifecycle, lease, observed, "revalidate handoff marker", err))
	}
	if !sameHandoffMarkerObservation(observed, current) {
		return withLifecycle(unknownAfterTentativeLease(lifecycle, lease, current, "revalidate handoff marker", errors.New("handoff marker changed during owner probe")))
	}
	now, err = restartDeadlineNow(request.Deadlines)
	if err != nil {
		return withLifecycle(unknownAfterTentativeLease(lifecycle, lease, current, "re-read restart clock", err))
	}
	if reservationWindowOpen(current, now) {
		releaseErr := lease.release()
		lifecycle.PublishRelease(releaseErr)
		if releaseErr != nil {
			return withLifecycle(unknownGUIOwnerLease(current, "release tentative lease for reservation", releaseErr))
		}
		return withLifecycle(heldGUIOwnerLease(current, ErrHandoffReserved))
	}
	if err := ctx.Err(); err != nil {
		return withLifecycle(unknownAfterTentativeLease(lifecycle, lease, current, "probe cancelled after marker read", err))
	}

	return GUIOwnerLeaseProbeResult{
		State:     GUIOwnerLeaseStateFree,
		Lease:     lease,
		Record:    current,
		Lifecycle: lifecycle,
	}
}

func readValidatedHandoffMarker(reader HandoffMarkerReader) (*HandoffMarkerRecord, error) {
	if reader == nil {
		return nil, errors.New("handoff marker reader is nil")
	}
	record, err := reader.Read()
	if err != nil {
		return nil, err
	}
	if record != nil {
		if err := validateHandoffMarker(record); err != nil {
			return nil, err
		}
	}
	return record, nil
}

func restartDeadlineNow(deadlines RestartDeadlines) (time.Time, error) {
	if deadlines.Now == nil {
		return time.Time{}, errors.New("restart clock is nil")
	}
	now := deadlines.Now().UTC()
	if now.IsZero() {
		return time.Time{}, errors.New("restart clock returned zero time")
	}
	return now, nil
}

func validateGUIOwnerLeasePath(pidportPath string) error {
	if strings.TrimSpace(pidportPath) == "" {
		return errors.New("pidport path is empty")
	}
	if !filepath.IsAbs(pidportPath) {
		return fmt.Errorf("pidport path is not absolute: %q", pidportPath)
	}
	return nil
}

func reservationWindowOpen(record *HandoffMarkerRecord, now time.Time) bool {
	return record != nil &&
		record.Phase == HandoffPhaseReserved &&
		!record.ReservationExpiresAt.IsZero() &&
		now.Before(record.ReservationExpiresAt)
}

func designatedChildMatches(record *HandoffMarkerRecord, nonce []byte) bool {
	if record == nil || len(nonce) == 0 || !isCanonicalDesignatedChildHash(record.DesignatedChildHash) {
		return false
	}
	got := []byte(hashDesignatedChildNonce(nonce))
	want := []byte(record.DesignatedChildHash)
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1
}

func isCanonicalDesignatedChildHash(value string) bool {
	const prefix = "sha256:"
	if len(value) != len(prefix)+(sha256.Size*2) || !strings.HasPrefix(value, prefix) {
		return false
	}
	for i := len(prefix); i < len(value); i++ {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}

func hashDesignatedChildNonce(nonce []byte) string {
	sum := sha256.Sum256(nonce)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sameHandoffMarkerObservation(observed, current *HandoffMarkerRecord) bool {
	if observed == nil || current == nil {
		return observed == nil && current == nil
	}
	return *observed == *current
}

func heldGUIOwnerLease(record *HandoffMarkerRecord, reason error) GUIOwnerLeaseProbeResult {
	return GUIOwnerLeaseProbeResult{
		State:  GUIOwnerLeaseStateHeld,
		Reason: reason,
		Record: record,
	}
}

func unknownGUIOwnerLease(record *HandoffMarkerRecord, operation string, cause error) GUIOwnerLeaseProbeResult {
	return GUIOwnerLeaseProbeResult{
		State:  GUIOwnerLeaseStateUnknown,
		Reason: newGUIOwnerLeaseUnknown(operation, cause),
		Record: record,
	}
}

func unknownAfterTentativeLease(lifecycle *GUIOwnerLeaseLifecycle, lease *SingleInstanceLock, record *HandoffMarkerRecord, operation string, cause error) GUIOwnerLeaseProbeResult {
	releaseErr := lease.release()
	lifecycle.PublishRelease(releaseErr)
	if releaseErr != nil {
		cause = errors.Join(cause, fmt.Errorf("release tentative lease: %w", releaseErr))
	}
	return unknownGUIOwnerLease(record, operation, cause)
}

func releaseTentativeLeaseUnknown(lease *SingleInstanceLock, operation string, cause error) error {
	if releaseErr := lease.release(); releaseErr != nil {
		cause = errors.Join(cause, fmt.Errorf("release tentative lease: %w", releaseErr))
	}
	return newGUIOwnerLeaseUnknown(operation, cause)
}

func releaseTentativeLeaseWithReason(lease *SingleInstanceLock, reason error) error {
	if releaseErr := lease.release(); releaseErr != nil {
		return newGUIOwnerLeaseUnknown("release rejected tentative lease", errors.Join(reason, releaseErr))
	}
	return reason
}

func newGUIOwnerLeaseUnknown(operation string, cause error) error {
	if cause == nil {
		cause = errors.New("unspecified uncertainty")
	}
	return &GUIOwnerLeaseUnknownError{Operation: operation, Cause: cause}
}

// RewritePidportPort overwrites the pidport file with the current PID and
// the supplied port. Used by the CLI after Server.Start resolves an
// OS-assigned port (--port 0): the lock was acquired before bind with
// the originally requested port, but second-instance handshake probes
// need the actual bound port. The caller must hold the single-instance
// lock — the flock on *.lock gates ownership, the pidport file is
// ownership metadata the lock holder freely updates.
func RewritePidportPort(pidportPath string, port int) error {
	return api.WriteStateFileBytesLockHeld(pidportPath, []byte(formatPidport(os.Getpid(), port)))
}

// WritePidport overwrites the pidport file with the supplied PID and
// port. Used by the CLI after Server.Start signals ready, called
// unconditionally so the takeover path (--force --kill) replaces the
// killed incumbent's PID + port with the current process's PID + bound
// port. Idempotent for the normal-acquire path (PID + port are already
// what AcquireSingleInstanceAt wrote, modulo --port 0 ephemeral
// resolution). The caller must hold the single-instance lock.
//
// Codex PR #23 P2 #2: replaces the previous conditional
// RewritePidportPort(actualPort) call which only fired when actualPort
// != requestedPort and only updated the port field — leaving the
// killed incumbent's PID stale in the pidport after a successful kill.
func WritePidport(pidportPath string, pid, port int) error {
	return api.WriteStateFileBytesLockHeld(pidportPath, []byte(formatPidport(pid, port)))
}

// VerdictClass enumerates the result of Probe / KillRecordedHolder.
type VerdictClass int

const (
	VerdictHealthy         VerdictClass = iota // incumbent ping matches recorded PID
	VerdictLiveUnreachable                     // recorded PID is alive but not serving HTTP
	VerdictDeadPID                             // recorded PID does not exist
	VerdictMalformed                           // pidport is missing/garbage/incomplete
	VerdictKilledRecovered                     // KillRecordedHolder succeeded; new flock acquired
	VerdictKillRefused                         // three-part identity gate failed
	VerdictKillFailed                          // SIGKILL/TerminateProcess returned error
	VerdictRaceLost                            // post-kill, a competitor won the new acquire
	// VerdictIndeterminate is appended LAST (not inserted among the existing
	// values) so its numeric JSON encoding never renumbers an already-shipped
	// class — Verdict.Class serializes to the GUI's /api/force-kill/probe
	// response. residual 1(a): the per-platform identity probe
	// (processIDImpl) returned a PLATFORM-LEVEL error that is NOT proof the
	// recorded PID is gone (a transient OpenProcess/kill(2) failure other
	// than the platform's OWN definitive "no such process" signal). Before
	// this class existed, probeOnce collapsed every such ambiguous error
	// into VerdictDeadPID (id.Alive's zero value), which AUTHORIZES the
	// headless-fleet relaunch on a merely-transient platform hiccup. Callers
	// MUST treat VerdictIndeterminate exactly like VerdictMalformed — never
	// as proof of death.
	VerdictIndeterminate
)

func (c VerdictClass) String() string {
	switch c {
	case VerdictHealthy:
		return "Healthy"
	case VerdictLiveUnreachable:
		return "LiveUnreachable"
	case VerdictDeadPID:
		return "DeadPID"
	case VerdictMalformed:
		return "Malformed"
	case VerdictKilledRecovered:
		return "KilledRecovered"
	case VerdictKillRefused:
		return "KillRefused"
	case VerdictKillFailed:
		return "KillFailed"
	case VerdictRaceLost:
		return "RaceLost"
	case VerdictIndeterminate:
		return "Indeterminate"
	}
	return fmt.Sprintf("VerdictClass(%d)", int(c))
}

// Verdict bundles the Probe result. JSON marshaling skips Diagnose
// and Hint (Codex r6 #4): A4-b's POST /api/force-kill returns the
// raw structured fields and the UI formats locally.
//
// pidCmdlineRaw and macOSUnsupported are unexported and therefore
// invisible to encoding/json. They carry signals that must NOT
// reach JSON or the diagnostic block: pidCmdlineRaw is the full,
// untruncated argv the identity-gate (cmdlineIsGui) reads; the
// public PIDCmdline is the truncated display/JSON copy. Truncating
// argv before the gate would drop argv[1] when argv[0] (the
// binary path) exceeds 1KB and let a non-GUI mcphub subcommand
// pass the gate's len(argv)==1 branch (Codex iter-3 P2 #1).
//
// macOSUnsupported, when true, marks a Verdict produced on a
// platform where processIDImpl returned errMacOSProbeUnsupported
// (currently darwin). KillRecordedHolder reads it to short-circuit
// to a macOS-specific KillRefused message instead of cascading
// through the image/argv/start-time gates with empty fields and
// emitting "image ” is not an mcphub binary" (Codex iter-3 P2 #2).
type Verdict struct {
	Class    VerdictClass `json:"class"`
	PID      int          `json:"pid"`
	Port     int          `json:"port"`
	Mtime    time.Time    `json:"mtime"`
	PIDAlive bool         `json:"pid_alive"`
	PIDImage string       `json:"pid_image"`
	// PIDCmdline is the truncated argv kept for in-process consumers.
	// It is NOT serialized to JSON (Codex iter-13 P2): the future
	// POST /api/force-kill API/UI must not echo full argv because
	// commands like `mcphub secrets set --value <SECRET>` carry
	// secrets there. PIDSubcommand below carries the gate-relevant
	// token (argv[1]) which is what the operator actually needs to
	// see — "the recorded PID is `daemon`, not `gui`".
	PIDCmdline []string `json:"-"`
	// PIDSubcommand is argv[1] (or "" when len(argv) < 2). Safe to
	// expose because the identity gate's argv check is exactly
	// "argv[1] == 'gui' OR len(argv) == 1"; argv[1] alone is enough
	// to explain a refusal without leaking the rest of the command
	// line.
	PIDSubcommand string    `json:"pid_subcommand"`
	PIDStart      time.Time `json:"pid_start"`
	PingMatch     bool      `json:"ping_match"`
	Diagnose      string    `json:"-"`
	Hint          string    `json:"-"`

	// pidCmdlineRaw is the untruncated argv used by the identity
	// gate. Unexported so encoding/json never serializes it; the
	// truncated PIDCmdline above is the only argv that reaches
	// display, JSON, or the diagnostic block.
	pidCmdlineRaw []string

	// macOSUnsupported flags Verdicts produced when processIDImpl
	// returned errMacOSProbeUnsupported. KillRecordedHolder uses
	// this to refuse the kill with a macOS-specific message.
	macOSUnsupported bool

	// archUnsupported flags Verdicts produced when processIDImpl
	// returned errWindowsArchUnsupported (windows && !amd64 builds
	// where PEB offsets don't apply). probeOnce routes such verdicts
	// to VerdictLiveUnreachable instead of VerdictDeadPID, and
	// checkIdentityGateInternal refuses the kill with an
	// arch-specific reason. Codex bot review on PR #23 P2.
	archUnsupported bool
}

// IdentityProbeUnsupported reports whether this Verdict was produced on a
// platform where the OS identity probe (processIDImpl) could not run AT ALL —
// darwin (errMacOSProbeUnsupported) or a Windows non-amd64 build
// (errWindowsArchUnsupported, PEB offsets are 64-bit-only). It is the exported
// read of the two unexported flags above, so a consumer OUTSIDE this package
// can tell an OBSERVED liveness fact apart from an unobservable one without
// those flags (or their JSON-invisibility) leaking.
//
// Why a consumer needs it: probeOnce routes an unsupported-probe Verdict to
// VerdictLiveUnreachable, which on every supported platform means the strong
// fact "the recorded PID IS alive, it just is not answering /api/ping". On an
// unsupported platform it means only "we could not look". The Class alone
// cannot distinguish the two, so a caller that treats VerdictLiveUnreachable
// as proof of liveness draws a confident conclusion from a probe that never
// ran (round 4 review finding on probeGUIOwnerAlive in
// internal/cli/supervise_ensure_alive.go).
//
// NOTE the flags are stamped BEFORE probeOnce's pingMatched early return, so a
// VerdictHealthy can also report true here. That combination is NOT ambiguous:
// a ping whose reply carries the recorded PID is an independent, positive
// liveness proof that needs no identity probe. Consumers must therefore branch
// on this only for classes whose liveness claim actually rests on the identity
// probe (VerdictLiveUnreachable), never as a blanket "distrust this verdict".
func (v Verdict) IdentityProbeUnsupported() bool {
	return v.macOSUnsupported || v.archUnsupported
}

// NewIdentityProbeUnsupportedVerdictForTest builds a Verdict that reports
// IdentityProbeUnsupported() == true, so a test in ANOTHER package can
// exercise its consumers' branch logic without running on darwin or a
// Windows non-amd64 host (this repo's CI and dev host are windows/amd64,
// where processIDImpl always succeeds and the flags are unreachable).
// Mirrors NewPendingSupervisorEventEmitForTest's exported-test-constructor
// pattern in internal/api. Only tests may call it.
func NewIdentityProbeUnsupportedVerdictForTest(class VerdictClass, pid, port int) Verdict {
	return Verdict{Class: class, PID: pid, Port: port, archUnsupported: true}
}

// KillOpts controls KillRecordedHolder behavior.
type KillOpts struct {
	// PingTimeout is how long pingIncumbent waits before declaring
	// "unreachable". Default 500ms when zero.
	PingTimeout time.Duration
	// AcquireDeadline is the maximum total time KillRecordedHolder
	// waits for TryLock to succeed after sending the kill signal.
	// Default 2s when zero.
	AcquireDeadline time.Duration
	// AcquireBackoff is the inter-attempt delay during the
	// post-kill TryLock poll. Default 50ms when zero.
	AcquireBackoff time.Duration
	// KillExitDeadline is the maximum total time KillRecordedHolder
	// waits between killProcess and the acquire-poll for the kernel
	// to register the kill via processID(pid).Alive==false. Default
	// 5s when zero.
	//
	// Per memo §"Take-over protocol" step 5f: TerminateProcess on
	// Windows is asynchronous, and Unix kernel cleanup (zombie
	// reaping, fd close, flock release) is not instant. Without an
	// explicit wait the AcquireDeadline (default 2s) can elapse
	// before the kernel releases the flock, producing a spurious
	// VerdictRaceLost. Codex iter-9 P2 #2.
	KillExitDeadline time.Duration
	// KillExitBackoff is the inter-poll delay during the kill-exit
	// wait. Default 50ms when zero.
	KillExitBackoff time.Duration
	// Expected, when populated (non-zero PID), is the identity the
	// caller already showed to the user (e.g. via runForceKill's
	// confirmation prompt). KillRecordedHolder's internal re-probe
	// must observe an identical (PID, Port, Mtime) tuple before any
	// kill happens; otherwise classification flips to
	// VerdictRaceLost and no signal is sent. Closes the TOCTOU
	// window between the cli's first Probe and the internal probe
	// where a competitor could rewrite the pidport with a different
	// PID and trick the gate into killing the wrong process.
	// Codex iter-5 P1.
	Expected ExpectedIdentity
}

// ExpectedIdentity carries the (PID, Port, Mtime) tuple the caller
// already validated against before invoking KillRecordedHolder.
// A zero PID disables the check (back-compat for callers that do not
// pre-Probe). Codex iter-5 P1.
type ExpectedIdentity struct {
	PID   int
	Port  int
	Mtime time.Time
}

// IsZero reports whether the ExpectedIdentity carries no expectation.
// PID == 0 is the canonical "unset" sentinel — any pidport with a
// recorded PID of 0 is malformed and would already be rejected by
// probe/ReadPidport before reaching here.
func (e ExpectedIdentity) IsZero() bool { return e.PID == 0 }

// Probe inspects the pidport file and classifies the incumbent's
// state without taking any destructive action. Used by bare
// `mcphub gui --force` to build the diagnostic block.
//
// Class progression:
//   - Pidport unreadable / unparseable → VerdictMalformed.
//   - processID(pid).Alive == false   → VerdictDeadPID.
//   - PID alive AND ping matches      → VerdictHealthy.
//   - PID alive AND ping fails/wrong  → VerdictLiveUnreachable.
//
// Three-part identity gate is NOT run here — it's specific to
// KillRecordedHolder. Probe is read-only and provides display data.
func Probe(ctx context.Context, pidportPath string) Verdict {
	return probe(ctx, pidportPath, 500*time.Millisecond)
}

// probeStartupRetries / probeStartupBackoff bound the retry loop that
// covers the AcquireSingleInstanceAt → ReadyHook startup window
// (Codex PR #23 P2 #1, widened in iter-2). Total retry budget is
// bounded at 5 × 100ms = 500ms (plus per-attempt pingTimeout on the
// final successful read, which short-circuits via Healthy).
//
// probeStartupWindow is the mtime threshold separating "incumbent
// just wrote pidport, listener may still be binding" from "real stuck
// incumbent". 5s is intentionally generous: it is far better to add
// ~500ms latency to a 5-seconds-old "real stuck" case than to kill a
// healthy-but-slow startup. Real stuck incumbents will always have
// pidport mtime well past 5s old, so they skip the retry and are
// classified LiveUnreachable on the first probe.
const (
	probeStartupRetries = 5
	probeStartupBackoff = 100 * time.Millisecond
	probeStartupWindow  = 5 * time.Second
)

// probe is the internal implementation shared by Probe and
// KillRecordedHolder. pingTimeout controls how long pingIncumbent
// waits before declaring the incumbent unreachable.
//
// Startup-window retry (Codex PR #23 P2 #1, widened in iter-2):
// when classification would otherwise be VerdictLiveUnreachable AND
// the recorded PID is alive AND the pidport mtime is recent
// (within probeStartupWindow), the function retries up to
// probeStartupRetries times spaced by probeStartupBackoff, re-reading
// the pidport on each iteration. This closes the kill-vulnerable
// window between AcquireSingleInstanceAt (which writes pidport with
// {pid, requestedPort} immediately) and Server.Start binding
// 127.0.0.1:requestedPort (which signals ready and triggers the
// final pidport rewrite): a holder finishing its bind during the
// retry loop flips the verdict from LiveUnreachable to Healthy.
//
// The mtime gate (instead of the iter-1 `port==0` gate) is the right
// signal because the same race exists for explicit `--port=N`:
// AcquireSingleInstanceAt writes pidport with `{pid, N}` before
// Server.Start binds. The iter-1 gate missed that case entirely.
// Real stuck incumbents have pidport mtimes far older than the
// startup window, so they still skip the retry and classify
// LiveUnreachable immediately.
func probe(ctx context.Context, pidportPath string, pingTimeout time.Duration) Verdict {
	v := probeOnce(ctx, pidportPath, pingTimeout)
	if !shouldRetryProbe(v) {
		return v
	}
	// Retry loop: re-read pidport on each iteration in case the
	// holder finishes its bind. The mtime gate and PIDAlive gate
	// keep this loop bounded — once mtime ages past the startup
	// window, or the PID dies, retries stop.
	for i := 0; i < probeStartupRetries; i++ {
		select {
		case <-ctx.Done():
			return v
		case <-time.After(probeStartupBackoff):
		}
		retry := probeOnce(ctx, pidportPath, pingTimeout)
		// Any verdict that no longer meets the retry conditions is
		// final — return it. This includes:
		//   - Healthy (holder finished bind + ping matches)
		//   - LiveUnreachable with old mtime (real stuck instance)
		//   - DeadPID (holder exited mid-startup)
		//   - Malformed (pidport corrupted under us)
		if !shouldRetryProbe(retry) {
			return retry
		}
		// Still in the startup window — keep the latest verdict
		// (its mtime/PIDStart are the freshest) and try again.
		v = retry
	}
	return v
}

// shouldRetryProbe reports whether a Verdict represents a transient
// startup-race state worth retrying. Returns true iff:
//
//  1. Class == VerdictLiveUnreachable (alive PID, ping fails);
//  2. PIDAlive == true (defensive — Class implies it, but pin it);
//  3. Pidport mtime is non-zero and within probeStartupWindow.
//
// The mtime gate replaces the iter-1 (port==0) gate, which only
// covered the --port=0 startup race and missed the analogous
// --port=N startup race entirely. (Codex PR #23 P2 #1 iter-2.)
func shouldRetryProbe(v Verdict) bool {
	if v.Class != VerdictLiveUnreachable {
		return false
	}
	if !v.PIDAlive {
		return false
	}
	if v.Mtime.IsZero() {
		return false
	}
	return time.Since(v.Mtime) < probeStartupWindow
}

// probeOnce runs a single classification pass without retry. Split
// out from probe so the retry loop above can call it cleanly.
//
// Ordering invariant (Codex iter-3 P2 #2): ping runs FIRST, before
// processID. Healthy classification depends on ping match alone —
// processID telemetry is needed for the LiveUnreachable vs DeadPID
// distinction and for the destructive identity gate, but Healthy
// is a ping-only verdict. This lets bare `mcphub gui --force` on
// macOS detect a healthy incumbent and route to handshake, even
// though processIDImpl returns errMacOSProbeUnsupported. Pre-fix
// code returned VerdictMalformed early on macOS and never reached
// the ping branch, breaking activate-window for healthy incumbents.
//
// Truncation invariant (Codex iter-3 P2 #1): id.Cmdline is the
// raw, untruncated argv. We store it on Verdict.pidCmdlineRaw
// (unexported) for the identity gate, and truncate only when
// populating the display field Verdict.PIDCmdline. Truncating
// before the gate would let a non-GUI mcphub subcommand whose
// argv[0] exceeds 1KB pass cmdlineIsGui's len(argv)==1 branch.
func probeOnce(ctx context.Context, pidportPath string, pingTimeout time.Duration) Verdict {
	v := Verdict{}
	pid, port, err := ReadPidport(pidportPath)
	if err != nil || pid <= 0 {
		v.Class = VerdictMalformed
		v.Diagnose = fmt.Sprintf("pidport unreadable or empty: %v", err)
		// Codex iter-7 P2 #2: do NOT tell operators to delete the
		// pidport directory contents — that directory contains
		// gui.pidport.lock, and removing it under a live flock
		// holder splits ownership (the exact scenario the runbook
		// warns against). Reboot is the only safe universal recovery
		// when the pidport itself is corrupt; the lock file is left
		// to the OS to release when the holder exits.
		v.Hint = "Reboot to clear the lock; do NOT delete gui.pidport.lock under a live holder (see CLAUDE.md §Stuck-instance recovery)."
		return v
	}
	// Codex iter-8 P2 #1: out-of-range port is also a Malformed
	// verdict. A corrupt port (e.g., -1, 70000) parses fine through
	// ReadPidport but causes ping to fail unconditionally; without
	// this guard, --force --kill could classify the holder as
	// LiveUnreachable and kill an otherwise-healthy GUI whose only
	// flaw is bad metadata. TCP ports are 0..65535; 0 is the
	// well-known "auto-assign placeholder" that AcquireSingleInstance
	// writes before Server.Start binds, so it's not an error.
	if port < 0 || port > 65535 {
		v.Class = VerdictMalformed
		v.PID = pid
		v.Port = port
		v.Diagnose = fmt.Sprintf("pidport port %d out of range (0..65535)", port)
		v.Hint = "Reboot to clear the lock; do NOT delete gui.pidport.lock under a live holder (see CLAUDE.md §Stuck-instance recovery)."
		return v
	}
	v.PID = pid
	v.Port = port
	if st, statErr := os.Stat(pidportPath); statErr == nil {
		v.Mtime = st.ModTime()
	}

	// Ping first. A successful ping that matches the recorded PID
	// is a complete Healthy verdict regardless of whether processID
	// is supported on this platform.
	matched, perr := pingIncumbent(ctx, port, pingTimeout)
	pingMatched := perr == nil && matched == pid

	id, idErr := processID(pid)
	v.PIDAlive = id.Alive
	v.PIDImage = id.ImagePath
	v.pidCmdlineRaw = id.Cmdline
	v.PIDCmdline = truncateCmdline(id.Cmdline, 1024)
	// Codex iter-13 P2: populate PIDSubcommand (the safe-to-serialize
	// gate token) so JSON consumers can distinguish gui/daemon/etc
	// without seeing the full argv. Empty string when argv has fewer
	// than 2 elements (which the gate treats as the no-arg Explorer
	// auto-gui case anyway).
	if len(id.Cmdline) >= 2 {
		v.PIDSubcommand = id.Cmdline[1]
	}
	v.PIDStart = id.StartTime
	v.macOSUnsupported = errors.Is(idErr, errMacOSProbeUnsupported)
	v.archUnsupported = errors.Is(idErr, errWindowsArchUnsupported)

	if pingMatched {
		v.Class = VerdictHealthy
		v.PingMatch = true
		v.Diagnose = fmt.Sprintf("incumbent PID %d is healthy on port %d", pid, port)
		v.Hint = ""
		return v
	}

	// Codex PR #23 P2 #3 (iter-2, refined iter-3): on platforms
	// where the identity probe is unimplemented (currently macOS —
	// see probe_darwin.go), processIDImpl returns ProcessIdentity{}
	// + a sentinel error. Without a healthy ping we have no useful
	// liveness signal, so classify as VerdictLiveUnreachable with
	// macOS-specific diagnose/hint. KillRecordedHolder reads
	// macOSUnsupported and refuses with a clear message instead of
	// cascading through identity gates that read empty fields.
	if v.macOSUnsupported {
		v.Class = VerdictLiveUnreachable
		v.PIDAlive = false
		if perr != nil {
			v.Diagnose = fmt.Sprintf("recorded PID %d: macOS identity probe not supported and /api/ping on %d failed: %v", pid, port, perr)
		} else {
			v.Diagnose = fmt.Sprintf("recorded PID %d: macOS identity probe not supported and /api/ping on %d returned PID %d", pid, port, matched)
		}
		v.Hint = "macOS: identity probe not supported; --force --kill is blocked. Reboot is the recovery path."
		return v
	}

	// Same shape on Windows non-amd64 builds. processIDImpl returns
	// errWindowsArchUnsupported because PEB offsets are 64-bit-only,
	// and reading them on a 32-bit/arm64 layout would mis-classify a
	// legitimate stuck holder as PID-recycled. Without arch-aware
	// short-circuit, the !id.Alive cascade below would emit
	// VerdictDeadPID — implying "the holder exited" when in reality
	// we just couldn't observe it. Codex bot review on PR #23 P2.
	if v.archUnsupported {
		v.Class = VerdictLiveUnreachable
		v.PIDAlive = false
		if perr != nil {
			v.Diagnose = fmt.Sprintf("recorded PID %d: Windows non-amd64 build cannot enumerate cmdline/start-time and /api/ping on %d failed: %v", pid, port, perr)
		} else {
			v.Diagnose = fmt.Sprintf("recorded PID %d: Windows non-amd64 build cannot enumerate cmdline/start-time and /api/ping on %d returned PID %d", pid, port, matched)
		}
		v.Hint = "This Windows build (non-amd64) lacks PEB-offset support; --force --kill is blocked. Use OS tools (Task Manager) to identify and end the stuck process, or rebuild for amd64."
		return v
	}

	// Residual 1(a) fix: id.Indeterminate marks a platform-level identity
	// probe error that is NOT the platform's own definitive "no such
	// process" signal (see probe_windows.go's classifyOpenProcessError /
	// probe_linux.go's classifyKillError — only ERROR_INVALID_PARAMETER /
	// ESRCH may claim id.Alive=false; every other OpenProcess/kill(2) error
	// sets Indeterminate instead). Checked BEFORE the !id.Alive cascade so a
	// transient platform hiccup can never fall through to VerdictDeadPID,
	// which is the ONLY class that authorizes a destructive relaunch/kill
	// downstream (probeGUIOwnerAlive, KillRecordedHolder's skip-list below).
	if id.Indeterminate {
		v.Class = VerdictIndeterminate
		v.PIDAlive = false
		if idErr != nil {
			v.Diagnose = fmt.Sprintf("recorded PID %d: liveness probe returned an ambiguous platform error (%v); this is NOT proof the process is dead", pid, idErr)
		} else {
			v.Diagnose = fmt.Sprintf("recorded PID %d: liveness probe returned an ambiguous result; this is NOT proof the process is dead", pid)
		}
		v.Hint = "The identity probe could not determine whether the previous incumbent is alive or dead (a transient platform error, not a confirmed exit). Retry, or use OS tools (Task Manager/ps) to check the PID directly."
		return v
	}

	if !id.Alive {
		v.Class = VerdictDeadPID
		v.Diagnose = fmt.Sprintf("recorded PID %d is not alive", pid)
		v.Hint = "The previous incumbent process has exited. The lock should release on its own; if not, reboot."
		return v
	}

	v.Class = VerdictLiveUnreachable
	if perr != nil {
		v.Diagnose = fmt.Sprintf("recorded PID %d alive but /api/ping on %d failed: %v", pid, port, perr)
	} else {
		v.Diagnose = fmt.Sprintf("recorded PID %d alive but /api/ping on %d returned PID %d", pid, port, matched)
	}
	v.Hint = "Kill the recorded PID manually OR rerun with --force --kill (subject to identity gate)."
	return v
}

// KillRecordedHolder is the destructive opt-in path for
// `mcphub gui --force --kill`. Runs the healthy-incumbent early-exit,
// then the three-part identity gate, then SIGKILL/TerminateProcess
// on the recorded PID, then a TryLock poll loop until acquired or
// AcquireDeadline expires.
//
// Returns (lock, verdict, err). On VerdictKilledRecovered, lock is
// the freshly-acquired SingleInstanceLock the caller must Release.
// On all other Verdicts lock is nil.
//
// Three-part identity gate (memo §"Why automation is opt-in"):
//
//  1. matchBasename(image) — "mcphub.exe" Windows / "mcphub" POSIX.
//  2. argv subcommand: argv[1] == "gui" OR len(argv) == 1.
//     The len(argv)==1 branch covers cmd/mcphub/main.go, which
//     internally appends "gui" to os.Args on a no-arg launch —
//     an Explorer/Start-menu double-click OR a bare `mcphub` typed
//     at a terminal; externally the command line is just the
//     executable path in both cases.
//  3. process start time ≤ pidport mtime + 1s tolerance.
//
// Codex r4 #7: never os.Remove the lock file. The OS releases the
// flock as a side effect of process termination.
func KillRecordedHolder(ctx context.Context, pidportPath string, opts KillOpts) (*SingleInstanceLock, Verdict, error) {
	if opts.PingTimeout == 0 {
		opts.PingTimeout = 500 * time.Millisecond
	}
	if opts.AcquireDeadline == 0 {
		opts.AcquireDeadline = 2 * time.Second
	}
	if opts.AcquireBackoff == 0 {
		opts.AcquireBackoff = 50 * time.Millisecond
	}
	if opts.KillExitDeadline == 0 {
		opts.KillExitDeadline = 5 * time.Second
	}
	if opts.KillExitBackoff == 0 {
		opts.KillExitBackoff = 50 * time.Millisecond
	}

	v := probe(ctx, pidportPath, opts.PingTimeout)
	switch v.Class {
	case VerdictMalformed, VerdictDeadPID, VerdictIndeterminate:
		// VerdictIndeterminate (residual 1(a)) MUST skip the kill exactly
		// like Malformed/DeadPID: an ambiguous platform-level identity
		// error is not proof of anything, and this switch has NO default
		// arm — any class that falls through here proceeds straight into
		// the destructive identity gate below.
		return nil, v, fmt.Errorf("kill skipped: %s", v.Class)
	case VerdictHealthy:
		// Codex r5 #7b: incumbent is healthy — do NOT kill. Caller
		// routes to handshake instead. Verdict is returned as-is so
		// the cli layer can print "incumbent is healthy; activating
		// instead of killing" before TryActivateIncumbent.
		return nil, v, nil
	}

	// Codex iter-5 P1: TOCTOU guard between the caller's confirmed
	// identity (the PID it showed to the user, or the cli's first
	// Probe in --yes mode) and the identity our internal re-probe
	// just observed. A competitor that rewrote the pidport between
	// the two probes would flip PID/Port/Mtime; if we proceed here
	// we may signal a different process than the user confirmed.
	// Mismatch → VerdictRaceLost; no kill attempted. The check is
	// gated on Expected.PID != 0 so callers that don't pre-Probe
	// (no production callers today, but the seam is preserved for
	// older tests) keep their original behavior.
	if !opts.Expected.IsZero() {
		if v.PID != opts.Expected.PID || v.Port != opts.Expected.Port || !v.Mtime.Equal(opts.Expected.Mtime) {
			confirmed := opts.Expected
			v.Class = VerdictRaceLost
			v.Diagnose = fmt.Sprintf(
				"pidport changed between user confirmation and kill: confirmed PID %d port %d mtime %s, found PID %d port %d mtime %s",
				confirmed.PID, confirmed.Port, confirmed.Mtime.UTC().Format(time.RFC3339Nano),
				v.PID, v.Port, v.Mtime.UTC().Format(time.RFC3339Nano),
			)
			v.Hint = "Rerun mcphub gui without --force to handshake with the new incumbent."
			return nil, v, fmt.Errorf("pidport changed mid-prompt")
		}
	}

	// Codex iter-9 P2 #1: defense in depth — even though the cli
	// runs CheckIdentityGate before the prompt, KillRecordedHolder
	// re-runs the identical gate on its own re-probe so a future
	// caller (HTTP API in A4-b, ad-hoc Go consumer) cannot bypass
	// the identity protections by skipping the cli's pre-prompt
	// check. Both call sites share checkIdentityGateInternal so the
	// gate logic is not duplicated.
	if refused, diagnose, hint, errReason := checkIdentityGateInternal(v); refused {
		v.Class = VerdictKillRefused
		v.Diagnose = diagnose
		v.Hint = hint
		return nil, v, fmt.Errorf("kill refused: %s", errReason)
	}

	// All three gates passed. Kill.
	//
	// Codex iter-11 P1: check ctx.Done() in the smallest possible
	// window before the destructive call. signal.NotifyContext
	// marks ctx.Done() when SIGINT/SIGTERM arrives but does NOT
	// preempt the running goroutine; without this guard, an
	// operator who Ctrl+C's between the probe and this point still
	// sees the kill go through. The check is best-effort (a SIGINT
	// arriving between the check and killProcess still wins the
	// race), but closes the obvious window. Apply BEFORE the seam
	// override so production behavior matches test behavior.
	if err := ctx.Err(); err != nil {
		v.Class = VerdictKillFailed
		v.Diagnose = fmt.Sprintf("kill cancelled before SIGKILL: %v", err)
		v.Hint = "Operator cancelled (Ctrl+C/SIGTERM) before the destructive step; no kill was attempted."
		return nil, v, err
	}

	// Final pre-kill identity recheck — runs unconditionally, NOT
	// gated on opts.Expected. Closes the window between the gate-pass
	// at line ~645 and the destructive syscall: a PID could be reused
	// in that interval (process exited and the OS reissued the same
	// PID to a new launch). Codex finding fix: PR #81 gated this on
	// opts.Expected, which the GUI HTTP path passes empty, so the
	// production force-kill skipped the recheck entirely. We rebuild
	// a fresh Verdict from the live identity probe and re-run the
	// shared three-part gate (image basename + argv[1]==gui + start
	// time ≤ pidport mtime) so future code paths cannot weaken the
	// production gate by skipping opts.Expected.
	//
	// holderGoneAtRecheck records that the recheck observed a genuinely
	// dead PID (idErr==nil && !Alive). pr301 r7 Finding 2 (P2 #717): the
	// gone case must skip BOTH the gate re-run AND the kill — see the
	// guarded block below.
	holderGoneAtRecheck := false
	if !v.macOSUnsupported && !v.archUnsupported {
		idNow, idErr := processID(v.PID)
		if idErr != nil && !errors.Is(idErr, errMacOSProbeUnsupported) && !errors.Is(idErr, errWindowsArchUnsupported) {
			// Fail closed: identity telemetry unavailable in the
			// final window means we cannot prove the recorded PID
			// still belongs to this mcphub gui instance.
			v.Class = VerdictKillRefused
			v.Diagnose = fmt.Sprintf("pre-kill identity probe failed for PID %d: %v", v.PID, idErr)
			v.Hint = "Identity could not be reverified before kill; rerun mcphub gui --force --kill to retry, or use the manual recovery path."
			return nil, v, fmt.Errorf("kill refused: pre-kill identity probe failed")
		}
		if idErr == nil {
			// pr301 r6 (bot Finding, second gate): an already-GONE
			// holder must reach the recovery path, NOT be refused on
			// empty identity. processID of a dead PID returns
			// Alive=false with empty ImagePath/Cmdline/StartTime (and
			// idErr==nil) on both Windows and Linux. If we re-ran the
			// shared gate on that empty Verdict, matchBasename("")
			// would trip the image arm → VerdictKillRefused, stranding
			// the operator on a stuck lock whose holder is already
			// dead. But a dead holder's exit is exactly what releases
			// the flock, so the kill the recheck guards is moot.
			//
			// This keys strictly on !idNow.Alive, NOT on empty
			// identity: a LIVE-but-unverifiable holder (Denied=true on
			// EPERM/ACCESS_DENIED) reports Alive=true with empty
			// ImagePath, so it STILL falls through to the gate re-run
			// below and STILL refuses on the image arm (fail closed —
			// SEC-F3 preserved). Only a genuinely-dead PID skips here.
			if idNow.Alive {
				// Build a fresh Verdict carrying just the identity
				// fields the gate consults, then re-run the shared
				// gate so any future widening of the gate logic
				// applies here automatically. A live holder whose
				// identity does NOT match (image/argv/start-time/owner
				// mismatch) must still be refused.
				vNow := Verdict{
					PID:           v.PID,
					PIDAlive:      idNow.Alive,
					PIDImage:      idNow.ImagePath,
					pidCmdlineRaw: idNow.Cmdline,
					PIDStart:      idNow.StartTime,
					Mtime:         v.Mtime,
				}
				if len(idNow.Cmdline) >= 2 {
					vNow.PIDSubcommand = idNow.Cmdline[1]
				}
				if refused, diagnose, hint, errReason := checkIdentityGateInternal(vNow); refused {
					v.Class = VerdictKillRefused
					v.Diagnose = "pre-kill recheck: " + diagnose
					v.Hint = hint
					return nil, v, fmt.Errorf("kill refused: pre-kill identity recheck: %s", errReason)
				}
			} else {
				// pr301 r7 Finding 2 (P2 #717): the holder is genuinely
				// gone. The r6 fix skipped only the gate re-run and then
				// FELL THROUGH to kill(v.PID) below — but there is no
				// validated target to kill, and a PID REUSED between this
				// recheck and the kill (or a transient probe that
				// classified the holder not-Alive) could be terminated
				// WITHOUT the image/argv/start-time/owner gate we just
				// skipped. Mark the holder gone so the kill+wait block is
				// bypassed entirely and we go STRAIGHT to the acquire-poll
				// recovery path: the holder's exit is exactly what releases
				// the flock, so there is nothing to kill — only a lock to
				// reacquire.
				holderGoneAtRecheck = true
			}
		}
	}

	// Kill the (still-live, gate-passed) holder. SKIPPED when the recheck
	// observed a genuinely-dead PID (holderGoneAtRecheck): killing a gone
	// PID is at best a no-op and at worst terminates a reused PID without
	// the identity gate (pr301 r7 Finding 2). The acquire-poll below is the
	// recovery path for the already-gone case.
	if !holderGoneAtRecheck {
		// killProcessOverride is the test seam for the kill helper.
		// Lets the wait-for-exit unit test (Codex iter-9 P2 #2) replace
		// killProcess with a no-op so the test doesn't actually
		// SIGKILL/TerminateProcess any real process. Production code
		// path is unchanged when this is nil.
		kill := killProcess
		if killProcessOverride != nil {
			kill = killProcessOverride
		}
		if err := kill(v.PID); err != nil {
			// Codex iter-12 P2 #1: if the recorded PID exited between
			// probe and kill, Linux returns ESRCH and Windows fails
			// OpenProcess. The process exit is exactly what releases the
			// flock — fall through to acquire-poll instead of returning
			// VerdictKillFailed and forcing a rerun. processID's alive
			// telemetry confirms the PID is gone before we proceed.
			if id, idErr := processID(v.PID); idErr == nil && !id.Alive {
				// PID is genuinely gone; proceed to acquire-poll. The
				// loop below will succeed quickly when the kernel
				// finishes releasing the flock.
			} else {
				v.Class = VerdictKillFailed
				v.Diagnose = fmt.Sprintf("kill PID %d failed: %v", v.PID, err)
				v.Hint = "Permission denied or process already gone; rerun mcphub gui without --force to handshake."
				return nil, v, err
			}
		}
	}

	// Codex iter-9 P2 #2 / memo §"Take-over protocol" step 5f: wait
	// for the kernel to register the kill before the acquire-poll
	// loop. Without this, async TerminateProcess on Windows or slow
	// Unix cleanup (zombie reaping, fd close, flock release) could
	// keep the flock held past the acquire deadline and produce a
	// spurious VerdictRaceLost. The acquire-poll's own deadline is
	// the final safety net if processID telemetry lags.
	exitDeadline := time.Now().Add(opts.KillExitDeadline)
	for time.Now().Before(exitDeadline) {
		id, _ := processID(v.PID)
		if !id.Alive {
			break
		}
		select {
		case <-ctx.Done():
			v.Class = VerdictKillFailed
			v.Diagnose = "context cancelled while waiting for killed process to exit"
			v.Hint = ""
			return nil, v, ctx.Err()
		case <-time.After(opts.KillExitBackoff):
		}
	}

	// postKillHook fires between the kill+wait and the acquire-poll
	// loop. Tests use it to simulate a race-winner competing for the
	// flock. Note: this fires AFTER the iter-9 wait-for-exit so
	// existing tests that simulate "kernel released flock" via the
	// hook still work — the wait short-circuits when processID
	// reports alive=false (which the override seam can simulate).
	if postKillHook != nil {
		postKillHook()
	}

	// Acquire-poll loop (memo §"Take-over protocol" step 5g).
	deadline := time.Now().Add(opts.AcquireDeadline)
	for time.Now().Before(deadline) {
		// Codex iter-12 P2 #2: honor cancellation before each
		// acquire attempt. signal.NotifyContext marks ctx.Done()
		// when SIGINT/SIGTERM arrives; without this check the loop
		// would keep trying and could acquire+rewrite pidport AFTER
		// the user cancelled (the cancellation only surfaces later
		// in startGuiServer).
		if err := ctx.Err(); err != nil {
			v.Class = VerdictKillFailed
			v.Diagnose = "context cancelled during post-kill acquire poll"
			v.Hint = "Operator cancelled (Ctrl+C/SIGTERM) after the kill but before the new lock was acquired; the killed incumbent is gone but no replacement gui was started."
			return nil, v, err
		}
		lock, err := AcquireSingleInstanceAt(pidportPath, v.Port)
		if err == nil {
			v.Class = VerdictKilledRecovered
			v.Diagnose = fmt.Sprintf("force-killed previous incumbent PID %d and acquired lock", v.PID)
			v.Hint = ""
			return lock, v, nil
		}
		if !errors.Is(err, ErrSingleInstanceBusy) {
			v.Class = VerdictKillFailed
			v.Diagnose = fmt.Sprintf("post-kill acquire failed: %v", err)
			v.Hint = ""
			return nil, v, err
		}
		// Sleep with cancellation awareness so a SIGINT during
		// the back-off interval is observed promptly rather than
		// after the full opts.AcquireBackoff elapses.
		select {
		case <-ctx.Done():
			v.Class = VerdictKillFailed
			v.Diagnose = "context cancelled during post-kill acquire poll"
			v.Hint = "Operator cancelled (Ctrl+C/SIGTERM) after the kill but before the new lock was acquired; the killed incumbent is gone but no replacement gui was started."
			return nil, v, ctx.Err()
		case <-time.After(opts.AcquireBackoff):
		}
	}
	v.Class = VerdictRaceLost
	v.Diagnose = fmt.Sprintf("kill succeeded but a competitor acquired the lock during %s deadline", opts.AcquireDeadline)
	v.Hint = "Rerun mcphub gui without --force to handshake with the new incumbent."
	return nil, v, fmt.Errorf("race lost")
}

// CheckIdentityGate runs the three-part identity gate (image basename
// / argv subcommand / start-time vs pidport mtime) and the macOS
// shortcut against a Verdict, without sending any signal. Callers run
// this BEFORE the destructive confirmation prompt so the operator
// never confirms a kill that the gate would later refuse. Returns
// (refused=false, "") when all checks pass and the kill should
// proceed.
//
// KillRecordedHolder still re-runs the same gate internally for
// defense in depth (so a non-cli caller cannot skip the check), and
// the second invocation is the production source of truth — its
// re-probe sees the latest pidport state. The pre-prompt invocation
// guards UX, not safety.
//
// Codex iter-9 P2 #1.
func CheckIdentityGate(v Verdict) (refused bool, reason string) {
	refused, reason, _, _ = checkIdentityGateInternal(v)
	return refused, reason
}

// checkIdentityGateInternal is the shared gate implementation used
// by both CheckIdentityGate (UX pre-prompt check) and
// KillRecordedHolder (defense-in-depth post-prompt check). Returns
// the user-facing diagnose, the user-facing hint, and a short
// machine-readable errReason all derived from the gate that tripped.
//
// The override seam (identityGateOverride) is honored first so
// existing seam-mocked tests continue to work; macOS shortcut runs
// only when the override is nil. Codex iter-9 P2 #1 deduplicates the
// production gate cascade so the public CheckIdentityGate and the
// internal KillRecordedHolder gate cannot drift.
func checkIdentityGateInternal(v Verdict) (refused bool, diagnose, hint, errReason string) {
	// Test override comes first so seam-mocked tests reach this
	// branch on linux/windows even when v.macOSUnsupported is true.
	if identityGateOverride != nil {
		if r, reason := identityGateOverride(v); r {
			return true,
				"identity gate (test override): " + reason,
				"",
				"override: " + reason
		}
		return false, "", "", ""
	}

	// Codex iter-3 P2 #2: macOS shortcut — when probeOnce flagged
	// the verdict as macOSUnsupported, processIDImpl returned no
	// useful identity signals. Refuse the kill explicitly with a
	// macOS-specific diagnose instead of letting the cascade emit
	// "image '' is not an mcphub binary".
	if v.macOSUnsupported {
		return true,
			"kill refused: macOS identity probe not supported; reboot is the recovery path",
			"Tracked as backlog: macOS libproc/sysctl-based identity (see probe_darwin.go).",
			"macOS identity probe not supported"
	}

	// Same shape on Windows non-amd64 builds: probeOnce sets
	// archUnsupported because PEB offsets are amd64-only. Refuse the
	// kill with an arch-specific diagnose. Codex bot review on PR
	// #23 P2 (probeOnce arch sentinel handling).
	if v.archUnsupported {
		return true,
			"kill refused: Windows non-amd64 build cannot enumerate cmdline/start-time; identity gate cannot run",
			"This Windows build lacks PEB-offset support for the running architecture. Use Task Manager / Get-Process to identify the stuck mcphub PID and end it manually, or rebuild mcphub for amd64.",
			"Windows arch identity probe not supported"
	}

	if !matchBasename(v.PIDImage) {
		return true,
			fmt.Sprintf("recorded PID %d image %q is not an mcphub binary", v.PID, v.PIDImage),
			"Identity-gate (image basename) failed; identify and kill the actual flock holder via OS tools.",
			"image gate"
	}
	// Codex iter-3 P2 #1: read v.pidCmdlineRaw (the unmodified
	// argv populated by probeOnce), not v.PIDCmdline (truncated for
	// display). Truncating before this gate would drop argv[1]
	// when argv[0] (the binary path) exceeds 1KB and allow a
	// non-GUI mcphub subcommand whose long path triggers truncation
	// to pass the len(argv)==1 branch.
	if !cmdlineIsGui(v.pidCmdlineRaw) {
		// Codex iter-10 P2 #2: print ONLY the offending subcommand
		// token (argv[1]), not the full argv. mcphub commands like
		// `mcphub secrets set --value <SECRET>` carry secret material
		// in argv; if a stale/recycled/corrupt pidport points at one
		// of those processes, echoing the full argv leaks the secret
		// into stderr/CI logs. The gate decision only depends on
		// argv[1], so that's all the diagnostic needs.
		var subcommand string
		if len(v.pidCmdlineRaw) >= 2 {
			subcommand = v.pidCmdlineRaw[1]
		} else {
			subcommand = "(none)"
		}
		return true,
			fmt.Sprintf("recorded PID %d argv subcommand is %q, not 'gui'", v.PID, subcommand),
			"Identity-gate (argv subcommand) failed; the recorded PID is a different mcphub subcommand.",
			"argv gate"
	}
	if !startTimeBeforeMtime(v.PIDStart, v.Mtime, time.Second) {
		return true,
			fmt.Sprintf("recorded PID %d start-time %s postdates pidport mtime %s — PID-recycled", v.PID, v.PIDStart.Format(time.RFC3339), v.Mtime.Format(time.RFC3339)),
			"Identity-gate (start-time) failed; the PID has been recycled to a different process.",
			"start-time gate"
	}
	// SEC-F3 owner-SID gate (additional, fail-closed): refuse to kill a flock
	// holder owned by a DIFFERENT user SID even when image/argv/start-time all
	// match, mirroring the POSIX reaper's UID gate. A different-owner SID OR an
	// unverifiable owner (token open/query failure) refuses the kill. On
	// non-Windows the seam default is a no-op (true, nil), so this arm never
	// changes the Linux/macOS gate verdict.
	if match, err := processOwnerSIDMatchesCurrentFn(v.PID); err != nil {
		if errors.Is(err, process.ErrProcessAlreadyExited) {
			// pr301 r5 Finding 3: the flock holder exited between the
			// image/argv/start-time identity probe and the owner-SID gate's
			// OpenProcess (a TOCTOU window). The owner-SID arm surfaces the
			// canonical ErrProcessAlreadyExited sentinel. A GONE holder is NOT
			// an unverifiable-owner failure — its exit is exactly what releases
			// the flock, so the kill the gate guards is moot. Do NOT refuse:
			// return refused=false so KillRecordedHolder reaches the
			// kill+acquire-poll recovery path (which handles an already-dead PID
			// gracefully — kill() of a gone PID falls through to the
			// flock-release acquire-poll and yields VerdictKilledRecovered).
			// Converting this to VerdictKillRefused would strand the operator on
			// a stuck lock whose holder is already gone.
			return false, "", "", ""
		}
		return true,
			fmt.Sprintf("recorded PID %d owner could not be verified: %v", v.PID, err),
			"Identity-gate (owner SID) could not verify the process owner; refusing the kill. Identify and kill the actual flock holder via OS tools.",
			"owner-SID gate"
	} else if !match {
		return true,
			fmt.Sprintf("recorded PID %d is owned by a different user; refusing to kill", v.PID),
			"Identity-gate (owner SID) failed; the recorded PID belongs to a different user. mcphub will not terminate another user's process.",
			"owner-SID gate"
	}
	return false, "", "", ""
}

// cmdlineIsGui implements the rev 9 argv-subcommand gate:
// argv[1] == "gui" OR len(argv) == 1 (no-arg auto-gui: Explorer
// double-click OR a bare `mcphub` typed at a terminal).
func cmdlineIsGui(argv []string) bool {
	if len(argv) == 1 {
		return true
	}
	if len(argv) >= 2 && argv[1] == "gui" {
		return true
	}
	return false
}

// startTimeBeforeMtime returns true iff start ≤ mtime + tolerance.
// A start time strictly later than the pidport mtime indicates the
// PID was recycled to a process that began AFTER our pidport was
// written.
func startTimeBeforeMtime(start, mtime time.Time, tolerance time.Duration) bool {
	if start.IsZero() || mtime.IsZero() {
		// Defensive: missing telemetry → fail closed.
		return false
	}
	return !start.After(mtime.Add(tolerance))
}

// truncateCmdline caps the total argv string length at maxBytes for
// safe logging/JSON. The identity gate (cmdlineIsGui) reads the raw
// argv from Verdict.pidCmdlineRaw, NOT the truncated PIDCmdline this
// function produces, so truncation cannot influence the gate's
// decision. (Codex iter-3 P2 #1: pre-fix code truncated before the
// gate, which dropped argv[1] when argv[0] exceeded maxBytes and
// let the len(argv)==1 branch of cmdlineIsGui pass for a non-GUI
// mcphub subcommand whose binary path was long enough.)
//
// Truncation is display/JSON-bounding only, not a security mitigation.
func truncateCmdline(argv []string, maxBytes int) []string {
	if len(argv) == 0 {
		return argv
	}
	out := make([]string, 0, len(argv))
	used := 0
	for _, a := range argv {
		if used+len(a) > maxBytes {
			remaining := maxBytes - used
			if remaining > 0 {
				out = append(out, a[:remaining])
			}
			break
		}
		out = append(out, a)
		used += len(a)
	}
	return out
}

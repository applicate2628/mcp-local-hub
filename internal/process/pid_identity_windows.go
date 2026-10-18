//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const terminateWaitMilliseconds = 5000

type processHandleIdentity struct {
	Basename       string
	ExecutablePath string
	CreationTime   time.Time
}

// PIDHandleIdentity is one precise kernel observation of a process PID. It is
// an observation value only; it does not grant termination authority.
type PIDHandleIdentity struct {
	PID            int
	ExecutablePath string
	StartedAt      time.Time
}

// HeldPIDIdentity reads the exact generation from an already-owned process
// handle. Unlike a PID lookup, it cannot silently switch to a reused PID.
type HeldPIDIdentity interface {
	Identity() (PIDHandleIdentity, error)
}

type heldPIDGeneration struct {
	mu             sync.Mutex
	pid            int
	handle         windows.Handle
	ops            heldPIDGenerationOps
	closeAttempted bool
	closeErr       error
}

// HeldPIDJobObservation owns a query-only process handle. The caller must
// Close it on every path; it carries no process-termination authority.
type HeldPIDJobObservation interface {
	VerifyIdentity(PIDIdentityProof) error
	InAnyJob() (bool, error)
	Close() error
}

// HeldPIDObservation retains one query/synchronize-only generation. No raw
// handle, signal or termination capability is exposed to its consumer.
type HeldPIDObservation interface {
	Identity() (PIDHandleIdentity, error)
	OwnerSID() (string, error)
	Wait() (PIDWaitObservation, error)
	VerifyIdentity(PIDIdentityProof) error
	Close() error
}

type PIDWaitObservation struct{ Result uint32 }

type heldPIDObservation struct {
	*heldPIDJobObservation
	ownerSID           func(windows.Handle) (*windows.SID, error)
	wait               func(windows.Handle, uint32) (uint32, error)
	closeMu            sync.Mutex
	closeAttempted     bool
	closeErr           error
	ownerSIDFormatting *SIDFormatObservation
}

func (h *heldPIDObservation) Close() error {
	if h == nil || h.heldPIDJobObservation == nil {
		return errors.New("process: original held observation required")
	}
	h.closeMu.Lock()
	defer h.closeMu.Unlock()
	if h.closeAttempted {
		return h.closeErr
	}
	h.closeAttempted = true
	h.closeErr = h.heldPIDJobObservation.Close()
	return h.closeErr
}

func HoldPIDForObservation(pid int) (HeldPIDObservation, error) {
	base, err := holdPIDForJobObservationWithAccess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, windows.OpenProcess, verifyHandleIdentity, processInAnyJob, windows.CloseHandle)
	if err != nil {
		return nil, err
	}
	return &heldPIDObservation{heldPIDJobObservation: base, ownerSID: processHandleOwnerUserSID, wait: windows.WaitForSingleObject}, nil
}

func (h *heldPIDObservation) OwnerSID() (string, error) {
	if h == nil || h.heldPIDJobObservation == nil || h.handle == 0 {
		return "", errors.New("process: held observation is closed")
	}
	sid, err := h.ownerSID(h.handle)
	if err != nil {
		return "", err
	}
	if sid == nil {
		return "", errors.New("process: held owner SID unavailable")
	}
	value, observation, formatErr := FormatSIDObserved(sid)
	h.ownerSIDFormatting = observation
	if formatErr != nil {
		return "", formatErr
	}
	if err := observation.ValidateInput(sid, value); err != nil {
		return "", err
	}
	return value, nil
}

func (h *heldPIDObservation) Wait() (PIDWaitObservation, error) {
	if h == nil || h.heldPIDJobObservation == nil || h.handle == 0 || !h.verified {
		return PIDWaitObservation{}, errors.New("process: held observation lacks verified identity")
	}
	result, err := h.wait(h.handle, 0)
	if result == windows.WAIT_FAILED {
		if err == nil {
			err = errors.New("Windows wait failed without an immediate error")
		}
		return PIDWaitObservation{Result: result}, fmt.Errorf("process: held wait failed: %w", err)
	}
	if result != windows.WAIT_OBJECT_0 && result != uint32(windows.WAIT_TIMEOUT) {
		return PIDWaitObservation{Result: result}, fmt.Errorf("process: held wait returned unsupported state %d", result)
	}
	return PIDWaitObservation{Result: result}, nil
}

type heldPIDJobObservation struct {
	pid      int
	handle   windows.Handle
	verified bool
	verify   func(PIDIdentityProof, windows.Handle) error
	query    func(windows.Handle) (bool, error)
	close    func(windows.Handle) error
	identity func(int, windows.Handle) (processHandleIdentity, error)
}

func HoldPIDForJobObservation(pid int) (HeldPIDJobObservation, error) {
	return holdPIDForJobObservation(pid, windows.OpenProcess, verifyHandleIdentity, processInAnyJob, windows.CloseHandle)
}

func holdPIDForJobObservation(pid int, open func(uint32, bool, uint32) (windows.Handle, error), verify func(PIDIdentityProof, windows.Handle) error, query func(windows.Handle) (bool, error), close func(windows.Handle) error) (HeldPIDJobObservation, error) {
	return holdPIDForJobObservationWithAccess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION, open, verify, query, close)
}

func holdPIDForJobObservationWithAccess(pid int, access uint32, open func(uint32, bool, uint32) (windows.Handle, error), verify func(PIDIdentityProof, windows.Handle) error, query func(windows.Handle) (bool, error), close func(windows.Handle) error) (*heldPIDJobObservation, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("process: invalid PID %d", pid)
	}
	h, err := open(access, false, uint32(pid))
	if err != nil {
		return nil, fmt.Errorf("process: open PID %d for job observation: %w", pid, err)
	}
	return &heldPIDJobObservation{pid: pid, handle: h, verify: verify, query: query, close: close, identity: identityFromHandle}, nil
}

func (h *heldPIDJobObservation) Identity() (PIDHandleIdentity, error) {
	if h == nil || h.handle == 0 {
		return PIDHandleIdentity{}, errors.New("process: held PID job observation is closed")
	}
	return identityFromHeld(h.pid, h.handle, h.identity)
}

func (h *heldPIDJobObservation) VerifyIdentity(proof PIDIdentityProof) error {
	if h == nil || h.handle == 0 {
		return errors.New("process: held PID job observation is closed")
	}
	h.verified = false
	if proof.PID != h.pid {
		return fmt.Errorf("%w: proof PID %d differs from held PID %d", ErrProcessIdentityMismatch, proof.PID, h.pid)
	}
	if err := h.verify(proof, h.handle); err != nil {
		return err
	}
	h.verified = true
	return nil
}

func (h *heldPIDJobObservation) InAnyJob() (bool, error) {
	if h == nil || h.handle == 0 || !h.verified {
		return false, errors.New("process: held PID job observation lacks verified identity")
	}
	member, err := h.query(h.handle)
	if err != nil {
		return false, fmt.Errorf("process: query PID %d job membership: %w", h.pid, err)
	}
	return member, nil
}

func (h *heldPIDJobObservation) Close() error {
	if h == nil || h.handle == 0 {
		return nil
	}
	err := h.close(h.handle)
	h.handle, h.verified = 0, false
	if err != nil {
		return fmt.Errorf("process: close PID %d job observation: %w", h.pid, err)
	}
	return nil
}

func processInAnyJob(h windows.Handle) (bool, error) {
	var member uint32
	r1, _, callErr := procIsProcessInJob.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&member)))
	if r1 == 0 {
		return false, fmt.Errorf("IsProcessInJob: %w", callErr)
	}
	return member != 0, nil
}

type heldPIDGenerationOps struct {
	identity       func(int, windows.Handle) (processHandleIdentity, error)
	verifyIdentity func(PIDIdentityProof, windows.Handle) error
	terminate      func(windows.Handle, uint32) error
	wait           func(windows.Handle, uint32) (uint32, error)
	close          func(windows.Handle) error
	waitExit       func(context.Context, uintptr) (HeldPIDExitResult, error)
}

var productionHeldPIDGenerationOps = heldPIDGenerationOps{
	identity:       identityFromHandle,
	verifyIdentity: verifyHandleIdentity,
	terminate:      windows.TerminateProcess,
	wait:           windows.WaitForSingleObject,
	close:          windows.CloseHandle,
	waitExit:       WaitRetainedProcessExit,
}

// HoldPIDForTermination opens one process generation with the complete access
// needed to verify and terminate it. The caller owns the returned handle and
// must close it on every path.
func HoldPIDForTermination(pid int) (HeldPIDGeneration, error) {
	return holdPIDForTermination(pid, windows.OpenProcess, productionHeldPIDGenerationOps)
}

func holdPIDForTermination(
	pid int,
	openProcess func(uint32, bool, uint32) (windows.Handle, error),
	ops heldPIDGenerationOps,
) (HeldPIDGeneration, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("process: invalid PID %d", pid)
	}
	access := uint32(windows.PROCESS_TERMINATE | windows.SYNCHRONIZE | windows.PROCESS_QUERY_LIMITED_INFORMATION)
	h, err := openProcess(access, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil, fmt.Errorf("process: PID %d already exited before generation hold: %w", pid, ErrProcessAlreadyExited)
		}
		return nil, fmt.Errorf("process: open PID %d for generation hold: %w", pid, err)
	}
	return &heldPIDGeneration{pid: pid, handle: h, ops: ops}, nil
}

func (h *heldPIDGeneration) PID() int {
	if h == nil {
		return 0
	}
	return h.pid
}

func (h *heldPIDGeneration) Identity() (PIDHandleIdentity, error) {
	if h == nil {
		return PIDHandleIdentity{}, errors.New("process: held PID generation is closed")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handle == 0 || h.closeAttempted {
		return PIDHandleIdentity{}, errors.New("process: held PID generation is closed")
	}
	return identityFromHeld(h.pid, h.handle, h.ops.identity)
}

func identityFromHeld(pid int, handle windows.Handle, read func(int, windows.Handle) (processHandleIdentity, error)) (PIDHandleIdentity, error) {
	if read == nil {
		return PIDHandleIdentity{}, errors.New("process: held identity reader unavailable")
	}
	identity, err := read(pid, handle)
	if err != nil {
		return PIDHandleIdentity{}, fmt.Errorf("process: read held PID %d identity: %w", pid, err)
	}
	return PIDHandleIdentity{PID: pid, ExecutablePath: identity.ExecutablePath, StartedAt: identity.CreationTime.UTC()}, nil
}

func (h *heldPIDGeneration) VerifyIdentity(proof PIDIdentityProof) error {
	if h == nil {
		return fmt.Errorf("process: held PID generation is closed")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handle == 0 || h.closeAttempted {
		return fmt.Errorf("process: held PID generation is closed")
	}
	if proof.PID != h.pid {
		return fmt.Errorf("%w: proof PID %d does not match held PID %d", ErrProcessIdentityMismatch, proof.PID, h.pid)
	}
	return h.ops.verifyIdentity(proof, h.handle)
}

func (h *heldPIDGeneration) Terminate() (bool, error) {
	if h == nil {
		return false, fmt.Errorf("process: held PID generation is closed")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handle == 0 || h.closeAttempted {
		return false, fmt.Errorf("process: held PID generation is closed")
	}
	if err := h.ops.terminate(h.handle, 1); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			if ev, waitErr := h.ops.wait(h.handle, 0); waitErr == nil && ev == windows.WAIT_OBJECT_0 {
				return false, fmt.Errorf("process: PID %d already exited before terminate: %w", h.pid, ErrProcessAlreadyExited)
			}
		}
		return false, fmt.Errorf("process: terminate PID %d: %w", h.pid, err)
	}
	ev, waitErr := h.ops.wait(h.handle, terminateWaitMilliseconds)
	if waitErr != nil {
		return true, fmt.Errorf("process: wait for PID %d after terminate: %w", h.pid, waitErr)
	}
	if ev == uint32(windows.WAIT_TIMEOUT) {
		return true, fmt.Errorf("process: timeout waiting for PID %d to exit after terminate", h.pid)
	}
	if ev != windows.WAIT_OBJECT_0 {
		return true, fmt.Errorf("process: wait for PID %d after terminate returned unexpected code %d", h.pid, ev)
	}
	return true, nil
}

func (h *heldPIDGeneration) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closeAttempted {
		return h.closeErr
	}
	if h.handle == 0 {
		return nil
	}
	h.closeAttempted = true
	err := h.ops.close(h.handle)
	if err != nil {
		h.closeErr = fmt.Errorf("process: close held PID %d generation: %w", h.pid, err)
		return h.closeErr
	}
	h.handle = 0
	return nil
}

func (h *heldPIDGeneration) WaitExit(ctx context.Context) (HeldPIDExitResult, error) {
	if ctx == nil {
		return HeldPIDExitResult{}, errors.New("process: exit wait context required")
	}
	if h == nil {
		return HeldPIDExitResult{}, errors.New("process: held PID generation is closed")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handle == 0 || h.closeAttempted {
		return HeldPIDExitResult{}, errors.New("process: held PID generation is closed")
	}
	if h.ops.waitExit == nil {
		return HeldPIDExitResult{}, ErrProcessIdentityUnsupported
	}
	return h.ops.waitExit(ctx, uintptr(h.handle))
}

type retainedProcessExitOps struct {
	createEvent func() (windows.Handle, error)
	setEvent    func(windows.Handle) error
	wait        func([]windows.Handle, bool, uint32) (uint32, error)
	exitCode    func(windows.Handle, *uint32) error
	close       func(windows.Handle) error
}

// WaitRetainedProcessExit borrows this exact original waitable process object.
// It opens/terminates/closes no process and adds no caller budget or waiter.
func WaitRetainedProcessExit(ctx context.Context, handle uintptr) (HeldPIDExitResult, error) {
	return waitRetainedProcessExit(ctx, handle, retainedProcessExitOps{
		createEvent: func() (windows.Handle, error) { return windows.CreateEvent(nil, 1, 0, nil) },
		setEvent:    windows.SetEvent, wait: windows.WaitForMultipleObjects,
		exitCode: windows.GetExitCodeProcess, close: windows.CloseHandle,
	})
}

func waitRetainedProcessExit(ctx context.Context, handle uintptr, ops retainedProcessExitOps) (result HeldPIDExitResult, resultErr error) {
	if ctx == nil || handle == 0 {
		return result, errors.New("process: original handle and exit wait context required")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	cancel, err := ops.createEvent()
	if err != nil || cancel == 0 {
		if cancel != 0 {
			err = errors.Join(err, ops.close(cancel))
		}
		if err == nil {
			err = errors.New("native cancellation event unavailable")
		}
		return result, fmt.Errorf("create process observation cancellation event: %w", err)
	}
	wakeDone := make(chan struct{})
	var wakeErr error
	stopWake := context.AfterFunc(ctx, func() {
		wakeErr = ops.setEvent(cancel)
		close(wakeDone)
	})
	defer func() {
		if !stopWake() {
			<-wakeDone
			if wakeErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("wake process observation: %w", wakeErr))
			}
		}
		if err := ops.close(cancel); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close process observation cancellation event: %w", err))
		}
	}()
	for {
		// Native milliseconds are a representation limit, not a polling budget.
		timeout := uint32(windows.INFINITE)
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return result, context.DeadlineExceeded
			}
			millis := remaining.Milliseconds()
			if remaining%time.Millisecond != 0 {
				millis++
			}
			if millis >= int64(windows.INFINITE) {
				timeout = windows.INFINITE - 1
			} else {
				timeout = uint32(millis)
			}
		}
		event, err := ops.wait([]windows.Handle{windows.Handle(handle), cancel}, false, timeout)
		if err != nil {
			return result, fmt.Errorf("wait original retained process: %w", err)
		}
		switch event {
		case windows.WAIT_OBJECT_0:
			result.Exited = true
			var code uint32
			if err := ops.exitCode(windows.Handle(handle), &code); err != nil {
				return result, fmt.Errorf("read original retained process exit: %w", err)
			}
			result.ExitCode, result.ExitCodeAvailable = code, true
			return result, nil
		case windows.WAIT_OBJECT_0 + 1:
			if err := ctx.Err(); err != nil {
				return result, err
			}
			return result, errors.New("process cancellation event signalled without context cancellation")
		case uint32(windows.WAIT_TIMEOUT):
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if timeout == windows.INFINITE {
				return result, errors.New("original retained process wait timed out without a deadline")
			}
		default:
			return result, fmt.Errorf("wait original retained process returned %#x", event)
		}
	}
}

func PIDExecutableMatches(pid int, expectedPath string) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ident, err := identityFromHandle(pid, h)
	if err != nil {
		return false
	}
	return executablePathMatches(ident.ExecutablePath, expectedPath)
}

// LookupPIDHandleIdentity returns the executable path and exact kernel creation
// time obtained from one handle. The handle is closed before this function
// returns on both success and error paths.
func LookupPIDHandleIdentity(pid int) (PIDHandleIdentity, error) {
	return lookupPIDHandleIdentity(pid, windows.OpenProcess, identityFromHandle, windows.CloseHandle)
}

func lookupPIDHandleIdentity(
	pid int,
	openProcess func(uint32, bool, uint32) (windows.Handle, error),
	identityFromHandle func(int, windows.Handle) (processHandleIdentity, error),
	closeHandle func(windows.Handle) error,
) (identity PIDHandleIdentity, retErr error) {
	if pid <= 0 {
		return PIDHandleIdentity{}, fmt.Errorf("process: invalid PID %d", pid)
	}
	h, err := openProcess(uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE), false, uint32(pid))
	if err != nil {
		return PIDHandleIdentity{}, fmt.Errorf("process: open PID %d for handle identity: %w", pid, err)
	}
	defer func() {
		if err := closeHandle(h); err != nil {
			identity = PIDHandleIdentity{}
			retErr = errors.Join(retErr, fmt.Errorf("process: close PID %d handle identity: %w", pid, err))
		}
	}()
	handleIdentity, err := identityFromHandle(pid, h)
	if err != nil {
		return PIDHandleIdentity{}, err
	}
	if handleIdentity.ExecutablePath == "" || handleIdentity.CreationTime.IsZero() {
		return PIDHandleIdentity{}, fmt.Errorf("process: PID %d handle identity is incomplete", pid)
	}
	return PIDHandleIdentity{PID: pid, ExecutablePath: handleIdentity.ExecutablePath, StartedAt: handleIdentity.CreationTime.UTC()}, nil
}

func VerifyPIDIdentity(proof PIDIdentityProof) error {
	if proof.PID <= 0 {
		return fmt.Errorf("process: invalid PID %d", proof.PID)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(proof.PID))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return fmt.Errorf("process: PID %d already exited before identity open: %w", proof.PID, ErrProcessAlreadyExited)
		}
		return fmt.Errorf("process: open PID %d for identity: %w", proof.PID, err)
	}
	defer windows.CloseHandle(h)
	return verifyHandleIdentity(proof, h)
}

func TerminatePIDWithIdentity(proof PIDIdentityProof) error {
	return terminatePIDWithIdentity(proof, HoldPIDForTermination)
}

func terminatePIDWithIdentity(
	proof PIDIdentityProof,
	hold func(int) (HeldPIDGeneration, error),
) error {
	held, err := hold(proof.PID)
	if err != nil {
		return err
	}
	defer func() { _ = held.Close() }()
	if err := held.VerifyIdentity(proof); err != nil {
		return err
	}
	_, err = held.Terminate()
	return err
}

func verifyHandleIdentity(proof PIDIdentityProof, h windows.Handle) error {
	ident, err := identityFromHandle(proof.PID, h)
	if err != nil {
		return err
	}
	if !executablePathMatches(ident.ExecutablePath, proof.ExecutablePath) {
		return fmt.Errorf("%w: PID %d executable path mismatch", ErrProcessIdentityMismatch, proof.PID)
	}
	if !strings.EqualFold(ident.Basename, filepath.Base(proof.ExecutablePath)) {
		return fmt.Errorf("%w: PID %d executable basename mismatch", ErrProcessIdentityMismatch, proof.PID)
	}
	recorded, err := parseExpectedStartedAt(proof.StartedAt)
	if err != nil {
		return err
	}
	observed := ident.CreationTime.UTC()
	if !startTimesMatchWithin(recorded, observed, pidIdentityStartToleranceFor(proof)) {
		return fmt.Errorf("%w: PID %d started_at mismatch recorded=%s observed=%s", ErrProcessIdentityMismatch, proof.PID, recorded.Format(time.RFC3339Nano), observed.Format(time.RFC3339Nano))
	}
	return nil
}

func identityFromHandle(pid int, h windows.Handle) (processHandleIdentity, error) {
	exe, err := queryProcessImagePath(h)
	if err != nil {
		return processHandleIdentity{}, fmt.Errorf("process: query PID %d image path: %w", pid, err)
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return processHandleIdentity{}, fmt.Errorf("process: query PID %d process times: %w", pid, err)
	}
	return processHandleIdentity{
		Basename:       filepath.Base(exe),
		ExecutablePath: exe,
		CreationTime:   time.Unix(0, creation.Nanoseconds()).UTC(),
	}, nil
}

func queryProcessImagePath(h windows.Handle) (string, error) {
	size := uint32(windows.MAX_PATH)
	for {
		buf := make([]uint16, size)
		n := size
		err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n)
		if err == nil {
			return windows.UTF16ToString(buf[:n]), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return "", err
		}
		size *= 2
		if size > 32768 {
			return "", fmt.Errorf("image path exceeds 32768 UTF-16 code units")
		}
	}
}

func executablePathMatches(got, expected string) bool {
	if got == "" || expected == "" {
		return false
	}
	// IDENTITY-GATE SHORT-CIRCUIT.
	//
	// WHAT THIS GATE PROTECTS AGAINST: Windows reuses PIDs. A PID the
	// supervisor recorded for one of its daemons can, after that daemon
	// exits, belong to an unrelated — possibly hostile — process. This
	// comparison is one of the conjuncts (with the basename check and the
	// kernel creation-time check in verifyHandleIdentity) that proves the
	// process now holding this PID is the binary we spawned, before callers
	// report it healthy or terminate it.
	//
	// WHY SKIPPING NORMALIZATION HERE CANNOT WEAKEN THAT: this branch can
	// only ever answer "equal". Any difference falls through to the full
	// normalize-and-compare below, byte-for-byte unchanged — so the
	// REJECTION path, which is the security-relevant direction, is not
	// touched at all. Normalization is a deterministic function of (path,
	// filesystem state), so two spellings that are already equal under the
	// same comparison this function ends with necessarily produce canonical
	// forms that are also equal under it. The branch therefore skips work
	// whose result is already determined; it cannot turn a mismatch into a
	// match.
	//
	// This is NOT a cache: nothing is retained between calls, so there is no
	// entry that can go stale against a binary swap, a repointed symlink, or
	// an `mcphub install --upgrade` rename-aside. Do not "optimize" this
	// into a memo — TestExecutablePathMatchesReresolvesAfterBinarySwap and
	// TestNormalizeWindowsExecutablePathFollowsSymlinkRepoint pin that.
	if strings.EqualFold(got, expected) {
		return true
	}
	got = normalizeWindowsExecutablePath(got)
	expected = normalizeWindowsExecutablePath(expected)
	return got != "" && expected != "" && strings.EqualFold(got, expected)
}

func normalizeWindowsExecutablePath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

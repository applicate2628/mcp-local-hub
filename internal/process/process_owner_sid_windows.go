//go:build windows

// Package process — SEC-F3 Windows owner-SID gate (defense-in-depth).
//
// The operator-facing Windows process-kill gates (GUI `mcphub gui --force
// --kill`, `mcphub stop --force` daemon kill, supervisor-reaper force-kill)
// historically verified image basename + argv + process-start-time but NOT
// the target process's OWNER SID. The POSIX cold-start reaper DOES gate on
// UID (supervise_reaper_posix.go: os.Getuid vs the target's stat uid), so the
// Windows kill surfaces were asymmetric: on a multi-user Windows host an
// admin-token mcphub session (or a `taskkill /F /T` shell-out) could terminate
// a DIFFERENT user's legitimately-named mcphub.exe.
//
// ProcessOwnerSIDMatchesCurrent closes that asymmetry. It is the single owner
// of the SID-comparison logic, called by the api stop-force gate, the cli
// supervisor-reaper gate, and (through the api seam) the gui force-kill gate.
package process

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

// currentProcessUserSID resolves the SID of the user the CURRENT process token
// belongs to (the user running mcphub). It is the same value for the lifetime
// of the process, so it is resolved once and cached. GetCurrentProcessToken
// returns a pseudo-handle that needs no Close.
var (
	currentSIDOnce sync.Once
	currentSID     *windows.SID
	currentSIDErr  error
)

func currentProcessUserSID() (*windows.SID, error) {
	currentSIDOnce.Do(func() {
		t := windows.GetCurrentProcessToken()
		u, err := t.GetTokenUser()
		if err != nil {
			currentSIDErr = fmt.Errorf("process: current token user: %w", err)
			return
		}
		// Copy so the SID outlives the *Tokenuser buffer the syscall returned.
		sid, err := u.User.Sid.Copy()
		if err != nil {
			currentSIDErr = fmt.Errorf("process: copy current SID: %w", err)
			return
		}
		currentSID = sid
	})
	return currentSID, currentSIDErr
}

// processOwnerUserSID resolves the owner (token-user) SID of an arbitrary PID.
// Opens the target with PROCESS_QUERY_LIMITED_INFORMATION (the least-privilege
// access that still permits OpenProcessToken; works without admin in the common
// same-user case), opens its token with TOKEN_QUERY, and reads TokenUser.
//
// pr301 r4 Finding 2 (TOCTOU): the operator-facing kill gates verify a target's
// image/argv/start-time identity FIRST, then call this gate. A process that
// passed that earlier probe can EXIT before this OpenProcess runs. A dead PID
// makes OpenProcess fail with ERROR_INVALID_PARAMETER (and, more rarely,
// ERROR_NOT_FOUND). That is NOT an "unverifiable owner" condition — the target
// is simply GONE — so it is mapped to the package-canonical ErrProcessAlreadyExited
// sentinel (same mapping pid_identity_windows.go / pid_alive_windows.go /
// best_effort_kill_windows.go already apply to OpenProcess on a dead PID). The
// caller treats a gone target as a benign already-dead no-op (the kill it was
// about to perform is moot), NOT as a hard force-kill failure. Any OTHER
// OpenProcess error (e.g. ERROR_ACCESS_DENIED on a LIVE process whose token we
// may not open) stays a wrapped error → the caller fails closed and refuses to
// kill a live process whose owner it cannot verify.
func processOwnerUserSID(pid int) (*windows.SID, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("process: invalid PID %d", pid)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if openProcessErrIsProcessGone(err) {
			return nil, fmt.Errorf("process: OpenProcess(PID %d): %w", pid, ErrProcessAlreadyExited)
		}
		return nil, fmt.Errorf("process: OpenProcess(PID %d): %w", pid, err)
	}
	defer windows.CloseHandle(h)

	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		return nil, fmt.Errorf("process: OpenProcessToken(PID %d): %w", pid, err)
	}
	defer token.Close()

	u, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("process: GetTokenInformation(TokenUser, PID %d): %w", pid, err)
	}
	// Copy so the returned SID outlives the *Tokenuser buffer.
	sid, err := u.User.Sid.Copy()
	if err != nil {
		return nil, fmt.Errorf("process: copy SID (PID %d): %w", pid, err)
	}
	return sid, nil
}

// openProcessErrIsProcessGone reports whether an OpenProcess failure means the
// target PID no longer refers to a live process (it exited / was never there),
// as opposed to a permission/transient failure on a still-live process.
//
// ERROR_INVALID_PARAMETER is the canonical "PID does not refer to a live
// process" code Windows returns from OpenProcess (the same mapping
// pid_alive_windows.go, pid_identity_windows.go, and best_effort_kill_windows.go
// already use). ERROR_NOT_FOUND is included defensively for the rarer kernel
// path where a just-exited PID surfaces as "element not found". ACCESS_DENIED is
// deliberately EXCLUDED: it indicates a LIVE process whose token we may not open
// (e.g. a different-integrity-level or different-owner process), which must stay
// a fail-closed "unverifiable" error, never a "gone" verdict.
func openProcessErrIsProcessGone(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_NOT_FOUND)
}

// ProcessOwnerSIDMatchesCurrent reports whether the target PID is owned by the
// SAME user SID as the current mcphub process. It is the Windows owner-SID arm
// of the operator-facing kill gates (SEC-F3).
//
// Tri-state contract:
//
//   - (true,  nil) — MATCH. Target owner SID == current process owner SID. The
//     common single-user case: the daemon/GUI being killed belongs to the same
//     user running mcphub, so the kill proceeds exactly as before.
//   - (false, nil) — PROVEN MISMATCH (benign). BOTH SIDs were read successfully
//     and they differ — a different-owner process. This is NOT an error: it is a
//     verified fact that the target belongs to another user. The caller MUST
//     refuse (skip) the kill, but the refusal is a normal, expected outcome —
//     e.g. a stale supervisor.lock sidecar pointing at ANOTHER user's
//     mcphub-shaped supervisor must let `install --upgrade` SKIP the foreign
//     process and proceed, not ABORT. (#301-1.)
//   - (false, ErrProcessAlreadyExited) — TARGET GONE (benign already-dead).
//     pr301 r4 Finding 2: the target PID exited between the caller's earlier
//     image/argv/start-time identity probe and this gate's OpenProcess (a
//     TOCTOU window the operator-facing kill paths necessarily have). The kill
//     this gate was guarding is moot — there is nothing left to kill — so the
//     caller treats it as a benign already-dead no-op (errors.Is(err,
//     ErrProcessAlreadyExited)), exactly like the pre-existing already-exited
//     handling elsewhere on the force-kill path. It is NOT a hard failure: a
//     gone process is not a live process whose owner we failed to verify.
//   - (false, err) — UNVERIFIABLE (fail-closed). The current process SID could
//     not be resolved, OR the target is LIVE but its token could not be
//     opened/queried (ACCESS_DENIED on OpenProcess, or OpenProcessToken /
//     GetTokenUser / SID copy failed). The owner is UNKNOWN on a process that
//     still exists, so the caller MUST refuse the kill AND treat the non-nil
//     error as a hard force-kill FAILURE (never kill a process whose owner
//     cannot be proven to match; never proceed past a process we cannot
//     identify).
//
// Callers treat (false, nil), (false, ErrProcessAlreadyExited), and the generic
// (false, err) all as "do not kill THIS process", but they diverge on whether
// the surrounding flow may CONTINUE: (false, nil) is a benign different-owner
// skip; (false, ErrProcessAlreadyExited) is a benign already-dead skip (the kill
// succeeded by virtue of the target being gone); the generic (false, err) is a
// propagated failure on a live-but-unverifiable target. Error-first branching
// (errors.Is on ErrProcessAlreadyExited BEFORE inspecting the bool) keeps the
// already-dead case from being misread as a proven different-owner.
func ProcessOwnerSIDMatchesCurrent(pid int) (bool, error) {
	cur, err := currentProcessUserSID()
	if err != nil {
		// UNVERIFIABLE: cannot resolve our own SID → fail closed.
		return false, err
	}
	target, err := processOwnerUserSID(pid)
	if err != nil {
		// Either TARGET GONE (ErrProcessAlreadyExited, benign already-dead) or
		// UNVERIFIABLE on a live target (fail closed). The sentinel is preserved
		// in the wrapped chain so the caller's errors.Is branch resolves it.
		return false, err
	}
	return sidsMatchResult(cur, target)
}

// sidsMatchResult is the pure comparison core extracted from
// ProcessOwnerSIDMatchesCurrent so the proven-mismatch contract can be
// unit-tested without exercising the OpenProcess / token syscalls (#301-1).
//
// Both arguments are SIDs that were already read successfully. The result is
// therefore the two VERIFIED outcomes only:
//
//   - (true,  nil) — the SIDs are equal (match).
//   - (false, nil) — the SIDs differ. This is a PROVEN mismatch, NOT an error:
//     a benign identity-difference the caller safely SKIPs. Returning an error
//     here would force the Windows reaper to treat a known different-owner
//     process as a force-kill FAILURE and abort install --upgrade against a
//     stale foreign supervisor.lock sidecar, instead of skipping it.
//
// The UNVERIFIABLE (false, err) case never originates here — it is the syscall
// wrapper's job to fail closed when a token cannot be read.
func sidsMatchResult(cur, target *windows.SID) (bool, error) {
	if !target.Equals(cur) {
		return false, nil
	}
	return true, nil
}

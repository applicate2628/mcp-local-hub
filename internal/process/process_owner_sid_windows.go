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
func processOwnerUserSID(pid int) (*windows.SID, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("process: invalid PID %d", pid)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
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

// ProcessOwnerSIDMatchesCurrent reports whether the target PID is owned by the
// SAME user SID as the current mcphub process. It is the Windows owner-SID arm
// of the operator-facing kill gates (SEC-F3).
//
// Tri-state contract, FAIL CLOSED:
//
//   - (true,  nil) — target owner SID == current process owner SID. The common
//     single-user case: the daemon/GUI being killed belongs to the same user
//     running mcphub, so the kill proceeds exactly as before.
//   - (false, nil) — target owner SID DIFFERS from the current user. A
//     different-owner process: the caller MUST refuse the kill.
//   - (false, err) — the target's token could not be opened or queried (or the
//     current process SID could not be resolved). The owner is UNVERIFIABLE, so
//     the caller MUST refuse the kill (never kill a process whose owner cannot
//     be proven to match).
//
// Callers treat both (false, nil) and (false, err) as "do not kill"; the error
// is propagated for diagnostics.
func ProcessOwnerSIDMatchesCurrent(pid int) (bool, error) {
	cur, err := currentProcessUserSID()
	if err != nil {
		return false, err
	}
	target, err := processOwnerUserSID(pid)
	if err != nil {
		return false, err
	}
	if !target.Equals(cur) {
		return false, fmt.Errorf("process: PID %d owner SID %s does not match current user SID %s",
			pid, sidStringOrUnresolved(target), sidStringOrUnresolved(cur))
	}
	return true, nil
}

// sidStringOrUnresolved renders a SID for diagnostics, tolerating a nil or
// unconvertible SID without panicking.
func sidStringOrUnresolved(sid *windows.SID) string {
	if sid == nil {
		return "<nil-sid>"
	}
	if s := sid.String(); s != "" {
		return s
	}
	return "<unresolved-sid>"
}

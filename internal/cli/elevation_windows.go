//go:build windows

// Package cli — Task 11 elevation detector (Windows variant).
//
// Plan v13 §42 Administrator install refusal: `mcphub setup` (and
// `mcphub watchdog install`) refuse to install the per-user watchdog
// scheduled task when invoked from a process running with elevated
// privileges, UNLESS --allow-elevated is passed. Rationale: a watchdog
// task installed from an elevated context could land with the wrong
// principal, opening a privilege-escalation surface.
//
// Detection on Windows uses GetTokenInformation(TokenElevation) per
// §42. The TOKEN_ELEVATION struct returns a single DWORD; nonzero
// means the token is elevated. The check is one syscall + zero
// state, so it's safe to invoke unconditionally.
package cli

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// isElevatedReal returns true when the current process token reports
// TokenElevation.TokenIsElevated != 0. Errors propagate verbatim so
// callers can decide whether to fail closed (refuse) or fall back
// (treat as not-elevated). Per plan §42 the production fail-closed
// policy is: treat resolution failure as elevated → require
// --allow-elevated to proceed.
//
// Implementation: prefer the pseudo-token returned by
// GetCurrentProcessToken (no Close needed) so the helper is allocation-
// free on the happy path. If GetTokenInformation against that token
// fails, fall back to OpenProcessToken on the current process and
// retry once before giving up.
func isElevatedReal() (bool, error) {
	type tokenElevation struct {
		TokenIsElevated uint32
	}
	queryElevation := func(t windows.Token) (bool, error) {
		var te tokenElevation
		var returnedLen uint32
		if err := windows.GetTokenInformation(
			t,
			windows.TokenElevation,
			(*byte)(unsafe.Pointer(&te)),
			uint32(unsafe.Sizeof(te)),
			&returnedLen,
		); err != nil {
			return false, err
		}
		return te.TokenIsElevated != 0, nil
	}

	// First try: pseudo-token from GetCurrentProcessToken (no Close needed).
	pseudo := windows.GetCurrentProcessToken()
	if elev, err := queryElevation(pseudo); err == nil {
		return elev, nil
	} else {
		// Fall through to the fully-opened token path; remember the
		// first error in case both paths fail so we can surface it.
		firstErr := err
		var opened windows.Token
		if openErr := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &opened); openErr != nil {
			return false, fmt.Errorf("GetTokenInformation(TokenElevation): %w (also OpenProcessToken: %v)", firstErr, openErr)
		}
		defer opened.Close()
		elev, retryErr := queryElevation(opened)
		if retryErr != nil {
			return false, fmt.Errorf("GetTokenInformation(TokenElevation) on opened token: %w", retryErr)
		}
		return elev, nil
	}
}

//go:build !windows

// Package cli — Task 11 EventLog source registration (POSIX variant).
//
// Plan v13 §60 specifies the Windows EventLog cascade; per §17 the
// v0.3.0 watchdog ships Windows-only. On POSIX the registration /
// removal helpers are no-ops so runSetupWatchdog can call them
// unconditionally without OS-branching.
package cli

// registerEventLogSourceReal is a no-op on POSIX. Returning nil keeps
// runSetupWatchdog's cascade reading correctly: any error here
// triggers the watchdog.log fallback entry, but POSIX has no
// EventLog so there's nothing to register.
func registerEventLogSourceReal() error { return nil }

// removeEventLogSourceReal is a no-op on POSIX. Same rationale as
// registerEventLogSourceReal.
func removeEventLogSourceReal() error { return nil }

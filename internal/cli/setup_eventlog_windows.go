//go:build windows

// Package cli — Task 11 EventLog source registration (Windows variant).
//
// Plan v13 §60: `mcphub setup` registers the Windows EventLog source
// `mcp-local-hub` so the §49 audit-degraded cascade can use
// eventlog.Notify when watchdog.log + stderr are both unwritable.
// Registration is idempotent — registry.ErrAlreadyExists is treated
// as success. Failure is non-fatal per §60: the cascade still has
// stderr/syslog as fallbacks; setup logs the failure to watchdog.log
// as `eventlog-source-registration-failed-non-fatal`.
//
// `mcphub uninstall` removes the source via eventlog.Remove; "source
// not found" / `registry.ErrNotExist` are treated as success.
package cli

import (
	"errors"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"
)

// _ keeps the registry import alive for the errors.Is(err,
// registry.ErrNotExist) belt-and-braces fallback below; the
// already-exists path uses the string-match helper because the
// upstream API returns a plain errors.New (no typed sentinel).
var _ = errors.Is

// eventLogSourceName is the canonical Application-log source per §60.
// Matches the literal expected by the audit-degraded cascade
// fallback (Task 9 §49) so eventlog.Notify(sourceName, ...) writes
// land in the correct registry tree.
const eventLogSourceName = "mcp-local-hub"

// registerEventLogSourceReal registers the EventLog source per §60.
// Returns nil on success or already-exists; any other error is
// surfaced verbatim. Caller (runSetupWatchdog) translates the
// non-nil error into a non-fatal watchdog.log entry.
//
// Implementation note: eventlog.InstallAsEventCreate doesn't expose
// a typed already-exists error — it returns errors.New("... key
// already exists"). The plan §60 reference to registry.ErrExist /
// registry.ErrAlreadyExists describes the intent, not the literal
// API. We tolerate the API by string match against the well-known
// phrase + the registry.ErrNotExist sentinel for sibling cases.
func registerEventLogSourceReal() error {
	err := eventlog.InstallAsEventCreate(
		eventLogSourceName,
		eventlog.Info|eventlog.Warning|eventlog.Error,
	)
	if err == nil {
		return nil
	}
	// Defensive — modern Windows builds return the literal string;
	// older versions might wrap registry.ErrNotExist or similar.
	if isAlreadyExistsEventLogErr(err) {
		return nil
	}
	return err
}

// removeEventLogSourceReal removes the EventLog source per §60. The
// "source not found" path is treated as success — uninstall must be
// idempotent so repeated runs do not error after the source was
// deleted by a prior teardown or by a manual operator action.
func removeEventLogSourceReal() error {
	err := eventlog.Remove(eventLogSourceName)
	if err == nil {
		return nil
	}
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if isNotFoundEventLogErr(err) {
		return nil
	}
	return err
}

// isAlreadyExistsEventLogErr inspects err for the well-known
// "already exists" message variants emitted by older Windows
// releases that don't wrap registry.ErrExist explicitly.
func isAlreadyExistsEventLogErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Common variants: "already exists", "registry key already exists".
	for _, sub := range []string{"already exists"} {
		if containsFold(msg, sub) {
			return true
		}
	}
	return false
}

// isNotFoundEventLogErr detects "source not found" variants that
// don't wrap registry.ErrNotExist. Belt-and-braces alongside the
// errors.Is path above.
func isNotFoundEventLogErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, sub := range []string{"not exist", "not found", "cannot find"} {
		if containsFold(msg, sub) {
			return true
		}
	}
	return false
}

// containsFold is a tiny case-insensitive substring check kept local
// so this file stays free of strings/strings.ToLower allocations on
// the hot path. Cheaper than importing strings package-wide just for
// one fold check.
func containsFold(haystack, needle string) bool {
	hl := len(haystack)
	nl := len(needle)
	if nl == 0 {
		return true
	}
	if nl > hl {
		return false
	}
	for i := 0; i+nl <= hl; i++ {
		match := true
		for j := 0; j < nl; j++ {
			ch := haystack[i+j]
			nc := needle[j]
			if ch >= 'A' && ch <= 'Z' {
				ch += 'a' - 'A'
			}
			if nc >= 'A' && nc <= 'Z' {
				nc += 'a' - 'A'
			}
			if ch != nc {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

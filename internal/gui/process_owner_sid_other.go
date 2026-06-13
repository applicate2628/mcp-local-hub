//go:build !windows

package gui

// defaultProcessOwnerSIDMatchesCurrent (non-Windows) is a no-op: POSIX has no
// Windows token model and the Linux/macOS identity gate is intentionally left
// unchanged by SEC-F3. Always reporting a match keeps the gui force-kill gate
// behavior identical to before on non-Windows hosts.
func defaultProcessOwnerSIDMatchesCurrent(_ int) (bool, error) {
	return true, nil
}

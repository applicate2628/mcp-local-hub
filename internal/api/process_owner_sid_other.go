//go:build !windows

package api

// defaultProcessOwnerSIDMatchesCurrent (non-Windows) is a no-op: POSIX has no
// Windows token model and the POSIX kill paths are intentionally left
// unchanged by SEC-F3 (the cold-start reaper carries its own UID gate). Always
// reporting a match keeps the api stop-force gate behavior identical to before
// on Linux/macOS.
func defaultProcessOwnerSIDMatchesCurrent(_ int) (bool, error) {
	return true, nil
}

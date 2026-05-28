//go:build !windows

package process

// JobProtectionStatus returns nil on non-Windows platforms because
// NewKillOnCloseJob is a no-op compatibility stub there, not a real
// Windows Job Object protection boundary.
func JobProtectionStatus(_ error) *bool {
	return nil
}

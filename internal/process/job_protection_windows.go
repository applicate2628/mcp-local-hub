//go:build windows

package process

// JobProtectionStatus maps the platform Job Object creation outcome
// onto the status tri-state. On Windows, NewKillOnCloseJob returning
// nil error means the current spawn has kill-on-close Job protection;
// any error means the supervisor will take the documented non-fatal
// fallback and start the daemon without that protection.
func JobProtectionStatus(jobErr error) *bool {
	protected := jobErr == nil
	return &protected
}

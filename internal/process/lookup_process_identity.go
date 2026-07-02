package process

// ProcessIdentity collects every field migration's 4-gate ownership
// check needs to verify a PID belongs to a v0.4.x mcphub daemon
// (image basename, command-line pattern, creation time, install-dir
// anchor). See docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-
// architecture.md §"Pre-unregister daemon stop" line 259-263.
//
// The struct is DECLARED here (no build tag) rather than beside the
// Windows-only LookupProcessIdentity so cross-platform callers can name
// process.ProcessIdentity in a signature and still compile on POSIX. The
// only PRODUCER of a populated value is the Windows LookupProcessIdentity;
// on non-Windows targets the type exists but is never produced (the
// supervisor's port-squatter classifier fails closed to observe-only on
// those platforms — no reap authority without a start-time-proof handle).
type ProcessIdentity struct {
	// PID is the input PID echoed for caller convenience.
	PID int
	// Basename is the executable file name (e.g., "mcphub.exe");
	// derived from the CIM Win32_Process.Name property which already
	// strips the directory path.
	Basename string
	// CommandLine is the full command-line verbatim as CIM reports it.
	// Operators must NOT shell-parse this string for security
	// boundaries — use ExecutablePath for that.
	CommandLine string
	// ExecutablePath is the absolute path to the executable image.
	// This is the 4-gate ownership check's anchor against
	// same-user-attacker spoofing of name+argv.
	ExecutablePath string
	// CreationDateUnix is process start time as Unix seconds (UTC).
	// Computed inside PowerShell via Get-Date -UFormat %s to sidestep
	// locale-formatted CIM date string parsing on the Go side.
	CreationDateUnix int64
}

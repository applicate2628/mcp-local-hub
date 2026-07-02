//go:build windows

package cli

import "mcp-local-hub/internal/process"

// On Windows the port-squatter classifier resolves the owner PID's identity via
// process.LookupProcessIdentity (PowerShell CIM primary, wmic fallback,
// start-time proof). That function only compiles on Windows, so it is bound
// here; on every other platform squatterLookupIdentityFn stays nil and
// classifyPortSquatter fails closed to squatterUnverified (MUST-FIX #6:
// Windows-only reap authority in v1).
func init() {
	squatterLookupIdentityFn = process.LookupProcessIdentity
}

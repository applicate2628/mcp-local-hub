//go:build windows

package cli

import (
	"context"
	"time"

	"mcp-local-hub/internal/process"
)

// squatterIdentityLookupDeadline bounds a single owner-identity lookup so one
// wedged host WMI query cannot stall the caller forever. It is generous — well
// above the lookup's own 3-attempt + 2×1s-backoff budget (~6.5s worst case on a
// slow AV host) — so a healthy/slow lookup is never cut off; only a genuinely
// hung shell-out hits the deadline and returns ctx.DeadlineExceeded, which
// classifyPortSquatter maps to squatterUnverified (fail-closed, no kill). The
// F1 port-gate worker + the liveness sweep are the callers; bounding here means
// neither the single worker nor the sweep goroutine can wedge on a hung WMI.
const squatterIdentityLookupDeadline = 12 * time.Second

// On Windows the port-squatter classifier resolves the owner PID's identity via
// process.LookupProcessIdentityContext (PowerShell CIM primary, wmic fallback,
// start-time proof), each call wrapped in a per-call deadline. That function
// only compiles on Windows, so it is bound here; on every other platform
// squatterLookupIdentityFn stays nil and classifyPortSquatter fails closed to
// squatterUnverified (MUST-FIX #6: Windows-only reap authority in v1).
func init() {
	squatterLookupIdentityFn = func(ctx context.Context, pid int) (process.ProcessIdentity, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, cancel := context.WithTimeout(ctx, squatterIdentityLookupDeadline)
		defer cancel()
		return process.LookupProcessIdentityContext(ctx, pid)
	}
}

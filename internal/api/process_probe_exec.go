package api

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"mcp-local-hub/internal/process"
)

// ErrProbeTimeout marks an OS-fact probe that was cut by its deadline rather
// than answering. Callers MUST treat it as "unknown", never as a negative
// answer they can act on: a timed-out ownership probe does not prove the port
// is foreign, it proves we could not find out.
var ErrProbeTimeout = errors.New("process probe timed out")

// ── Probe deadline budgets ───────────────────────────────────────────────────
//
// Every OS-fact probe in this package (netstat / wmic / PowerShell
// Get-CimInstance) is a short-lived diagnostic child process. Before this
// owner existed each one ran through a bare exec.Command + cmd.Output(), which
// on Windows is WaitForSingleObject(handle, INFINITE) — an unbounded wait with
// no recovery if the child never exits. A wedged Winmgmt service (WMI
// repository corruption, a hung CIM query, AV inspecting the child) therefore
// blocked the CALLER forever. The GUI /api/server/readiness handler is one such
// caller, so the hang was operator-facing, not merely a test problem.
//
// The numbers below are measured, not guessed. On the reference Windows 11
// host under real load (38 live mcphub daemons):
//
//	netstat -ano ................................  110 ms
//	schtasks /Query /TN <task> /V /FO LIST ......  177 ms
//	wmic process where ProcessId=<live pid> .....  4.1 s – 6.7 s
//	powershell Get-CimInstance (single PID) ..... 10.5 s
//
// The WMI-backed queries dominate, and the PowerShell fallback is SLOWER than
// the wmic path it backs up. So:
//
//   - probeCommandTimeout (15s) bounds ONE child. It sits above the slowest
//     measured healthy probe (10.5s) with headroom, so a loaded-but-working
//     host is never falsely cut; its job is to kill a wedged child in bounded
//     time, not to control latency.
//   - probeChainBudget (20s) bounds a whole wmic→PowerShell FALLBACK CHAIN.
//     The fallback exists because Windows 11 24H2+ removes wmic.exe (which
//     fails fast), NOT because wmic is slow — so a slow wmic must not buy a
//     second full-price PowerShell attempt. One shared deadline across both
//     attempts keeps the chain's worst case at 20s instead of 2×15s = 30s.
//     20s covers the measured worst-case healthy chain (6.7s + 10.5s = 17.2s).
//
// Latency of a whole multi-probe operation is bounded by its own caller's
// budget (see allServerReadinessBudget in readiness.go); these constants only
// guarantee that no SINGLE probe can block forever.
const (
	probeCommandTimeout = 15 * time.Second
	probeChainBudget    = 20 * time.Second

	// probeWaitDelay bounds the window between "context expired, child killed"
	// and "Wait returns". Without it, Cmd.Wait still blocks on the stdout-copy
	// goroutines whenever a GRANDCHILD inherited the pipe and outlives the
	// child we killed — so killing the child alone would NOT have made the call
	// bounded. WaitDelay forces the pipes closed and Wait to return.
	probeWaitDelay = 2 * time.Second
)

// runProbeCommand runs one OS-fact probe under its own deadline and returns its
// stdout. This is the single owner of "how a diagnostic subprocess is spawned"
// in this package: every netstat / wmic / PowerShell probe goes through here so
// the deadline cannot be forgotten at an individual call site.
func runProbeCommand(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runProbeCommandCtx(ctx, name, args...)
}

// runProbeCommandCtx runs one OS-fact probe under a CALLER-OWNED deadline, so a
// fallback chain (wmic then PowerShell) can share one budget across both
// attempts instead of granting each attempt a fresh full timeout.
//
// The child is additionally capped at probeCommandTimeout even when ctx allows
// longer, so a single wedged probe can never consume an entire chain budget and
// starve the fallback that would have answered.
func runProbeCommandCtx(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		// Budget already spent by an earlier attempt in the chain: do not spawn
		// a child that cannot finish in time.
		return nil, fmt.Errorf("%s: %w", name, ErrProbeTimeout)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, probeCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, name, args...)
	// WaitDelay is what makes this call actually bounded — see probeWaitDelay.
	cmd.WaitDelay = probeWaitDelay
	process.NoConsole(cmd)

	out, err := cmd.Output()
	if err != nil {
		// Distinguish "we ran out of time" from "the tool answered with a
		// failure" (e.g. wmic.exe absent on Windows 11 24H2+, which must still
		// fall through to the PowerShell path immediately).
		if cmdCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("%s: %w", name, ErrProbeTimeout)
		}
		return nil, err
	}
	return out, nil
}

// newProbeChainContext returns the shared deadline for a wmic→PowerShell
// fallback chain. Callers defer the cancel.
func newProbeChainContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), probeChainBudget)
}

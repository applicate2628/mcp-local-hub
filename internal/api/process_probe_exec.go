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
// host (build 26100 = 24H2, 1018 live processes, 38 mcphub daemons):
//
//	netstat -ano ...................................   110 ms
//	schtasks /Query /TN <task> /V /FO LIST .........   177 ms
//	wmic single-PID filtered, idle .................  2.3 s –  4.6 s
//	wmic single-PID filtered, under readiness load .  1.6 s – 10.3 s
//	wmic FULL-TABLE (runProcessSnapshot) ...........  2.9 s –  8.2 s
//	powershell Get-CimInstance (single PID) ........          10.5 s
//	powershell Get-CimInstance (full table) ........           7.8 s
//
// RECONCILIATION with internal/api/main_test.go:82-88, which records "~31s per
// wmic call on Win11 24H2". This host IS 24H2, and nothing measured here comes
// close to 31s — the worst single probe observed, deliberately sampled WHILE the
// readiness fan-out was hammering WMI, was 10.3s. The gap is NOT explained by
// filtered-vs-full-table (both were measured above; full-table is not the slow
// one). So the 31s record is either from a different host/WMI-repository state
// or from an aggregate of several calls, and it could NOT be reproduced here.
//
// Because it could not be REPRODUCED but also could not be FALSIFIED, the caps
// below are sized to accommodate it rather than to match today's measurement.
// That choice follows from the asymmetry of harm:
//
//   - A cap set too HIGH costs bounded extra latency before the deadline fires.
//     The whole-report budget (readinessBudget in readiness.go) is what actually
//     controls user-visible latency, so this costs little.
//   - A cap set too LOW manufactures FALSE timeouts on a healthy-but-slow host.
//     Post-FIX-1 a timeout degrades to an honest UNKNOWN — safe, but a report
//     that is mostly UNKNOWN is useless, and mass-UNKNOWN would be self-inflicted.
//
// A wedged probe is caught either way; only the honesty of a healthy host's
// report depends on getting this right. Hence:
//
//   - probeCommandTimeout (45s) bounds ONE child: above the repo's recorded 31s
//     worst case with ~1.45x headroom, and ~4.3x the worst probe measured here.
//     Its ONLY job is to kill a wedged child in bounded time. It is deliberately
//     NOT a latency control.
//   - probeChainBudget (60s) bounds a whole wmic→PowerShell FALLBACK CHAIN. The
//     fallback exists because Windows 11 24H2+ removes wmic.exe (which fails
//     FAST), NOT because wmic is slow — so a slow wmic must not buy a second
//     full-price PowerShell attempt. One shared deadline keeps the chain's worst
//     case at 60s rather than 2x45s = 90s.
const (
	probeCommandTimeout = 45 * time.Second
	probeChainBudget    = 60 * time.Second

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

package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

// fakeLiveProc models the live identity of a PID as
// process.TerminatePIDWithIdentity sees it on a held handle.
type fakeLiveProc struct {
	exePath      string
	startedAt    string // RFC3339Nano; the live kernel start time for the PID
	cmdline      string
	gone         bool // simulate a process that exited before terminate
	accessDenied bool // simulate an ACCESS_DENIED refusal
}

// fakeTerminator returns a stand-in for orphanTerminateFn that faithfully
// mirrors process.TerminatePIDWithIdentity's fail-closed contract: it kills a
// PID ONLY when the proof's {ExecutablePath, StartedAt} match the PID's LIVE
// identity. A PID recycled onto a different process between census and kill
// (different exe / start time) yields ErrProcessIdentityMismatch — no kill.
// Every proof it receives is appended to calls so tests can assert the reaper
// went through the identity-gated primitive with the census-captured proof.
func fakeTerminator(live map[int]fakeLiveProc, calls *[]process.PIDIdentityProof) func(process.PIDIdentityProof) error {
	return func(p process.PIDIdentityProof) error {
		*calls = append(*calls, p)
		lp, ok := live[p.PID]
		if !ok || lp.gone {
			return fmt.Errorf("process: PID %d gone: %w", p.PID, process.ErrProcessAlreadyExited)
		}
		if lp.accessDenied {
			return fmt.Errorf("process: terminate PID %d: Access is denied.", p.PID)
		}
		recorded, recErr := time.Parse(time.RFC3339Nano, p.StartedAt)
		observed, obsErr := time.Parse(time.RFC3339Nano, lp.startedAt)
		if p.StartTolerance > 0 && recErr == nil && obsErr == nil {
			delta := recorded.Sub(observed)
			if delta < 0 {
				delta = -delta
			}
			if delta > p.StartTolerance {
				return fmt.Errorf("%w: PID %d identity re-verify failed", process.ErrProcessIdentityMismatch, p.PID)
			}
		} else if lp.startedAt != p.StartedAt {
			return fmt.Errorf("%w: PID %d identity re-verify failed", process.ErrProcessIdentityMismatch, p.PID)
		}
		if !strings.EqualFold(lp.exePath, p.ExecutablePath) {
			return fmt.Errorf("%w: PID %d identity re-verify failed", process.ErrProcessIdentityMismatch, p.PID)
		}
		return nil
	}
}

func swapOrphanTerminator(t *testing.T, fn func(process.PIDIdentityProof) error) {
	t.Helper()
	prev := orphanTerminateFn
	orphanTerminateFn = fn
	t.Cleanup(func() { orphanTerminateFn = prev })
}

func swapOrphanLookupIdentity(t *testing.T, fn func(context.Context, int) (process.ProcessIdentity, error)) {
	t.Helper()
	prev := orphanLookupIdentityFn
	orphanLookupIdentityFn = fn
	t.Cleanup(func() { orphanLookupIdentityFn = prev })
}

func swapOrphanIdentityLookupTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := orphanIdentityLookupTimeout
	orphanIdentityLookupTimeout = d
	t.Cleanup(func() { orphanIdentityLookupTimeout = prev })
}

// TestParseOrphans_CapturesIdentityProof verifies the census wires the
// ExecutablePath column and the RFC3339Nano StartedAt (derived from the
// snapshot CreationDate) onto each detected orphan — the two fields that,
// with the PID, form the kill-time identity proof.
func TestParseOrphans_CapturesIdentityProof(t *testing.T) {
	const created = "20250101120000.000000+000"
	const exe = `C:\Program Files\nodejs\node.exe`
	csv := "Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize\n" +
		`HOST,"C:\Windows\explorer.exe",` + created + `,C:\Windows\explorer.exe,1,4000,10000000` + "\n" +
		`HOST,"node.exe c:\path\to\wolfram-server.js",` + created + `,` + exe + `,4000,5000,80000000` + "\n"

	orphans := parseOrphans(strings.NewReader(csv), []string{"wolfram-server"})
	if len(orphans) != 1 {
		t.Fatalf("expected exactly 1 orphan (PID 5000), got %d: %+v", len(orphans), orphans)
	}
	o := orphans[0]
	if o.PID != 5000 {
		t.Fatalf("orphan PID = %d, want 5000", o.PID)
	}
	if o.ExecutablePath != exe {
		t.Errorf("orphan.ExecutablePath = %q, want %q", o.ExecutablePath, exe)
	}
	if o.StartedAt == "" {
		t.Fatal("orphan.StartedAt is empty; census must capture the process start time for the identity proof")
	}
	parsed, err := time.Parse(time.RFC3339Nano, o.StartedAt)
	if err != nil {
		t.Fatalf("orphan.StartedAt %q is not RFC3339Nano: %v", o.StartedAt, err)
	}
	// StartedAt must be the SAME instant the census CreationDate parses to
	// (the instant the kill-time kernel re-read compares against).
	wantInstant := parseWmicDate(created).UTC()
	if !parsed.Equal(wantInstant) {
		t.Errorf("orphan.StartedAt instant = %s, want %s (census CreationDate instant)", parsed, wantInstant)
	}
}

// TestReapOrphans_IdentityMatchKills verifies a genuine orphan whose census
// proof matches its live identity IS killed (KillErr empty), and that the
// reaper drove the identity-gated primitive with the exact census proof.
func TestReapOrphans_IdentityMatchKills(t *testing.T) {
	const exe = `C:\Program Files\nodejs\node.exe`
	const started = "2025-01-01T12:00:00Z"
	const cmdline = `node.exe c:\path\to\wolfram-server.js`
	var calls []process.PIDIdentityProof
	swapOrphanLookupIdentity(t, func(_ context.Context, pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: pid, CommandLine: cmdline}, nil
	})
	swapOrphanTerminator(t, fakeTerminator(map[int]fakeLiveProc{
		5000: {exePath: exe, startedAt: started}, // live identity == census proof
	}, &calls))

	orphans := []OrphanProcess{{PID: 5000, ExecutablePath: exe, StartedAt: started, Cmdline: cmdline}}
	reapOrphans(orphans, false)

	if orphans[0].KillErr != "" {
		t.Errorf("KillErr = %q, want empty (identity match should kill)", orphans[0].KillErr)
	}
	if len(calls) != 1 {
		t.Fatalf("terminator called %d times, want 1", len(calls))
	}
	got := calls[0]
	if got.PID != 5000 || got.ExecutablePath != exe || got.StartedAt != started {
		t.Errorf("proof = %+v, want PID=5000 exe=%q started=%q (census-captured proof)", got, exe, started)
	}
	if got.StartTolerance != cleanupIdentityStartTolerance {
		t.Errorf("proof.StartTolerance = %v, want cleanup tolerance %v", got.StartTolerance, cleanupIdentityStartTolerance)
	}
}

// TestReapOrphans_PIDRecycleRefused is the core friendly-fire guard: an orphan
// whose census-captured {ExecutablePath, StartedAt} no longer match the LIVE
// process at that PID (the PID was recycled onto an unrelated process between
// census and kill) is REFUSED by the identity-gated primitive and NOT killed —
// recorded as an identity mismatch, not a successful kill. Mirrors the
// supervisor squatter reap's PID-reuse test.
func TestReapOrphans_PIDRecycleRefused(t *testing.T) {
	const censusExe = `C:\Program Files\nodejs\node.exe`
	const censusStarted = "2025-01-01T12:00:00Z"
	const cmdline = `node.exe c:\path\to\wolfram-server.js`
	var calls []process.PIDIdentityProof
	swapOrphanLookupIdentity(t, func(_ context.Context, pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: pid, CommandLine: cmdline}, nil
	})
	// PID 5000 is now a DIFFERENT process (svchost, started later) — the OS
	// recycled the PID after the census scan recorded the node orphan.
	swapOrphanTerminator(t, fakeTerminator(map[int]fakeLiveProc{
		5000: {exePath: `C:\Windows\System32\svchost.exe`, startedAt: "2025-01-01T12:05:00Z"},
	}, &calls))

	orphans := []OrphanProcess{{PID: 5000, ExecutablePath: censusExe, StartedAt: censusStarted, Cmdline: cmdline}}
	reapOrphans(orphans, false)

	if len(calls) != 1 {
		t.Fatalf("terminator called %d times, want 1 (proof must be attempted so the primitive can refuse)", len(calls))
	}
	if orphans[0].KillErr == "" {
		t.Fatal("KillErr empty; a recycled PID must be recorded as skipped, not reported as killed")
	}
	if !strings.Contains(orphans[0].KillErr, "identity mismatch") || !strings.Contains(orphans[0].KillErr, "recycled") {
		t.Errorf("KillErr = %q, want an identity-mismatch / PID-recycled note", orphans[0].KillErr)
	}
}

func TestReapOneOrphan_StrictStartToleranceRefusesSameImageRecycle(t *testing.T) {
	const exe = `C:\Program Files\nodejs\node.exe`
	const cmdline = `node.exe c:\path\to\wolfram-server.js`
	const censusStarted = "2025-01-01T12:00:00Z"
	const recycledStarted = "2025-01-01T12:00:00.750Z"
	var calls []process.PIDIdentityProof
	swapOrphanLookupIdentity(t, func(_ context.Context, pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: pid, CommandLine: cmdline}, nil
	})
	swapOrphanTerminator(t, fakeTerminator(map[int]fakeLiveProc{
		5000: {exePath: exe, startedAt: recycledStarted, cmdline: cmdline},
	}, &calls))

	got := reapOneOrphan(OrphanProcess{
		PID:            5000,
		ExecutablePath: exe,
		StartedAt:      censusStarted,
		Cmdline:        cmdline,
	})

	if len(calls) != 1 {
		t.Fatalf("terminator called %d times, want 1", len(calls))
	}
	if calls[0].StartTolerance != cleanupIdentityStartTolerance {
		t.Fatalf("proof.StartTolerance = %v, want %v", calls[0].StartTolerance, cleanupIdentityStartTolerance)
	}
	if got == "" || !strings.Contains(got, "identity mismatch") {
		t.Fatalf("KillErr = %q, want identity mismatch for same-image PID reuse outside cleanup tolerance", got)
	}
}

func TestReapOneOrphan_CommandLineMismatchRefusedBeforeTerminate(t *testing.T) {
	const exe = `C:\Program Files\nodejs\node.exe`
	const started = "2025-01-01T12:00:00Z"
	const censusCmdline = `node.exe c:\path\to\wolfram-server.js`
	swapOrphanLookupIdentity(t, func(_ context.Context, pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: pid, CommandLine: `node.exe c:\other\server.js`}, nil
	})
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, fakeTerminator(map[int]fakeLiveProc{
		5000: {exePath: exe, startedAt: started, cmdline: censusCmdline},
	}, &calls))

	got := reapOneOrphan(OrphanProcess{
		PID:            5000,
		ExecutablePath: exe,
		StartedAt:      started,
		Cmdline:        censusCmdline,
	})

	if len(calls) != 0 {
		t.Fatalf("terminator called %d times, want 0 when argv re-read mismatches census", len(calls))
	}
	if got == "" || !strings.Contains(got, "identity mismatch") {
		t.Fatalf("KillErr = %q, want identity mismatch for command-line mismatch", got)
	}
}

func TestReapOneOrphan_CommandLineQuoteNormalizationAllowsSameCommand(t *testing.T) {
	const exe = `C:\Program Files\nodejs\node.exe`
	const started = "2025-01-01T12:00:00Z"
	const censusCmdline = `C:\Program Files\nodejs\node.exe C:\Users\Ada Lovelace\wolfram-server.js --name alpha`
	const liveCmdline = `"C:\Program Files\nodejs\node.exe"   "C:\Users\Ada Lovelace\wolfram-server.js"   --name   alpha`
	swapOrphanLookupIdentity(t, func(_ context.Context, pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: pid, CommandLine: liveCmdline}, nil
	})
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, fakeTerminator(map[int]fakeLiveProc{
		5000: {exePath: exe, startedAt: started, cmdline: liveCmdline},
	}, &calls))

	got := reapOneOrphan(OrphanProcess{
		PID:            5000,
		ExecutablePath: exe,
		StartedAt:      started,
		Cmdline:        censusCmdline,
	})

	if got != "" {
		t.Fatalf("KillErr = %q, want empty when live command only differs by quotes/spacing", got)
	}
	if len(calls) != 1 {
		t.Fatalf("terminator called %d times, want 1 for quote-normalized command-line match", len(calls))
	}
}

func TestReapOneOrphan_LookupTimeoutRefusesBeforeTerminate(t *testing.T) {
	const exe = `C:\Program Files\nodejs\node.exe`
	const started = "2025-01-01T12:00:00Z"
	const cmdline = `node.exe c:\path\to\wolfram-server.js`
	swapOrphanIdentityLookupTimeout(t, 25*time.Millisecond)
	lookupStarted := make(chan struct{})
	swapOrphanLookupIdentity(t, func(ctx context.Context, pid int) (process.ProcessIdentity, error) {
		close(lookupStarted)
		select {
		case <-ctx.Done():
			return process.ProcessIdentity{}, ctx.Err()
		case <-time.After(time.Second):
			return process.ProcessIdentity{PID: pid, CommandLine: cmdline}, nil
		}
	})
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, fakeTerminator(map[int]fakeLiveProc{
		5000: {exePath: exe, startedAt: started, cmdline: cmdline},
	}, &calls))

	start := time.Now()
	got := reapOneOrphan(OrphanProcess{
		PID:            5000,
		ExecutablePath: exe,
		StartedAt:      started,
		Cmdline:        cmdline,
	})
	elapsed := time.Since(start)

	select {
	case <-lookupStarted:
	default:
		t.Fatal("lookup seam was not called")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("reapOneOrphan took %v, want lookup deadline to refuse quickly", elapsed)
	}
	if len(calls) != 0 {
		t.Fatalf("terminator called %d times, want 0 when identity lookup times out", len(calls))
	}
	if got == "" || !strings.Contains(got, "identity unverified") {
		t.Fatalf("KillErr = %q, want identity-unverified refusal for lookup timeout", got)
	}
}

// TestReapOrphans_DryRunNeverKills verifies dry-run never touches the kill
// primitive regardless of a matching proof.
func TestReapOrphans_DryRunNeverKills(t *testing.T) {
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, fakeTerminator(map[int]fakeLiveProc{
		5000: {exePath: `C:\node.exe`, startedAt: "2025-01-01T12:00:00Z"},
	}, &calls))

	orphans := []OrphanProcess{{PID: 5000, ExecutablePath: `C:\node.exe`, StartedAt: "2025-01-01T12:00:00Z"}}
	reapOrphans(orphans, true) // dryRun

	if len(calls) != 0 {
		t.Errorf("terminator called %d times in dry-run, want 0", len(calls))
	}
	if orphans[0].KillErr != "" {
		t.Errorf("KillErr = %q in dry-run, want empty (nothing was attempted)", orphans[0].KillErr)
	}
}

// TestReapOrphans_MissingProofSkips verifies an orphan with no census-captured
// identity (empty ExecutablePath or StartedAt) is NOT killed — the primitive is
// never even called, and the outcome is recorded as skipped (fail-closed).
func TestReapOrphans_MissingProofSkips(t *testing.T) {
	var calls []process.PIDIdentityProof
	swapOrphanTerminator(t, fakeTerminator(map[int]fakeLiveProc{}, &calls))

	orphans := []OrphanProcess{
		{PID: 5000, ExecutablePath: "", StartedAt: "2025-01-01T12:00:00Z"}, // no exe path
		{PID: 5001, ExecutablePath: `C:\node.exe`, StartedAt: ""},          // no start time
	}
	reapOrphans(orphans, false)

	if len(calls) != 0 {
		t.Errorf("terminator called %d times, want 0 (no proof → never attempt a kill)", len(calls))
	}
	for i, o := range orphans {
		if o.KillErr == "" || !strings.Contains(o.KillErr, "identity unavailable") {
			t.Errorf("orphan[%d].KillErr = %q, want an 'identity unavailable' skip note", i, o.KillErr)
		}
	}
}

// TestReapOrphans_AlreadyExitedRecorded verifies a process that has already
// exited by kill time is recorded distinctly (not a hard failure, not a
// silent success), and that ErrProcessAlreadyExited is classified separately
// from an identity mismatch.
func TestReapOrphans_AlreadyExitedRecorded(t *testing.T) {
	const cmdline = `node.exe c:\path\to\wolfram-server.js`
	var calls []process.PIDIdentityProof
	swapOrphanLookupIdentity(t, func(_ context.Context, pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: pid, CommandLine: cmdline}, nil
	})
	swapOrphanTerminator(t, fakeTerminator(map[int]fakeLiveProc{
		5000: {gone: true},
	}, &calls))

	orphans := []OrphanProcess{{PID: 5000, ExecutablePath: `C:\node.exe`, StartedAt: "2025-01-01T12:00:00Z", Cmdline: cmdline}}
	reapOrphans(orphans, false)

	if len(calls) != 1 {
		t.Fatalf("terminator called %d times, want 1", len(calls))
	}
	if !strings.Contains(orphans[0].KillErr, "already exited") {
		t.Errorf("KillErr = %q, want an 'already exited' note", orphans[0].KillErr)
	}
	if strings.Contains(orphans[0].KillErr, "identity mismatch") {
		t.Errorf("KillErr = %q, an already-exited process must not be reported as an identity mismatch", orphans[0].KillErr)
	}
}

// TestReapOneOrphan_AccessDeniedPropagated verifies a terminate failure that is
// neither already-exited nor an identity mismatch (e.g. ACCESS_DENIED) surfaces
// its message verbatim so operators keep the diagnostic.
func TestReapOneOrphan_AccessDeniedPropagated(t *testing.T) {
	const cmdline = `node.exe c:\path\to\wolfram-server.js`
	swapOrphanLookupIdentity(t, func(_ context.Context, pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: pid, CommandLine: cmdline}, nil
	})
	swapOrphanTerminator(t, fakeTerminator(map[int]fakeLiveProc{
		5000: {accessDenied: true},
	}, &[]process.PIDIdentityProof{}))

	got := reapOneOrphan(OrphanProcess{PID: 5000, ExecutablePath: `C:\node.exe`, StartedAt: "2025-01-01T12:00:00Z", Cmdline: cmdline})
	if !strings.Contains(got, "Access is denied") {
		t.Errorf("KillErr = %q, want the underlying access-denied message preserved", got)
	}
}

// Guard: ErrProcessIdentityMismatch classification is by errors.Is on the
// wrapped sentinel, not string matching, so a wrapped mismatch anywhere in the
// chain is caught.
func TestReapOneOrphan_MismatchClassifiedByErrorsIs(t *testing.T) {
	const cmdline = `node.exe c:\path\to\wolfram-server.js`
	swapOrphanLookupIdentity(t, func(_ context.Context, pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: pid, CommandLine: cmdline}, nil
	})
	swapOrphanTerminator(t, func(process.PIDIdentityProof) error {
		return fmt.Errorf("outer wrap: %w", fmt.Errorf("inner: %w", process.ErrProcessIdentityMismatch))
	})
	got := reapOneOrphan(OrphanProcess{PID: 1, ExecutablePath: `C:\x.exe`, StartedAt: "2025-01-01T12:00:00Z", Cmdline: cmdline})
	if !strings.Contains(got, "identity mismatch") {
		t.Errorf("KillErr = %q, want identity-mismatch classification via errors.Is", got)
	}
	if !errors.Is(process.ErrProcessIdentityMismatch, process.ErrProcessIdentityMismatch) {
		t.Fatal("sanity: sentinel identity")
	}
}

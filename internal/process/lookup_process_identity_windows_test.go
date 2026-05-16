//go:build windows

package process

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLookupProcessIdentity_PowerShellFallback proves the primary
// PowerShell Get-CimInstance path resolves the running test process's
// own PID into a populated ProcessIdentity. On Win11 24H2+ hosts where
// wmic.exe is absent this is the ONLY path; on older Windows it shares
// behavior with the wmic fallback.
//
// This test must remain self-contained: it queries os.Getpid() so it
// does not depend on external processes, manifest state, or scheduler
// fixtures.
func TestLookupProcessIdentity_PowerShellFallback(t *testing.T) {
	pid := os.Getpid()
	id, err := LookupProcessIdentity(pid)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if id.PID != pid {
		t.Fatalf("PID echo mismatch: got %d, want %d", id.PID, pid)
	}
	if !strings.Contains(strings.ToLower(id.Basename), "test") && !strings.Contains(strings.ToLower(id.Basename), ".exe") {
		t.Fatalf("basename suspicious: %q", id.Basename)
	}
	if id.ExecutablePath == "" {
		t.Fatalf("ExecutablePath required for 4-gate check")
	}
}

// TestProbePowerShellCLM_FullLanguagePasses verifies the probe returns
// (true, nil) on a dev host with default PowerShell policy. CLM hosts
// (enterprise WDAC, AppLocker enforced) would return (false, nil) and
// hit the t.Skip — see plan §"Pre-unregister daemon stop" for the
// production fallback (wmic if available, else
// MIGRATION_POWERSHELL_LOCKED abort).
func TestProbePowerShellCLM_FullLanguagePasses(t *testing.T) {
	ok, err := ProbePowerShellCLM()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !ok {
		t.Skip("PowerShell in Constrained Language Mode — unexpected on dev host; skipping")
	}
}

// TestLookupProcessIdentity_NotFound asserts that a PID known not to
// exist (PID 0 is the System Idle Process pseudo-entry on Windows; the
// CIM query filter ProcessId=0 returns an empty result set) maps to
// the ErrProcessNotFound sentinel — NOT a generic error — so callers
// can distinguish "missing" from "CLM-locked + wmic-absent".
func TestLookupProcessIdentity_NotFound(t *testing.T) {
	// 999_999_999 is far above the 32-bit Windows PID ceiling (2^22 in
	// practice on 24H2). CIM returns empty for any unbound PID.
	_, err := LookupProcessIdentity(999_999_999)
	if !errors.Is(err, ErrProcessNotFound) {
		t.Fatalf("want ErrProcessNotFound, got %v", err)
	}
}

// TestLookupProcessIdentity_NegativePID validates input-validation
// runs BEFORE the shell-out — guards a CommandLine where -1 would be
// substituted directly into the PS filter expression. Negative PIDs
// are impossible on Windows; rejecting them up-front protects against
// accidental injection if callers ever pipe untrusted ints.
func TestLookupProcessIdentity_NegativePID(t *testing.T) {
	_, err := LookupProcessIdentity(-1)
	if err == nil {
		t.Fatal("negative PID must produce an error")
	}
	if errors.Is(err, ErrProcessNotFound) {
		t.Fatal("negative PID is invalid input, not 'not found'")
	}
}

// TestLookupProcessIdentity_RetriesOnTransient proves the 3-retry
// loop calls the backend up to 3 times when the first two attempts
// return a transient error, then returns the third attempt's success.
//
// Uses the package-level lookupBackendFn seam to inject a fake — no
// real powershell.exe runs in this test.
func TestLookupProcessIdentity_RetriesOnTransient(t *testing.T) {
	calls := 0
	transient := errors.New("transient: AV scanner stall")
	swapLookupBackend(t, func(pid int) (ProcessIdentity, error) {
		calls++
		if calls < 3 {
			return ProcessIdentity{}, transient
		}
		return ProcessIdentity{
			PID:              pid,
			Basename:         "mcphub.exe",
			CommandLine:      "mcphub.exe daemon --server S --daemon D",
			ExecutablePath:   `C:\tools\mcphub.exe`,
			CreationDateUnix: time.Now().Unix(),
		}, nil
	})

	id, err := LookupProcessIdentity(1234)
	if err != nil {
		t.Fatalf("retry success path: %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 calls (two transient + one success), got %d", calls)
	}
	if id.Basename != "mcphub.exe" {
		t.Fatalf("identity not propagated: %+v", id)
	}
}

// TestLookupProcessIdentity_RetryExhaustion proves that when all 3
// retries return transient errors, the final error is wrapped with
// context that includes the retry count so operators reading logs
// can distinguish "tried 3 times" from "first-shot failure".
func TestLookupProcessIdentity_RetryExhaustion(t *testing.T) {
	calls := 0
	swapLookupBackend(t, func(pid int) (ProcessIdentity, error) {
		calls++
		return ProcessIdentity{}, errors.New("transient: stall")
	})

	_, err := LookupProcessIdentity(1234)
	if err == nil {
		t.Fatal("want error after retry exhaustion, got nil")
	}
	if calls != 3 {
		t.Fatalf("want 3 calls (full retry budget), got %d", calls)
	}
	// Sanity: ErrProcessNotFound is reserved for empty-CIM-result;
	// transient failures must NOT collapse into ErrProcessNotFound.
	if errors.Is(err, ErrProcessNotFound) {
		t.Fatal("transient exhaustion must not surface as ErrProcessNotFound")
	}
}

// TestLookupProcessIdentity_CreationDateUnixIsRecent guards against
// the locale-formatted date trap: if the PowerShell projection
// `[int64](Get-Date $_.CreationDate -UFormat %s)` ever silently
// returns 0 (e.g., parser swallows the timezone), this test catches
// it. The test process started within the last 24h by definition.
func TestLookupProcessIdentity_CreationDateUnixIsRecent(t *testing.T) {
	id, err := LookupProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	now := time.Now().Unix()
	if id.CreationDateUnix <= 0 {
		t.Fatalf("CreationDateUnix=%d (zero or negative — locale parse trap?)", id.CreationDateUnix)
	}
	if now-id.CreationDateUnix > 24*60*60 {
		t.Fatalf("CreationDateUnix=%d is more than 24h ago (now=%d); locale parse trap?", id.CreationDateUnix, now)
	}
	if id.CreationDateUnix > now+60 {
		t.Fatalf("CreationDateUnix=%d is in the future (now=%d)", id.CreationDateUnix, now)
	}
}

// TestLookupProcessIdentity_CommandLineNonEmpty checks that the
// CommandLine field is populated for the test process. CIM populates
// CommandLine when the caller has read permissions on the target
// process; for the test's own process this always succeeds.
func TestLookupProcessIdentity_CommandLineNonEmpty(t *testing.T) {
	id, err := LookupProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if id.CommandLine == "" {
		t.Fatal("CommandLine must be populated for own-process lookup")
	}
}

// swapLookupBackend installs fn as the package-level backend for the
// duration of the test and restores the original on Cleanup. Tests
// that need to exercise the retry loop deterministically MUST go
// through this helper rather than mutating lookupBackendFn directly,
// so a panicking test does not leak the fake into sibling tests.
func swapLookupBackend(t *testing.T, fn func(int) (ProcessIdentity, error)) {
	t.Helper()
	original := lookupBackendFn
	lookupBackendFn = fn
	t.Cleanup(func() {
		lookupBackendFn = original
	})
}

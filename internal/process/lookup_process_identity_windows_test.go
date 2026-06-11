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
// production fallback (wmic if available, else the caller aborts).
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

// swapRealLookupBackendForDispatchTests rebinds the outer retry loop
// to invoke realLookupBackend directly. The dispatch-policy tests
// below need the realLookupBackend function under test, but the
// outer retry loop in LookupProcessIdentity uses lookupBackendFn so
// production startup can default it to realLookupBackend. We have to
// restore that pointer here so the retry harness exercises the real
// dispatch under test.
func swapRealLookupBackendForDispatchTests(t *testing.T) {
	t.Helper()
	swapLookupBackend(t, realLookupBackend)
}

// swapProbePowerShellCLM installs fn as the package-level CLM probe
// for the duration of the test and restores the original on Cleanup.
// Use it to drive realLookupBackend's (clmAvailable, probeErr) decision
// matrix without shelling out to powershell.exe.
func swapProbePowerShellCLM(t *testing.T, fn func() (bool, error)) {
	t.Helper()
	original := probePowerShellCLMFn
	probePowerShellCLMFn = fn
	t.Cleanup(func() {
		probePowerShellCLMFn = original
	})
}

// swapLookupViaPowerShell installs fn as the package-level PowerShell
// terminal-path backend for the duration of the test.
func swapLookupViaPowerShell(t *testing.T, fn func(int) (ProcessIdentity, error)) {
	t.Helper()
	original := lookupViaPowerShellFn
	lookupViaPowerShellFn = fn
	t.Cleanup(func() {
		lookupViaPowerShellFn = original
	})
}

// swapLookupViaWmic installs fn as the package-level wmic terminal-path
// backend for the duration of the test.
func swapLookupViaWmic(t *testing.T, fn func(int) (ProcessIdentity, error)) {
	t.Helper()
	original := lookupViaWmicFn
	lookupViaWmicFn = fn
	t.Cleanup(func() {
		lookupViaWmicFn = original
	})
}

// swapWmicPathLookup installs fn as the package-level wmic.exe PATH
// probe for the duration of the test. fn returns nil to simulate
// "wmic present", or an error to simulate "wmic absent".
func swapWmicPathLookup(t *testing.T, fn func() error) {
	t.Helper()
	original := wmicPathLookupFn
	wmicPathLookupFn = fn
	t.Cleanup(func() {
		wmicPathLookupFn = original
	})
}

// TestLookupProcessIdentity_CLMAvailablePathStaysOnPowerShellOnTransient
// proves the dispatch policy keeps PowerShell as the canonical path
// when CLM is available, even if PowerShell's first attempts return
// transient errors. The outer retry loop must drive PS up to 3 times;
// wmic must NOT fire as a silent fallback because PS is healthy
// (FullLanguage) — a transient stall is the retry loop's job, not
// wmic's. (codex-r2-b-p1, round-1 remains.)
func TestLookupProcessIdentity_CLMAvailablePathStaysOnPowerShellOnTransient(t *testing.T) {
	swapRealLookupBackendForDispatchTests(t)
	swapProbePowerShellCLM(t, func() (bool, error) {
		return true, nil
	})
	psCalls := 0
	transient := errors.New("transient: AV scanner stall")
	swapLookupViaPowerShell(t, func(pid int) (ProcessIdentity, error) {
		psCalls++
		if psCalls < 3 {
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
	wmicCalls := 0
	swapLookupViaWmic(t, func(pid int) (ProcessIdentity, error) {
		wmicCalls++
		return ProcessIdentity{}, errors.New("wmic must not be invoked when CLM is available")
	})
	// Make wmic appear "present" so any accidental dispatch to it
	// would proceed (and fail this test loudly via the fake above);
	// the policy guarantee is that this PATH probe is never even
	// consulted when CLM is available.
	swapWmicPathLookup(t, func() error {
		return nil
	})

	id, err := LookupProcessIdentity(1234)
	if err != nil {
		t.Fatalf("PowerShell retry success path: %v", err)
	}
	if psCalls != 3 {
		t.Fatalf("want 3 PowerShell calls (two transient + one success), got %d", psCalls)
	}
	if wmicCalls != 0 {
		t.Fatalf("want 0 wmic calls (CLM available means PS is canonical), got %d", wmicCalls)
	}
	if id.Basename != "mcphub.exe" {
		t.Fatalf("identity not propagated from PS: %+v", id)
	}
}

// TestLookupProcessIdentity_CLMLockedRoutesToWmic proves the dispatch
// policy chooses the wmic terminal path when CLM is locked and
// wmic.exe is present on PATH. PowerShell must NOT be invoked on
// CLM-locked hosts because the CIM cmdlet itself is blocked.
// (codex-r2-b-p1, round-1 remains.)
func TestLookupProcessIdentity_CLMLockedRoutesToWmic(t *testing.T) {
	swapRealLookupBackendForDispatchTests(t)
	swapProbePowerShellCLM(t, func() (bool, error) {
		return false, nil
	})
	psCalls := 0
	swapLookupViaPowerShell(t, func(pid int) (ProcessIdentity, error) {
		psCalls++
		return ProcessIdentity{}, errors.New("PS must not be invoked when CLM is locked")
	})
	wmicCalls := 0
	swapLookupViaWmic(t, func(pid int) (ProcessIdentity, error) {
		wmicCalls++
		return ProcessIdentity{
			PID:              pid,
			Basename:         "mcphub.exe",
			CommandLine:      "mcphub.exe daemon --server S --daemon D",
			ExecutablePath:   `C:\tools\mcphub.exe`,
			CreationDateUnix: time.Now().Unix(),
		}, nil
	})
	swapWmicPathLookup(t, func() error {
		return nil // wmic present
	})

	id, err := LookupProcessIdentity(1234)
	if err != nil {
		t.Fatalf("wmic terminal path: %v", err)
	}
	if psCalls != 0 {
		t.Fatalf("want 0 PowerShell calls (CLM locked), got %d", psCalls)
	}
	if wmicCalls != 1 {
		t.Fatalf("want 1 wmic call, got %d", wmicCalls)
	}
	if id.Basename != "mcphub.exe" {
		t.Fatalf("identity not propagated from wmic: %+v", id)
	}
}

// TestLookupProcessIdentity_CLMLockedNoWmicReturnsError proves the
// dispatch policy returns a clear error mentioning BOTH conditions
// (CLM-locked AND wmic-absent) when neither path is available. The
// operator needs both names to decide whether to relax the security
// policy or install wmic. (codex-r2-b-p1, round-1 remains.)
func TestLookupProcessIdentity_CLMLockedNoWmicReturnsError(t *testing.T) {
	swapRealLookupBackendForDispatchTests(t)
	swapProbePowerShellCLM(t, func() (bool, error) {
		return false, nil
	})
	psCalls := 0
	swapLookupViaPowerShell(t, func(pid int) (ProcessIdentity, error) {
		psCalls++
		return ProcessIdentity{}, errors.New("PS must not be invoked when CLM is locked")
	})
	wmicCalls := 0
	swapLookupViaWmic(t, func(pid int) (ProcessIdentity, error) {
		wmicCalls++
		return ProcessIdentity{}, errors.New("wmic must not be invoked when wmic is absent")
	})
	swapWmicPathLookup(t, func() error {
		return errors.New("exec: \"wmic.exe\": executable file not found in %PATH%")
	})

	_, err := LookupProcessIdentity(1234)
	if err == nil {
		t.Fatal("want error when CLM locked and wmic absent, got nil")
	}
	if psCalls != 0 {
		t.Fatalf("want 0 PowerShell calls, got %d", psCalls)
	}
	if wmicCalls != 0 {
		t.Fatalf("want 0 wmic calls, got %d", wmicCalls)
	}
	msg := err.Error()
	// The retry loop wraps the per-attempt error with attempt-count
	// context; the underlying realLookupBackend error must still name
	// both conditions so operators reading logs can act.
	if !strings.Contains(msg, "CLM-locked") {
		t.Fatalf("error must name CLM-locked condition: %v", err)
	}
	if !strings.Contains(strings.ToLower(msg), "wmic") {
		t.Fatalf("error must name wmic condition: %v", err)
	}
	// Must NOT collapse into ErrProcessNotFound — this is a routing
	// defect, not a missing-PID signal.
	if errors.Is(err, ErrProcessNotFound) {
		t.Fatalf("policy error must not surface as ErrProcessNotFound: %v", err)
	}
}

// TestLookupProcessIdentity_ProbeErrorIsFatalNotFallback proves the
// dispatch policy surfaces probe-transport failures as errors rather
// than silently downgrading to wmic. A failed probe is an
// environmental defect (powershell.exe missing, CIM shell-out hung)
// that the operator must see — not a green-light for the weaker
// fallback path. (codex-r2-b-p1, round-1 remains.)
func TestLookupProcessIdentity_ProbeErrorIsFatalNotFallback(t *testing.T) {
	swapRealLookupBackendForDispatchTests(t)
	probeErr := errors.New("transport: powershell.exe not on PATH")
	swapProbePowerShellCLM(t, func() (bool, error) {
		return false, probeErr
	})
	psCalls := 0
	swapLookupViaPowerShell(t, func(pid int) (ProcessIdentity, error) {
		psCalls++
		return ProcessIdentity{}, errors.New("PS must not be invoked when probe failed")
	})
	wmicCalls := 0
	swapLookupViaWmic(t, func(pid int) (ProcessIdentity, error) {
		wmicCalls++
		return ProcessIdentity{}, errors.New("wmic must not be invoked when probe failed")
	})
	// Make wmic appear "present" so any accidental silent fallback
	// would fire (and fail this test); the policy guarantee is that
	// a probe error never even reaches the wmic path.
	swapWmicPathLookup(t, func() error {
		return nil
	})

	_, err := LookupProcessIdentity(1234)
	if err == nil {
		t.Fatal("want probe-error propagation, got nil")
	}
	if psCalls != 0 {
		t.Fatalf("want 0 PowerShell calls when probe failed, got %d", psCalls)
	}
	if wmicCalls != 0 {
		t.Fatalf("want 0 wmic calls when probe failed (no silent fallback), got %d", wmicCalls)
	}
	// The probe-transport error must propagate through realLookupBackend
	// (wrapped with "probe failed") and through the outer retry loop
	// (wrapped with attempt-count context). Either way, the operator-
	// facing message must mention the probe defect.
	msg := err.Error()
	if !strings.Contains(strings.ToLower(msg), "probe") {
		t.Fatalf("error must name probe failure: %v", err)
	}
	// Sanity: probe-transport failures must NOT collapse into
	// ErrProcessNotFound.
	if errors.Is(err, ErrProcessNotFound) {
		t.Fatalf("probe error must not surface as ErrProcessNotFound: %v", err)
	}
}

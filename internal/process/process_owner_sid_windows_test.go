//go:build windows

package process

import (
	"testing"

	"golang.org/x/sys/windows"
)

// #301-1 (P2): a PROVEN different-owner SID must surface as (false, nil) — a
// benign identity mismatch the caller safely SKIPs — NOT (false, err).
//
// Before the fix, ProcessOwnerSIDMatchesCurrent's mismatch branch returned
// fmt.Errorf("...does not match..."), so the Windows reaper
// (supervisorPIDIsLiveMcphubSupervisor) treated a stale supervisor.lock sidecar
// pointing at ANOTHER user's mcphub-shaped supervisor as a force-kill FAILURE
// and ABORTED `install --upgrade`, instead of skipping the foreign process.
//
// The seam tests in internal/cli only ever INJECTED (false, nil) via
// reapOwnerSIDMatchesCurrentFn, so they never exercised the production helper's
// actual mismatch return. These tests pin the production comparison core
// (sidsMatchResult) so the contract regression can't slip back in.

// TestSidsMatchResult_ProvenMismatch is the FALSIFYING CORE of #301-1. Two
// distinct, successfully-read SIDs must compare to (false, nil): a proven
// mismatch is benign, not an error. Pre-fix this branch returned a non-nil
// error.
func TestSidsMatchResult_ProvenMismatch(t *testing.T) {
	// Two well-known, always-distinct SIDs. Reading them cannot fail, so the
	// only thing under test is the comparison verdict.
	system, err := windows.StringToSid("S-1-5-18") // LocalSystem
	if err != nil {
		t.Fatalf("StringToSid(LocalSystem): %v", err)
	}
	service, err := windows.StringToSid("S-1-5-19") // LocalService
	if err != nil {
		t.Fatalf("StringToSid(LocalService): %v", err)
	}

	match, mErr := sidsMatchResult(system, service)
	if match {
		t.Fatalf("two distinct SIDs reported as matching; want false")
	}
	if mErr != nil {
		t.Fatalf("#301-1 regression: a PROVEN different-owner SID must return "+
			"(false, nil) (benign mismatch the caller SKIPs), but sidsMatchResult "+
			"returned a non-nil error: %v", mErr)
	}
}

// TestSidsMatchResult_Match pins the positive case: identical SIDs compare to
// (true, nil) so the same-user kill proceeds.
func TestSidsMatchResult_Match(t *testing.T) {
	admin, err := windows.StringToSid("S-1-5-32-544") // BUILTIN\Administrators
	if err != nil {
		t.Fatalf("StringToSid(Administrators): %v", err)
	}
	adminCopy, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatalf("StringToSid(Administrators) copy: %v", err)
	}

	match, mErr := sidsMatchResult(admin, adminCopy)
	if mErr != nil {
		t.Fatalf("equal SIDs must return (true, nil); got err %v", mErr)
	}
	if !match {
		t.Fatalf("equal SIDs must compare as matching; got false")
	}
}

// TestProcessOwnerSIDMatchesCurrent_SelfIsMatch exercises the PRODUCTION helper
// end-to-end against our OWN PID. The current process is trivially owned by the
// current user's SID, so the full OpenProcess → OpenProcessToken → GetTokenUser
// → sidsMatchResult path must yield (true, nil). This proves the production
// wrapper routes a successfully-read same-owner SID through the comparison core
// (rather than, say, erroring on the token read), complementing the
// pure-core proven-mismatch assertion above.
func TestProcessOwnerSIDMatchesCurrent_SelfIsMatch(t *testing.T) {
	self := windows.GetCurrentProcessId()
	match, err := ProcessOwnerSIDMatchesCurrent(int(self))
	if err != nil {
		t.Fatalf("ProcessOwnerSIDMatchesCurrent(self PID %d): unexpected err %v", self, err)
	}
	if !match {
		t.Fatalf("our own process must match our own SID; got (false, nil)")
	}
}

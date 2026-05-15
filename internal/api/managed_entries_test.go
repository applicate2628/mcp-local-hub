package api

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// managedEntriesTestHelper redirects DaemonStateDir() to a
// DACL-hardened scratch dir for the duration of the test. The
// marker file is written through SecureWriteClientConfig, which
// rejects parents that fail the single-user allowlist (a plain
// t.TempDir() under %TEMP% inherits Authenticated Users on
// Windows). The hardenedTempDir helper synthesizes a
// parent dir owner-only + LocalSystem + BuiltinAdministrators
// so the write succeeds.
func managedEntriesTestHelper(t *testing.T) string {
	t.Helper()
	statePathsHelper(t)
	root := hardenedTempDir(t)
	daemonStateRootOverride = root
	return root
}

// TestIsManagedEntry_FalseWhenMarkerMissing verifies the
// fresh-install path: no marker file on disk → IsManagedEntry
// returns (false, nil), not an error. Critical for existing users
// post-upgrade: their entries (installed before PR #187) have no
// marker and must remain fail-closed in demigrate (not silently
// "managed").
func TestIsManagedEntry_FalseWhenMarkerMissing(t *testing.T) {
	managedEntriesTestHelper(t)
	managed, err := IsManagedEntry("claude-code", "memory")
	if err != nil {
		t.Fatalf("IsManagedEntry on fresh state: %v", err)
	}
	if managed {
		t.Errorf("fresh install must report (managed=false); got true")
	}
}

// TestRecordManagedEntry_RoundTrip verifies record → query → forget.
// Three observable invariants:
//
//  1. After RecordManagedEntry, IsManagedEntry returns (true, nil).
//  2. After ForgetManagedEntry, IsManagedEntry returns (false, nil).
//  3. Operations are idempotent — re-recording an existing tuple
//     does not duplicate the row (it refreshes installed_at).
func TestRecordManagedEntry_RoundTrip(t *testing.T) {
	managedEntriesTestHelper(t)

	if err := RecordManagedEntry("claude-code", "memory"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := IsManagedEntry("claude-code", "memory")
	if err != nil {
		t.Fatalf("IsManaged after record: %v", err)
	}
	if !got {
		t.Errorf("IsManaged after Record = false; want true")
	}

	// Re-record — must NOT duplicate the row.
	beforeAt := time.Now().UTC()
	time.Sleep(2 * time.Millisecond) // ensure timestamp can advance on coarse clocks
	if err := RecordManagedEntry("claude-code", "memory"); err != nil {
		t.Fatalf("Re-Record: %v", err)
	}
	m, err := readManagedEntries()
	if err != nil {
		t.Fatalf("readManagedEntries: %v", err)
	}
	count := 0
	for _, e := range m.Entries {
		if e.Client == "claude-code" && e.Server == "memory" {
			count++
			if !e.InstalledAt.After(beforeAt) {
				t.Errorf("re-Record did not refresh InstalledAt: at=%v before=%v", e.InstalledAt, beforeAt)
			}
		}
	}
	if count != 1 {
		t.Errorf("re-Record duplicated row; entries for (claude-code, memory) = %d, want 1", count)
	}

	// Forget — IsManaged must return false.
	if err := ForgetManagedEntry("claude-code", "memory"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	got, err = IsManagedEntry("claude-code", "memory")
	if err != nil {
		t.Fatalf("IsManaged after forget: %v", err)
	}
	if got {
		t.Errorf("IsManaged after Forget = true; want false")
	}
}

// TestRecordManagedEntry_DistinctTuples verifies entries for
// different (client, server) tuples coexist without interference.
func TestRecordManagedEntry_DistinctTuples(t *testing.T) {
	managedEntriesTestHelper(t)

	tuples := []struct{ client, server string }{
		{"claude-code", "memory"},
		{"claude-code", "time"},
		{"gemini-cli", "memory"},
		{"codex-cli", "wolfram"},
	}
	for _, tu := range tuples {
		if err := RecordManagedEntry(tu.client, tu.server); err != nil {
			t.Fatalf("Record %s/%s: %v", tu.client, tu.server, err)
		}
	}
	for _, tu := range tuples {
		got, err := IsManagedEntry(tu.client, tu.server)
		if err != nil {
			t.Fatalf("IsManaged %s/%s: %v", tu.client, tu.server, err)
		}
		if !got {
			t.Errorf("IsManaged %s/%s = false; want true", tu.client, tu.server)
		}
	}
	// A non-listed tuple must remain (false, nil).
	got, err := IsManagedEntry("antigravity", "memory")
	if err != nil {
		t.Fatalf("IsManaged for non-listed: %v", err)
	}
	if got {
		t.Errorf("IsManaged for non-listed (antigravity, memory) = true; want false")
	}
}

// TestRecordManagedEntry_RejectsEmptyArgs pins the input-validation
// contract: empty client or server is a programming error and must
// not silently corrupt the marker file with empty-string keys.
func TestRecordManagedEntry_RejectsEmptyArgs(t *testing.T) {
	managedEntriesTestHelper(t)

	if err := RecordManagedEntry("", "memory"); err == nil {
		t.Errorf("Record with empty client: want error, got nil")
	}
	if err := RecordManagedEntry("claude-code", ""); err == nil {
		t.Errorf("Record with empty server: want error, got nil")
	}
	// IsManagedEntry rejects same.
	if _, err := IsManagedEntry("", "memory"); err == nil {
		t.Errorf("IsManaged with empty client: want error, got nil")
	}
	if _, err := IsManagedEntry("claude-code", ""); err == nil {
		t.Errorf("IsManaged with empty server: want error, got nil")
	}
	// ForgetManagedEntry rejects same.
	if err := ForgetManagedEntry("", "memory"); err == nil {
		t.Errorf("Forget with empty client: want error, got nil")
	}
}

// TestForgetManagedEntry_AbsentIsNoOp pins idempotent forget — the
// demigrate path calls Forget after RemoveEntry; if the row was
// already absent (e.g. operator-edited config out of band), the
// Forget call must not raise.
func TestForgetManagedEntry_AbsentIsNoOp(t *testing.T) {
	managedEntriesTestHelper(t)

	// File doesn't exist yet — Forget on absent must not error.
	if err := ForgetManagedEntry("claude-code", "memory"); err != nil {
		t.Errorf("Forget on missing marker: %v", err)
	}
	// Record a different tuple, then Forget the original — must not
	// disturb the other row.
	if err := RecordManagedEntry("gemini-cli", "wolfram"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := ForgetManagedEntry("claude-code", "memory"); err != nil {
		t.Errorf("Forget non-existent row when other rows exist: %v", err)
	}
	got, _ := IsManagedEntry("gemini-cli", "wolfram")
	if !got {
		t.Errorf("Forget removed the wrong row; gemini-cli/wolfram is missing")
	}
}

// TestRecordManagedEntry_ConcurrentNoLostUpdate pins codex bot r1
// P2 closure on PR #187: concurrent RecordManagedEntry calls (from
// goroutines simulating cross-process migrate races) must not lose
// any tuples. Without flock, the read-modify-write cycles would
// interleave and the later writer would overwrite the earlier
// update, dropping tuples.
//
// Test fires N concurrent goroutines each recording a distinct
// (client, server) tuple, then asserts the final marker contains
// ALL N tuples. Goroutines in the same process exercise the in-
// process mutex; the test does NOT exercise cross-process flock
// directly (that would need a separate test binary), but the
// in-process mutex + flock are acquired together in
// withManagedEntriesLock so the same critical section is protected
// in both cases.
func TestRecordManagedEntry_ConcurrentNoLostUpdate(t *testing.T) {
	managedEntriesTestHelper(t)

	const N = 20
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			errs <- RecordManagedEntry("claude-code", fmt.Sprintf("server-%02d", i))
		}(i)
	}
	for i := 0; i < N; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Record: %v", err)
		}
	}

	m, err := readManagedEntries()
	if err != nil {
		t.Fatalf("readManagedEntries: %v", err)
	}
	if len(m.Entries) != N {
		names := make([]string, 0, len(m.Entries))
		for _, e := range m.Entries {
			names = append(names, e.Server)
		}
		t.Errorf("expected %d tuples after concurrent Record, got %d: %v", N, len(m.Entries), names)
	}
}

// TestReadManagedEntries_RejectsUnknownSchemaVersion pins forward
// compatibility — a newer marker file (written by a future version
// of mcphub) must be refused, not silently treated as v1.
func TestReadManagedEntries_RejectsUnknownSchemaVersion(t *testing.T) {
	managedEntriesTestHelper(t)

	// Write a future-version file directly.
	future := []byte(`{"version":99,"entries":[]}`)
	if err := writeHubMcpStateFile(managedEntriesFileLeaf, future); err != nil {
		t.Fatalf("seed future-version marker: %v", err)
	}
	if _, err := readManagedEntries(); err == nil {
		t.Errorf("readManagedEntries on future schema: want error, got nil")
	} else if !strings.Contains(err.Error(), "schema version 99") {
		t.Errorf("error must name the offending version; got %v", err)
	}
}

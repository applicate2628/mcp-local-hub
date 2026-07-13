package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// P3-1 — the reserved ".lease" manifest-name suffix. adoptSnapshotDir("foo.lease")
// and adoptManifestLeasePath("foo") both resolve <state>/adopt-provenance/foo.lease,
// so a ".lease"-suffixed manifest's snapshot dir COLLIDES with another manifest's
// held lease file. Both path owners must refuse the suffix (arch C1), fail-closing at
// tryAcquireAdoptManifestLease with ZERO side effects.
// ---------------------------------------------------------------------------

// Test (5) — a full adopt of a ".lease"-suffixed manifest is refused fail-closed with
// ZERO provenance-path side effects, while a concurrently-HELD "foo" lease file
// survives. Includes the NON-VACUITY proof: the snapshot-dir path that
// removeAdoptSnapshots("foo.lease") WOULD RemoveAll is byte-identical to manifest
// "foo"'s lease-file path (the real collision the guard averts).
func TestAdoptLeaseSuffixManifestRefusedFailClosed(t *testing.T) {
	entry := "foolease"
	codexBody := "[mcp_servers." + entry + "]\ncommand = \"go\"\nargs = [\"version\"]\n"
	_, _, stateRoot := setupAdoptTestEnv(t, entry, codexBody)

	// NON-VACUITY — PROVE the collision the guard averts. Pre-guard, adoptSnapshotDir
	// builds <state>/adopt-provenance/<manifest>; for manifest "foo.lease" that is
	// byte-identical to manifest "foo"'s LEASE file path. Without the guard,
	// removeAdoptSnapshots("foo.lease")'s os.RemoveAll would unlink a HELD "foo" lease
	// → split-lease → the reap classifier's dead-owner precondition is defeated → a
	// live committed "foo" row can be reaped (P1 data loss).
	stateDir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	wouldRemoveAll := filepath.Join(stateDir, adoptProvenanceSnapshotSubdir, "foo.lease")
	fooLeasePath, err := adoptManifestLeasePath("foo")
	if err != nil {
		t.Fatalf("adoptManifestLeasePath(\"foo\"): %v", err)
	}
	if wouldRemoveAll != fooLeasePath {
		t.Fatalf("collision precondition not met (test would be vacuous): removeAll target %q != foo lease %q", wouldRemoveAll, fooLeasePath)
	}

	// Victim: manifest "foo" holds its lease (creates + flocks the lease file).
	lkFoo, ok, err := tryAcquireAdoptManifestLease("foo")
	if err != nil || !ok {
		t.Fatalf("hold foo lease: ok=%v err=%v", ok, err)
	}
	defer func() { _ = lkFoo.Unlock() }()
	if st, sErr := os.Stat(fooLeasePath); sErr != nil || st.IsDir() {
		t.Fatalf("foo lease file must exist as a regular file: stat err=%v isDir=%v", sErr, st != nil && st.IsDir())
	}

	// The exact fix point: acquiring the lease for a ".lease"-suffixed manifest fails
	// CLOSED (returns an error, not the ordinary already-held (nil,false,nil)).
	if lk2, leased, lErr := tryAcquireAdoptManifestLease("foo.lease"); lErr == nil {
		if lk2 != nil {
			_ = lk2.Unlock()
		}
		t.Errorf("tryAcquireAdoptManifestLease(\"foo.lease\") must fail closed on the reserved suffix; got leased=%v err=nil", leased)
	}

	// Full adopt of a "foo.lease" manifest is refused at step 0b with zero side effects.
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: "foo.lease", Port: port})
	if err != nil {
		// A refusal at plan build is also fail-closed (nothing to execute); the
		// zero-side-effect assertions below still hold.
		t.Logf("BuildAdoptPlan refused foo.lease at plan build (also fail-closed): %v", err)
	} else if execErr := NewAPI().ExecuteAdopt(plan, nil); execErr == nil {
		t.Fatal("adopt of a \"foo.lease\" manifest must be refused")
	} else if !strings.Contains(execErr.Error(), "lease") {
		t.Errorf("adopt refusal should surface the lease-acquire failure: %v", execErr)
	}

	// Zero provenance-path side effects: no adopted-entries row for foo.lease, and the
	// victim "foo" lease file is untouched (still a file, never RemoveAll'd into a dir).
	if _, found, _ := ReadAdoptProvenance("foo.lease"); found {
		t.Errorf("a refused foo.lease adopt must leave NO adopted-entries row")
	}
	if st, sErr := os.Stat(fooLeasePath); sErr != nil || st.IsDir() {
		t.Errorf("held foo lease file was disturbed by the refused foo.lease adopt: stat err=%v isDir=%v", sErr, st != nil && st.IsDir())
	}
	// The event log lives under the state root; a refused adopt writes no provenance
	// capture there, but that is asserted indirectly by the no-row check above.
	_ = stateRoot
}

// Test (6) — adoptSnapshotDir("x.lease") and adoptManifestLeasePath("x.lease") both
// error, and NOTHING is created under adopt-provenance/ (both reject BEFORE any
// mkdir/write side effect). A non-".lease" name still resolves (suffix-scoped guard,
// not a blanket refusal).
func TestAdoptSnapshotAndLeasePathsRejectLeaseSuffix(t *testing.T) {
	_, _, stateRoot := setupAdoptTestEnv(t, "unused", "[mcp_servers]\n")

	if _, err := adoptSnapshotDir("x.lease"); err == nil {
		t.Errorf("adoptSnapshotDir(\"x.lease\") must error on the reserved suffix")
	}
	if _, err := adoptManifestLeasePath("x.lease"); err == nil {
		t.Errorf("adoptManifestLeasePath(\"x.lease\") must error on the reserved suffix")
	}

	// Neither rejected call may have created the provenance parent (guards precede the
	// MkdirAll in adoptManifestLeasePath).
	provDir := filepath.Join(stateRoot, adoptProvenanceSnapshotSubdir)
	if _, err := os.Stat(provDir); !os.IsNotExist(err) {
		t.Errorf("adopt-provenance/ must NOT exist after rejected .lease paths; stat err=%v", err)
	}

	// Positive control — the guard is suffix-scoped: a benign name still resolves.
	if _, err := adoptSnapshotDir("xlease"); err != nil {
		t.Errorf("adoptSnapshotDir(\"xlease\") must succeed (only the .lease suffix is reserved): %v", err)
	}
	if _, err := adoptManifestLeasePath("normalname"); err != nil {
		t.Errorf("adoptManifestLeasePath(\"normalname\") must succeed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// P3-3 — a GC reap/removeSnapshots failure is no longer SILENT; it emits
// adopt-provenance-reap-failed{phase} so a stuck secret-bearing orphan is
// operator-visible.
// ---------------------------------------------------------------------------

// Test (7) — a Phase-2 row-reap failure emits adopt-provenance-reap-failed
// {phase:"gc-row"}. Injected via the reapAdoptProvenanceRowFn seam. NON-VACUITY:
// while the reap fails, orphan-reaped is NOT emitted; once the seam is restored the
// SAME candidate reaps for real and orphan-reaped fires — proving the reap-failed
// emit is conditional on the failure (and that the else branch, previously silent,
// is what fires it).
func TestAdoptGcPhase2ReapFailureEmitsReapFailed(t *testing.T) {
	entry := "gcreapfail"
	codexBody := "[mcp_servers." + entry + "]\ncommand = \"go\"\nargs = [\"version\"]\n"
	codexPath, _, stateRoot := setupAdoptTestEnv(t, entry, codexBody)
	liveBytes, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	sha := ManifestHashContent(liveBytes)

	// Aged CRASH_REAP orphan: manifest absent + byte-frozen config => passes the
	// classifier AND both destructive-safety gates, so the reap is actually attempted.
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:    entry,
		SourceEntryName: entry,
		AdoptClients:    []string{"codex-cli"},
		OperationState:  AdoptOperationStateAdopting,
		UpdatedAt:       time.Now().Add(-2 * time.Hour).UTC(),
		Clients: []AdoptClientProvenance{{
			Client: "codex-cli", OriginalState: AdoptOriginalStatePresent,
			SnapshotRef: "adopt-provenance/" + entry + "/codex-cli.snapshot", SnapshotSHA256: sha,
		}},
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatal(err)
	}
	d, _ := adoptSnapshotDir(entry)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "codex-cli.snapshot"), liveBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(stateRoot, SupervisorEventLogFileLeaf)

	// (a) Force the Phase-2 reap to fail.
	orig := reapAdoptProvenanceRowFn
	reapAdoptProvenanceRowFn = func(string, AdoptOperationState, time.Time) error {
		return fmt.Errorf("induced reap failure")
	}
	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (the reap failed)", reaped)
	}
	ev, _ := findSupervisorEventByName(t, logPath, "adopt-provenance-reap-failed")
	if ev == nil {
		t.Fatal("no adopt-provenance-reap-failed event on a Phase-2 reap failure")
	}
	if ev["severity"] != SupervisorEventSeverityWarn {
		t.Errorf("reap-failed severity = %v, want warn", ev["severity"])
	}
	if ev["source"] != adoptProvenanceEventSource {
		t.Errorf("reap-failed source = %v, want %q", ev["source"], adoptProvenanceEventSource)
	}
	body, _ := ev["body"].(map[string]any)
	if body == nil || body["manifest"] != entry {
		t.Errorf("reap-failed body manifest = %v, want %q", body["manifest"], entry)
	}
	if body["phase"] != adoptReapFailPhaseRow {
		t.Errorf("reap-failed body phase = %v, want %q", body["phase"], adoptReapFailPhaseRow)
	}
	if reason, ok := body["reason"].(string); !ok || reason == "" {
		t.Errorf("reap-failed body reason = %v, want a non-empty string", body["reason"])
	}
	// NON-VACUITY (part 1): the row is NOT reaped and orphan-reaped is NOT emitted while
	// the reap fails.
	if orphan, _ := findSupervisorEventByName(t, logPath, "adopt-provenance-orphan-reaped"); orphan != nil {
		t.Error("orphan-reaped must NOT fire when the reap failed")
	}
	if _, found, _ := ReadAdoptProvenance(entry); !found {
		t.Error("row must survive a failed reap")
	}

	// (b) NON-VACUITY (part 2): restore the real reap; the SAME candidate now reaps and
	// orphan-reaped fires — proving reap-failed was specifically the injected failure.
	reapAdoptProvenanceRowFn = orig
	reaped2, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc (restored): %v", err)
	}
	if reaped2 != 1 {
		t.Errorf("restored reaped = %d, want 1 (the candidate is a genuine crash orphan)", reaped2)
	}
	if orphan, _ := findSupervisorEventByName(t, logPath, "adopt-provenance-orphan-reaped"); orphan == nil {
		t.Error("orphan-reaped must fire once the reap succeeds")
	}
}

// Test (8) — a Phase-3 rowless-dir removeSnapshots failure emits
// adopt-provenance-reap-failed{phase:"gc-rowless-dir"}. Injected via the
// gcRemoveRowlessSnapshotsFn seam. NON-VACUITY: restoring the seam removes the same
// rowless dir for real and emits orphan-reaped.
func TestAdoptGcPhase3RowlessReapFailureEmitsReapFailed(t *testing.T) {
	stateRoot := isolateStateDir(t)
	m := "rowlessreapfail"
	d, _ := adoptSnapshotDir(m)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "codex-cli.snapshot"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(stateRoot, SupervisorEventLogFileLeaf)

	// (a) Force the Phase-3 rowless removal to fail.
	orig := gcRemoveRowlessSnapshotsFn
	gcRemoveRowlessSnapshotsFn = func(string) error { return fmt.Errorf("induced rowless removal failure") }
	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (the rowless removal failed)", reaped)
	}
	ev, _ := findSupervisorEventByName(t, logPath, "adopt-provenance-reap-failed")
	if ev == nil {
		t.Fatal("no adopt-provenance-reap-failed event on a Phase-3 removeSnapshots failure")
	}
	body, _ := ev["body"].(map[string]any)
	if body == nil || body["manifest"] != m {
		t.Errorf("reap-failed body manifest = %v, want %q", body["manifest"], m)
	}
	if body["phase"] != adoptReapFailPhaseRowlessDir {
		t.Errorf("reap-failed body phase = %v, want %q", body["phase"], adoptReapFailPhaseRowlessDir)
	}
	if _, statErr := os.Stat(d); statErr != nil {
		t.Errorf("rowless dir must survive a failed removal: %v", statErr)
	}
	if orphan, _ := findSupervisorEventByName(t, logPath, "adopt-provenance-orphan-reaped"); orphan != nil {
		t.Error("orphan-reaped must NOT fire when the rowless removal failed")
	}

	// (b) NON-VACUITY: restore the real removal; the same rowless dir reaps and
	// orphan-reaped fires.
	gcRemoveRowlessSnapshotsFn = orig
	reaped2, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc (restored): %v", err)
	}
	if reaped2 != 1 {
		t.Errorf("restored reaped = %d, want 1", reaped2)
	}
	if _, statErr := os.Stat(d); !os.IsNotExist(statErr) {
		t.Errorf("rowless dir must be removed once the removal succeeds: %v", statErr)
	}
	if orphan, _ := findSupervisorEventByName(t, logPath, "adopt-provenance-orphan-reaped"); orphan == nil {
		t.Error("orphan-reaped must fire once the removal succeeds")
	}
}

// Test (9) — the reap-failed audit body carries NAMES/PATHS/COUNTS only and NEVER the
// secret VALUE embedded in the snapshot file, even when the failure reason carries the
// snapshot's filesystem PATH (the realistic removeAdoptSnapshots error shape). The
// path-bearing reason is deliberately non-empty so the redaction assertion is not
// vacuous.
func TestAdoptGcReapFailedEventRedactsSecret(t *testing.T) {
	stateRoot := isolateStateDir(t)
	m := "redactreapfail"
	secret := "SECRET-TOKEN-sk-live-abc123-DO-NOT-LEAK"
	d, _ := adoptSnapshotDir(m)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "codex-cli.snapshot"), []byte(`{"env":{"API_KEY":"`+secret+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Fail with the realistic removeAdoptSnapshots error shape: it names the manifest +
	// the dir PATH, never the file CONTENT.
	orig := gcRemoveRowlessSnapshotsFn
	gcRemoveRowlessSnapshotsFn = func(name string) error {
		dir, _ := adoptSnapshotDir(name)
		return fmt.Errorf("remove adopt snapshots %s: unlinkat %s: device or resource busy", name, dir)
	}
	t.Cleanup(func() { gcRemoveRowlessSnapshotsFn = orig })

	if _, err := gcOrphanedAdoptingProvenance(1 * time.Hour); err != nil {
		t.Fatalf("gc: %v", err)
	}
	logPath := filepath.Join(stateRoot, SupervisorEventLogFileLeaf)
	ev, raw := findSupervisorEventByName(t, logPath, "adopt-provenance-reap-failed")
	if ev == nil {
		t.Fatal("no adopt-provenance-reap-failed event")
	}
	// The secret VALUE must not appear anywhere in the serialized event line.
	if strings.Contains(raw, secret) {
		t.Errorf("secret leaked into the reap-failed event line: %s", raw)
	}
	body, _ := ev["body"].(map[string]any)
	if body == nil {
		t.Fatal("reap-failed event has no body")
	}
	for k, v := range body {
		if s, ok := v.(string); ok && strings.Contains(s, secret) {
			t.Errorf("secret leaked into reap-failed body field %q: %q", k, s)
		}
	}
	// Non-vacuity: the reason IS populated (carries the path), so the redaction check
	// above is meaningful rather than trivially passing on an empty body.
	if reason, _ := body["reason"].(string); reason == "" {
		t.Error("reason is empty; the redaction assertion would be vacuous")
	} else if !strings.Contains(reason, m) {
		t.Errorf("reason should carry the path/class detail (manifest %q); got %q", m, reason)
	}
}

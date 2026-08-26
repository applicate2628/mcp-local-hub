package api

import (
	"encoding/json"
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

// Test (5) — a ".lease"-suffixed manifest is refused fail-closed at adopt step 0b
// (tryAcquireAdoptManifestLease, the exact fix point) with the reserved-suffix error,
// while a concurrently-HELD "foo" lease file survives. Asserts the fix point DIRECTLY
// rather than through ExecuteAdopt/BuildAdoptPlan, because adopt-v1 requires
// --name == entry name, so a plan with ManifestName="foo.lease" is rejected at plan
// build BEFORE the guard runs — making a full-adopt assertion VACUOUS (a regression in
// the lease guard would still pass). Includes the NON-VACUITY collision proof: the
// snapshot-dir path removeAdoptSnapshots("foo.lease") WOULD RemoveAll is byte-identical
// to manifest "foo"'s lease-file path. Neuter the guard => the direct call returns
// (lock,true,nil) => this test fails (confirmed via neuter→fail→restore).
func TestAdoptLeaseSuffixManifestRefusedFailClosed(t *testing.T) {
	_, _, stateRoot := setupAdoptTestEnv(t, "unused", "[mcp_servers]\n")

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

	// THE FIX POINT (adopt step 0b): acquiring the lease for a ".lease"-suffixed manifest
	// fails CLOSED with the reserved-suffix error — never the ordinary already-held
	// (nil,false,nil). ExecuteAdoptWithOpts' step 0b does exactly this, so a full adopt of
	// such a manifest is refused before any mutation.
	lk2, leased, lErr := tryAcquireAdoptManifestLease("foo.lease")
	if lk2 != nil {
		_ = lk2.Unlock()
	}
	if lErr == nil {
		t.Fatalf("tryAcquireAdoptManifestLease(\"foo.lease\") must fail closed on the reserved suffix; got leased=%v err=nil", leased)
	}
	if !strings.Contains(lErr.Error(), "reserved") || !strings.Contains(lErr.Error(), adoptManifestLeaseSuffix) {
		t.Errorf("lease-acquire error must name the reserved %q suffix (proving it is the guard, not an unrelated failure); got %v", adoptManifestLeaseSuffix, lErr)
	}

	// Zero provenance-path side effects: no adopted-entries row for foo.lease, and the
	// victim "foo" lease file is untouched (still a file, never RemoveAll'd into a dir).
	if _, found, _ := ReadAdoptProvenance("foo.lease"); found {
		t.Errorf("a refused foo.lease adopt must leave NO adopted-entries row")
	}
	if st, sErr := os.Stat(fooLeasePath); sErr != nil || st.IsDir() {
		t.Errorf("held foo lease file was disturbed: stat err=%v isDir=%v", sErr, st != nil && st.IsDir())
	}
	_ = stateRoot
}

// Test (6) — adoptSnapshotDir("x.lease") and adoptManifestLeasePath("x.lease") both
// error, and NOTHING is created under adopt-provenance/ (both reject BEFORE any
// mkdir/write side effect). A non-".lease" name still resolves (suffix-scoped guard,
// not a blanket refusal).
func TestAdoptSnapshotAndLeasePathsRejectLeaseSuffix(t *testing.T) {
	statePathsHelper(t)
	stateRoot := hardenedTempDir(t)
	daemonStateRootOverride = stateRoot

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
	ref, sha, err := writeAdoptClientSnapshot(entry, "codex-cli", liveBytes)
	if err != nil {
		t.Fatal(err)
	}

	// Aged CRASH_REAP orphan: manifest absent + pinned native entry still present =>
	// passes the classifier AND both destructive-safety gates, so reap is attempted.
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:    entry,
		SourceEntryName: entry,
		AdoptClients:    []string{"codex-cli"},
		OperationState:  AdoptOperationStateAdopting,
		UpdatedAt:       time.Now().Add(-2 * time.Hour).UTC(),
		Clients: []AdoptClientProvenance{{
			Client: "codex-cli", OriginalState: AdoptOriginalStatePresent,
			SnapshotRef: ref, SnapshotSHA256: sha,
		}},
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(stateRoot, SupervisorEventLogFileLeaf)

	// (a) Force the Phase-2 reap to fail. Register the restore IMMEDIATELY via t.Cleanup
	// (F2) so a t.Fatal before the manual mid-test restore below cannot leak the induced
	// seam into the rest of the package run.
	orig := reapAdoptProvenanceRowFn
	t.Cleanup(func() { reapAdoptProvenanceRowFn = orig })
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

	// (a) Force the Phase-3 rowless removal to fail. Register the restore IMMEDIATELY via
	// t.Cleanup (F2) so a t.Fatal before the manual mid-test restore cannot leak the seam.
	orig := gcRemoveRowlessSnapshotsFn
	t.Cleanup(func() { gcRemoveRowlessSnapshotsFn = orig })
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
	// the dir PATH, never the file CONTENT. Restore registered IMMEDIATELY (F2).
	orig := gcRemoveRowlessSnapshotsFn
	t.Cleanup(func() { gcRemoveRowlessSnapshotsFn = orig })
	gcRemoveRowlessSnapshotsFn = func(name string) error {
		dir, _ := adoptSnapshotDir(name)
		return fmt.Errorf("remove adopt snapshots %s: unlinkat %s: device or resource busy", name, dir)
	}

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

// reapFailedManifestsForPhase scans supervisor-events.log for every
// adopt-provenance-reap-failed event with the given phase and returns the set of
// manifest names it names.
func reapFailedManifestsForPhase(t *testing.T, logPath, phase string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}
		}
		t.Fatalf("read %s: %v", logPath, err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("supervisor-events.log line invalid JSON: %v: %q", err, line)
		}
		if ev["event"] != "adopt-provenance-reap-failed" {
			continue
		}
		body, _ := ev["body"].(map[string]any)
		if body == nil || body["phase"] != phase {
			continue
		}
		if name, ok := body["manifest"].(string); ok {
			out[name] = true
		}
	}
	return out
}

// Test (F1) — a legacy ".lease"-suffixed provenance orphan (row-bearing OR rowless,
// left by a pre-P3-1 build that allowed the name) can no longer be reaped because its
// lease path now fails the reserved-suffix guard. The GC must REPORT it
// (adopt-provenance-reap-failed{phase:"gc-lease-path-error"}) instead of silently
// skipping, so an operator can remove adopt-provenance/<name> manually. Exercises BOTH
// the Phase-2 (row-bearing) and Phase-3 (rowless-dir) lease-acquire-error branches.
func TestAdoptGcLegacyLeaseNameOrphanEmitsLeasePathError(t *testing.T) {
	stateRoot := isolateStateDir(t)
	stateDir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}

	// Phase-2 path: a row-bearing aged `adopting` orphan named "foo.lease" (writeAdopted-
	// Entries does not validate the name, so a legacy on-disk row is reproducible).
	rowM := "foo.lease"
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:    rowM,
		SourceEntryName: rowM,
		AdoptClients:    []string{"codex-cli"},
		OperationState:  AdoptOperationStateAdopting,
		UpdatedAt:       time.Now().Add(-2 * time.Hour).UTC(),
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatal(err)
	}

	// Phase-3 path: a ROWLESS snapshot dir named "bar.lease" (constructed manually since
	// adoptSnapshotDir now refuses the name — this reproduces a pre-P3-1 residue dir).
	dirM := "bar.lease"
	legacyDir := filepath.Join(stateDir, adoptProvenanceSnapshotSubdir, dirM)
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "codex-cli.snapshot"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (legacy .lease orphans are unreachable, only reported)", reaped)
	}

	logPath := filepath.Join(stateRoot, SupervisorEventLogFileLeaf)
	got := reapFailedManifestsForPhase(t, logPath, adoptReapFailPhaseLeasePathError)
	for _, want := range []string{rowM, dirM} {
		if !got[want] {
			t.Errorf("no gc-lease-path-error reap-failed event for legacy orphan %q (previously a SILENT skip); got %v", want, got)
		}
	}

	// The orphans SURVIVE (the guard prevents any removal); an operator removes them.
	if _, statErr := os.Stat(legacyDir); statErr != nil {
		t.Errorf("rowless legacy dir must survive (unreachable, only reported): %v", statErr)
	}
	if _, found, _ := ReadAdoptProvenance(rowM); !found {
		t.Errorf("row-bearing legacy orphan must survive (unreachable, only reported)")
	}
}

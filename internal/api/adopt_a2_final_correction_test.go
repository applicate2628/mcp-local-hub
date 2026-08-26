package api

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGCUnlockFailureReturnsPartialCountAndStops(t *testing.T) {
	t.Run("skip", func(t *testing.T) {
		stateRoot := isolateStateDir(t)
		seedAgedAdoptingRow(t, "aa-gc-skip", withAdoptRowPort(9441))
		seedAgedAdoptingRow(t, "zz-gc-later", withAdoptRowPort(9442))
		oldExists := adoptManifestExistsFn
		oldUnmutated := adoptRowProvablyUnmutatedFn
		oldUnlock := adoptLeaseUnlockFailureHook
		t.Cleanup(func() {
			adoptManifestExistsFn = oldExists
			adoptRowProvablyUnmutatedFn = oldUnmutated
			adoptLeaseUnlockFailureHook = oldUnlock
		})
		adoptManifestExistsFn = func(string) (bool, error) { return false, nil }
		seen := []string{}
		adoptRowProvablyUnmutatedFn = func(rec AdoptProvenanceRecord) bool {
			seen = append(seen, rec.ManifestName)
			return false
		}
		adoptLeaseUnlockFailureHook = func() error { return errors.New("injected unlock failure") }

		reaped, err := gcOrphanedAdoptingProvenance(time.Hour)
		if reaped != 0 || !hasLeaseFailureID(err, adoptLeaseFailureCleanup) {
			t.Fatalf("gc = (%d, %v), want partial 0 and %s", reaped, err, adoptLeaseFailureCleanup)
		}
		if fmt.Sprint(seen) != "[aa-gc-skip]" {
			t.Fatalf("GC continued after unlock failure: seen=%v", seen)
		}
		assertAdoptGCLeaseFailureReleasedAndEmitted(t, "aa-gc-skip", stateRoot)
	})

	t.Run("reap", func(t *testing.T) {
		stateRoot := isolateStateDir(t)
		seedAgedAdoptingRow(t, "aa-gc-reap", withAdoptRowPort(9443))
		seedAgedAdoptingRow(t, "zz-gc-later", withAdoptRowPort(9444))
		oldExists := adoptManifestExistsFn
		oldUnmutated := adoptRowProvablyUnmutatedFn
		oldReap := reapAdoptProvenanceRowFn
		oldUnlock := adoptLeaseUnlockFailureHook
		t.Cleanup(func() {
			adoptManifestExistsFn = oldExists
			adoptRowProvablyUnmutatedFn = oldUnmutated
			reapAdoptProvenanceRowFn = oldReap
			adoptLeaseUnlockFailureHook = oldUnlock
		})
		adoptManifestExistsFn = func(string) (bool, error) { return false, nil }
		adoptRowProvablyUnmutatedFn = func(AdoptProvenanceRecord) bool { return true }
		reapedNames := []string{}
		reapAdoptProvenanceRowFn = func(name string, _ AdoptOperationState, _ time.Time) error {
			reapedNames = append(reapedNames, name)
			return nil
		}
		adoptLeaseUnlockFailureHook = func() error { return errors.New("injected unlock failure") }

		reaped, err := gcOrphanedAdoptingProvenance(time.Hour)
		if reaped != 1 || !hasLeaseFailureID(err, adoptLeaseFailureCleanup) {
			t.Fatalf("gc = (%d, %v), want partial 1 and %s", reaped, err, adoptLeaseFailureCleanup)
		}
		if fmt.Sprint(reapedNames) != "[aa-gc-reap]" {
			t.Fatalf("GC continued after post-reap unlock failure: reaped=%v", reapedNames)
		}
		assertAdoptGCLeaseFailureReleasedAndEmitted(t, "aa-gc-reap", stateRoot)
	})

	t.Run("rowless", func(t *testing.T) {
		stateRoot := isolateStateDir(t)
		for _, name := range []string{"aa-rowless", "zz-rowless-later"} {
			dir, err := adoptSnapshotDir(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "codex-cli.snapshot"), []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		oldUnlock := adoptLeaseUnlockFailureHook
		t.Cleanup(func() { adoptLeaseUnlockFailureHook = oldUnlock })
		adoptLeaseUnlockFailureHook = func() error { return errors.New("injected unlock failure") }

		reaped, err := gcOrphanedAdoptingProvenance(time.Hour)
		if reaped != 1 || !hasLeaseFailureID(err, adoptLeaseFailureCleanup) {
			t.Fatalf("gc = (%d, %v), want partial 1 and %s", reaped, err, adoptLeaseFailureCleanup)
		}
		later, _ := adoptSnapshotDir("zz-rowless-later")
		if _, statErr := os.Stat(later); statErr != nil {
			t.Fatalf("later rowless candidate was touched after unlock failure: %v", statErr)
		}
		assertAdoptGCLeaseFailureReleasedAndEmitted(t, "aa-rowless", stateRoot)
	})
}

func assertAdoptGCLeaseFailureReleasedAndEmitted(t *testing.T, manifestName, stateRoot string) {
	t.Helper()
	adoptLeaseUnlockFailureHook = nil
	lease, acquired, err := tryAcquireAdoptManifestLease(manifestName)
	if err != nil || !acquired {
		t.Fatalf("OS lease was not released after cleanup failure: acquired=%v err=%v", acquired, err)
	}
	if err := lease.Unlock(); err != nil {
		t.Fatalf("release verification lease: %v", err)
	}
	events, err := os.ReadFile(filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read lease failure event: %v", err)
	}
	if !bytes.Contains(events, []byte(`"event":"adopt-lease-failed"`)) ||
		!bytes.Contains(events, []byte(`"failure_id":"E_ADOPT_LEASE_CLEANUP"`)) {
		t.Fatalf("missing safe adopt-lease-failed event: %s", events)
	}
}

func TestAdoptReceiverRejectsParseableManifestHashDrift(t *testing.T) {
	plan, rec, _, manifestRoot, _ := prepareAppliedAdoptA2(t, "a2-manifest-drift")
	manifestPath := filepath.Join(manifestRoot, plan.ManifestName, "manifest.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(raw, []byte("\n# parseable drift\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	err = verifyAdoptReceivers(NewAPI(), plan, rec)
	assertAdoptStage(t, err, "manifest-verify")
}

func TestAdoptFreshIntentHashMatchesManifestAndProvenance(t *testing.T) {
	entry := "a2-fresh-hash"
	_, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, fmt.Sprintf("[mcp_servers.%s]\ncommand = \"go\"\nargs = [\"version\"]\n", entry))
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()), Clients: []string{"codex-cli"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewAPI().ExecuteAdopt(plan, &bytes.Buffer{}); err != nil {
		t.Fatalf("fresh adopt: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(manifestRoot, entry, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := ManifestHashContent(manifest)
	rec, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found || rec.ExpectedManifestHash != want {
		t.Fatalf("provenance hash=%q found=%v err=%v, want %q", rec.ExpectedManifestHash, found, err, want)
	}
	intent, err := ReadSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatal(err)
	}
	if len(intent.Daemons) != 1 || intent.Daemons[0].ManifestHash != want {
		t.Fatalf("intent rows=%#v, want one exact hash %q", intent.Daemons, want)
	}
}

func TestAdoptRepeatRejectsLegacyEmptyIntentHash(t *testing.T) {
	initial, _, codexPath, manifestRoot, stateRoot := prepareAppliedAdoptA2(t, "a2-repeat-empty-intent-hash")
	repeat, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: initial.EntryName, Client: initial.SourceClient, ManifestName: initial.ManifestName})
	if err != nil {
		t.Fatal(err)
	}
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	intent.Daemons[0].ManifestHash = ""
	if err := WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatal(err)
	}
	before := snapshotAdoptA2(t, repeat, codexPath, manifestRoot, stateRoot)
	err = NewAPI().ExecuteAdopt(repeat, &bytes.Buffer{})
	assertExistingStateInconsistent(t, err)
	after := snapshotAdoptA2(t, repeat, codexPath, manifestRoot, stateRoot)
	assertAdoptA2SnapshotEqual(t, before, after)
}

func TestAdoptReceiverRejectsIntentIdentityDrift(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*SupervisorDaemon, string)
		wantErr bool
	}{
		{"server", func(d *SupervisorDaemon, _ string) { d.Server = "other" }, true},
		{"port", func(d *SupervisorDaemon, _ string) { d.Port++ }, true},
		{"hash", func(d *SupervisorDaemon, _ string) { d.ManifestHash = strings.Repeat("0", 64) }, true},
		{"empty-task", func(d *SupervisorDaemon, _ string) { d.TaskName = "" }, true},
		{"noncanonical-task", func(d *SupervisorDaemon, _ string) { d.TaskName = strings.TrimPrefix(d.TaskName, `\`) }, true},
		{"valid-other-task", func(d *SupervisorDaemon, entry string) {
			d.TaskName = canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon(entry, "other"))
		}, true},
		{"exact-task", func(d *SupervisorDaemon, entry string) {
			d.TaskName = canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon(entry, "default"))
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, rec, _, _, stateRoot := prepareAppliedAdoptA2(t, "a2-intent-"+tc.name)
			intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
			intent, err := ReadSupervisorIntent(intentPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(intent.Daemons) == 0 {
				t.Fatal("fixture has no supervisor daemon")
			}
			tc.mutate(&intent.Daemons[0], plan.ManifestName)
			if err := WriteSupervisorIntent(intentPath, intent); err != nil {
				t.Fatal(err)
			}
			err = verifyAdoptReceivers(NewAPI(), plan, rec)
			if tc.wantErr {
				assertAdoptStage(t, err, "intent-task-verify")
			} else if err != nil {
				t.Fatalf("exact task identity rejected: %v", err)
			}
		})
	}
}

func TestAdoptRepeatRejectsRequestedStateDrift(t *testing.T) {
	tests := []struct {
		name string
		opts func(*AdoptPlan) AdoptOpts
	}{
		{"port", func(p *AdoptPlan) AdoptOpts {
			return AdoptOpts{EntryName: p.EntryName, Client: p.SourceClient, ManifestName: p.ManifestName, Port: p.Port + 1}
		}},
		{"clients", func(p *AdoptPlan) AdoptOpts {
			return AdoptOpts{EntryName: p.EntryName, Client: p.SourceClient, ManifestName: p.ManifestName, Clients: []string{"claude-code", "codex-cli"}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			initial, _, codexPath, manifestRoot, stateRoot := prepareAppliedAdoptA2(t, "a2-request-"+tc.name)
			repeat, err := NewAPI().BuildAdoptPlan(tc.opts(initial))
			if err != nil {
				t.Fatalf("BuildAdoptPlan repeat: %v", err)
			}
			before := snapshotAdoptA2(t, repeat, codexPath, manifestRoot, stateRoot)
			var out bytes.Buffer
			err = NewAPI().ExecuteAdopt(repeat, &out)
			assertExistingStateInconsistent(t, err)
			if out.Len() != 0 {
				t.Fatalf("repeat mismatch wrote success output: %q", out.String())
			}
			after := snapshotAdoptA2(t, repeat, codexPath, manifestRoot, stateRoot)
			assertAdoptA2SnapshotEqual(t, before, after)
		})
	}
}

func TestAdoptRepeatRejectsMalformedProvenance(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AdoptProvenanceRecord)
	}{
		{"empty-clients", func(r *AdoptProvenanceRecord) { r.AdoptClients = nil }},
		{"duplicate-clients", func(r *AdoptProvenanceRecord) { r.AdoptClients = []string{"codex-cli", "codex-cli"} }},
		{"source-omitted", func(r *AdoptProvenanceRecord) { r.AdoptClients = []string{"claude-code"} }},
		{"missing-client-record", func(r *AdoptProvenanceRecord) { r.Clients = nil }},
		{"duplicate-client-record", func(r *AdoptProvenanceRecord) { r.Clients = append(r.Clients, r.Clients[0]) }},
		{"target-drift", func(r *AdoptProvenanceRecord) { r.Clients[0].TargetEntryName = "different-target" }},
		{"empty-adopt-hash", func(r *AdoptProvenanceRecord) { r.AdoptManifestHash = "" }},
		{"hash-disagreement", func(r *AdoptProvenanceRecord) { r.ExpectedManifestHash = strings.Repeat("f", 64) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			initial, _, codexPath, manifestRoot, stateRoot := prepareAppliedAdoptA2(t, "a2-malformed-"+tc.name)
			repeat, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: initial.EntryName, Client: initial.SourceClient, ManifestName: initial.ManifestName})
			if err != nil {
				t.Fatalf("BuildAdoptPlan repeat: %v", err)
			}
			mutateAdoptA2Record(t, initial.ManifestName, tc.mutate)
			before := snapshotAdoptA2(t, repeat, codexPath, manifestRoot, stateRoot)
			err = NewAPI().ExecuteAdopt(repeat, &bytes.Buffer{})
			assertExistingStateInconsistent(t, err)
			after := snapshotAdoptA2(t, repeat, codexPath, manifestRoot, stateRoot)
			assertAdoptA2SnapshotEqual(t, before, after)
		})
	}
}

func TestAdoptRepeatExactStateIsByteStableAndReportsAlreadyAdopted(t *testing.T) {
	initial, _, codexPath, manifestRoot, stateRoot := prepareAppliedAdoptA2(t, "a2-repeat-exact")
	repeat, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: initial.EntryName, Client: initial.SourceClient, ManifestName: initial.ManifestName,
		Port: initial.Port, Clients: []string{"codex-cli", "codex-cli"},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan repeat: %v", err)
	}
	before := snapshotAdoptA2(t, repeat, codexPath, manifestRoot, stateRoot)
	var out bytes.Buffer
	if err := NewAPI().ExecuteAdopt(repeat, &out); err != nil {
		t.Fatalf("ExecuteAdopt repeat: %v", err)
	}
	if !strings.Contains(out.String(), "Already adopted logical source") {
		t.Fatalf("repeat output = %q, want Already adopted", out.String())
	}
	after := snapshotAdoptA2(t, repeat, codexPath, manifestRoot, stateRoot)
	assertAdoptA2SnapshotEqual(t, before, after)
}

func prepareAppliedAdoptA2(t *testing.T, entry string) (*AdoptPlan, *AdoptProvenanceRecord, string, string, string) {
	t.Helper()
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, fmt.Sprintf("[mcp_servers.%s]\ncommand = \"go\"\nargs = [\"version\"]\n", entry))
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()), Clients: []string{"codex-cli"}})
	if err != nil {
		t.Fatalf("BuildAdoptPlan initial: %v", err)
	}
	if err := NewAPI().ExecuteAdoptWithOpts(plan, &bytes.Buffer{}, ExecuteAdoptOpts{ReceivingVerifier: func(*API, *AdoptPlan, *AdoptProvenanceRecord) error { return nil }}); err != nil {
		t.Fatalf("ExecuteAdopt initial: %v", err)
	}
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range intent.Daemons {
		if intent.Daemons[i].Server == entry && intent.Daemons[i].Port == plan.Port {
			intent.Daemons[i].ManifestHash = ManifestHashContent([]byte(plan.ManifestYAML))
			found = true
		}
	}
	if !found {
		t.Fatal("initial install did not create supervisor intent row")
	}
	if err := WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := ReadAdoptProvenance(entry)
	if err != nil || !ok {
		t.Fatalf("ReadAdoptProvenance: found=%v err=%v", ok, err)
	}
	if err := verifyAdoptReceivers(NewAPI(), plan, rec); err != nil {
		t.Fatalf("corrected fixture does not satisfy production receiver: %v", err)
	}
	return plan, rec, codexPath, manifestRoot, stateRoot
}

func mutateAdoptA2Record(t *testing.T, manifest string, mutate func(*AdoptProvenanceRecord)) {
	t.Helper()
	if err := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return err
		}
		for i := range store.Records {
			if store.Records[i].ManifestName == manifest {
				mutate(&store.Records[i])
				return writeAdoptedEntries(store)
			}
		}
		return fmt.Errorf("missing record %q", manifest)
	}); err != nil {
		t.Fatal(err)
	}
}

func snapshotAdoptA2(t *testing.T, plan *AdoptPlan, codexPath, manifestRoot, stateRoot string) map[string][]byte {
	t.Helper()
	paths := []string{
		codexPath,
		filepath.Join(manifestRoot, plan.ManifestName, "manifest.yaml"),
		filepath.Join(stateRoot, supervisorIntentFileLeaf),
		filepath.Join(stateRoot, adoptedEntriesFileLeaf),
		filepath.Join(stateRoot, SupervisorEventLogFileLeaf),
	}
	rec, found, err := ReadAdoptProvenance(plan.ManifestName)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		for _, client := range rec.Clients {
			if client.SnapshotRef != "" {
				paths = append(paths, filepath.Join(stateRoot, filepath.FromSlash(client.SnapshotRef)))
			}
		}
	}
	out := make(map[string][]byte, len(paths))
	for _, path := range paths {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				out[path] = nil
				continue
			}
			t.Fatal(readErr)
		}
		out[path] = raw
	}
	return out
}

func assertAdoptA2SnapshotEqual(t *testing.T, before, after map[string][]byte) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("snapshot path count changed: before=%d after=%d", len(before), len(after))
	}
	for path, want := range before {
		if got, ok := after[path]; !ok || !bytes.Equal(got, want) {
			t.Fatalf("repeat mutated %q", path)
		}
	}
}

func assertAdoptStage(t *testing.T, err error, stage string) {
	t.Helper()
	var staged *AdoptStageError
	if !errors.As(err, &staged) || staged.Stage != stage || staged.CommitState != "committed_unverified" {
		t.Fatalf("error=%v stage=%#v, want %s/committed_unverified", err, staged, stage)
	}
}

func assertExistingStateInconsistent(t *testing.T, err error) {
	t.Helper()
	assertAdoptStage(t, err, "existing-state-inconsistent")
	if !strings.HasPrefix(err.Error(), "E_ADOPT_STAGE_EXISTING_STATE_INCONSISTENT") {
		t.Fatalf("error=%q, want stable existing-state-inconsistent code", err)
	}
}

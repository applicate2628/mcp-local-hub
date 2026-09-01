package api

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const profileUpdateFixture = `name: profile-update
kind: global
transport: stdio-bridge
command: go
base_args: [version]
daemons:
  - name: default
    port: 9371
client_bindings: []
weekly_refresh: false
`

func seedAdoptProfileUpdate(t *testing.T) (string, AdoptProvenanceRecord, InstallSupervisorIntentTarget) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
	t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
	path := filepath.Join(root, "profile-update", "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(profileUpdateFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := ManifestHashContent([]byte(profileUpdateFixture))
	rec := AdoptProvenanceRecord{ManifestName: "profile-update", SourceClient: "codex-cli", SourceEntryName: "profile-update", Port: 9371, AdoptClients: []string{"codex-cli"}, AdoptManifestHash: hash, ExpectedManifestHash: hash, OperationState: AdoptOperationStateAdopted, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Clients: []AdoptClientProvenance{{Client: "codex-cli", OriginalState: AdoptOriginalStatePresent}}}
	if err := writeAdoptedEntries(&AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{rec}}); err != nil {
		t.Fatal(err)
	}
	target, _, err := buildInstallSupervisorIntentTarget(rec.ManifestName, []byte(profileUpdateFixture), rec.Port)
	if err != nil {
		t.Fatal(err)
	}
	p, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSupervisorIntent(p, &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{*target.Row}}); err != nil {
		t.Fatal(err)
	}
	return root, rec, target
}

func profileUpdateJournal(t *testing.T, rec AdoptProvenanceRecord, old InstallSupervisorIntentTarget) *AdoptProfileUpdateJournal {
	t.Helper()
	newManifest, err := updateAdoptProfileManifest([]byte(profileUpdateFixture), rec.Port, "stdio-http-legacy-2024-11-05")
	if err != nil {
		t.Fatal(err)
	}
	newTarget, _, err := buildInstallSupervisorIntentTarget(rec.ManifestName, newManifest, rec.Port)
	if err != nil {
		t.Fatal(err)
	}
	return &AdoptProfileUpdateJournal{ManifestName: rec.ManifestName, Profile: "stdio-http-legacy-2024-11-05", OldManifestHash: rec.ExpectedManifestHash, NewManifestHash: ManifestHashContent(newManifest), OldManifestYAML: profileUpdateFixture, NewManifestYAML: string(newManifest), OldTarget: old, NewTarget: newTarget}
}

func writeProfileUpdateJournal(t *testing.T, j *AdoptProfileUpdateJournal) {
	t.Helper()
	if err := withAdoptedEntriesLock(func() error {
		s, err := readAdoptedEntries()
		if err != nil {
			return err
		}
		s.ProtocolProfileUpdates = append(s.ProtocolProfileUpdates, *j)
		return writeAdoptedEntries(s)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptProfileUpdatePlanDryRunHashesAndRestartOnly(t *testing.T) {
	_, rec, _ := seedAdoptProfileUpdate(t)
	plan, err := NewAPI().BuildAdoptProfileUpdatePlan(rec.ManifestName, "stdio-http-legacy-2024-11-05")
	if err != nil || plan.Noop || !plan.RestartRequired || plan.OldManifestHash == plan.NewManifestHash {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
}

func TestAdoptProfileUpdateExecuteIntentOnlyPreservesSibling(t *testing.T) {
	_, rec, old := seedAdoptProfileUpdate(t)
	p, _ := DefaultSupervisorIntentPath()
	intent, _ := ReadSupervisorIntent(p)
	intent.Daemons = append(intent.Daemons, SupervisorDaemon{TaskName: "\\sibling", Server: "sibling", Daemon: "default", Port: 9999})
	if err := WriteSupervisorIntent(p, intent); err != nil {
		t.Fatal(err)
	}
	plan, err := NewAPI().BuildAdoptProfileUpdatePlan(rec.ManifestName, "stdio-http-legacy-2024-11-05")
	if err != nil {
		t.Fatal(err)
	}
	if err := NewAPI().ExecuteAdoptProfileUpdate(plan, AdoptProfileUpdateOpts{}); err != nil {
		t.Fatal(err)
	}
	got, found, _ := ReadAdoptProvenance(rec.ManifestName)
	if !found || got.AdoptManifestHash != rec.AdoptManifestHash || got.ExpectedManifestHash != plan.NewManifestHash || got.MCPProtocolCompatibilityProfile != plan.Profile {
		t.Fatalf("record=%+v", got)
	}
	intent, _ = ReadSupervisorIntent(p)
	if intent.FindSupervisorDaemonByTaskName("\\sibling") == nil {
		t.Fatal("sibling intent row was lost")
	}
	actual, err := readInstallSupervisorIntentTarget(old.Row.TaskName)
	if err != nil || actual.Fingerprint == old.Fingerprint {
		t.Fatalf("target did not refresh: %+v err=%v", actual, err)
	}
}

func TestAdoptProfileUpdateRecoveryNewNewAndThirdTargetConflict(t *testing.T) {
	_, rec, old := seedAdoptProfileUpdate(t)
	j := profileUpdateJournal(t, rec, old)
	writeProfileUpdateJournal(t, j)
	if err := NewAPI().recoverAdoptProfileUpdateUnderLease(rec.ManifestName); err != nil {
		t.Fatalf("old/old forward: %v", err)
	}
	writeProfileUpdateJournal(t, j)
	if err := NewAPI().recoverAdoptProfileUpdateUnderLease(rec.ManifestName); err != nil {
		t.Fatalf("new/new clear: %v", err)
	}
	writeProfileUpdateJournal(t, j)
	p, _ := DefaultSupervisorIntentPath()
	intent, _ := ReadSupervisorIntent(p)
	for i := range intent.Daemons {
		if intent.Daemons[i].TaskName == j.NewTarget.Row.TaskName {
			intent.Daemons[i].Command = "foreign"
		}
	}
	if err := WriteSupervisorIntent(p, intent); err != nil {
		t.Fatal(err)
	}
	err := NewAPI().recoverAdoptProfileUpdateUnderLease(rec.ManifestName)
	var failure *AdoptProfileUpdateFailure
	if !errors.As(err, &failure) || failure.FailureID != AdoptProfileUpdateRecovery {
		t.Fatalf("err=%v failure=%+v", err, failure)
	}
}

func TestAdoptProfileUpdateRecoveryOldNewRestoresTargetBeforeReceipt(t *testing.T) {
	root, rec, old := seedAdoptProfileUpdate(t)
	j := profileUpdateJournal(t, rec, old)
	// Simulate a completed forward update followed by a crash after manifest
	// rollback but before target/provenance restoration.
	if _, err := NewAPI().ManifestEditInWithHash(root, rec.ManifestName, j.NewManifestYAML, j.OldManifestHash); err != nil {
		t.Fatal(err)
	}
	if _, err := materializeInstallSupervisorIntentTarget(j.OldTarget, j.NewTarget, InstallMutationReceiptV1{}); err != nil {
		t.Fatal(err)
	}
	if err := commitAdoptProfileUpdate(rec.ManifestName, &AdoptProfileUpdateJournal{ManifestName: rec.ManifestName, NewManifestHash: j.NewManifestHash, OldManifestHash: j.OldManifestHash, Profile: j.Profile}); err == nil {
		t.Fatal("commit without journal accepted")
	}
	if err := withAdoptedEntriesLock(func() error {
		s, err := readAdoptedEntries()
		if err != nil {
			return err
		}
		s.ProtocolProfileUpdates = append(s.ProtocolProfileUpdates, *j)
		s.Records[0].ExpectedManifestHash = j.NewManifestHash
		s.Records[0].MCPProtocolCompatibilityProfile = j.Profile
		return writeAdoptedEntries(s)
	}); err != nil {
		t.Fatal(err)
	}
	if err := markAdoptProfileUpdateRollback(rec.ManifestName); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rec.ManifestName, "manifest.yaml"), []byte(profileUpdateFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewAPI().recoverAdoptProfileUpdateUnderLease(rec.ManifestName); err != nil {
		t.Fatal(err)
	}
	actual, err := readInstallSupervisorIntentTarget(j.NewTarget.Row.TaskName)
	if err != nil || actual.Fingerprint != j.OldTarget.Fingerprint {
		t.Fatalf("target=%+v err=%v", actual, err)
	}
	got, _, _ := ReadAdoptProvenance(rec.ManifestName)
	if got.ExpectedManifestHash != j.OldManifestHash || got.MCPProtocolCompatibilityProfile != "" {
		t.Fatalf("record=%+v", got)
	}
}

func TestMaterializeSupervisorIntentTargetRejectsClientSettlement(t *testing.T) {
	_, rec, old := seedAdoptProfileUpdate(t)
	j := profileUpdateJournal(t, rec, old)
	receipt, err := materializeInstallSupervisorIntentTarget(old, j.NewTarget, InstallMutationReceiptV1{ClientConfigSettlements: []ClientConfigSettlementV1{{}}})
	if err == nil || len(receipt.ClientConfigSettlements) != 1 || receipt.Committed {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestValidateAdoptRepeatProfileMigratedRecordFlagPresence(t *testing.T) {
	manifest := "name: x\n"
	currentHash := ManifestHashContent([]byte(manifest))
	originalAdoptHash := ManifestHashContent([]byte("name: x\ncompatibility_profile: original\n"))
	if originalAdoptHash == currentHash {
		t.Fatal("fixture does not model a migrated manifest")
	}
	rec := &AdoptProvenanceRecord{ManifestName: "x", SourceClient: "codex-cli", SourceEntryName: "x", Port: 1, AdoptClients: []string{"codex-cli"}, AdoptManifestHash: originalAdoptHash, ExpectedManifestHash: currentHash, MCPProtocolCompatibilityProfile: "stdio-http-legacy-2024-11-05", Clients: []AdoptClientProvenance{{Client: "codex-cli"}}}
	plan := &AdoptPlan{ManifestName: "x", SourceClient: "codex-cli", EntryName: "x", Port: 1, AdoptClients: []string{"codex-cli"}, ManifestYAML: manifest, TargetEntryNames: map[string]string{"codex-cli": "x"}}
	if err := validateAdoptRepeatState(plan, rec); err != nil {
		t.Fatalf("flagless re-adopt: %v", err)
	}
	plan.requestedCompatibilityProfileExplicit = true
	plan.requestedCompatibilityProfile = rec.MCPProtocolCompatibilityProfile
	if err := validateAdoptRepeatState(plan, rec); err != nil {
		t.Fatalf("explicit same profile: %v", err)
	}
	plan.requestedCompatibilityProfile = ""
	if err := validateAdoptRepeatState(plan, rec); err == nil {
		t.Fatal("explicit empty profile accepted")
	}
	plan.requestedCompatibilityProfile = "different"
	if err := validateAdoptRepeatState(plan, rec); err == nil {
		t.Fatal("explicit mismatched profile accepted")
	}
}

func TestAdoptProfileUpdateManifestPlanCASConflictFailsClosed(t *testing.T) {
	root, rec, _ := seedAdoptProfileUpdate(t)
	plan, err := NewAPI().BuildAdoptProfileUpdatePlan(rec.ManifestName, "stdio-http-legacy-2024-11-05")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rec.ManifestName, "manifest.yaml"), []byte(profileUpdateFixture+"# changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = NewAPI().ExecuteAdoptProfileUpdate(plan, AdoptProfileUpdateOpts{})
	var failure *AdoptProfileUpdateFailure
	if !errors.As(err, &failure) || failure.FailureID != AdoptProfileUpdateIdentity {
		t.Fatalf("err=%v failure=%+v", err, failure)
	}
}

type busyProfileUpdateLease struct{}

func (busyProfileUpdateLease) AcquireAdoptLease(string) (AdoptLease, bool, error) {
	return nil, false, nil
}
func TestAdoptProfileUpdateLeaseBusyFailsClosed(t *testing.T) {
	_, rec, _ := seedAdoptProfileUpdate(t)
	plan, _ := NewAPI().BuildAdoptProfileUpdatePlan(rec.ManifestName, "stdio-http-legacy-2024-11-05")
	err := NewAPI().ExecuteAdoptProfileUpdate(plan, AdoptProfileUpdateOpts{LeaseOwner: busyProfileUpdateLease{}})
	var failure *AdoptProfileUpdateFailure
	if !errors.As(err, &failure) || failure.FailureID != AdoptProfileUpdateRecovery {
		t.Fatalf("err=%v failure=%+v", err, failure)
	}
}

func TestAdoptProfileUpdateFailureProjectsOnlyStableID(t *testing.T) {
	err := asAdoptProfileFailure(AdoptProfileUpdateRecovery, "store", &os.PathError{Op: "read", Path: `C:\private\profile-update`, Err: os.ErrPermission})
	if err.Error() != AdoptProfileUpdateRecovery {
		t.Fatalf("public error leaked: %q", err)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal("raw cause was not retained for in-process diagnosis")
	}
}

func TestAdoptProfileUpdateCrashFingerprintRecoveryMatrix(t *testing.T) {
	t.Run("forward-new-old-already-new", func(t *testing.T) {
		root, rec, old := seedAdoptProfileUpdate(t)
		j := profileUpdateJournal(t, rec, old)
		if err := os.WriteFile(filepath.Join(root, rec.ManifestName, "manifest.yaml"), []byte(j.NewManifestYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := materializeInstallSupervisorIntentTarget(j.OldTarget, j.NewTarget, InstallMutationReceiptV1{}); err != nil {
			t.Fatal(err)
		}
		writeProfileUpdateJournal(t, j)
		if err := NewAPI().recoverAdoptProfileUpdateUnderLease(rec.ManifestName); err != nil {
			t.Fatal(err)
		}
		got, _, _ := ReadAdoptProvenance(rec.ManifestName)
		if got.ExpectedManifestHash != j.NewManifestHash {
			t.Fatalf("record=%+v", got)
		}
	})
	t.Run("rollback-old-old-still-new", func(t *testing.T) {
		_, rec, old := seedAdoptProfileUpdate(t)
		j := profileUpdateJournal(t, rec, old)
		j.Rollback = true
		if _, err := materializeInstallSupervisorIntentTarget(j.OldTarget, j.NewTarget, InstallMutationReceiptV1{}); err != nil {
			t.Fatal(err)
		}
		writeProfileUpdateJournal(t, j)
		if err := NewAPI().recoverAdoptProfileUpdateUnderLease(rec.ManifestName); err != nil {
			t.Fatal(err)
		}
		actual, err := readInstallSupervisorIntentTarget(j.OldTarget.Row.TaskName)
		if err != nil || actual.Fingerprint != j.OldTarget.Fingerprint {
			t.Fatalf("target=%+v err=%v", actual, err)
		}
	})
	t.Run("rollback-old-new-already-old", func(t *testing.T) {
		root, rec, old := seedAdoptProfileUpdate(t)
		j := profileUpdateJournal(t, rec, old)
		j.Rollback = true
		if err := withAdoptedEntriesLock(func() error {
			s, err := readAdoptedEntries()
			if err != nil {
				return err
			}
			s.Records[0].ExpectedManifestHash = j.NewManifestHash
			s.Records[0].MCPProtocolCompatibilityProfile = j.Profile
			s.ProtocolProfileUpdates = append(s.ProtocolProfileUpdates, *j)
			return writeAdoptedEntries(s)
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rec.ManifestName, "manifest.yaml"), []byte(j.OldManifestYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := NewAPI().recoverAdoptProfileUpdateUnderLease(rec.ManifestName); err != nil {
			t.Fatal(err)
		}
		got, _, _ := ReadAdoptProvenance(rec.ManifestName)
		if got.ExpectedManifestHash != j.OldManifestHash {
			t.Fatalf("record=%+v", got)
		}
	})
}

func TestIntentOnlyMaterializerReceiptAndLifecycleZeroGuard(t *testing.T) {
	_, rec, old := seedAdoptProfileUpdate(t)
	j := profileUpdateJournal(t, rec, old)
	receipt, err := materializeInstallSupervisorIntentTarget(old, j.NewTarget, InstallMutationReceiptV1{})
	if err != nil || !receipt.Committed || receipt.DryRun || len(receipt.ClientConfigSettlements) != 0 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	actual, err := readInstallSupervisorIntentTarget(j.NewTarget.Row.TaskName)
	if err != nil || actual.Fingerprint != j.NewTarget.Fingerprint {
		t.Fatalf("target=%+v err=%v", actual, err)
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller unavailable")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(source), "install_supervisor_intent_target.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"a.Install(", "installWithFrozenPlan(", "admitFrozen", "restartWith", "stopSupervisor", "killDaemon", "autostart."} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("intent-only materializer reaches lifecycle seam %q", forbidden)
		}
	}
}

func TestIntentOnlyMaterializerConcurrentSiblingForwardAndRollbackPreservesBoth(t *testing.T) {
	_, rec, old := seedAdoptProfileUpdate(t)
	j := profileUpdateJournal(t, rec, old)
	p, _ := DefaultSupervisorIntentPath()
	intent, _ := ReadSupervisorIntent(p)
	siblingOldRow := SupervisorDaemon{TaskName: "\\sibling", Server: "sibling", Daemon: "default", Command: "old", Port: 9999}
	intent.Daemons = append(intent.Daemons, siblingOldRow)
	if err := WriteSupervisorIntent(p, intent); err != nil {
		t.Fatal(err)
	}
	siblingOld := InstallSupervisorIntentTarget{Row: &siblingOldRow, Fingerprint: supervisorIntentTargetFingerprint(&siblingOldRow)}
	siblingNewRow := siblingOldRow
	siblingNewRow.Command = "new"
	siblingNew := InstallSupervisorIntentTarget{Row: &siblingNewRow, Fingerprint: supervisorIntentTargetFingerprint(&siblingNewRow)}
	errs := make(chan error, 2)
	go func() {
		_, err := materializeInstallSupervisorIntentTarget(old, j.NewTarget, InstallMutationReceiptV1{})
		errs <- err
	}()
	go func() {
		_, err := materializeInstallSupervisorIntentTarget(siblingOld, siblingNew, InstallMutationReceiptV1{})
		errs <- err
	}()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if err := restoreInstallSupervisorIntentTarget(j.NewTarget, old); err != nil {
		t.Fatal(err)
	}
	actualSibling, err := readInstallSupervisorIntentTarget(siblingNew.Row.TaskName)
	if err != nil || actualSibling.Fingerprint != siblingNew.Fingerprint {
		t.Fatalf("sibling=%+v err=%v", actualSibling, err)
	}
	actualTarget, err := readInstallSupervisorIntentTarget(old.Row.TaskName)
	if err != nil || actualTarget.Fingerprint != old.Fingerprint {
		t.Fatalf("target=%+v err=%v", actualTarget, err)
	}
}

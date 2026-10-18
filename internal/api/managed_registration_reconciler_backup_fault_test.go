package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
)

var errW10ReconcilerBackupFault = errors.New("injected W10 reconciler backup fault")

type w10ReconcilerBackupFault string

const (
	w10ReconcilerCancelAfterCreate w10ReconcilerBackupFault = "cancel-after-create"
	w10ReconcilerErrorAfterCreate  w10ReconcilerBackupFault = "error-after-create"
	w10ReconcilerCancelAfterRead   w10ReconcilerBackupFault = "cancel-after-read"
	w10ReconcilerErrorAfterRead    w10ReconcilerBackupFault = "error-after-read"
)

type w10ReconcilerFaultBackupIO struct {
	base          managedRegistrationBackupIO
	fault         w10ReconcilerBackupFault
	cancel        context.CancelFunc
	createLeaves  []string
	readLeaves    []string
	readCompleted bool
}

func newW10ReconcilerReadFaultIO(fault w10ReconcilerBackupFault, cancel context.CancelFunc) *w10ReconcilerFaultBackupIO {
	return &w10ReconcilerFaultBackupIO{fault: fault, cancel: cancel}
}

func newW10ReconcilerCreateFaultIO(fault w10ReconcilerBackupFault, cancel context.CancelFunc) *w10ReconcilerFaultBackupIO {
	io := &w10ReconcilerFaultBackupIO{fault: fault, cancel: cancel}
	io.base.createOwnerOnly = func(path string, contents []byte) (bool, error) {
		created, err := SecureCreateOwnerOnlyFile(path, contents)
		if err != nil || !created {
			return created, err
		}
		switch io.fault {
		case w10ReconcilerCancelAfterCreate:
			io.cancel()
			return true, nil
		case w10ReconcilerErrorAfterCreate:
			return true, errW10ReconcilerBackupFault
		default:
			return true, nil
		}
	}
	return io
}

func (io *w10ReconcilerFaultBackupIO) Create(ctx context.Context, parentDir, leaf string, contents []byte, expectedSHA256 string) (clients.ManagedRegistrationBackupCreateReceipt, error) {
	io.createLeaves = append(io.createLeaves, leaf)
	return io.base.Create(ctx, parentDir, leaf, contents, expectedSHA256)
}

func (io *w10ReconcilerFaultBackupIO) Read(ctx context.Context, parentDir, leaf, expectedSHA256 string) ([]byte, error) {
	io.readLeaves = append(io.readLeaves, leaf)
	contents, err := io.base.Read(ctx, parentDir, leaf, expectedSHA256)
	if err != nil {
		return nil, err
	}
	io.readCompleted = true
	switch io.fault {
	case w10ReconcilerCancelAfterRead:
		io.cancel()
		return contents, nil
	case w10ReconcilerErrorAfterRead:
		return contents, errW10ReconcilerBackupFault
	default:
		return contents, nil
	}
}

// TestW10_ReconcilerBackupFaultsStopBeforeMutation catches a reconciler that
// normalizes a post-publication backup failure and continues into durable
// prepare, the client writer, projection, or terminal settlement.
func TestW10_ReconcilerBackupFaultsStopBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault w10ReconcilerBackupFault
	}{
		{name: "forward/cancel_after_committed_create", fault: w10ReconcilerCancelAfterCreate},
		{name: "forward/error_after_candidate_publication", fault: w10ReconcilerErrorAfterCreate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newW10ReconcilerFixture(t, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			backupIO := newW10ReconcilerCreateFaultIO(tc.fault, cancel)
			reconciler := fixture.newReconciler(t, backupIO)
			writerCalls := guardW10ReconcilerClientWriter(t)

			report, err := reconciler.Reconcile(ctx, fixture.request)
			if err == nil {
				t.Fatal("Reconcile error = nil, want backup failure")
			}
			if *writerCalls != 0 {
				t.Fatalf("client writer calls = %d, want zero", *writerCalls)
			}
			fixture.requireConfigBytes(t, fixture.initialConfig)
			if len(report.Rows) != 1 || report.Rows[0].Settlement != clients.ManagedRegistrationSettlementCaptured || len(report.Events) != 0 {
				t.Fatalf("forward report = %#v, want one nonterminal captured row and no event", report)
			}
			stored := fixture.readJournal(t)
			if len(stored.Rows) != 1 || stored.Rows[0].Settlement != clients.ManagedRegistrationSettlementCaptured || stored.Rows[0].BackupID.String() != "" {
				t.Fatalf("stored forward row = %#v, want retained nonterminal candidate state", stored.Rows)
			}
			if len(backupIO.createLeaves) != 1 {
				t.Fatalf("candidate leaves = %#v, want one", backupIO.createLeaves)
			}
			fixture.requireCandidateBytes(t, backupIO.createLeaves[0], fixture.initialConfig)
		})
	}

	for _, tc := range []struct {
		name  string
		fault w10ReconcilerBackupFault
	}{
		{name: "recover_prepared_absent/cancel_after_pinned_read", fault: w10ReconcilerCancelAfterRead},
		{name: "recover_prepared_absent/error_after_pinned_read", fault: w10ReconcilerErrorAfterRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newW10ReconcilerFixture(t, false)
			candidateLeaf := fixture.writePreparedAbsentJournal(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			backupIO := newW10ReconcilerReadFaultIO(tc.fault, cancel)
			reconciler := fixture.newReconciler(t, backupIO)
			writerCalls := guardW10ReconcilerClientWriter(t)

			report, err := reconciler.Recover(ctx)
			if err == nil {
				t.Fatal("Recover error = nil, want backup failure")
			}
			if *writerCalls != 0 {
				t.Fatalf("client writer calls = %d, want zero", *writerCalls)
			}
			fixture.requireConfigBytes(t, fixture.initialConfig)
			if !backupIO.readCompleted || len(backupIO.readLeaves) != 1 || backupIO.readLeaves[0] != candidateLeaf {
				t.Fatalf("recovery backup reads = %#v completed=%t, want one completed pinned read of %q", backupIO.readLeaves, backupIO.readCompleted, candidateLeaf)
			}
			if len(report.Rows) != 1 || report.Rows[0].Settlement != clients.ManagedRegistrationSettlementPrepared || len(report.Events) != 0 {
				t.Fatalf("recovery report = %#v, want one retained prepared row and no event", report)
			}
			stored := fixture.readJournal(t)
			if len(stored.Rows) != 1 || stored.Rows[0].Settlement != clients.ManagedRegistrationSettlementPrepared || stored.Rows[0].BackupID.String() != candidateLeaf {
				t.Fatalf("stored recovery row = %#v, want retained prepared candidate", stored.Rows)
			}
			fixture.requireCandidateBytes(t, candidateLeaf, fixture.initialConfig)
		})
	}

	for _, tc := range []struct {
		name  string
		fault w10ReconcilerBackupFault
	}{
		{name: "rollback/cancel_after_pinned_read", fault: w10ReconcilerCancelAfterRead},
		{name: "rollback/error_after_pinned_read", fault: w10ReconcilerErrorAfterRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newW10ReconcilerFixture(t, true)
			candidateLeaf, appliedConfig := fixture.seedSettledForward(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			backupIO := newW10ReconcilerReadFaultIO(tc.fault, cancel)
			reconciler := fixture.newReconciler(t, backupIO)
			writerCalls := guardW10ReconcilerClientWriter(t)

			report, err := reconciler.RollbackTargetGroup(ctx, ManagedRegistrationRollbackRequest{
				GenerationID: fixture.request.GenerationID,
				Server:       "serena",
				Daemon:       "default",
				absenceProof: &ManagedRegistrationHubOwnerAbsentProof{server: "serena", daemon: "default"},
				clientIDs:    map[string]struct{}{"codex-cli": {}},
			})
			if err == nil {
				t.Fatal("RollbackTargetGroup error = nil, want backup failure")
			}
			if *writerCalls != 0 {
				t.Fatalf("client writer calls = %d, want zero", *writerCalls)
			}
			fixture.requireConfigBytes(t, appliedConfig)
			if !backupIO.readCompleted || len(backupIO.readLeaves) != 1 || backupIO.readLeaves[0] != candidateLeaf {
				t.Fatalf("rollback backup reads = %#v completed=%t, want one completed pinned read of %q", backupIO.readLeaves, backupIO.readCompleted, candidateLeaf)
			}
			if len(report.Rows) != 1 || report.Rows[0].Settlement != clients.ManagedRegistrationSettlementSettled {
				t.Fatalf("rollback report = %#v, want original settled row retained and not rolled back", report)
			}
			stored := fixture.readJournal(t)
			if len(stored.Rows) != 1 || stored.Rows[0].Settlement != clients.ManagedRegistrationSettlementSettled || stored.Rows[0].BackupID.String() != candidateLeaf {
				t.Fatalf("stored rollback row = %#v, want retained settled candidate", stored.Rows)
			}
			fixture.requireCandidateBytes(t, candidateLeaf, fixture.initialConfig)
		})
	}
}

type w10ReconcilerFixture struct {
	stateDir      string
	configPath    string
	initialConfig []byte
	managed       clients.ManagedRegistrationClient
	request       ManagedRegistrationReconcileRequest
	targetGroup   clients.ManagedRegistrationFingerprint
	release       clients.ManagedRegistrationFingerprint
	now           time.Time
}

func newW10ReconcilerFixture(t *testing.T, entryExists bool) w10ReconcilerFixture {
	t.Helper()
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	configPath := filepath.Join(codexHome, "config.toml")
	initial := []byte("[history]\npersistence = \"save-all\"\n")
	if entryExists {
		initial = []byte("[mcp_servers.serena]\ncommand = \"uvx\"\nargs = [\"serena\", \"start-mcp-server\"]\n\n[history]\npersistence = \"save-all\"\n")
	}
	if err := os.WriteFile(configPath, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := clients.NewCodexCLI()
	if err != nil {
		t.Fatalf("NewCodexCLI: %v", err)
	}
	managed, ok := clients.AsManagedRegistrationClient(client)
	if !ok {
		t.Fatalf("NewCodexCLI returned %T without managed-registration capability", client)
	}
	desired := clients.MCPEntry{Name: "serena", URL: "http://127.0.0.1:9121/mcp", ToolTimeoutSec: 45}
	targetIdentity, err := clients.ManagedRegistrationTargetFingerprint(desired, false)
	if err != nil {
		t.Fatalf("target fingerprint: %v", err)
	}
	target := ManagedRegistrationTarget{TaskKey: "serena-default", Server: "serena", Daemon: "default", Port: 9121, Path: "/mcp", TargetIdentitySHA256: targetIdentity, Desired: true, Ready: true, Live: true}
	targetGroup, err := managedRegistrationTargetGroupFingerprint(target)
	if err != nil {
		t.Fatalf("target-group fingerprint: %v", err)
	}
	releaseIdentity := ManagedRegistrationReleaseIdentity{Version: "0.4.37-test", Commit: "fixture"}
	release, err := managedRegistrationReleaseFingerprint(releaseIdentity, "w10-reconciler-fixture")
	if err != nil {
		t.Fatalf("release fingerprint: %v", err)
	}
	return w10ReconcilerFixture{
		stateDir: stateDir, configPath: configPath, initialConfig: initial, managed: managed,
		targetGroup: targetGroup, release: release,
		now: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
		request: ManagedRegistrationReconcileRequest{
			Operation: ManagedRegistrationForward, Trigger: ManagedRegistrationTriggerInstall,
			GenerationID: "w10-reconciler-forward", ReleaseIdentity: releaseIdentity,
			Targets:       []ManagedRegistrationTarget{target},
			Entries:       []ManagedRegistrationEntryIntent{{ClientID: "codex-cli", EntryName: "serena", Server: "serena", EnabledState: clients.ManagedRegistrationEnabled, Transport: clients.ManagedRegistrationTransportHTTP, Desired: desired}},
			sourceReceipt: "w10-reconciler-fixture",
		},
	}
}

func (f w10ReconcilerFixture) newReconciler(t *testing.T, backupIO clients.ManagedRegistrationBackupIO) *ManagedRegistrationReconciler {
	t.Helper()
	reconciler, err := NewManagedRegistrationReconciler(ManagedRegistrationReconcilerOptions{
		StateDir:                      f.stateDir,
		Clients:                       map[string]clients.ManagedRegistrationClient{"codex-cli": f.managed},
		Now:                           func() time.Time { return f.now },
		BackupIO:                      backupIO,
		ExistingManagedRelationships:  map[string]map[string]string{"codex-cli": {"serena": "serena"}},
		ExpectedReleaseIdentitySHA256: f.release,
		ResolveRecoveryEntryName: func(_ context.Context, clientID string, _ clients.ManagedRegistrationEntrySnapshot) (string, error) {
			if clientID != "codex-cli" {
				return "", errW10ReconcilerBackupFault
			}
			return "serena", nil
		},
		ResolveRollbackTargetGroup: func(_ context.Context, server, daemon string) (ManagedRegistrationTargetGroupResolution, error) {
			if server != "serena" || daemon != "default" {
				return ManagedRegistrationTargetGroupResolution{}, errW10ReconcilerBackupFault
			}
			return ManagedRegistrationTargetGroupResolution{TargetGroupIdentitySHA256: f.targetGroup, AbsenceProofTargetGroupSHA256: f.targetGroup, SupervisorSnapshotSHA256: f.targetGroup}, nil
		},
		resolvePreparedAbsentCreate: func(_ context.Context, clientID string, targetGroup clients.ManagedRegistrationFingerprint) (managedRegistrationPreparedCreateResolution, error) {
			if clientID != "codex-cli" || targetGroup != f.targetGroup {
				return managedRegistrationPreparedCreateResolution{}, errW10ReconcilerBackupFault
			}
			return managedRegistrationPreparedCreateResolution{EntryName: "serena", Desired: f.request.Entries[0].Desired, Target: f.request.Targets[0]}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewManagedRegistrationReconciler: %v", err)
	}
	return reconciler
}

func (f w10ReconcilerFixture) writePreparedAbsentJournal(t *testing.T) string {
	t.Helper()
	before, err := f.managed.SnapshotManagedRegistration(context.Background(), "serena")
	if err != nil || before.Exists {
		t.Fatalf("prepared-absent baseline = %#v, %v", before, err)
	}
	leaf := filepath.Base(f.configPath) + ".bak-mcp-local-hub-20260910-000000"
	backupID, err := clients.ParseManagedRegistrationBackupID(leaf)
	if err != nil {
		t.Fatalf("ParseManagedRegistrationBackupID: %v", err)
	}
	receipt, err := (managedRegistrationBackupIO{}).Create(context.Background(), filepath.Dir(f.configPath), leaf, f.initialConfig, w10BackupDigest(f.initialConfig))
	if err != nil || receipt.Disposition != clients.ManagedRegistrationBackupCommitted {
		t.Fatalf("seed recovery candidate = %#v, %v", receipt, err)
	}
	store, err := newManagedRegistrationJournalStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	file := managedRegistrationJournalFile{
		Schema: managedRegistrationJournalSchema, GenerationID: f.request.GenerationID, ReleaseIdentitySHA256: f.release,
		Rows: []managedRegistrationJournalRow{{
			ClientID: "codex-cli", Owner: "hub-supervisor", Transport: clients.ManagedRegistrationTransportHTTP,
			EnabledState: clients.ManagedRegistrationEnabled, IdentityState: clients.ManagedRegistrationIdentityManagedHTTP, Exists: true,
			Settlement: clients.ManagedRegistrationSettlementPrepared, Phase: ManagedRegistrationPhasePrepare,
			BackupID: backupID, TargetGroupIdentitySHA256: f.targetGroup, Before: before,
		}},
	}
	if err := store.write(context.Background(), file); err != nil {
		t.Fatalf("write prepared-absent journal: %v", err)
	}
	return leaf
}

func (f w10ReconcilerFixture) seedSettledForward(t *testing.T) (string, []byte) {
	t.Helper()
	reconciler := f.newReconciler(t, managedRegistrationBackupIO{})
	report, err := reconciler.Reconcile(context.Background(), f.request)
	if err != nil || len(report.Rows) != 1 || report.Rows[0].Settlement != clients.ManagedRegistrationSettlementSettled {
		t.Fatalf("seed forward report = %#v, %v", report, err)
	}
	file := f.readJournal(t)
	if len(file.Rows) != 1 || file.Rows[0].BackupID.String() == "" {
		t.Fatalf("seed forward journal = %#v", file)
	}
	applied, err := os.ReadFile(f.configPath)
	if err != nil || string(applied) == string(f.initialConfig) {
		t.Fatalf("seed applied config = %q, %v", applied, err)
	}
	return file.Rows[0].BackupID.String(), applied
}

func (f w10ReconcilerFixture) readJournal(t *testing.T) managedRegistrationJournalFile {
	t.Helper()
	store, err := newManagedRegistrationJournalStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.read(context.Background())
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	return file
}

func (f w10ReconcilerFixture) requireConfigBytes(t *testing.T, want []byte) {
	t.Helper()
	got, err := os.ReadFile(f.configPath)
	if err != nil || string(got) != string(want) {
		t.Fatalf("config bytes changed: got %q, err=%v, want %q", got, err, want)
	}
}

func (f w10ReconcilerFixture) requireCandidateBytes(t *testing.T, leaf string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(filepath.Dir(f.configPath), leaf))
	if err != nil || string(got) != string(want) {
		t.Fatalf("candidate %q = %q, err=%v, want %q", leaf, got, err, want)
	}
}

func guardW10ReconcilerClientWriter(t *testing.T) *int {
	t.Helper()
	previous := clients.WriteConfigFile
	calls := 0
	clients.WriteConfigFile = func(string, []byte) error {
		calls++
		return errors.New("unexpected W10 fixture client writer call")
	}
	t.Cleanup(func() { clients.WriteConfigFile = previous })
	return &calls
}

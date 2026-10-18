package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
)

// TestW10_LegacyRowExpansionIsNoWrite catches the already-desired shortcut
// returning a false settled result before a legacy tuple has been proven against
// the frozen physical entry and durably enriched.
func TestW10_LegacyRowExpansionIsNoWrite(t *testing.T) {
	fixture := newW10LegacyFixture(t, w10LegacyConfig(45), nil)
	beforeState := fixture.readManagedEntriesBytes(t)
	var seeded ManagedEntries
	if err := json.Unmarshal(beforeState, &seeded); err != nil || len(seeded.Entries) != 2 || !managedRegistrationEntryIsLegacy(seeded.Entries[0]) {
		t.Fatalf("legacy fixture discriminator = %#v, %v", seeded, err)
	}
	writerCalls := guardW10ReconcilerClientWriter(t)

	report, err := (&API{}).ReconcileManagedRegistrations(context.Background(), fixture.options())
	if err != nil {
		t.Fatalf("ReconcileManagedRegistrations: %#v; state=%s", err, fixture.readManagedEntriesBytes(t))
	}
	if *writerCalls != 0 {
		t.Fatalf("client writer calls = %d, want zero", *writerCalls)
	}
	fixture.requireConfigBytes(t, fixture.config)
	if len(report.Rows) != 1 || report.Rows[0].Settlement != clients.ManagedRegistrationSettlementSettled || len(report.Events) != 1 {
		t.Fatalf("legacy exact report = %#v, want journal-backed settled row and one event", report)
	}
	afterState := fixture.readManagedEntriesBytes(t)
	if string(afterState) == string(beforeState) {
		t.Fatal("legacy exact managed-entry row was not enriched")
	}
	fixture.requireManagedEntriesProjection(t)
	fixture.requireNoMutationJournal(t)

	enrichedState := fixture.readManagedEntriesBytes(t)
	second := fixture.options()
	second.Request.GenerationID = "w10-enriched-alias-rerun"
	secondReport, err := (&API{}).ReconcileManagedRegistrations(context.Background(), second)
	if err != nil || len(secondReport.Rows) != 1 || secondReport.Rows[0].Settlement != clients.ManagedRegistrationSettlementSettled {
		t.Fatalf("enriched alias rerun = %#v, %v", secondReport, err)
	}
	if *writerCalls != 0 {
		t.Fatalf("enriched alias rerun writer calls = %d, want zero", *writerCalls)
	}
	if got := fixture.readManagedEntriesBytes(t); string(got) != string(enrichedState) {
		t.Fatalf("enriched alias rerun changed projection:\nbefore=%s\nafter=%s", enrichedState, got)
	}
}

// TestW10_LegacyRowMismatchPreservesState catches a legacy tuple being treated
// as write authority when the physical key is absent/opaque/ambiguous or when
// the endpoint matches but the current Codex timeout differs from frozen input.
func TestW10_LegacyRowMismatchPreservesState(t *testing.T) {
	secondAlias := managedRegistrationFrozenInstallTarget{
		ClientID: "codex-cli", LogicalEntryName: "serena", PhysicalEntryName: "serena-alt", Action: ClientUpdateAddReplace,
		DaemonName: "alternate", TaskName: "serena-alternate", Port: 9122, URLPath: "/mcp",
		Desired: clients.MCPEntry{Name: "serena-alt", URL: "http://127.0.0.1:9122/mcp", ToolTimeoutSec: 45},
		Codex:   &managedRegistrationFrozenCodexRelocation{SourceEntryName: "serena", TargetEntryName: "serena-alt", ExpectedSource: clients.CodexTransportNone, AlreadyConfigured: true},
	}
	for _, tc := range []struct {
		name         string
		config       []byte
		extraTargets []managedRegistrationFrozenInstallTarget
		checkTimeout bool
	}{
		{name: "timeout_mismatch", config: w10LegacyConfig(46), checkTimeout: true},
		{name: "absent_physical_key", config: []byte("[history]\npersistence = \"save-all\"\n")},
		{name: "opaque_physical_entry", config: []byte("[mcp_servers.serena-mcphub]\nurl = \"http:opaque\"\ntool_timeout_sec = 45\n")},
		{name: "ambiguous_frozen_physical_key", config: append(w10LegacyConfig(45), []byte("\n[mcp_servers.serena-alt]\nurl = \"http://127.0.0.1:9122/mcp\"\ntool_timeout_sec = 45\n")...), extraTargets: []managedRegistrationFrozenInstallTarget{secondAlias}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newW10LegacyFixture(t, tc.config, tc.extraTargets)
			if tc.checkTimeout {
				fixture.requireTimeoutFingerprintSeparation(t)
			}
			beforeState := fixture.readManagedEntriesBytes(t)
			writerCalls := guardW10ReconcilerClientWriter(t)

			report, err := (&API{}).ReconcileManagedRegistrations(context.Background(), fixture.options())
			if err == nil || err.Error() != "mcp-registration-legacy-ownership-unproven" {
				t.Fatalf("legacy mismatch error = %v, want mcp-registration-legacy-ownership-unproven", err)
			}
			if report != nil && (len(report.Rows) != 0 || len(report.Events) != 0) {
				t.Fatalf("legacy mismatch report = %#v, want no false settlement", report)
			}
			if *writerCalls != 0 {
				t.Fatalf("client writer calls = %d, want zero", *writerCalls)
			}
			fixture.requireConfigBytes(t, tc.config)
			afterState := fixture.readManagedEntriesBytes(t)
			if string(afterState) != string(beforeState) {
				t.Fatalf("legacy mismatch changed managed entries:\nbefore=%s\nafter=%s", beforeState, afterState)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.stateDir, managedRegistrationJournalLeaf)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("legacy mismatch journal exists: %v", statErr)
			}
		})
	}
}

func TestW10_InstallRequireAlreadyDesiredMismatchIsNoWrite(t *testing.T) {
	fixture := newW10LegacyFixture(t, w10LegacyConfig(46), nil)
	beforeState := fixture.readManagedEntriesBytes(t)
	writerCalls := guardW10ReconcilerClientWriter(t)
	options := fixture.options()
	options.Request.Trigger = ManagedRegistrationTriggerInstall
	options.Request.RequireAlreadyDesired = true
	report, err := (&API{}).ReconcileManagedRegistrations(context.Background(), options)
	if err == nil || err.Error() != "mcp-registration-precondition-conflict" {
		t.Fatalf("install mismatch error = %v, want mcp-registration-precondition-conflict", err)
	}
	if report != nil && (len(report.Rows) != 0 || len(report.Events) != 0) {
		t.Fatalf("install mismatch report = %#v, want no settlement", report)
	}
	if *writerCalls != 0 {
		t.Fatalf("install mismatch client writer calls = %d, want zero", *writerCalls)
	}
	fixture.requireConfigBytes(t, fixture.config)
	if got := fixture.readManagedEntriesBytes(t); string(got) != string(beforeState) {
		t.Fatalf("install mismatch changed managed entries:\nbefore=%s\nafter=%s", beforeState, got)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.stateDir, managedRegistrationJournalLeaf)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("install mismatch journal exists: %v", statErr)
	}
}

type w10LegacyFixture struct {
	stateDir   string
	configPath string
	config     []byte
	runtime    *ManagedRegistrationRuntime
	foreign    ManagedEntry
}

func newW10LegacyFixture(t *testing.T, config []byte, extraTargets []managedRegistrationFrozenInstallTarget) w10LegacyFixture {
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
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := clients.NewCodexCLI()
	if err != nil {
		t.Fatalf("NewCodexCLI: %v", err)
	}
	target := managedRegistrationFrozenInstallTarget{
		ClientID: "codex-cli", LogicalEntryName: "serena", PhysicalEntryName: "serena-mcphub", Action: ClientUpdateAddReplace,
		DaemonName: "default", TaskName: "serena-default", Port: 9121, URLPath: "/mcp",
		Desired: clients.MCPEntry{Name: "serena-mcphub", URL: "http://127.0.0.1:9121/mcp", ToolTimeoutSec: 45},
		Codex:   &managedRegistrationFrozenCodexRelocation{SourceEntryName: "serena", TargetEntryName: "serena-mcphub", ExpectedSource: clients.CodexTransportNone, AlreadyConfigured: true},
	}
	targets := append([]managedRegistrationFrozenInstallTarget{target}, extraTargets...)
	runtime, err := newManagedRegistrationRuntime(managedRegistrationFrozenInstallInput{
		Server: "serena", ReleaseIdentity: managedRegistrationReleaseIdentity{Version: "0.4.37-test", Commit: "legacy-fixture"},
		CapturedAtUTC: time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC), StateRoot: stateDir, JournalRoot: stateDir,
		BackupIO: managedRegistrationBackupIO{}, ClientPopulation: map[string]clients.Client{"codex-cli": client}, Targets: targets,
	})
	if err != nil {
		t.Fatalf("newManagedRegistrationRuntime: %v", err)
	}
	foreign := ManagedEntry{Client: "cursor", Server: "memory", InstalledAt: time.Date(2026, 9, 9, 1, 2, 3, 0, time.UTC)}
	state, err := json.MarshalIndent(map[string]interface{}{
		"version": managedEntriesSchemaVersion,
		"entries": []map[string]interface{}{
			{"client": "codex-cli", "server": "serena", "installed_at": time.Date(2026, 9, 8, 1, 2, 3, 0, time.UTC)},
			{"client": foreign.Client, "server": foreign.Server, "installed_at": foreign.InstalledAt},
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy managed entries: %v", err)
	}
	if err := WriteStateFileBytesAtomic(filepath.Join(stateDir, managedEntriesFileLeaf), state); err != nil {
		t.Fatalf("write legacy managed entries: %v", err)
	}
	return w10LegacyFixture{stateDir: stateDir, configPath: configPath, config: append([]byte(nil), config...), runtime: runtime, foreign: foreign}
}

func (f w10LegacyFixture) options() ManagedRegistrationReconcileOptions {
	return ManagedRegistrationReconcileOptions{
		Runtime: f.runtime,
		Request: ManagedRegistrationReconcileRequest{
			Operation: ManagedRegistrationForward, Trigger: ManagedRegistrationTriggerLegacyUpgrade,
			Servers: []string{"serena"}, Clients: []string{"codex-cli"}, GenerationID: "w10-legacy-expansion",
		},
	}
}

func (f w10LegacyFixture) readManagedEntriesBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := ReadStateFileInodeAnchored(filepath.Join(f.stateDir, managedEntriesFileLeaf))
	if err != nil {
		t.Fatalf("read managed entries: %v", err)
	}
	return raw
}

func (f w10LegacyFixture) requireConfigBytes(t *testing.T, want []byte) {
	t.Helper()
	got, err := os.ReadFile(f.configPath)
	if err != nil || string(got) != string(want) {
		t.Fatalf("config = %q, %v, want %q", got, err, want)
	}
}

func (f w10LegacyFixture) requireManagedEntriesProjection(t *testing.T) {
	t.Helper()
	var wire struct {
		Version int                      `json:"version"`
		Entries []map[string]interface{} `json:"entries"`
	}
	if err := json.Unmarshal(f.readManagedEntriesBytes(t), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Version != 1 || len(wire.Entries) != 2 {
		t.Fatalf("managed entries wire = %#v", wire)
	}
	var target, foreign map[string]interface{}
	for _, entry := range wire.Entries {
		switch entry["client"] {
		case "codex-cli":
			target = entry
		case "cursor":
			foreign = entry
		}
	}
	for _, key := range []string{"owner", "transport", "enabled_state", "identity_state", "exists", "config_sha256", "entry_sha256", "desired_projection_sha256", "config_path_sha256", "entry_key_sha256", "target_identity_sha256"} {
		if target[key] == nil || target[key] == "" {
			t.Fatalf("enriched managed entry missing %q: %#v", key, target)
		}
	}
	if foreign["server"] != f.foreign.Server || foreign["installed_at"] != f.foreign.InstalledAt.Format(time.RFC3339) || len(foreign) != 3 {
		t.Fatalf("foreign row changed: %#v", foreign)
	}
}

func (f w10LegacyFixture) requireNoMutationJournal(t *testing.T) {
	t.Helper()
	var wire struct {
		Rows []map[string]interface{} `json:"rows"`
	}
	raw, err := ReadStateFileInodeAnchored(filepath.Join(f.stateDir, managedRegistrationJournalLeaf))
	if err != nil || json.Unmarshal(raw, &wire) != nil || len(wire.Rows) != 1 {
		t.Fatalf("no-mutation journal = %s, %v", raw, err)
	}
	if wire.Rows[0]["snapshot_verified_without_mutation"] != true || wire.Rows[0]["settlement"] != "settled" || wire.Rows[0]["backup_id"] != "" {
		t.Fatalf("no-mutation journal row = %#v", wire.Rows[0])
	}
}

func (f w10LegacyFixture) requireTimeoutFingerprintSeparation(t *testing.T) {
	t.Helper()
	managed, ok := clients.AsManagedRegistrationClient(f.runtime.frozen.ClientPopulation["codex-cli"])
	if !ok {
		t.Fatal("missing managed Codex client")
	}
	current, err := managed.SnapshotManagedRegistration(context.Background(), "serena-mcphub")
	if err != nil {
		t.Fatal(err)
	}
	wantTarget, err := clients.ManagedRegistrationTargetFingerprint(f.runtime.frozen.Targets[0].Desired, false)
	if err != nil || current.TargetIdentitySHA256 != wantTarget {
		t.Fatalf("timeout mismatch target identity = %s, want same endpoint %s, err=%v", current.TargetIdentitySHA256.String(), wantTarget.String(), err)
	}
	exactConfig := w10LegacyConfig(45)
	if err := os.WriteFile(f.configPath, exactConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	exact, err := managed.SnapshotManagedRegistration(context.Background(), "serena-mcphub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.configPath, f.config, 0o600); err != nil {
		t.Fatal(err)
	}
	if exact.TargetIdentitySHA256 != current.TargetIdentitySHA256 || exact.DesiredProjectionSHA256 == current.DesiredProjectionSHA256 {
		t.Fatalf("timeout fingerprints: exact target=%s projection=%s mismatch target=%s projection=%s", exact.TargetIdentitySHA256.String(), exact.DesiredProjectionSHA256.String(), current.TargetIdentitySHA256.String(), current.DesiredProjectionSHA256.String())
	}
}

func w10LegacyConfig(timeout int) []byte {
	return []byte("[mcp_servers.serena-mcphub]\nurl = \"http://127.0.0.1:9121/mcp\"\ntool_timeout_sec = " + strconv.Itoa(timeout) + "\n\n[history]\npersistence = \"save-all\"\n")
}

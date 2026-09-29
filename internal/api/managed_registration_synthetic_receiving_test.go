package api

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// TestW10_SyntheticReceivingCrossesFrozenInstallFileBoundary proves the one
// admitted synthetic receiving path: public frozen-plan mutation, materializer,
// real managed core, temporary Codex file, v2 journal/projection/event, and
// existing receiver. It never starts a task, listener, or external process.
func TestW10_SyntheticReceivingCrossesFrozenInstallFileBoundary(t *testing.T) {
	fixture := newW10InstallCompositionFixture(t)
	beforeConfig, err := os.ReadFile(fixture.client.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	core := &w10SyntheticReceivingCore{inner: managedRegistrationInstallCoreV1{api: fixture.api}}
	codex, ok := clients.AsCodexTransportClient(fixture.client)
	if !ok {
		t.Fatalf("temporary client %T cannot relocate Codex entries", fixture.client)
	}
	materializeCalls := 0
	materializeRows := -1
	deps := fixture.deps(core, func(_ context.Context, _ *API, manifest *config.ServerManifest, plan *Plan, _ InstallOpts, _ ClientRoutingAuthorityRequest, _ io.Writer) error {
		materializeCalls++
		materializeRows = len(plan.ClientUpdates)
		if materializeRows != 1 {
			t.Fatalf("materializer received %d client updates, want 1", materializeRows)
		}
		update := plan.ClientUpdates[0]
		_, err := codex.RelocateHTTPEntry(clients.CodexHTTPRelocation{
			SourceEntryName: manifest.Name,
			TargetEntryName: update.EntryName,
			Entry:           installClientEntryV1(manifest.Name, update, plan.installRelayExePath),
			ExpectedSource:  clients.CodexTransportStdio,
		})
		return err
	})
	mutation := &w10InstallCommandMutation{fixture: fixture, deps: deps}
	receiver := &commandReceiverFake{settlement: CommandSettlementV1{CommitState: "settled"}}
	settlement, err := (CommandCoordinator{Mutation: mutation, Receiver: receiver}).Install(context.Background(), fixture.opts)
	if err != nil || settlement.CommitState != "settled" {
		t.Fatalf("synthetic receiving settlement = %#v, %v", settlement, err)
	}
	if mutation.calls != 1 || materializeCalls != 1 || materializeRows != 1 || core.calls != 1 || receiver.calls != 1 {
		t.Fatalf("mutation/materialize/rows/core/receiver = %d/%d/%d/%d/%d", mutation.calls, materializeCalls, materializeRows, core.calls, receiver.calls)
	}
	if core.report == nil || len(core.report.Rows) != 1 || core.report.Rows[0].Settlement != clients.ManagedRegistrationSettlementSettled || len(core.report.Events) != 1 || core.report.Events[0].EventID != "managed-registration-reconcile-settled.v2" {
		t.Fatalf("managed core report = %#v", core.report)
	}
	afterConfig, err := os.ReadFile(fixture.client.ConfigPath())
	if err != nil || string(afterConfig) == string(beforeConfig) {
		t.Fatalf("temporary client config did not cross boundary: %q, %v", afterConfig, err)
	}
	reader, ok := fixture.client.(interface {
		GetEntry(string) (*clients.MCPEntry, error)
	})
	if !ok {
		t.Fatalf("temporary client %T cannot read Codex entries", fixture.client)
	}
	logical, err := reader.GetEntry("serena")
	if err != nil || logical != nil {
		t.Fatalf("logical entry after receiving = %#v, %v", logical, err)
	}
	physical, err := reader.GetEntry("serena-mcphub")
	if err != nil || physical == nil || physical.ToolTimeoutSec != 45 {
		t.Fatalf("physical entry after receiving = %#v, %v", physical, err)
	}
	journal, err := newManagedRegistrationJournalStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := journal.read(context.Background())
	if err != nil || !managedRegistrationJournalSettled(stored) {
		t.Fatalf("v2 journal = %#v, %v", stored, err)
	}
	rawEntries, err := ReadStateFileInodeAnchored(fixture.stateDir + string(os.PathSeparator) + managedEntriesFileLeaf)
	if err != nil {
		t.Fatal(err)
	}
	var entries ManagedEntries
	if err := json.Unmarshal(rawEntries, &entries); err != nil || len(entries.Entries) != 1 {
		t.Fatalf("enriched managed entries = %#v, %v", entries, err)
	}
	entry := entries.Entries[0]
	if entry.Client != "codex-cli" || entry.Server != "serena" || entry.DesiredProjectionSHA256.String() == "" || entry.ConfigPathSHA256.String() == "" || entry.EntryKeySHA256.String() == "" {
		t.Fatalf("enriched managed entry = %#v", entry)
	}
}

type w10SyntheticReceivingCore struct {
	inner  managedRegistrationInstallCore
	calls  int
	report *ManagedRegistrationReconcileReport
}

func (c *w10SyntheticReceivingCore) ReconcileInstall(ctx context.Context, input managedRegistrationFrozenInstallInput) (*ManagedRegistrationReconcileReport, error) {
	c.calls++
	report, err := c.inner.ReconcileInstall(ctx, input)
	c.report = report
	return report, err
}

package api

import (
	"context"
	"errors"
	"testing"

	"mcp-local-hub/internal/clients"
)

type raceSerenaClient struct {
	*reconcileFakeClient
	beforeConditional func(string)
}

func (f *raceSerenaClient) ConditionalEntryMutation(req clients.ConditionalEntryMutationRequest) clients.EntryMutationObserved {
	f.beforeConditional(req.EntryName)
	return f.reconcileFakeClient.ConditionalEntryMutation(req)
}

type raceLSPRouterClient struct {
	*lspRouterFakeClient
	beforeConditional func(string)
}

func (f *raceLSPRouterClient) ConditionalEntryMutation(req clients.ConditionalEntryMutationRequest) clients.EntryMutationObserved {
	f.beforeConditional(req.EntryName)
	return f.lspRouterFakeClient.ConditionalEntryMutation(req)
}

type raceSnapshotClient struct {
	*snapshotFakeClient
	beforeConditional func(string)
}

func (f *raceSnapshotClient) ConditionalEntryMutation(req clients.ConditionalEntryMutationRequest) clients.EntryMutationObserved {
	f.beforeConditional(req.EntryName)
	return f.snapshotFakeClient.ConditionalEntryMutation(req)
}

// lspRouterFakeClient is declared in lsp_client_router_test.go; its
// conditional implementation lives here because this file owns the version-3
// plan/apply contract.
func (f *lspRouterFakeClient) ConditionalEntryMutation(req clients.ConditionalEntryMutationRequest) (out clients.EntryMutationObserved) {
	before, err := f.GetEntry(req.EntryName)
	out.Before = before
	if err != nil {
		out.ObservationErr = err
		return out
	}
	if req.ExpectedLive == nil || !req.ExpectedLive(before) {
		out.PreconditionConflict = true
		out.PreparationErr = clients.ErrEntryMutationPreconditionConflict
		return out
	}
	if req.BackupKeepN != nil {
		out.BackupPath, out.PreparationErr = f.BackupKeep(*req.BackupKeepN)
		if out.PreparationErr != nil {
			return out
		}
	}
	if req.BeforeMutation != nil {
		out.PreparationErr = req.BeforeMutation(clients.EntryMutationPreparation{
			Before: before, BackupPath: out.BackupPath,
		})
		if out.PreparationErr != nil {
			return out
		}
	}
	out.Invoked = true
	if req.Operation == clients.EntryMutationAdd {
		out.MutationErr = f.AddEntry(req.Entry)
	} else {
		out.MutationErr = f.RemoveEntry(req.EntryName)
	}
	out.After, out.ObservationErr = f.GetEntry(req.EntryName)
	return out
}

func TestMCPFrontV3_ConditionalMutationRejectsInterveningEdit(t *testing.T) {
	const operatorURL = "https://operator.example/mcp"
	const legacyName = "mcp-language-server-go-abcd"
	canonicalName := LSPRouterEntryName("go")

	t.Run("serena-add", func(t *testing.T) {
		base := newReconcileFakeClient("claude-code")
		client := &raceSerenaClient{
			reconcileFakeClient: base,
			beforeConditional: func(entryName string) {
				base.entries[entryName] = clients.MCPEntry{Name: entryName, URL: operatorURL}
			},
		}
		report, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
			Port: 9137, Ping: okPing,
			Clients:        map[string]clients.Client{"claude-code": client},
			ClientsInclude: []string{"claude-code"},
		})
		if err != nil {
			t.Fatalf("Serena reconcile returned whole-run error for a per-client conflict: %v", err)
		}
		if len(report.Failed) != 1 || report.Failed[0].Client != "claude-code" {
			t.Fatalf("Failed=%+v, want the intervening Serena edit reported per client", report.Failed)
		}
		if base.addCalls != 0 {
			t.Fatalf("Serena addCalls=%d, want 0", base.addCalls)
		}
		if got := base.entries[serenaEntryName].URL; got != operatorURL {
			t.Fatalf("intervening Serena entry changed: %q", got)
		}
	})

	t.Run("lsp-forward-canonical-add", func(t *testing.T) {
		base := newLSPRouterFakeClient(t, "claude-code", true)
		client := &raceLSPRouterClient{
			lspRouterFakeClient: base,
			beforeConditional: func(entryName string) {
				base.entries[entryName] = clients.MCPEntry{Name: entryName, URL: operatorURL}
			},
		}
		pre := LSPRouterEntrySnapshot{Client: "claude-code", Language: "go", EntryName: canonicalName}
		intended := LSPRouterEntrySnapshot{
			Client: "claude-code", Language: "go", EntryName: canonicalName,
			Present: true, URL: LSPRouterURL(9137, "go"),
		}
		plan := &LSPRouterClientPlan{
			Operations: []LSPRouterPlannedOperation{{
				Client: "claude-code", Language: "go", EntryName: canonicalName,
				Operation: "add", PreState: pre, IntendedState: intended,
				entry: clients.MCPEntry{Name: canonicalName, URL: intended.URL},
			}},
			clientMap: map[string]clients.Client{"claude-code": client},
			keepN:     3,
		}
		if _, err := NewAPI().ApplyLSPRouterClientPlan(plan, LSPRouterApplyCallbacks{}); err == nil {
			t.Fatal("intervening canonical edit must fail the conditional precondition")
		}
		if base.addCalls != 0 {
			t.Fatalf("canonical addCalls=%d, want 0", base.addCalls)
		}
		if got := base.entries[canonicalName].URL; got != operatorURL {
			t.Fatalf("intervening canonical entry changed: %q", got)
		}
	})

	t.Run("lsp-forward-legacy-remove", func(t *testing.T) {
		base := newLSPRouterFakeClient(t, "codex-cli", true)
		baselineURL := "http://127.0.0.1:9200/mcp"
		base.entries[legacyName] = clients.MCPEntry{Name: legacyName, URL: baselineURL}
		client := &raceLSPRouterClient{
			lspRouterFakeClient: base,
			beforeConditional: func(entryName string) {
				base.entries[entryName] = clients.MCPEntry{Name: entryName, URL: operatorURL}
			},
		}
		pre := LSPRouterEntrySnapshot{
			Client: "codex-cli", Language: "go", EntryName: legacyName,
			Present: true, URL: baselineURL,
		}
		plan := &LSPRouterClientPlan{
			Operations: []LSPRouterPlannedOperation{{
				Client: "codex-cli", Language: "go", EntryName: legacyName,
				Operation: "remove", PreState: pre,
				IntendedState: LSPRouterEntrySnapshot{
					Client: "codex-cli", Language: "go", EntryName: legacyName,
				},
			}},
			clientMap: map[string]clients.Client{"codex-cli": client},
			keepN:     3,
		}
		if _, err := NewAPI().ApplyLSPRouterClientPlan(plan, LSPRouterApplyCallbacks{}); err == nil {
			t.Fatal("intervening legacy edit must fail the conditional precondition")
		}
		if base.removeCalls != 0 {
			t.Fatalf("legacy removeCalls=%d, want 0", base.removeCalls)
		}
		if got := base.entries[legacyName].URL; got != operatorURL {
			t.Fatalf("intervening legacy entry changed: %q", got)
		}
	})

	t.Run("lsp-rollback-add", func(t *testing.T) {
		base := newSnapshotFakeClient("codex-cli", true)
		client := &raceSnapshotClient{
			snapshotFakeClient: base,
			beforeConditional: func(entryName string) {
				base.entries[entryName] = clients.MCPEntry{Name: entryName, URL: operatorURL}
			},
		}
		baseline := LSPRouterEntrySnapshot{
			Client: "codex-cli", Language: "go", EntryName: legacyName,
			Present: true, URL: "http://127.0.0.1:9200/mcp",
		}
		applied := LSPRouterEntrySnapshot{Client: "codex-cli", Language: "go", EntryName: legacyName}
		_, results, _ := NewAPI().RestoreLSPRouterRecoveryRows(
			[]LSPRouterRecoveryRow{{Baseline: baseline, Applied: &applied}},
			LSPClientRouterOpts{Clients: map[string]clients.Client{"codex-cli": client}},
			LSPRouterRestoreCallbacks{},
		)
		if len(results) != 1 || results[0].Status != LSPRouterRestoreConflict {
			t.Fatalf("results=%+v, want one conflict", results)
		}
		if base.addCalls != 0 {
			t.Fatalf("rollback addCalls=%d, want 0", base.addCalls)
		}
		if got := base.entries[legacyName].URL; got != operatorURL {
			t.Fatalf("intervening rollback-add entry changed: %q", got)
		}
	})

	t.Run("lsp-rollback-remove", func(t *testing.T) {
		base := newSnapshotFakeClient("claude-code", true)
		base.put(clients.MCPEntry{Name: canonicalName, URL: LSPRouterURL(9137, "go")})
		client := &raceSnapshotClient{
			snapshotFakeClient: base,
			beforeConditional: func(entryName string) {
				base.entries[entryName] = clients.MCPEntry{Name: entryName, URL: operatorURL}
			},
		}
		baseline := LSPRouterEntrySnapshot{Client: "claude-code", Language: "go", EntryName: canonicalName}
		applied := LSPRouterEntrySnapshot{
			Client: "claude-code", Language: "go", EntryName: canonicalName,
			Present: true, URL: LSPRouterURL(9137, "go"),
		}
		_, results, _ := NewAPI().RestoreLSPRouterRecoveryRows(
			[]LSPRouterRecoveryRow{{Baseline: baseline, Applied: &applied}},
			LSPClientRouterOpts{Clients: map[string]clients.Client{"claude-code": client}},
			LSPRouterRestoreCallbacks{},
		)
		if len(results) != 1 || results[0].Status != LSPRouterRestoreConflict {
			t.Fatalf("results=%+v, want one conflict", results)
		}
		if base.removeCalls != 0 {
			t.Fatalf("rollback removeCalls=%d, want 0", base.removeCalls)
		}
		if got := base.entries[canonicalName].URL; got != operatorURL {
			t.Fatalf("intervening rollback-remove entry changed: %q", got)
		}
	})
}

func TestMCPFrontV3_RealMutationSeamsRequireDurablePrepare(t *testing.T) {
	prepareErr := errors.New("induced durable prepare failure")
	const legacyName = "mcp-language-server-go-abcd"
	canonicalName := LSPRouterEntryName("go")

	t.Run("serena-add", func(t *testing.T) {
		client := newReconcileFakeClient("claude-code")
		_, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
			Port: 9137, Ping: okPing,
			Clients:        map[string]clients.Client{"claude-code": client},
			ClientsInclude: []string{"claude-code"},
			OnAttemptPrepared: func(SerenaReconcileAttemptResult) error {
				return prepareErr
			},
		})
		if !errors.Is(err, prepareErr) {
			t.Fatalf("error=%v, want durable prepare failure", err)
		}
		if client.addCalls != 0 {
			t.Fatalf("Serena addCalls=%d, want 0", client.addCalls)
		}
	})

	for _, tc := range []struct {
		name string
		op   LSPRouterPlannedOperation
		seed *clients.MCPEntry
	}{
		{
			name: "lsp-forward-canonical-add",
			op: LSPRouterPlannedOperation{
				Client: "claude-code", Language: "go", EntryName: canonicalName, Operation: "add",
				PreState: LSPRouterEntrySnapshot{Client: "claude-code", Language: "go", EntryName: canonicalName},
				IntendedState: LSPRouterEntrySnapshot{
					Client: "claude-code", Language: "go", EntryName: canonicalName,
					Present: true, URL: LSPRouterURL(9137, "go"),
				},
				entry: clients.MCPEntry{Name: canonicalName, URL: LSPRouterURL(9137, "go")},
			},
		},
		{
			name: "lsp-forward-legacy-remove",
			op: LSPRouterPlannedOperation{
				Client: "codex-cli", Language: "go", EntryName: legacyName, Operation: "remove",
				PreState: LSPRouterEntrySnapshot{
					Client: "codex-cli", Language: "go", EntryName: legacyName,
					Present: true, URL: "http://127.0.0.1:9200/mcp",
				},
				IntendedState: LSPRouterEntrySnapshot{
					Client: "codex-cli", Language: "go", EntryName: legacyName,
				},
			},
			seed: &clients.MCPEntry{Name: legacyName, URL: "http://127.0.0.1:9200/mcp"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newLSPRouterFakeClient(t, tc.op.Client, true)
			if tc.seed != nil {
				client.entries[tc.seed.Name] = *tc.seed
			}
			plan := &LSPRouterClientPlan{
				Operations: []LSPRouterPlannedOperation{tc.op},
				clientMap:  map[string]clients.Client{tc.op.Client: client},
				keepN:      3,
			}
			_, err := NewAPI().ApplyLSPRouterClientPlan(plan, LSPRouterApplyCallbacks{
				OnPrepared: func(LSPRouterPlannedOperation) error { return prepareErr },
			})
			if !errors.Is(err, prepareErr) {
				t.Fatalf("error=%v, want durable prepare failure", err)
			}
			if client.addCalls != 0 || client.removeCalls != 0 {
				t.Fatalf("adapter calls add=%d remove=%d, want 0/0", client.addCalls, client.removeCalls)
			}
		})
	}

	for _, tc := range []struct {
		name     string
		baseline LSPRouterEntrySnapshot
		applied  LSPRouterEntrySnapshot
		seed     *clients.MCPEntry
	}{
		{
			name: "lsp-rollback-add",
			baseline: LSPRouterEntrySnapshot{
				Client: "codex-cli", Language: "go", EntryName: legacyName,
				Present: true, URL: "http://127.0.0.1:9200/mcp",
			},
			applied: LSPRouterEntrySnapshot{Client: "codex-cli", Language: "go", EntryName: legacyName},
		},
		{
			name:     "lsp-rollback-remove",
			baseline: LSPRouterEntrySnapshot{Client: "claude-code", Language: "go", EntryName: canonicalName},
			applied: LSPRouterEntrySnapshot{
				Client: "claude-code", Language: "go", EntryName: canonicalName,
				Present: true, URL: LSPRouterURL(9137, "go"),
			},
			seed: &clients.MCPEntry{Name: canonicalName, URL: LSPRouterURL(9137, "go")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newSnapshotFakeClient(tc.baseline.Client, true)
			if tc.seed != nil {
				client.put(*tc.seed)
			}
			_, _, err := NewAPI().RestoreLSPRouterRecoveryRows(
				[]LSPRouterRecoveryRow{{Baseline: tc.baseline, Applied: &tc.applied}},
				LSPClientRouterOpts{Clients: map[string]clients.Client{tc.baseline.Client: client}},
				LSPRouterRestoreCallbacks{
					BeforeMutation: func(LSPRouterRestoreRowResult) error { return prepareErr },
				},
			)
			if !errors.Is(err, prepareErr) {
				t.Fatalf("error=%v, want durable prepare failure", err)
			}
			if client.addCalls != 0 || client.removeCalls != 0 {
				t.Fatalf("adapter calls add=%d remove=%d, want 0/0", client.addCalls, client.removeCalls)
			}
		})
	}
}

func TestMCPFrontV3_ClientAppearingBetweenCaptureAndApplyIsNotMutated(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})
	restoreRegistry := overrideLSPRouterRegistry(t)
	defer restoreRegistry()

	present := newLSPRouterFakeClient(t, "claude-code", true)
	appearing := newLSPRouterFakeClient(t, "cursor", false)
	plan, err := NewAPI().PlanLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort: 9137,
		Clients: map[string]clients.Client{
			"claude-code": present,
			"cursor":      appearing,
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	appearing.exists = true

	if _, err := NewAPI().ApplyLSPRouterClientPlan(plan, LSPRouterApplyCallbacks{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if appearing.addCalls != 0 || appearing.removeCalls != 0 || len(appearing.entries) != 0 {
		t.Fatalf("client admitted after capture was mutated: add=%d remove=%d entries=%+v",
			appearing.addCalls, appearing.removeCalls, appearing.entries)
	}
	if present.addCalls != 1 {
		t.Fatalf("captured present client addCalls=%d, want 1", present.addCalls)
	}
}

func TestMCPFrontV3_PlanPopulationAndPrestateAreFrozen(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})
	restoreRegistry := overrideLSPRouterRegistry(t)
	defer restoreRegistry()

	client := newLSPRouterFakeClient(t, "claude-code", true)
	replacement := newLSPRouterFakeClient(t, "claude-code", true)
	clientMap := map[string]clients.Client{"claude-code": client}
	plan, err := NewAPI().PlanLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort: 9137,
		Clients: clientMap,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Operations) != 1 || plan.Operations[0].Operation != "add" {
		t.Fatalf("operations=%+v, want one canonical add", plan.Operations)
	}

	const operatorURL = "https://operator.example/lsp/go/mcp"
	entryName := LSPRouterEntryName("go")
	client.entries[entryName] = clients.MCPEntry{Name: entryName, URL: operatorURL}
	clientMap["claude-code"] = replacement
	prepared := 0
	finished := 0
	preconditionConflicts := 0
	report, err := NewAPI().ApplyLSPRouterClientPlan(plan, LSPRouterApplyCallbacks{
		OnPrepared: func(LSPRouterPlannedOperation) error {
			prepared++
			return nil
		},
		OnFinished: func(LSPRouterMutationObservation) error {
			finished++
			return nil
		},
		OnPreconditionConflict: func(observation LSPRouterMutationObservation) error {
			preconditionConflicts++
			if observation.AdapterErr != ErrLSPRouterPlanPreconditionConflict {
				t.Fatalf("precondition callback AdapterErr=%v", observation.AdapterErr)
			}
			return nil
		},
	})
	if err == nil {
		t.Fatal("apply must report the precondition conflict")
	}
	if len(report.Failed) != 1 || report.Failed[0].Op != "precondition" {
		t.Fatalf("Failed=%+v, want one precondition failure", report.Failed)
	}
	if prepared != 0 || finished != 0 || preconditionConflicts != 1 || client.addCalls != 0 || client.removeCalls != 0 {
		t.Fatalf("precondition conflict callback/write counts: prepared=%d finished=%d conflicts=%d add=%d remove=%d",
			prepared, finished, preconditionConflicts, client.addCalls, client.removeCalls)
	}
	if got := client.entries[entryName].URL; got != operatorURL {
		t.Fatalf("precondition refusal overwrote operator entry: %q", got)
	}
	if replacement.addCalls != 0 || replacement.removeCalls != 0 || len(replacement.entries) != 0 {
		t.Fatalf("apply used caller-replaced adapter instead of frozen one: add=%d remove=%d entries=%+v",
			replacement.addCalls, replacement.removeCalls, replacement.entries)
	}
}

func TestMCPFrontV3_CanonicalFailurePreservesAllLegacyRoutes(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})
	restoreRegistry := overrideLSPRouterRegistry(t)
	defer restoreRegistry()
	seedLegacyLSPWorkspace(t, "codex-cli", "mcp-language-server-go-abcd")

	client := newLSPRouterFakeClient(t, "codex-cli", true)
	const legacyName = "mcp-language-server-go-abcd"
	client.entries[legacyName] = clients.MCPEntry{
		Name: legacyName,
		URL:  "http://localhost:9200/mcp",
	}
	client.addErr = errors.New("induced canonical add failure")
	plan, err := NewAPI().PlanLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort: 9137,
		Clients: map[string]clients.Client{"codex-cli": client},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Operations) != 2 {
		t.Fatalf("operations=%+v, want canonical add plus legacy remove", plan.Operations)
	}

	report, err := NewAPI().ApplyLSPRouterClientPlan(plan, LSPRouterApplyCallbacks{})
	if err == nil {
		t.Fatal("canonical add failure must be reported")
	}
	if len(report.Failed) != 1 || report.Failed[0].EntryName != LSPRouterEntryName("go") {
		t.Fatalf("Failed=%+v, want canonical add failure", report.Failed)
	}
	if client.removeCalls != 0 {
		t.Fatalf("legacy removeCalls=%d after canonical failure, want 0", client.removeCalls)
	}
	if got, ok := client.entries[legacyName]; !ok || got.URL != "http://localhost:9200/mcp" {
		t.Fatalf("canonical failure removed or changed the legacy route: %+v", client.entries)
	}
}

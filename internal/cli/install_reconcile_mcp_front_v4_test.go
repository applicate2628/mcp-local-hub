package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func emptyMCPFrontGeneration(version, generation, port int, settled bool) *mcpFrontReconcileReport {
	return &mcpFrontReconcileReport{
		Version: version, SnapshotComplete: true, Generation: generation,
		Rows: map[string]mcpFrontReconcileRow{}, Settled: settled,
		ActivePlan: &mcpFrontReconcilePlan{Generation: generation, Port: port, Rows: []string{}, Operations: []mcpFrontReconcilePlanOp{}},
	}
}

func validMCPFrontRouteActivationReport() *mcpFrontReconcileReport {
	report := emptyMCPFrontGeneration(mcpFrontReconcileVersion5, 7, 9137, true)
	report.RouteActivation = &mcpFrontRouteActivation{
		Phase:     mcpFrontRouteActivationPrepared,
		FromEpoch: api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFront, Generation: 7, Port: 9137},
		ToEpoch:   api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFrontPreparing, Generation: 8, Port: 9138},
	}
	return report
}

func cloneMCPFrontReportForRecoveryTest(t *testing.T, report *mcpFrontReconcileReport) *mcpFrontReconcileReport {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var clone mcpFrontReconcileReport
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

func TestMCPFrontRouteActivation_VersionCompatibilityIsStrict(t *testing.T) {
	for _, version := range []int{mcpFrontReconcileVersion3, mcpFrontReconcileVersion4} {
		report := emptyMCPFrontGeneration(version, 1, 9137, true)
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeMCPFrontReconcileReport(raw, "legacy.json")
		if err != nil {
			t.Fatalf("version %d decode: %v", version, err)
		}
		if decoded.RouteActivation != nil {
			t.Fatalf("version %d decoded a route activation: %+v", version, decoded.RouteActivation)
		}
		if err := validateMCPFrontReconcileReport(decoded, "legacy.json"); err != nil {
			t.Fatalf("version %d validation: %v", version, err)
		}
		report.RouteActivation = validMCPFrontRouteActivationReport().RouteActivation
		if err := validateMCPFrontReconcileReport(report, "legacy-with-phase.json"); err == nil || !strings.Contains(err.Error(), "route activation requires version 5") {
			t.Fatalf("version %d phase validation = %v", version, err)
		}
	}

	v5 := validMCPFrontRouteActivationReport()
	raw, err := json.Marshal(v5)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMCPFrontReconcileReport(raw, "v5.json")
	if err != nil {
		t.Fatalf("version 5 decode: %v", err)
	}
	if err := validateMCPFrontReconcileReport(decoded, "v5.json"); err != nil {
		t.Fatalf("version 5 validation: %v", err)
	}
	if err := mcpFrontArtifactRefusalForVersion("v5.json", mcpFrontReconcileVersion5, mcpFrontReconcileVersion4, "declares version 5"); err == nil || !strings.Contains(err.Error(), "NEWER mcphub") {
		t.Fatalf("version 4 binary refusal = %v", err)
	}
	for _, mutate := range []func(*mcpFrontRouteActivation){
		func(phase *mcpFrontRouteActivation) { phase.ToEpoch.Generation = phase.FromEpoch.Generation },
		func(phase *mcpFrontRouteActivation) { phase.ToEpoch.Port = phase.FromEpoch.Port },
		func(phase *mcpFrontRouteActivation) { phase.FromEpoch.Port = 0 },
	} {
		bad := validMCPFrontRouteActivationReport()
		mutate(bad.RouteActivation)
		if err := validateMCPFrontReconcileReport(bad, "bad-v5-epoch.json"); err == nil {
			t.Fatalf("invalid version-5 epoch was accepted: %+v", bad.RouteActivation)
		}
	}
}

func TestMCPFrontRouteActivation_NormalAdmissionReplacesPhase(t *testing.T) {
	prior := emptyMCPFrontGeneration(mcpFrontReconcileVersion4, 7, 9137, true)
	decision := mcpFrontGenerationDecision{
		Generation: 8,
		Port:       9138,
		FromEpoch:  api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFront, Generation: 7, Port: 9137},
		ToEpoch:    api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFrontPreparing, Generation: 8, Port: 9138},
		Admission:  true,
	}
	journal, err := newMCPFrontV3Journal("report.json", prior, mcpFrontReconcileVersion5, decision.Generation, decision.Port, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared := *prior
	prepared.Version = mcpFrontReconcileVersion5
	prepared.RouteActivation = &mcpFrontRouteActivation{Phase: mcpFrontRouteActivationPrepared, FromEpoch: decision.FromEpoch, ToEpoch: decision.ToEpoch}
	current := cloneMCPFrontReportForRecoveryTest(t, &prepared)
	routing := decision.FromEpoch
	persistedAdmissions := 0
	ops := mcpFrontGenerationAdmissionOps{
		routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) { return routing, nil },
		transition: func(_ context.Context, expected, next api.MCPFrontRoutingTargetSnapshot) error {
			if routing != expected {
				return errors.New("routing compare-and-set mismatch")
			}
			routing = next
			return nil
		},
		persist: func(_ string, report *mcpFrontReconcileReport) error {
			persistedAdmissions++
			current = cloneMCPFrontReportForRecoveryTest(t, report)
			return nil
		},
	}
	if err := admitMCPFrontGeneration(context.Background(), journal, decision, ops); err != nil {
		t.Fatalf("normal admission: %v", err)
	}
	if persistedAdmissions != 2 || current.Version != mcpFrontReconcileVersion5 || current.Generation != decision.Generation || current.ActivePlan == nil || current.ActivePlan.Port != decision.Port || current.RouteActivation != nil || current.GenerationAdmission != nil || routing != decision.ToEpoch {
		t.Fatalf("normal admission did not replace the phase: persists=%d report=%+v routing=%+v", persistedAdmissions, current, routing)
	}
}

func TestRecoverMCPFrontRouteActivationLocked_RestoresAndClearsAfterProof(t *testing.T) {
	current := validMCPFrontRouteActivationReport()
	from := current.RouteActivation.FromEpoch
	to := current.RouteActivation.ToEpoch
	var order []string
	ops := mcpFrontRouteActivationRecoveryOps{
		readReport: func(string) (*mcpFrontReconcileReport, error) {
			order = append(order, "read")
			return cloneMCPFrontReportForRecoveryTest(t, current), nil
		},
		routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) {
			order = append(order, "routing")
			return from, nil
		},
		projectRoute: func(context.Context, mcpFrontRouteActivation) error {
			order = append(order, "project")
			return nil
		},
		reconcile: func(context.Context) error {
			order = append(order, "reconcile")
			return nil
		},
		preflight: func(context.Context, int) error {
			order = append(order, "preflight")
			return nil
		},
		assertAbsent: func(context.Context, int) error {
			order = append(order, "absent")
			return nil
		},
		verifyDescriptor: func(mcpFrontRouteActivation, int) error {
			order = append(order, "descriptor")
			return nil
		},
		persist: func(_ string, report *mcpFrontReconcileReport) error {
			order = append(order, "persist")
			current = cloneMCPFrontReportForRecoveryTest(t, report)
			return nil
		},
	}

	recovered, err := recoverMCPFrontRouteActivationLocked(context.Background(), "report.json", ops)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered == nil || recovered.FromEpoch != from || recovered.ToEpoch != to {
		t.Fatalf("recovered activation = %+v", recovered)
	}
	if current.RouteActivation != nil {
		t.Fatalf("durable phase was not cleared: %+v", current.RouteActivation)
	}
	wantOrder := "read,routing,project,reconcile,preflight,absent,routing,descriptor,read,persist,read"
	if got := strings.Join(order, ","); got != wantOrder {
		t.Fatalf("recovery order = %s, want %s", got, wantOrder)
	}
}

func TestRecoverMCPFrontRouteActivationLocked_NoPhaseDoesNotUndoCompletion(t *testing.T) {
	report := validMCPFrontRouteActivationReport()
	report.RouteActivation = nil
	ops := mcpFrontRouteActivationRecoveryOps{
		readReport: func(string) (*mcpFrontReconcileReport, error) {
			return cloneMCPFrontReportForRecoveryTest(t, report), nil
		},
		routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) {
			t.Fatal("no-phase recovery read routing")
			return api.MCPFrontRoutingTargetSnapshot{}, nil
		},
		projectRoute: func(context.Context, mcpFrontRouteActivation) error {
			t.Fatal("no-phase recovery projected a route")
			return nil
		},
		reconcile:        func(context.Context) error { t.Fatal("no-phase recovery reconciled"); return nil },
		preflight:        func(context.Context, int) error { t.Fatal("no-phase recovery preflighted"); return nil },
		assertAbsent:     func(context.Context, int) error { t.Fatal("no-phase recovery probed destination"); return nil },
		verifyDescriptor: func(mcpFrontRouteActivation, int) error { t.Fatal("no-phase recovery verified descriptor"); return nil },
		persist:          func(string, *mcpFrontReconcileReport) error { t.Fatal("no-phase recovery persisted"); return nil },
	}
	recovered, err := recoverMCPFrontRouteActivationLocked(context.Background(), "report.json", ops)
	if err != nil || recovered != nil {
		t.Fatalf("no-phase recovery=%+v err=%v", recovered, err)
	}
}

func TestRecoverMCPFrontRouteActivationLocked_FailuresRetainPhase(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*mcpFrontReconcileReport)
		routing    api.MCPFrontRoutingTargetSnapshot
		projectErr error
		clearErr   error
		wantReason string
	}{
		{name: "bad-phase", mutate: func(report *mcpFrontReconcileReport) { report.RouteActivation.Phase = "unknown" }, wantReason: "decode-version-refusal"},
		{name: "contradictory-epoch", routing: api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFront, Generation: 99, Port: 9999}, wantReason: "routing-epoch-mismatch"},
		{name: "descriptor-mismatch", projectErr: errors.New("descriptor-mismatch: C"), wantReason: "descriptor-mismatch"},
		{name: "cancelled", projectErr: context.Canceled, wantReason: "cancelled"},
		{name: "clear-failure", clearErr: errors.New("disk full"), wantReason: "clear-failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			current := validMCPFrontRouteActivationReport()
			if tc.mutate != nil {
				tc.mutate(current)
			}
			before, _ := json.Marshal(current)
			mutations := 0
			routing := tc.routing
			if routing.State == "" {
				routing = current.RouteActivation.FromEpoch
			}
			ops := mcpFrontRouteActivationRecoveryOps{
				readReport: func(string) (*mcpFrontReconcileReport, error) {
					return cloneMCPFrontReportForRecoveryTest(t, current), nil
				},
				routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) { return routing, nil },
				projectRoute: func(context.Context, mcpFrontRouteActivation) error {
					if tc.projectErr != nil {
						return tc.projectErr
					}
					mutations++
					return nil
				},
				reconcile:        func(context.Context) error { return nil },
				preflight:        func(context.Context, int) error { return nil },
				assertAbsent:     func(context.Context, int) error { return nil },
				verifyDescriptor: func(mcpFrontRouteActivation, int) error { return nil },
				persist: func(_ string, report *mcpFrontReconcileReport) error {
					if tc.clearErr != nil {
						return tc.clearErr
					}
					current = cloneMCPFrontReportForRecoveryTest(t, report)
					return nil
				},
			}
			_, err := recoverMCPFrontRouteActivationLocked(context.Background(), "report.json", ops)
			var recoveryErr *mcpFrontRouteActivationRecoveryError
			if !errors.As(err, &recoveryErr) || recoveryErr.Reason != tc.wantReason {
				t.Fatalf("recovery error = %T %v, want reason %q", err, err, tc.wantReason)
			}
			after, _ := json.Marshal(current)
			if string(after) != string(before) {
				t.Fatal("failed recovery changed the persisted phase")
			}
			if (tc.name == "bad-phase" || tc.name == "contradictory-epoch" || tc.name == "descriptor-mismatch" || tc.name == "cancelled") && mutations != 0 {
				t.Fatalf("refused recovery performed %d mutation(s)", mutations)
			}
		})
	}
}

func TestRecoverMCPFrontRouteActivation_EmitsStableFailureAndRetainsPhase(t *testing.T) {
	current := validMCPFrontRouteActivationReport()
	from := current.RouteActivation.FromEpoch
	var events []api.SupervisorEvent
	released := false
	ops := mcpFrontRouteActivationRecoveryOps{
		acquireLock: func(context.Context, string) (func(), error) {
			return func() { released = true }, nil
		},
		readReport: func(string) (*mcpFrontReconcileReport, error) {
			return cloneMCPFrontReportForRecoveryTest(t, current), nil
		},
		routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) { return from, nil },
		projectRoute: func(context.Context, mcpFrontRouteActivation) error { return nil },
		reconcile:    func(context.Context) error { return errors.New("synthetic reconcile failure") },
		preflight:    func(context.Context, int) error { t.Fatal("preflight after reconcile failure"); return nil },
		assertAbsent: func(context.Context, int) error { t.Fatal("absence probe after reconcile failure"); return nil },
		verifyDescriptor: func(mcpFrontRouteActivation, int) error {
			t.Fatal("descriptor proof after reconcile failure")
			return nil
		},
		persist: func(string, *mcpFrontReconcileReport) error { t.Fatal("clear after reconcile failure"); return nil },
		emit:    func(event api.SupervisorEvent) error { events = append(events, event); return nil },
	}

	err := recoverMCPFrontRouteActivation(context.Background(), t.TempDir(), ops)
	var recoveryErr *mcpFrontRouteActivationRecoveryError
	if !errors.As(err, &recoveryErr) || recoveryErr.Reason != "reconcile-failed" {
		t.Fatalf("recovery error = %T %v", err, err)
	}
	if !released {
		t.Fatal("failed recovery retained transaction lock")
	}
	if len(events) != 1 || events[0].Event != "mcp-front-route-activation-recovery-failed" || events[0].Body["reason"] != "reconcile-failed" || events[0].Body["from_generation"] != 7 || events[0].Body["from_port"] != 9137 || events[0].Body["to_generation"] != 8 || events[0].Body["to_port"] != 9138 {
		t.Fatalf("failure event = %+v", events)
	}
	if current.RouteActivation == nil {
		t.Fatal("failed recovery cleared the retryable phase")
	}
}

func TestRecoverMCPFrontRouteActivation_WaitsForCLITransactionLock(t *testing.T) {
	stateDir := t.TempDir()
	lockPath := filepath.Join(stateDir, mcpFrontReconcileLockLeaf)
	holder, err := api.AcquireSupervisorLock(lockPath)
	if err != nil {
		t.Fatalf("hold CLI transaction lock: %v", err)
	}
	current := validMCPFrontRouteActivationReport()
	from := current.RouteActivation.FromEpoch
	projected := make(chan struct{}, 1)
	ops := mcpFrontRouteActivationRecoveryOps{
		acquireLock: acquireMCPFrontReconcileLockContext,
		readReport: func(string) (*mcpFrontReconcileReport, error) {
			return cloneMCPFrontReportForRecoveryTest(t, current), nil
		},
		routingState:     func() (api.MCPFrontRoutingTargetSnapshot, error) { return from, nil },
		projectRoute:     func(context.Context, mcpFrontRouteActivation) error { projected <- struct{}{}; return nil },
		reconcile:        func(context.Context) error { return nil },
		preflight:        func(context.Context, int) error { return nil },
		assertAbsent:     func(context.Context, int) error { return nil },
		verifyDescriptor: func(mcpFrontRouteActivation, int) error { return nil },
		persist: func(_ string, report *mcpFrontReconcileReport) error {
			current = cloneMCPFrontReportForRecoveryTest(t, report)
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- recoverMCPFrontRouteActivation(ctx, stateDir, ops) }()
	select {
	case <-projected:
		holder.Release()
		t.Fatal("recovery overlapped the held CLI transaction")
	case <-time.After(100 * time.Millisecond):
	}
	holder.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recovery after CLI release: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("recovery did not settle after CLI release: %v", ctx.Err())
	}
	if current.RouteActivation != nil {
		t.Fatal("recovery did not clear the phase after acquiring the CLI transaction")
	}
}

func TestRunRollbackMCPFront_V5RecoversRouteActivationBeforeRows(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.json")
	if err := writeMCPFrontReconcileReport(reportPath, validMCPFrontRouteActivationReport()); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("stop after route recovery admission")
	recoveryCalls := 0
	ops := mcpFrontRollbackOps{
		recoverRouteActivation: func(context.Context, string) error {
			recoveryCalls++
			return sentinel
		},
		readStateFile: func(context.Context, string, []string, string) ([]byte, error) {
			t.Fatal("row pin read before route recovery")
			return nil, nil
		},
		restoreSerena: func([]api.SerenaOwnedRestoreRequest) ([]api.SerenaOwnedRestoreResult, error) {
			t.Fatal("Serena restore before route recovery")
			return nil, nil
		},
		restoreLSP: func([]api.LSPRouterRecoveryRow, api.LSPClientRouterOpts, api.LSPRouterRestoreCallbacks) (*api.LSPClientRouterReport, []api.LSPRouterRestoreRowResult, error) {
			t.Fatal("LSP restore before route recovery")
			return nil, nil, nil
		},
		routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) {
			t.Fatal("routing rollback before route recovery")
			return api.MCPFrontRoutingTargetSnapshot{}, nil
		},
	}
	err := runRollbackMCPFrontWithOps(newMCPFrontTestCmd(), reportPath, ops)
	if !errors.Is(err, sentinel) || recoveryCalls != 1 {
		t.Fatalf("rollback error=%v recovery_calls=%d", err, recoveryCalls)
	}
}

func TestMCPFrontGenerationAdmission_SameGenerationDifferentPortIsByteStableConflict(t *testing.T) {
	prior := emptyMCPFrontGeneration(mcpFrontReconcileVersion3, 3, 9137, false)
	key := "lsp|claude-code|go|mcp-local-hub-lsp-go"
	prior.Rows[key] = mcpFrontReconcileRow{Surface: mcpFrontSurfaceLSP, Client: "claude-code", Language: "go", EntryName: "mcp-local-hub-lsp-go", BaselineSet: true,
		Baseline: mcpFrontEntryState{LSP: &api.LSPRouterEntrySnapshot{Client: "claude-code", Language: "go", Present: false}}}
	prior.ActivePlan.Rows = []string{key}
	prior.ActivePlan.Operations = []mcpFrontReconcilePlanOp{{RowKey: key, Operation: "add", PreState: prior.Rows[key].Baseline, IntendedState: mcpFrontEntryState{Present: true, LSP: &api.LSPRouterEntrySnapshot{Client: "claude-code", Language: "go", Present: true, URL: "http://127.0.0.1:9137/lsp/go"}}}}
	before, _ := json.Marshal(prior)
	routing := api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFrontPreparing, Generation: 3, Port: 9137}
	decision, err := classifyMCPFrontGeneration(prior, routing, 9138)
	if err == nil || decision.Admission || !strings.Contains(err.Error(), "forward-generation-not-terminal") {
		t.Fatalf("classification=%+v err=%v", decision, err)
	}
	after, _ := json.Marshal(prior)
	if string(after) != string(before) {
		t.Fatal("rejected port drift mutated the prior journal")
	}
}

func TestMCPFrontGenerationAdmission_CrossPortBumpsExactlyOnce(t *testing.T) {
	prior := emptyMCPFrontGeneration(mcpFrontReconcileVersion3, 7, 9137, false)
	from := api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFrontPreparing, Generation: 7, Port: 9137}
	decision, err := classifyMCPFrontGeneration(prior, from, 9138)
	if err != nil || !decision.Admission || decision.Generation != 8 || decision.ToEpoch.Port != 9138 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	journal, err := newMCPFrontV3Journal("record.json", prior, mcpFrontReconcileVersion4, decision.Generation, decision.Port, &api.LSPRouterClientPlan{Port: decision.Port})
	if err != nil {
		t.Fatal(err)
	}
	routing := from
	ops := mcpFrontGenerationAdmissionOps{
		routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) { return routing, nil },
		transition: func(_ context.Context, expected, next api.MCPFrontRoutingTargetSnapshot) error {
			if !mcpFrontRoutingEpochEqual(routing, expected) {
				return errors.New("CAS mismatch")
			}
			routing = next
			return nil
		},
		persist: func(string, *mcpFrontReconcileReport) error { return nil },
	}
	if err := admitMCPFrontGeneration(context.Background(), journal, decision, ops); err != nil {
		t.Fatal(err)
	}
	retry, err := classifyMCPFrontGeneration(&journal.record, routing, 9138)
	if err != nil || retry.Admission || retry.Generation != 8 {
		t.Fatalf("same-port replay bumped twice: decision=%+v err=%v", retry, err)
	}
}

func TestMCPFrontGenerationAdmission_NonTerminalShapesFailClosed(t *testing.T) {
	base := emptyMCPFrontGeneration(mcpFrontReconcileVersion4, 2, 9137, false)
	key := "x"
	base.Rows[key] = mcpFrontReconcileRow{}
	base.ActivePlan.Rows = []string{key}
	base.ActivePlan.Operations = []mcpFrontReconcilePlanOp{{RowKey: key}}
	from := api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFrontPreparing, Generation: 2, Port: 9137}
	for _, tc := range []struct {
		name   string
		mutate func(*mcpFrontReconcileReport)
	}{
		{name: "unattempted", mutate: func(*mcpFrontReconcileReport) {}},
		{name: "prepared", mutate: func(r *mcpFrontReconcileReport) {
			r.Rows[key] = mcpFrontReconcileRow{Attempt: &mcpFrontReconcileAttempt{Generation: 2, State: mcpFrontAttemptPrepared}}
		}},
		{name: "conflict", mutate: func(r *mcpFrontReconcileReport) {
			r.Rows[key] = mcpFrontReconcileRow{Attempt: &mcpFrontReconcileAttempt{Generation: 2, State: mcpFrontAttemptConflict}}
		}},
		{name: "disposition", mutate: func(r *mcpFrontReconcileReport) {
			r.Rows[key] = mcpFrontReconcileRow{Disposition: &mcpFrontRollbackDisposition{State: mcpFrontDispositionPending}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var report mcpFrontReconcileReport
			_ = json.Unmarshal(raw, &report)
			tc.mutate(&report)
			if decision, err := classifyMCPFrontGeneration(&report, from, 9138); err == nil || decision.Admission {
				t.Fatalf("shape admitted: decision=%+v err=%v", decision, err)
			}
		})
	}
}

func TestMCPFrontGenerationAdmission_StageAndEpochCrashWindowsConverge(t *testing.T) {
	from := api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFront, Generation: 1, Port: 9137}
	to := api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFrontPreparing, Generation: 2, Port: 9138}
	decision := mcpFrontGenerationDecision{Generation: 2, Port: 9138, FromEpoch: from, ToEpoch: to, Admission: true}
	t.Run("stage-before-epoch", func(t *testing.T) {
		journal := &mcpFrontReconcileJournal{reportPath: "record.json", record: *emptyMCPFrontGeneration(mcpFrontReconcileVersion4, 2, 9138, false)}
		routing, persisted, transitions := from, 0, 0
		ops := mcpFrontGenerationAdmissionOps{
			routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) { return routing, nil },
			transition: func(context.Context, api.MCPFrontRoutingTargetSnapshot, api.MCPFrontRoutingTargetSnapshot) error {
				transitions++
				return context.Canceled
			},
			persist: func(string, *mcpFrontReconcileReport) error { persisted++; return nil },
		}
		err := admitMCPFrontGeneration(context.Background(), journal, decision, ops)
		if !errors.Is(err, context.Canceled) || journal.record.GenerationAdmission == nil || routing != from || persisted != 1 || transitions != 1 {
			t.Fatalf("stage crash err=%v admission=%+v routing=%+v persists=%d transitions=%d", err, journal.record.GenerationAdmission, routing, persisted, transitions)
		}
		ops.transition = func(_ context.Context, expected, next api.MCPFrontRoutingTargetSnapshot) error {
			if routing != expected {
				return errors.New("CAS mismatch")
			}
			routing = next
			return nil
		}
		if err := reconcileMCPFrontGenerationAdmission(context.Background(), journal.reportPath, &journal.record, ops); err != nil || routing != to || journal.record.GenerationAdmission != nil {
			t.Fatalf("stage recovery err=%v routing=%+v admission=%+v", err, routing, journal.record.GenerationAdmission)
		}
	})
	t.Run("epoch-before-finalize", func(t *testing.T) {
		report := emptyMCPFrontGeneration(mcpFrontReconcileVersion4, 2, 9138, false)
		report.GenerationAdmission = &mcpFrontGenerationAdmission{Phase: mcpFrontGenerationAdmissionPrepared, FromEpoch: from, ToEpoch: to}
		failFinalize := true
		ops := mcpFrontGenerationAdmissionOps{
			routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) { return to, nil },
			transition: func(context.Context, api.MCPFrontRoutingTargetSnapshot, api.MCPFrontRoutingTargetSnapshot) error {
				t.Fatal("committed epoch was transitioned twice")
				return nil
			},
			persist: func(string, *mcpFrontReconcileReport) error {
				if failFinalize {
					return errors.New("disk full")
				}
				return nil
			},
		}
		if err := reconcileMCPFrontGenerationAdmission(context.Background(), "record.json", report, ops); err == nil || !strings.Contains(err.Error(), "generation-admission-finalize-failed") || report.GenerationAdmission == nil {
			t.Fatalf("finalize crash err=%v admission=%+v", err, report.GenerationAdmission)
		}
		failFinalize = false
		if err := reconcileMCPFrontGenerationAdmission(context.Background(), "record.json", report, ops); err != nil || report.GenerationAdmission != nil {
			t.Fatalf("finalize replay err=%v admission=%+v", err, report.GenerationAdmission)
		}
	})
}

// TestMCPFrontGenerationAdmission_CrashWindowsFinalizeBeforeCompositionRootCallbacks
// drives the rollback composition root from each durable admission crash
// window.  The injected client owners are tripwires: any Serena or LSP call
// before the finalized report is durably re-read fails the test.
func TestMCPFrontGenerationAdmission_CrashWindowsFinalizeBeforeCompositionRootCallbacks(t *testing.T) {
	from := api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFront, Generation: 1, Port: 9137}
	to := api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFrontPreparing, Generation: 2, Port: 9138}

	for _, tc := range []struct {
		name    string
		routing api.MCPFrontRoutingTargetSnapshot
	}{
		{name: "staged-before-epoch", routing: from},
		{name: "epoch-before-finalize", routing: to},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reportPath := filepath.Join(t.TempDir(), "record.json")
			// Retain one valid generation-1 Serena row so the composition root
			// reaches BOTH injected client owners after the admission is clear.
			report := newV3RollbackRecord(t, reportPath, from.Port, "claude-code")
			report.Generation = to.Generation
			report.ActivePlan.Generation = to.Generation
			report.ActivePlan.Port = to.Port
			report.GenerationAdmission = &mcpFrontGenerationAdmission{
				Phase: mcpFrontGenerationAdmissionPrepared, FromEpoch: from, ToEpoch: to,
			}
			if err := api.WriteStateFileAtomic(reportPath, report); err != nil {
				t.Fatalf("seed durable crash window: %v", err)
			}

			routing := tc.routing
			admissionDurablyCleared := false
			finalizeWrites := 0
			serenaCallsBeforeFinalize := 0
			lspCallsBeforeFinalize := 0
			serenaCallsAfterFinalize := 0
			lspCallsAfterFinalize := 0
			ops := mcpFrontRollbackOps{
				readStateFile: api.ReadStateFileBeneathRootNoFollow,
				restoreSerena: func([]api.SerenaOwnedRestoreRequest) ([]api.SerenaOwnedRestoreResult, error) {
					if !admissionDurablyCleared {
						serenaCallsBeforeFinalize++
						return nil, errors.New("Serena callback reached before durable admission finalization")
					}
					serenaCallsAfterFinalize++
					return []api.SerenaOwnedRestoreResult{{Status: api.SerenaOwnedRestoreConflict}}, nil
				},
				restoreLSP: func([]api.LSPRouterRecoveryRow, api.LSPClientRouterOpts, api.LSPRouterRestoreCallbacks) (*api.LSPClientRouterReport, []api.LSPRouterRestoreRowResult, error) {
					if !admissionDurablyCleared {
						lspCallsBeforeFinalize++
						return nil, nil, errors.New("LSP callback reached before durable admission finalization")
					}
					lspCallsAfterFinalize++
					return &api.LSPClientRouterReport{}, nil, nil
				},
				routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) { return routing, nil },
				transition: func(_ context.Context, expected, next api.MCPFrontRoutingTargetSnapshot) error {
					if routing != expected {
						return errors.New("routing compare-and-set mismatch")
					}
					routing = next
					return nil
				},
				persist: func(path string, persisted *mcpFrontReconcileReport) error {
					if persisted.GenerationAdmission != nil {
						return errors.New("admission finalization persist retained the staged handoff")
					}
					if err := api.WriteStateFileAtomic(path, persisted); err != nil {
						return err
					}
					durable, err := readMCPFrontReconcileReport(path)
					if err != nil || durable == nil || durable.GenerationAdmission != nil {
						return errors.New("admission finalization was not durably cleared")
					}
					finalizeWrites++
					admissionDurablyCleared = true
					return nil
				},
				bindMigration: func(context.Context, *mcpFrontReconcileReport, api.MCPFrontRoutingTargetSnapshot) (api.MCPFrontRoutingTargetSnapshot, error) {
					return api.MCPFrontRoutingTargetSnapshot{}, errors.New("unexpected migration binding")
				},
			}

			err := runRollbackMCPFrontWithOps(newMCPFrontTestCmd(), reportPath, ops)
			if err == nil || !strings.Contains(err.Error(), "entry was left untouched") {
				t.Fatalf("composition-root retry result=%v, want terminal injected Serena conflict", err)
			}
			if !admissionDurablyCleared || finalizeWrites != 1 || serenaCallsBeforeFinalize != 0 || lspCallsBeforeFinalize != 0 || serenaCallsAfterFinalize != 1 || lspCallsAfterFinalize != 1 {
				t.Fatalf("durable=%t finalize_writes=%d Serena(before=%d after=%d) LSP(before=%d after=%d)", admissionDurablyCleared, finalizeWrites, serenaCallsBeforeFinalize, serenaCallsAfterFinalize, lspCallsBeforeFinalize, lspCallsAfterFinalize)
			}
		})
	}
}

func TestMCPFrontGenerationAdmission_CorruptEpochAndStrictVersionsFailClosed(t *testing.T) {
	v3 := emptyMCPFrontGeneration(mcpFrontReconcileVersion3, 1, 9137, false)
	raw, _ := json.Marshal(v3)
	decoded, err := decodeMCPFrontReconcileReport(raw, "v3.json")
	if err != nil || decoded.Version != mcpFrontReconcileVersion3 || decoded.GenerationAdmission != nil {
		t.Fatalf("strict v3 recovery decode=%+v err=%v", decoded, err)
	}
	if err := validateMCPFrontReconcileReport(decoded, "v3.json"); err != nil {
		t.Fatalf("strict v3 validation: %v", err)
	}
	if err := mcpFrontArtifactRefusalForVersion("v4.json", 4, 3, "declares version 4"); err == nil || !strings.Contains(err.Error(), "NEWER mcphub") {
		t.Fatalf("v3-only downgrade refusal=%v", err)
	}
	report := emptyMCPFrontGeneration(mcpFrontReconcileVersion4, 2, 9138, false)
	report.GenerationAdmission = &mcpFrontGenerationAdmission{Phase: mcpFrontGenerationAdmissionPrepared,
		FromEpoch: api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFront, Generation: 1, Port: 9137},
		ToEpoch:   api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFrontPreparing, Generation: 2, Port: 9138}}
	ops := mcpFrontGenerationAdmissionOps{
		routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) {
			return api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFront, Generation: 9, Port: 9999}, nil
		},
		transition: func(context.Context, api.MCPFrontRoutingTargetSnapshot, api.MCPFrontRoutingTargetSnapshot) error {
			t.Fatal("corrupt epoch reached CAS")
			return nil
		},
		persist: func(string, *mcpFrontReconcileReport) error { t.Fatal("corrupt epoch reached persist"); return nil },
	}
	if err := reconcileMCPFrontGenerationAdmission(context.Background(), "record.json", report, ops); err == nil || !strings.Contains(err.Error(), "MCP_FRONT_GENERATION_ADMISSION_INCOMPLETE") || report.GenerationAdmission == nil {
		t.Fatalf("corrupt staged epoch err=%v admission=%+v", err, report.GenerationAdmission)
	}
}

func TestMCPFrontGenerationAdmission_SamePortNPlusOnePersistedDestinationFailsClosedBeforeCallbacks(t *testing.T) {
	report := emptyMCPFrontGeneration(mcpFrontReconcileVersion4, 2, 9137, false)
	report.GenerationAdmission = &mcpFrontGenerationAdmission{Phase: mcpFrontGenerationAdmissionPrepared,
		FromEpoch: api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFront, Generation: 1, Port: 9137},
		ToEpoch:   api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFrontPreparing, Generation: 2, Port: 9137}}
	reportPath := filepath.Join(t.TempDir(), "record.json")
	if err := api.WriteStateFileAtomic(reportPath, report); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}

	callbackCalls := 0
	transitionCalls := 0
	persistCalls := 0
	ops := mcpFrontRollbackOps{
		readStateFile: func(context.Context, string, []string, string) ([]byte, error) {
			callbackCalls++
			return nil, errors.New("unexpected pin read")
		},
		restoreSerena: func([]api.SerenaOwnedRestoreRequest) ([]api.SerenaOwnedRestoreResult, error) {
			callbackCalls++
			return nil, errors.New("unexpected Serena mutation")
		},
		restoreLSP: func([]api.LSPRouterRecoveryRow, api.LSPClientRouterOpts, api.LSPRouterRestoreCallbacks) (*api.LSPClientRouterReport, []api.LSPRouterRestoreRowResult, error) {
			callbackCalls++
			return nil, nil, errors.New("unexpected LSP mutation")
		},
		routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) {
			callbackCalls++
			return report.GenerationAdmission.ToEpoch, nil
		},
		transition: func(context.Context, api.MCPFrontRoutingTargetSnapshot, api.MCPFrontRoutingTargetSnapshot) error {
			transitionCalls++
			return errors.New("unexpected routing transition")
		},
		persist: func(string, *mcpFrontReconcileReport) error {
			persistCalls++
			return errors.New("unexpected journal persist")
		},
		bindMigration: func(context.Context, *mcpFrontReconcileReport, api.MCPFrontRoutingTargetSnapshot) (api.MCPFrontRoutingTargetSnapshot, error) {
			callbackCalls++
			return api.MCPFrontRoutingTargetSnapshot{}, errors.New("unexpected migration binding")
		},
	}
	err = runRollbackMCPFrontWithOps(&cobra.Command{}, reportPath, ops)
	if !errors.Is(err, api.ErrMCPFrontTargetInvalid) {
		t.Fatalf("same-port N+1 recovery error=%T %v, want typed target invalid", err, err)
	}
	after, readErr := os.ReadFile(reportPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(before) || callbackCalls != 0 || transitionCalls != 0 || persistCalls != 0 {
		t.Fatalf("same-port N+1 recovery mutated state or reached callbacks: bytes_unchanged=%t callbacks=%d transitions=%d persists=%d", string(after) == string(before), callbackCalls, transitionCalls, persistCalls)
	}
}

func TestMCPFrontGenerationAdmission_V3MissingPortMigrationUsesJournalOnly(t *testing.T) {
	prior := emptyMCPFrontGeneration(mcpFrontReconcileVersion3, 4, 9137, false)
	for _, state := range []api.MCPFrontRoutingTarget{api.MCPFrontRoutingTargetFrontPreparing, api.MCPFrontRoutingTargetGUIRestoring} {
		t.Run(string(state), func(t *testing.T) {
			unbound := api.MCPFrontRoutingTargetSnapshot{State: state, Generation: 4}
			transitioned := 0
			bound, err := bindMCPFrontRoutingPortForMigrationWithOps(context.Background(), prior, unbound, mcpFrontRoutingMigrationOps{
				transition: func(_ context.Context, expected, next api.MCPFrontRoutingTargetSnapshot) error {
					transitioned++
					if expected != unbound || next.State != state || next.Generation != 4 || next.Port != 9137 {
						t.Fatalf("migration inferred a non-journal epoch: expected=%+v next=%+v", expected, next)
					}
					return nil
				},
			})
			if err != nil || transitioned != 1 || bound.Port != 9137 {
				t.Fatalf("bound=%+v transitions=%d err=%v", bound, transitioned, err)
			}
		})
	}
	t.Run("stable-front-requires-intended-and-readiness", func(t *testing.T) {
		unbound := api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFront, Generation: 4}
		verified, preflighted, transitioned := 0, 0, 0
		bound, err := bindMCPFrontRoutingPortForMigrationWithOps(context.Background(), prior, unbound, mcpFrontRoutingMigrationOps{
			verify: func(got *mcpFrontReconcileReport) error {
				verified++
				if got != prior {
					t.Fatal("wrong journal proof")
				}
				return nil
			},
			preflight: func(_ context.Context, port int) error {
				preflighted++
				if port != 9137 {
					t.Fatalf("preflight port=%d", port)
				}
				return nil
			},
			transition: func(_ context.Context, expected, next api.MCPFrontRoutingTargetSnapshot) error {
				transitioned++
				if expected != unbound || next.Port != 9137 {
					t.Fatal("wrong exact bind")
				}
				return nil
			},
		})
		if err != nil || bound.Port != 9137 || verified != 1 || preflighted != 1 || transitioned != 1 {
			t.Fatalf("bound=%+v verify=%d preflight=%d transition=%d err=%v", bound, verified, preflighted, transitioned, err)
		}
	})
}

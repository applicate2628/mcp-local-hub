package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	journal, err := newMCPFrontV3Journal("record.json", prior, mcpFrontReconcileReportVersion, decision.Generation, decision.Port, &api.LSPRouterClientPlan{Port: decision.Port})
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
	base := emptyMCPFrontGeneration(mcpFrontReconcileReportVersion, 2, 9137, false)
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
		journal := &mcpFrontReconcileJournal{reportPath: "record.json", record: *emptyMCPFrontGeneration(mcpFrontReconcileReportVersion, 2, 9138, false)}
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
		report := emptyMCPFrontGeneration(mcpFrontReconcileReportVersion, 2, 9138, false)
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
	report := emptyMCPFrontGeneration(mcpFrontReconcileReportVersion, 2, 9138, false)
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
	report := emptyMCPFrontGeneration(mcpFrontReconcileReportVersion, 2, 9137, false)
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

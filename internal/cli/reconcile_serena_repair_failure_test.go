// Package cli — tests for surfacing a FAILED serena registry/intent self-heal
// to reconcile callers (bot PR #590 P2, "Surface Serena repair failures to
// reconcile callers").
//
// The defect: handleReconcile fed RepairSerenaIntentFromRegistry's error to the
// audit log ONLY and still answered OK. Because a failed repair never lands the
// orphan in supervisor-intent.json, the orphan is absent from the drift report
// too — so the response was byte-identical to a healthy pass and `mcphub
// reconcile --apply` printed `no drift — scheduler state and intent are already
// aligned` and exited 0 while the registered workspace stayed unusable.
//
// Two layers are pinned here:
//   - the SERVER layer (handleReconcile) must put the failure in
//     ReconcileResponse.SerenaRepairError while still answering OK, because a
//     serena orphan must not fail the drift report `mcphub stop` / `mcphub
//     restart` also dispatch;
//   - the COMMAND layer (`mcphub reconcile`) must print it, must not claim
//     alignment, and in --apply mode must exit non-zero.
package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

// malformedSerenaManifestYAML is unparseable YAML, which makes the repair's
// loadSerenaCatalogManifest fail at the point it materializes the missing
// daemon rows — the "malformed catalog" cause the finding names, reached only
// after an orphan has been classified and the §7.1 introduce guard passed.
const malformedSerenaManifestYAML = "name: serena\nbase_args: [unterminated\n  : : :\n"

// seedRepairFailureFixture builds the one state the repair errors on: a healthy
// spec-bearing serena daemon in the intent (so HasRuntimeSpecRow passes and the
// repair does not merely DEFER), an orphaned serena registry row (so there IS
// something to materialize), and a catalog manifest that cannot be parsed.
// Returns the fixture and the orphan's workspace key.
func seedRepairFailureFixture(t *testing.T) (*reconcileTestFixture, string) {
	t.Helper()
	manifestDir := t.TempDir()
	seedSerenaManifest(t, manifestDir, malformedSerenaManifestYAML)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)

	healthyWS := t.TempDir()
	healthyKey := api.WorkspaceKey(healthyWS)
	healthyTaskName := `\mcp-local-hub-serena-` + healthyKey
	fx := newReconcileTestFixture(t, &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:  healthyTaskName,
				Server:    api.SerenaServerName,
				Daemon:    "serena-" + healthyKey,
				Command:   "mcphub",
				Args:      []string{"daemon", "serena-proxy", "--task-name", healthyTaskName},
				Workspace: healthyWS,
				Port:      9150,
				RuntimeSpec: &api.DaemonRuntimeSpec{
					SpecVersion:   1,
					ChildCommand:  "uvx",
					UpstreamPort:  19150,
					ExternalPort:  9150,
					WorkspacePath: healthyWS,
				},
			},
		},
	})

	// Seed the orphan AFTER the fixture redirects LOCALAPPDATA/XDG_STATE_HOME.
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	orphanKey := seedOrphanSerenaRegistryRow(t, regPath, t.TempDir(), 9151)
	installSchedulerListFake(t, []scheduler.TaskStatus{})
	return fx, orphanKey
}

// TestReconcileIPC_ApplyRepairFailureIsReportedNotSwallowed is the server-side
// half: the repair fails, the response still answers OK with a valid drift
// report, but SerenaRepairError names the failure so the caller cannot read
// silence as success.
func TestReconcileIPC_ApplyRepairFailureIsReportedNotSwallowed(t *testing.T) {
	fx, orphanKey := seedRepairFailureFixture(t)

	conn := newFakeIPCConn()
	req := api.IPCRequest{ID: 2001, Cmd: "reconcile", Args: map[string]any{"apply": true}}
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	resp, body := decodeReconcileResponse(t, conn)

	// The request itself still succeeds — a serena orphan must not fail the
	// reconcile that `mcphub stop` / `mcphub restart` also dispatch.
	if !resp.OK || resp.Error != nil {
		t.Fatalf("reconcile frame must stay OK on a serena repair failure; got OK=%v err=%+v", resp.OK, resp.Error)
	}
	if body.SerenaRepairError == "" {
		t.Fatalf("SerenaRepairError is empty: the repair failure was swallowed, so this response is indistinguishable from a healthy pass (drift_count=%d, serena_orphans_repaired=%d)",
			body.DriftCount, body.SerenaOrphansRepaired)
	}
	if body.SerenaRepairOutcome != api.SerenaIntentRepairOutcomeError {
		t.Errorf("SerenaRepairOutcome = %q, want %q", body.SerenaRepairOutcome, api.SerenaIntentRepairOutcomeError)
	}
	if !strings.Contains(body.SerenaRepairError, "serena intent repair") {
		t.Errorf("SerenaRepairError = %q, want the repair's own error text", body.SerenaRepairError)
	}
	if body.SerenaOrphansRepaired != 0 {
		t.Errorf("SerenaOrphansRepaired = %d, want 0 (nothing was materialized)", body.SerenaOrphansRepaired)
	}

	// Precondition for the whole finding: the orphan really is invisible in
	// every other field, which is why SerenaRepairError has to exist.
	intentPath := filepath.Join(fx.deps.stateDir, "supervisor-intent.json")
	onDisk, rerr := api.ReadSupervisorIntent(intentPath)
	if rerr != nil {
		t.Fatalf("re-read supervisor-intent.json: %v", rerr)
	}
	if onDisk.HasSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Fatalf("precondition broken: the repair actually materialized %s, so this test is not exercising a failure", orphanKey)
	}
	for _, d := range body.Drift {
		if strings.Contains(d.TaskName, orphanKey) {
			t.Fatalf("precondition broken: the orphan appears in the drift report as %s", d.TaskName)
		}
	}
}

// The dry-run preview error must be surfaced too: a swallowed one reported
// `serena orphans would repair: 0` for a preview that never reached a verdict.
// It must still NOT fail the request (a dry run mutates nothing).
func TestReconcileIPC_DryRunPreviewFailureIsReported(t *testing.T) {
	fx, _ := seedRepairFailureFixture(t)

	conn := newFakeIPCConn()
	req := api.IPCRequest{ID: 2002, Cmd: "reconcile", Args: map[string]any{"apply": false}}
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	resp, body := decodeReconcileResponse(t, conn)
	if !resp.OK || resp.Error != nil {
		t.Fatalf("dry-run reconcile must never fail; got OK=%v err=%+v", resp.OK, resp.Error)
	}
	if body.SerenaRepairError == "" {
		t.Fatal("SerenaRepairError is empty on a failed dry-run preview; `serena orphans would repair: 0` would then be reported for a preview that got no verdict")
	}
	if body.SerenaRepairOutcome != api.SerenaIntentRepairOutcomeError {
		t.Errorf("SerenaRepairOutcome = %q, want %q", body.SerenaRepairOutcome, api.SerenaIntentRepairOutcomeError)
	}
	if !body.DryRun {
		t.Error("DryRun = false on an apply=false request")
	}
}

// TestReconcileCmd_ApplyExitsNonZeroOnSerenaRepairFailure is the command-side
// half of the finding: exit 0 + `no drift` was the operator-visible defect.
func TestReconcileCmd_ApplyExitsNonZeroOnSerenaRepairFailure(t *testing.T) {
	uninstall := setReconcileDialFnForTest(func(ctx context.Context, apply bool) (api.ReconcileResponse, error) {
		return api.ReconcileResponse{
			DryRun:              !apply,
			DriftCount:          0,
			AppliedCount:        0,
			SerenaRepairOutcome: api.SerenaIntentRepairOutcomeError,
			SerenaRepairError:   "serena intent repair: load serena catalog manifest: parse serena manifest: boom",
		}, nil
	})
	defer uninstall()

	cmd := newReconcileCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--apply"})
	err := cmd.Execute()
	out := buf.String()

	if err == nil {
		t.Fatalf("Execute returned nil: `mcphub reconcile --apply` still exits 0 over a failed self-heal; output:\n%s", out)
	}
	if !errors.Is(err, errSerenaRepairFailed) {
		t.Errorf("error is not errSerenaRepairFailed; got %v", err)
	}
	if !strings.Contains(err.Error(), "parse serena manifest: boom") {
		t.Errorf("error does not carry the underlying cause; got %v", err)
	}
	if !strings.Contains(out, "serena orphan repair FAILED") {
		t.Errorf("report does not name the failed repair; got:\n%s", out)
	}
	if strings.Contains(out, "no drift — scheduler state and intent are already aligned") {
		t.Errorf("report still claims alignment while the repair failed; got:\n%s", out)
	}
}

// Dry-run keeps exit 0 (it mutates nothing and promised only a report) but must
// still PRINT the failure.
func TestReconcileCmd_DryRunPrintsRepairFailureWithoutFailing(t *testing.T) {
	uninstall := setReconcileDialFnForTest(func(ctx context.Context, apply bool) (api.ReconcileResponse, error) {
		return api.ReconcileResponse{
			DryRun:              true,
			DriftCount:          0,
			SerenaRepairOutcome: api.SerenaIntentRepairOutcomeError,
			SerenaRepairError:   "serena intent repair: build dynamic-pool manifest: boom",
		}, nil
	})
	defer uninstall()

	cmd := newReconcileCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("dry-run must not fail on a preview error; got %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "serena orphan repair PREVIEW failed") {
		t.Errorf("dry-run report does not name the failed preview; got:\n%s", out)
	}
	if !strings.Contains(out, "build dynamic-pool manifest: boom") {
		t.Errorf("dry-run report does not carry the cause; got:\n%s", out)
	}
}

// A clean pass keeps its unqualified wording and exit 0 — the new branch must
// not colour normal output.
func TestReconcileCmd_ApplyStaysCleanWhenRepairSucceeded(t *testing.T) {
	uninstall := setReconcileDialFnForTest(func(ctx context.Context, apply bool) (api.ReconcileResponse, error) {
		return api.ReconcileResponse{
			DryRun:              !apply,
			DriftCount:          0,
			SerenaRepairOutcome: api.SerenaIntentRepairOutcomeCompleted,
		}, nil
	})
	defer uninstall()

	cmd := newReconcileCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--apply"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "no drift — scheduler state and intent are already aligned") {
		t.Errorf("clean apply lost its unqualified alignment line; got:\n%s", out)
	}
	if strings.Contains(out, "repair FAILED") {
		t.Errorf("clean apply printed a repair failure; got:\n%s", out)
	}
}

// TestReconcileCmd_IncompleteSerenaOutcomesAreFailClosed covers the new
// additive outcome field separately from actual repair errors above. A lock
// skip is retryable, but not completed; absent and future values are version
// skew, and therefore likewise cannot certify fleet alignment. Apply must
// fail loud after printing the report, while dry-run remains an honest,
// non-mutating report with exit 0.
func TestReconcileCmd_IncompleteSerenaOutcomesAreFailClosed(t *testing.T) {
	tests := []struct {
		name             string
		apply            bool
		outcome          api.SerenaIntentRepairOutcome
		incomplete       []api.SerenaIntentRepairIncomplete
		wantApplyFailure bool
		wantDetail       string
	}{
		{
			name:             "apply held removal generation",
			apply:            true,
			outcome:          api.SerenaIntentRepairOutcomeIncompleteRemovalFence,
			incomplete:       []api.SerenaIntentRepairIncomplete{{WorkspaceKey: "abcd1234", Reason: api.SerenaIntentRepairIncompleteHolderLive}},
			wantApplyFailure: true,
			wantDetail:       "holder_live",
		},
		{
			name:             "apply fresh legacy removal generation",
			apply:            true,
			outcome:          api.SerenaIntentRepairOutcomeIncompleteRemovalFence,
			incomplete:       []api.SerenaIntentRepairIncomplete{{WorkspaceKey: "abcd1234", Reason: api.SerenaIntentRepairIncompleteLegacyLeaseFresh}},
			wantApplyFailure: true,
			wantDetail:       "legacy_lease_fresh",
		},
		{
			name:             "apply mismatched removal generation",
			apply:            true,
			outcome:          api.SerenaIntentRepairOutcomeIncompleteRemovalFence,
			incomplete:       []api.SerenaIntentRepairIncomplete{{WorkspaceKey: "abcd1234", Reason: api.SerenaIntentRepairIncompleteGenerationMismatch}},
			wantApplyFailure: true,
			wantDetail:       "generation_mismatch",
		},
		{
			name:             "apply removal generation probe failure",
			apply:            true,
			outcome:          api.SerenaIntentRepairOutcomeIncompleteRemovalFence,
			incomplete:       []api.SerenaIntentRepairIncomplete{{WorkspaceKey: "abcd1234", Reason: api.SerenaIntentRepairIncompleteGenerationProbeFailed}},
			wantApplyFailure: true,
			wantDetail:       "generation_probe_failed",
		},
		{
			name:       "preview fresh legacy removal generation",
			outcome:    api.SerenaIntentRepairOutcomeIncompleteRemovalFence,
			incomplete: []api.SerenaIntentRepairIncomplete{{WorkspaceKey: "abcd1234", Reason: api.SerenaIntentRepairIncompleteLegacyLeaseFresh}},
			wantDetail: "legacy_lease_fresh",
		},
		{
			name:             "apply registry lock",
			apply:            true,
			outcome:          api.SerenaIntentRepairOutcomeSkippedRegistryLock,
			wantApplyFailure: true,
			wantDetail:       "registry lock",
		},
		{
			name:             "apply intent lock",
			apply:            true,
			outcome:          api.SerenaIntentRepairOutcomeSkippedIntentLock,
			wantApplyFailure: true,
			wantDetail:       "supervisor-intent lock",
		},
		{
			name:             "apply removal fence probe",
			apply:            true,
			outcome:          api.SerenaIntentRepairOutcomeSkippedRemovalFenceProbe,
			wantApplyFailure: true,
			wantDetail:       "liveness fence",
		},
		{
			name:             "apply missing outcome",
			apply:            true,
			wantApplyFailure: true,
			wantDetail:       "unavailable",
		},
		{
			name:             "apply unknown outcome",
			apply:            true,
			outcome:          api.SerenaIntentRepairOutcome("future_outcome"),
			wantApplyFailure: true,
			wantDetail:       "unknown",
		},
		{
			name:       "preview registry lock",
			outcome:    api.SerenaIntentRepairOutcomeSkippedRegistryLock,
			wantDetail: "registry lock",
		},
		{
			name:       "preview intent lock",
			outcome:    api.SerenaIntentRepairOutcomeSkippedIntentLock,
			wantDetail: "supervisor-intent lock",
		},
		{
			name:       "preview removal fence probe",
			outcome:    api.SerenaIntentRepairOutcomeSkippedRemovalFenceProbe,
			wantDetail: "liveness fence",
		},
		{
			name:       "preview missing outcome",
			wantDetail: "unavailable",
		},
		{
			name:       "preview unknown outcome",
			outcome:    api.SerenaIntentRepairOutcome("future_outcome"),
			wantDetail: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uninstall := setReconcileDialFnForTest(func(context.Context, bool) (api.ReconcileResponse, error) {
				return api.ReconcileResponse{
					DryRun:                 !tt.apply,
					DriftCount:             0,
					SerenaRepairOutcome:    tt.outcome,
					SerenaRepairIncomplete: tt.incomplete,
				}, nil
			})
			defer uninstall()

			cmd := newReconcileCmdReal()
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			if tt.apply {
				cmd.SetArgs([]string{"--apply"})
			}
			err := cmd.Execute()
			out := buf.String()
			if tt.wantApplyFailure {
				if !errors.Is(err, errSerenaRepairIncomplete) {
					t.Fatalf("error = %v, want errSerenaRepairIncomplete", err)
				}
			} else if err != nil {
				t.Fatalf("dry-run must keep exit 0; got %v", err)
			}
			if !strings.Contains(out, tt.wantDetail) {
				t.Errorf("report does not name the incomplete classification detail %q; got:\n%s", tt.wantDetail, out)
			}
			if strings.Contains(out, "no drift — scheduler state and intent are already aligned") {
				t.Errorf("report claimed alignment for an incomplete Serena outcome; got:\n%s", out)
			}
			if tt.apply && !strings.Contains(out, "serena orphan repair skipped") {
				t.Errorf("apply report does not mark a skip; got:\n%s", out)
			}
			if !tt.apply && !strings.Contains(out, "serena orphan repair PREVIEW skipped") {
				t.Errorf("dry-run report does not mark a preview skip; got:\n%s", out)
			}
		})
	}
}

func TestPrintReconcileTable_ReportsSerenaRecoveryReasons(t *testing.T) {
	tests := []struct {
		name     string
		dryRun   bool
		recovery api.SerenaIntentRepairRecovery
		wantVerb string
	}{
		{
			name:     "apply exact generation",
			recovery: api.SerenaIntentRepairRecovery{WorkspaceKey: "abcd1234", Reason: api.SerenaIntentRepairRecoveryGenerationReclaimed},
			wantVerb: "serena pending removals recovered",
		},
		{
			name:     "preview expired legacy lease",
			dryRun:   true,
			recovery: api.SerenaIntentRepairRecovery{WorkspaceKey: "efgh5678", Reason: api.SerenaIntentRepairRecoveryLegacyLeaseExpired},
			wantVerb: "serena pending removals would recover",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := printReconcileTable(&buf, api.ReconcileResponse{
				DryRun:                tt.dryRun,
				SerenaRepairOutcome:   api.SerenaIntentRepairOutcomeCompleted,
				SerenaRepairRecovered: []api.SerenaIntentRepairRecovery{tt.recovery},
			})
			if err != nil {
				t.Fatalf("printReconcileTable: %v", err)
			}
			out := buf.String()
			for _, want := range []string{tt.wantVerb, tt.recovery.WorkspaceKey, string(tt.recovery.Reason)} {
				if !strings.Contains(out, want) {
					t.Errorf("report missing %q; got:\n%s", want, out)
				}
			}
		})
	}
}

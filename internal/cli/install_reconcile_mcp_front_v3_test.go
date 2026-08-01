package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

func v3LSPSnapshot(client, language, entryName string, present bool, url string) api.LSPRouterEntrySnapshot {
	return api.LSPRouterEntrySnapshot{
		Client: client, Language: language, EntryName: entryName, Present: present, URL: url,
	}
}

func v3LSPAdd(client, language, entryName string, from, to api.LSPRouterEntrySnapshot) api.LSPRouterPlannedOperation {
	return api.LSPRouterPlannedOperation{
		Client: client, Language: language, EntryName: entryName, Operation: "add",
		PreState: from, IntendedState: to,
	}
}

func v3LSPRemove(client, language, entryName string, from api.LSPRouterEntrySnapshot) api.LSPRouterPlannedOperation {
	return api.LSPRouterPlannedOperation{
		Client: client, Language: language, EntryName: entryName, Operation: "remove",
		PreState:      from,
		IntendedState: v3LSPSnapshot(client, language, entryName, false, ""),
	}
}

// --- version-3 rollback fixtures -------------------------------------------
//
// The rollback-side command tests below used to hand-write the version-2 body
// (`port` / `serena` / `lsp` / `pins` as TOP-LEVEL keys) while stamping the
// current version number on it. Once the strict version-3 decoder shipped,
// every one of those fixtures died at the PARSE step with
// `json: unknown field "lsp"` — so the semantic refusals they were written to
// pin (a missing recovery section, an unreadable pin) were never reached and
// the tests were asserting against a code path that no longer runs.
//
// These builders are the fix for that class: a fixture is now produced from the
// SAME structs the production writer persists, so a future schema change breaks
// them at compile time instead of silently moving the failure to the decoder.

// v3SerenaRowKeyFor is the exact row key of one client's serena recovery row.
func v3SerenaRowKeyFor(client string) string {
	return mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, client, "", "serena")
}

// newV3RollbackRecord builds a MINIMAL but fully VALID version-3 journal for a
// rollback-side test: generation 1, one serena row that owns its pin and
// carries a same-generation applied receipt, and an active plan that agrees
// with it. It passes validateMCPFrontReconcileReport as-is, so a test can
// invalidate exactly ONE thing and know that is what the refusal is about.
//
// It also materialises the pin: the pin file lives inside the report's own pin
// directory (verifyMCPFrontSerenaPins refuses a path outside it before it ever
// looks at the bytes) and its recorded SHA-256 is the real digest.
func newV3RollbackRecord(t *testing.T, reportPath string, port int, client string) mcpFrontReconcileReport {
	t.Helper()
	pinDir := filepath.Join(mcpFrontReconcilePinDir(reportPath), client)
	if err := os.MkdirAll(pinDir, 0o700); err != nil {
		t.Fatalf("create pin dir: %v", err)
	}
	pinPath := filepath.Join(pinDir, "pre-reconcile.json")
	pinBody := []byte(`{"mcpServers":{"serena":{"url":"http://127.0.0.1:9125/serena/mcp"}}}`)
	if err := os.WriteFile(pinPath, pinBody, 0o600); err != nil {
		t.Fatalf("write pin: %v", err)
	}
	sum, err := fileSHA256(pinPath)
	if err != nil {
		t.Fatalf("hash pin: %v", err)
	}
	return newV3RollbackRecordWithPin(t, port, client, mcpFrontSerenaPin{
		Client: client, Origin: "rolling", Path: pinPath, SHA256: sum,
	})
}

// newV3RollbackRecordWithPin is the same shape with a caller-supplied pin, for
// the tests whose subject IS the pin (missing file, wrong digest, escaping
// path). It does not touch the filesystem.
func newV3RollbackRecordWithPin(t *testing.T, port int, client string, pin mcpFrontSerenaPin) mcpFrontReconcileReport {
	t.Helper()
	baseline := mcpFrontSerenaState("pre-reconcile-fingerprint")
	intended := mcpFrontSerenaState("front-port-fingerprint")
	key := v3SerenaRowKeyFor(client)
	row := mcpFrontReconcileRow{
		Surface: mcpFrontSurfaceSerena, Client: client, EntryName: "serena",
		Baseline: baseline, BaselineSet: true, Pin: &pin,
		Attempt: &mcpFrontReconcileAttempt{
			Generation: 1, Operation: "add",
			PreState: baseline, IntendedState: intended,
			State: mcpFrontAttemptApplied,
		},
		Applied: &mcpFrontAppliedReceipt{Generation: 1, Port: port, PostState: intended},
	}
	return mcpFrontReconcileReport{
		Version:          mcpFrontReconcileReportVersion,
		SnapshotComplete: true,
		Generation:       1,
		Rows: map[string]mcpFrontReconcileRow{
			key: row,
		},
		ActivePlan: &mcpFrontReconcilePlan{
			Generation: 1, Port: port, Rows: []string{key},
			Operations: []mcpFrontReconcilePlanOp{
				mcpFrontPlanOperationForRow(key, row, "add", baseline, intended),
			},
		},
	}
}

// v3RecordAsMap round-trips a record through its own JSON encoding so a test
// can delete or null ONE key and seed bytes the production reader will actually
// meet on disk. Building the perturbation from the encoded form (rather than a
// hand-typed literal) is what keeps these fixtures honest about the real wire
// shape.
func v3RecordAsMap(t *testing.T, record mcpFrontReconcileReport) map[string]any {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal v3 record: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("re-decode v3 record: %v", err)
	}
	return out
}

// v3Journal builds a persisted version-3 journal for the tests below.
//
// It installs the hermetic client-config env (mcpFrontPR588Env) even though most
// of these tests read only the journal artifact. Reason: several of them drive
// runReconcileMCPFront / runRollbackMCPFront, and those reach production code
// that falls back to the LIVE registry when opts.Clients is nil
// (api.RestoreLSPRouterRecoveryRows, lsp_client_router_snapshot.go:199). Before
// this line, TestMCPFrontV3_RollbackCallerRereadsDurableStateBeforeRetirement
// resolved the operator's REAL C:\Users\<user>\.claude.json — it survived only
// because its row named a client ("missing-test-client") that no adapter
// matches. A sibling test naming a real client would have written there. Caught
// by the client-config sandbox audit
// (internal/clients/config_path_sandbox_audit.go); isolating the SHARED fixture
// rather than the one test that tripped is what keeps the next test in this file
// safe by default.
//
// No test in this file calls both mcpFrontPR588Env and v3Journal, so this
// installs exactly one home redirect per test.
func v3Journal(t *testing.T, port int, prior *mcpFrontReconcileReport, ops ...api.LSPRouterPlannedOperation) *mcpFrontReconcileJournal {
	t.Helper()
	tmp := mcpFrontPR588Env(t)
	reportPath := filepath.Join(tmp, "recovery.json")
	journal, err := newMCPFrontV3Journal(reportPath, prior, port, &api.LSPRouterClientPlan{
		Port: port, Operations: ops,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.persist(); err != nil {
		t.Fatal(err)
	}
	return journal
}

// v3JournalAt is the same durable journal fixture at the report-path seam used
// by the real forward command. It keeps retry tests on the actual refusal
// boundary instead of reproducing that boundary in a test-local helper.
func v3JournalAt(t *testing.T, reportPath string, port int, prior *mcpFrontReconcileReport, ops ...api.LSPRouterPlannedOperation) *mcpFrontReconcileJournal {
	t.Helper()
	journal, err := newMCPFrontV3Journal(reportPath, prior, port, &api.LSPRouterClientPlan{
		Port: port, Operations: ops,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.persist(); err != nil {
		t.Fatal(err)
	}
	return journal
}

func assertMCPFrontBytesEqual(t *testing.T, path string, want []byte, subject string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s %s: %v", subject, path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s bytes changed across a refusal", subject)
	}
}

// setupMCPFrontForwardRefusalTest installs the actual route and ownership
// proofs, but leaves a client config under the test's redirected profile. The
// subsequent forward call is therefore a real command retry whose only allowed
// effect is its refusal before plan capture or either adapter mutation.
func setupMCPFrontForwardRefusalTest(t *testing.T) (reportPath string, port int, configPath string, configBefore []byte) {
	t.Helper()
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath = withMCPFrontReportPathSeam(t)
	port, cleanup := startTestRouteServer(t)
	t.Cleanup(cleanup)
	seedSupervisorOwnedRoutePort(t, port)
	if err := api.NewAPI().SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	configPath = seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena":                     map[string]any{"url": "http://127.0.0.1:9125/serena/mcp"},
		api.LSPRouterEntryName("go"): map[string]any{"url": "http://127.0.0.1:9125/lsp/go/mcp"},
	})
	var err error
	configBefore, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read initial redirected config: %v", err)
	}
	return reportPath, port, configPath, configBefore
}

func TestMCPFrontV3_LSPArtifactRoundTripPreservesCanonicalAndMultipleLegacyRows(t *testing.T) {
	const client, language = "codex-cli", "go"
	canonicalName := api.LSPRouterEntryName(language)
	canonical := v3LSPSnapshot(client, language, canonicalName, true, api.LSPRouterURL(9125, language))
	legacyOne := v3LSPSnapshot(client, language, "mcp-language-server-go-one", true, "http://127.0.0.1:9200/mcp")
	legacyTwo := v3LSPSnapshot(client, language, "mcp-language-server-go-two", true, "http://127.0.0.1:9201/mcp")
	journal := v3Journal(t, 9137, nil,
		v3LSPAdd(client, language, canonicalName, canonical,
			v3LSPSnapshot(client, language, canonicalName, true, api.LSPRouterURL(9137, language))),
		v3LSPRemove(client, language, legacyOne.EntryName, legacyOne),
		v3LSPRemove(client, language, legacyTwo.EntryName, legacyTwo),
	)
	reloaded, err := readMCPFrontReconcileReport(journal.reportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, baseline := range []api.LSPRouterEntrySnapshot{canonical, legacyOne, legacyTwo} {
		key := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, client, language, baseline.EntryName)
		row, ok := reloaded.Rows[key]
		if !ok || row.Baseline.LSP == nil || !mcpFrontStateEqual(row.Baseline, mcpFrontLSPState(baseline)) {
			t.Fatalf("row %q lost exact baseline: %+v", key, row)
		}
	}
}

func TestMCPFrontV3_PartialCrossPortRetryKeepsPerRowAppliedPorts(t *testing.T) {
	name := api.LSPRouterEntryName("go")
	preA := v3LSPSnapshot("claude-code", "go", name, false, "")
	atA := v3LSPSnapshot("claude-code", "go", name, true, api.LSPRouterURL(9137, "go"))
	firstOp := v3LSPAdd("claude-code", "go", name, preA, atA)
	first := v3Journal(t, 9137, nil, firstOp)
	if err := first.prepareLSPOperation(firstOp); err != nil {
		t.Fatal(err)
	}
	if err := first.finishLSPOperation(api.LSPRouterMutationObservation{
		Operation: firstOp, ObservedState: atA, Invoked: true,
	}); err != nil {
		t.Fatal(err)
	}
	prior, err := readMCPFrontReconcileReport(first.reportPath)
	if err != nil {
		t.Fatal(err)
	}
	atB := v3LSPSnapshot("claude-code", "go", name, true, api.LSPRouterURL(9138, "go"))
	retryOp := v3LSPAdd("claude-code", "go", name, atA, atB)
	retry := v3Journal(t, 9138, prior, retryOp)
	if err := retry.prepareLSPOperation(retryOp); err != nil {
		t.Fatal(err)
	}
	if err := retry.finishLSPOperation(api.LSPRouterMutationObservation{
		Operation: retryOp, ObservedState: atB, Invoked: true,
	}); err != nil {
		t.Fatal(err)
	}
	row := retry.record.Rows[mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "claude-code", "go", name)]
	receipt, uncertain := effectiveMCPFrontAppliedReceipt(row)
	if uncertain || receipt == nil || receipt.Port != 9138 {
		t.Fatalf("retry receipt=%+v uncertain=%v, want port 9138", receipt, uncertain)
	}
}

func TestMCPFrontV3_EveryMutationRequiresDurablePrepared(t *testing.T) {
	name := api.LSPRouterEntryName("go")
	pre := v3LSPSnapshot("claude-code", "go", name, false, "")
	post := v3LSPSnapshot("claude-code", "go", name, true, api.LSPRouterURL(9137, "go"))
	op := v3LSPAdd("claude-code", "go", name, pre, post)
	journal := v3Journal(t, 9137, nil, op)
	err := journal.finishLSPOperation(api.LSPRouterMutationObservation{
		Operation: op, ObservedState: post, Invoked: true,
	})
	if err == nil || !strings.Contains(err.Error(), "no durable prepared attempt") {
		t.Fatalf("finish without durable prepare err=%v", err)
	}
}

func TestMCPFrontV3_ReentrySettlesWriteReceiptCrashWindows(t *testing.T) {
	name := api.LSPRouterEntryName("go")
	pre := v3LSPSnapshot("claude-code", "go", name, false, "")
	post := v3LSPSnapshot("claude-code", "go", name, true, api.LSPRouterURL(9137, "go"))
	op := v3LSPAdd("claude-code", "go", name, pre, post)
	journal := v3Journal(t, 9137, nil, op)
	if err := journal.prepareLSPOperation(op); err != nil {
		t.Fatal(err)
	}
	reloaded, err := readMCPFrontReconcileReport(journal.reportPath)
	if err != nil {
		t.Fatal(err)
	}
	uncertain, err := settleMCPFrontReconcileAttempts(journal.reportPath, reloaded)
	if err != nil {
		t.Fatalf("settlement must make the pending marker durable: %v", err)
	}
	if len(uncertain) != 1 {
		t.Fatalf("re-entry must not equality-promote a prepared attempt; uncertain=%v", uncertain)
	}
	row := reloaded.Rows[mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "claude-code", "go", name)]
	if row.Attempt.State != mcpFrontAttemptPrepared || row.Applied != nil ||
		row.Disposition == nil || row.Disposition.Reason != "pending-ownership-unknown" {
		t.Fatalf("prepared crash window was promoted: %+v", row)
	}
}

func TestMCPFrontV3_NoInvocationStateEqualityNeverCreatesReceipt(t *testing.T) {
	name := api.LSPRouterEntryName("go")
	pre := v3LSPSnapshot("claude-code", "go", name, false, "")
	post := v3LSPSnapshot("claude-code", "go", name, true, api.LSPRouterURL(9137, "go"))
	op := v3LSPAdd("claude-code", "go", name, pre, post)
	journal := v3Journal(t, 9137, nil, op)
	if err := journal.finishLSPOperation(api.LSPRouterMutationObservation{
		Operation: op, ObservedState: post, Invoked: false, PreconditionConflict: true,
	}); err != nil {
		t.Fatal(err)
	}
	row := journal.record.Rows[mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "claude-code", "go", name)]
	if row.Applied != nil || row.Attempt == nil ||
		row.Attempt.State != mcpFrontAttemptPreconditionConflict ||
		row.Disposition == nil || row.Disposition.State != mcpFrontDispositionConflict {
		t.Fatalf("no-invocation equality created authority: %+v", row)
	}
}

func TestMCPFrontV3_FirstGenerationSerenaConflictRoundTripRetryRollback(t *testing.T) {
	journal := v3Journal(t, 9137, nil)
	result := api.SerenaReconcileAttemptResult{
		Client: "claude-code", PreFingerprint: "", IntendedFingerprint: "front", ObservedFingerprint: "operator",
		PreconditionConflict: true,
	}
	if err := journal.finishSerenaAttempt(result); err != nil {
		t.Fatalf("finish first-generation no-write Serena conflict: %v", err)
	}
	key := v3SerenaRowKeyFor("claude-code")
	row, ok := journal.record.Rows[key]
	if !ok || row.Pin != nil || row.Applied != nil || row.Attempt == nil ||
		row.Attempt.State != mcpFrontAttemptPreconditionConflict || row.Disposition == nil ||
		row.Disposition.State != mcpFrontDispositionConflict || row.Disposition.Reason != "forward-plan-precondition-conflict" {
		t.Fatalf("first-generation conflict did not persist its exact authority-free shape: %+v", row)
	}
	if got := classifyMCPFrontSerenaConflict(&journal.record, key, row); got != mcpFrontSerenaConflictFreshAuthorityFreePinless {
		t.Fatalf("row pin authority classification is wrong: %+v", row)
	}
	persisted, err := readMCPFrontReconcileReport(journal.reportPath)
	if err != nil {
		t.Fatalf("reader rejected durable authority-free conflict: %v", err)
	}
	loaded, err := loadMCPFrontVerifiedSerenaPins(context.Background(), persisted, journal.reportPath)
	if err != nil {
		t.Fatalf("pinless authority-free conflict must not request a loader input: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("authority-free conflict loaded pins: %+v", loaded)
	}
	rollbackErr := runRollbackMCPFront(newMCPFrontTestCmd(), api.NewAPI(), journal.reportPath)
	if rollbackErr == nil || !strings.Contains(rollbackErr.Error(), "rollback completed") {
		t.Fatalf("first-generation conflict rollback=%v, want consumed terminal conflict result", rollbackErr)
	}
	if _, statErr := os.Stat(journal.reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("first-generation authority-free conflict was not retired: %v", statErr)
	}
}

// TestMCPFrontV3_ConflictForwardRetriesAreByteStable pins the durable side of
// both no-write conflict shapes. The forward command is invoked after a fresh
// process-style re-read, so a green result proves the command refuses before it
// can replace the active generation or touch either client-config surface.
func TestMCPFrontV3_ConflictForwardRetriesAreByteStable(t *testing.T) {
	tests := []struct {
		name                 string
		build                func(*testing.T, string, int) *mcpFrontReconcileJournal
		wantRollbackConflict bool
	}{
		{
			name: "first-generation",
			build: func(t *testing.T, reportPath string, port int) *mcpFrontReconcileJournal {
				journal := v3JournalAt(t, reportPath, port, nil)
				if err := journal.finishSerenaAttempt(api.SerenaReconcileAttemptResult{
					Client: "claude-code", PreFingerprint: "", IntendedFingerprint: "front", ObservedFingerprint: "operator",
					PreconditionConflict: true,
				}); err != nil {
					t.Fatalf("finish first-generation conflict: %v", err)
				}
				return journal
			},
			wantRollbackConflict: true,
		},
		{
			name: "retained-pinned-no-receipt",
			build: func(t *testing.T, reportPath string, port int) *mcpFrontReconcileJournal {
				backupPath := filepath.Join(t.TempDir(), "backup.json")
				if err := os.WriteFile(backupPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				first := v3JournalAt(t, reportPath, port, nil)
				if err := first.prepareSerenaAttempt(api.SerenaReconcileAttemptResult{
					Client: "claude-code", BackupPath: backupPath, PreFingerprint: "", IntendedFingerprint: "front-a",
				}); err != nil {
					t.Fatalf("prepare retained pin: %v", err)
				}
				retry, err := newMCPFrontV3Journal(reportPath, &first.record, port+1, &api.LSPRouterClientPlan{Port: port + 1})
				if err != nil {
					t.Fatalf("start retained conflict generation: %v", err)
				}
				if err := retry.finishSerenaAttempt(api.SerenaReconcileAttemptResult{
					Client: "claude-code", PreFingerprint: "front-a", IntendedFingerprint: "front-b", ObservedFingerprint: "operator",
					PreconditionConflict: true,
				}); err != nil {
					t.Fatalf("finish retained conflict: %v", err)
				}
				return retry
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reportPath, port, configPath, configBefore := setupMCPFrontForwardRefusalTest(t)
			journal := test.build(t, reportPath, port)
			reportBefore, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatalf("read durable conflict report: %v", err)
			}
			// The fresh reader is the restart oracle: do not rely on the in-memory
			// journal the test used to create the artifact.
			if _, err := readMCPFrontReconcileReport(reportPath); err != nil {
				t.Fatalf("restart reader rejected a valid conflict artifact: %v", err)
			}
			forwardErr := runReconcileMCPFront(newMCPFrontTestCmd(), false)
			if forwardErr == nil || !strings.Contains(forwardErr.Error(), "forward-recovery-disposition-active") {
				t.Fatalf("forward retry=%v, want durable conflict refusal", forwardErr)
			}
			assertMCPFrontBytesEqual(t, reportPath, reportBefore, "recovery report")
			assertMCPFrontBytesEqual(t, configPath, configBefore, "Serena/LSP config")
			restarted, err := readMCPFrontReconcileReport(reportPath)
			if err != nil {
				t.Fatalf("refused retry changed artifact validity: %v", err)
			}
			if restarted.Generation != journal.record.Generation || restarted.ActivePlan == nil ||
				restarted.ActivePlan.Generation != journal.record.ActivePlan.Generation {
				t.Fatalf("refused retry changed generation or plan: got=%+v want=%+v", restarted, journal.record)
			}

			rollbackErr := runReconcileMCPFront(newMCPFrontTestCmd(), true)
			if test.wantRollbackConflict {
				if rollbackErr == nil || !strings.Contains(rollbackErr.Error(), "rollback completed") {
					t.Fatalf("first-generation rollback=%v, want terminal no-write conflict outcome", rollbackErr)
				}
			} else if rollbackErr != nil {
				t.Fatalf("retained-pin no-receipt rollback: %v", rollbackErr)
			}
			assertMCPFrontBytesEqual(t, configPath, configBefore, "no-write rollback config")
			if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
				t.Fatalf("only rollback may retire the conflict artifact: %v", statErr)
			}
		})
	}
}

func TestMCPFrontV3_SerenaPreconditionConflictPreservesEarlierAppliedReceipt(t *testing.T) {
	backupPath := filepath.Join(t.TempDir(), "backup.json")
	if err := os.WriteFile(backupPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first := v3Journal(t, 9137, nil)
	if err := first.prepareSerenaAttempt(api.SerenaReconcileAttemptResult{
		Client: "claude-code", BackupPath: backupPath, PreFingerprint: "", IntendedFingerprint: "front-a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.finishSerenaAttempt(api.SerenaReconcileAttemptResult{
		Client: "claude-code", PreFingerprint: "", IntendedFingerprint: "front-a", ObservedFingerprint: "front-a", Invoked: true,
	}); err != nil {
		t.Fatal(err)
	}
	retry, err := newMCPFrontV3Journal(first.reportPath, &first.record, 9138, &api.LSPRouterClientPlan{Port: 9138})
	if err != nil {
		t.Fatalf("new generation: %v", err)
	}
	if err := retry.finishSerenaAttempt(api.SerenaReconcileAttemptResult{
		Client: "claude-code", PreFingerprint: "front-a", IntendedFingerprint: "front-b", ObservedFingerprint: "operator",
		PreconditionConflict: true,
	}); err != nil {
		t.Fatalf("finish retry no-write conflict: %v", err)
	}
	key := v3SerenaRowKeyFor("claude-code")
	row := retry.record.Rows[key]
	receipt, uncertain := effectiveMCPFrontAppliedReceipt(row)
	if uncertain || receipt == nil || receipt.Port != 9137 || row.Pin == nil || row.Attempt == nil ||
		row.Attempt.State != mcpFrontAttemptPreconditionConflict || row.Disposition == nil ||
		row.Disposition.State != mcpFrontDispositionPending || row.Disposition.Reason != "forward-precondition-conflict-prior-owned" {
		t.Fatalf("retry conflict lost earlier authority: row=%+v receipt=%+v uncertain=%v", row, receipt, uncertain)
	}
	persisted, err := readMCPFrontReconcileReport(retry.reportPath)
	if err != nil {
		t.Fatalf("reader rejected retained prior ownership: %v", err)
	}
	if persisted.Rows[key].Applied == nil || persisted.Rows[key].Applied.Port != 9137 {
		t.Fatalf("persisted retry lost prior receipt: %+v", persisted.Rows[key])
	}
}

func TestMCPFrontV3_ForwardPostInvocationDependencyChangePersistsUnknown(t *testing.T) {
	const clientName = "codex-cli"
	const language = "go"
	const legacyName = "mcp-language-server-go-legacy"
	canonicalName := api.LSPRouterEntryName(language)
	canonical := v3LSPSnapshot(clientName, language, canonicalName, true, api.LSPRouterURL(9137, language))
	legacyBefore := v3LSPSnapshot(clientName, language, legacyName, true, "http://127.0.0.1:9200/mcp")
	canonicalOp := v3LSPAdd(clientName, language, canonicalName, canonical, canonical)
	legacyOp := v3LSPRemove(clientName, language, legacyName, legacyBefore)
	journal := v3Journal(t, 9137, nil, canonicalOp, legacyOp)
	if err := journal.prepareLSPOperation(legacyOp); err != nil {
		t.Fatal(err)
	}
	err := journal.finishLSPOperation(api.LSPRouterMutationObservation{
		Operation: legacyOp, ObservedState: legacyOp.IntendedState, Invoked: true,
		DependencyFailure: &clients.EntryMutationDependencyFailure{
			Phase: clients.EntryMutationDependencyFailureAfterPredicateMismatch,
			Kind:  clients.EntryMutationDependencyFailurePredicateMismatch, EntryName: canonicalName,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "forward-ownership-unknown") {
		t.Fatalf("post-invocation dependency change err=%v, want durable ownership-unknown", err)
	}
	key := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, clientName, language, legacyName)
	row := journal.record.Rows[key]
	if row.Applied != nil || row.Disposition == nil || row.Disposition.State != mcpFrontDispositionPending ||
		row.Disposition.Reason != "forward-ownership-unknown" || row.Disposition.DependencyEntryName != canonicalName {
		t.Fatalf("row=%+v, want dependency-identified pending ownership", row)
	}
	persisted, readErr := readMCPFrontReconcileReport(journal.reportPath)
	if readErr != nil {
		t.Fatalf("durable post-invocation dependency marker is invalid: %v", readErr)
	}
	if got := persisted.Rows[key].Disposition.DependencyEntryName; got != canonicalName {
		t.Fatalf("durable dependency identity=%q, want %q", got, canonicalName)
	}
}

func TestMCPFrontV3_ForwardPostInvocationDependencyRetryKeepsDurablePending(t *testing.T) {
	reportPath, port, configPath, configBefore := setupMCPFrontForwardRefusalTest(t)
	const clientName, language, legacyName = "codex-cli", "go", "mcp-language-server-go-legacy"
	canonicalName := api.LSPRouterEntryName(language)
	canonical := v3LSPSnapshot(clientName, language, canonicalName, true, api.LSPRouterURL(port, language))
	legacyBefore := v3LSPSnapshot(clientName, language, legacyName, true, "http://127.0.0.1:9200/mcp")
	legacyOp := v3LSPRemove(clientName, language, legacyName, legacyBefore)
	journal := v3JournalAt(t, reportPath, port, nil,
		v3LSPAdd(clientName, language, canonicalName, canonical, canonical), legacyOp)
	if err := journal.prepareLSPOperation(legacyOp); err != nil {
		t.Fatal(err)
	}
	if err := journal.finishLSPOperation(api.LSPRouterMutationObservation{
		Operation: legacyOp, ObservedState: legacyOp.IntendedState, Invoked: true,
		DependencyFailure: &clients.EntryMutationDependencyFailure{
			Phase: clients.EntryMutationDependencyFailureAfterPredicateMismatch,
			Kind:  clients.EntryMutationDependencyFailurePredicateMismatch, EntryName: canonicalName,
		},
	}); err == nil {
		t.Fatal("post-invocation dependency change must become durable pending")
	}
	before, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	forwardErr := runReconcileMCPFront(newMCPFrontTestCmd(), false)
	if forwardErr == nil || !strings.Contains(forwardErr.Error(), "forward-recovery-disposition-active") {
		t.Fatalf("forward retry=%v, want pending-recovery refusal", forwardErr)
	}
	assertMCPFrontBytesEqual(t, reportPath, before, "pending recovery report")
	assertMCPFrontBytesEqual(t, configPath, configBefore, "pending retry config")
	persisted, err := readMCPFrontReconcileReport(reportPath)
	if err != nil {
		t.Fatalf("pending artifact is not restart-readable: %v", err)
	}
	key := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, clientName, language, legacyName)
	row := persisted.Rows[key]
	if row.Disposition == nil || row.Disposition.State != mcpFrontDispositionPending ||
		row.Disposition.Reason != "forward-ownership-unknown" || row.Disposition.DependencyEntryName != canonicalName {
		t.Fatalf("retry changed durable pending identity: %+v", row)
	}
	if _, statErr := os.Stat(reportPath); statErr != nil {
		t.Fatalf("forward retry retired a pending recovery artifact: %v", statErr)
	}
}

func TestMCPFrontV3_SerenaConflictClassifierMutationTable(t *testing.T) {
	journal := v3Journal(t, 9137, nil)
	if err := journal.finishSerenaAttempt(api.SerenaReconcileAttemptResult{
		Client: "claude-code", PreFingerprint: "", IntendedFingerprint: "front", ObservedFingerprint: "operator", PreconditionConflict: true,
	}); err != nil {
		t.Fatal(err)
	}
	key := v3SerenaRowKeyFor("claude-code")
	base := journal.record.Rows[key]
	clone := func() (*mcpFrontReconcileReport, mcpFrontReconcileRow) {
		copyReport := journal.record
		copyReport.Rows = map[string]mcpFrontReconcileRow{}
		for rowKey, row := range journal.record.Rows {
			copyReport.Rows[rowKey] = row
		}
		plan := *journal.record.ActivePlan
		plan.Rows = append([]string(nil), journal.record.ActivePlan.Rows...)
		plan.Operations = append([]mcpFrontReconcilePlanOp(nil), journal.record.ActivePlan.Operations...)
		copyReport.ActivePlan = &plan
		copyRow := base
		if base.Attempt != nil {
			attempt := *base.Attempt
			copyRow.Attempt = &attempt
		}
		if base.Disposition != nil {
			disposition := *base.Disposition
			copyRow.Disposition = &disposition
		}
		if base.Pin != nil {
			pin := *base.Pin
			copyRow.Pin = &pin
		}
		if base.Applied != nil {
			applied := *base.Applied
			copyRow.Applied = &applied
		}
		copyReport.Rows[key] = copyRow
		return &copyReport, copyRow
	}
	retainedPinNoReceipt := func(report *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
		row.Pin = &mcpFrontSerenaPin{
			Client: row.Client, Path: "pin.json", Origin: "rolling", SHA256: strings.Repeat("0", 64),
		}
		report.Generation = 2
		report.ActivePlan.Generation = report.Generation
		row.Attempt.Generation = report.Generation
		row.Attempt.PreState = mcpFrontSerenaState("front-port-fingerprint-generation-one")
		row.Attempt.IntendedState = mcpFrontSerenaState("front-port-fingerprint-generation-two")
		report.ActivePlan.Operations[0] = mcpFrontPlanOperationForRow(
			key, *row, row.Attempt.Operation, row.Attempt.PreState, row.Attempt.IntendedState,
		)
	}
	retainedPriorApplied := func(report *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
		retainedPinNoReceipt(report, row)
		report.Generation = 2
		report.ActivePlan.Generation = 2
		row.Attempt.Generation = 2
		row.Applied = &mcpFrontAppliedReceipt{
			Generation: 1, Port: 9137, PostState: row.Attempt.PreState,
		}
		row.Disposition.State = mcpFrontDispositionPending
		row.Disposition.Reason = "forward-precondition-conflict-prior-owned"
	}
	tests := []struct {
		name    string
		prepare func(*mcpFrontReconcileReport, *mcpFrontReconcileRow)
		mutate  func(*mcpFrontReconcileReport, *mcpFrontReconcileRow)
		want    mcpFrontSerenaConflictClass
	}{
		{name: "fresh-authority-free-pinless", want: mcpFrontSerenaConflictFreshAuthorityFreePinless},
		{name: "retained-pinned-no-receipt", want: mcpFrontSerenaConflictRetainedPinnedNoReceipt,
			prepare: retainedPinNoReceipt},
		{name: "retained-prior-applied", want: mcpFrontSerenaConflictRetainedPriorApplied,
			prepare: retainedPriorApplied},
		{name: "applied-without-pin-is-invalid", want: mcpFrontSerenaConflictInvalid,
			prepare: retainedPriorApplied,
			mutate:  func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Pin = nil }},
		{name: "retained-no-receipt-without-pin-is-invalid", want: mcpFrontSerenaConflictInvalid,
			prepare: retainedPinNoReceipt,
			mutate:  func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Pin = nil }},
		{name: "prior-applied-receipt-removed-is-invalid", want: mcpFrontSerenaConflictInvalid,
			prepare: retainedPriorApplied,
			mutate:  func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Applied = nil }},
		{name: "prior-applied-current-generation-is-invalid", want: mcpFrontSerenaConflictInvalid,
			prepare: retainedPriorApplied,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
				row.Applied.Generation = row.Attempt.Generation
			}},
		{name: "prior-applied-post-state-is-invalid", want: mcpFrontSerenaConflictInvalid,
			prepare: retainedPriorApplied,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
				row.Applied.PostState = mcpFrontSerenaState("different")
			}},
		{name: "nil-disposition-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Disposition = nil }},
		{name: "empty-disposition-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
				row.Disposition = &mcpFrontRollbackDisposition{}
			}},
		{name: "baseline-only-disposition-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
				row.Disposition.State = mcpFrontDispositionBaselineOnly
			}},
		{name: "restored-disposition-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
				row.Disposition.State = mcpFrontDispositionRestored
			}},
		{name: "failed-disposition-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
				row.Disposition.State = mcpFrontDispositionFailed
			}},
		{name: "unknown-disposition-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Disposition.State = "unknown" }},
		{name: "wrong-conflict-reason-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Disposition.Reason = "wrong" }},
		{name: "wrong-pending-reason-is-invalid", want: mcpFrontSerenaConflictInvalid,
			prepare: retainedPriorApplied,
			mutate:  func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Disposition.Reason = "wrong" }},
		{name: "dependency-identity-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
				row.Disposition.DependencyEntryName = "not-for-serena"
			}},
		{name: "surface-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Surface = mcpFrontSurfaceLSP }},
		{name: "client-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Client = "" }},
		{name: "language-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Language = "go" }},
		{name: "row-identity-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.EntryName = "not-serena" }},
		{name: "baseline-marker-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.BaselineSet = false }},
		{name: "fresh-baseline-state-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
				row.Baseline = mcpFrontSerenaState("different")
			}},
		{name: "attempt-generation-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Attempt.Generation++ }},
		{name: "attempt-operation-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) { row.Attempt.Operation = "remove" }},
		{name: "attempt-pre-state-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
				row.Attempt.PreState = mcpFrontSerenaState("different")
			}},
		{name: "attempt-intended-state-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
				row.Attempt.IntendedState = mcpFrontSerenaState("different")
			}},
		{name: "active-plan-generation-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(report *mcpFrontReconcileReport, _ *mcpFrontReconcileRow) { report.ActivePlan.Generation++ }},
		{name: "report-generation-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(report *mcpFrontReconcileReport, _ *mcpFrontReconcileRow) { report.Generation++ }},
		{name: "active-plan-row-key-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(report *mcpFrontReconcileReport, _ *mcpFrontReconcileRow) {
				report.ActivePlan.Operations[0].RowKey = "other"
			}},
		{name: "active-plan-operation-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(report *mcpFrontReconcileReport, _ *mcpFrontReconcileRow) {
				report.ActivePlan.Operations[0].Operation = "remove"
			}},
		{name: "active-plan-pre-state-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(report *mcpFrontReconcileReport, _ *mcpFrontReconcileRow) {
				report.ActivePlan.Operations[0].PreState = mcpFrontSerenaState("different")
			}},
		{name: "active-plan-intended-state-is-invalid", want: mcpFrontSerenaConflictInvalid,
			mutate: func(report *mcpFrontReconcileReport, _ *mcpFrontReconcileRow) {
				report.ActivePlan.Operations[0].IntendedState = mcpFrontSerenaState("different")
			}},
		{name: "not-precondition-conflict", want: mcpFrontSerenaConflictNotPreconditionConflict,
			mutate: func(_ *mcpFrontReconcileReport, row *mcpFrontReconcileRow) {
				row.Attempt.State = mcpFrontAttemptApplied
			}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, row := clone()
			if test.prepare != nil {
				test.prepare(report, &row)
			}
			if test.mutate != nil {
				test.mutate(report, &row)
			}
			report.Rows[key] = row
			if got := classifyMCPFrontSerenaConflict(report, key, row); got != test.want {
				t.Fatalf("class=%q, want %q; row=%+v", got, test.want, row)
			}
			if test.want != mcpFrontSerenaConflictInvalid {
				return
			}
			reportPath := filepath.Join(t.TempDir(), "recovery.json")
			if err := api.WriteStateFileAtomic(reportPath, report); err != nil {
				t.Fatal(err)
			}
			if _, err := readMCPFrontReconcileReport(reportPath); err == nil {
				t.Fatal("invalid Serena conflict shape passed durable validation")
			}
			readerCalls := 0
			err := runRollbackMCPFrontWithReader(newMCPFrontTestCmd(), api.NewAPI(), reportPath, func(context.Context, string, []string, string) ([]byte, error) {
				readerCalls++
				return nil, errors.New("invalid conflict shape reached pin reader")
			})
			if err == nil {
				t.Fatal("invalid Serena conflict shape reached rollback")
			}
			if readerCalls != 0 {
				t.Fatalf("invalid Serena conflict shape reached pin reader %d time(s)", readerCalls)
			}
		})
	}
}

// TestMCPFrontV3_ClassifierRejectsPinMutationAcrossAcceptedShapes exhausts the
// v3 Pin authority matrix. It keeps every tested Pin value syntactically valid
// unless malformed metadata itself is the subject, so a green result proves
// exact row/plan identity rather than merely rejecting empty fields.
func TestMCPFrontV3_ClassifierRejectsPinMutationAcrossAcceptedShapes(t *testing.T) {
	type fixture struct {
		record mcpFrontReconcileReport
		key    string
		want   mcpFrontSerenaConflictClass
	}
	type mutation struct {
		name  string
		apply func(*mcpFrontReconcileRow, *mcpFrontReconcilePlanOp, mcpFrontSerenaPin)
	}
	cloneRecord := func(t *testing.T, record mcpFrontReconcileReport) mcpFrontReconcileReport {
		t.Helper()
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		var copy mcpFrontReconcileReport
		if err := json.Unmarshal(raw, &copy); err != nil {
			t.Fatal(err)
		}
		return copy
	}
	planOperation := func(t *testing.T, record *mcpFrontReconcileReport, key string) *mcpFrontReconcilePlanOp {
		t.Helper()
		for i := range record.ActivePlan.Operations {
			if record.ActivePlan.Operations[i].RowKey == key {
				return &record.ActivePlan.Operations[i]
			}
		}
		t.Fatalf("active plan has no operation for %q", key)
		return nil
	}
	validInjectedPin := func(t *testing.T, tmp, client string) mcpFrontSerenaPin {
		t.Helper()
		path := filepath.Join(tmp, "injected-pin.json")
		if err := os.WriteFile(path, []byte(`{"mcpServers":{"serena":{"url":"http://127.0.0.1:9125/serena/mcp"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		digest, err := fileSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		return mcpFrontSerenaPin{Client: client, Origin: "rolling", Path: path, SHA256: digest}
	}
	validFieldMutation := func(pin *mcpFrontSerenaPin, field string) {
		switch field {
		case "client":
			pin.Client = "other-client"
		case "origin":
			pin.Origin = "other-origin"
		case "path":
			pin.Path = filepath.Join(filepath.Dir(pin.Path), "sibling-pin.json")
		case "sha256":
			candidate := strings.Repeat("0", 64)
			if pin.SHA256 == candidate {
				candidate = strings.Repeat("1", 64)
			}
			pin.SHA256 = candidate
		default:
			t.Fatalf("unknown pin field %q", field)
		}
	}
	snapshotPins := func(t *testing.T, record mcpFrontReconcileReport) map[string][]byte {
		t.Helper()
		paths := map[string]bool{}
		for _, row := range record.Rows {
			if row.Pin != nil && row.Pin.Path != "" {
				paths[row.Pin.Path] = true
			}
		}
		for _, op := range record.ActivePlan.Operations {
			if op.Pin != nil && op.Pin.Path != "" {
				paths[op.Pin.Path] = true
			}
		}
		out := map[string][]byte{}
		for path := range paths {
			bytes, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				t.Fatalf("read retained pin %s: %v", path, err)
			}
			out[path] = bytes
		}
		return out
	}
	builds := []struct {
		name  string
		fresh bool
		build func(*testing.T, string) fixture
	}{
		{
			name: "fresh-authority-free-pinless", fresh: true,
			build: func(t *testing.T, reportPath string) fixture {
				journal := v3JournalAt(t, reportPath, 9137, nil)
				if err := journal.finishSerenaAttempt(api.SerenaReconcileAttemptResult{
					Client: "claude-code", PreFingerprint: "", IntendedFingerprint: "front", ObservedFingerprint: "operator",
					PreconditionConflict: true,
				}); err != nil {
					t.Fatal(err)
				}
				return fixture{record: journal.record, key: v3SerenaRowKeyFor("claude-code"), want: mcpFrontSerenaConflictFreshAuthorityFreePinless}
			},
		},
		{
			name: "retained-pinned-no-receipt",
			build: func(t *testing.T, reportPath string) fixture {
				backupPath := filepath.Join(filepath.Dir(reportPath), "backup.json")
				if err := os.WriteFile(backupPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				first, err := newMCPFrontV3Journal(reportPath, nil, 9137, &api.LSPRouterClientPlan{Port: 9137})
				if err != nil {
					t.Fatal(err)
				}
				if err := first.persist(); err != nil {
					t.Fatal(err)
				}
				if err := first.prepareSerenaAttempt(api.SerenaReconcileAttemptResult{
					Client: "claude-code", BackupPath: backupPath, PreFingerprint: "", IntendedFingerprint: "front-a",
				}); err != nil {
					t.Fatal(err)
				}
				retry, err := newMCPFrontV3Journal(reportPath, &first.record, 9138, &api.LSPRouterClientPlan{Port: 9138})
				if err != nil {
					t.Fatal(err)
				}
				if err := retry.finishSerenaAttempt(api.SerenaReconcileAttemptResult{
					Client: "claude-code", PreFingerprint: "front-a", IntendedFingerprint: "front-b", ObservedFingerprint: "operator",
					PreconditionConflict: true,
				}); err != nil {
					t.Fatal(err)
				}
				return fixture{record: retry.record, key: v3SerenaRowKeyFor("claude-code"), want: mcpFrontSerenaConflictRetainedPinnedNoReceipt}
			},
		},
		{
			name: "retained-prior-applied",
			build: func(t *testing.T, reportPath string) fixture {
				record := newV3RollbackRecord(t, reportPath, 9137, "claude-code")
				key := v3SerenaRowKeyFor("claude-code")
				row := record.Rows[key]
				record.Generation = 2
				record.ActivePlan.Generation = record.Generation
				record.ActivePlan.Port = 9138
				row.Attempt = &mcpFrontReconcileAttempt{
					Generation: record.Generation, Operation: "add",
					PreState:      mcpFrontSerenaState("front-generation-one"),
					IntendedState: mcpFrontSerenaState("front-generation-two"),
					State:         mcpFrontAttemptPreconditionConflict,
				}
				row.Applied = &mcpFrontAppliedReceipt{Generation: 1, Port: 9137, PostState: row.Attempt.PreState}
				row.Disposition = &mcpFrontRollbackDisposition{
					State: mcpFrontDispositionPending, Reason: "forward-precondition-conflict-prior-owned",
				}
				record.Rows[key] = row
				record.ActivePlan.Operations[0] = mcpFrontPlanOperationForRow(
					key, row, row.Attempt.Operation, row.Attempt.PreState, row.Attempt.IntendedState,
				)
				return fixture{record: record, key: key, want: mcpFrontSerenaConflictRetainedPriorApplied}
			},
		},
	}
	for _, base := range builds {
		base := base
		mutations := []mutation{
			{name: "pin-bound-cleared", apply: func(_ *mcpFrontReconcileRow, op *mcpFrontReconcilePlanOp, _ mcpFrontSerenaPin) { op.PinBound = false }},
		}
		if base.fresh {
			mutations = append(mutations,
				mutation{name: "valid-row-pin-with-plan-nil", apply: func(row *mcpFrontReconcileRow, _ *mcpFrontReconcilePlanOp, pin mcpFrontSerenaPin) {
					row.Pin = cloneMCPFrontSerenaPin(&pin)
				}},
				mutation{name: "valid-plan-pin-with-row-nil", apply: func(_ *mcpFrontReconcileRow, op *mcpFrontReconcilePlanOp, pin mcpFrontSerenaPin) {
					op.Pin = cloneMCPFrontSerenaPin(&pin)
				}},
			)
			for _, field := range []string{"client", "origin", "path", "sha256"} {
				field := field
				mutations = append(mutations, mutation{name: "malformed-" + field, apply: func(row *mcpFrontReconcileRow, op *mcpFrontReconcilePlanOp, pin mcpFrontSerenaPin) {
					switch field {
					case "client":
						pin.Client = ""
					case "origin":
						pin.Origin = ""
					case "path":
						pin.Path = ""
					case "sha256":
						pin.SHA256 = "not-hex"
					}
					row.Pin = cloneMCPFrontSerenaPin(&pin)
					op.Pin = cloneMCPFrontSerenaPin(&pin)
				}})
			}
		} else {
			for _, field := range []string{"client", "origin", "path", "sha256"} {
				field := field
				mutations = append(mutations,
					mutation{name: "valid-" + field + "-row-side", apply: func(row *mcpFrontReconcileRow, _ *mcpFrontReconcilePlanOp, _ mcpFrontSerenaPin) {
						validFieldMutation(row.Pin, field)
					}},
					mutation{name: "valid-" + field + "-plan-side", apply: func(_ *mcpFrontReconcileRow, op *mcpFrontReconcilePlanOp, _ mcpFrontSerenaPin) {
						validFieldMutation(op.Pin, field)
					}},
				)
			}
			mutations = append(mutations,
				mutation{name: "row-pin-nil", apply: func(row *mcpFrontReconcileRow, _ *mcpFrontReconcilePlanOp, _ mcpFrontSerenaPin) { row.Pin = nil }},
				mutation{name: "plan-pin-nil", apply: func(_ *mcpFrontReconcileRow, op *mcpFrontReconcilePlanOp, _ mcpFrontSerenaPin) { op.Pin = nil }},
				mutation{name: "empty-metadata", apply: func(row *mcpFrontReconcileRow, op *mcpFrontReconcilePlanOp, _ mcpFrontSerenaPin) {
					empty := &mcpFrontSerenaPin{}
					row.Pin = cloneMCPFrontSerenaPin(empty)
					op.Pin = cloneMCPFrontSerenaPin(empty)
				}},
				mutation{name: "uppercase-sha256", apply: func(row *mcpFrontReconcileRow, op *mcpFrontReconcilePlanOp, _ mcpFrontSerenaPin) {
					row.Pin.SHA256 = strings.Repeat("A", 64)
					op.Pin.SHA256 = row.Pin.SHA256
				}},
			)
		}
		for _, mutation := range mutations {
			mutation := mutation
			t.Run(base.name+"/"+mutation.name, func(t *testing.T) {
				tmp := mcpFrontPR588Env(t)
				reportPath := filepath.Join(tmp, "recovery.json")
				fixture := base.build(t, reportPath)
				if err := api.WriteStateFileAtomic(reportPath, fixture.record); err != nil {
					t.Fatal(err)
				}
				baseline, err := readMCPFrontReconcileReport(reportPath)
				if err != nil {
					t.Fatalf("valid %s base refused: %v", base.name, err)
				}
				row := baseline.Rows[fixture.key]
				if got := classifyMCPFrontSerenaConflict(baseline, fixture.key, row); got != fixture.want {
					t.Fatalf("valid %s class=%q, want %q", base.name, got, fixture.want)
				}
				pinsBefore := snapshotPins(t, *baseline)
				record := cloneRecord(t, *baseline)
				row = record.Rows[fixture.key]
				op := planOperation(t, &record, fixture.key)
				pin := validInjectedPin(t, tmp, row.Client)
				mutation.apply(&row, op, pin)
				record.Rows[fixture.key] = row
				if got := classifyMCPFrontSerenaConflict(&record, fixture.key, row); got != mcpFrontSerenaConflictInvalid {
					t.Fatalf("class=%q, want invalid", got)
				}
				if err := api.WriteStateFileAtomic(reportPath, &record); err != nil {
					t.Fatal(err)
				}
				reportBefore, err := os.ReadFile(reportPath)
				if err != nil {
					t.Fatal(err)
				}
				configPath := seedClaudeCodeConfig(t, tmp, map[string]any{
					"serena":                     map[string]any{"url": "http://127.0.0.1:9137/serena/mcp"},
					api.LSPRouterEntryName("go"): map[string]any{"url": "http://127.0.0.1:9137/lsp/go/mcp"},
				})
				configBefore, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				for path, bytes := range snapshotPins(t, record) {
					pinsBefore[path] = bytes
				}
				if _, err := readMCPFrontReconcileReport(reportPath); err == nil || !strings.Contains(err.Error(), "serena-pin-set-invalid") {
					t.Fatalf("strict durable read=%v, want safe pin-binding refusal", err)
				}
				readerCalls, serenaCalls, lspCalls := 0, 0, 0
				ops := mcpFrontRollbackOps{
					readStateFile: func(context.Context, string, []string, string) ([]byte, error) {
						readerCalls++
						return nil, errors.New("invalid pin binding reached reader")
					},
					restoreSerena: func([]api.SerenaOwnedRestoreRequest) ([]api.SerenaOwnedRestoreResult, error) {
						serenaCalls++
						return nil, errors.New("invalid pin binding reached serena restore")
					},
					restoreLSP: func([]api.LSPRouterRecoveryRow, api.LSPClientRouterOpts, api.LSPRouterRestoreCallbacks) (*api.LSPClientRouterReport, []api.LSPRouterRestoreRowResult, error) {
						lspCalls++
						return nil, nil, errors.New("invalid pin binding reached lsp restore")
					},
				}
				for retry := 0; retry < 2; retry++ {
					err := runRollbackMCPFrontWithOps(newMCPFrontTestCmd(), reportPath, ops)
					if err == nil || !strings.Contains(err.Error(), "serena-pin-set-invalid") {
						t.Fatalf("rollback retry %d=%v, want safe pin-binding refusal", retry+1, err)
					}
					assertMCPFrontBytesEqual(t, reportPath, reportBefore, "invalid-pin recovery report")
					assertMCPFrontBytesEqual(t, configPath, configBefore, "invalid-pin Serena/LSP config")
					for path, want := range pinsBefore {
						assertMCPFrontBytesEqual(t, path, want, "invalid-pin retained input")
					}
				}
				if readerCalls != 0 || serenaCalls != 0 || lspCalls != 0 {
					t.Fatalf("invalid pin binding reached reader=%d serena=%d lsp=%d", readerCalls, serenaCalls, lspCalls)
				}
				entries, err := os.ReadDir(filepath.Dir(reportPath))
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if strings.Contains(entry.Name(), "retired") {
						t.Fatalf("invalid pin binding created retired report sibling %q", entry.Name())
					}
				}
			})
		}
	}
}

func TestMCPFrontV3_InterimMissingPinBoundRefusesBytePreserving(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	reportPath := filepath.Join(tmp, "recovery.json")
	record := newV3RollbackRecord(t, reportPath, 9137, "claude-code")
	key := v3SerenaRowKeyFor("claude-code")
	body := v3RecordAsMap(t, record)
	activePlan, ok := body["active_plan"].(map[string]any)
	if !ok {
		t.Fatal("fixture active plan is not an object")
	}
	operations, ok := activePlan["operations"].([]any)
	if !ok || len(operations) != 1 {
		t.Fatalf("fixture operations=%#v, want one operation", activePlan["operations"])
	}
	op, ok := operations[0].(map[string]any)
	if !ok {
		t.Fatal("fixture operation is not an object")
	}
	delete(op, "pin_bound")
	if err := api.WriteStateFileAtomic(reportPath, body); err != nil {
		t.Fatal(err)
	}
	reportBefore, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	configPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena":                     map[string]any{"url": "http://127.0.0.1:9137/serena/mcp"},
		api.LSPRouterEntryName("go"): map[string]any{"url": "http://127.0.0.1:9137/lsp/go/mcp"},
	})
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	pinBefore, err := os.ReadFile(record.Rows[key].Pin.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readMCPFrontReconcileReport(reportPath); err == nil || !strings.Contains(err.Error(), "unbound pin binding") {
		t.Fatalf("interim missing binding read=%v, want unbound refusal", err)
	}
	readerCalls, serenaCalls, lspCalls := 0, 0, 0
	ops := mcpFrontRollbackOps{
		readStateFile: func(context.Context, string, []string, string) ([]byte, error) {
			readerCalls++
			return nil, errors.New("interim binding reached reader")
		},
		restoreSerena: func([]api.SerenaOwnedRestoreRequest) ([]api.SerenaOwnedRestoreResult, error) {
			serenaCalls++
			return nil, errors.New("interim binding reached serena restore")
		},
		restoreLSP: func([]api.LSPRouterRecoveryRow, api.LSPClientRouterOpts, api.LSPRouterRestoreCallbacks) (*api.LSPClientRouterReport, []api.LSPRouterRestoreRowResult, error) {
			lspCalls++
			return nil, nil, errors.New("interim binding reached lsp restore")
		},
	}
	for retry := 0; retry < 2; retry++ {
		err := runRollbackMCPFrontWithOps(newMCPFrontTestCmd(), reportPath, ops)
		if err == nil || !strings.Contains(err.Error(), "unbound pin binding") {
			t.Fatalf("interim rollback retry %d=%v, want unbound refusal", retry+1, err)
		}
		assertMCPFrontBytesEqual(t, reportPath, reportBefore, "interim version-3 report")
		assertMCPFrontBytesEqual(t, configPath, configBefore, "interim Serena/LSP config")
		assertMCPFrontBytesEqual(t, record.Rows[key].Pin.Path, pinBefore, "interim retained pin")
	}
	if readerCalls != 0 || serenaCalls != 0 || lspCalls != 0 {
		t.Fatalf("interim missing binding reached reader=%d serena=%d lsp=%d", readerCalls, serenaCalls, lspCalls)
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("interim version-3 artifact was changed or retired: %v", err)
	}
}

func TestMCPFrontV3_PlanPinBindingRoundTripDeepCopyAndPlanChange(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	reportPath := filepath.Join(tmp, "recovery.json")
	backupPath := filepath.Join(tmp, "backup.json")
	if err := os.WriteFile(backupPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first := v3JournalAt(t, reportPath, 9137, nil)
	if err := first.prepareSerenaAttempt(api.SerenaReconcileAttemptResult{
		Client: "claude-code", BackupPath: backupPath, PreFingerprint: "", IntendedFingerprint: "front-a",
	}); err != nil {
		t.Fatal(err)
	}
	key := v3SerenaRowKeyFor("claude-code")
	firstRow := first.record.Rows[key]
	firstOp, found := activeMCPFrontPlanOperation(first.record.ActivePlan, key)
	if !found || !firstOp.PinBound || firstOp.Pin == nil || firstOp.Pin == firstRow.Pin || !mcpFrontSerenaPinEqual(firstOp.Pin, firstRow.Pin) {
		t.Fatalf("first plan did not deep-copy its exact pin: row=%+v op=%+v", firstRow, firstOp)
	}
	next, err := newMCPFrontV3Journal(reportPath, &first.record, 9138, &api.LSPRouterClientPlan{Port: 9138})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.finishSerenaAttempt(api.SerenaReconcileAttemptResult{
		Client: "claude-code", PreFingerprint: "front-a", IntendedFingerprint: "front-b", ObservedFingerprint: "operator",
		PreconditionConflict: true,
	}); err != nil {
		t.Fatal(err)
	}
	row := next.record.Rows[key]
	op, found := activeMCPFrontPlanOperation(next.record.ActivePlan, key)
	if !found || next.record.Generation != 2 || next.record.ActivePlan.Generation != 2 || !op.PinBound || op.Pin == nil || op.Pin == row.Pin || !mcpFrontSerenaPinEqual(op.Pin, row.Pin) {
		t.Fatalf("new generation did not rebind the exact pin: row=%+v op=%+v record=%+v", row, op, next.record)
	}
	durable, err := readMCPFrontReconcileReport(reportPath)
	if err != nil {
		t.Fatalf("plan pin round trip: %v", err)
	}
	durableRow := durable.Rows[key]
	durableOp, found := activeMCPFrontPlanOperation(durable.ActivePlan, key)
	if !found || !durableOp.PinBound || durableOp.Pin == nil || durableOp.Pin == durableRow.Pin || !mcpFrontSerenaPinEqual(durableOp.Pin, durableRow.Pin) {
		t.Fatalf("durable plan pin lost deep-copy identity: row=%+v op=%+v", durableRow, durableOp)
	}
	planOrigin := op.Pin.Origin
	row.Pin.Origin = "operator-mutated-origin"
	if op.Pin.Origin != planOrigin {
		t.Fatal("row pin mutation aliased the active-plan pin")
	}
	next.record.Rows[key] = row
	if got := classifyMCPFrontSerenaConflict(&next.record, key, row); got != mcpFrontSerenaConflictInvalid {
		t.Fatalf("one-sided in-memory pin mutation class=%q, want invalid", got)
	}
}

func TestMCPFrontV3_ExactPinBindingKeepsLoaderChecksumOracle(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	reportPath := filepath.Join(tmp, "recovery.json")
	record := newV3RollbackRecord(t, reportPath, 9137, "claude-code")
	if err := api.WriteStateFileAtomic(reportPath, record); err != nil {
		t.Fatal(err)
	}
	persisted, err := readMCPFrontReconcileReport(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	key := v3SerenaRowKeyFor("claude-code")
	if err := os.WriteFile(persisted.Rows[key].Pin.Path, []byte(`{"mcpServers":{"serena":"tampered"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMCPFrontVerifiedSerenaPins(context.Background(), persisted, reportPath); err == nil || !strings.Contains(err.Error(), "serena-pin-checksum-mismatch") {
		t.Fatalf("changed retained pin load=%v, want checksum refusal", err)
	}
}

func TestMCPFrontV3_RetainedPinnedNoReceiptConflictRoundTrip(t *testing.T) {
	backupPath := filepath.Join(t.TempDir(), "backup.json")
	if err := os.WriteFile(backupPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first := v3Journal(t, 9137, nil)
	if err := first.prepareSerenaAttempt(api.SerenaReconcileAttemptResult{
		Client: "claude-code", BackupPath: backupPath, PreFingerprint: "", IntendedFingerprint: "front-a",
	}); err != nil {
		t.Fatal(err)
	}
	retry, err := newMCPFrontV3Journal(first.reportPath, &first.record, 9138, &api.LSPRouterClientPlan{Port: 9138})
	if err != nil {
		t.Fatalf("new generation: %v", err)
	}
	if err := retry.finishSerenaAttempt(api.SerenaReconcileAttemptResult{
		Client: "claude-code", PreFingerprint: "front-a", IntendedFingerprint: "front-b", ObservedFingerprint: "operator",
		PreconditionConflict: true,
	}); err != nil {
		t.Fatalf("finish retained no-receipt conflict: %v", err)
	}
	key := v3SerenaRowKeyFor("claude-code")
	row := retry.record.Rows[key]
	if got := classifyMCPFrontSerenaConflict(&retry.record, key, row); got != mcpFrontSerenaConflictRetainedPinnedNoReceipt {
		t.Fatalf("class=%q, want retained pinned/no-receipt; row=%+v", got, row)
	}
	persisted, err := readMCPFrontReconcileReport(retry.reportPath)
	if err != nil {
		t.Fatalf("reader rejected retained pinned/no-receipt conflict: %v", err)
	}
	loaded, err := loadMCPFrontVerifiedSerenaPins(context.Background(), persisted, retry.reportPath)
	if err != nil || len(loaded) != 1 {
		zeroMCPFrontVerifiedSerenaPins(loaded)
		t.Fatalf("retained pin must be independently loaded before rollback: pins=%d err=%v", len(loaded), err)
	}
	zeroMCPFrontVerifiedSerenaPins(loaded)
	if err := runRollbackMCPFront(newMCPFrontTestCmd(), api.NewAPI(), retry.reportPath); err != nil {
		t.Fatalf("explicit rollback must safely consume a retained pin with no receipt: %v", err)
	}
	if _, err := os.Stat(retry.reportPath); !os.IsNotExist(err) {
		t.Fatalf("consumed report remains active: %v", err)
	}
}

func TestMCPFrontV3_PersistTerminalDispositionConsumesPreconditionConflictAttempt(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  string
		reason string
	}{
		{name: "baseline-only", state: mcpFrontDispositionBaselineOnly, reason: "no-effective-applied-receipt"},
		{name: "restored", state: mcpFrontDispositionRestored, reason: "inverse-verified"},
		{name: "rollback-conflict", state: mcpFrontDispositionConflict, reason: "rollback-cas-conflict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reportPath := filepath.Join(t.TempDir(), "recovery.json")
			record := newV3RollbackRecord(t, reportPath, 9137, "claude-code")
			key := v3SerenaRowKeyFor("claude-code")
			row := record.Rows[key]
			record.Generation = 2
			record.ActivePlan.Generation = record.Generation
			record.ActivePlan.Port = 9138
			row.Attempt = &mcpFrontReconcileAttempt{
				Generation: record.Generation, Operation: "add",
				PreState: row.Baseline, IntendedState: mcpFrontSerenaState("front-port-fingerprint-generation-two"),
				State: mcpFrontAttemptPreconditionConflict,
			}
			row.Applied = &mcpFrontAppliedReceipt{
				Generation: 1, Port: 9137, PostState: row.Baseline,
			}
			row.Disposition = &mcpFrontRollbackDisposition{
				State: mcpFrontDispositionPending, Reason: "forward-precondition-conflict-prior-owned",
			}
			record.Rows[key] = row
			record.ActivePlan.Operations[0] = mcpFrontPlanOperationForRow(
				key, row, row.Attempt.Operation, row.Attempt.PreState, row.Attempt.IntendedState,
			)
			if err := api.WriteStateFileAtomic(reportPath, &record); err != nil {
				t.Fatal(err)
			}
			if err := persistMCPFrontDisposition(reportPath, &record, key, test.state, test.reason, ""); err != nil {
				t.Fatalf("persist terminal disposition: %v", err)
			}
			durable, err := readMCPFrontReconcileReport(reportPath)
			if err != nil {
				t.Fatalf("terminal disposition must stay readable under the strict classifier: %v", err)
			}
			got := durable.Rows[key]
			if got.Attempt != nil || got.Disposition == nil || got.Disposition.State != test.state || got.Disposition.Reason != test.reason {
				t.Fatalf("durable row=%+v, want terminal disposition with consumed conflict attempt", got)
			}
			op, found := activeMCPFrontPlanOperation(durable.ActivePlan, key)
			if !found || !op.PinBound || op.Pin == nil || !mcpFrontSerenaPinEqual(op.Pin, got.Pin) {
				t.Fatalf("terminal durable read lost active-plan pin anchor: row=%+v op=%+v", got, op)
			}
		})
	}
}

func TestMCPFrontV3_MalformedPreconditionConflictDispositionRefusesBeforePinReadOrWrite(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*mcpFrontReconcileRow)
	}{
		{name: "nil", mutate: func(row *mcpFrontReconcileRow) { row.Disposition = nil }},
		{name: "empty", mutate: func(row *mcpFrontReconcileRow) { row.Disposition = &mcpFrontRollbackDisposition{} }},
		{name: "baseline-only", mutate: func(row *mcpFrontReconcileRow) { row.Disposition.State = mcpFrontDispositionBaselineOnly }},
		{name: "restored", mutate: func(row *mcpFrontReconcileRow) { row.Disposition.State = mcpFrontDispositionRestored }},
		{name: "failed", mutate: func(row *mcpFrontReconcileRow) { row.Disposition.State = mcpFrontDispositionFailed }},
		{name: "unknown", mutate: func(row *mcpFrontReconcileRow) { row.Disposition.State = "unknown" }},
		{name: "wrong-conflict-reason", mutate: func(row *mcpFrontReconcileRow) { row.Disposition.Reason = "wrong" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := v3Journal(t, 9137, nil)
			if err := journal.finishSerenaAttempt(api.SerenaReconcileAttemptResult{
				Client: "claude-code", PreFingerprint: "", IntendedFingerprint: "front", ObservedFingerprint: "operator",
				PreconditionConflict: true,
			}); err != nil {
				t.Fatal(err)
			}
			key := v3SerenaRowKeyFor("claude-code")
			row := journal.record.Rows[key]
			test.mutate(&row)
			journal.record.Rows[key] = row
			if err := journal.persist(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(journal.reportPath)
			if err != nil {
				t.Fatal(err)
			}
			readerCalls := 0
			err = runRollbackMCPFrontWithReader(newMCPFrontTestCmd(), api.NewAPI(), journal.reportPath, func(context.Context, string, []string, string) ([]byte, error) {
				readerCalls++
				return nil, errors.New("malformed conflict must refuse before pin read")
			})
			if err == nil || !strings.Contains(err.Error(), "invalid authority shape") {
				t.Fatalf("rollback=%v, want invalid authority refusal", err)
			}
			if readerCalls != 0 {
				t.Fatalf("reader calls=%d, want zero before malformed conflict refusal", readerCalls)
			}
			after, readErr := os.ReadFile(journal.reportPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != string(before) {
				t.Fatal("malformed conflict rollback rewrote the report")
			}
		})
	}
}

func TestMCPFrontV3_RollbackLoaderUsesRetainedBytesWithoutReopen(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	reportPath := filepath.Join(tmp, "recovery.json")
	record := newV3RollbackRecord(t, reportPath, 9137, "claude-code")
	key := v3SerenaRowKeyFor("claude-code")
	row := record.Rows[key]
	if row.Pin == nil {
		t.Fatal("fixture has no pin")
	}
	pinBytes, err := os.ReadFile(row.Pin.Path)
	if err != nil {
		t.Fatal(err)
	}
	configPath := claudeCodeConfigPath(t, tmp)
	if err := os.WriteFile(configPath, pinBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	baselineFingerprint, err := api.SerenaClientEntryFingerprint("claude-code", nil)
	if err != nil || baselineFingerprint == "" {
		t.Fatalf("baseline fingerprint=%q err=%v", baselineFingerprint, err)
	}
	seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": "http://127.0.0.1:9137/serena/mcp"},
	})
	appliedFingerprint, err := api.SerenaClientEntryFingerprint("claude-code", nil)
	if err != nil || appliedFingerprint == "" {
		t.Fatalf("applied fingerprint=%q err=%v", appliedFingerprint, err)
	}
	row.Baseline = mcpFrontSerenaState(baselineFingerprint)
	row.Attempt.PreState = row.Baseline
	row.Attempt.IntendedState = mcpFrontSerenaState(appliedFingerprint)
	row.Applied = &mcpFrontAppliedReceipt{
		Generation: row.Attempt.Generation, Port: 9137, PostState: row.Attempt.IntendedState,
	}
	record.Rows[key] = row
	record.ActivePlan.Operations[0] = mcpFrontPlanOperationForRow(
		key, row, row.Attempt.Operation, row.Attempt.PreState, row.Attempt.IntendedState,
	)
	if err := api.WriteStateFileAtomic(reportPath, record); err != nil {
		t.Fatal(err)
	}
	readerCalls := 0
	err = runRollbackMCPFrontWithReader(newMCPFrontTestCmd(), api.NewAPI(), reportPath, func(ctx context.Context, root string, components []string, digest string) ([]byte, error) {
		readerCalls++
		loaded, readErr := api.ReadStateFileBeneathRootNoFollow(ctx, root, components, digest)
		if readErr != nil {
			return nil, readErr
		}
		if removeErr := os.Remove(row.Pin.Path); removeErr != nil {
			return nil, fmt.Errorf("delete pin after retained-handle read: %w", removeErr)
		}
		return loaded, nil
	})
	if err != nil {
		t.Fatalf("rollback from retained bytes: %v", err)
	}
	if readerCalls != 1 {
		t.Fatalf("reader calls=%d, want exactly one", readerCalls)
	}
	if _, statErr := os.Stat(row.Pin.Path); !os.IsNotExist(statErr) {
		t.Fatalf("test reader did not remove pin pathname: %v", statErr)
	}
	if got, ok := claudeCodeEntryURL(t, configPath, "serena"); !ok || got != "http://127.0.0.1:9125/serena/mcp" {
		t.Fatalf("owned inverse did not consume retained baseline bytes: url=%q present=%v", got, ok)
	}
	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("successful rollback did not retire its report: %v", statErr)
	}
}

// TestMCPFrontV3_PriorAppliedConflictRollbackUsesOnlyOlderReceipt covers the
// retained-prior-authority shape after a durable restart. The happy path proves
// a real CAS inverse consumes the older receipt; the operator-edit path proves
// that same inverse refuses rather than overwriting newer live state.
func TestMCPFrontV3_PriorAppliedConflictRollbackUsesOnlyOlderReceipt(t *testing.T) {
	for _, test := range []struct {
		name         string
		operatorEdit bool
	}{
		{name: "restart-and-real-cas-restore"},
		{name: "operator-edit-refused", operatorEdit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tmp := mcpFrontPR588Env(t)
			reportPath := filepath.Join(tmp, "recovery.json")
			const client = "claude-code"
			record := newV3RollbackRecord(t, reportPath, 9137, client)
			key := v3SerenaRowKeyFor(client)
			row := record.Rows[key]
			if row.Pin == nil || row.Applied == nil || row.Attempt == nil {
				t.Fatalf("invalid retained-prior fixture: %+v", row)
			}
			baselineBytes, err := os.ReadFile(row.Pin.Path)
			if err != nil {
				t.Fatal(err)
			}
			configPath := claudeCodeConfigPath(t, tmp)
			if err := os.WriteFile(configPath, baselineBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			baselineFingerprint, err := api.SerenaClientEntryFingerprint(client, nil)
			if err != nil || baselineFingerprint == "" {
				t.Fatalf("baseline fingerprint=%q err=%v", baselineFingerprint, err)
			}
			seedClaudeCodeConfig(t, tmp, map[string]any{
				"serena": map[string]any{"url": "http://127.0.0.1:9137/serena/mcp"},
			})
			appliedFingerprint, err := api.SerenaClientEntryFingerprint(client, nil)
			if err != nil || appliedFingerprint == "" {
				t.Fatalf("applied fingerprint=%q err=%v", appliedFingerprint, err)
			}
			record.Generation = 2
			record.ActivePlan.Generation = record.Generation
			record.ActivePlan.Port = 9138
			row.Baseline = mcpFrontSerenaState(baselineFingerprint)
			row.Attempt = &mcpFrontReconcileAttempt{
				Generation: record.Generation, Operation: "add",
				PreState:      mcpFrontSerenaState(appliedFingerprint),
				IntendedState: mcpFrontSerenaState("front-port-fingerprint-generation-two"),
				State:         mcpFrontAttemptPreconditionConflict,
			}
			row.Applied = &mcpFrontAppliedReceipt{
				Generation: 1, Port: 9137, PostState: row.Attempt.PreState,
			}
			row.Disposition = &mcpFrontRollbackDisposition{
				State: mcpFrontDispositionPending, Reason: "forward-precondition-conflict-prior-owned",
			}
			record.Rows[key] = row
			record.ActivePlan.Operations[0] = mcpFrontPlanOperationForRow(
				key, row, row.Attempt.Operation, row.Attempt.PreState, row.Attempt.IntendedState,
			)
			if err := api.WriteStateFileAtomic(reportPath, &record); err != nil {
				t.Fatal(err)
			}

			// Reconstruct solely from the persisted bytes, not the fixture copy.
			restarted, err := readMCPFrontReconcileReport(reportPath)
			if err != nil {
				t.Fatalf("restart read: %v", err)
			}
			durable := restarted.Rows[key]
			durableOp, found := activeMCPFrontPlanOperation(restarted.ActivePlan, key)
			if durable.Pin == nil || durable.Pin.Path != row.Pin.Path || durable.Pin.SHA256 != row.Pin.SHA256 ||
				durable.Applied == nil || durable.Applied.Generation != 1 || durable.Applied.Port != 9137 ||
				!mcpFrontStateEqual(durable.Baseline, row.Baseline) || !mcpFrontStateEqual(durable.Applied.PostState, row.Applied.PostState) ||
				!found || !durableOp.PinBound || !mcpFrontSerenaPinEqual(durableOp.Pin, durable.Pin) {
				t.Fatalf("restart lost retained authority: durable=%+v fixture=%+v", durable, row)
			}
			persistedPinBytes, err := os.ReadFile(durable.Pin.Path)
			if err != nil || string(persistedPinBytes) != string(baselineBytes) {
				t.Fatalf("restart pin bytes changed: err=%v", err)
			}

			var operatorBytes []byte
			if test.operatorEdit {
				seedClaudeCodeConfig(t, tmp, map[string]any{
					"serena": map[string]any{"url": "https://operator.example/serena/mcp"},
				})
				operatorBytes, err = os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
			}

			rollbackErr := runRollbackMCPFront(newMCPFrontTestCmd(), api.NewAPI(), reportPath)
			if test.operatorEdit {
				if rollbackErr == nil || !strings.Contains(rollbackErr.Error(), "rollback completed") {
					t.Fatalf("operator-edited rollback=%v, want CAS refusal reported", rollbackErr)
				}
				assertMCPFrontBytesEqual(t, configPath, operatorBytes, "operator-edited config")
			} else {
				if rollbackErr != nil {
					t.Fatalf("older-receipt rollback: %v", rollbackErr)
				}
				// The owned CAS adapter restores the captured entry, not a
				// whole-file blob: JSON layout may be normalized while the exact
				// retained pin bytes stay available in the durable record above.
				if got, present := claudeCodeEntryURL(t, configPath, "serena"); !present || got != "http://127.0.0.1:9125/serena/mcp" {
					t.Fatalf("CAS rollback did not restore the retained baseline entry: url=%q present=%v", got, present)
				}
			}
			if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
				t.Fatalf("terminal CAS result did not retire recovery report: %v", statErr)
			}
		})
	}
}

func TestMCPFrontV3_AllPinsLoadBeforeAnyRollbackWrite(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	reportPath := filepath.Join(tmp, "recovery.json")
	record := newV3RollbackRecord(t, reportPath, 9137, "claude-code")
	firstKey := v3SerenaRowKeyFor("claude-code")
	firstRow := record.Rows[firstKey]
	secondKey := v3SerenaRowKeyFor("z-client")
	secondRow := firstRow
	secondRow.Client = "z-client"
	secondPin := *firstRow.Pin
	secondPin.Client = secondRow.Client
	secondPin.Origin = "rolling-z-client"
	secondPin.Path = filepath.Join(mcpFrontReconcilePinDir(reportPath), "z-client", "pre-reconcile.json")
	secondPin.SHA256 = strings.Repeat("0", 64)
	secondRow.Pin = &secondPin
	record.Rows[secondKey] = secondRow
	record.ActivePlan.Rows = append(record.ActivePlan.Rows, secondKey)
	record.ActivePlan.Operations = append(record.ActivePlan.Operations, mcpFrontPlanOperationForRow(
		secondKey, secondRow, secondRow.Attempt.Operation, secondRow.Attempt.PreState, secondRow.Attempt.IntendedState,
	))
	lspName := api.LSPRouterEntryName("go")
	lspKey := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "claude-code", "go", lspName)
	lspBefore := v3LSPSnapshot("claude-code", "go", lspName, false, "")
	lspIntended := v3LSPSnapshot("claude-code", "go", lspName, true, api.LSPRouterURL(9137, "go"))
	lspRow := mcpFrontReconcileRow{
		Surface: mcpFrontSurfaceLSP, Client: "claude-code", Language: "go", EntryName: lspName,
		Baseline: mcpFrontLSPState(lspBefore), BaselineSet: true,
	}
	record.Rows[lspKey] = lspRow
	record.ActivePlan.Rows = append(record.ActivePlan.Rows, lspKey)
	record.ActivePlan.Operations = append(record.ActivePlan.Operations, mcpFrontPlanOperationForRow(
		lspKey, lspRow, "add", mcpFrontLSPState(lspBefore), mcpFrontLSPState(lspIntended),
	))
	if err := api.WriteStateFileAtomic(reportPath, record); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	loadedFirst := []byte("first retained pin bytes")
	readerCalls := 0
	err = runRollbackMCPFrontWithReader(newMCPFrontTestCmd(), api.NewAPI(), reportPath, func(context.Context, string, []string, string) ([]byte, error) {
		readerCalls++
		if readerCalls == 1 {
			return loadedFirst, nil
		}
		return nil, errors.New("last sorted pin is unreadable")
	})
	if err == nil || !strings.Contains(err.Error(), "serena-pin-open-unsafe") {
		t.Fatalf("rollback=%v, want last-pin refusal before any inverse", err)
	}
	if readerCalls != 2 {
		t.Fatalf("reader calls=%d, want both sorted Serena pins", readerCalls)
	}
	for i, b := range loadedFirst {
		if b != 0 {
			t.Fatalf("loaded pin byte %d=%d, want zero after later pin failure", i, b)
		}
	}
	after, readErr := os.ReadFile(reportPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(before) {
		t.Fatal("rollback changed the report before every pin was verified")
	}
}

// TestMCPFrontV3_AllPinsOversizeRefusesBeforeBothConfigSurfaces extends the
// mixed-pin barrier with the independent too-large category. The final sorted
// pin fails before either the Serena inverse or LSP restore is reached, so the
// exact sandbox config bytes are the mutation oracle for both adapters.
func TestMCPFrontV3_AllPinsOversizeRefusesBeforeBothConfigSurfaces(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	reportPath := filepath.Join(tmp, "recovery.json")
	configPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena":                     map[string]any{"url": "http://127.0.0.1:9137/serena/mcp"},
		api.LSPRouterEntryName("go"): map[string]any{"url": "http://127.0.0.1:9137/lsp/go/mcp"},
	})
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	record := newV3RollbackRecord(t, reportPath, 9137, "claude-code")
	firstKey := v3SerenaRowKeyFor("claude-code")
	firstRow := record.Rows[firstKey]
	secondKey := v3SerenaRowKeyFor("z-client")
	secondRow := firstRow
	secondRow.Client = "z-client"
	secondPin := *firstRow.Pin
	secondPin.Client = secondRow.Client
	secondPin.Origin = "rolling-z-client"
	secondPin.Path = filepath.Join(mcpFrontReconcilePinDir(reportPath), "z-client", "pre-reconcile.json")
	secondPin.SHA256 = strings.Repeat("f", 64)
	secondRow.Pin = &secondPin
	record.Rows[secondKey] = secondRow
	record.ActivePlan.Rows = append(record.ActivePlan.Rows, secondKey)
	record.ActivePlan.Operations = append(record.ActivePlan.Operations, mcpFrontPlanOperationForRow(
		secondKey, secondRow, secondRow.Attempt.Operation, secondRow.Attempt.PreState, secondRow.Attempt.IntendedState,
	))
	lspName := api.LSPRouterEntryName("go")
	lspKey := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "claude-code", "go", lspName)
	lspBefore := v3LSPSnapshot("claude-code", "go", lspName, true, api.LSPRouterURL(9137, "go"))
	lspRow := mcpFrontReconcileRow{
		Surface: mcpFrontSurfaceLSP, Client: "claude-code", Language: "go", EntryName: lspName,
		Baseline: mcpFrontLSPState(lspBefore), BaselineSet: true,
	}
	record.Rows[lspKey] = lspRow
	record.ActivePlan.Rows = append(record.ActivePlan.Rows, lspKey)
	record.ActivePlan.Operations = append(record.ActivePlan.Operations, mcpFrontPlanOperationForRow(
		lspKey, lspRow, "add", mcpFrontLSPState(lspBefore), mcpFrontLSPState(lspBefore),
	))
	if err := api.WriteStateFileAtomic(reportPath, record); err != nil {
		t.Fatal(err)
	}
	reportBefore, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	loadedFirst := []byte("first retained pin bytes")
	readerCalls := 0
	err = runRollbackMCPFrontWithReader(newMCPFrontTestCmd(), api.NewAPI(), reportPath, func(context.Context, string, []string, string) ([]byte, error) {
		readerCalls++
		if readerCalls == 1 {
			return loadedFirst, nil
		}
		return nil, &api.StateFileReadError{Category: api.StateFileReadErrorTooLarge, Operation: "read"}
	})
	if err == nil || !strings.Contains(err.Error(), "serena-pin-too-large") {
		t.Fatalf("oversize last pin rollback=%v, want typed oversize refusal", err)
	}
	if readerCalls != 2 {
		t.Fatalf("reader calls=%d, want every sorted Serena pin before any write", readerCalls)
	}
	for i, b := range loadedFirst {
		if b != 0 {
			t.Fatalf("loaded pin byte %d=%d, want zero after oversize refusal", i, b)
		}
	}
	assertMCPFrontBytesEqual(t, reportPath, reportBefore, "oversize recovery report")
	assertMCPFrontBytesEqual(t, configPath, configBefore, "oversize Serena/LSP config")
}

func TestMCPFrontV3_SerenaPinReadDiagnosticUsesTypedCategory(t *testing.T) {
	tests := []struct {
		category api.StateFileReadErrorCategory
		want     string
	}{
		{category: api.StateFileReadErrorInvalidInput, want: "serena-pin-set-invalid"},
		{category: api.StateFileReadErrorCanceled, want: "serena-pin-read-canceled"},
		{category: api.StateFileReadErrorTooLarge, want: "serena-pin-too-large"},
		{category: api.StateFileReadErrorChecksumMismatch, want: "serena-pin-checksum-mismatch"},
		{category: api.StateFileReadErrorUnsafeObjectOrIO, want: "serena-pin-open-unsafe"},
	}
	for _, test := range tests {
		t.Run(string(test.category), func(t *testing.T) {
			err := fmt.Errorf("caller prose changed: %w", &api.StateFileReadError{Category: test.category, Operation: "read"})
			if got := mcpFrontSerenaPinReadDiagnostic(err); got != test.want {
				t.Fatalf("diagnostic=%q, want %q", got, test.want)
			}
		})
	}
}

func TestMCPFrontV3_UncertainAttemptBlocksPlanReplacement(t *testing.T) {
	name := api.LSPRouterEntryName("go")
	pre := v3LSPSnapshot("claude-code", "go", name, false, "")
	post := v3LSPSnapshot("claude-code", "go", name, true, api.LSPRouterURL(9137, "go"))
	op := v3LSPAdd("claude-code", "go", name, pre, post)
	journal := v3Journal(t, 9137, nil, op)
	if err := journal.prepareLSPOperation(op); err != nil {
		t.Fatal(err)
	}
	prior, err := readMCPFrontReconcileReport(journal.reportPath)
	if err != nil {
		t.Fatal(err)
	}
	uncertain, settleErr := settleMCPFrontReconcileAttempts(journal.reportPath, prior)
	if settleErr != nil {
		t.Fatalf("settlement must make the pending marker durable: %v", settleErr)
	}
	if len(uncertain) == 0 {
		t.Fatal("uncertain prepared row must block a replacement generation")
	}
}

// TestMCPFrontV3_ForwardDispositionGateIsPlanAgnostic completes the admission
// cross-product: a terminal or pending disposition blocks both a retry that
// asks for the same plan and one that asks for a different port. The raw report
// and sandbox config bytes prove the gate fires before generation/plan/pin or
// either adapter can change.
func TestMCPFrontV3_ForwardDispositionGateIsPlanAgnostic(t *testing.T) {
	for _, test := range []struct {
		name           string
		pending        bool
		changedRequest bool
	}{
		{name: "terminal-same-request"},
		{name: "terminal-changed-request", changedRequest: true},
		{name: "pending-same-request", pending: true},
		{name: "pending-changed-request", pending: true, changedRequest: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reportPath, port, configPath, configBefore := setupMCPFrontForwardRefusalTest(t)
			planPort := port
			if test.changedRequest {
				planPort++
			}
			const client, language = "claude-code", "go"
			entryName := api.LSPRouterEntryName(language)
			pre := v3LSPSnapshot(client, language, entryName, false, "")
			intended := v3LSPSnapshot(client, language, entryName, true, api.LSPRouterURL(planPort, language))
			op := v3LSPAdd(client, language, entryName, pre, intended)
			journal := v3JournalAt(t, reportPath, planPort, nil, op)
			if test.pending {
				if err := journal.prepareLSPOperation(op); err != nil {
					t.Fatal(err)
				}
				if err := journal.finishLSPOperation(api.LSPRouterMutationObservation{
					Operation: op, Invoked: true, ObservationErr: errors.New("deterministic post-invocation readback failure"),
				}); err == nil {
					t.Fatal("post-invocation failure must produce pending disposition")
				}
			} else if err := journal.finishLSPOperation(api.LSPRouterMutationObservation{
				Operation: op, ObservedState: pre, Invoked: false, PreconditionConflict: true,
			}); err != nil {
				t.Fatalf("persist terminal no-write disposition: %v", err)
			}
			before, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeRecord, err := readMCPFrontReconcileReport(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			forwardErr := runReconcileMCPFront(newMCPFrontTestCmd(), false)
			if forwardErr == nil || !strings.Contains(forwardErr.Error(), "forward-recovery-disposition-active") {
				t.Fatalf("forward admission=%v, want disposition gate", forwardErr)
			}
			assertMCPFrontBytesEqual(t, reportPath, before, "disposition-gated report")
			assertMCPFrontBytesEqual(t, configPath, configBefore, "disposition-gated Serena/LSP config")
			afterRecord, err := readMCPFrontReconcileReport(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			if afterRecord.Generation != beforeRecord.Generation || afterRecord.ActivePlan == nil || beforeRecord.ActivePlan == nil ||
				afterRecord.ActivePlan.Generation != beforeRecord.ActivePlan.Generation || afterRecord.ActivePlan.Port != beforeRecord.ActivePlan.Port ||
				len(afterRecord.Rows) != len(beforeRecord.Rows) {
				t.Fatalf("disposition gate changed report metadata: before=%+v after=%+v", beforeRecord, afterRecord)
			}
		})
	}
}

func TestMCPFrontV3_ConfirmedNoWriteKeepsEarlierPortOwnership(t *testing.T) {
	name := api.LSPRouterEntryName("go")
	pre := v3LSPSnapshot("claude-code", "go", name, false, "")
	atA := v3LSPSnapshot("claude-code", "go", name, true, api.LSPRouterURL(9137, "go"))
	firstOp := v3LSPAdd("claude-code", "go", name, pre, atA)
	first := v3Journal(t, 9137, nil, firstOp)
	if err := first.prepareLSPOperation(firstOp); err != nil {
		t.Fatal(err)
	}
	if err := first.finishLSPOperation(api.LSPRouterMutationObservation{
		Operation: firstOp, ObservedState: atA, Invoked: true,
	}); err != nil {
		t.Fatal(err)
	}
	prior := &first.record
	atB := v3LSPSnapshot("claude-code", "go", name, true, api.LSPRouterURL(9138, "go"))
	retryOp := v3LSPAdd("claude-code", "go", name, atA, atB)
	retry := v3Journal(t, 9138, prior, retryOp)
	if err := retry.prepareLSPOperation(retryOp); err != nil {
		t.Fatal(err)
	}
	if err := retry.finishLSPOperation(api.LSPRouterMutationObservation{
		Operation: retryOp, ObservedState: atA, Invoked: true,
	}); err != nil {
		t.Fatal(err)
	}
	row := retry.record.Rows[mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "claude-code", "go", name)]
	receipt, uncertain := effectiveMCPFrontAppliedReceipt(row)
	if uncertain || receipt == nil || receipt.Port != 9137 ||
		row.Attempt.State != mcpFrontAttemptConfirmedNoWrite {
		t.Fatalf("confirmed-no-effect lost earlier receipt: row=%+v receipt=%+v", row, receipt)
	}
}

func TestMCPFrontV3_PostWriteEvidenceFailureStaysPending(t *testing.T) {
	name := api.LSPRouterEntryName("go")
	pre := v3LSPSnapshot("claude-code", "go", name, false, "")
	post := v3LSPSnapshot("claude-code", "go", name, true, api.LSPRouterURL(9137, "go"))
	op := v3LSPAdd("claude-code", "go", name, pre, post)
	journal := v3Journal(t, 9137, nil, op)
	if err := journal.prepareLSPOperation(op); err != nil {
		t.Fatal(err)
	}
	if err := journal.finishLSPOperation(api.LSPRouterMutationObservation{
		Operation: op, Invoked: true, ObservationErr: errors.New("induced readback failure"),
	}); err == nil {
		t.Fatal("post-write readback failure must stop later writes")
	}
	row := journal.record.Rows[mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "claude-code", "go", name)]
	if row.Applied != nil || row.Disposition == nil || row.Disposition.State != mcpFrontDispositionPending {
		t.Fatalf("post-write unknown became terminal/owned: %+v", row)
	}
}

func TestMCPFrontV3_PersistenceFailureOrPendingGroupPreventsRetirement(t *testing.T) {
	report := &mcpFrontReconcileReport{
		Version: mcpFrontReconcileReportVersion, SnapshotComplete: true, Generation: 1,
		Rows: map[string]mcpFrontReconcileRow{
			"x": {Surface: mcpFrontSurfaceLSP, Client: "c", Language: "go", EntryName: "x",
				Disposition: &mcpFrontRollbackDisposition{State: mcpFrontDispositionPending}},
		},
		ActivePlan: &mcpFrontReconcilePlan{Generation: 1, Port: 9137},
	}
	if canRetireMCPFrontReconcileReport(report) {
		t.Fatal("pending row authorized retirement")
	}
}

func TestMCPFrontV3_V1AndV2ArtifactsRefuseBeforeAnyWrite(t *testing.T) {
	for _, version := range []int{mcpFrontReconcileLegacyReportVersion, mcpFrontReconcileVersion2} {
		raw, err := json.Marshal(map[string]any{"version": version, "rows": map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = decodeMCPFrontReconcileReport(raw, "record.json")
		if err == nil || !strings.Contains(err.Error(), "legacy-ownership-unproven") {
			t.Fatalf("version %d err=%v", version, err)
		}
	}
}

// legacyMCPFrontJournalBody is the pre-version-3 record shape: two value-typed
// recovery sections plus (from version 2) a top-level pin array, with no `rows`
// map, no `active_plan`, and no per-row attempt or applied receipt anywhere.
// That absence IS the reason version 3 refuses to upgrade one: there is nothing
// in these bytes that says which client write actually landed.
func legacyMCPFrontJournalBody(version int, frontSerenaURL, backupPath string) map[string]any {
	body := map[string]any{
		"version":           version,
		"port":              9137,
		"snapshot_complete": true,
		"serena": map[string]any{"applied": []any{
			map[string]any{
				"server": "serena", "client": "claude-code",
				"url": frontSerenaURL, "backup_path": backupPath,
			},
		}},
		"lsp": []any{},
	}
	if version >= mcpFrontReconcileVersion2 {
		// Version 1 predates pinning; version 2 added the top-level array.
		body["pins"] = []any{map[string]any{
			"client": "claude-code", "origin": "rolling",
			"path": backupPath, "sha256": "deadbeef",
		}}
	}
	return body
}

// TestMCPFrontV3_LegacyJournalOnDiskRefusesRollbackWithAnActionableMessage is
// the FIELD path for the version-3 migration decision, and the reason that
// decision had to be written down at all.
//
// The situation is ordinary: an operator ran `--reconcile-mcp-front` on an
// older mcphub, upgraded, and now needs `--rollback`. Their journal is on disk
// in the old shape. Version 3 answers that by REFUSING, never by upgrading in
// place — a version-1/2 journal records which entries were captured, never
// which client write landed, so synthesising row authority from it would let
// the inverse overwrite an entry this hub never wrote.
//
// Refusing is only half an answer, though. The refusal the operator actually
// met was `json: unknown field "lsp"` (or, for a genuine version-2 file,
// `artifact version 2 cannot be consumed by version 3`) — neither of which
// names the file, says why it will not be upgraded, or offers a way forward.
// This test is the guard on the message being usable, not merely correct:
// the path, the reason, and BOTH concrete remedies must be in it.
//
// Three inputs, one contract. The third is the one a strict decoder turns into
// an unrecognisable error: an interim pre-release build that stamped the
// current version number onto the old body.
func TestMCPFrontV3_LegacyJournalOnDiskRefusesRollbackWithAnActionableMessage(t *testing.T) {
	const frontSerenaURL = "http://127.0.0.1:9137/serena/mcp"

	cases := []struct {
		name       string
		version    int
		wantDetail string
	}{
		{
			name:       "genuine version-1 journal",
			version:    mcpFrontReconcileLegacyReportVersion,
			wantDetail: "it declares version 1",
		},
		{
			name:       "genuine version-2 journal",
			version:    mcpFrontReconcileVersion2,
			wantDetail: "it declares version 2",
		},
		{
			name:       "interim build stamped version 3 onto a version-2 body",
			version:    mcpFrontReconcileReportVersion,
			wantDetail: "carries the pre-version-3 body shape",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := mcpFrontPR588Env(t)
			assertRedirectedStateDir(t, tmp)
			reportPath := withMCPFrontReportPathSeam(t)

			// A host mid-cutover: serena IS on the front port, so a journal
			// consumed on wrong authority is exactly what cannot be undone.
			cfgPath := seedClaudeCodeConfig(t, tmp, map[string]any{
				"serena": map[string]any{"url": frontSerenaURL},
			})
			configBefore, rerr := os.ReadFile(cfgPath)
			if rerr != nil {
				t.Fatalf("read seeded config: %v", rerr)
			}

			backupPath := filepath.Join(tmp, "legacy-backup.json")
			if werr := api.WriteStateFileAtomic(reportPath,
				legacyMCPFrontJournalBody(tc.version, frontSerenaURL, backupPath)); werr != nil {
				t.Fatalf("seed legacy journal: %v", werr)
			}
			journalBefore, rerr := os.ReadFile(reportPath)
			if rerr != nil {
				t.Fatalf("read seeded journal: %v", rerr)
			}

			err := runReconcileMCPFront(newMCPFrontTestCmd(), true)
			if err == nil {
				t.Fatalf("rollback must refuse a pre-version-3 journal: consuming it would write client configs on authority the old format never recorded")
			}
			for _, want := range []string{
				// The discriminator the decision's failure-mode table requires.
				"legacy-ownership-unproven",
				// WHICH file: an operator with several state files cannot act
				// on a message that does not name one.
				reportPath,
				// WHY it is refused rather than upgraded.
				"never which client write actually landed",
				// That nothing has happened yet.
				"no client config was touched",
				// Remedy 1: roll back with the binary that wrote it.
				"OLDER mcphub binary",
				// Remedy 2: move it aside and restore by hand.
				".legacy",
				// Which of the three inputs this is.
				tc.wantDetail,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal must carry %q so the operator can act on it; got:\n%v", want, err)
				}
			}

			configAfter, rerr := os.ReadFile(cfgPath)
			if rerr != nil {
				t.Fatalf("read config after the refusal: %v", rerr)
			}
			if string(configAfter) != string(configBefore) {
				t.Fatalf("a refused legacy journal must leave every client byte-identical.\nbefore: %s\nafter:  %s", configBefore, configAfter)
			}
			journalAfter, rerr := os.ReadFile(reportPath)
			if rerr != nil {
				t.Fatalf("a refused legacy journal must SURVIVE — it is the operator's only record of the pre-reconcile state: %v", rerr)
			}
			if string(journalAfter) != string(journalBefore) {
				t.Fatalf("the journal must not be repaired, projected or partially upgraded in place.\nbefore: %s\nafter:  %s", journalBefore, journalAfter)
			}
			// No retired sibling either: refusing is not consuming.
			retired, gerr := filepath.Glob(reportPath + ".retired-*")
			if gerr != nil {
				t.Fatalf("glob retired reports: %v", gerr)
			}
			if len(retired) != 0 {
				t.Fatalf("a refused legacy journal must not be retired out of the active namespace; found %v", retired)
			}
		})
	}
}

func TestMCPFrontV3_LSPPreconditionConflictIsDurable(t *testing.T) {
	TestMCPFrontV3_NoInvocationStateEqualityNeverCreatesReceipt(t)
}

func TestMCPFrontV3_SerenaAddFailureDoesNotPromoteApplied(t *testing.T) {
	backupPath := filepath.Join(t.TempDir(), "backup.json")
	if err := os.WriteFile(backupPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := v3Journal(t, 9137, nil)
	prepared := api.SerenaReconcileAttemptResult{
		Client: "claude-code", BackupPath: backupPath,
		PreFingerprint: "", IntendedFingerprint: "intended",
	}
	if err := journal.prepareSerenaAttempt(prepared); err != nil {
		t.Fatal(err)
	}
	prepared.Invoked = true
	prepared.ObservedFingerprint = ""
	prepared.AdapterErr = errors.New("induced add failure")
	if err := journal.finishSerenaAttempt(prepared); err != nil {
		t.Fatal(err)
	}
	row := journal.record.Rows[mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, "claude-code", "", "serena")]
	if row.Applied != nil || row.Attempt.State != mcpFrontAttemptConfirmedNoWrite {
		t.Fatalf("failed add promoted ownership: %+v", row)
	}
}

func TestMCPFrontV3_RowsExclusivelyOwnSerenaPins(t *testing.T) {
	backupPath := filepath.Join(t.TempDir(), "backup.json")
	if err := os.WriteFile(backupPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := v3Journal(t, 9137, nil)
	if err := journal.prepareSerenaAttempt(api.SerenaReconcileAttemptResult{
		Client: "claude-code", BackupPath: backupPath,
		PreFingerprint: "", IntendedFingerprint: "intended",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(journal.reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"serena", "lsp", "pins", "applied", "port"} {
		if _, ok := top[forbidden]; ok {
			t.Fatalf("version-3 artifact still persists forbidden top-level projection %q", forbidden)
		}
	}
	row := journal.record.Rows[mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, "claude-code", "", "serena")]
	if row.Pin == nil || row.Pin.Path == "" || row.Pin.SHA256 == "" {
		t.Fatalf("serena row does not own its pin: %+v", row)
	}
	if _, err := loadMCPFrontVerifiedSerenaPins(context.Background(), &journal.record, journal.reportPath); err != nil {
		t.Fatalf("valid row-owned pin: %v", err)
	}
}

func TestMCPFrontV3_RollbackCallerRereadsDurableStateBeforeRetirement(t *testing.T) {
	const client, language = "missing-test-client", "go"
	entryName := api.LSPRouterEntryName(language)
	baseline := v3LSPSnapshot(client, language, entryName, false, "")
	intended := v3LSPSnapshot(client, language, entryName, true, api.LSPRouterURL(9137, language))
	journal := v3Journal(t, 9137, nil, v3LSPAdd(client, language, entryName, baseline, intended))

	durablePending, err := readMCPFrontReconcileReport(journal.reportPath)
	if err != nil {
		t.Fatalf("read pending durable report: %v", err)
	}
	if canRetireMCPFrontReconcileReport(durablePending) {
		t.Fatal("fresh journal unexpectedly starts terminal")
	}

	origReadForRetirement := mcpFrontReadReportForRetirementFn
	origRetire := mcpFrontRetireReportFn
	retireCalls := 0
	mcpFrontReadReportForRetirementFn = func(path string) (*mcpFrontReconcileReport, error) {
		if path != journal.reportPath {
			t.Fatalf("retirement re-read path=%q, want %q", path, journal.reportPath)
		}
		return durablePending, nil
	}
	mcpFrontRetireReportFn = func(string) (string, error) {
		retireCalls++
		return "", errors.New("retirement must not be attempted while durable state is pending")
	}
	t.Cleanup(func() {
		mcpFrontReadReportForRetirementFn = origReadForRetirement
		mcpFrontRetireReportFn = origRetire
	})

	err = runRollbackMCPFront(newMCPFrontTestCmd(), api.NewAPI(), journal.reportPath)
	if retireCalls != 0 {
		t.Fatalf("retirement attempts=%d, want zero while durable report is pending", retireCalls)
	}
	if err == nil || !strings.Contains(err.Error(), "recovery remains pending") {
		t.Fatalf("rollback error=%v, want durable-pending refusal", err)
	}
	if _, statErr := os.Stat(journal.reportPath); statErr != nil {
		t.Fatalf("active recovery report must be preserved: %v", statErr)
	}
}

// --- B1: `conflict` is an uncertain state, and settlement must say so --------

// v3ConflictedLSPJournal drives one LSP row all the way to a durable
// post-write `conflict`: the mutation WAS invoked, and the same-call readback
// then observed a value that is neither the pre-state nor the intended state.
// Nothing on the host can say whether that value is ours, so the row carries no
// receipt and no terminal disposition — it is uncertain in exactly the sense
// effectiveMCPFrontAppliedReceipt already means by it.
func v3ConflictedLSPJournal(t *testing.T) (*mcpFrontReconcileJournal, string) {
	t.Helper()
	const client, language = "claude-code", "go"
	name := api.LSPRouterEntryName(language)
	pre := v3LSPSnapshot(client, language, name, false, "")
	intended := v3LSPSnapshot(client, language, name, true, api.LSPRouterURL(9137, language))
	// A third value: not what we started from, not what we meant to write.
	surprise := v3LSPSnapshot(client, language, name, true, "http://127.0.0.1:19999/mcp")
	op := v3LSPAdd(client, language, name, pre, intended)

	journal := v3Journal(t, 9137, nil, op)
	if err := journal.prepareLSPOperation(op); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := journal.finishLSPOperation(api.LSPRouterMutationObservation{
		Operation: op, ObservedState: surprise, Invoked: true,
	}); err == nil {
		t.Fatal("a post-write conflict must be reported to its caller")
	}
	key := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, client, language, name)
	if got := journal.record.Rows[key].Attempt.State; got != mcpFrontAttemptConflict {
		t.Fatalf("attempt state = %q, want %q — this fixture no longer produces the state it exists to cover", got, mcpFrontAttemptConflict)
	}
	return journal, key
}

// TestMCPFrontV3_PostWriteConflictBlocksTheNextForwardGeneration is the B1
// guard.
//
// `conflict` and `prepared` are the SAME classification —
// effectiveMCPFrontAppliedReceipt returns uncertain for both, and the validator
// refuses both when they belong to a superseded generation. Only settlement
// disagreed: it filtered on `prepared` alone, so a `conflict` row walked
// straight through the forward gate that exists to stop it.
func TestMCPFrontV3_PostWriteConflictBlocksTheNextForwardGeneration(t *testing.T) {
	journal, key := v3ConflictedLSPJournal(t)

	prior, err := readMCPFrontReconcileReport(journal.reportPath)
	if err != nil {
		t.Fatalf("a conflicted generation must stay readable by its own reader: %v", err)
	}
	if _, uncertain := effectiveMCPFrontAppliedReceipt(prior.Rows[key]); !uncertain {
		t.Fatal("premise broken: the classifier no longer calls a post-write conflict uncertain")
	}
	uncertain, settleErr := settleMCPFrontReconcileAttempts(journal.reportPath, prior)
	if settleErr != nil {
		t.Fatalf("settlement must make the pending marker durable: %v", settleErr)
	}
	if len(uncertain) != 1 || uncertain[0] != key {
		t.Fatalf("an uncertain post-write conflict must block the next forward generation, exactly as a prepared attempt does; uncertain=%v", uncertain)
	}
}

// TestMCPFrontV3_ForwardNeverPublishesAnArtifactItsOwnReaderRefuses is the B1
// consequence guard, and the reason the classification mismatch was not merely
// untidy.
//
// It replays the forward pass's exact order — settle, build the next
// generation, persist — and then asks the one question that decides whether the
// operator still has a way out: can this binary read what it just wrote? A
// carried-forward conflict at generation G, stamped against an active plan at
// G+1, trips the validator's own superseded-uncertainty check, and BOTH exits
// are then closed: the forward run refuses to merge into an unreadable prior,
// and `--rollback` refuses to consume it. The documented escape (move the file
// aside) destroys the only rollback authority for clients already on the front
// port.
//
// The guard is deliberately written against the OUTCOME rather than against one
// implementation site, so it holds whether the refusal lands in settlement or
// in the journal constructor.
func TestMCPFrontV3_ForwardNeverPublishesAnArtifactItsOwnReaderRefuses(t *testing.T) {
	journal, _ := v3ConflictedLSPJournal(t)

	prior, err := readMCPFrontReconcileReport(journal.reportPath)
	if err != nil {
		t.Fatalf("read the conflicted generation: %v", err)
	}
	uncertain, settleErr := settleMCPFrontReconcileAttempts(journal.reportPath, prior)
	if settleErr == nil && len(uncertain) == 0 {
		next, buildErr := newMCPFrontV3Journal(journal.reportPath, prior, 9138,
			&api.LSPRouterClientPlan{Port: 9138})
		if buildErr == nil {
			// runForwardReconcileMCPFront persists here, BEFORE the first client
			// mutation, so this byte sequence reaches disk on a run that has
			// changed nothing yet.
			if perr := next.persist(); perr != nil {
				t.Fatalf("persist the replacement generation: %v", perr)
			}
		}
	}

	if _, rerr := readMCPFrontReconcileReport(journal.reportPath); rerr != nil {
		t.Fatalf("the forward pass published a recovery artifact that its own reader refuses, so `--rollback` and the next forward run are both dead ends and the operator's only way back is to delete the record: %v", rerr)
	}
}

// --- B2: rollback uncertainty is row-local, never a global early return ------

// TestMCPFrontV3_UncertainRowDoesNotSuppressAnIndependentRollback is the B2
// guard for invariant I11.
//
// The forward run below genuinely rewrites claude-code's serena entry onto the
// front port and records an applied receipt for it. A SECOND, unrelated row is
// then left uncertain — the state a post-write readback failure produces
// without any crash: finishAttempt records the pending disposition while the
// attempt is still `prepared`, and that shape is durable.
//
// Rollback owed the first row its inverse regardless. The pre-fix code returned
// on the settlement error before the serena loop ran, so one transient readback
// failure on any row left EVERY client stranded on the front port with the
// rollback reporting zero restorations.
func TestMCPFrontV3_UncertainRowDoesNotSuppressAnIndependentRollback(t *testing.T) {
	const preReconcileURL = "http://127.0.0.1:9125/serena/mcp"

	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	configPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": preReconcileURL},
	})
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward reconcile: %v", err)
	}
	if got, _ := claudeCodeEntryURL(t, configPath, "serena"); got == preReconcileURL {
		t.Fatalf("premise broken: the forward run did not move serena off %q", preReconcileURL)
	}

	// Inject one independent uncertain row into the durable record. It belongs
	// to a client that is not installed here, so it can never be restored and
	// must stay pending — which is precisely why it must not be allowed to
	// decide anything about claude-code.
	record, err := readMCPFrontReconcileReport(reportPath)
	if err != nil {
		t.Fatalf("read the forward record: %v", err)
	}
	const otherClient, language = "missing-test-client", "go"
	entryName := api.LSPRouterEntryName(language)
	baseline := v3LSPSnapshot(otherClient, language, entryName, false, "")
	intended := v3LSPSnapshot(otherClient, language, entryName, true, api.LSPRouterURL(port, language))
	uncertainKey := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, otherClient, language, entryName)
	uncertainRow := mcpFrontReconcileRow{
		Surface: mcpFrontSurfaceLSP, Client: otherClient, Language: language, EntryName: entryName,
		Baseline: mcpFrontLSPState(baseline), BaselineSet: true,
		Attempt: &mcpFrontReconcileAttempt{
			Generation: record.Generation, Operation: "add",
			PreState: mcpFrontLSPState(baseline), IntendedState: mcpFrontLSPState(intended),
			State: mcpFrontAttemptPrepared,
		},
	}
	record.Rows[uncertainKey] = uncertainRow
	record.ActivePlan.Rows = append(record.ActivePlan.Rows, uncertainKey)
	record.ActivePlan.Operations = append(record.ActivePlan.Operations, mcpFrontPlanOperationForRow(
		uncertainKey, uncertainRow, "add", mcpFrontLSPState(baseline), mcpFrontLSPState(intended),
	))
	if werr := api.WriteStateFileAtomic(reportPath, record); werr != nil {
		t.Fatalf("persist the injected uncertain row: %v", werr)
	}
	if _, verr := readMCPFrontReconcileReport(reportPath); verr != nil {
		t.Fatalf("the injected record must be a VALID version-3 artifact, otherwise this test proves nothing about rollback: %v", verr)
	}

	rollbackErr := runReconcileMCPFront(newMCPFrontTestCmd(), true)

	if got, _ := claudeCodeEntryURL(t, configPath, "serena"); got != preReconcileURL {
		t.Fatalf("an unrelated uncertain row suppressed a rollback that was owed: serena is %q, want its pre-reconcile %q (rollback err = %v)", got, preReconcileURL, rollbackErr)
	}
	if rollbackErr == nil {
		t.Fatal("an unresolved uncertain row must still produce a non-zero outcome — the operator has to know something was left behind")
	}
	if !strings.Contains(rollbackErr.Error(), "recovery remains pending") {
		t.Fatalf("the outcome must be the aggregate pending report, not a global refusal; got: %v", rollbackErr)
	}
	if _, statErr := os.Stat(reportPath); statErr != nil {
		t.Fatalf("a record with a pending row must be preserved: %v", statErr)
	}
}

// --- B3: a forward no-write conflict is not an operator edit -----------------

// TestMCPFrontV3_ForwardNoWriteConflictIsNotReportedAsAnOperatorEdit is the B3
// guard.
//
// Two very different things reach the terminal `skipped-conflict` disposition.
// A ROLLBACK-time compare-and-swap conflict means the live entry no longer
// equals what the forward run recorded writing — somebody edited it, and
// restoring would discard that edit. A FORWARD-time precondition conflict means
// the opposite: the forward run's own precondition check refused before any
// write, so the reconcile never touched that entry at all.
//
// The closing message keyed on the disposition alone and told both of them the
// first story. On a host carrying several such rows that is a false accusation
// on every run, and it points the operator at an edit they never made.
func TestMCPFrontV3_ForwardNoWriteConflictIsNotReportedAsAnOperatorEdit(t *testing.T) {
	const client, language = "missing-test-client", "go"
	entryName := api.LSPRouterEntryName(language)
	baseline := v3LSPSnapshot(client, language, entryName, false, "")
	intended := v3LSPSnapshot(client, language, entryName, true, api.LSPRouterURL(9137, language))
	op := v3LSPAdd(client, language, entryName, baseline, intended)

	journal := v3Journal(t, 9137, nil, op)
	// Invoked=false plus PreconditionConflict: the wrapper refused before
	// writing anything, which is the row shape finishAttempt makes terminal.
	if err := journal.finishLSPOperation(api.LSPRouterMutationObservation{
		Operation: op, ObservedState: baseline, Invoked: false, PreconditionConflict: true,
	}); err != nil {
		t.Fatalf("a forward-plan precondition conflict must persist without failing the run: %v", err)
	}
	key := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, client, language, entryName)
	row := journal.record.Rows[key]
	if row.Disposition == nil ||
		row.Disposition.State != mcpFrontDispositionConflict ||
		row.Disposition.Reason != "forward-plan-precondition-conflict" {
		t.Fatalf("premise broken: the fixture no longer produces a forward no-write conflict: %+v", row.Disposition)
	}

	err := runRollbackMCPFront(newMCPFrontTestCmd(), api.NewAPI(), journal.reportPath)
	if err == nil {
		t.Fatal("a skipped row must still produce a non-zero outcome")
	}
	if strings.Contains(err.Error(), "edited after the reconcile ran") {
		t.Fatalf("the reconcile never wrote this row — its own precondition check refused first — so telling the operator their config was edited after the reconcile ran accuses them of an edit they did not make and sends them looking for it; got:\n%v", err)
	}
	if !strings.Contains(err.Error(), strings.ReplaceAll(key, "\x00", "/")) {
		t.Fatalf("the message must still name the exact row it skipped; got:\n%v", err)
	}
}

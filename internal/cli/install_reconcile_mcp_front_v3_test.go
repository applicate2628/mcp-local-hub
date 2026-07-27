package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
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

func v3Journal(t *testing.T, port int, prior *mcpFrontReconcileReport, ops ...api.LSPRouterPlannedOperation) *mcpFrontReconcileJournal {
	t.Helper()
	reportPath := filepath.Join(t.TempDir(), "recovery.json")
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
	if err := settleMCPFrontReconcileAttempts(journal.reportPath, reloaded); err == nil {
		t.Fatal("re-entry must not equality-promote a prepared attempt")
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
	if err := settleMCPFrontReconcileAttempts(journal.reportPath, prior); err == nil {
		t.Fatal("uncertain prepared row must block a replacement generation")
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
	if err := verifyMCPFrontSerenaPins(&journal.record, journal.reportPath); err != nil {
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

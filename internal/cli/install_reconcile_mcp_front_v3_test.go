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
	return mcpFrontReconcileReport{
		Version:          mcpFrontReconcileReportVersion,
		SnapshotComplete: true,
		Generation:       1,
		Rows: map[string]mcpFrontReconcileRow{
			key: {
				Surface: mcpFrontSurfaceSerena, Client: client, EntryName: "serena",
				Baseline: baseline, BaselineSet: true, Pin: &pin,
				Attempt: &mcpFrontReconcileAttempt{
					Generation: 1, Operation: "add",
					PreState: baseline, IntendedState: intended,
					State: mcpFrontAttemptApplied,
				},
				Applied: &mcpFrontAppliedReceipt{Generation: 1, Port: port, PostState: intended},
			},
		},
		ActivePlan: &mcpFrontReconcilePlan{
			Generation: 1, Port: port, Rows: []string{key},
			Operations: []mcpFrontReconcilePlanOp{{
				RowKey: key, Operation: "add", PreState: baseline, IntendedState: intended,
			}},
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

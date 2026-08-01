// internal/api/lsp_client_router_snapshot_review_test.go
//
// Pre-submission adversarial-review coverage for the LSP half of the
// `mcphub install --reconcile-mcp-front` recovery record:
//
//   - finding 4 [P2]: a snapshotted client that is not reachable at restore
//     time was silently skipped, so the caller saw a clean report and deleted
//     the record — permanently losing that client's rollback row.
//   - finding 6 [P2]: the captured Disabled state was never restored, so a
//     rollback left an ENABLED entry pointing at the port it had just retired.
//   - finding 8 [P2]: an absent-pre-state entry was classified hub-owned on
//     its reserved NAME alone, so a rollback deleted an operator-created entry
//     that merely happened to use that name.
//
// These are api-level tests on purpose: each finding is a property of the
// restore ROUTINE, and pinning it here means the property survives regardless
// of which command drives it.
package api

import (
	"errors"
	"testing"

	"mcp-local-hub/internal/clients"
)

// snapshotFakeClient models the ACTUAL adapter contract the restore path
// writes against, which is what makes the finding-6 tests meaningful.
//
// The critical fidelity detail is AddEntry: no shipped adapter has a
// "disabled" knob, so an entry written through AddEntry lands ENABLED unless
// the adapter writes a verbatim Raw value (MiMoCode/OpenCode). Modelling that
// faithfully is the difference between a test that proves the disabled bit is
// restored and one that proves only that the fake remembers what it was told.
type snapshotFakeClient struct {
	name              string
	exists            bool
	entries           map[string]clients.MCPEntry
	addCalls          int
	removeCalls       int
	addErrs           map[string]error
	removeErrs        map[string]error
	postGroupMutation func()
}

func newSnapshotFakeClient(name string, exists bool) *snapshotFakeClient {
	return &snapshotFakeClient{
		name: name, exists: exists, entries: map[string]clients.MCPEntry{},
		addErrs: map[string]error{}, removeErrs: map[string]error{},
	}
}

func (f *snapshotFakeClient) put(entry clients.MCPEntry) { f.entries[entry.Name] = entry }

func (f *snapshotFakeClient) Name() string       { return f.name }
func (f *snapshotFakeClient) ConfigPath() string { return f.name + ".json" }
func (f *snapshotFakeClient) Exists() bool       { return f.exists }
func (f *snapshotFakeClient) IsRelayStdio() bool { return clients.IsRelayStdio(f.name) }
func (f *snapshotFakeClient) InitEmpty() (bool, error) {
	return false, nil
}
func (f *snapshotFakeClient) Backup() (string, error)        { return f.BackupKeep(0) }
func (f *snapshotFakeClient) BackupKeep(int) (string, error) { return f.name + ".bak", nil }
func (f *snapshotFakeClient) Restore(string) error           { return nil }
func (f *snapshotFakeClient) LatestBackupPath() (string, bool, error) {
	return f.name + ".bak", true, nil
}

// AddEntry mirrors the real adapters: Raw (when present) is written verbatim
// and therefore carries the disabled bit; without Raw the entry is written
// ENABLED because the lean MCPEntry write path has no way to express anything
// else.
func (f *snapshotFakeClient) AddEntry(e clients.MCPEntry) error {
	f.addCalls++
	if err := f.addErrs[e.Name]; err != nil {
		return err
	}
	stored := e
	stored.Disabled = false
	if e.Raw != nil {
		if disabled, ok := e.Raw["disabled"].(bool); ok {
			stored.Disabled = disabled
		}
		if enabled, ok := e.Raw["enabled"].(bool); ok && !enabled {
			stored.Disabled = true
		}
		if url, ok := e.Raw["url"].(string); ok {
			stored.URL = url
		}
	}
	f.entries[e.Name] = stored
	return nil
}

func (f *snapshotFakeClient) RemoveEntry(name string) error {
	f.removeCalls++
	if err := f.removeErrs[name]; err != nil {
		return err
	}
	delete(f.entries, name)
	return nil
}

func (f *snapshotFakeClient) GetEntry(name string) (*clients.MCPEntry, error) {
	entry, ok := f.entries[name]
	if !ok {
		return nil, nil
	}
	cp := entry
	return &cp, nil
}

func (f *snapshotFakeClient) ConditionalEntryMutation(req clients.ConditionalEntryMutationRequest) (out clients.EntryMutationObserved) {
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

func (f *snapshotFakeClient) ConditionalEntryGroupMutation(req clients.ConditionalEntryGroupMutationRequest) (out clients.ConditionalEntryGroupMutationObserved) {
	before, err := f.GetEntry(req.EntryName)
	out.Before = before
	if err != nil {
		out.ObservationErr = err
		return out
	}
	if req.ExpectedLive == nil || !req.ExpectedLive(before) {
		out.PreconditionConflict = true
		out.PreparationErr = clients.ErrEntryMutationPreconditionConflict
		out.ConflictScope = "target"
		out.ConflictEntryName = req.EntryName
		return out
	}
	out.Dependencies = make([]clients.EntryMutationDependencyObserved, len(req.Dependencies))
	for i, dependency := range req.Dependencies {
		live, readErr := f.GetEntry(dependency.EntryName)
		out.Dependencies[i] = clients.EntryMutationDependencyObserved{EntryName: dependency.EntryName, Before: live, ObservationErr: readErr}
		if readErr != nil {
			out.ObservationErr = readErr
			return out
		}
		if dependency.ExpectedLive == nil || !dependency.ExpectedLive(live) {
			out.PreconditionConflict = true
			out.PreparationErr = clients.ErrEntryMutationPreconditionConflict
			out.ConflictScope = "dependency"
			out.ConflictEntryName = dependency.EntryName
			return out
		}
	}
	if req.BackupKeepN != nil {
		out.BackupPath, out.PreparationErr = f.BackupKeep(*req.BackupKeepN)
		if out.PreparationErr != nil {
			return out
		}
	}
	if req.BeforeMutation != nil {
		out.PreparationErr = req.BeforeMutation(clients.EntryMutationPreparation{Before: before, BackupPath: out.BackupPath})
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
	if out.MutationErr == nil && f.postGroupMutation != nil {
		f.postGroupMutation()
	}
	out.After, out.ObservationErr = f.GetEntry(req.EntryName)
	for i, dependency := range req.Dependencies {
		live, readErr := f.GetEntry(dependency.EntryName)
		out.Dependencies[i].After = live
		out.Dependencies[i].ObservationErr = readErr
		if readErr != nil {
			if out.ObservationErr == nil {
				out.ObservationErr = readErr
			}
			if out.DependencyFailure == nil {
				out.DependencyFailure = &clients.EntryMutationDependencyFailure{
					Phase: clients.EntryMutationDependencyFailureAfterRead,
					Kind:  clients.EntryMutationDependencyFailureObservation, EntryName: dependency.EntryName, Cause: readErr,
				}
			}
			continue
		}
		matches := dependency.ExpectedLive(live)
		out.Dependencies[i].AfterMatchesExpected = &matches
		if !matches && out.DependencyFailure == nil {
			out.DependencyFailure = &clients.EntryMutationDependencyFailure{
				Phase: clients.EntryMutationDependencyFailureAfterPredicateMismatch,
				Kind:  clients.EntryMutationDependencyFailurePredicateMismatch, EntryName: dependency.EntryName,
			}
		}
	}
	return out
}

func (f *snapshotFakeClient) RestoreEntryFromBackup(string, string) error            { return nil }
func (f *snapshotFakeClient) RestoreEntryFromBackupForRollback(string, string) error { return nil }
func (f *snapshotFakeClient) BackupContainsEntry(string, string) (bool, error)       { return false, nil }
func (f *snapshotFakeClient) BackupEntryIsHubManaged(string, string) (bool, error)   { return false, nil }
func (f *snapshotFakeClient) AllStdioEntries() ([]clients.StdioEntry, error)         { return nil, nil }
func (f *snapshotFakeClient) FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error) {
	return nil, nil
}

var _ clients.Client = (*snapshotFakeClient)(nil)

// --- finding 4 -------------------------------------------------------------

// TestSnapshotRestore_UnreachableClientIsPendingNotSilentlyRestored is the
// finding-4 guard.
//
// The pre-fix restore iterated the LIVE ADAPTER MAP and skipped any client it
// did not find there. A recorded client whose adapter is missing on this host,
// or whose config file has temporarily gone, therefore produced no row of any
// kind — and the caller, seeing zero failures, deleted the recovery record.
// That client's rollback row was then gone permanently, while the client
// itself was still pointing at the front port.
//
// The assertion is that the row is reported as Pending: neither restored
// (it wasn't) nor failed (nothing was attempted), so the caller can refuse to
// consume the record.
func TestSnapshotRestore_UnreachableClientIsPendingNotSilentlyRestored(t *testing.T) {
	a := NewAPI()

	present := newSnapshotFakeClient("claude-code", true)
	present.put(clients.MCPEntry{Name: LSPRouterEntryName("go"), URL: LSPRouterURL(9137, "go")})
	// "cursor" is in the SNAPSHOT but absent from the live adapter map, and
	// "windsurf" is present as an adapter whose config file no longer exists.
	vanished := newSnapshotFakeClient("windsurf", false)

	snapshot := []LSPRouterEntrySnapshot{
		{Client: "claude-code", Language: "go", EntryName: LSPRouterEntryName("go"), Present: true, URL: LSPRouterURL(9125, "go")},
		{Client: "cursor", Language: "go", EntryName: LSPRouterEntryName("go"), Present: true, URL: LSPRouterURL(9125, "go")},
		{Client: "windsurf", Language: "go", EntryName: LSPRouterEntryName("go"), Present: true, URL: LSPRouterURL(9125, "go")},
	}

	report, err := a.RestoreLSPRouterClientEntriesSnapshot(snapshot, LSPClientRouterOpts{
		GUIPort:     9137,
		BackupKeepN: 3,
		Clients:     map[string]clients.Client{"claude-code": present, "windsurf": vanished},
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	if len(report.Pending) != 2 {
		t.Fatalf("both unreachable recorded clients must be reported Pending so the caller keeps the record; got %d pending row(s): %+v (a silent skip is what let the caller delete the record and lose these rows for good)", len(report.Pending), report.Pending)
	}
	pending := map[string]bool{}
	for _, row := range report.Pending {
		pending[row.Client] = true
	}
	if !pending["cursor"] {
		t.Fatalf("a recorded client with NO adapter on this host must be Pending; got %+v", report.Pending)
	}
	if !pending["windsurf"] {
		t.Fatalf("a recorded client whose config file no longer exists must be Pending; got %+v", report.Pending)
	}
	// The reachable client must still be restored — pending rows must not
	// block the work that CAN be done.
	if got := present.entries[LSPRouterEntryName("go")].URL; got != LSPRouterURL(9125, "go") {
		t.Fatalf("the reachable client must still be restored to its recorded pre-state: got %q, want %q", got, LSPRouterURL(9125, "go"))
	}
}

func TestSnapshotRestore_AppliedOrUncertainAbsentBaselineUnreachableIsPending(t *testing.T) {
	baseline := LSPRouterEntrySnapshot{
		Client: "cursor", Language: "go", EntryName: LSPRouterEntryName("go"),
	}
	applied := LSPRouterEntrySnapshot{
		Client: "cursor", Language: "go", EntryName: LSPRouterEntryName("go"),
		Present: true, URL: LSPRouterURL(9137, "go"),
	}
	tests := []struct {
		name        string
		row         LSPRouterRecoveryRow
		wantStatus  LSPRouterRestoreStatus
		wantPending int
	}{
		{
			name:        "applied-receipt",
			row:         LSPRouterRecoveryRow{Baseline: baseline, Applied: &applied},
			wantStatus:  LSPRouterRestorePending,
			wantPending: 1,
		},
		{
			name:        "uncertain-write",
			row:         LSPRouterRecoveryRow{Baseline: baseline, Uncertain: true},
			wantStatus:  LSPRouterRestorePending,
			wantPending: 1,
		},
		{
			name:        "baseline-only",
			row:         LSPRouterRecoveryRow{Baseline: baseline},
			wantStatus:  LSPRouterRestoreBaselineOnly,
			wantPending: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report, results, err := NewAPI().RestoreLSPRouterRecoveryRows(
				[]LSPRouterRecoveryRow{tc.row},
				LSPClientRouterOpts{Clients: map[string]clients.Client{}},
				LSPRouterRestoreCallbacks{},
			)
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			if len(results) != 1 || results[0].Status != tc.wantStatus {
				t.Fatalf("results=%+v, want one %s row", results, tc.wantStatus)
			}
			if len(report.Pending) != tc.wantPending {
				t.Fatalf("Pending=%+v, want %d row(s)", report.Pending, tc.wantPending)
			}
		})
	}
}

func TestMCPFrontV3_LegacyRestoreFailureKeepsCanonicalRoute(t *testing.T) {
	const legacyName = "mcp-language-server-go-abcd"
	canonicalName := LSPRouterEntryName("go")
	client := newSnapshotFakeClient("codex-cli", true)
	client.put(clients.MCPEntry{Name: canonicalName, URL: LSPRouterURL(9137, "go")})
	client.addErrs[legacyName] = errors.New("induced legacy restore failure")

	legacyBaseline := LSPRouterEntrySnapshot{
		Client: "codex-cli", Language: "go", EntryName: legacyName,
		Present: true, URL: "http://localhost:9200/mcp",
	}
	legacyApplied := LSPRouterEntrySnapshot{
		Client: "codex-cli", Language: "go", EntryName: legacyName,
	}
	canonicalBaseline := LSPRouterEntrySnapshot{
		Client: "codex-cli", Language: "go", EntryName: canonicalName,
	}
	canonicalApplied := LSPRouterEntrySnapshot{
		Client: "codex-cli", Language: "go", EntryName: canonicalName,
		Present: true, URL: LSPRouterURL(9137, "go"),
	}

	report, results, err := NewAPI().RestoreLSPRouterRecoveryRows(
		[]LSPRouterRecoveryRow{
			{Baseline: canonicalBaseline, Applied: &canonicalApplied},
			{Baseline: legacyBaseline, Applied: &legacyApplied},
		},
		LSPClientRouterOpts{Clients: map[string]clients.Client{"codex-cli": client}},
		LSPRouterRestoreCallbacks{},
	)
	if err == nil {
		t.Fatal("legacy inverse failure must be reported")
	}
	if len(results) != 2 ||
		results[0].EntryName != legacyName || results[0].Status != LSPRouterRestoreFailed ||
		results[1].EntryName != canonicalName || results[1].Status != LSPRouterRestorePending {
		t.Fatalf("results=%+v, want legacy failed then canonical pending", results)
	}
	if len(report.Pending) != 1 || report.Pending[0].EntryName != canonicalName {
		t.Fatalf("Pending=%+v, want canonical route-preservation row", report.Pending)
	}
	if got, ok := client.entries[canonicalName]; !ok || got.URL != LSPRouterURL(9137, "go") {
		t.Fatalf("canonical route was removed after legacy restore failed: %+v", client.entries)
	}
}

func TestMCPFrontV3_LSPDependencyBarrierSurvivesRetry(t *testing.T) {
	const clientName = "codex-cli"
	const language = "go"
	const legacyName = "mcp-language-server-go-abcd"
	canonicalName := LSPRouterEntryName(language)
	legacyBaseline := LSPRouterEntrySnapshot{
		Client: clientName, Language: language, EntryName: legacyName,
		Present: true, URL: "http://127.0.0.1:9200/mcp",
	}
	legacyApplied := LSPRouterEntrySnapshot{
		Client: clientName, Language: language, EntryName: legacyName,
	}
	canonicalBaseline := LSPRouterEntrySnapshot{
		Client: clientName, Language: language, EntryName: canonicalName,
	}
	canonicalApplied := LSPRouterEntrySnapshot{
		Client: clientName, Language: language, EntryName: canonicalName,
		Present: true, URL: LSPRouterURL(9137, language),
	}

	t.Run("terminal-conflict-blocks-canonical-on-every-call", func(t *testing.T) {
		client := newSnapshotFakeClient(clientName, true)
		client.put(clients.MCPEntry{Name: legacyName, URL: "https://operator.example/lsp/go"})
		client.put(clients.MCPEntry{Name: canonicalName, URL: canonicalApplied.URL})
		rows := []LSPRouterRecoveryRow{
			{Baseline: canonicalBaseline, Applied: &canonicalApplied},
			{Baseline: legacyBaseline, Applied: &legacyApplied},
		}

		_, first, err := NewAPI().RestoreLSPRouterRecoveryRows(
			rows,
			LSPClientRouterOpts{Clients: map[string]clients.Client{clientName: client}},
			LSPRouterRestoreCallbacks{},
		)
		if err != nil {
			t.Fatalf("first rollback: %v", err)
		}
		if len(first) != 2 ||
			first[0].EntryName != legacyName || first[0].Status != LSPRouterRestoreConflict ||
			first[1].EntryName != canonicalName || first[1].Status != LSPRouterRestoreConflict ||
			first[1].Reason != "skipped-dependency-conflict" {
			t.Fatalf("first results=%+v, want legacy conflict then canonical dependency conflict", first)
		}
		if client.removeCalls != 0 {
			t.Fatalf("first rollback canonical removeCalls=%d, want 0", client.removeCalls)
		}

		rows[0].Disposition = string(first[1].Status)
		rows[0].DispositionReason = first[1].Reason
		rows[1].Disposition = string(first[0].Status)
		rows[1].DispositionReason = first[0].Reason
		_, second, err := NewAPI().RestoreLSPRouterRecoveryRows(
			rows,
			LSPClientRouterOpts{Clients: map[string]clients.Client{clientName: client}},
			LSPRouterRestoreCallbacks{},
		)
		if err != nil {
			t.Fatalf("second rollback: %v", err)
		}
		if len(second) != 2 ||
			second[0].EntryName != legacyName || second[0].Status != LSPRouterRestoreConflict ||
			second[1].EntryName != canonicalName || second[1].Status != LSPRouterRestoreConflict ||
			second[1].Reason != "skipped-dependency-conflict" {
			t.Fatalf("second results=%+v, want the same durable dependency barrier", second)
		}
		if client.removeCalls != 0 {
			t.Fatalf("canonical removeCalls=%d across both calls, want 0", client.removeCalls)
		}
		if got := client.entries[canonicalName].URL; got != canonicalApplied.URL {
			t.Fatalf("canonical route changed across retry: %q", got)
		}
	})

	for _, tc := range []struct {
		name                string
		legacyDisposition   LSPRouterRestoreStatus
		baseline            LSPRouterEntrySnapshot
		legacy              *clients.MCPEntry
		reachable           bool
		wantLegacy          LSPRouterRestoreStatus
		wantCanonical       LSPRouterRestoreStatus
		wantCanonicalWrites int
	}{
		{
			name: "baseline-only-missing", reachable: true,
			wantLegacy: LSPRouterRestoreConflict, wantCanonical: LSPRouterRestoreConflict,
		},
		{
			name: "baseline-only-unreachable", reachable: false,
			wantLegacy: LSPRouterRestorePending, wantCanonical: LSPRouterRestorePending,
		},
		{
			name: "baseline-only-disabled", reachable: true,
			baseline: LSPRouterEntrySnapshot{
				Client: clientName, Language: language, EntryName: legacyName,
				Present: true, URL: legacyBaseline.URL, Disabled: true,
			},
			legacy: &clients.MCPEntry{
				Name: legacyName, URL: legacyBaseline.URL, Disabled: true,
			},
			wantLegacy: LSPRouterRestoreConflict, wantCanonical: LSPRouterRestoreConflict,
		},
		{
			name: "baseline-only-non-routable", reachable: true,
			baseline: LSPRouterEntrySnapshot{
				Client: clientName, Language: language, EntryName: legacyName,
				Present: true,
			},
			legacy: &clients.MCPEntry{
				Name: legacyName,
			},
			wantLegacy: LSPRouterRestoreConflict, wantCanonical: LSPRouterRestoreConflict,
		},
		{
			name: "baseline-only-live-ready", reachable: true,
			legacy: &clients.MCPEntry{
				Name: legacyName, URL: legacyBaseline.URL,
			},
			wantLegacy: LSPRouterRestoreBaselineOnly, wantCanonical: LSPRouterRestoreRestored,
			wantCanonicalWrites: 1,
		},
		{
			name: "restored-live-ready", reachable: true,
			legacyDisposition: LSPRouterRestoreRestored,
			legacy: &clients.MCPEntry{
				Name: legacyName, URL: legacyBaseline.URL,
			},
			wantLegacy: LSPRouterRestoreRestored, wantCanonical: LSPRouterRestoreRestored,
			wantCanonicalWrites: 1,
		},
		{
			name: "restored-but-now-missing", reachable: true,
			legacyDisposition: LSPRouterRestoreRestored,
			wantLegacy:        LSPRouterRestoreConflict, wantCanonical: LSPRouterRestoreConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newSnapshotFakeClient(clientName, tc.reachable)
			if tc.legacy != nil {
				client.put(*tc.legacy)
			}
			client.put(clients.MCPEntry{Name: canonicalName, URL: canonicalApplied.URL})
			baseline := tc.baseline
			if baseline.EntryName == "" {
				baseline = legacyBaseline
			}
			legacyRow := LSPRouterRecoveryRow{Baseline: baseline}
			if tc.legacyDisposition != "" {
				legacyRow.Applied = &legacyApplied
				legacyRow.Disposition = string(tc.legacyDisposition)
				legacyRow.DispositionReason = "inverse-verified"
			}
			_, results, err := NewAPI().RestoreLSPRouterRecoveryRows(
				[]LSPRouterRecoveryRow{
					{Baseline: canonicalBaseline, Applied: &canonicalApplied},
					legacyRow,
				},
				LSPClientRouterOpts{Clients: map[string]clients.Client{clientName: client}},
				LSPRouterRestoreCallbacks{},
			)
			if err != nil {
				t.Fatalf("rollback: %v", err)
			}
			if len(results) != 2 ||
				results[0].EntryName != legacyName || results[0].Status != tc.wantLegacy ||
				results[1].EntryName != canonicalName || results[1].Status != tc.wantCanonical {
				t.Fatalf("results=%+v, want legacy=%s canonical=%s", results, tc.wantLegacy, tc.wantCanonical)
			}
			if client.removeCalls != tc.wantCanonicalWrites {
				t.Fatalf("canonical removeCalls=%d, want %d", client.removeCalls, tc.wantCanonicalWrites)
			}
			_, canonicalPresent := client.entries[canonicalName]
			if wantPresent := tc.wantCanonicalWrites == 0; canonicalPresent != wantPresent {
				t.Fatalf("canonical present=%v, want %v", canonicalPresent, wantPresent)
			}
		})
	}
}

func TestMCPFrontV3_RollbackCanonicalInverseRejectsInterveningLegacyDependencyEdit(t *testing.T) {
	const clientName = "codex-cli"
	const language = "go"
	const legacyName = "mcp-language-server-go-abcd"
	const legacyURL = "http://127.0.0.1:9200/mcp"
	const operatorURL = "https://operator.example/lsp/go"
	canonicalName := LSPRouterEntryName(language)
	canonicalApplied := LSPRouterEntrySnapshot{
		Client: clientName, Language: language, EntryName: canonicalName,
		Present: true, URL: LSPRouterURL(9137, language),
	}
	legacyBaseline := LSPRouterEntrySnapshot{
		Client: clientName, Language: language, EntryName: legacyName,
		Present: true, URL: legacyURL,
	}
	legacyApplied := LSPRouterEntrySnapshot{Client: clientName, Language: language, EntryName: legacyName}
	canonicalBaseline := LSPRouterEntrySnapshot{Client: clientName, Language: language, EntryName: canonicalName}

	base := newSnapshotFakeClient(clientName, true)
	base.put(clients.MCPEntry{Name: canonicalName, URL: canonicalApplied.URL})
	client := &raceSnapshotClient{
		snapshotFakeClient: base,
		beforeConditional: func(entryName string) {
			if entryName == canonicalName {
				base.entries[legacyName] = clients.MCPEntry{Name: legacyName, URL: operatorURL}
			}
		},
	}
	_, results, err := NewAPI().RestoreLSPRouterRecoveryRows(
		[]LSPRouterRecoveryRow{
			{Baseline: canonicalBaseline, Applied: &canonicalApplied},
			{Baseline: legacyBaseline, Applied: &legacyApplied},
		},
		LSPClientRouterOpts{Clients: map[string]clients.Client{clientName: client}},
		LSPRouterRestoreCallbacks{},
	)
	if err != nil {
		t.Fatalf("a row-local dependency conflict must remain a rollback result, not abort unrelated rows: %v", err)
	}
	if len(results) != 2 || results[0].EntryName != legacyName || results[0].Status != LSPRouterRestoreRestored ||
		results[1].EntryName != canonicalName || results[1].Status != LSPRouterRestoreConflict ||
		results[1].Reason != "skipped-dependency-conflict" {
		t.Fatalf("results=%+v, want restored legacy then canonical dependency conflict", results)
	}
	if base.removeCalls != 0 {
		t.Fatalf("canonical removeCalls=%d, want 0", base.removeCalls)
	}
	if canonical := base.entries[canonicalName]; canonical.URL != canonicalApplied.URL {
		t.Fatalf("dependency refusal removed or changed canonical route: %+v", canonical)
	}
	if legacy := base.entries[legacyName]; legacy.URL != operatorURL {
		t.Fatalf("operator legacy edit was overwritten: %+v", legacy)
	}
}

// TestMCPFrontV3_RollbackDependencyAuthorizationMatrix makes both required
// legacy aliases real rollback dependencies. The injected change occurs only
// after both legacy inverses, immediately before the canonical group
// authorization, so a canonical inverse must never erase the last live route.
func TestMCPFrontV3_RollbackDependencyAuthorizationMatrix(t *testing.T) {
	const clientName = "codex-cli"
	const language = "go"
	const legacyURL = "http://127.0.0.1:9200/mcp"
	canonicalName := LSPRouterEntryName(language)
	legacyNames := []string{
		"mcp-language-server-go-legacy-a",
		"mcp-language-server-go-legacy-b",
	}
	canonicalBaseline := LSPRouterEntrySnapshot{Client: clientName, Language: language, EntryName: canonicalName}
	canonicalApplied := LSPRouterEntrySnapshot{
		Client: clientName, Language: language, EntryName: canonicalName,
		Present: true, URL: LSPRouterURL(9137, language),
	}
	rows := func() []LSPRouterRecoveryRow {
		out := []LSPRouterRecoveryRow{{Baseline: canonicalBaseline, Applied: &canonicalApplied}}
		for _, name := range legacyNames {
			baseline := LSPRouterEntrySnapshot{
				Client: clientName, Language: language, EntryName: name,
				Present: true, URL: legacyURL,
			}
			applied := LSPRouterEntrySnapshot{Client: clientName, Language: language, EntryName: name}
			out = append(out, LSPRouterRecoveryRow{Baseline: baseline, Applied: &applied})
		}
		return out
	}

	for _, tc := range []struct {
		name       string
		aliasIndex int
		mutate     func(*snapshotFakeClient, string)
	}{
		{
			name:       "delete-first-alias",
			aliasIndex: 0,
			mutate: func(base *snapshotFakeClient, name string) {
				delete(base.entries, name)
			},
		},
		{
			name:       "replace-first-alias",
			aliasIndex: 0,
			mutate: func(base *snapshotFakeClient, name string) {
				base.entries[name] = clients.MCPEntry{Name: name, URL: "https://operator.example/lsp/go"}
			},
		},
		{
			name:       "disable-first-alias",
			aliasIndex: 0,
			mutate: func(base *snapshotFakeClient, name string) {
				base.entries[name] = clients.MCPEntry{Name: name, URL: legacyURL, Disabled: true}
			},
		},
		{
			name:       "delete-non-first-alias",
			aliasIndex: 1,
			mutate: func(base *snapshotFakeClient, name string) {
				delete(base.entries, name)
			},
		},
		{
			name:       "replace-non-first-alias",
			aliasIndex: 1,
			mutate: func(base *snapshotFakeClient, name string) {
				base.entries[name] = clients.MCPEntry{Name: name, URL: "https://operator.example/lsp/go"}
			},
		},
		{
			name:       "disable-non-first-alias",
			aliasIndex: 1,
			mutate: func(base *snapshotFakeClient, name string) {
				base.entries[name] = clients.MCPEntry{Name: name, URL: legacyURL, Disabled: true}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newSnapshotFakeClient(clientName, true)
			base.put(clients.MCPEntry{Name: canonicalName, URL: canonicalApplied.URL})
			targetAlias := legacyNames[tc.aliasIndex]
			adapter := &raceSnapshotClient{
				snapshotFakeClient: base,
				beforeConditional: func(entryName string) {
					if entryName == canonicalName {
						tc.mutate(base, targetAlias)
					}
				},
			}

			report, results, err := NewAPI().RestoreLSPRouterRecoveryRows(
				rows(),
				LSPClientRouterOpts{Clients: map[string]clients.Client{clientName: adapter}},
				LSPRouterRestoreCallbacks{},
			)
			if err != nil {
				t.Fatalf("row-local dependency conflict must not abort rollback: %v", err)
			}
			byEntry := map[string]LSPRouterRestoreRowResult{}
			for _, result := range results {
				byEntry[result.EntryName] = result
			}
			canonical, found := byEntry[canonicalName]
			if len(results) != 3 || !found || canonical.Status != LSPRouterRestoreConflict ||
				canonical.Reason != "skipped-dependency-conflict" || canonical.DependencyEntryName != targetAlias {
				t.Fatalf("results=%+v, want canonical dependency conflict for %q", results, targetAlias)
			}
			if len(report.Pending) != 0 || base.removeCalls != 0 {
				t.Fatalf("report=%+v removeCalls=%d, want no canonical inverse", report, base.removeCalls)
			}
			if canonicalLive, exists := base.entries[canonicalName]; !exists || canonicalLive.URL != canonicalApplied.URL {
				t.Fatalf("canonical inverse changed the protected route: %+v", base.entries)
			}
			liveAliases := 0
			for _, name := range legacyNames {
				entry, exists := base.entries[name]
				if exists && activeEntryPointsAtLegacyLSPPort(&entry, map[int]bool{9200: true}) {
					liveAliases++
				}
			}
			if liveAliases == 0 {
				t.Fatalf("dependency refusal erased every live legacy route: %+v", base.entries)
			}
		})
	}
}

func TestMCPFrontV3_RollbackPostInvocationDependencyChangeStaysPending(t *testing.T) {
	const clientName = "codex-cli"
	const language = "go"
	const legacyName = "mcp-language-server-go-abcd"
	const legacyURL = "http://127.0.0.1:9200/mcp"
	const operatorURL = "https://operator.example/lsp/go"
	canonicalName := LSPRouterEntryName(language)
	canonicalApplied := LSPRouterEntrySnapshot{
		Client: clientName, Language: language, EntryName: canonicalName,
		Present: true, URL: LSPRouterURL(9137, language),
	}
	canonicalBaseline := LSPRouterEntrySnapshot{Client: clientName, Language: language, EntryName: canonicalName}
	legacyBaseline := LSPRouterEntrySnapshot{
		Client: clientName, Language: language, EntryName: legacyName, Present: true, URL: legacyURL,
	}
	legacyApplied := LSPRouterEntrySnapshot{Client: clientName, Language: language, EntryName: legacyName}
	client := newSnapshotFakeClient(clientName, true)
	client.put(clients.MCPEntry{Name: canonicalName, URL: canonicalApplied.URL})
	client.postGroupMutation = func() {
		client.entries[legacyName] = clients.MCPEntry{Name: legacyName, URL: operatorURL}
	}

	report, results, err := NewAPI().RestoreLSPRouterRecoveryRows(
		[]LSPRouterRecoveryRow{
			{Baseline: canonicalBaseline, Applied: &canonicalApplied},
			{Baseline: legacyBaseline, Applied: &legacyApplied},
		},
		LSPClientRouterOpts{Clients: map[string]clients.Client{clientName: client}},
		LSPRouterRestoreCallbacks{},
	)
	if err != nil {
		t.Fatalf("a row-local post-invocation dependency change must remain pending without aborting recovery: %v", err)
	}
	if len(results) != 2 || results[0].EntryName != legacyName || results[0].Status != LSPRouterRestoreRestored ||
		results[1].EntryName != canonicalName || results[1].Status != LSPRouterRestorePending ||
		results[1].Reason != "rollback-ownership-unknown" || results[1].DependencyEntryName != legacyName {
		t.Fatalf("results=%+v, want restored legacy then dependency-identified canonical pending", results)
	}
	if len(report.Pending) != 1 || report.Pending[0].EntryName != canonicalName || client.removeCalls != 1 {
		t.Fatalf("report=%+v removeCalls=%d, want one invoked-but-pending canonical inverse", report, client.removeCalls)
	}
	if legacy := client.entries[legacyName]; legacy.URL != operatorURL {
		t.Fatalf("post-invocation dependency state was not retained: %+v", legacy)
	}
}

// --- finding 6 -------------------------------------------------------------

// TestSnapshotRestore_RestoresCapturedDisabledStateFromRaw is the finding-6
// positive.
//
// The forward pass rewrites a DISABLED canonical entry as ENABLED
// (entryMatchesLSPRouter treats a disabled entry as non-matching, and the
// hub-owned guard it consults does not exclude disabled entries). Restoring
// only the endpoint therefore hands the operator an ACTIVE entry pointing at
// the port the rollback just retired — a broken MCP server in their client,
// where before there was an inert one.
//
// The complete captured entry is replayed through Raw, which the adapters that
// can express "disabled" write verbatim.
func TestSnapshotRestore_RestoresCapturedDisabledStateFromRaw(t *testing.T) {
	a := NewAPI()
	entryName := LSPRouterEntryName("go")
	priorURL := LSPRouterURL(9125, "go")

	client := newSnapshotFakeClient("opencode", true)
	// Post-forward live state: rewritten to the front port AND enabled.
	client.put(clients.MCPEntry{Name: entryName, URL: LSPRouterURL(9137, "go")})

	report, err := a.RestoreLSPRouterClientEntriesSnapshot([]LSPRouterEntrySnapshot{{
		Client: "opencode", Language: "go", EntryName: entryName,
		Present: true, URL: priorURL, Disabled: true,
		Raw: map[string]any{"url": priorURL, "enabled": false},
	}}, LSPClientRouterOpts{
		GUIPort:     9137,
		BackupKeepN: 3,
		Clients:     map[string]clients.Client{"opencode": client},
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("a disabled pre-state WITH a verbatim raw value is fully restorable; got failures %+v", report.Failed)
	}
	got := client.entries[entryName]
	if got.URL != priorURL {
		t.Fatalf("restore must put the pre-reconcile URL back: got %q, want %q", got.URL, priorURL)
	}
	if !got.Disabled {
		t.Fatalf("restore must put the captured DISABLED state back; the entry is ENABLED and now points at %q, a port this rollback just retired — the operator gets a failing MCP server where they previously had an inert entry", got.URL)
	}
}

// TestSnapshotRestore_RefusesToRestoreDisabledEntryItCannotDisable is the
// finding-6 fail-loud half.
//
// Most adapters report "disabled" on read but have no way to write it back
// (their AddEntry has no disabled knob and they produce no verbatim Raw). For
// those, the only honest outcomes are "restore it correctly" or "say you
// could not" — never "restore the URL and quietly drop the bit", which
// re-enables an entry against a retired port.
func TestSnapshotRestore_RefusesToRestoreDisabledEntryItCannotDisable(t *testing.T) {
	a := NewAPI()
	entryName := LSPRouterEntryName("go")
	priorURL := LSPRouterURL(9125, "go")

	client := newSnapshotFakeClient("amazon-q", true)
	client.put(clients.MCPEntry{Name: entryName, URL: LSPRouterURL(9137, "go")})

	report, err := a.RestoreLSPRouterClientEntriesSnapshot([]LSPRouterEntrySnapshot{{
		Client: "amazon-q", Language: "go", EntryName: entryName,
		Present: true, URL: priorURL, Disabled: true, // no Raw: unrepresentable
	}}, LSPClientRouterOpts{
		GUIPort:     9137,
		BackupKeepN: 3,
		Clients:     map[string]clients.Client{"amazon-q": client},
	})
	if err == nil {
		t.Fatalf("a restore that cannot reproduce the captured disabled state must return an error so the caller keeps the record; got nil")
	}
	if len(report.Failed) != 1 || report.Failed[0].Op != "disabled-state" {
		t.Fatalf("the unrestorable row must be reported as a disabled-state failure, not silently applied; got %+v", report.Failed)
	}
	if client.addCalls != 0 {
		t.Fatalf("nothing must be written for a row whose disabled state cannot be reproduced; AddEntry was called %d time(s), which would have left an ENABLED entry on a retired port", client.addCalls)
	}
}

// TestSnapshotRestore_DisabledRowAlreadyInPreStateIsANoop pins the ordering
// that keeps the fail-loud rule from misfiring: a client the forward pass
// never actually mutated (its rewrite failed the ownership guard, say) is
// already in its recorded pre-state, so there is nothing to restore and
// nothing to fail.
func TestSnapshotRestore_DisabledRowAlreadyInPreStateIsANoop(t *testing.T) {
	a := NewAPI()
	entryName := LSPRouterEntryName("go")
	priorURL := LSPRouterURL(9125, "go")

	client := newSnapshotFakeClient("amazon-q", true)
	client.put(clients.MCPEntry{Name: entryName, URL: priorURL, Disabled: true})

	report, err := a.RestoreLSPRouterClientEntriesSnapshot([]LSPRouterEntrySnapshot{{
		Client: "amazon-q", Language: "go", EntryName: entryName,
		Present: true, URL: priorURL, Disabled: true,
	}}, LSPClientRouterOpts{
		GUIPort:     9137,
		BackupKeepN: 3,
		Clients:     map[string]clients.Client{"amazon-q": client},
	})
	if err != nil {
		t.Fatalf("an entry already in its recorded pre-state needs no write and must not fail: %v", err)
	}
	if len(report.Failed) != 0 || client.addCalls != 0 {
		t.Fatalf("expected a pure no-op; failures=%+v addCalls=%d", report.Failed, client.addCalls)
	}
}

// --- finding 8 -------------------------------------------------------------

// TestSnapshotRestore_AbsentPreStateDoesNotDeleteAnOperatorEntryOnTheSameName
// is the finding-8 guard.
//
// For a row recorded ABSENT, the pre-fix removal branch asked only "is this
// entry hub-owned?", and entryIsOwnedLSPRouterForLanguage answers yes for ANY
// LSP-router-shaped loopback URL carried under the reserved entry NAME,
// whatever its port. The reserved name governs where the hub WRITES; it does
// not make the hub the owner of whatever an operator later puts there. So an
// operator who pointed that entry at their own language server after the
// cutover had it deleted by a rollback — and the record was consumed, so there
// was nothing left to explain what happened.
//
// Removal is now conditioned on the entry still being EXACTLY what this
// forward generation wrote.
func TestSnapshotRestore_AbsentPreStateDoesNotDeleteAnOperatorEntryOnTheSameName(t *testing.T) {
	a := NewAPI()
	entryName := LSPRouterEntryName("go")
	operatorURL := LSPRouterURL(9999, "go") // same shape, DIFFERENT port

	client := newSnapshotFakeClient("claude-code", true)
	client.put(clients.MCPEntry{Name: entryName, URL: operatorURL})

	report, err := a.RestoreLSPRouterClientEntriesSnapshot([]LSPRouterEntrySnapshot{{
		Client: "claude-code", Language: "go", EntryName: entryName, Present: false,
	}}, LSPClientRouterOpts{
		GUIPort:     9137,
		BackupKeepN: 3,
		Clients:     map[string]clients.Client{"claude-code": client},
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if client.removeCalls != 0 {
		t.Fatalf("rollback DELETED an operator-created entry: the live entry pointed at %q, which is not the %q this forward generation wrote, so it was never ours to remove", operatorURL, LSPRouterURL(9137, "go"))
	}
	if _, still := client.entries[entryName]; !still {
		t.Fatalf("the operator's entry %s must survive rollback", entryName)
	}
	if len(report.Skipped) != 1 {
		t.Fatalf("the untouched entry must be reported Skipped so the operator can see it was left alone; got %+v", report.Skipped)
	}
}

// TestSnapshotRestore_AbsentPreStateStillRemovesTheEntryThisRunCreated is the
// finding-8 positive control: narrowing the removal rule must not stop the
// rollback from undoing its OWN creation. Without this, "fix finding 8" could
// be satisfied by never removing anything.
func TestSnapshotRestore_AbsentPreStateStillRemovesTheEntryThisRunCreated(t *testing.T) {
	a := NewAPI()
	entryName := LSPRouterEntryName("go")

	client := newSnapshotFakeClient("claude-code", true)
	client.put(clients.MCPEntry{Name: entryName, URL: LSPRouterURL(9137, "go")})

	if _, err := a.RestoreLSPRouterClientEntriesSnapshot([]LSPRouterEntrySnapshot{{
		Client: "claude-code", Language: "go", EntryName: entryName, Present: false,
	}}, LSPClientRouterOpts{
		GUIPort:     9137,
		BackupKeepN: 3,
		Clients:     map[string]clients.Client{"claude-code": client},
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, still := client.entries[entryName]; still {
		t.Fatalf("the inverse of an absent pre-state is absence: the entry this forward generation created must be removed again")
	}
}

// TestSnapshotCapturesRawForACompleteRestore pins the capture half of finding
// 6: the restore can only replay a verbatim entry if the snapshot recorded
// one.
func TestSnapshotCapturesRawForACompleteRestore(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})
	a := NewAPI()

	entryName := LSPRouterEntryName("go")
	raw := map[string]any{"url": LSPRouterURL(9125, "go"), "enabled": false}
	client := newSnapshotFakeClient("opencode", true)
	client.put(clients.MCPEntry{Name: entryName, URL: LSPRouterURL(9125, "go"), Disabled: true, Raw: raw})

	rows, err := a.SnapshotLSPRouterClientEntries(LSPClientRouterOpts{
		Clients: map[string]clients.Client{"opencode": client},
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var found *LSPRouterEntrySnapshot
	for i := range rows {
		if rows[i].Client == "opencode" && rows[i].Language == "go" {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatalf("snapshot must cover the present client's go entry; got %+v", rows)
	}
	if !found.Disabled {
		t.Fatalf("the captured row must record the disabled pre-state; got %+v", *found)
	}
	if len(found.Raw) == 0 {
		t.Fatalf("the captured row must record the adapter's verbatim entry value — without it the rollback has no way to put a disabled entry back, and a URL-only restore re-enables it against a retired port; got %+v", *found)
	}
}

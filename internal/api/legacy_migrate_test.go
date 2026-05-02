package api

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Memo D1 exemption regression guard: legacy_migrate.go preserves
// hardcoded WeeklyRefresh:true with WeeklyRefreshExplicit:true so it
// bypasses the knob. Source-level assertions catch any future flip.
func TestLegacyMigrate_ExemptionFromKnob(t *testing.T) {
	src, err := os.ReadFile("legacy_migrate.go")
	if err != nil {
		t.Fatalf("read legacy_migrate.go: %v", err)
	}
	body := string(src)

	// Must still hardcode WeeklyRefresh:true with explicit override.
	// Robust to both single-line ("WeeklyRefreshExplicit: true") and multi-line
	// gofmt-aligned ("WeeklyRefreshExplicit:         true") forms.
	if !regexp.MustCompile(`WeeklyRefreshExplicit:\s+true`).MatchString(body) {
		t.Error("legacy_migrate.go missing WeeklyRefreshExplicit:true (memo D1 exemption)")
	}
	// Robust to both single-line ("WeeklyRefresh: true") and multi-line
	// gofmt-aligned ("WeeklyRefresh:         true") forms.
	if !regexp.MustCompile(`WeeklyRefresh:\s+true`).MatchString(body) {
		t.Error("legacy_migrate.go no longer hardcodes WeeklyRefresh:true (memo D1 exemption violated)")
	}

	// Must contain the rationale comment so future readers see why.
	if !strings.Contains(body, "Legacy import preserves the pre-A4-b register-time default") {
		t.Error("legacy_migrate.go missing memo-D1 rationale comment")
	}
}

// TestMigrateLegacy_DryRun confirms dry-run prints a plan but makes no
// registry or client changes.
func TestMigrateLegacy_DryRun(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	// Seed an existing legacy entry so RemoveEntry would have something to
	// delete — if dry-run were honoring the flag, it would remain afterwards.
	h.fakeClients.entries["codex-cli"]["python-lsp"] = "legacy"
	entries := []LegacyLSEntry{
		{Client: "codex-cli", EntryName: "python-lsp", Workspace: t.TempDir(), Language: "python", LspCommand: "pyright-langserver"},
	}
	buf := &bytes.Buffer{}
	a := NewAPI()
	report, err := a.MigrateLegacy(entries, LegacyMigrateOpts{DryRun: true, Writer: buf})
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if len(report.Planned) != 1 {
		t.Errorf("expected 1 planned, got %d", len(report.Planned))
	}
	if len(report.Applied) != 0 {
		t.Errorf("dry-run should not apply: got %+v", report.Applied)
	}
	// Legacy entry still in the client config.
	if _, ok := h.fakeClients.entries["codex-cli"]["python-lsp"]; !ok {
		t.Error("dry-run removed the legacy entry — must be a no-op")
	}
	// No registry file written.
	if len(h.fakeSch.createdSpecs) != 0 {
		t.Errorf("dry-run created scheduler tasks: %+v", h.fakeSch.createdSpecs)
	}
	// Plan text mentions the workspace + entry.
	if !strings.Contains(buf.String(), "python-lsp") {
		t.Errorf("dry-run output should name the legacy entry: %q", buf.String())
	}
}

// TestMigrateLegacy_DedupByWorkspace seeds 3 legacy rows for workspace W1
// (python, rust, typescript) + 1 row for workspace W2 (clangd). The
// migration must emit exactly 2 Register calls — one per unique workspace
// — not 4.
func TestMigrateLegacy_DedupByWorkspace(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	w1 := t.TempDir()
	w2 := t.TempDir()
	// Pre-seed the legacy rows in the fake client map so RemoveEntry has
	// something to delete after Register succeeds.
	h.fakeClients.entries["codex-cli"]["py-lsp"] = "legacy"
	h.fakeClients.entries["codex-cli"]["rust-lsp"] = "legacy"
	h.fakeClients.entries["codex-cli"]["ts-lsp"] = "legacy"
	h.fakeClients.entries["codex-cli"]["clangd-lsp"] = "legacy"
	entries := []LegacyLSEntry{
		{Client: "codex-cli", EntryName: "py-lsp", Workspace: w1, Language: "python", LspCommand: "pyright-langserver"},
		{Client: "codex-cli", EntryName: "rust-lsp", Workspace: w1, Language: "rust", LspCommand: "rust-analyzer"},
		{Client: "codex-cli", EntryName: "ts-lsp", Workspace: w1, Language: "typescript", LspCommand: "typescript-language-server"},
		{Client: "codex-cli", EntryName: "clangd-lsp", Workspace: w2, Language: "clangd", LspCommand: "clangd"},
	}
	buf := &bytes.Buffer{}
	a := NewAPI()
	report, err := a.MigrateLegacy(entries, LegacyMigrateOpts{Yes: true, Writer: buf})
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	// Each Register(ws, nil, ...) loads the full manifest and creates 9
	// scheduler tasks (one per language). 2 workspaces = 18 tasks total.
	// We assert the per-workspace count rather than the raw task count to
	// keep the test independent of manifest size.
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	workspaceKeySet := map[string]bool{}
	for _, e := range reg.Workspaces {
		workspaceKeySet[e.WorkspaceKey] = true
	}
	if len(workspaceKeySet) != 2 {
		t.Fatalf("expected 2 unique workspace_keys in registry, got %d: %+v",
			len(workspaceKeySet), workspaceKeySet)
	}
	// All 4 legacy rows must be Applied (Register succeeded for both
	// workspaces → all 4 deletes succeed).
	if len(report.Applied) != 4 {
		t.Errorf("expected 4 applied, got %d: %+v", len(report.Applied), report.Applied)
	}
	// Every legacy entry removed from the fake client.
	for _, name := range []string{"py-lsp", "rust-lsp", "ts-lsp", "clangd-lsp"} {
		if _, still := h.fakeClients.entries["codex-cli"][name]; still {
			t.Errorf("legacy entry %q not removed after successful Register", name)
		}
	}
}

// TestMigrateLegacy_InteractivePrompt confirms the interactive path reads
// a line per unique workspace and skips on "n".
func TestMigrateLegacy_InteractivePrompt(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	w1 := t.TempDir()
	w2 := t.TempDir()
	h.fakeClients.entries["codex-cli"]["w1-lsp"] = "legacy"
	h.fakeClients.entries["codex-cli"]["w2-lsp"] = "legacy"
	entries := []LegacyLSEntry{
		{Client: "codex-cli", EntryName: "w1-lsp", Workspace: w1, Language: "python", LspCommand: "pyright-langserver"},
		{Client: "codex-cli", EntryName: "w2-lsp", Workspace: w2, Language: "python", LspCommand: "pyright-langserver"},
	}
	// Answer: y for first workspace (sorted), n for second.
	// workspaceOrder is sorted alphabetically; lock in the expected order
	// by inspecting which temp dir sorts first.
	first, second := w1, w2
	if w2 < w1 {
		first, second = w2, w1
	}
	_ = first
	_ = second
	// The prompts fire in sorted order. We answer y then n.
	in := strings.NewReader("y\nn\n")
	buf := &bytes.Buffer{}
	a := NewAPI()
	report, err := a.MigrateLegacy(entries, LegacyMigrateOpts{In: in, Writer: buf})
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if len(report.Applied) != 1 {
		t.Errorf("expected 1 applied (first workspace only), got %d: %+v", len(report.Applied), report.Applied)
	}
	if len(report.Skipped) != 1 {
		t.Errorf("expected 1 skipped (second workspace), got %d: %+v", len(report.Skipped), report.Skipped)
	}
}

// TestMigrateLegacy_YesFlag confirms --yes skips every prompt and applies
// every eligible entry.
func TestMigrateLegacy_YesFlag(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	h.fakeClients.entries["codex-cli"]["yes-lsp"] = "legacy"
	entries := []LegacyLSEntry{
		{Client: "codex-cli", EntryName: "yes-lsp", Workspace: ws, Language: "python", LspCommand: "pyright-langserver"},
	}
	buf := &bytes.Buffer{}
	a := NewAPI()
	report, err := a.MigrateLegacy(entries, LegacyMigrateOpts{Yes: true, Writer: buf})
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if len(report.Applied) != 1 {
		t.Fatalf("expected 1 applied, got %+v", report)
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	// Register(ws, nil, ...) registers all 9 manifest languages.
	if len(reg.Workspaces) != 9 {
		t.Errorf("expected 9 registry entries (all languages), got %d", len(reg.Workspaces))
	}
}

// TestMigrateLegacy_RemovesLegacyAfterSuccess proves the ordering
// contract: legacy rows are deleted AFTER Register completes. We use a
// scheduler-failure trap to prove the reverse too.
func TestMigrateLegacy_RemovesLegacyAfterSuccess(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	h.fakeClients.entries["codex-cli"]["ok-lsp"] = "legacy"
	entries := []LegacyLSEntry{
		{Client: "codex-cli", EntryName: "ok-lsp", Workspace: ws, Language: "python", LspCommand: "pyright-langserver"},
	}
	a := NewAPI()
	report, err := a.MigrateLegacy(entries, LegacyMigrateOpts{Yes: true, Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if len(report.Applied) != 1 {
		t.Fatalf("expected Applied=1, got %+v", report)
	}
	if _, still := h.fakeClients.entries["codex-cli"]["ok-lsp"]; still {
		t.Error("legacy entry still present after Applied — ordering contract violated")
	}
}

// TestMigrateLegacy_PreservesInPlaceReplacedEntry guards the narrow case
// where a legacy row already used the CANONICAL managed name (e.g.
// "mcp-language-server-python") — the exact name Register's ResolveEntryName
// would return for the new registration. In that case Register.AddEntry
// overwrites the legacy key in place with the new workspace-proxy URL,
// and a subsequent RemoveEntry call would wipe the freshly-migrated
// entry. Migration should detect the in-place replacement and skip
// RemoveEntry. The workspace ends up registered AND the client config
// has the correct new URL — which is the success criterion.
func TestMigrateLegacy_PreservesInPlaceReplacedEntry(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	// Pre-seed the legacy entry under the EXACT canonical name Register
	// will produce for this language. Register.AddEntry will overwrite
	// this value with the new URL; RemoveEntry MUST NOT follow.
	h.fakeClients.entries["codex-cli"]["mcp-language-server-python"] = "legacy-url"
	entries := []LegacyLSEntry{
		{Client: "codex-cli", EntryName: "mcp-language-server-python", Workspace: ws, Language: "python", LspCommand: "pyright-langserver"},
	}
	a := NewAPI()
	report, err := a.MigrateLegacy(entries, LegacyMigrateOpts{Yes: true, Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if len(report.Applied) != 1 {
		t.Fatalf("expected Applied=1, got %+v", report)
	}
	got, stillThere := h.fakeClients.entries["codex-cli"]["mcp-language-server-python"]
	if !stillThere {
		t.Fatal("freshly-migrated entry was deleted by post-register cleanup (regression)")
	}
	if got == "legacy-url" {
		t.Errorf("entry still holds legacy URL: Register.AddEntry did not overwrite")
	}
	// The URL should now be a workspace-proxy loopback URL.
	if !strings.HasPrefix(got, "http://localhost:") {
		t.Errorf("expected workspace-proxy URL, got %q", got)
	}
}

// TestMigrateLegacy_ClosedStdinDeclinesConfirmation guards against a
// silent auto-approval bug: the interactive prompt used to ignore
// ReadString errors and treat empty input as "yes" (default-Y). With
// closed stdin (CI without --yes, redirected input, broken pipe),
// that meant migrate-legacy would register workspaces and delete
// legacy entries without any user confirmation, contradicting the
// documented "interactive-by-default unless --yes" contract.
func TestMigrateLegacy_ClosedStdinDeclinesConfirmation(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	h.fakeClients.entries["codex-cli"]["legacy"] = "legacy-url"
	entries := []LegacyLSEntry{
		{Client: "codex-cli", EntryName: "legacy", Workspace: ws, Language: "python", LspCommand: "pyright-langserver"},
	}
	a := NewAPI()
	// Yes=false (interactive) + empty reader (immediate EOF) simulates
	// closed stdin. Migration MUST skip, not auto-approve.
	report, err := a.MigrateLegacy(entries, LegacyMigrateOpts{
		Yes:    false,
		In:     strings.NewReader(""),
		Writer: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if len(report.Applied) != 0 {
		t.Errorf("expected Applied=0 on closed stdin, got %+v", report.Applied)
	}
	if len(report.Skipped) != 1 {
		t.Errorf("expected Skipped=1 on closed stdin, got %+v", report.Skipped)
	}
	if _, ok := h.fakeClients.entries["codex-cli"]["legacy"]; !ok {
		t.Error("legacy entry removed despite closed stdin (silent auto-approve)")
	}
}

// TestMigrateLegacy_InPlaceCheckScopedToCurrentWorkspace guards the
// global-vs-local-scope fix for entryJustWrittenByRegister. If another
// workspace happened to have an entry with the same (client, name) as
// the legacy row being migrated, a global check would falsely treat
// that as an in-place replacement and skip RemoveEntry — leaving the
// current workspace's legacy entry dangling. The check must be scoped
// to the workspace being migrated.
func TestMigrateLegacy_InPlaceCheckScopedToCurrentWorkspace(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	// Workspace A already registered python with the CANONICAL name.
	wsA := t.TempDir()
	a := NewAPI()
	if _, err := a.registerWithManifest(nineLanguageManifest(), wsA, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("prep register wsA: %v", err)
	}
	// Workspace B has a LEGACY entry with the same canonical name.
	// Register for wsB will assign a suffixed name (collision), so the
	// legacy entry is NOT the name Register wrote for wsB. A global
	// check would still see "mcp-language-server-python" in wsA's
	// entries and wrongly skip the delete. Scoped check must detect
	// wsA is not the current workspace and proceed with RemoveEntry.
	wsB := t.TempDir()
	h.fakeClients.entries["codex-cli"]["mcp-language-server-python"] = "wsB-legacy-url"
	entries := []LegacyLSEntry{
		{Client: "codex-cli", EntryName: "mcp-language-server-python", Workspace: wsB, Language: "python", LspCommand: "pyright-langserver"},
	}
	report, err := a.MigrateLegacy(entries, LegacyMigrateOpts{Yes: true, Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if len(report.Applied) != 1 {
		t.Fatalf("expected Applied=1, got %+v", report)
	}
	// The legacy entry (with the canonical-but-belongs-to-wsA-too name)
	// must be GONE because wsB's Register used a suffixed name.
	if _, still := h.fakeClients.entries["codex-cli"]["mcp-language-server-python"]; still {
		t.Error("legacy entry survived because in-place check falsely matched another workspace (regression)")
	}
}

// TestMigrateLegacy_KeepsLegacyOnRegisterFailure proves the inverse: if
// Register fails, the legacy rows stay intact so the user can re-run.
// We induce a failure by making the first client AddEntry call fail,
// which triggers Register's rollback and returns an error.
func TestMigrateLegacy_KeepsLegacyOnRegisterFailure(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	h.fakeClients.entries["codex-cli"]["fail-lsp"] = "legacy"
	// Fail the very first client AddEntry — Register's rollback fires
	// and the call returns an error.
	h.fakeClients.failAddEntryCalls = 1
	entries := []LegacyLSEntry{
		{Client: "codex-cli", EntryName: "fail-lsp", Workspace: ws, Language: "python", LspCommand: "pyright-langserver"},
	}
	a := NewAPI()
	report, err := a.MigrateLegacy(entries, LegacyMigrateOpts{Yes: true, Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("MigrateLegacy returned top-level error: %v", err)
	}
	if len(report.Applied) != 0 {
		t.Errorf("expected 0 applied on Register failure, got %+v", report.Applied)
	}
	if len(report.Failed) == 0 {
		t.Errorf("expected failures recorded in report, got %+v", report)
	}
	// Legacy row must still be there so the user can re-run.
	if _, ok := h.fakeClients.entries["codex-cli"]["fail-lsp"]; !ok {
		t.Error("legacy entry removed despite Register failure — ordering contract violated")
	}
}

// TestMigrateLegacy_SkipsUnknownLanguage confirms entries with Language=""
// (unknown LSP binary) are skipped with a diagnostic and do NOT trigger a
// Register call. The workspace stays unregistered if ALL its entries were
// unknown.
func TestMigrateLegacy_SkipsUnknownLanguage(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	entries := []LegacyLSEntry{
		{Client: "codex-cli", EntryName: "weird-lsp", Workspace: ws, Language: "", LspCommand: "weird-language-server"},
	}
	a := NewAPI()
	report, err := a.MigrateLegacy(entries, LegacyMigrateOpts{Yes: true, Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if len(report.Skipped) != 1 {
		t.Errorf("expected 1 skipped, got %+v", report)
	}
	if len(report.Applied) != 0 {
		t.Errorf("expected 0 applied, got %+v", report)
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 0 {
		t.Errorf("expected 0 registry entries (all skipped), got %d", len(reg.Workspaces))
	}
}

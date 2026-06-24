package clients

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the bot PR #425 FOLLOW-UP refinement (architect GATE PASS): the
// CLASS of two codex-bot gaps plus two latent surfaces closed by CHANGES 1-3.
//
//   - GAP 1 (CHANGE 1): the merge-based re-resolve (readMergedLayersExcluding) folds
//     ONLY the read-layer files + inline + claude-import; the TWO MANAGED layers
//     (macOS MDM plist + managed config dir) are DETECT-ONLY, outside the fold. The
//     managed half of the re-resolve now flows through the single owner
//     mimoCodeManagedLayerReResolves (shadow shape minus disable-only), shared by
//     RemovableStdioEntries, FindStdioLanguageServerEntries, the combined predicate,
//     and the RemoveEntry pre-check.
//   - CHANGE 2 (RemoveEntry write-then-fail): a managed-active retention now refuses
//     BEFORE deleteMember mutates o.path, so mimocode.json is left byte-unchanged.
//   - GAP 2 (CHANGE 3): the destructive gopls/LSP cleanup's survivor filter was
//     stdio/LSP-WIDE (workspace-blind); it moved CALLER-SIDE to a workspace-scoped
//     recheck (register.go), so a same-name lower-layer entry for a DIFFERENT
//     workspace no longer blocks removal of the real workspace-A entry. These tests
//     pin the adapter-side primitives; the register-side workspace scope is pinned
//     in internal/api/register_test.go.
//
// Managed-layer seam: MIMOCODE_TEST_MANAGED_CONFIG_DIR (the managed config dir
// override, exercised through the real mimoCodeManagedConfigDirShadows reader so it
// works on every OS, not just darwin). The macOS MDM plist path is seam-tested
// separately via mimoCodeManagedPrefsReader in mimocode_pr420_fixes_test.go.

const followupStdioGopls = `{"type":"local","command":["gopls","mcp"],"enabled":true}`

// seedManagedConfigDir writes a managed config dir file and points the test
// override at it (overriding the non-existent path isolateMimoCodeEnv set).
func seedManagedConfigDir(t *testing.T, body string) {
	t.Helper()
	managedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(managedDir, "mimocode.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIMOCODE_TEST_MANAGED_CONFIG_DIR", managedDir)
}

// CLAIM 1 — a managed ACTIVE gopls + a write-target gopls → NOT removable (the
// managed-only branch (b) excludes it from RemovableStdioEntries) AND RemoveEntry
// leaves mimocode.json BYTE-UNCHANGED (CHANGE 2: refuse before deleteMember).
func TestMimoCode_Followup_ManagedActiveGopls_NotRemovable_AndRemoveEntryByteUnchanged(t *testing.T) {
	isolateMimoCodeEnv(t)
	// Managed config dir actively redefines gopls (full stdio redefinition).
	seedManagedConfigDir(t, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	const writeTargetBody = `{"mcp":{"gopls":` + followupStdioGopls + `}}`
	writeMimoFile(t, writeTargetPath, writeTargetBody)
	o := &mimoCodeClient{path: writeTargetPath}

	// (1a) RemovableStdioEntries must NOT report gopls — the managed layer actively
	// retains it, so the cleanup must not even offer it as removable.
	if reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("a managed-active gopls must be excluded from RemovableStdioEntries (managed branch (b) retains it)")
	}

	// (1b) The managed-only re-resolve owner reports the active managed retention. The
	// predicate is consumer-agnostic (it reads only the managed layer's own value).
	managedRetains, err := o.mimoCodeManagedLayerReResolves("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
	}
	if !managedRetains {
		t.Fatalf("an active managed gopls must re-resolve (mimoCodeManagedLayerReResolves=true)")
	}

	// (1c) RemoveEntry must REFUSE with the typed retention error AND leave the
	// write target byte-identical (no write-then-fail).
	before, err := os.ReadFile(writeTargetPath)
	if err != nil {
		t.Fatal(err)
	}
	err = o.RemoveEntry("gopls")
	var retainErr *ErrMimoCodeHigherLayerRetainsServer
	if !errors.As(err, &retainErr) {
		t.Fatalf("RemoveEntry over a managed-retained gopls must return ErrMimoCodeHigherLayerRetainsServer, got %T: %v", err, err)
	}
	if retainErr.Source.Kind != "managed-config-dir" {
		t.Errorf("retained Source.Kind = %q, want %q", retainErr.Source.Kind, "managed-config-dir")
	}
	// bot #425 r5 ("don't report a delete before it happens"): the pre-check refuses
	// BEFORE deleteMember, so the error must NOT claim the server was "removed" — it
	// must flag WriteTargetUnchanged and say the write target was left UNCHANGED.
	if !retainErr.WriteTargetUnchanged {
		t.Error("managed pre-check error must set WriteTargetUnchanged=true (file untouched)")
	}
	if msg := retainErr.Error(); strings.Contains(msg, "removed server") || !strings.Contains(msg, "UNCHANGED") {
		t.Errorf("pre-check error message must say the write target was left UNCHANGED, not \"removed\": %q", msg)
	}
	after, err := os.ReadFile(writeTargetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("WRITE-THEN-FAIL: mimocode.json was mutated before the retention error fired.\nbefore=%q\nafter =%q", before, after)
	}
}

// CLAIM 2 — a managed {enabled:true}-only overlay is inert once the write-target
// key is gone → STILL removable (managed branch (b) does not retain it).
func TestMimoCode_Followup_ManagedEnabledOnlyTrueOverlay_StillRemovable(t *testing.T) {
	isolateMimoCodeEnv(t)
	// Managed config dir carries ONLY an {enabled:true} overlay (no command/url).
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":true}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// Managed-only re-resolve must be FALSE (enabled-only:true is not a shadow).
	managedRetains, err := o.mimoCodeManagedLayerReResolves("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
	}
	if managedRetains {
		t.Fatalf("a managed {enabled:true}-only overlay is inert once the write-target key is gone — must NOT retain (removable)")
	}
	// RemovableStdioEntries must report gopls (it is removable).
	if !reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("RemovableStdioEntries must report gopls when the only managed override is a content-less {enabled:true} stub")
	}
	// RemoveEntry must SUCCEED and actually remove the entry.
	if err := o.RemoveEntry("gopls"); err != nil {
		t.Fatalf("RemoveEntry must succeed over a managed enabled-only:true overlay, got %v", err)
	}
	if v, ok, _ := mimoCodeFileEntryValue(writeTargetPath, "gopls"); ok && v != nil {
		t.Fatalf("RemoveEntry must have deleted the write-target gopls; it is still present: %+v", v)
	}
}

// CLAIM 2b — a managed {enabled:false}-only (disable-only) stub also leaves the
// entry removable (subtracted via mimoCodeShadowIsDisableOnlyOverride), and
// RemoveEntry succeeds (the disable-only-overlay success path is preserved).
func TestMimoCode_Followup_ManagedDisableOnlyStub_StillRemovable(t *testing.T) {
	isolateMimoCodeEnv(t)
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":false}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	managedRetains, err := o.mimoCodeManagedLayerReResolves("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
	}
	if managedRetains {
		t.Fatalf("a managed {enabled:false}-only disable-only stub retains NO active server — must NOT retain (removable)")
	}
	if !reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("RemovableStdioEntries must report gopls when the only managed override is a disable-only stub")
	}
	if err := o.RemoveEntry("gopls"); err != nil {
		t.Fatalf("RemoveEntry must succeed over a managed disable-only overlay (disable-only success path preserved), got %v", err)
	}
}

// CLAIM (managed full-redefine, LSP shape) — a managed ACTIVE mcp-language-server
// retains the name for the LSP path too (FindStdioLanguageServerEntries excludes
// it; RemoveEntry refuses byte-unchanged).
func TestMimoCode_Followup_ManagedActiveLSP_NotReported_AndRemoveEntryByteUnchanged(t *testing.T) {
	const stdioLS = `{"type":"local","command":["mcp-language-server","--lsp","go"],"enabled":true}`
	isolateMimoCodeEnv(t)
	seedManagedConfigDir(t, `{"mcp":{"gols":`+stdioLS+`}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gols":`+stdioLS+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	if stdioLSPEntryNames(t, o)["gols"] {
		t.Fatalf("a managed-active mcp-language-server must be excluded from FindStdioLanguageServerEntries")
	}
	before, _ := os.ReadFile(writeTargetPath)
	err := o.RemoveEntry("gols")
	var retainErr *ErrMimoCodeHigherLayerRetainsServer
	if !errors.As(err, &retainErr) {
		t.Fatalf("RemoveEntry over a managed-retained LSP entry must fail loud, got %T: %v", err, err)
	}
	after, _ := os.ReadFile(writeTargetPath)
	if string(before) != string(after) {
		t.Fatalf("WRITE-THEN-FAIL on the LSP path: mimocode.json was mutated before the retention error fired")
	}
}

// CLAIM 3 (adapter primitive) — the post-removal active-stdio reader surfaces a
// re-emergent lower-layer entry for a DIFFERENT workspace, which the CALLER scopes
// out. Here we pin the adapter half: ActiveStdioEntriesExcludingWriteTarget returns
// the re-emergent lower-layer gopls(workspace B), carrying its Command+Args so the
// caller can apply directEntryWorkspaceMatches. The write-target gopls(A) passes
// branch (a) and is NOT excluded by the managed-only branch (b).
func TestMimoCode_Followup_ActiveStdioReader_SurfacesDifferentWorkspaceReEmergence(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// Write target: gopls scoped to workspace A.
	writeMimoFile(t, writeTargetPath,
		`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp","--workspace","/ws/A"],"enabled":true}}}`)
	// config.json BELOW: SAME name, gopls scoped to workspace B (different workspace).
	writeMimoFile(t, filepath.Join(dir, "config.json"),
		`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp","--workspace","/ws/B"],"enabled":true}}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// branch (a): write target owns the stdio gopls shape.
	owns, err := o.mimoCodeWriteTargetDefinesStdio("gopls")
	if err != nil {
		t.Fatalf("mimoCodeWriteTargetDefinesStdio: %v", err)
	}
	if !owns {
		t.Fatalf("the write-target gopls(A) must pass branch (a)")
	}
	// The WORKSPACE-AWARE register-grain candidate source (branch (a) + managed-only,
	// NOT the conservative full-survivor RemovableStdioEntries) must report gopls(A)
	// as a candidate — the file-layer gopls(B) survivor is a CALLER-side
	// workspace-scoped recheck, not an adapter-side decline.
	cands, err := o.RemovableStdioCandidatesWriteTargetOwned()
	if err != nil {
		t.Fatalf("RemovableStdioCandidatesWriteTargetOwned: %v", err)
	}
	candNames := map[string]bool{}
	for _, e := range cands {
		candNames[e.Name] = true
	}
	if !candNames["gopls"] {
		t.Fatalf("with no managed layer, the write-target gopls(A) must be a RemovableStdioCandidatesWriteTargetOwned candidate (the workspace-scoped survivor is a CALLER-side recheck)")
	}
	// Sanity: the CONSERVATIVE RemovableStdioEntries (full survivor, workspace-free)
	// correctly DECLINES gopls here (config.json gopls(B) re-emerges active) — proving
	// the workspace-free CLI consumer stays protected against false-success deletes.
	if reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("the conservative full-survivor RemovableStdioEntries must DECLINE gopls when a lower-layer gopls re-emerges (workspace-free CLI protection)")
	}
	// The post-removal active-stdio reader surfaces the re-emergent gopls(B).
	survivors, err := o.ActiveStdioEntriesExcludingWriteTarget("gopls")
	if err != nil {
		t.Fatalf("ActiveStdioEntriesExcludingWriteTarget: %v", err)
	}
	var found *StdioEntry
	for i := range survivors {
		if survivors[i].Name == "gopls" {
			found = &survivors[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the post-removal reader must surface the re-emergent config.json gopls(B) so the caller can workspace-scope it")
	}
	// Its Args carry the DIFFERENT workspace, so the caller's directEntryWorkspaceMatches
	// will NOT match workspace A → does not block → gopls(A) IS removed (proven caller-side).
	hasWorkspaceFlag, hasB := false, false
	for _, a := range found.Args {
		if a == "--workspace" {
			hasWorkspaceFlag = true
		}
		if a == "/ws/B" {
			hasB = true
		}
	}
	if !hasWorkspaceFlag {
		t.Fatalf("the re-emergent entry must carry its --workspace arg for caller-side scoping, got args=%v", found.Args)
	}
	if !hasB {
		t.Fatalf("the re-emergent entry must be the workspace-B gopls, got args=%v", found.Args)
	}
}

// CLAIM 4 (adapter primitive) — the LSP post-removal reader surfaces a re-emergent
// lower-layer mcp-language-server for a DIFFERENT workspace, carrying Language +
// Args so the caller workspace-scopes it. The reader's Language comes from the
// single canonical classifier (matchLanguageServerStdio inside
// findLanguageServerStdioInMap) — never re-derived caller-side.
func TestMimoCode_Followup_ActiveLSPReader_SurfacesDifferentWorkspaceReEmergence(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// Write target: mcp-language-server go for workspace A.
	writeMimoFile(t, writeTargetPath,
		`{"mcp":{"mls":{"type":"local","command":["mcp-language-server","--lsp","go","--workspace","/ws/A"],"enabled":true}}}`)
	// config.json BELOW: SAME name, for workspace B.
	writeMimoFile(t, filepath.Join(dir, "config.json"),
		`{"mcp":{"mls":{"type":"local","command":["mcp-language-server","--lsp","go","--workspace","/ws/B"],"enabled":true}}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// The WORKSPACE-AWARE register-grain candidate source (branch (a) + managed-only)
	// reports mls — no managed layer retains it, so it is a candidate; the
	// workspace-scoped survivor is decided caller-side.
	cands, err := o.FindStdioLanguageServerCandidatesWriteTargetOwned()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerCandidatesWriteTargetOwned: %v", err)
	}
	candNames := map[string]bool{}
	for _, e := range cands {
		candNames[e.Name] = true
	}
	if !candNames["mls"] {
		t.Fatalf("with no managed layer, the write-target mcp-language-server(A) must be a FindStdioLanguageServerCandidatesWriteTargetOwned candidate")
	}
	// Sanity: the CONSERVATIVE FindStdioLanguageServerEntries (full survivor) correctly
	// DECLINES mls here (config.json mls(B) re-emerges active) — the workspace-free CLI
	// consumer stays protected.
	if stdioLSPEntryNames(t, o)["mls"] {
		t.Fatalf("the conservative full-survivor FindStdioLanguageServerEntries must DECLINE mls when a lower-layer mcp-language-server re-emerges (workspace-free CLI protection)")
	}
	survivors, err := o.ActiveLanguageServerEntriesExcludingWriteTarget("mls")
	if err != nil {
		t.Fatalf("ActiveLanguageServerEntriesExcludingWriteTarget: %v", err)
	}
	var found *LanguageServerStdioEntry
	for i := range survivors {
		if survivors[i].Name == "mls" {
			found = &survivors[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the LSP post-removal reader must surface the re-emergent config.json mcp-language-server(B)")
	}
	// Language is classifier-derived (go) and Args carry workspace B.
	if found.Language != "go" {
		t.Errorf("the re-emergent LSP entry's Language must be classifier-derived 'go', got %q", found.Language)
	}
	hasB := false
	for _, a := range found.Args {
		if a == "/ws/B" {
			hasB = true
		}
	}
	if !hasB {
		t.Fatalf("the re-emergent LSP entry must be the workspace-B one, got args=%v", found.Args)
	}
}

// CHANGE 1 regression — mimoCodeHigherLayerDefining still delegates steps 1+1b to
// mimoCodeManagedLayerShadows with byte-identical behavior: a managed-config-dir
// active shadow is still returned (so AddEntry still fails loud), and an
// enabled-only:true managed override is still NOT a shadow.
func TestMimoCode_Followup_HigherLayerDefining_DelegatesManaged(t *testing.T) {
	t.Run("managed-config-dir active shadow still detected via the extracted owner", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		seedManagedConfigDir(t, `{"mcp":{"serena":{"type":"remote","url":"http://managed/mcp","enabled":true}}}`)
		o := &mimoCodeClient{path: filepath.Join(t.TempDir(), "mimocode.json")}
		// The extracted owner sees it.
		src, err := o.mimoCodeManagedLayerShadows("serena")
		if err != nil {
			t.Fatalf("mimoCodeManagedLayerShadows: %v", err)
		}
		if src.Kind != "managed-config-dir" {
			t.Fatalf("mimoCodeManagedLayerShadows must detect the managed-config-dir shadow, got Kind=%q", src.Kind)
		}
		// mimoCodeHigherLayerDefining (delegating) returns the SAME source.
		hld, err := o.mimoCodeHigherLayerDefining("serena")
		if err != nil {
			t.Fatalf("mimoCodeHigherLayerDefining: %v", err)
		}
		if hld.Kind != "managed-config-dir" || hld.File != src.File {
			t.Fatalf("mimoCodeHigherLayerDefining must delegate to mimoCodeManagedLayerShadows byte-identically, got %+v want %+v", hld, src)
		}
	})
	t.Run("managed enabled-only:true is not a shadow", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		seedManagedConfigDir(t, `{"mcp":{"serena":{"enabled":true}}}`)
		o := &mimoCodeClient{path: filepath.Join(t.TempDir(), "mimocode.json")}
		src, err := o.mimoCodeManagedLayerShadows("serena")
		if err != nil {
			t.Fatalf("mimoCodeManagedLayerShadows: %v", err)
		}
		if src.Kind != "" {
			t.Fatalf("a managed enabled-only:true override must not be a shadow, got Kind=%q", src.Kind)
		}
		// Sanity: an enable-true managed overlay is not a shadow (Kind==""), so the
		// managed-own-value-only predicate reports false (removable) — the AddEntry-guard
		// "not a shadow" verdict and the re-resolve verdict agree.
		reResolves, err := o.mimoCodeManagedLayerReResolves("serena")
		if err != nil {
			t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
		}
		if reResolves {
			t.Fatalf("a managed enable-true overlay must NOT re-resolve on its own (removable)")
		}
	})
}

// NOTE: the F3 enable-true RE-ACTIVATION-over-DISABLED-lower-full test was DROPPED in
// the managed-OR simplification (architect GATE REVISE → PATH-B). It asserted the
// REMOVED effective-merge behavior (a managed enable-true overlay over a below-layer
// config.json full entry RETAINS the server via the merge). The managed-own-value-only
// predicate instead reports such an overlay retains NOTHING on its own, so the entry is
// REMOVABLE and the below-layer re-emergence is the INTENDED B4 rollback. That agreement
// is now pinned by TestMimoCode_UnifiedManagedOR_PreCheckAndB4Agree_EnableTrueOverBelowLayerFull
// in mimocode_pr425_unified_managed_or_test.go; the residual is documented in
// work-items/bugs/2026-06-24-mimocode-managed-enable-over-lower-residual.md.

// A managed enable-true overlay (which is NOT a managed shadow — Kind=="") retains
// NOTHING on its own, so the entry stays REMOVABLE. With no surviving content-bearing
// lower entry there is nothing to re-emerge either. This pins that the
// managed-own-value-only predicate does not over-block an enable-true overlay.
func TestMimoCode_Followup_ManagedEnableTrue_NoLowerContent_StaysRemovable(t *testing.T) {
	isolateMimoCodeEnv(t)
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":true}}}`)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// The content-bearing gopls lives ONLY in the write target; no config.json below.
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	reResolves, err := o.mimoCodeManagedLayerReResolves("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
	}
	if reResolves {
		t.Fatalf("a managed enable-true overlay must stay REMOVABLE (it retains nothing on its own)")
	}
	if !reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("gopls must be removable when the managed enable-true overlay has no surviving lower content to re-activate")
	}
}

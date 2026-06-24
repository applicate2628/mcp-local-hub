package clients

import (
	"os"
	"path/filepath"
	"testing"
)

// This file pins the bot PR #425 FOLLOW-UP CONSERVATIVE CLEANUP GUARDS (two codex-bot
// findings on PR #425). The two SIMULATE/cleanup consumers
// (RemovableStdioCandidatesWriteTargetOwned + FindStdioLanguageServerCandidatesWriteTargetOwned)
// now EXCLUDE a candidate the merge-based simulate would otherwise FALSELY report
// removable, in two cases the bot asked us to "conservatively block / fall back to the
// conservative non-removable result":
//
//   FINDING 1 — a MANAGED enable-only:true overlay over a lower DISABLED full entry.
//     MiMoCode's enabled-overlay merge re-activates the lower entry, so after the
//     write-target key is deleted the server stays ACTIVE — but the cleanup would report
//     it cleared. The dedicated own-value-only detector
//     (mimoCodeManagedEnableOnlyTrueOverlay) excludes the candidate. PROTECTED
//     mimoCodeManagedLayerReResolves (PATH-B, shared with the RemoveEntry pre-check + B4)
//     correctly returns FALSE for an enable-only overlay (Kind==""), so it does NOT cover
//     this — the new guard is ADDITIVE and lives ONLY in the two consumers.
//
//   FINDING 2 — a lower-layer file HARD-LINKED to the write target that defines the name.
//     The merge-based simulate (readMergedLayersExcluding) matches the write target by
//     INODE identity and drops the name from BOTH copies, predicting removable.
//     PRODUCTION diverges: deleteMember's temp+rename breaks the link and leaves the lower
//     entry LIVE. mimoCodeLowerLayerHardLinkedToWriteTargetDefines excludes the candidate.
//
// These guards are SIMULATE-CONSUMER-ONLY: the RemoveEntry pre-check
// (mimoCodeManagedLayerReResolves) + B4 post-delete guard are UNCHANGED, so they still
// AGREE by construction — pinned by the asserts below and by
// TestMimoCode_UnifiedManagedOR_PreCheckAndB4Agree_EnableTrueOverBelowLayerFull.

// goplsStdioDisabled is the lower-layer DISABLED full gopls entry a managed enable-only
// overlay re-activates (FINDING 1). followupStdioGopls is the ENABLED full entry.
const goplsStdioDisabled = `{"type":"local","command":["gopls","mcp"],"enabled":false}`

// stdioLSDisabled is the LSP-shaped sibling: a DISABLED full mcp-language-server entry.
const stdioLSDisabled = `{"type":"local","command":["mcp-language-server","--lsp","go"],"enabled":false}`
const stdioLSEnabled = `{"type":"local","command":["mcp-language-server","--lsp","go"],"enabled":true}`

// removableCandidateNames returns the RemovableStdioCandidatesWriteTargetOwned name set.
func removableCandidateNames(t *testing.T, o *mimoCodeClient) map[string]bool {
	t.Helper()
	cands, err := o.RemovableStdioCandidatesWriteTargetOwned()
	if err != nil {
		t.Fatalf("RemovableStdioCandidatesWriteTargetOwned: %v", err)
	}
	got := map[string]bool{}
	for _, e := range cands {
		got[e.Name] = true
	}
	return got
}

// lspCandidateNames returns the FindStdioLanguageServerCandidatesWriteTargetOwned name set.
func lspCandidateNames(t *testing.T, o *mimoCodeClient) map[string]bool {
	t.Helper()
	cands, err := o.FindStdioLanguageServerCandidatesWriteTargetOwned()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerCandidatesWriteTargetOwned: %v", err)
	}
	got := map[string]bool{}
	for _, e := range cands {
		got[e.Name] = true
	}
	return got
}

// FINDING 1 (managed config dir) — a managed {enabled:true} enable-only overlay over a
// lower DISABLED full config.json gopls → RemovableStdioCandidatesWriteTargetOwned
// EXCLUDES the candidate (conservative block — the server stays active from config.json +
// the managed enable after the write-target delete, so cleanup must NOT report it
// removable). The PROTECTED PATH-B owner mimoCodeManagedLayerReResolves stays FALSE (it
// is the additive consumer-only detector that excludes).
func TestMimoCode_CleanupGuard_F1_ManagedEnableOverLowerDisabled_StdioExcluded(t *testing.T) {
	isolateMimoCodeEnv(t)
	// Managed config dir carries ONLY an {enabled:true} overlay (no command/url).
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":true}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// Write target: the ENABLED full gopls the hub owns (passes branch (a)).
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	// config.json BELOW: the SAME name as a DISABLED full entry that re-emerges and is
	// re-activated by the managed enable overlay.
	writeMimoFile(t, filepath.Join(dir, "config.json"), `{"mcp":{"gopls":`+goplsStdioDisabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// The conservative consumer guard must EXCLUDE gopls.
	if removableCandidateNames(t, o)["gopls"] {
		t.Fatalf("FINDING 1: a managed enable-only:true overlay over a lower disabled full entry must EXCLUDE the candidate from RemovableStdioCandidatesWriteTargetOwned (conservative block — server stays active after the delete)")
	}

	// The dedicated own-value-only detector reports the managed enable-only overlay.
	hasOverlay, err := o.mimoCodeManagedEnableOnlyTrueOverlay("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedEnableOnlyTrueOverlay: %v", err)
	}
	if !hasOverlay {
		t.Fatalf("mimoCodeManagedEnableOnlyTrueOverlay must detect the managed {enabled:true} overlay")
	}

	// PROTECTED PATH-B owner UNCHANGED: it returns FALSE for an enable-only overlay
	// (Kind==""), so the RemoveEntry pre-check / B4 path is not perturbed by this guard.
	preCheck, err := o.mimoCodeManagedLayerReResolves("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
	}
	if preCheck {
		t.Fatalf("PROTECTED mimoCodeManagedLayerReResolves must stay FALSE for an enable-only overlay (the guard is consumer-only, NOT in the pre-check)")
	}
}

// FINDING 1 (LSP shape) — the same over-lower-disabled managed enable overlay excludes the
// candidate from FindStdioLanguageServerCandidatesWriteTargetOwned.
func TestMimoCode_CleanupGuard_F1_ManagedEnableOverLowerDisabled_LSPExcluded(t *testing.T) {
	isolateMimoCodeEnv(t)
	seedManagedConfigDir(t, `{"mcp":{"mls":{"enabled":true}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"mls":`+stdioLSEnabled+`}}`)
	writeMimoFile(t, filepath.Join(dir, "config.json"), `{"mcp":{"mls":`+stdioLSDisabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	if lspCandidateNames(t, o)["mls"] {
		t.Fatalf("FINDING 1 (LSP): a managed enable-only:true overlay over a lower disabled full mcp-language-server must EXCLUDE the candidate from FindStdioLanguageServerCandidatesWriteTargetOwned")
	}
}

// FINDING 1 (MDM plist seam) — the managed enable-only:true overlay is also detected via
// the macOS Managed Preferences (MDM) plist seam, exercised on any OS through the
// mimoCodeManagedPrefsEnableOnlyTrueReader func-var. Both consumers exclude.
func TestMimoCode_CleanupGuard_F1_ManagedEnableMDMSeam_BothConsumersExclude(t *testing.T) {
	isolateMimoCodeEnv(t)

	prev := mimoCodeManagedPrefsEnableOnlyTrueReader
	t.Cleanup(func() { mimoCodeManagedPrefsEnableOnlyTrueReader = prev })
	mimoCodeManagedPrefsEnableOnlyTrueReader = func(name string) (bool, error) {
		return name == "gopls" || name == "mls", nil
	}

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath,
		`{"mcp":{"gopls":`+followupStdioGopls+`,"mls":`+stdioLSEnabled+`}}`)
	// Lower disabled full entries the MDM enable overlay re-activates.
	writeMimoFile(t, filepath.Join(dir, "config.json"),
		`{"mcp":{"gopls":`+goplsStdioDisabled+`,"mls":`+stdioLSDisabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	if removableCandidateNames(t, o)["gopls"] {
		t.Fatalf("FINDING 1 (MDM seam): the stdio consumer must EXCLUDE gopls when the MDM plist carries an enable-only:true overlay")
	}
	if lspCandidateNames(t, o)["mls"] {
		t.Fatalf("FINDING 1 (MDM seam): the LSP consumer must EXCLUDE mls when the MDM plist carries an enable-only:true overlay")
	}
}

// FINDING 1 over-block (architect-ruled ACCEPTABLE) — a managed {enabled:true} overlay
// with NO lower entry still EXCLUDES the candidate from both consumers. There is nothing
// to re-activate, but over-blocking is safe conservatism (no false-cleanup; the operator
// cleans the redundant managed entry manually).
func TestMimoCode_CleanupGuard_F1_ManagedEnableNoLower_OverBlockAccepted(t *testing.T) {
	isolateMimoCodeEnv(t)
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":true},"mls":{"enabled":true}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// The content-bearing entries live ONLY in the write target; no config.json below.
	writeMimoFile(t, writeTargetPath,
		`{"mcp":{"gopls":`+followupStdioGopls+`,"mls":`+stdioLSEnabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	if removableCandidateNames(t, o)["gopls"] {
		t.Fatalf("FINDING 1 over-block: a managed {enabled:true} overlay (no lower entry) is EXCLUDED from the stdio consumer (acceptable conservatism)")
	}
	if lspCandidateNames(t, o)["mls"] {
		t.Fatalf("FINDING 1 over-block: a managed {enabled:true} overlay (no lower entry) is EXCLUDED from the LSP consumer (acceptable conservatism)")
	}
}

// FINDING 2 (stdio) — config.json HARD-LINKED to the write target and defining the
// candidate name → RemovableStdioCandidatesWriteTargetOwned EXCLUDES it (production's
// temp+rename breaks the link and leaves the config.json entry live, so the simulate's
// "removable" prediction is false). os.Link creates the hard link; a non-default
// deliberate operator setup.
func TestMimoCode_CleanupGuard_F2_HardLinkedLowerLayer_StdioExcluded(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// Write target AND config.json share ONE inode (hard-linked), both carrying gopls.
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	configPath := filepath.Join(dir, "config.json")
	if err := os.Link(writeTargetPath, configPath); err != nil {
		t.Skipf("os.Link (hard link) unsupported on this filesystem: %v", err)
	}
	o := &mimoCodeClient{path: writeTargetPath}

	// Sanity: the detector confirms the hard-linked lower layer defines the name.
	hardLinked, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines("gopls")
	if err != nil {
		t.Fatalf("mimoCodeLowerLayerHardLinkedToWriteTargetDefines: %v", err)
	}
	if !hardLinked {
		t.Fatalf("the detector must report config.json (hard-linked to the write target, defining gopls) as a re-emergent lower layer")
	}

	if removableCandidateNames(t, o)["gopls"] {
		t.Fatalf("FINDING 2: a hard-linked config.json defining gopls must EXCLUDE the candidate from RemovableStdioCandidatesWriteTargetOwned (temp+rename leaves the entry live)")
	}
}

// FINDING 2 (LSP) — the same hard-link case excludes the candidate from the LSP consumer.
func TestMimoCode_CleanupGuard_F2_HardLinkedLowerLayer_LSPExcluded(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"mls":`+stdioLSEnabled+`}}`)
	configPath := filepath.Join(dir, "config.json")
	if err := os.Link(writeTargetPath, configPath); err != nil {
		t.Skipf("os.Link (hard link) unsupported on this filesystem: %v", err)
	}
	o := &mimoCodeClient{path: writeTargetPath}

	if lspCandidateNames(t, o)["mls"] {
		t.Fatalf("FINDING 2 (LSP): a hard-linked config.json defining mls must EXCLUDE the candidate from FindStdioLanguageServerCandidatesWriteTargetOwned")
	}
}

// FINDING 2 negative — a config.json that is a DISTINCT file (NOT hard-linked) defining a
// DIFFERENT-content entry must NOT be excluded by the hard-link guard. This proves the
// guard keys on inode identity, not mere name presence: the write-target gopls(A) stays a
// candidate (its lower-layer survivor is the CALLER-side workspace recheck's job, not the
// hard-link guard's). Mirrors the existing
// TestMimoCode_Followup_ActiveStdioReader_SurfacesDifferentWorkspaceReEmergence shape.
func TestMimoCode_CleanupGuard_F2_DistinctLowerLayer_NotExcludedByHardLinkGuard(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath,
		`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp","--workspace","/ws/A"],"enabled":true}}}`)
	// A DISTINCT (separately-written, non-linked) config.json for a different workspace.
	writeMimoFile(t, filepath.Join(dir, "config.json"),
		`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp","--workspace","/ws/B"],"enabled":true}}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// The hard-link detector must report FALSE (distinct inode).
	hardLinked, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines("gopls")
	if err != nil {
		t.Fatalf("mimoCodeLowerLayerHardLinkedToWriteTargetDefines: %v", err)
	}
	if hardLinked {
		t.Fatalf("a DISTINCT (non-hard-linked) config.json must NOT trip the hard-link guard")
	}
	// gopls(A) stays a candidate (no managed layer, no hard link; the workspace-scoped
	// survivor is decided caller-side).
	if !removableCandidateNames(t, o)["gopls"] {
		t.Fatalf("the write-target gopls(A) must remain a candidate when the lower config.json is a DISTINCT file (the workspace survivor is a CALLER-side recheck)")
	}
}

// AGREEMENT (pre-check + B4 UNCHANGED) — under the FINDING 1 scenario the two simulate
// consumers EXCLUDE the candidate, yet the PROTECTED RemoveEntry managed pre-check
// (mimoCodeManagedLayerReResolves) and the post-delete B4 guard owner
// (mimoCodeHigherLayerDefining) both still ALLOW the delete (Kind==""/false). This proves
// the guard is consumer-only and introduces NO pre-check/B4 divergence — the same
// invariant TestMimoCode_UnifiedManagedOR_PreCheckAndB4Agree_EnableTrueOverBelowLayerFull
// pins, re-asserted here alongside the consumer exclusion.
func TestMimoCode_CleanupGuard_PreCheckAndB4Unchanged_WhileConsumersExclude(t *testing.T) {
	isolateMimoCodeEnv(t)
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":true}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	writeMimoFile(t, filepath.Join(dir, "config.json"), `{"mcp":{"gopls":`+goplsStdioDisabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// Consumer EXCLUDES (the conservative block).
	if removableCandidateNames(t, o)["gopls"] {
		t.Fatalf("the stdio consumer must EXCLUDE gopls under the FINDING 1 scenario")
	}

	// PRE-CHECK owner UNCHANGED — still FALSE (allows the delete; PATH-B own-value-only).
	preCheck, err := o.mimoCodeManagedLayerReResolves("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
	}
	if preCheck {
		t.Fatalf("PROTECTED pre-check must stay FALSE (the consumer guard must NOT leak into the pre-check)")
	}

	// B4 owner UNCHANGED — still Kind=="" (allows the delete; the managed enable-true is
	// not a shadow and config.json is below the write target).
	hld, err := o.mimoCodeHigherLayerDefining("gopls")
	if err != nil {
		t.Fatalf("mimoCodeHigherLayerDefining: %v", err)
	}
	if hld.Kind != "" {
		t.Fatalf("PROTECTED B4 guard must stay Kind=\"\" (no divergence introduced), got %q", hld.Kind)
	}
}

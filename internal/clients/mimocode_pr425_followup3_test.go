package clients

import (
	"path/filepath"
	"testing"
)

// This file pins the bot PR #425 FOLLOW-UP-3 refinements — three codex-bot findings that
// are SYMMETRIC-GAP corrections of the PR #425 follow-up cleanup guards/gates:
//
//   FINDING 1 (CLI managed-enable guard) — the workspace-FREE CLI consumers
//     (FindStdioLanguageServerEntries + RemovableStdioEntries, consumed by `mcphub
//     language-server cleanup`) got the FINDING-2 hard-link guard but NOT a managed
//     enable-only guard. In the {managed {enabled:true} overlay + lower DISABLED full
//     entry} scenario, the CLI's branch-(b) survivor reads removable (it never folds the
//     managed layer and drops the disabled lower entry), so the CLI deletes the
//     write-target key while the managed enable re-activates the lower entry → false
//     cleanup. The conditioned guard mimoCodeManagedEnableOnlyReactivatesLowerSurvivor now
//     EXCLUDES exactly that case. It is NARROWER than the register grain's over-blocking
//     guard: a managed enable-only overlay with NO surviving lower content stays REMOVABLE
//     on the CLI path (the full file survivor would otherwise wrongly drop it) — pinned by
//     the CLAIM-2 followup tests, re-asserted here for the CLI methods.
//
//   FINDING 2 (case-alias is NOT a hard link) — mimoCodeLowerLayerHardLinkedToWriteTarget
//     Defines compared the lower-layer path against the write target with a case-SENSITIVE
//     filepath.Clean equality. On a case-INSENSITIVE volume a case-only alias of the write
//     target (e.g. MIMOCODE.JSON vs mimocode.json, an operator MIMOCODE_CONFIG spelling) is
//     the SAME directory entry; the case-sensitive compare missed it, so os.Lstat+os.SameFile
//     mis-classified it as a hard link and wrongly BLOCKED an otherwise-removable candidate.
//     Like a symlink, a case-only alias FOLLOWS deleteMember's temp+rename, so it must NOT
//     block. The fix folds the compare via the volume-probe owner so a genuine case-variant
//     file on a case-SENSITIVE volume still reaches the os.SameFile hard-link check.
//
//   FINDING 3 (effective-enabled own-stdio gate) — the own-stdio gates
//     (mimoCodeWriteTargetDefinesStdio / ...StdioLSP) required the write target's OWN value
//     to physically carry a command. A write-target bare {enabled:true} enable-only overlay
//     that ACTIVATES a lower-layer stdio command (config.json {command, enabled:false}) made
//     the merged effective entry an ACTIVE direct gopls/LSP, and deleting the write-target
//     key re-disables it — so the hub OWNS the activation and it IS removable. The gates now
//     also own a write-target enable-only-TRUE overlay whose merge yields an active stdio,
//     so the candidate reaches the survivor recheck instead of being dropped. DISTINCT from
//     the FINDING-1 MANAGED enable-only (which EXCLUDES — the hub cannot clean a managed
//     layer).
//
// State-safe: every test uses isolateMimoCodeEnv + t.TempDir paths; the adapter never
// touches the developer's real ~/.config/mimocode. Reuses writeMimoFile, isolateMimoCodeEnv,
// seedManagedConfigDir, followupStdioGopls, goplsStdioDisabled, stdioLSEnabled,
// stdioLSDisabled, reResolveRemovableNames, stdioLSPEntryNames, removableCandidateNames,
// lspCandidateNames, mimoCodeDirCaseInsensitive.

// FINDING 1 (stdio CLI path) — a managed {enabled:true} enable-only overlay over a lower
// DISABLED full config.json gopls, with the hub's enabled gopls in the write target, must
// be EXCLUDED from the workspace-FREE RemovableStdioEntries (the `mcphub language-server
// cleanup` stdio path). After RemoveEntry deletes the write-target key the managed enable
// re-activates config.json's lower entry → the server stays ACTIVE, so reporting it
// removable would be a false cleanup. SYMMETRIC with the register-grain candidate methods
// (which already excluded this case) and with the CLI hard-link guard.
func TestMimoCode_CleanupGuardF3_F1_CLIManagedEnableOverLowerDisabled_StdioExcluded(t *testing.T) {
	isolateMimoCodeEnv(t)
	// Managed config dir carries ONLY an {enabled:true} overlay (no command/url).
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":true}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// Write target: the ENABLED full gopls the hub owns (passes branch (a) directly).
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	// config.json BELOW: a DISABLED full entry the managed enable re-activates after the
	// write-target key is deleted.
	writeMimoFile(t, filepath.Join(dir, "config.json"), `{"mcp":{"gopls":`+goplsStdioDisabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// The conditioned detector reports the re-activatable lower survivor (stdio consumer
	// shape — the lower disabled gopls IS a stdio command, bot PR #425 FINDING 3).
	reactivates, err := o.mimoCodeManagedEnableOnlyReactivatesLowerSurvivor("gopls", reResolveConsumerStdio)
	if err != nil {
		t.Fatalf("mimoCodeManagedEnableOnlyReactivatesLowerSurvivor: %v", err)
	}
	if !reactivates {
		t.Fatalf("FINDING 1: a managed {enabled:true} overlay over a lower disabled full entry must be detected as re-activating a lower survivor")
	}

	// The workspace-FREE CLI stdio consumer must EXCLUDE gopls.
	if reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("FINDING 1: a managed enable-only:true overlay over a lower disabled full gopls must EXCLUDE the entry from RemovableStdioEntries (CLI) — server stays active after the delete")
	}

	// PROTECTED PATH-B owner UNCHANGED: returns FALSE for an enable-only overlay, so the
	// RemoveEntry pre-check / B4 path is not perturbed.
	preCheck, err := o.mimoCodeManagedLayerReResolves("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
	}
	if preCheck {
		t.Fatalf("PROTECTED mimoCodeManagedLayerReResolves must stay FALSE for an enable-only overlay (the CLI guard is consumer-only, NOT in the pre-check)")
	}
}

// FINDING 1 (LSP CLI path) — the same managed-enable-over-lower-disabled case excludes the
// entry from the workspace-FREE FindStdioLanguageServerEntries (the LSP CLI cleanup path).
func TestMimoCode_CleanupGuardF3_F1_CLIManagedEnableOverLowerDisabled_LSPExcluded(t *testing.T) {
	isolateMimoCodeEnv(t)
	seedManagedConfigDir(t, `{"mcp":{"mls":{"enabled":true}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"mls":`+stdioLSEnabled+`}}`)
	writeMimoFile(t, filepath.Join(dir, "config.json"), `{"mcp":{"mls":`+stdioLSDisabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	if stdioLSPEntryNames(t, o)["mls"] {
		t.Fatalf("FINDING 1 (LSP): a managed enable-only:true overlay over a lower disabled full mcp-language-server must EXCLUDE the entry from FindStdioLanguageServerEntries (CLI)")
	}
}

// FINDING 1 (CLI no-over-block guard) — a managed {enabled:true} overlay with NO surviving
// lower content (the gopls command lives ONLY in the write target) must STAY removable on
// the CLI path. This pins that the conditioned guard is NARROWER than the register grain's
// over-block: the CLI's full file survivor would wrongly drop a genuinely-removable entry
// if the guard fired unconditionally. (Mirrors the existing CLAIM-2 followup tests, asserted
// here through both CLI consumers.)
func TestMimoCode_CleanupGuardF3_F1_CLIManagedEnableNoLower_StaysRemovable(t *testing.T) {
	isolateMimoCodeEnv(t)
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":true},"mls":{"enabled":true}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// The content-bearing entries live ONLY in the write target; no config.json below.
	writeMimoFile(t, writeTargetPath,
		`{"mcp":{"gopls":`+followupStdioGopls+`,"mls":`+stdioLSEnabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	reactivates, err := o.mimoCodeManagedEnableOnlyReactivatesLowerSurvivor("gopls", reResolveConsumerStdio)
	if err != nil {
		t.Fatalf("mimoCodeManagedEnableOnlyReactivatesLowerSurvivor: %v", err)
	}
	if reactivates {
		t.Fatalf("FINDING 1 (no-over-block): a managed {enabled:true} overlay with NO lower survivor must NOT report a re-activatable lower survivor (CLI must not over-block)")
	}
	if !reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("FINDING 1 (no-over-block): gopls must STAY removable on the CLI RemovableStdioEntries path when the managed enable overlay has no surviving lower content")
	}
	if !stdioLSPEntryNames(t, o)["mls"] {
		t.Fatalf("FINDING 1 (no-over-block): mls must STAY reported on the CLI FindStdioLanguageServerEntries path when the managed enable overlay has no surviving lower content")
	}
}

// FINDING 2 (case-only alias is NOT a hard link) — a layer path that names the write target
// with DIFFERENT CASING (e.g. MIMOCODE_CONFIG pointing at MIMOCODE.JSON while the write
// target is mimocode.json) on a case-INSENSITIVE volume is the SAME directory entry, NOT a
// distinct hard link. The detector must report FALSE (it FOLLOWS the temp+rename, so the
// entry does not re-emerge), keeping the candidate REMOVABLE. On a case-SENSITIVE volume the
// two are genuinely distinct files, so the test skips (the fold must not exempt there).
func TestMimoCode_CleanupGuardF3_F2_CaseOnlyAlias_NotHardLink(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	// Probe AFTER writing the write target so the dir has a case-flippable entry
	// (mimocode.json) to probe; an empty dir returns inconclusive.
	if ci, ok := mimoCodeDirCaseInsensitive(dir); !ok || !ci {
		// On a case-SENSITIVE volume MIMOCODE.JSON and mimocode.json are distinct files
		// and there is no case-alias to exercise; the fold is gated on the same probe, so
		// it correctly does not exempt there. Skip rather than assert a no-op.
		t.Skip("temp volume is case-sensitive (or the probe was inconclusive); the case-alias fold does not apply")
	}
	// MIMOCODE_CONFIG points at a CASE-VARIANT spelling of the write target. On a
	// case-insensitive volume this is the SAME file as mimocode.json — a case-only alias,
	// NOT a distinct hard link. configFile is appended to readLayerFiles ABOVE the globals.
	caseAlias := filepath.Join(dir, "MIMOCODE.JSON")
	o := &mimoCodeClient{path: writeTargetPath, configFile: caseAlias}

	// The detector must report FALSE — the case-alias layer is the write target itself
	// under a different spelling, not a true hard link.
	hardLinked, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines("gopls")
	if err != nil {
		t.Fatalf("mimoCodeLowerLayerHardLinkedToWriteTargetDefines: %v", err)
	}
	if hardLinked {
		t.Fatalf("FINDING 2: a CASE-ONLY alias of the write target (MIMOCODE.JSON vs mimocode.json) on a case-insensitive volume must NOT be treated as a hard link (it is the same directory entry, follows the temp+rename)")
	}
	// The candidate stays removable through the workspace-free CLI consumer (no hard link,
	// no managed layer; the case-alias resolves to the write target itself).
	if !reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("FINDING 2: gopls must stay removable when the only extra layer is a CASE-ALIAS of the write target (not blocked by the hard-link guard)")
	}
}

// FINDING 3 (stdio own-gate) — a write-target bare {enabled:true} enable-only overlay (NO
// command of its own) over a lower config.json {command, enabled:false} gopls makes the
// MERGED effective entry an ACTIVE direct gopls; deleting the write-target key re-disables
// it (config.json's enabled:false takes over). The hub OWNS the activation, so the own-stdio
// gate mimoCodeWriteTargetDefinesStdio must report it OWNED and the entry must reach the
// candidate set (register grain) / be reported removable (CLI). DISTINCT from a MANAGED
// enable-only (which EXCLUDES).
func TestMimoCode_CleanupGuardF3_F3_WriteTargetEnableOnlyActivatesLowerGopls_Owned(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// Write target's OWN value is a bare {enabled:true} — no command. It ACTIVATES the
	// lower config.json gopls via MiMoCode's enabled-overlay merge.
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":{"enabled":true}}}`)
	// config.json BELOW: the disabled full gopls command the write-target enable flips on.
	writeMimoFile(t, filepath.Join(dir, "config.json"), `{"mcp":{"gopls":`+goplsStdioDisabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// The own-stdio gate must OWN the entry via the effective-enabled branch even though
	// the write target's own value carries no command.
	owned, err := o.mimoCodeWriteTargetDefinesStdio("gopls")
	if err != nil {
		t.Fatalf("mimoCodeWriteTargetDefinesStdio: %v", err)
	}
	if !owned {
		t.Fatalf("FINDING 3: a write-target {enabled:true} enable-only overlay that activates a lower gopls command must be OWNED by mimoCodeWriteTargetDefinesStdio (effective-enabled ownership)")
	}

	// The register-grain candidate source must REPORT gopls (reaches the workspace recheck):
	// no managed layer (the enable is the WRITE TARGET, not managed), no hard link.
	if !removableCandidateNames(t, o)["gopls"] {
		t.Fatalf("FINDING 3: the effective-enabled write-target gopls must be a RemovableStdioCandidatesWriteTargetOwned candidate (reach the recheck)")
	}

	// The workspace-FREE CLI consumer must also report it removable (after removing the
	// write-target key, config.json reverts to disabled → not active → cleanly removable).
	if !reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("FINDING 3: the effective-enabled write-target gopls must be reported removable by RemovableStdioEntries (CLI) — removal re-disables it via config.json")
	}
}

// FINDING 3 (LSP own-gate sibling) — the same effective-enabled ownership applies to the
// LSP shape: a write-target {enabled:true} overlay over a lower disabled full
// mcp-language-server command makes the merged effective entry an active direct LSP, so
// mimoCodeWriteTargetDefinesStdioLSP must OWN it and the entry must reach the candidate /
// CLI sets.
func TestMimoCode_CleanupGuardF3_F3_WriteTargetEnableOnlyActivatesLowerLSP_Owned(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"mls":{"enabled":true}}}`)
	writeMimoFile(t, filepath.Join(dir, "config.json"), `{"mcp":{"mls":`+stdioLSDisabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	owned, err := o.mimoCodeWriteTargetDefinesStdioLSP("mls")
	if err != nil {
		t.Fatalf("mimoCodeWriteTargetDefinesStdioLSP: %v", err)
	}
	if !owned {
		t.Fatalf("FINDING 3 (LSP): a write-target {enabled:true} overlay that activates a lower mcp-language-server command must be OWNED by mimoCodeWriteTargetDefinesStdioLSP")
	}
	if !lspCandidateNames(t, o)["mls"] {
		t.Fatalf("FINDING 3 (LSP): the effective-enabled write-target LSP must be a FindStdioLanguageServerCandidatesWriteTargetOwned candidate")
	}
	if !stdioLSPEntryNames(t, o)["mls"] {
		t.Fatalf("FINDING 3 (LSP): the effective-enabled write-target LSP must be reported by FindStdioLanguageServerEntries (CLI)")
	}
}

// FINDING 3 guard — the effective-enabled OWN-stdio branch must NOT fire for a write-target
// {enabled:false} overlay over a lower disabled command (the merge stays disabled → nothing
// active → nothing the hub's key contributes to an active server). Pins that the branch is
// the EFFECTIVE-ACTIVE check, not bare enable-only presence, so a disabling overlay is not
// mis-owned.
func TestMimoCode_CleanupGuardF3_F3_WriteTargetDisableOverlay_NotOwned(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// Write target {enabled:false} overlay over a lower disabled command → merged disabled.
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":{"enabled":false}}}`)
	writeMimoFile(t, filepath.Join(dir, "config.json"), `{"mcp":{"gopls":`+goplsStdioDisabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	owned, err := o.mimoCodeWriteTargetDefinesStdio("gopls")
	if err != nil {
		t.Fatalf("mimoCodeWriteTargetDefinesStdio: %v", err)
	}
	if owned {
		t.Fatalf("FINDING 3 guard: a write-target {enabled:false} overlay over a lower disabled command must NOT be owned (merge stays disabled — nothing active to clean)")
	}
}

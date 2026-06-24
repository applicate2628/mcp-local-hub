package clients

import (
	"os"
	"path/filepath"
	"testing"
)

// This file pins the bot PR #425 FOLLOW-UP-2 refinements — three codex-bot findings on the
// PR #425 follow-up cleanup-consumer guards:
//
//   FINDING 1 (true-hard-link detection) — mimoCodeLowerLayerHardLinkedToWriteTargetDefines
//     must distinguish a REAL hard link from a SYMLINK / case-only alias. A symlink to the
//     write target reports the same inode under os.Stat (which FOLLOWS symlinks), so the
//     prior os.Stat+os.SameFile detector mis-classified it as a hard link and wrongly
//     EXCLUDED an otherwise-removable candidate. Unlike a true hard link, a symlink FOLLOWS
//     deleteMember's temp+rename (it re-points at the new file), so NO old entry re-emerges
//     and the candidate MUST stay removable. The corrected detector os.Lstats both ends,
//     requires BOTH regular (non-symlink), then confirms the same inode.
//
//   FINDING 2 (re-enabled LSP ownership gate) — the LSP write-target ownership gate must
//     judge the write target's OWN value WITHOUT mimoCodeDropDisabled, so a write-target
//     mcp-language-server {enabled:false} that a HIGHER overlay re-enables is still OWNED
//     (reaches the workspace recheck / is reported removable), mirroring the plain-gopls
//     parity mimoCodeWriteTargetDefinesStdio already had.
//
//   FINDING 3 (CLI-path hard-link guard) — the workspace-FREE CLI consumers
//     (FindStdioLanguageServerEntries + RemovableStdioEntries, consumed by `mcphub
//     language-server cleanup`) rely on the merge-based re-resolve survivor, which matches
//     the write target by INODE and so mis-predicts a hard-linked lower layer as removable.
//     The same FINDING-1-corrected detector now excludes such an entry from the CLI path.
//
// State-safe: every test uses isolateMimoCodeEnv + t.TempDir paths; the adapter never
// touches the developer's real ~/.config/mimocode. Reuses writeMimoFile, isolateMimoCodeEnv,
// followupStdioGopls, stdioLSEnabled, stdioLSDisabled, removableCandidateNames,
// lspCandidateNames, reResolveRemovableNames, stdioLSPEntryNames.

// FINDING 1 (symlink NOT blocked) — a lower-layer config.json that is a SYMLINK to the
// write-target mimocode.json (so os.Stat reports the same inode) must NOT be treated as a
// hard link: the symlink follows the temp+rename, so the entry does NOT re-emerge and the
// candidate stays REMOVABLE. The detector returns false; the register-grain candidate
// methods keep the candidate.
func TestMimoCode_CleanupGuard_F1Corr_SymlinkLowerLayer_NotBlocked(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	configPath := filepath.Join(dir, "config.json")
	// config.json is a SYMLINK to the write target (a deliberate operator overlay setup,
	// e.g. MIMOCODE_CONFIG/overlay symlinked to mimocode.json). os.Stat would report the
	// SAME inode via symlink-follow; os.Lstat distinguishes it.
	if err := os.Symlink(writeTargetPath, configPath); err != nil {
		t.Skipf("os.Symlink unsupported on this host/filesystem (Windows needs the privilege): %v", err)
	}
	o := &mimoCodeClient{path: writeTargetPath}

	// The corrected detector must report FALSE — a symlink is NOT a true hard link.
	hardLinked, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines("gopls")
	if err != nil {
		t.Fatalf("mimoCodeLowerLayerHardLinkedToWriteTargetDefines: %v", err)
	}
	if hardLinked {
		t.Fatalf("FINDING 1: a SYMLINK to the write target must NOT be treated as a hard link (it FOLLOWS the temp+rename; the entry does not re-emerge)")
	}
	// The candidate stays removable in the register-grain candidate source (no hard link,
	// no managed layer; the symlink resolves to the write target itself).
	if !removableCandidateNames(t, o)["gopls"] {
		t.Fatalf("FINDING 1: gopls must remain a candidate when the only lower-layer 'config.json' is a SYMLINK to the write target (not excluded by the hard-link guard)")
	}
}

// FINDING 1 (real hard link STILL blocked) — the os.Link (true hard link) case must still
// be EXCLUDED by the corrected detector, proving the Lstat/regular-file gate did not break
// the genuine hard-link detection it was protecting.
func TestMimoCode_CleanupGuard_F1Corr_RealHardLink_StillBlocked(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	configPath := filepath.Join(dir, "config.json")
	if err := os.Link(writeTargetPath, configPath); err != nil {
		t.Skipf("os.Link (hard link) unsupported on this filesystem: %v", err)
	}
	o := &mimoCodeClient{path: writeTargetPath}

	hardLinked, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines("gopls")
	if err != nil {
		t.Fatalf("mimoCodeLowerLayerHardLinkedToWriteTargetDefines: %v", err)
	}
	if !hardLinked {
		t.Fatalf("FINDING 1: a REAL hard link (os.Link, two regular entries one inode) must STILL be detected (the Lstat/regular gate must not break true hard-link detection)")
	}
	if removableCandidateNames(t, o)["gopls"] {
		t.Fatalf("FINDING 1: a real hard-linked config.json defining gopls must still EXCLUDE the candidate")
	}
}

// FINDING 2 (stdio-LSP) — a write-target mcp-language-server {enabled:false} that a HIGHER
// mimocode.jsonc overlay re-enables ({enabled:true}) is the ACTIVE direct LSP. The LSP
// ownership gate (mimoCodeWriteTargetDefinesStdioLSP, no disabled-drop) must judge the
// write target's OWN value as the stdio-LSP shape disabled-or-not, so the register-grain
// candidate method REPORTS it (reaches the workspace recheck), mirroring the gopls case.
func TestMimoCode_CleanupGuard_F2_ReEnabledWriteTargetLSP_IsOwned(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// Write target's OWN value: the LSP command, but DISABLED.
	writeMimoFile(t, writeTargetPath, `{"mcp":{"mls":`+stdioLSDisabled+`}}`)
	// Higher overlay re-enables it (content-less {enabled:true} deep-merges over the
	// write-target command, yielding an effectively-ENABLED merged LSP).
	writeMimoFile(t, filepath.Join(dir, "mimocode.jsonc"), `{"mcp":{"mls":{"enabled":true}}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// The ownership gate (no disabled-drop) must see the write target OWNS the stdio-LSP
	// shape even though its own value is enabled:false.
	owned, err := o.mimoCodeWriteTargetDefinesStdioLSP("mls")
	if err != nil {
		t.Fatalf("mimoCodeWriteTargetDefinesStdioLSP: %v", err)
	}
	if !owned {
		t.Fatalf("FINDING 2: an enabled:false write-target mcp-language-server re-enabled by a higher overlay must still be OWNED by mimoCodeWriteTargetDefinesStdioLSP (no disabled-drop)")
	}

	// The register-grain candidate source must REPORT mls (no managed layer, no hard link;
	// the workspace-scoped survivor is the caller's recheck — the candidate must reach it).
	if !lspCandidateNames(t, o)["mls"] {
		t.Fatalf("FINDING 2: the re-enabled write-target LSP must be a FindStdioLanguageServerCandidatesWriteTargetOwned candidate (reach the workspace recheck), mirroring the gopls case")
	}
}

// FINDING 2 (plain-stdio parity sanity) — the SAME re-enabled-write-target case for a plain
// gopls already worked via mimoCodeWriteTargetDefinesStdio (the parity this finding mirrors
// for the LSP shape). Pinned here so the two gates are demonstrably symmetric.
func TestMimoCode_CleanupGuard_F2_ReEnabledWriteTargetStdio_IsOwned_ParitySanity(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+goplsStdioDisabled+`}}`)
	writeMimoFile(t, filepath.Join(dir, "mimocode.jsonc"), `{"mcp":{"gopls":{"enabled":true}}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	owns, err := o.mimoCodeWriteTargetDefinesStdio("gopls")
	if err != nil {
		t.Fatalf("mimoCodeWriteTargetDefinesStdio: %v", err)
	}
	if !owns {
		t.Fatalf("parity: an enabled:false write-target gopls re-enabled by a higher overlay must be OWNED by mimoCodeWriteTargetDefinesStdio (no disabled-drop)")
	}
	if !removableCandidateNames(t, o)["gopls"] {
		t.Fatalf("parity: the re-enabled write-target gopls must be a RemovableStdioCandidatesWriteTargetOwned candidate")
	}
}

// FINDING 3 (LSP CLI path) — a config.json HARD-LINKED to the write target defining the LSP
// name must be EXCLUDED from the workspace-FREE FindStdioLanguageServerEntries (the `mcphub
// language-server cleanup` source). The merge-based survivor matches the write target by
// inode and would otherwise mis-report it removable; production's temp+rename leaves the
// config.json entry LIVE → false cleanup.
func TestMimoCode_CleanupGuard_F3_HardLinkedLowerLayer_CLILSPExcluded(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"mls":`+stdioLSEnabled+`}}`)
	configPath := filepath.Join(dir, "config.json")
	if err := os.Link(writeTargetPath, configPath); err != nil {
		t.Skipf("os.Link (hard link) unsupported on this filesystem: %v", err)
	}
	o := &mimoCodeClient{path: writeTargetPath}

	// Sanity: without the guard the merge-based survivor reads the hard-link as removable,
	// so the detector is what must flag it.
	hardLinked, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines("mls")
	if err != nil {
		t.Fatalf("mimoCodeLowerLayerHardLinkedToWriteTargetDefines: %v", err)
	}
	if !hardLinked {
		t.Fatalf("the detector must report the hard-linked config.json (defining mls) as a re-emergent lower layer")
	}

	if stdioLSPEntryNames(t, o)["mls"] {
		t.Fatalf("FINDING 3: a hard-linked config.json defining mls must EXCLUDE the entry from the workspace-FREE FindStdioLanguageServerEntries (CLI cleanup) — temp+rename leaves it live")
	}
}

// FINDING 3 (plain-stdio CLI path) — the same hard-link case excludes the entry from the
// workspace-FREE RemovableStdioEntries (the stdio sibling CLI path).
func TestMimoCode_CleanupGuard_F3_HardLinkedLowerLayer_CLIStdioExcluded(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	configPath := filepath.Join(dir, "config.json")
	if err := os.Link(writeTargetPath, configPath); err != nil {
		t.Skipf("os.Link (hard link) unsupported on this filesystem: %v", err)
	}
	o := &mimoCodeClient{path: writeTargetPath}

	if reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("FINDING 3: a hard-linked config.json defining gopls must EXCLUDE the entry from the workspace-FREE RemovableStdioEntries (CLI cleanup) — temp+rename leaves it live")
	}
}

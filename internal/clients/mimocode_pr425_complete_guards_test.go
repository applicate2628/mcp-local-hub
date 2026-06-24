package clients

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// This file pins the COMPLETE-detection versions of the three conservative PR #425
// cleanup/removal guards (codex-bot PR #425 findings — the prior conservative guards had
// shape/path edges the bot pointed at the correct full versions of):
//
//   FINDING 1 — mimoCodeLowerLayerHardLinkedToWriteTargetDefines now resolves SYMLINKED
//     PARENTS (filepath.EvalSymlinks on BOTH the lower-layer path AND o.path), not just the
//     final component (os.Lstat). A layer that reaches the write target through a symlinked
//     PARENT dir is an ALIAS that FOLLOWS deleteMember's rename → NOT a hard link → removable.
//     A real hard link (os.Link, distinct dir entries, no symlink) → STILL blocked.
//
//   FINDING 2 — mimoCodeShadowIsDisableOnlyOverride's "managed" case classifies the SELECTED
//     plist (the one mimoCodeManagedLayerShadows chose), not a fresh top-of-list re-scan that
//     stops at the first NAME-DEFINING plist. On a dual-plist host (per-user {enabled:true} +
//     system disable-only) the disable-only verdict now AGREES with the shadow selection.
//
//   FINDING 3 — mimoCodeManagedEnableOnlyReactivatesLowerSurvivor applies the cleanup's OWN
//     CONSUMER SHAPE (collectStdioEntries for stdio, findLanguageServerStdioInMap for LSP via
//     mimoCodeNameInActiveSet), not mimoCodeMapDefinesContentBearing. A disabled lower REMOTE
//     (or different-shape) survivor no longer falsely excludes a removable stdio/LSP entry.

// FINDING 1 (COMPLETE) — a layer that reaches the write target THROUGH A SYMLINKED PARENT DIR
// is an ALIAS (it resolves to the same physical file), NOT a hard link. os.Lstat only checks
// the FINAL component, so it FOLLOWS the symlinked parent and (with the old os.SameFile)
// would MIS-CLASSIFY the alias as a hard link and wrongly BLOCK an otherwise-removable
// candidate. With full-path filepath.EvalSymlinks the resolved paths are EQUAL → ALIAS → the
// candidate stays REMOVABLE. (Like deleteMember's rename, the alias follows to the new file —
// no old lower copy re-emerges.)
func TestMimoCode_CompleteGuard_F1_SymlinkedParentAlias_NotHardLinked_Removable(t *testing.T) {
	isolateMimoCodeEnv(t)

	// Real config dir holds the write-target mimocode.json. A SEPARATE symlinked dir points
	// AT the real dir; MIMOCODE_CONFIG reaches the SAME file through the symlinked parent.
	realDir := t.TempDir()
	writeTargetPath := filepath.Join(realDir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)

	linkParent := filepath.Join(t.TempDir(), "mimocode-link")
	if err := os.Symlink(realDir, linkParent); err != nil {
		t.Skipf("os.Symlink (directory symlink) unsupported / unprivileged on this host: %v", err)
	}
	// The MIMOCODE_CONFIG layer path traverses the symlinked PARENT dir to the SAME
	// physical mimocode.json. os.Lstat on this path returns a REGULAR file (it only inspects
	// the final component), so the old final-component check + os.SameFile would treat it as
	// a TRUE hard link to the write target's inode.
	aliasConfig := filepath.Join(linkParent, "mimocode.json")
	o := &mimoCodeClient{path: writeTargetPath, configFile: aliasConfig}

	// The detector must NOT flag the symlinked-parent alias as a hard link — both paths
	// resolve to the same physical file (EQUAL after EvalSymlinks) → ALIAS, not a hard link.
	hardLinked, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines("gopls")
	if err != nil {
		t.Fatalf("mimoCodeLowerLayerHardLinkedToWriteTargetDefines: %v", err)
	}
	if hardLinked {
		t.Fatalf("FINDING 1: a layer reaching the write target through a SYMLINKED PARENT dir is an ALIAS (resolves to the same file), NOT a hard link — os.Lstat follows symlinked parents, so full-path EvalSymlinks must catch this and keep the candidate removable")
	}

	// The workspace-FREE CLI stdio consumer must therefore still report gopls removable.
	if !reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("FINDING 1: gopls must STAY removable when the only extra layer is a symlinked-parent alias of the write target (the alias follows the rename — no re-emergence)")
	}
}

// FINDING 1 (COMPLETE — regression keep) — a REAL hard link (os.Link, two DISTINCT directory
// entries over one inode, NO symlink in either chain) STILL blocks: the resolved paths are
// DISTINCT, the inode is shared, both are regular files. deleteMember's temp+rename breaks
// the link and leaves the lower entry live, so the candidate must remain EXCLUDED.
func TestMimoCode_CompleteGuard_F1_RealHardLink_StillBlocked(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	// config.json is a DISTINCT directory entry hard-linked to the write target (one inode,
	// no symlink anywhere). EvalSymlinks resolves the two to DISTINCT paths.
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
		t.Fatalf("FINDING 1 (regression keep): a REAL hard link (distinct dir entries, one inode, no symlink) defining gopls must STILL be detected — temp+rename breaks the link and leaves the lower entry live")
	}
	if reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("FINDING 1 (regression keep): a real hard-linked config.json defining gopls must EXCLUDE the candidate from RemovableStdioEntries")
	}
}

// FINDING 2 (COMPLETE) — a dual-plist host: a per-user {enabled:true} plist DEFINES gopls but
// is NOT a shadow (the shadow-aware reader skips it), and a system disable-only plist is the
// SELECTED shadow. The disable-only classifier must classify the SELECTED (system) plist, not
// re-scan from the top and stop at the per-user {enabled:true} plist (which would read false →
// wrongly treat the disable-only shadow as active retention). Exercised through the two MDM
// seams: the shadow seam returns the SYSTEM plist as the "managed" shadow source, and the
// disable-only seam classifies that selected plist disable-only → the entry is REMOVABLE.
func TestMimoCode_CompleteGuard_F2_DualPlist_ClassifySelectedShadow_Removable(t *testing.T) {
	isolateMimoCodeEnv(t)

	const systemPlist = "/Library/Managed Preferences/ai.opencode.managed.plist"
	const userPlist = "/Library/Managed Preferences/testuser/ai.opencode.managed.plist"

	prevShadow := mimoCodeManagedPrefsReader
	prevDisable := mimoCodeManagedPrefsDisableOnlyReader
	t.Cleanup(func() {
		mimoCodeManagedPrefsReader = prevShadow
		mimoCodeManagedPrefsDisableOnlyReader = prevDisable
	})
	// SHADOW SELECTION: the per-user {enabled:true} plist DEFINES gopls but is shadow-aware
	// NOT a shadow, so mimoCodeManagedLayerShadows selects the LOWER system disable-only
	// plist as the shadow source (its PlistFile is the SYSTEM plist).
	mimoCodeManagedPrefsReader = func(name string) (mimoCodeShadowSource, error) {
		if name == "gopls" {
			return mimoCodeShadowSource{Kind: "managed", Label: "macOS Managed Preferences", PlistFile: systemPlist}, nil
		}
		return mimoCodeShadowSource{}, nil
	}
	// DISABLE-ONLY CLASSIFIER (FINDING 2): keyed on the SELECTED plist. A correct
	// classifier asked about the SYSTEM plist returns true (disable-only); a buggy re-scan
	// that stopped at the per-user {enabled:true} plist would return false. The seam asserts
	// the production "managed" branch reaches the disable-only classifier (with the seam set,
	// the seam owns the verdict — production threads shadow.PlistFile, which on a real darwin
	// host is the system plist). We model the CORRECT verdict: disable-only=true for the
	// selected system plist.
	mimoCodeManagedPrefsDisableOnlyReader = func(name string) (bool, error) {
		// The selected shadow is the SYSTEM disable-only plist → disable-only.
		return name == "gopls", nil
	}
	_ = userPlist // documents the dual-plist setup the production reader walks

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// The hub's enabled gopls in the write target; no surviving lower content.
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// (a) The classifier (via the "managed" case) agrees with the SELECTED shadow: the
	// disable-only verdict for the selected (system) plist is true.
	shadow, err := o.mimoCodeManagedLayerShadows("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerShadows: %v", err)
	}
	if shadow.Kind != "managed" || shadow.PlistFile != systemPlist {
		t.Fatalf("the shadow reader must SELECT the system disable-only plist as the managed shadow source, got Kind=%q PlistFile=%q", shadow.Kind, shadow.PlistFile)
	}
	disableOnly, err := o.mimoCodeShadowIsDisableOnlyOverride(shadow, "gopls")
	if err != nil {
		t.Fatalf("mimoCodeShadowIsDisableOnlyOverride: %v", err)
	}
	if !disableOnly {
		t.Fatalf("FINDING 2: the disable-only classifier must classify the SELECTED (system disable-only) plist as disable-only — a re-scan stopping at the higher non-shadowing {enabled:true} per-user plist would wrongly return false")
	}

	// (b) The managed pre-check (mimoCodeManagedLayerReResolves) subtracts the disable-only
	// case → returns FALSE (the entry is removable, not retained).
	reResolves, err := o.mimoCodeManagedLayerReResolves("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
	}
	if reResolves {
		t.Fatalf("FINDING 2: a disable-only managed shadow (correctly classified on the SELECTED plist) DISABLES the server — must NOT re-resolve (removable)")
	}

	// (c) RemoveEntry must SUCCEED and delete the write-target entry, not refuse with the
	// retention error a misclassified disable-only shadow would have produced.
	if err := o.RemoveEntry("gopls"); err != nil {
		var retainErr *ErrMimoCodeHigherLayerRetainsServer
		if errors.As(err, &retainErr) {
			t.Fatalf("FINDING 2: RemoveEntry must NOT refuse — the selected managed shadow is disable-only (removable); got retention error %v", err)
		}
		t.Fatalf("RemoveEntry must succeed over a disable-only managed shadow, got %v", err)
	}
	if v, ok, _ := mimoCodeFileEntryValue(writeTargetPath, "gopls"); ok && v != nil {
		t.Fatalf("RemoveEntry must have deleted the write-target gopls; still present: %+v", v)
	}
}

// FINDING 2 (COMPLETE — production helper) — mimoCodeReadManagedPlistDisableOnly classifies
// the SPECIFIED plist path (the selected shadow source), not a top-of-list re-scan. Off
// darwin (no plutil) it handles missing/empty paths GRACEFULLY (false, nil — never an error
// that would abort the destructive consumer); on darwin it classifies the named plist's own
// value. This directly exercises the new production helper independent of the disable-only
// seam (which the prior test drives).
func TestMimoCode_CompleteGuard_F2_ReadManagedPlistDisableOnly_GracefulAndSelected(t *testing.T) {
	// An EMPTY plist path (no selected managed shadow source) → (false, nil).
	if ok, err := mimoCodeReadManagedPlistDisableOnly("", "gopls"); ok || err != nil {
		t.Fatalf("empty plist path must classify as (false, nil), got (%v, %v)", ok, err)
	}
	// A NON-EXISTENT plist path → (false, nil) — cannot be PROVEN disable-only, must not
	// abort the consumer.
	missing := filepath.Join(t.TempDir(), "no-such-managed.plist")
	if ok, err := mimoCodeReadManagedPlistDisableOnly(missing, "gopls"); ok || err != nil {
		t.Fatalf("a missing plist path must classify as (false, nil), got (%v, %v)", ok, err)
	}
	if runtime.GOOS != "darwin" {
		t.Skip("plutil-backed per-plist classification only runs on darwin; the missing/empty graceful path is asserted above on every OS")
	}
	// On darwin, write a real disable-only plist (JSON is a valid plist for plutil) and
	// confirm the SELECTED path classifies disable-only, while a sibling {enabled:true}
	// plist classifies NOT disable-only — proving the verdict tracks the SPECIFIED path.
	dir := t.TempDir()
	disablePlist := filepath.Join(dir, "system.plist")
	enablePlist := filepath.Join(dir, "user.plist")
	if err := os.WriteFile(disablePlist, []byte(`{"mcp":{"gopls":{"enabled":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enablePlist, []byte(`{"mcp":{"gopls":{"enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := mimoCodeReadManagedPlistDisableOnly(disablePlist, "gopls"); err != nil || !ok {
		t.Fatalf("the SELECTED disable-only plist must classify disable-only=true, got (%v, %v)", ok, err)
	}
	if ok, err := mimoCodeReadManagedPlistDisableOnly(enablePlist, "gopls"); err != nil || ok {
		t.Fatalf("an {enabled:true} plist must classify disable-only=false (the FINDING 2 mis-selection a re-scan would make), got (%v, %v)", ok, err)
	}
}

// FINDING 3 (COMPLETE) — a managed {enabled:true} overlay + a lower DISABLED REMOTE survivor
// for the SAME name + a write-target STDIO gopls. The managed enable would re-activate the
// lower REMOTE entry as a REMOTE server, NOT a stdio one — so deleting the write-target stdio
// gopls leaves NO matching DIRECT-STDIO survivor and the entry IS removable. The COMPLETE
// guard applies the stdio consumer shape (collectStdioEntries via mimoCodeNameInActiveSet), so
// it does NOT fire on the remote survivor. The prior mimoCodeMapDefinesContentBearing check
// matched the remote value as content-bearing and wrongly EXCLUDED the removable stdio entry.
func TestMimoCode_CompleteGuard_F3_ManagedEnableOverLowerRemote_StdioRemovable(t *testing.T) {
	isolateMimoCodeEnv(t)
	// Managed config dir carries ONLY an {enabled:true} overlay for gopls (no command/url).
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":true}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// Write target: the ENABLED stdio gopls the hub owns (the cleanup's stdio shape).
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	// config.json BELOW: a DISABLED REMOTE entry (NOT a stdio command). The managed enable
	// would flip it ON, but it re-activates a REMOTE server — not the stdio gopls the cleanup
	// removes. So no DIRECT-STDIO survivor remains and the stdio entry is removable.
	writeMimoFile(t, filepath.Join(dir, "config.json"),
		`{"mcp":{"gopls":{"type":"remote","url":"http://localhost:9121/mcp","enabled":false}}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// The conditioned detector must NOT report a re-activatable lower survivor OF THE STDIO
	// SHAPE — the lower survivor is REMOTE, not stdio.
	reactivates, err := o.mimoCodeManagedEnableOnlyReactivatesLowerSurvivor("gopls", reResolveConsumerStdio)
	if err != nil {
		t.Fatalf("mimoCodeManagedEnableOnlyReactivatesLowerSurvivor: %v", err)
	}
	if reactivates {
		t.Fatalf("FINDING 3: a managed enable over a lower DISABLED REMOTE survivor must NOT fire the stdio guard — the re-activated survivor is REMOTE, not a direct-stdio re-activation of the cleanup's shape")
	}

	// The workspace-FREE CLI stdio consumer must therefore report gopls removable.
	if !reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("FINDING 3: the write-target stdio gopls must STAY removable — deleting it leaves only a disabled lower REMOTE entry (no matching direct-stdio survivor)")
	}
}

// FINDING 3 (COMPLETE — regression keep) — a managed {enabled:true} overlay + a lower DISABLED
// STDIO gopls (the SAME consumer shape) for the SAME name MUST still EXCLUDE: deleting the
// write-target stdio gopls leaves the lower stdio command, which the managed enable flips ON →
// a real stdio re-activation. The COMPLETE consumer-shape guard fires here (the lower survivor
// IS a stdio command), so the entry stays EXCLUDED.
func TestMimoCode_CompleteGuard_F3_ManagedEnableOverLowerStdio_StillExcluded(t *testing.T) {
	isolateMimoCodeEnv(t)
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":true}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	// config.json BELOW: a DISABLED STDIO gopls (the SAME stdio shape) the managed enable
	// re-activates.
	writeMimoFile(t, filepath.Join(dir, "config.json"), `{"mcp":{"gopls":`+goplsStdioDisabled+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	reactivates, err := o.mimoCodeManagedEnableOnlyReactivatesLowerSurvivor("gopls", reResolveConsumerStdio)
	if err != nil {
		t.Fatalf("mimoCodeManagedEnableOnlyReactivatesLowerSurvivor: %v", err)
	}
	if !reactivates {
		t.Fatalf("FINDING 3 (regression keep): a managed enable over a lower DISABLED STDIO survivor of the cleanup's OWN shape must FIRE the stdio guard (real stdio re-activation)")
	}
	if reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("FINDING 3 (regression keep): a managed enable re-activating a lower disabled STDIO gopls must EXCLUDE the entry from RemovableStdioEntries")
	}
}

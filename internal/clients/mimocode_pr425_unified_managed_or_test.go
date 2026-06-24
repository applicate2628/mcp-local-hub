package clients

import (
	"errors"
	"path/filepath"
	"testing"
)

// This file pins the bot PR #425 FOLLOW-UP MANAGED-OR SIMPLIFICATION (architect GATE
// REVISE → PATH-B). The prior revision computed an EFFECTIVE-managed-merge (the managed
// value MERGED over the post-removal lower file merge, then a consumer-shape active
// test). The architect ruled that a WRONG ABSTRACTION: it made the managed verdict
// depend on the below-layer/file merge, creating a TWO-OWNER invariant (the RemoveEntry
// managed pre-check vs the post-delete B4 guard) impossible to hold by hand, and gave
// the F3 enable-over-lower feature an irreducible conflict with B4's intended
// below-layer rollback.
//
// mimoCodeManagedLayerReResolves(name) is now a MANAGED-OWN-VALUE-ONLY predicate: it
// reads ONLY the managed layer's own value (mimoCodeManagedLayerShadows + the
// disable-only subtract via mimoCodeShadowIsDisableOnlyOverride — the SAME two readers
// the B4 post-delete guard uses), is CONSUMER-AGNOSTIC, and NEVER reads
// readMergedLayersExcluding. The pre-check and B4 therefore cannot diverge BY
// CONSTRUCTION.
//
//   - F1 (KEPT): a disable-only MDM ("managed" kind) is classified disable-only and the
//     entry is removable. Covered here by the shadow seam (Kind "managed") + the
//     disable-only seam.
//   - The new AGREEMENT test pins the regression-#3 guard: for {managed enable-true
//     overlay + a below-layer config.json full entry} the RemoveEntry pre-check AND the
//     post-delete B4 guard BOTH ALLOW the delete (the below-layer re-emergence is the
//     INTENDED B4 rollback). The dropped effective-merge tests asserted the OPPOSITE
//     (retention via the below-layer merge), which the architect rejected.

// CLAIM 1 (FINDING 1, KEPT) — a disable-only MDM ("managed" kind, via the MDM
// disable-only seam) leaves the entry REMOVABLE and RemoveEntry actually deletes it.
// Before the FINDING 1 fix mimoCodeShadowIsDisableOnlyOverride's default case treated
// every "managed" kind as NOT disable-only, so the RemoveEntry pre-check refused to
// delete a write-target entry the disable-only MDM overlay was actually disabling.
//
// Under the managed-own-value-only predicate this is covered WITHOUT the (now-deleted)
// effective-merge value seam: the shadow seam makes mimoCodeManagedLayerShadows return
// Kind "managed", and the disable-only seam makes mimoCodeShadowIsDisableOnlyOverride
// classify it disable-only → mimoCodeManagedLayerReResolves returns false (removable).
func TestMimoCode_UnifiedManagedOR_MDMDisableOnly_Removable(t *testing.T) {
	isolateMimoCodeEnv(t)

	// Inject an MDM (macOS Managed Preferences) "managed" shadow + classify it
	// disable-only via the two SURVIVING MDM seams (the shadow seam + the disable-only
	// seam) so the path is exercised on any OS (no real /Library plist). The shadow seam
	// makes mimoCodeManagedLayerShadows return Kind "managed"; the disable-only seam
	// makes mimoCodeShadowIsDisableOnlyOverride classify it disable-only.
	prevShadow := mimoCodeManagedPrefsReader
	prevDisable := mimoCodeManagedPrefsDisableOnlyReader
	t.Cleanup(func() {
		mimoCodeManagedPrefsReader = prevShadow
		mimoCodeManagedPrefsDisableOnlyReader = prevDisable
	})
	mimoCodeManagedPrefsReader = func(name string) (mimoCodeShadowSource, error) {
		if name == "gopls" {
			// A bare {enabled:false} overlay IS a shadow (disabling) for the AddEntry
			// guard — mimoCodeMapShadows would return true for it, so the production
			// reader returns a "managed" shadow here.
			return mimoCodeShadowSource{Kind: "managed", Label: "macOS Managed Preferences", PlistFile: "/Library/Managed Preferences/ai.opencode.managed.plist"}, nil
		}
		return mimoCodeShadowSource{}, nil
	}
	mimoCodeManagedPrefsDisableOnlyReader = func(name string) (bool, error) {
		return name == "gopls", nil
	}

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// The content-bearing gopls is in the write target; no surviving lower content.
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// (1a) the managed-own-value-only predicate must report FALSE (disable-only MDM →
	// retains no active server → removable).
	reResolves, err := o.mimoCodeManagedLayerReResolves("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
	}
	if reResolves {
		t.Fatalf("a disable-only MDM overlay DISABLES the server — must NOT re-resolve (removable); FINDING 1 regression")
	}

	// (1b) RemovableStdioEntries must report gopls (removable).
	if !reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("RemovableStdioEntries must report gopls when the only managed override is a disable-only MDM overlay")
	}

	// (1c) RemoveEntry must SUCCEED and actually delete the write-target entry (not
	// refuse with the retention error the regression produced).
	if err := o.RemoveEntry("gopls"); err != nil {
		t.Fatalf("RemoveEntry must succeed over a disable-only MDM overlay (FINDING 1 regression fix), got %v", err)
	}
	if v, ok, _ := mimoCodeFileEntryValue(writeTargetPath, "gopls"); ok && v != nil {
		t.Fatalf("RemoveEntry must have deleted the write-target gopls; still present: %+v", v)
	}
}

// AGREEMENT (regression-#3 guard) — for {managed enable-true overlay + a below-layer
// config.json full entry}, the RemoveEntry managed PRE-CHECK and the post-delete B4
// GUARD must AGREE that the delete is ALLOWED. This is the case the architect ruled the
// effective-merge revision got WRONG: that revision read the below-layer merge into the
// managed verdict and RETAINED the entry (edge #3, the F3 feature's irreducible conflict
// with B4). The managed-own-value-only predicate instead reports the managed layer
// retains nothing on its own (an enable-true overlay is Kind==""), so the below-layer
// config.json re-emergence is the INTENDED B4 rollback and the delete succeeds.
func TestMimoCode_UnifiedManagedOR_PreCheckAndB4Agree_EnableTrueOverBelowLayerFull(t *testing.T) {
	isolateMimoCodeEnv(t)
	// Managed config dir carries ONLY an {enabled:true} overlay for gopls (no
	// command/url) — correctly NOT a shadow.
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":true}}}`)

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// Write target: an enabled stdio gopls (the entry the hub owns and will delete).
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	// config.json BELOW the write target: a content-bearing full entry that SURVIVES the
	// write-target removal. The managed {enabled:true} overlay would re-activate it —
	// but that re-emergence lands on the BELOW-target (intended-rollback) layer.
	writeMimoFile(t, filepath.Join(dir, "config.json"),
		`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp"],"enabled":false}}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// (a) The managed PRE-CHECK owner (mimoCodeManagedLayerReResolves) must report FALSE:
	// the managed layer (an enable-only:true overlay) retains nothing ON ITS OWN. It does
	// NOT read the below-layer merge, so the surviving config.json entry does not flip
	// the verdict.
	preCheckRetains, err := o.mimoCodeManagedLayerReResolves("gopls")
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
	}
	if preCheckRetains {
		t.Fatalf("the managed pre-check must ALLOW the delete (an enable-true managed overlay retains nothing on its own); regression-#3")
	}

	// (b) The post-delete B4 guard's owners must AGREE: mimoCodeHigherLayerDefining
	// returns Kind "" (the managed enable-true is not a shadow, and config.json is BELOW
	// the write target so it is excluded), so B4 also ALLOWS the delete. This is the
	// structural guarantee — both verdicts flow from the SAME shadow reader.
	hld, err := o.mimoCodeHigherLayerDefining("gopls")
	if err != nil {
		t.Fatalf("mimoCodeHigherLayerDefining: %v", err)
	}
	if hld.Kind != "" {
		t.Fatalf("the B4 guard must ALLOW the delete (no winning higher layer; config.json is a below-target survivor), got Kind=%q", hld.Kind)
	}

	// (c) RemoveEntry must therefore SUCCEED and delete the write-target entry — neither
	// the pre-check nor the B4 post-delete guard refuses. (The server re-emerges from
	// config.json, the operator's own below-layer entry — the intended B4 rollback.)
	if err := o.RemoveEntry("gopls"); err != nil {
		var retainErr *ErrMimoCodeHigherLayerRetainsServer
		if errors.As(err, &retainErr) {
			t.Fatalf("RemoveEntry must NOT refuse: pre-check and B4 AGREE the delete is allowed (below-layer re-emergence is intended rollback), got retention error %v", err)
		}
		t.Fatalf("RemoveEntry must succeed, got %v", err)
	}
	if v, ok, _ := mimoCodeFileEntryValue(writeTargetPath, "gopls"); ok && v != nil {
		t.Fatalf("RemoveEntry must have deleted the write-target gopls; still present: %+v", v)
	}
	// The below-layer config.json entry is untouched (no data loss) — RemoveEntry only
	// edits the write target.
	if _, ok, _ := mimoCodeFileEntryValue(filepath.Join(dir, "config.json"), "gopls"); !ok {
		t.Fatalf("RemoveEntry must NOT touch the below-layer config.json gopls (no data loss); it is gone")
	}
}

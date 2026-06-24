package clients

import (
	"path/filepath"
	"testing"
)

// This file pins the bot PR #425 FOLLOW-UP UNIFIED MANAGED-OR REWORK (architect GATE
// PASS): mimoCodeManagedLayerReResolves(name, consumer) now computes the EFFECTIVE
// managed-layer merge (MDM plist value ⊕ managed-config-dir value, MDM-wins, via
// mimoCodeMergeMCPEntry) overlaid on the post-removal lower merge, then tests
// CONSUMER-SHAPE active membership. The three findings dissolve into that one
// predicate:
//   - FINDING 1: a disable-only MDM ("managed" kind) is classified disable-only and
//     the entry is removable (was a regression — the prior "managed" default→false in
//     mimoCodeShadowIsDisableOnlyOverride wrongly refused the delete).
//   - FINDING 3: a managed enable overlay over a REMOTE lower survivor is NOT
//     stdio-active for the register stdio consumer → removable (the consumer-blind
//     coarse content-bearing test wrongly retained it).
//   - FINDING 4: MDM {enabled:true} ⊕ managed-config-dir {enabled:false} re-enables to
//     active (over a surviving lower command), so it re-resolves true (the prior
//     first-shadow short-circuit classified the config-dir shadow disable-only in
//     ISOLATION and wrongly reported removable).

// CLAIM 1 (FINDING 1) — a disable-only MDM ("managed" kind, via the MDM disable-only
// seam) leaves the entry REMOVABLE and RemoveEntry actually deletes it. Before the
// fix mimoCodeShadowIsDisableOnlyOverride's default case treated every "managed" kind
// as NOT disable-only, so the RemoveEntry pre-check refused to delete a write-target
// entry the disable-only MDM overlay was actually disabling.
func TestMimoCode_UnifiedManagedOR_MDMDisableOnly_Removable(t *testing.T) {
	isolateMimoCodeEnv(t)

	// Inject an MDM (macOS Managed Preferences) "managed" shadow + a disable-only MDM
	// value via the two MDM seams so the path is exercised on any OS (no real /Library
	// plist). The shadow seam makes mimoCodeManagedLayerShadows return Kind "managed";
	// the disable-only seam makes mimoCodeShadowIsDisableOnlyOverride classify it
	// disable-only. The VALUE seam supplies the bare {enabled:false} so the effective
	// merge resolves disabled.
	prevShadow := mimoCodeManagedPrefsReader
	prevValue := mimoCodeManagedPrefsValueReader
	prevDisable := mimoCodeManagedPrefsDisableOnlyReader
	t.Cleanup(func() {
		mimoCodeManagedPrefsReader = prevShadow
		mimoCodeManagedPrefsValueReader = prevValue
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
	mimoCodeManagedPrefsValueReader = func(name string) (map[string]any, bool, error) {
		if name == "gopls" {
			return map[string]any{"enabled": false}, true, nil
		}
		return nil, false, nil
	}
	mimoCodeManagedPrefsDisableOnlyReader = func(name string) (bool, error) {
		return name == "gopls", nil
	}

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// The content-bearing gopls is in the write target; no surviving lower content.
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// (1a) the unified managed-OR predicate must report FALSE (disable-only MDM → not
	// active → removable) for the broadest semantic.
	reResolves, err := o.mimoCodeManagedLayerReResolves("gopls", reResolveConsumerAny)
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

// CLAIM 3 (FINDING 3) — a managed {enabled:true} overlay over a REMOTE lower survivor
// + a write-target stdio gopls → the stdio register-grain candidate source REMOVES it
// (the re-emergent value is an enabled REMOTE entry, NOT stdio-active, so the stdio
// cleanup leaves no duplicate). The TWIN: a STDIO lower survivor re-activates a stdio
// entry → retained. The coarse content-bearing test the prior predicate used would
// have retained BOTH; only the consumer-shape test distinguishes them.
func TestMimoCode_UnifiedManagedOR_ManagedEnableOverRemoteLower_StdioRemovable(t *testing.T) {
	cases := []struct {
		name        string
		lower       string // config.json value BELOW the write target (the survivor)
		wantRemove  bool   // does RemovableStdioCandidatesWriteTargetOwned offer gopls?
		description string
	}{
		{
			name:        "remote lower survivor -> stdio candidate REMOVED",
			lower:       `{"type":"remote","url":"http://localhost:9121/mcp","enabled":false}`,
			wantRemove:  true,
			description: "a remote re-emergence is not stdio-active, so deleting the write-target stdio leaves no stdio duplicate",
		},
		{
			name:        "stdio lower survivor -> stdio candidate RETAINED",
			lower:       `{"type":"local","command":["gopls","mcp"],"enabled":false}`,
			wantRemove:  false,
			description: "a stdio re-emergence IS stdio-active, so the managed enable overlay re-activates a stdio duplicate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateMimoCodeEnv(t)
			// Managed config dir carries ONLY an {enabled:true} overlay for gopls.
			seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":true}}}`)

			dir := t.TempDir()
			writeTargetPath := filepath.Join(dir, "mimocode.json")
			// Write target: an ENABLED stdio gopls (the removable candidate; passes
			// branch (a) write-target-owns-stdio).
			writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
			// config.json BELOW: a DISABLED full entry that survives the write-target
			// removal and is re-enabled by the managed enable-true overlay. Its SHAPE
			// (remote vs stdio) drives the consumer-shape verdict.
			writeMimoFile(t, filepath.Join(dir, "config.json"), `{"mcp":{"gopls":`+tc.lower+`}}`)
			o := &mimoCodeClient{path: writeTargetPath}

			// The STDIO register-grain candidate source (branch (a) + managed-only,
			// reResolveConsumerStdio).
			cands, err := o.RemovableStdioCandidatesWriteTargetOwned()
			if err != nil {
				t.Fatalf("RemovableStdioCandidatesWriteTargetOwned: %v", err)
			}
			offered := false
			for _, e := range cands {
				if e.Name == "gopls" {
					offered = true
				}
			}
			if offered != tc.wantRemove {
				t.Fatalf("RemovableStdioCandidatesWriteTargetOwned offered gopls = %v, want %v (%s)", offered, tc.wantRemove, tc.description)
			}

			// Cross-check the managed-only predicate with the stdio consumer directly.
			managedRetains, err := o.mimoCodeManagedLayerReResolves("gopls", reResolveConsumerStdio)
			if err != nil {
				t.Fatalf("mimoCodeManagedLayerReResolves(stdio): %v", err)
			}
			// retains == !removable for the stdio consumer.
			if managedRetains == tc.wantRemove {
				t.Fatalf("mimoCodeManagedLayerReResolves(stdio)=%v but wantRemove=%v (%s)", managedRetains, tc.wantRemove, tc.description)
			}
		})
	}
}

// CLAIM 4 (FINDING 4) — MDM {enabled:true} ⊕ managed-config-dir {enabled:false} over a
// surviving lower command re-enables to ACTIVE, so the unified predicate re-resolves
// true (NOT removable). The prior first-shadow short-circuit returned the config-dir
// shadow (disabling) and, classifying it disable-only IN ISOLATION, wrongly reported
// removable — it never saw the higher MDM enable that re-enables the effective merge.
func TestMimoCode_UnifiedManagedOR_MDMEnableOverConfigDirDisable_Retained(t *testing.T) {
	isolateMimoCodeEnv(t)

	// managed-config-dir carries a DISABLING {enabled:false} overlay for gopls (the
	// layer the first-shadow walk would short-circuit on).
	seedManagedConfigDir(t, `{"mcp":{"gopls":{"enabled":false}}}`)

	// MDM (higher than the config dir) carries an ENABLE-ONLY {enabled:true} overlay
	// for gopls — injected via the VALUE seam so the effective merge MDM⊕config-dir
	// resolves to {enabled:true}. The shadow seam returns NO managed shadow (an
	// enable-only:true MDM overlay is correctly not a shadow), so the first-shadow path
	// would have stopped at the config-dir disable and missed this re-enable.
	prevShadow := mimoCodeManagedPrefsReader
	prevValue := mimoCodeManagedPrefsValueReader
	t.Cleanup(func() {
		mimoCodeManagedPrefsReader = prevShadow
		mimoCodeManagedPrefsValueReader = prevValue
	})
	mimoCodeManagedPrefsReader = func(string) (mimoCodeShadowSource, error) {
		return mimoCodeShadowSource{}, nil // MDM enable-true is not a shadow
	}
	mimoCodeManagedPrefsValueReader = func(name string) (map[string]any, bool, error) {
		if name == "gopls" {
			return map[string]any{"enabled": true}, true, nil
		}
		return nil, false, nil
	}

	dir := t.TempDir()
	writeTargetPath := filepath.Join(dir, "mimocode.json")
	// Write target: an enabled stdio gopls (the candidate the cleanup would try to
	// remove).
	writeMimoFile(t, writeTargetPath, `{"mcp":{"gopls":`+followupStdioGopls+`}}`)
	// config.json BELOW: a DISABLED full stdio gopls (the surviving lower COMMAND the
	// effective enable-true overlay re-activates after the write-target key is gone).
	writeMimoFile(t, filepath.Join(dir, "config.json"),
		`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp"],"enabled":false}}}`)
	o := &mimoCodeClient{path: writeTargetPath}

	// The unified predicate must RE-RESOLVE (retain) for the broadest semantic: the
	// EFFECTIVE managed value (config-dir {enabled:false} ⊕ MDM {enabled:true}) =
	// {enabled:true}, which re-enables the surviving lower gopls command → active.
	reResolves, err := o.mimoCodeManagedLayerReResolves("gopls", reResolveConsumerAny)
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves: %v", err)
	}
	if !reResolves {
		t.Fatalf("MDM {enabled:true} ⊕ config-dir {enabled:false} over a surviving lower command RE-ENABLES the server — must re-resolve (retained); FINDING 4 (the first-shadow short-circuit missed the MDM re-enable)")
	}

	// And the stdio register grain agrees (the re-activated value is a stdio gopls).
	reResolvesStdio, err := o.mimoCodeManagedLayerReResolves("gopls", reResolveConsumerStdio)
	if err != nil {
		t.Fatalf("mimoCodeManagedLayerReResolves(stdio): %v", err)
	}
	if !reResolvesStdio {
		t.Fatalf("the re-enabled effective value is a stdio gopls — the stdio consumer must also retain it")
	}

	// The conservative RemovableStdioEntries must DECLINE gopls (not removable).
	if reResolveRemovableNames(t, o)["gopls"] {
		t.Fatalf("gopls must NOT be removable when the effective managed merge re-enables it (FINDING 4)")
	}
}

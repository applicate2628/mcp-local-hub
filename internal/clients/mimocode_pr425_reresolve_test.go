package clients

import (
	"os"
	"path/filepath"
	"testing"
)

// This file pins the bot PR #425 re-resolve REDESIGN (architect GATE PASS): the
// destructive-cleanup re-resolve predicate
// (mimoCodeNameReResolvesAfterWriteTargetRemoval) now re-runs the REAL merge with
// the write-target's own mcp.<name> key excluded (readMergedLayersExcluding) and
// tests post-removal ACTIVE membership with the SAME survivor filter the consumer
// used to build the candidate — replacing the prior hand-walked per-layer
// re-derivation. The tests cover both destructive consumers (RemovableStdioEntries
// = stdio active set; FindStdioLanguageServerEntries = mcp-language-server active
// set), the three newly-flagged layer-merge edges, the regression-keeps, the
// parse-error abort, and the no-live-map-mutation invariant (architect claim 2).

// reResolveRemovableNames returns the RemovableStdioEntries name set.
func reResolveRemovableNames(t *testing.T, o *mimoCodeClient) map[string]bool {
	t.Helper()
	entries, err := o.RemovableStdioEntries()
	if err != nil {
		t.Fatalf("RemovableStdioEntries: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name] = true
	}
	return got
}

// TestMimoCode_ReResolve_EdgeTable pins the three newly-flagged layer-merge edges
// the prior per-layer re-derivation got wrong, plus the architect-listed
// regression-keeps, through the real destructive consumers.
func TestMimoCode_ReResolve_EdgeTable(t *testing.T) {
	const stdioGopls = `{"type":"local","command":["gopls","mcp"],"enabled":true}`

	t.Run("edge 2777: config.json {enabled:false} re-enabled by a higher enabled-only overlay -> re-resolves (blocks removal)", func(t *testing.T) {
		// The active gopls is in CONFIG.JSON (below the write target), DISABLED in its
		// own value, but RE-ENABLED by a higher enabled-only overlay ({enabled:true},
		// no command). The write target does NOT define gopls. Removing the
		// write-target key cannot clear the config.json/overlay-sourced effective
		// gopls — it must DECLINE. This is the enabled-only-overlay merge-input
		// interaction the prior post-hoc per-layer walk mishandled: the predicate must
		// drop the write-target key PRE-FOLD (which here changes nothing — gopls is
		// not in the write target) and observe that config.json + the overlay still
		// merge to an ACTIVE gopls.
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "config.json"),
			`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp"],"enabled":false}}}`)
		writeMimoFile(t, filepath.Join(dir, "mimocode.jsonc"),
			`{"mcp":{"gopls":{"enabled":true}}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		// gopls is effectively ACTIVE in the merged view.
		reResolves, err := o.mimoCodeNameReResolvesAfterWriteTargetRemoval("gopls", reResolveConsumerStdio)
		if err != nil {
			t.Fatalf("predicate: %v", err)
		}
		if !reResolves {
			t.Fatalf("a config.json gopls re-enabled by a higher enabled-only overlay re-resolves active after a write-target removal — must block removal (re-resolves=true)")
		}
	})

	t.Run("edge 2777 via write target: write-target {enabled:false} re-enabled by higher overlay -> does NOT re-resolve (removable)", func(t *testing.T) {
		// The WRITE TARGET carries the command but is disabled; a higher enabled-only
		// overlay re-enables it. The merged effective view is an ACTIVE gopls (the
		// write target supplies the command, the overlay flips it on). Removing the
		// write-target key leaves the overlay with no lower entry to overlay onto → it
		// stays a content-less inert stub with NO command → does NOT re-resolve → the
		// active direct gopls IS removable. This is the bot PR #425 finding 2 case,
		// and the reason the predicate must drop the write-target key PRE-FOLD on a
		// COPY (post-merge subtraction would keep the overlaid active entry).
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp"],"enabled":false}}}`)
		writeMimoFile(t, filepath.Join(dir, "mimocode.jsonc"),
			`{"mcp":{"gopls":{"enabled":true}}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		reResolves, err := o.mimoCodeNameReResolvesAfterWriteTargetRemoval("gopls", reResolveConsumerStdio)
		if err != nil {
			t.Fatalf("predicate: %v", err)
		}
		if reResolves {
			t.Fatalf("a write-target gopls re-enabled by an enabled-only overlay goes inert once the write-target key is gone — must NOT re-resolve (removable)")
		}
		// And the consumer reports it removable.
		if !reResolveRemovableNames(t, o)["gopls"] {
			t.Fatalf("RemovableStdioEntries must report the re-enabled write-target gopls")
		}
	})

	t.Run("edge 3162: content-less {enabled:true} stub (no command) -> does NOT re-resolve (removable)", func(t *testing.T) {
		// The write target has the ACTIVE direct gopls; a HIGHER layer carries only a
		// content-less {enabled:true} stub (no command/url). After the write-target
		// key is removed, the merged gopls = the bare stub, which collectStdioEntries
		// excludes (it requires a non-empty command, clients.go) → does NOT re-resolve
		// → the active gopls IS removable.
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"gopls":`+stdioGopls+`}}`)
		writeMimoFile(t, filepath.Join(dir, "mimocode.jsonc"),
			`{"mcp":{"gopls":{"enabled":true}}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		reResolves, err := o.mimoCodeNameReResolvesAfterWriteTargetRemoval("gopls", reResolveConsumerStdio)
		if err != nil {
			t.Fatalf("predicate: %v", err)
		}
		if reResolves {
			t.Fatalf("a content-less {enabled:true} stub has no command after the write-target removal — must NOT re-resolve (removable)")
		}
		if !reResolveRemovableNames(t, o)["gopls"] {
			t.Fatalf("RemovableStdioEntries must report gopls when the only higher layer is a content-less enabled-only stub")
		}
	})

	t.Run("edge 2799: enabled claude import + an explicit (disabled) higher layer -> precedence honored", func(t *testing.T) {
		// ~/.claude.json defines gopls ENABLED. A higher mimocode.jsonc layer ALSO
		// defines gopls but DISABLED ({type,command,enabled:false}) — an EXPLICIT
		// layer that wins over the import by skip-if-name-exists (the import only adds
		// names not already defined by any layer above; the explicit mimo layer
		// defines it, so the import is skipped). The write target also has the active
		// gopls. After removing the write-target key, the explicit higher DISABLED
		// entry still defines gopls → skip-if-name-exists STILL skips the import → the
		// merged gopls = the higher DISABLED entry → mimoCodeDropDisabled drops it →
		// does NOT re-resolve active → gopls IS removable. The predicate honors the
		// real precedence (explicit layer decides; the enabled import is suppressed),
		// which a per-layer "import is enabled => counts" walk would have gotten wrong.
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "")
		home := t.TempDir()
		globalDir := filepath.Join(home, ".config", "mimocode")
		if err := os.MkdirAll(globalDir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeMimoFile(t, filepath.Join(home, ".claude.json"),
			`{"mcpServers":{"gopls":{"command":"gopls","args":["mcp"]}}}`)
		writeMimoFile(t, filepath.Join(globalDir, "mimocode.jsonc"),
			`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp"],"enabled":false}}}`)
		writeMimoFile(t, filepath.Join(globalDir, "mimocode.json"),
			`{"mcp":{"gopls":`+stdioGopls+`}}`)
		o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), claudeHome: home}
		reResolves, err := o.mimoCodeNameReResolvesAfterWriteTargetRemoval("gopls", reResolveConsumerStdio)
		if err != nil {
			t.Fatalf("predicate: %v", err)
		}
		if reResolves {
			t.Fatalf("the explicit DISABLED higher layer suppresses the enabled import via skip-if-name-exists and is itself disabled — gopls must NOT re-resolve active (removable)")
		}
	})

	t.Run("regression: disabled-lower does not block; enabled-lower blocks", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"gopls":`+stdioGopls+`}}`)
		// Disabled lower: dropped → does not block.
		writeMimoFile(t, filepath.Join(dir, "config.json"),
			`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp"],"enabled":false}}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		if r, _ := o.mimoCodeNameReResolvesAfterWriteTargetRemoval("gopls", reResolveConsumerStdio); r {
			t.Fatalf("a DISABLED config.json-below entry must not block removal")
		}
		// Enabled lower: re-emerges active → blocks.
		writeMimoFile(t, filepath.Join(dir, "config.json"),
			`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp"],"enabled":true}}}`)
		if r, _ := o.mimoCodeNameReResolvesAfterWriteTargetRemoval("gopls", reResolveConsumerStdio); !r {
			t.Fatalf("an ENABLED config.json-below entry must block removal")
		}
	})

	t.Run("regression: write-target-only -> removable", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"gopls":`+stdioGopls+`}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		if r, _ := o.mimoCodeNameReResolvesAfterWriteTargetRemoval("gopls", reResolveConsumerStdio); r {
			t.Fatalf("a sole write-target entry must NOT re-resolve (removable)")
		}
		if !reResolveRemovableNames(t, o)["gopls"] {
			t.Fatalf("RemovableStdioEntries must report the sole write-target gopls")
		}
	})

	t.Run("regression: r12 higher-stdio-over-write-target-remote still excluded by branch (a)", func(t *testing.T) {
		// The write-target value for `srv` is REMOTE; a higher mimocode.jsonc layer is
		// stdio. The merged candidate is stdio (higher wins), but branch (a)
		// (mimoCodeWriteTargetDefinesStdio) sees the write target's OWN value is remote
		// (no command) → declines BEFORE the predicate. So `srv` is NOT reported, and
		// RemoveEntry never wrong-deletes the operator-visible remote. Branch (a) is
		// the gate that owns this; the predicate is not even consulted.
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.jsonc"),
			`{"mcp":{"srv":{"type":"local","command":["srv","mcp"],"enabled":true}}}`)
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"srv":{"type":"remote","url":"http://localhost:9121/mcp","enabled":true}}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		// Branch (a): the write-target value is NOT stdio.
		ownsStdio, err := o.mimoCodeWriteTargetDefinesStdio("srv")
		if err != nil {
			t.Fatalf("mimoCodeWriteTargetDefinesStdio: %v", err)
		}
		if ownsStdio {
			t.Fatalf("the write-target value for `srv` is REMOTE — branch (a) must NOT consider it write-target-owned stdio")
		}
		// The consumer therefore does not report it.
		if reResolveRemovableNames(t, o)["srv"] {
			t.Fatalf("`srv` is REMOTE in the write target — branch (a) must exclude it from RemovableStdioEntries (no wrong-delete)")
		}
	})
}

// TestMimoCode_ReResolve_ProductionChainReachesRealMethod confirms the predicate
// is reached through the public destructive consumer (not just the private
// helper), pinning that the redesigned merge-based path is the one production
// runs.
func TestMimoCode_ReResolve_ProductionChainReachesRealMethod(t *testing.T) {
	const stdioLS = `{"type":"local","command":["mcp-language-server","--lsp","go"],"enabled":true}`
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	// Write target sole stdio-LSP entry → reported; an enabled lower duplicate →
	// declined. Both go through FindStdioLanguageServerEntries → the predicate.
	writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
		`{"mcp":{"sole":`+stdioLS+`,"dup":`+stdioLS+`}}`)
	writeMimoFile(t, filepath.Join(dir, "config.json"),
		`{"mcp":{"dup":`+stdioLS+`}}`)
	o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
	got := stdioLSPEntryNames(t, o)
	if !got["sole"] {
		t.Fatalf("sole write-target stdio-LSP must be reported through the production FindStdioLanguageServerEntries chain")
	}
	if got["dup"] {
		t.Fatalf("an enabled LSP-shaped config.json-below `dup` re-resolves active — must be declined through the production chain")
	}
}

// TestMimoCode_ReResolve_MalformedLayerAborts pins architect claim 2: a malformed
// non-import layer (config.json / mimocode.jsonc) makes the predicate AND the
// destructive consumers return an error and delete NOTHING (the parse error
// propagates through readMergedLayersExcluding). A malformed layer must never be
// silently read as "does not define".
func TestMimoCode_ReResolve_MalformedLayerAborts(t *testing.T) {
	const stdioLS = `{"type":"local","command":["mcp-language-server","--lsp","go"],"enabled":true}`

	for _, tc := range []struct {
		name     string
		badLayer string
		badBody  string
	}{
		{"malformed config.json", "config.json", `{ this is not json`},
		{"malformed mimocode.jsonc", "mimocode.jsonc", `{ "mcp": { broken`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateMimoCodeEnv(t)
			dir := t.TempDir()
			writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
				`{"mcp":{"gopls":`+stdioLS+`}}`)
			writeMimoFile(t, filepath.Join(dir, tc.badLayer), tc.badBody)
			o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}

			// Predicate aborts.
			if _, err := o.mimoCodeNameReResolvesAfterWriteTargetRemoval("gopls", reResolveConsumerStdio); err == nil {
				t.Fatalf("a malformed %s must make the predicate return an error (abort, delete nothing)", tc.badLayer)
			}
			// Both destructive consumers abort.
			if _, err := o.RemovableStdioEntries(); err == nil {
				t.Fatalf("a malformed %s must make RemovableStdioEntries return an error (delete nothing)", tc.badLayer)
			}
			if _, err := o.FindStdioLanguageServerEntries(); err == nil {
				t.Fatalf("a malformed %s must make FindStdioLanguageServerEntries return an error (delete nothing)", tc.badLayer)
			}
		})
	}
}

// TestMimoCode_ReResolve_NoLiveMapMutation pins architect claim 2's
// no-live-mutation proof: running the re-resolve predicate (which excludes the
// write-target key from a COPY of that layer's mcp map) must NOT mutate the live
// parsed layer maps. A second readMergedLayers() AFTER the predicate must return
// the UNMODIFIED merged view (the excluded name still present, identical to a
// fresh read).
func TestMimoCode_ReResolve_NoLiveMapMutation(t *testing.T) {
	const stdioLS = `{"type":"local","command":["mcp-language-server","--lsp","go"],"enabled":true}`
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	// Write target defines gopls; config.json below also defines it. The predicate
	// will exclude gopls from the write-target layer copy.
	writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
		`{"mcp":{"gopls":`+stdioLS+`,"keep":`+stdioLS+`}}`)
	writeMimoFile(t, filepath.Join(dir, "config.json"),
		`{"mcp":{"gopls":{"type":"local","command":["gopls","mcp"],"enabled":true}}}`)
	o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}

	// Baseline merged view BEFORE the predicate.
	before, err := o.readMergedLayers()
	if err != nil {
		t.Fatalf("readMergedLayers before: %v", err)
	}
	beforeServers, _ := before[mimoCodeMCPKey].(map[string]any)
	if _, ok := beforeServers["gopls"]; !ok {
		t.Fatalf("baseline merged view must contain gopls")
	}

	// Run the excluded merge (this is what the predicate calls). It must NOT touch
	// the live parsed layer maps.
	excluded, err := o.readMergedLayersExcluding("gopls")
	if err != nil {
		t.Fatalf("readMergedLayersExcluding: %v", err)
	}
	exclServers, _ := excluded[mimoCodeMCPKey].(map[string]any)
	// In the excluded view the write-target gopls is gone, but config.json's gopls
	// re-emerges (enabled) — so gopls is still present, sourced from below.
	if _, ok := exclServers["gopls"]; !ok {
		t.Fatalf("excluded view must still show gopls (config.json below re-emerges it)")
	}

	// Also run the predicate itself for completeness.
	if _, err := o.mimoCodeNameReResolvesAfterWriteTargetRemoval("gopls", reResolveConsumerLSP); err != nil {
		t.Fatalf("predicate: %v", err)
	}

	// A SECOND readMergedLayers() AFTER the predicate must return the UNMODIFIED
	// merged view — gopls AND keep both present, identical to before. If the
	// predicate had mutated a live layer map (in-place delete), gopls would be
	// missing here.
	after, err := o.readMergedLayers()
	if err != nil {
		t.Fatalf("readMergedLayers after: %v", err)
	}
	afterServers, _ := after[mimoCodeMCPKey].(map[string]any)
	if _, ok := afterServers["gopls"]; !ok {
		t.Fatalf("LIVE-MAP MUTATION DETECTED: gopls is missing from the merged view after the predicate ran — the exclude must operate on a COPY, never mutate live layer maps")
	}
	if _, ok := afterServers["keep"]; !ok {
		t.Fatalf("the unrelated `keep` entry must survive the second merged read unchanged")
	}
	if len(afterServers) != len(beforeServers) {
		t.Fatalf("merged server set changed after the predicate ran: before=%d after=%d (live-map mutation)", len(beforeServers), len(afterServers))
	}
}

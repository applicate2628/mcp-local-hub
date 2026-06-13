package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/api/daemon_env_overlay"
)

// seedConfigEnvInstalledManifests seeds a hermetic installed-server catalog via
// MCPHUB_MANIFEST_DIR_OVERRIDE so the longest-installed-prefix disambiguator in
// resolveConfigEnvTargets sees EXACTLY the named servers (no leakage from the
// binary's embedded shipped set). Mirrors the api-side seedInstalledServerManifests,
// which is not visible to package cli.
func seedConfigEnvInstalledManifests(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatalf("mkdir manifest %q: %v", name, err)
		}
		body := "name: " + name + "\nkind: global\ntransport: stdio-bridge\ncommand: go\n"
		if err := os.WriteFile(filepath.Join(dir, name, "manifest.yaml"), []byte(body), 0o600); err != nil {
			t.Fatalf("write manifest %q: %v", name, err)
		}
	}
}

// TestResolveConfigEnvTargetsBlankServerHyphenatedDaemonExactMatch is the
// falsifying regression for the blank-Server daemonSelector-arm mis-split in
// resolveConfigEnvTargets (config_env.go).
//
// A blank-Server intent row (Server=="", Daemon=="") derives its (server,
// daemon) identity through api.ParseManagedTaskName, whose last-hyphen split
// misattributes hyphenated daemon names: \mcp-local-hub-demo-alpha-beta (real
// server "demo", real daemon "alpha-beta") parses as server="demo-alpha",
// daemon="beta". Before the fix the daemonSelector arm filtered
// (server==serverSelector && daemon==daemonSelector), so:
//   - `config env set demo/alpha-beta` MISSED the real row (server "demo" !=
//     parsed "demo-alpha"); and
//   - `config env set demo-alpha/beta` WRONGLY claimed it (matched the split).
//
// The fix mirrors the landed sibling family's longest-installed-prefix
// disambiguator (api.blankServerRowOwnedByLongestInstalledPrefix, r33-2): the
// blank-field row's true server is the longest installed-server-name prefix of
// its canonical task portion. With ONLY "demo" installed, "demo" owns the row,
// so demo/alpha-beta selects it and demo-alpha/beta (an uninstalled server name)
// does NOT. This test FAILS pre-fix on the demo/alpha-beta selection (the
// mis-split misses it).
func TestResolveConfigEnvTargetsBlankServerHyphenatedDaemonExactMatch(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	// Only "demo" is installed — NOT "demo-alpha". So "demo" is the longest
	// installed prefix of \mcp-local-hub-demo-alpha-beta and owns the row.
	seedConfigEnvInstalledManifests(t, "demo")
	const realTask = `\mcp-local-hub-demo-alpha-beta`
	// Blank Server AND Daemon — identity must come from the canonical task name
	// + installed catalog, not the ParseManagedTaskName last-hyphen split.
	seedConfigEnvIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: realTask,
		Port:     9123,
	})

	// Positive: real server "demo", hyphenated daemon "alpha-beta" selects the row.
	got, err := resolveConfigEnvTargets(stateDir, "demo/alpha-beta")
	if err != nil {
		t.Fatalf("resolveConfigEnvTargets(demo/alpha-beta): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("demo/alpha-beta matched %d targets, want exactly 1: %+v", len(got), got)
	}
	if got[0].TaskName != realTask {
		t.Fatalf("demo/alpha-beta selected %q, want %q", got[0].TaskName, realTask)
	}

	// Negative control: the mis-split shape demo-alpha/beta must NOT claim the
	// row. Pre-fix it would (ParseManagedTaskName parses the canonical name as
	// server="demo-alpha", daemon="beta"); post-fix demo-alpha is not an
	// installed server, so the longest-installed-prefix disambiguator denies it.
	if mis, err := resolveConfigEnvTargets(stateDir, "demo-alpha/beta"); err != nil {
		t.Fatalf("resolveConfigEnvTargets(demo-alpha/beta): %v", err)
	} else if len(mis) != 0 {
		t.Fatalf("demo-alpha/beta wrongly claimed %d targets, want 0: %+v", len(mis), mis)
	}
}

// TestResolveConfigEnvTargetsBlankServerLongerInstalledSiblingOwnsRow pins the
// other direction of the r31-F1 / r33-2 tension at the config-env layer: when
// BOTH "demo" and "demo-alpha" are installed, "demo-alpha" is the LONGER prefix
// and owns \mcp-local-hub-demo-alpha-beta, so demo-alpha/beta selects it and the
// shorter demo/alpha-beta must NOT claim it.
func TestResolveConfigEnvTargetsBlankServerLongerInstalledSiblingOwnsRow(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	seedConfigEnvInstalledManifests(t, "demo", "demo-alpha")
	const realTask = `\mcp-local-hub-demo-alpha-beta`
	seedConfigEnvIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: realTask,
		Port:     9124,
	})

	// demo-alpha (the longer installed prefix) owns the row.
	got, err := resolveConfigEnvTargets(stateDir, "demo-alpha/beta")
	if err != nil {
		t.Fatalf("resolveConfigEnvTargets(demo-alpha/beta): %v", err)
	}
	if len(got) != 1 || got[0].TaskName != realTask {
		t.Fatalf("demo-alpha/beta = %+v, want single %q", got, realTask)
	}

	// The shorter demo/alpha-beta must NOT claim the row when demo-alpha owns it.
	if mis, err := resolveConfigEnvTargets(stateDir, "demo/alpha-beta"); err != nil {
		t.Fatalf("resolveConfigEnvTargets(demo/alpha-beta): %v", err)
	} else if len(mis) != 0 {
		t.Fatalf("demo/alpha-beta wrongly claimed %d targets when demo-alpha owns row, want 0: %+v", len(mis), mis)
	}
}

// TestRunConfigEnvSetUnsetBlankServerHyphenatedDaemonWritesCorrectRow exercises
// the full write/delete decision path (runConfigEnvSet → resolveSingleConfigEnvTarget
// → resolveConfigEnvTargets) end to end for the hyphenated blank-Server case,
// proving the overlay row that gets written/deleted is keyed on the real
// canonical task name and that the mis-split selector cannot touch it.
func TestRunConfigEnvSetUnsetBlankServerHyphenatedDaemonWritesCorrectRow(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	seedConfigEnvInstalledManifests(t, "demo")
	const realTask = `\mcp-local-hub-demo-alpha-beta`
	seedConfigEnvIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: realTask,
		Port:     9125,
	})
	overlayPath := filepath.Join(stateDir, overlayBaseName)

	// set demo/alpha-beta KEY VAL writes the row keyed on the canonical task name.
	var out bytes.Buffer
	if err := runConfigEnvSet(stateDir, "demo/alpha-beta", "DEMO_FLAG", "on", &out); err != nil {
		t.Fatalf("runConfigEnvSet(demo/alpha-beta): %v", err)
	}
	ov, err := daemon_env_overlay.Load(overlayPath)
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	row, ok := ov.Daemons[realTask]
	if !ok {
		t.Fatalf("overlay row missing for %q: %+v", realTask, ov.Daemons)
	}
	if got := row.Env["DEMO_FLAG"]; got != "on" {
		t.Fatalf("DEMO_FLAG = %q, want on", got)
	}

	// The mis-split selector must error (no daemon matches) rather than silently
	// writing a phantom row.
	out.Reset()
	if err := runConfigEnvSet(stateDir, "demo-alpha/beta", "DEMO_FLAG", "off", &out); err == nil {
		t.Fatal("runConfigEnvSet(demo-alpha/beta) returned nil; want no-match error")
	}
	// Confirm the mis-split set did NOT mutate the real row.
	ov2, err := daemon_env_overlay.Load(overlayPath)
	if err != nil {
		t.Fatalf("reload overlay: %v", err)
	}
	if got := ov2.Daemons[realTask].Env["DEMO_FLAG"]; got != "on" {
		t.Fatalf("real row DEMO_FLAG mutated by mis-split selector: %q, want on", got)
	}

	// unset demo/alpha-beta removes the row through the same correct selection.
	out.Reset()
	if err := runConfigEnvUnset(stateDir, "demo/alpha-beta", "DEMO_FLAG", &out); err != nil {
		t.Fatalf("runConfigEnvUnset(demo/alpha-beta): %v", err)
	}
	ov3, err := daemon_env_overlay.Load(overlayPath)
	if err != nil {
		t.Fatalf("reload overlay after unset: %v", err)
	}
	if _, present := ov3.Daemons[realTask]; present {
		t.Fatalf("row still present after unset of last key: %+v", ov3.Daemons)
	}
}

// TestResolveConfigEnvTargetsPopulatedFieldRowStillPrecise guards the unchanged
// path: a populated-field row (Server!="" && Daemon!="") whose daemon name
// contains a hyphen is selected by exact field compare, and the catalog-aware
// blank-field branch does not interfere. This is the regression guard that the
// fix only touches the blank-field branch — it needs NO installed catalog.
func TestResolveConfigEnvTargetsPopulatedFieldRowStillPrecise(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	const task = `\mcp-local-hub-demo-alpha-beta`
	seedConfigEnvIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: task,
		Server:   "demo",
		Daemon:   "alpha-beta",
		Port:     9126,
	})

	got, err := resolveConfigEnvTargets(stateDir, "demo/alpha-beta")
	if err != nil {
		t.Fatalf("resolveConfigEnvTargets(demo/alpha-beta): %v", err)
	}
	if len(got) != 1 || got[0].TaskName != task {
		t.Fatalf("populated-field demo/alpha-beta = %+v, want single %q", got, task)
	}
	if got[0].Server != "demo" || got[0].Daemon != "alpha-beta" {
		t.Fatalf("populated-field identity = %s/%s, want demo/alpha-beta", got[0].Server, got[0].Daemon)
	}

	// The mis-split shape must not match the populated row either — the populated
	// path uses exact field compare (server "demo" != "demo-alpha").
	if mis, err := resolveConfigEnvTargets(stateDir, "demo-alpha/beta"); err != nil {
		t.Fatalf("resolveConfigEnvTargets(demo-alpha/beta): %v", err)
	} else if len(mis) != 0 {
		t.Fatalf("demo-alpha/beta wrongly matched populated row: %+v", mis)
	}
}

// TestResolveConfigEnvTargetsTaskSelectorArmUnchanged guards the exact/precise
// taskSelector arm: a full canonical task selector still selects the row
// regardless of the blank-field fix and needs no installed catalog.
func TestResolveConfigEnvTargetsTaskSelectorArmUnchanged(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	const task = `\mcp-local-hub-demo-alpha-beta`
	seedConfigEnvIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: task,
		Port:     9127,
	})

	for _, sel := range []string{
		`mcp-local-hub-demo-alpha-beta`,
		`\mcp-local-hub-demo-alpha-beta`,
	} {
		got, err := resolveConfigEnvTargets(stateDir, sel)
		if err != nil {
			t.Fatalf("resolveConfigEnvTargets(%q): %v", sel, err)
		}
		if len(got) != 1 || got[0].TaskName != task {
			t.Fatalf("task selector %q = %+v, want single %q", sel, got, task)
		}
	}
}

// TestBlankServerTaskOwnedByLongestInstalledPrefix is the pure-predicate unit
// test mirroring the api-side TestBlankServerRowOwnedByLongestInstalledPrefix,
// plus the config-env-specific adaptation (uninstalled selector denied when the
// catalog is non-empty; empty catalog claims any prefix-matching row).
func TestBlankServerTaskOwnedByLongestInstalledPrefix(t *testing.T) {
	set := func(names ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, n := range names {
			m[n] = struct{}{}
		}
		return m
	}
	cases := []struct {
		name      string
		taskName  string
		server    string
		installed map[string]struct{}
		want      bool
	}{
		{"demo owns when only demo installed", `\mcp-local-hub-demo-alpha-beta`, "demo", set("demo"), true},
		{"demo-alpha denied when not installed (config-env adaptation)", `\mcp-local-hub-demo-alpha-beta`, "demo-alpha", set("demo"), false},
		{"longer installed sibling owns row, shorter denied", `\mcp-local-hub-demo-alpha-beta`, "demo", set("demo", "demo-alpha"), false},
		{"longer installed prefix claims its own row", `\mcp-local-hub-demo-alpha-beta`, "demo-alpha", set("demo", "demo-alpha"), true},
		{"empty catalog claims any prefix row", `\mcp-local-hub-demo-alpha-beta`, "demo", nil, true},
		{"empty catalog also claims demo-alpha (no sibling proof)", `\mcp-local-hub-demo-alpha-beta`, "demo-alpha", nil, true},
		{"non-prefix server never claims", `\mcp-local-hub-other-d`, "demo", set("demo", "other"), false},
		{"bare server portion, no daemon segment, not claimed", `\mcp-local-hub-demo`, "demo", set("demo"), false},
		{"word-boundary: demonstration not owned by demo", `\mcp-local-hub-demonstration-x`, "demo", set("demo"), false},
		{"bare (non-canonical) task name canonicalizes and claims", `mcp-local-hub-demo-oldname`, "demo", set("demo"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blankServerTaskOwnedByLongestInstalledPrefix(tc.taskName, tc.server, tc.installed); got != tc.want {
				t.Fatalf("blankServerTaskOwnedByLongestInstalledPrefix(%q, %q, %v) = %v, want %v",
					tc.taskName, tc.server, tc.installed, got, tc.want)
			}
		})
	}
}

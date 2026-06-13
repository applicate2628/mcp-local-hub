package api

import (
	"context"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/config"
)

// makeInstalledSet builds an installed-server set from the given names for the
// pure-predicate tests below (these do NOT touch the filesystem; they call the
// disambiguator directly with an explicit set so the r31-F1 ↔ r33-2 boundary is
// exercised deterministically without manifest-dir seeding).
func makeInstalledSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// TestBlankServerRowOwnedByLongestInstalledPrefix is the pure unit test for the
// r33-2 disambiguator. It pins both directions of the r31-F1 ↔ r33-2 tension at
// the predicate level.
func TestBlankServerRowOwnedByLongestInstalledPrefix(t *testing.T) {
	cases := []struct {
		name      string
		taskName  string
		server    string
		installed map[string]struct{}
		want      bool
	}{
		{
			// r33-2 FIX: removed/renamed daemon, no sibling installed → demo
			// reclaims the stale row.
			name:      "stale removed daemon, no sibling → claimed",
			taskName:  `\mcp-local-hub-demo-oldname`,
			server:    "demo",
			installed: makeInstalledSet("demo", "other"),
			want:      true,
		},
		{
			// r31-F1 PRESERVE: demo-alpha is a longer installed prefix → demo
			// must NOT claim demo-alpha's row.
			name:      "longer installed sibling owns row → not claimed by shorter",
			taskName:  `\mcp-local-hub-demo-alpha-beta`,
			server:    "demo",
			installed: makeInstalledSet("demo", "demo-alpha"),
			want:      false,
		},
		{
			// Same row, but now asking for demo-alpha (the rightful owner) →
			// claimed.
			name:      "longer installed prefix claims its own row",
			taskName:  `\mcp-local-hub-demo-alpha-beta`,
			server:    "demo-alpha",
			installed: makeInstalledSet("demo", "demo-alpha"),
			want:      true,
		},
		{
			// Documented edge: row shaped like a sibling's, but the sibling is
			// NOT installed → the row can only be demo's, so demo claims it.
			name:      "sibling-shaped row, sibling not installed → claimed",
			taskName:  `\mcp-local-hub-demo-alpha-beta`,
			server:    "demo",
			installed: makeInstalledSet("demo"),
			want:      true,
		},
		{
			// Empty installed set (catalog read failed) → no sibling proof, so
			// any prefix-matching row is claimed (safe full-cleanup outcome).
			name:      "empty installed set → prefix row claimed",
			taskName:  `\mcp-local-hub-demo-alpha-beta`,
			server:    "demo",
			installed: nil,
			want:      true,
		},
		{
			// Non-prefix server → never claimed (foreign-server row).
			name:      "non-prefix server → not claimed",
			taskName:  `\mcp-local-hub-other-d`,
			server:    "demo",
			installed: makeInstalledSet("demo", "other"),
			want:      false,
		},
		{
			// Bare server portion with no daemon segment → degenerate, not a
			// daemon row this prefix server claims.
			name:      "bare server portion, no daemon → not claimed",
			taskName:  `\mcp-local-hub-demo`,
			server:    "demo",
			installed: makeInstalledSet("demo"),
			want:      false,
		},
		{
			// A prefix-colliding word that is NOT followed by a hyphen must not
			// match: demonstration is not owned by demo.
			name:      "word-boundary: demonstration not owned by demo",
			taskName:  `\mcp-local-hub-demonstration-x`,
			server:    "demo",
			installed: makeInstalledSet("demo"),
			want:      false,
		},
		{
			// Bare (non-canonical, no leading backslash) task name still parses
			// via canonicalIntentTaskKey.
			name:      "bare task name canonicalizes and claims",
			taskName:  `mcp-local-hub-demo-oldname`,
			server:    "demo",
			installed: makeInstalledSet("demo"),
			want:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := blankServerRowOwnedByLongestInstalledPrefix(tc.taskName, tc.server, tc.installed)
			if got != tc.want {
				t.Fatalf("blankServerRowOwnedByLongestInstalledPrefix(%q, %q, %v) = %v, want %v",
					tc.taskName, tc.server, tc.installed, got, tc.want)
			}
		})
	}
}

// TestSupervisorIntentRowOwnedByScope_LegacyFallbackArms exercises the full
// predicate through a scope: the exact arm, the gated legacy-fallback arm, and
// the populated-Server fast paths together.
func TestSupervisorIntentRowOwnedByScope_LegacyFallbackArms(t *testing.T) {
	// Scope mirroring a full-cleanup install of `demo` (daemon alpha), with
	// demo + demo-alpha installed as siblings, and the legacy fallback enabled.
	scope := &supervisorIntentOwnershipScope{legacyPrefixFallback: true}
	scope.addTaskName("mcp-local-hub-demo-alpha")
	scope.addDaemonKey("alpha")
	scope.addInstalledServer("demo")
	scope.addInstalledServer("demo-alpha")

	t.Run("populated Server exact match", func(t *testing.T) {
		d := SupervisorDaemon{TaskName: `\mcp-local-hub-demo-alpha`, Server: "demo", Daemon: "alpha"}
		if !supervisorIntentRowOwnedByScope(d, "demo", scope) {
			t.Fatal("populated Server==demo row must be owned by demo")
		}
	})
	t.Run("populated different Server not claimed", func(t *testing.T) {
		d := SupervisorDaemon{TaskName: `\mcp-local-hub-other-d`, Server: "other", Daemon: "d"}
		if supervisorIntentRowOwnedByScope(d, "demo", scope) {
			t.Fatal("populated Server==other row must not be owned by demo")
		}
	})
	t.Run("blank Server exact-arm match (current manifest task)", func(t *testing.T) {
		d := SupervisorDaemon{TaskName: `\mcp-local-hub-demo-alpha`}
		if !supervisorIntentRowOwnedByScope(d, "demo", scope) {
			t.Fatal("blank-Server row in current manifest task set must be owned (exact arm)")
		}
	})
	t.Run("blank Server legacy fallback NOT claimed when longer sibling installed", func(t *testing.T) {
		d := SupervisorDaemon{TaskName: `\mcp-local-hub-demo-alpha-beta`, Daemon: "beta"}
		if supervisorIntentRowOwnedByScope(d, "demo", scope) {
			t.Fatal("demo must not claim demo-alpha/beta when demo-alpha is installed (r31-F1)")
		}
	})

	// Scope where ONLY demo is installed: the stale removed-daemon row is
	// reclaimed (r33-2).
	soloScope := &supervisorIntentOwnershipScope{legacyPrefixFallback: true}
	soloScope.addTaskName("mcp-local-hub-demo-alpha")
	soloScope.addDaemonKey("alpha")
	soloScope.addInstalledServer("demo")
	t.Run("blank Server legacy fallback reclaims stale row when no sibling", func(t *testing.T) {
		d := SupervisorDaemon{TaskName: `\mcp-local-hub-demo-oldname`}
		if !supervisorIntentRowOwnedByScope(d, "demo", soloScope) {
			t.Fatal("demo must reclaim its stale removed-daemon row when no sibling is installed (r33-2)")
		}
	})

	// With the fallback DISABLED (e.g. a scope NOT built by the full-cleanup
	// builder), the legacy arm must not fire — exact-only behavior is preserved.
	noFallback := &supervisorIntentOwnershipScope{legacyPrefixFallback: false}
	noFallback.addTaskName("mcp-local-hub-demo-alpha")
	noFallback.addInstalledServer("demo")
	t.Run("fallback disabled → stale row NOT claimed", func(t *testing.T) {
		d := SupervisorDaemon{TaskName: `\mcp-local-hub-demo-oldname`}
		if supervisorIntentRowOwnedByScope(d, "demo", noFallback) {
			t.Fatal("with legacyPrefixFallback=false the stale row must not be claimed (additive/per-daemon safety)")
		}
	})

	// scope==nil keeps the documented legacy prefix residual.
	t.Run("scope nil keeps legacy prefix residual", func(t *testing.T) {
		d := SupervisorDaemon{TaskName: `\mcp-local-hub-demo-anything`}
		if !supervisorIntentRowOwnedBy(d, "demo") {
			t.Fatal("scope==nil prefix residual must still claim a blank-Server prefix row")
		}
	})
}

// TestSupervisorIntentOwnershipScopeForManifest_PopulatesInstalledSet confirms
// the full-cleanup scope builder enables the fallback and captures the
// installed-server catalog from the manifest-dir override.
func TestSupervisorIntentOwnershipScopeForManifest_PopulatesInstalledSet(t *testing.T) {
	seedInstalledServerManifests(t, "demo", "demo-alpha", "other")
	m := &config.ServerManifest{
		Name:      "demo",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "alpha", Port: 33123}},
	}
	scope := supervisorIntentOwnershipScopeForManifest(m, nil, "")
	if !scope.legacyPrefixFallback {
		t.Fatal("full-cleanup scope must enable legacyPrefixFallback")
	}
	for _, want := range []string{"demo", "demo-alpha", "other"} {
		if _, ok := scope.installedServers[want]; !ok {
			t.Errorf("installedServers missing %q; got %v", want, scope.installedServers)
		}
	}
}

// TestRemoveServerFromSupervisorIntent_ManifestUninstall_ReclaimsStaleRow is the
// uninstall-direction r33-2 fix: a manifest-backed uninstall of demo must remove
// demo's stale renamed/removed blank-Server row (not just its current-manifest
// rows), while preserving a longer-installed sibling's row.
func TestRemoveServerFromSupervisorIntent_ManifestUninstall_ReclaimsStaleRow(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	// demo + demo-alpha installed: demo-alpha owns its own row; demo owns its
	// current row AND its stale removed-daemon row.
	seedInstalledServerManifests(t, "demo", "demo-alpha", "other")
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)

	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-demo-alpha`, Server: "demo", Daemon: "alpha", Command: "demo-current", Port: 33123},
			{TaskName: `\mcp-local-hub-demo-oldname`, Command: "demo-stale-removed", Port: 33131},
			{TaskName: `\mcp-local-hub-demo-alpha-beta`, Daemon: "beta", Command: "preserve-demo-alpha-beta", Port: 33122},
			{TaskName: `\mcp-local-hub-other-d`, Server: "other", Daemon: "d", Command: "preserve-other", Port: 33133},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := &config.ServerManifest{
		Name:      "demo",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "alpha", Port: 33123}},
	}
	scope := supervisorIntentOwnershipScopeForManifest(m, nil, "")
	changed, removed, _, err := NewAPI().removeServerFromSupervisorIntentCore(context.Background(), "demo", scope, false)
	if err != nil {
		t.Fatalf("removeServerFromSupervisorIntentCore: %v", err)
	}
	if !changed {
		t.Fatal("uninstall must report changed=true (it removed demo's rows)")
	}

	removedTasks := make(map[string]struct{}, len(removed))
	for _, d := range removed {
		removedTasks[canonicalIntentTaskKey(d.TaskName)] = struct{}{}
	}
	// Both demo's current row AND its stale removed-daemon row must be removed.
	for _, want := range []string{`\mcp-local-hub-demo-alpha`, `\mcp-local-hub-demo-oldname`} {
		if _, ok := removedTasks[want]; !ok {
			t.Errorf("uninstall did not remove demo row %q; removed=%v", want, removedTasks)
		}
	}
	// The longer-installed sibling's row must NOT be removed.
	if _, ok := removedTasks[`\mcp-local-hub-demo-alpha-beta`]; ok {
		t.Error("uninstall of demo wrongly removed demo-alpha/beta (longer installed sibling owns it, r31-F1)")
	}

	got, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	survived := make(map[string]SupervisorDaemon, len(got.Daemons))
	for _, d := range got.Daemons {
		survived[canonicalIntentTaskKey(d.TaskName)] = d
	}
	if _, ok := survived[`\mcp-local-hub-demo-oldname`]; ok {
		t.Error("stale demo-oldname row survived manifest uninstall (r33-2 uninstall direction)")
	}
	if _, ok := survived[`\mcp-local-hub-demo-alpha`]; ok {
		t.Error("demo current row survived manifest uninstall")
	}
	if d, ok := survived[`\mcp-local-hub-demo-alpha-beta`]; !ok || d.Command != "preserve-demo-alpha-beta" {
		t.Errorf("demo-alpha/beta sibling not preserved verbatim; survived=%+v", survived)
	}
	if d, ok := survived[`\mcp-local-hub-other-d`]; !ok || d.Command != "preserve-other" {
		t.Errorf("other/d sibling not preserved verbatim; survived=%+v", survived)
	}
}

package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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

// TestServerOwningTaskByLongestInstalledPrefix is the pure unit test for the
// r37-1 EXPORTED single-owner of the longest-installed-prefix scan. It returns
// the OWNING server (the longest installed-server-name prefix), independent of
// any candidate — the primitive the cli signals/maintenance gates and the
// config-env dedup all compose. Every case is falsifying for a naive last-hyphen
// split (api.ServerFromTaskName), which would return "demo-alpha" for the
// hyphenated demo/alpha-beta row regardless of which servers are installed.
func TestServerOwningTaskByLongestInstalledPrefix(t *testing.T) {
	cases := []struct {
		name      string
		taskName  string
		installed map[string]struct{}
		wantOwner string
		wantOK    bool
	}{
		{
			// CORE r37-1: only demo installed → demo owns the hyphenated row,
			// NOT "demo-alpha" (which a last-hyphen split would produce).
			name:      "only demo installed → demo owns hyphenated row",
			taskName:  `\mcp-local-hub-demo-alpha-beta`,
			installed: makeInstalledSet("demo"),
			wantOwner: "demo",
			wantOK:    true,
		},
		{
			// Longer installed sibling owns the row when both are installed.
			name:      "demo+demo-alpha installed → demo-alpha (longest) owns",
			taskName:  `\mcp-local-hub-demo-alpha-beta`,
			installed: makeInstalledSet("demo", "demo-alpha"),
			wantOwner: "demo-alpha",
			wantOK:    true,
		},
		{
			// Empty catalog → no installed name can be a prefix → ok=false.
			name:      "empty catalog → no owner",
			taskName:  `\mcp-local-hub-demo-alpha-beta`,
			installed: nil,
			wantOwner: "",
			wantOK:    false,
		},
		{
			// Foreign row: no installed server prefixes it → ok=false (the
			// orphan/hub-wide fallback the cli callers preserve).
			name:      "foreign row, no installed prefix → no owner",
			taskName:  `\mcp-local-hub-zzz-x`,
			installed: makeInstalledSet("demo", "demo-alpha"),
			wantOwner: "",
			wantOK:    false,
		},
		{
			// Word-boundary: `demo` must NOT own `demonstration-x` (the prefix
			// requires a trailing hyphen).
			name:      "word-boundary: demonstration not owned by demo",
			taskName:  `\mcp-local-hub-demonstration-x`,
			installed: makeInstalledSet("demo"),
			wantOwner: "",
			wantOK:    false,
		},
		{
			// Bare server portion with no daemon segment → degenerate, no owner.
			name:      "bare server portion, no daemon → no owner",
			taskName:  `\mcp-local-hub-demo`,
			installed: makeInstalledSet("demo"),
			wantOwner: "",
			wantOK:    false,
		},
		{
			// Non-canonical (no leading backslash) task name canonicalizes.
			name:      "bare non-canonical task name canonicalizes",
			taskName:  `mcp-local-hub-demo-alpha-beta`,
			installed: makeInstalledSet("demo"),
			wantOwner: "demo",
			wantOK:    true,
		},
		{
			// Not a hub task at all → ok=false.
			name:      "non-hub task → no owner",
			taskName:  `\some-other-task`,
			installed: makeInstalledSet("demo"),
			wantOwner: "",
			wantOK:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, ok := ServerOwningTaskByLongestInstalledPrefix(tc.taskName, tc.installed)
			if owner != tc.wantOwner || ok != tc.wantOK {
				t.Fatalf("ServerOwningTaskByLongestInstalledPrefix(%q, %v) = (%q, %v), want (%q, %v)",
					tc.taskName, tc.installed, owner, ok, tc.wantOwner, tc.wantOK)
			}
		})
	}
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

// TestSupervisorIntentRowOwnedByScope_ArgvVetoPreventsSiblingReap is commission PR
// #505 r6 F1: the install/uninstall REAP-on-reinstall ownership predicate must not
// claim a blank-Server row whose LAUNCH ARGV names a DIFFERENT server, even when the
// row's ambiguous canonical task name is in the reaping scope's manifest task set.
// `mcphub uninstall demo-alpha` reaping demo's live `--server demo` daemon is the
// bug. The argv veto only SUBTRACTS — it never grants a claim the arms would refuse.
func TestSupervisorIntentRowOwnedByScope_ArgvVetoPreventsSiblingReap(t *testing.T) {
	// Scope mirroring `uninstall demo-alpha` (daemon beta) with demo + demo-alpha
	// both installed. The task \mcp-local-hub-demo-alpha-beta IS in demo-alpha's
	// manifest task set (its daemon is `beta`), so Arm 1 alone would claim it.
	scope := &supervisorIntentOwnershipScope{legacyPrefixFallback: true}
	scope.addTaskName("mcp-local-hub-demo-alpha-beta")
	scope.addInstalledServer("demo")
	scope.addInstalledServer("demo-alpha")

	// The row's argv unambiguously says server=demo. demo-alpha must NOT reap it.
	demoRow := SupervisorDaemon{
		TaskName: `\mcp-local-hub-demo-alpha-beta`,
		Args:     []string{"daemon", "--server", "demo", "--daemon", "alpha-beta"},
	}
	if supervisorIntentRowOwnedByScope(demoRow, "demo-alpha", scope) {
		t.Fatal("uninstall demo-alpha must NOT reap demo's live daemon (argv says --server demo) — veto failed")
	}

	// Same row under DEMO's own scope IS owned (argv agrees → arms claim it).
	demoScope := &supervisorIntentOwnershipScope{legacyPrefixFallback: true}
	demoScope.addTaskName("mcp-local-hub-demo-alpha-beta")
	demoScope.addInstalledServer("demo")
	demoScope.addInstalledServer("demo-alpha")
	if !supervisorIntentRowOwnedByScope(demoRow, "demo", demoScope) {
		t.Fatal("demo's own scope must still own its --server demo row (veto must never subtract an argv-agreeing claim)")
	}

	// A partial/corrupt global argv (--server demo, no --daemon) is also vetoed for
	// the sibling scope.
	corruptRow := SupervisorDaemon{
		TaskName: `\mcp-local-hub-demo-alpha-beta`,
		Args:     []string{"daemon", "--server", "demo"},
	}
	if supervisorIntentRowOwnedByScope(corruptRow, "demo-alpha", scope) {
		t.Fatal("partial global argv (--server demo) must not be reaped under demo-alpha's scope")
	}

	// commission PR #505 r6b: a FULLY-POPULATED row whose Server field CONTRADICTS
	// its launch argv must not be reaped by its stale field either — the reap path
	// obeys the same launch-truth rule as match/select.
	lying := SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory", Daemon: "default",
		Args: []string{"daemon", "--server", "time", "--daemon", "default"},
	}
	if supervisorIntentRowOwnedByScope(lying, "memory", scope) {
		t.Fatal("populated lying row (field memory, argv --server time) must NOT be reaped under uninstall memory (r6b)")
	}
	// A well-formed populated row is still owned (common-path neutral).
	wellFormed := SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory", Daemon: "default",
		Args: []string{"daemon", "--server", "memory", "--daemon", "default"},
	}
	if !supervisorIntentRowOwnedByScope(wellFormed, "memory", scope) {
		t.Fatal("well-formed populated memory row must still be owned by memory (dropping the field short-circuit is common-path neutral)")
	}
}

// TestMaintenanceTimerOwnedBy_ArgvVetoPreventsSiblingDrop is the maintenance-timer
// twin of the reap-leak veto (commission PR #505 r6 Q5): a blank-Server timer whose
// argv `restart --server demo` names demo must not be dropped when a hyphen sibling
// (demo-alpha) is uninstalled, and must be owned by demo.
func TestMaintenanceTimerOwnedBy_ArgvVetoPreventsSiblingDrop(t *testing.T) {
	tm := MaintenanceTimer{
		Name: `\mcp-local-hub-demo-alpha-weekly-refresh`,
		Kind: "server-weekly-refresh",
		Args: []string{"restart", "--server", "demo-alpha"},
	}
	if maintenanceTimerOwnedBy(tm, "demo") {
		t.Fatal("a demo-alpha timer (argv --server demo-alpha) must NOT be owned by demo via the ambiguous task-name split")
	}
	if !maintenanceTimerOwnedBy(tm, "demo-alpha") {
		t.Fatal("the timer's argv names demo-alpha — it must be owned by demo-alpha")
	}
	// A populated Server field that disagrees is not claimed by task name.
	populated := MaintenanceTimer{Name: `\mcp-local-hub-demo-weekly-refresh`, Kind: "server-weekly-refresh", Server: "other"}
	if maintenanceTimerOwnedBy(populated, "demo") {
		t.Fatal("populated Server=other timer must not be owned by demo")
	}
	// commission PR #505 r6b: a POPULATED timer whose Server field CONTRADICTS its
	// argv fails closed — owned by NEITHER the stale field nor the argv server
	// (consistent with the SupervisorDaemon reap rule).
	lying := MaintenanceTimer{Name: `\mcp-local-hub-demo-alpha-weekly-refresh`, Kind: "server-weekly-refresh", Server: "demo-alpha", Args: []string{"restart", "--server", "demo"}}
	if maintenanceTimerOwnedBy(lying, "demo-alpha") {
		t.Fatal("lying timer (field demo-alpha, argv --server demo) must not be owned by its stale field demo-alpha (r6b)")
	}
	if maintenanceTimerOwnedBy(lying, "demo") {
		t.Fatal("lying timer (field/argv mismatch) must fail closed — not owned by demo either")
	}
	// A well-formed populated timer (field agrees with argv) is still owned.
	wf := MaintenanceTimer{Name: `\mcp-local-hub-demo-weekly-refresh`, Kind: "server-weekly-refresh", Server: "demo", Args: []string{"restart", "--server", "demo"}}
	if !maintenanceTimerOwnedBy(wf, "demo") {
		t.Fatal("well-formed populated timer must still be owned by demo (common-path neutral)")
	}
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
	now := time.Unix(1700000000, 0).UTC()

	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-demo-alpha`, Server: "demo", Daemon: "alpha", Command: "demo-current", Port: 33123},
			{TaskName: `\mcp-local-hub-demo-oldname`, Command: "demo-stale-removed", Port: 33131},
			{TaskName: `\mcp-local-hub-demo-alpha-beta`, Daemon: "beta", Command: "preserve-demo-alpha-beta", Port: 33122},
			{TaskName: `\mcp-local-hub-other-d`, Server: "other", Daemon: "d", Command: "preserve-other", Port: 33133},
		},
		Stops: map[string]DaemonIntent{
			`\mcp-local-hub-demo-alpha`:      {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			`\mcp-local-hub-demo-oldname`:    {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			`\mcp-local-hub-demo-alpha-beta`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			`\mcp-local-hub-other-d`:         {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
		},
		LegacyStopWatermarks: map[string]DaemonIntent{
			`\mcp-local-hub-demo-alpha`:      {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			`\mcp-local-hub-demo-oldname`:    {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			`\mcp-local-hub-demo-alpha-beta`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			`\mcp-local-hub-other-d`:         {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
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
	for _, removedTask := range []string{`\mcp-local-hub-demo-alpha`, `\mcp-local-hub-demo-oldname`} {
		if _, ok := got.Stops[removedTask]; ok {
			t.Errorf("stop for removed demo row %q survived manifest uninstall: %+v", removedTask, got.Stops)
		}
		if _, ok := got.LegacyStopWatermarks[removedTask]; ok {
			t.Errorf("legacy-stop watermark for removed demo row %q survived manifest uninstall: %+v", removedTask, got.LegacyStopWatermarks)
		}
	}
	for _, siblingTask := range []string{`\mcp-local-hub-demo-alpha-beta`, `\mcp-local-hub-other-d`} {
		if _, ok := got.Stops[siblingTask]; !ok {
			t.Errorf("sibling stop %q was pruned by manifest uninstall: %+v", siblingTask, got.Stops)
		}
		if _, ok := got.LegacyStopWatermarks[siblingTask]; !ok {
			t.Errorf("sibling legacy-stop watermark %q was pruned by manifest uninstall: %+v", siblingTask, got.LegacyStopWatermarks)
		}
	}
}

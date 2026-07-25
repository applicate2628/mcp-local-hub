// internal/api/builtin_route_daemon_test.go
//
// Guard tests for Increment 1b (work-items/decisions/2026-07-25-supervisor-
// builtin-singleton-daemon.md), internal/api half. See
// internal/cli/builtin_route_daemon_test.go for the console-attach-
// inheritance and port-consistency guards; this file covers the two
// invariants that live entirely on the merge/catalog side: the reserved row
// must be invisible to ordinary per-server install/uninstall ownership
// scans, and "route" must never collide with a real shipped manifest name.
package api

import (
	"io"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/config"
)

func hasBuiltinRouteDaemonRow(f *SupervisorIntentFile) bool {
	if f == nil {
		return false
	}
	for _, d := range f.Daemons {
		if d.TaskName == BuiltinRouteTaskName {
			return true
		}
	}
	return false
}

// TestBuiltinRouteDaemon_SurvivesUnrelatedServerInstallThenUninstall is S4
// test 3. It seeds supervisor-intent.json with ONLY the built-in route row
// (as if a prior supervisor cold start already durably persisted it via
// ensureBuiltinRouteDaemonAtStartup), then drives the exact two ownership-
// scan entry points a real `mcphub install`/`mcphub uninstall demo` cycle for
// an UNRELATED server "demo" would use:
//
//  1. buildMergedSupervisorIntent(demo's manifest, ...) — the install-side
//     merge (install_parsed_manifest.go). Its kept-loop only drops rows this
//     manifest OWNS (supervisorIntentRowOwnedByScope, keyed on Server=="demo"
//     or a blank-Server task-name-prefix match); route's non-blank
//     Server=="route" is never a match, so the row must survive into
//     `merged`.
//  2. removeServerFromSupervisorIntent("demo") — the uninstall-side removal
//     (same file). Its predicate is server-name-scoped the same way; the
//     route row must survive the uninstall too.
//
// Mutation proof: temporarily seeding BuildBuiltinRouteDaemon with
// Server:"demo" (impersonating the manifest under install/uninstall in this
// test) makes both assertions below fail — the row gets dropped by BOTH the
// install merge and the uninstall removal, exactly as any real demo-owned
// row would be. Confirmed during development, then reverted; see the
// implementation report for the transcript.
func TestBuiltinRouteDaemon_SurvivesUnrelatedServerInstallThenUninstall(t *testing.T) {
	stateDir := phaseFStateDir(t)
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)

	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			BuildBuiltinRouteDaemon("/fake/mcphub", 9137),
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	// Step 1: full install of an unrelated global server "demo".
	m := &config.ServerManifest{
		Name: "demo",
		Daemons: []config.DaemonSpec{
			{Name: "alpha", Port: 19211},
		},
	}
	merged, _, _, err := NewAPI().buildMergedSupervisorIntent(m, intentPath, nil, "", io.Discard)
	if err != nil {
		t.Fatalf("buildMergedSupervisorIntent(install demo): %v", err)
	}
	if !hasBuiltinRouteDaemonRow(merged) {
		t.Fatalf("installing unrelated server %q dropped the built-in route row; merged.Daemons=%+v", m.Name, merged.Daemons)
	}
	if err := WriteSupervisorIntent(intentPath, merged); err != nil {
		t.Fatalf("write merged supervisor-intent.json (simulating install commit): %v", err)
	}

	// Step 2: uninstall the same unrelated server.
	if _, err := NewAPI().removeServerFromSupervisorIntent("demo"); err != nil {
		t.Fatalf("removeServerFromSupervisorIntent(demo): %v", err)
	}
	final, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent (post-uninstall): %v", err)
	}
	if !hasBuiltinRouteDaemonRow(final) {
		t.Fatalf("uninstalling unrelated server \"demo\" dropped the built-in route row; final.Daemons=%+v", final.Daemons)
	}
}

// TestBuiltinRouteDaemon_ReservedServerNameNotClaimedByAnyShippedManifest is
// S4 test 4. It mechanically re-verifies, against the binary's actual
// //go:embed servers/ catalog, that no shipped manifest declares
// name=="route" — the reserved-name isolation
// BuildBuiltinRouteDaemon/EnsureBuiltinRouteDaemon depend on (see
// builtin_route_daemon.go's doc comment on BuiltinRouteServer). If a future
// catalog addition ever used "route" as a server name, that manifest's own
// install/uninstall ownership scan (supervisorIntentRowOwnedByScope) WOULD
// legitimately claim and mutate the reserved row — this test exists so that
// collision is caught here, at catalog-authoring time, rather than being
// discovered later as a live daemon getting silently clobbered.
//
// Mutation proof: temporarily asserting `name == "memory"` (a real shipped
// manifest) instead of `name == BuiltinRouteServer` makes this test fail —
// confirming the loop and the embed source are both live, not a vacuously
// empty range. Confirmed during development, then reverted; see the
// implementation report for the transcript.
func TestBuiltinRouteDaemon_ReservedServerNameNotClaimedByAnyShippedManifest(t *testing.T) {
	names := embeddedManifestNames()
	if len(names) == 0 {
		t.Fatal("embeddedManifestNames() returned empty — //go:embed pattern broken? (see manifest_source_test.go's own sanity check)")
	}
	for _, name := range names {
		if name == BuiltinRouteServer {
			t.Fatalf("shipped manifest directory %q collides with the reserved built-in route daemon's Server name %q", name, BuiltinRouteServer)
		}
		data, err := loadManifestYAMLEmbedFirst(name)
		if err != nil {
			t.Fatalf("load embedded manifest %q: %v", name, err)
		}
		m, err := parseManifestForName(name, data)
		if err != nil {
			t.Fatalf("parse embedded manifest %q: %v", name, err)
		}
		if m.Name == BuiltinRouteServer {
			t.Fatalf("shipped manifest %q declares name=%q, colliding with the reserved built-in route daemon's Server name", name, m.Name)
		}
	}
}

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
	"reflect"
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

// TestEnsureBuiltinRouteDaemon_ForeignRowCollisionRejectedLoudly is the P2-4
// falsifying test (adversarial review, mcphub-front-daemon Increment 1):
// EnsureBuiltinRouteDaemon matched purely on the canonical task-name key, so
// a pre-existing row that happens to carry the reserved task name but a
// DIFFERENT identity (Server/Command) — e.g. an operator hand-edit, a stale
// migration artifact, or a future bug elsewhere that writes under this name —
// was silently wholesale-replaced with the canonical route descriptor. That
// destroys whatever the foreign row represented with no diagnostic at all.
// The corrected contract must refuse (return an error) instead.
//
// Mutation-proven: reverting EnsureBuiltinRouteDaemon to compare only
// Command/Args/Port (the pre-fix shape) makes this test fail — the foreign
// row is replaced with no error.
func TestEnsureBuiltinRouteDaemon_ForeignRowCollisionRejectedLoudly(t *testing.T) {
	f := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{
				TaskName: BuiltinRouteTaskName,
				Server:   "some-other-server",
				Daemon:   "default",
				Command:  `C:\custom.exe`,
				Args:     []string{"custom", "--flag"},
				Port:     9999,
			},
		},
	}
	before := append([]SupervisorDaemon(nil), f.Daemons...)

	changed, err := EnsureBuiltinRouteDaemon(f, "/fake/mcphub", 9137)
	if err == nil {
		t.Fatalf("EnsureBuiltinRouteDaemon: got changed=%v, err=nil; want a collision error — a foreign row squatting on the reserved task name %q must never be silently overwritten", changed, BuiltinRouteTaskName)
	}
	if changed {
		t.Errorf("EnsureBuiltinRouteDaemon reported changed=true alongside an error; the foreign row must be left untouched")
	}
	if len(f.Daemons) != len(before) || !reflect.DeepEqual(f.Daemons[0], before[0]) {
		t.Fatalf("foreign row was mutated despite the collision error: got %+v, want unchanged %+v", f.Daemons, before)
	}
}

// TestEnsureBuiltinRouteDaemon_ServerDaemonDriftIsNotSilentlyAcceptedAsCanonical
// is the P2-4 under-canonicalization falsifying test: a row whose
// Command/Args/Port already match the canonical descriptor but whose
// Server/Daemon differ from the reserved identity (BuiltinRouteServer/
// BuiltinRouteDaemonName) is NOT "already canonical" — it is the same
// collision class as the wholesale-clobber case above, just with a
// coincidentally-matching Command/Args/Port. The pre-fix comparison (which
// only checked Command/Args/Port) treated this as changed=false ("already
// correct"), silently leaving a non-canonical Server/Daemon in place forever.
//
// Mutation-proven: reverting to the Command/Args/Port-only comparison makes
// this test fail — EnsureBuiltinRouteDaemon returns changed=false, nil
// instead of an error.
func TestEnsureBuiltinRouteDaemon_ServerDaemonDriftIsNotSilentlyAcceptedAsCanonical(t *testing.T) {
	want := BuildBuiltinRouteDaemon("/fake/mcphub", 9137)
	f := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{
				TaskName: BuiltinRouteTaskName,
				Server:   "drifted-server",
				Daemon:   "drifted-daemon",
				Command:  want.Command,
				Args:     append([]string(nil), want.Args...),
				Port:     want.Port,
			},
		},
	}

	changed, err := EnsureBuiltinRouteDaemon(f, "/fake/mcphub", 9137)
	if err == nil {
		t.Fatalf("EnsureBuiltinRouteDaemon: got changed=%v, err=nil; want a collision error — Server/Daemon drift from the reserved identity must not be silently treated as already-canonical", changed)
	}
}

// TestEnsureBuiltinRouteDaemon_OwnRowFullCanonicalCompare proves the upsert
// still works correctly for rows that DO carry the reserved identity
// (Server==BuiltinRouteServer, Daemon==BuiltinRouteDaemonName): absent ->
// added, present-and-canonical -> unchanged, present-and-drifted (e.g. a
// relocated binary) -> replaced. This is the non-collision happy path the
// P2-4 fix must not regress.
func TestEnsureBuiltinRouteDaemon_OwnRowFullCanonicalCompare(t *testing.T) {
	// Absent -> added.
	f := &SupervisorIntentFile{Version: 1}
	changed, err := EnsureBuiltinRouteDaemon(f, "/fake/mcphub", 9137)
	if err != nil {
		t.Fatalf("EnsureBuiltinRouteDaemon (absent): unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("EnsureBuiltinRouteDaemon (absent): changed=false, want true")
	}
	if len(f.Daemons) != 1 {
		t.Fatalf("EnsureBuiltinRouteDaemon (absent): len(Daemons)=%d, want 1", len(f.Daemons))
	}

	// Present-and-canonical -> unchanged.
	changed, err = EnsureBuiltinRouteDaemon(f, "/fake/mcphub", 9137)
	if err != nil {
		t.Fatalf("EnsureBuiltinRouteDaemon (canonical): unexpected error: %v", err)
	}
	if changed {
		t.Errorf("EnsureBuiltinRouteDaemon (canonical): changed=true, want false (already canonical)")
	}

	// Present-and-drifted (binary relocated) -> replaced.
	changed, err = EnsureBuiltinRouteDaemon(f, "/fake/mcphub-relocated", 9137)
	if err != nil {
		t.Fatalf("EnsureBuiltinRouteDaemon (drifted command): unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("EnsureBuiltinRouteDaemon (drifted command): changed=false, want true")
	}
	if f.Daemons[0].Command != "/fake/mcphub-relocated" {
		t.Fatalf("EnsureBuiltinRouteDaemon (drifted command): Command = %q, want the relocated path", f.Daemons[0].Command)
	}
	if len(f.Daemons) != 1 {
		t.Fatalf("EnsureBuiltinRouteDaemon (drifted command): len(Daemons)=%d, want 1 (no duplicate row)", len(f.Daemons))
	}
}

// TestEnsureBuiltinRouteDaemon_DuplicateCanonicalKeyRowsCollapseToOne covers
// the "remove duplicate canonical-key rows" half of the P2-4 fix: if two
// rows both carry the reserved task name (and both carry the reserved
// identity, so neither is a foreign-collision), the upsert must collapse
// them to exactly one canonical row rather than leaving a duplicate behind.
func TestEnsureBuiltinRouteDaemon_DuplicateCanonicalKeyRowsCollapseToOne(t *testing.T) {
	dup := BuildBuiltinRouteDaemon("/fake/mcphub-stale", 9137)
	f := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{dup, dup},
	}

	changed, err := EnsureBuiltinRouteDaemon(f, "/fake/mcphub", 9137)
	if err != nil {
		t.Fatalf("EnsureBuiltinRouteDaemon: unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("EnsureBuiltinRouteDaemon: changed=false, want true (duplicate collapse + command update)")
	}
	count := 0
	for _, d := range f.Daemons {
		if canonicalIntentTaskKey(d.TaskName) == BuiltinRouteTaskName {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("EnsureBuiltinRouteDaemon: %d rows carry the reserved task name after upsert, want exactly 1: %+v", count, f.Daemons)
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

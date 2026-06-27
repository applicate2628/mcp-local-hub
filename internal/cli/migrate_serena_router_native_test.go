// Package cli — area-4 router-native catalog flip: existing-host safety guard.
//
// The shipped servers/serena/manifest.yaml was flipped to the dynamic-pool
// shape, so detectSerenaSourceState now classifies the SHIPPED catalog as
// serenaSourceAlreadyMigrated. This test is the LOAD-BEARING falsifier that the
// flip does NOT strand a legacy host: the no-op / cutover decision keys on the
// committed supervisor-intent.json (serenaRuntimeIntentIsDynamicPool — a
// nil-runtime_spec row ⇒ false), NOT on the catalog shape. A host whose
// committed intent is a legacy single 9121 `unified` daemon (no runtime_spec)
// must still get the full cutover (reap the legacy 9121 + write runtime_spec)
// even though the catalog now reads as already-migrated.
//
// This is DISTINCT from TestMigrateSerena_CatalogDynamicPool_RuntimeLegacyMissing_Proceeds:
// that test seeds NO intent (the absent-intent path returns false at
// serenaRuntimeIntentIsDynamicPool's os.ErrNotExist branch). This one seeds a
// PRESENT-but-legacy intent so the classifier walks a non-empty Daemons slice
// and finds no RuntimeSpec — the present-intent leg of the same predicate.
//
// Design ref: area-4 REVISE (architect a5920370 — accepted REVISE-to-narrower).
package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

func TestMigrateSerena_RouterNativeCatalog_LegacyIntentHost_ProceedsWithCutover(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)

	// CATALOG is the NOW-ROUTER-NATIVE shipped shape (daemon_template, no
	// daemons[]) → detectSerenaSourceState classifies it serenaSourceAlreadyMigrated.
	// This is exactly the shape servers/serena/manifest.yaml ships after the flip;
	// alreadyMigratedManifestYAML mirrors its dynamic-pool daemon_template.
	seedSerenaManifest(t, manifestDir, alreadyMigratedManifestYAML)

	// COMMITTED INTENT is LEGACY: a single 9121 `unified` daemon row with a NIL
	// RuntimeSpec — the v0.5.x unified-intermediate runtime a real existing host
	// carries before the cutover. serenaRuntimeIntentIsDynamicPool must read this
	// PRESENT intent, walk its Daemons, find no RuntimeSpec, and return false.
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	legacyIntent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: "\\mcp-local-hub-serena-unified",
			Server:   "serena",
			Daemon:   "unified",
			Command:  "mcphub",
			Args:     []string{"daemon", "--server", "serena", "--daemon", "unified"},
			Port:     9121,
			// RuntimeSpec intentionally nil — this is the legacy/global runtime row.
		}},
	}
	if err := api.WriteStateFileAtomic(intentPath, legacyIntent); err != nil {
		t.Fatalf("seed legacy nil-runtime_spec intent: %v", err)
	}

	// Sanity: prove the falsifier is non-vacuous — with this committed intent the
	// runtime predicate MUST be false (else the test below would pass trivially
	// because the migrate always proceeds when "not already migrated").
	already, err := serenaRuntimeIntentIsDynamicPool()
	if err != nil {
		t.Fatalf("serenaRuntimeIntentIsDynamicPool on the legacy intent: %v", err)
	}
	if already {
		t.Fatalf("a legacy nil-runtime_spec 9121 intent must NOT classify as dynamic-pool; the falsifier would be vacuous")
	}
	// And prove the CATALOG independently classifies as already-migrated — so the
	// test genuinely exercises the "catalog says migrated, intent says legacy"
	// collision the flip introduces.
	srcManifest, err := loadSerenaManifestForMigrateFn()
	if err != nil {
		t.Fatalf("load seeded catalog: %v", err)
	}
	catalogState, err := detectSerenaSourceState(srcManifest)
	if err != nil {
		t.Fatalf("detectSerenaSourceState on the router-native catalog: %v", err)
	}
	if catalogState != serenaSourceAlreadyMigrated {
		t.Fatalf("router-native catalog must classify as already-migrated; got %v", catalogState)
	}

	// One registered serena workspace so the cutover fan-out materializes a
	// spec-bearing row (the strongest proof the migrate PROCEEDED, not no-op'd).
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)

	// Model a RUNNING legacy supervisor: a cutover reaps it BEFORE the
	// spec-bearing write (§7.1). Start-support TRUE so the install/reap/start path
	// runs on non-Windows CI too (default false off Windows).
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	defer stubStartSupported(t, func() bool { return true })()

	reconcileInvoked, reapInvoked, startInvoked := false, false, false
	// REAL install (no stub) so the spec-bearing runtime_spec intent is actually
	// written and we can assert the legacy 9121 row was replaced.
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileInvoked = true
		return &api.MigrateReport{}, nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("legacy-intent host + router-native catalog must PROCEED with the cutover; got error: %v", err)
	}

	// PROCEEDED: the full cutover sequence fired (reconcile + reap + start), NOT
	// a catalog-driven no-op.
	if !reconcileInvoked || !reapInvoked || !startInvoked {
		t.Fatalf("legacy-intent host must run the cutover (reconcile=%v reap=%v start=%v); a catalog already-migrated no-op would strand the host",
			reconcileInvoked, reapInvoked, startInvoked)
	}
	if bytes.Contains(buf.Bytes(), []byte("nothing to do")) {
		t.Errorf("the migrate must not report a no-op for a legacy-intent host; got %q", buf.String())
	}

	// The committed intent must now be SPEC-BEARING (the legacy 9121 row was
	// reaped and replaced with a per-workspace runtime_spec row).
	after, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("cutover must write supervisor-intent.json; read err = %v", err)
	}
	if !after.HasRuntimeSpecRow() {
		t.Fatalf("cutover must write a spec-bearing serena intent for the registered workspace; got %+v", after.Daemons)
	}
	// The legacy 9121 `unified` row must be gone (the cutover replaced serena's
	// rows; no leftover legacy global row).
	for _, d := range after.Daemons {
		if d.Server == "serena" && d.Daemon == "unified" && d.RuntimeSpec == nil {
			t.Errorf("legacy nil-runtime_spec serena `unified`@%d row survived the cutover: %+v", d.Port, d)
		}
	}
}

// TestMigrateSerena_RouterNativeCatalog_ClassifiesAsAlreadyMigrated pins the
// classifier-side half of the flip: the shipped (now dynamic-pool) catalog
// classifies as serenaSourceAlreadyMigrated. The classifier LOGIC is unchanged
// — this guards that the flipped catalog actually reaches the already-migrated
// branch (and not malformed), so the existing-host-safety test above is
// exercising the real catalog shape.
func TestMigrateSerena_RouterNativeCatalog_ClassifiesAsAlreadyMigrated(t *testing.T) {
	// Parse the ACTUAL shipped catalog from disk (../../servers/serena/manifest.yaml)
	// rather than a fixture, so a future manifest edit that breaks the
	// already-migrated classification is caught here.
	f, err := os.Open("../../servers/serena/manifest.yaml")
	if err != nil {
		t.Fatalf("open shipped serena manifest: %v", err)
	}
	defer f.Close()
	m, err := config.ParseManifest(f)
	if err != nil {
		t.Fatalf("parse shipped serena manifest: %v", err)
	}
	state, err := detectSerenaSourceState(m)
	if err != nil {
		t.Fatalf("detectSerenaSourceState on the shipped router-native catalog: %v", err)
	}
	if state != serenaSourceAlreadyMigrated {
		t.Errorf("shipped serena catalog classifies as %v, want serenaSourceAlreadyMigrated (router-native flip)", state)
	}
}

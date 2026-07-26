// Package cli — composition test for BLOCKING 1 of the mcphub-register-intent
// REVISE round 2 review: `mcphub workspace unregister --backend serena` used
// to RESURRECT the intent descriptor it had just torn down.
//
// Root cause: PruneWorkspacePhases' serena phase removes the intent
// descriptor (td.RemoveSerenaIntent, which on a live supervisor nudges an
// apply-mode reconcile) BEFORE deleting the registry row (td.DeleteSerenaRow —
// deliberately, per bot r32 P2: the registry row must outlive a failed
// teardown so a retry has something to drive). RepairSerenaIntentFromRegistry
// runs INSIDE that same reconcile round trip (handleReconcile step 0). In the
// window between the descriptor's removal and the registry row's eventual
// delete, the registry row is STILL present with no matching intent daemon —
// exactly the shape repair's orphan classifier uses for "crash-orphan,
// re-append it". Without a way to tell "deliberately being removed" apart from
// "orphaned by a crash", repair re-appends the very row the operator is
// unregistering.
//
// The fix (internal/api/workspace_registry.go SetSerenaPendingRemoval +
// internal/api/prune_workspace.go PruneWorkspacePhases +
// internal/api/serena_intent_repair.go's PendingSerenaRemoval skip) marks the
// registry row BEFORE the teardown runs, so repair skips it for exactly this
// window.
package cli

import (
	"testing"

	"mcp-local-hub/internal/api"
)

// TestWorkspaceUnregisterSerena_DoesNotResurrectIntentDuringTeardown
// reproduces the resurrection directly: the stubbed RemoveSerenaIntent removes
// the target's intent descriptor (mirroring what the real
// RemoveSerenaSupervisorIntentForWorkspace does) and then invokes the REAL
// api.RepairSerenaIntentFromRegistry itself — modeling a concurrent
// apply-mode reconcile firing in the SAME window a live supervisor's own
// nudge would trigger it in. Per the review ("It fires whenever another
// runtime_spec row remains"), a pre-existing healthy spec-bearing workspace is
// seeded so repair's §7.1 introduce-crash guard does not defer instead of
// resurrecting.
func TestWorkspaceUnregisterSerena_DoesNotResurrectIntentDuringTeardown(t *testing.T) {
	withStateDir(t)
	withSerenaDynamicPoolCatalog(t)

	// A pre-existing healthy spec-bearing workspace satisfies repair's §7.1
	// introduce-crash guard.
	healthyWS := t.TempDir()
	seedHealthySerenaIntentRow(t, healthyWS, 9150)
	healthyKey := api.WorkspaceKey(healthyWS)

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"go"})
	wsKey := api.WorkspaceKey(ws)
	taskName := api.SerenaTaskNameForWorkspace(ws) // canonical, leading-backslash form

	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: ws,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9200,
		TaskName:      taskName,
		Languages:     []string{"go"},
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}

	// Seed the target's OWN spec-bearing intent descriptor alongside the
	// healthy one — a real register (or the supervisor's own self-heal) would
	// have materialized this before an unregister could ever tear it down.
	intentPath, err := api.DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("resolve intent path: %v", err)
	}
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("read seeded intent: %v", err)
	}
	intent.Daemons = append(intent.Daemons, api.SupervisorDaemon{
		TaskName:  taskName,
		Server:    api.SerenaServerName,
		Daemon:    "serena-" + wsKey,
		Command:   "mcphub",
		Workspace: ws,
		Port:      9200,
		RuntimeSpec: &api.DaemonRuntimeSpec{
			SpecVersion:   1,
			ChildCommand:  "uvx",
			UpstreamPort:  19200,
			ExternalPort:  9200,
			WorkspacePath: ws,
		},
	})
	if err := api.WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatalf("seed target intent descriptor: %v", err)
	}

	calls := stubSerenaSupervisorTeardown(t, func(canonicalPath string) (bool, error) {
		// 1. Remove the target's intent descriptor — mirrors what the real
		//    RemoveSerenaSupervisorIntentForWorkspace does.
		cur, rerr := api.ReadSupervisorIntent(intentPath)
		if rerr != nil {
			t.Fatalf("read intent inside stub: %v", rerr)
		}
		kept := cur.Daemons[:0]
		for _, d := range cur.Daemons {
			if d.TaskName == taskName {
				continue
			}
			kept = append(kept, d)
		}
		cur.Daemons = kept
		if werr := api.WriteSupervisorIntent(intentPath, cur); werr != nil {
			t.Fatalf("write intent after removing target descriptor: %v", werr)
		}

		// 2. Simulate a CONCURRENT apply-mode reconcile firing INSIDE this
		//    exact window — the mechanism BLOCKING 1 named. Without the
		//    PendingSerenaRemoval mark (set by PruneWorkspacePhases BEFORE
		//    this stub runs), this call would see the registry row (still
		//    present — DeleteSerenaRow has not run yet) with no matching
		//    intent daemon and re-append it.
		stateDir, serr := api.DaemonStateDir()
		if serr != nil {
			t.Fatalf("resolve state dir inside stub: %v", serr)
		}
		if _, _, rerr := api.NewAPI().RepairSerenaIntentFromRegistry(stateDir); rerr != nil {
			t.Fatalf("simulated concurrent repair inside stub: %v", rerr)
		}

		return true, nil
	})

	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "serena"); err != nil {
		t.Fatalf("unregister --backend serena: %v", err)
	}
	if *calls != 1 {
		t.Errorf("teardown seam invoked %d times, want 1", *calls)
	}

	// The registry row must be GONE (normal successful unregister).
	reg = api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if _, ok := reg.GetSerena(wsKey); ok {
		t.Fatalf("serena registry row for %s survived a successful unregister", wsKey)
	}

	// The intent must NOT have been resurrected for the unregistered
	// workspace — this is the exact BLOCKING 1 regression.
	final, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("read final intent: %v", err)
	}
	if final.HasSpecBearingSerenaDaemonForWorkspaceKey(wsKey) {
		t.Fatalf("BLOCKING 1 regression: unregister resurrected the intent descriptor for %s", wsKey)
	}
	// The unrelated healthy workspace must be untouched (append-only
	// invariant — this test is not just proving absence, it is proving the
	// fix does not overcorrect into dropping unrelated rows either).
	if !final.HasSpecBearingSerenaDaemonForWorkspaceKey(healthyKey) {
		t.Fatalf("unrelated healthy workspace %s daemon was lost", healthyKey)
	}
}

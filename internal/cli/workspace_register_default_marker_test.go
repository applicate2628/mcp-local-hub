// Package cli — tests for the two `mcphub workspace register` findings the bot
// raised on PR #590 that live in runWorkspaceRegister's tail:
//
//   - "Keep default-marker writes ordered with registration removal"
//     (workspace_cmd.go:314): the marker write sat OUTSIDE the registry-lock
//     hold, so a concurrent `workspace unregister` could delete the row and run
//     its own marker clear in the gap, after which this command created a
//     marker naming a row that no longer existed — a persisted default pointing
//     at an unregistered workspace, breaking no-path routing.
//
//   - "Correct recovery advice for an unintroduced Serena pool"
//     (workspace_cmd.go:536): the IPC-unavailable branch promised that merely
//     starting the supervisor would materialize the registration. On a host
//     whose serena dynamic pool was never introduced that is false — the
//     startup self-heal's §7.1 introduce guard DEFERS instead of appending, the
//     workspace stays orphaned, and a retry of `workspace register` is rejected
//     as already registered.
package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// ---------------------------------------------------------------------------
// Default-marker ordering
// ---------------------------------------------------------------------------

// TestWorkspaceRegisterSerena_DefaultMarkerWrittenUnderRegistryLock asserts the
// ordering invariant directly: at the instant the marker is written, the
// registry flock is HELD. That is what serializes this write against a
// concurrent unregister — whose row delete takes the same lock, and whose
// marker clear runs strictly after that delete — so no interleaving can leave a
// marker without its row.
//
// The probe is an in-process TryLock on the same registry path (the same
// contention mechanism TestWorkspaceList_LockedDuringRead relies on), taken
// from inside the marker-write seam rather than by racing the command, so the
// assertion is deterministic rather than timing-dependent.
func TestWorkspaceRegisterSerena_DefaultMarkerWrittenUnderRegistryLock(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)

	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}

	lockHeldAtWrite := false
	rowVisibleAtWrite := false
	writeCalls := 0
	orig := writeDefaultWorkspaceFn
	writeDefaultWorkspaceFn = func(stateDir, canonical string) error {
		writeCalls++
		// 1. The registry lock must be held by the command right now.
		probe := api.NewRegistry(regPath)
		unlock, acquired, terr := probe.TryLock()
		if terr != nil {
			t.Fatalf("probe TryLock: %v", terr)
		}
		if acquired {
			unlock()
		} else {
			lockHeldAtWrite = true
		}
		// 2. And the row must already be committed, so the marker can never
		//    name a registration that was not written first.
		fresh := api.NewRegistry(regPath)
		if lerr := fresh.Load(); lerr == nil {
			if _, ok := fresh.GetSerena(api.WorkspaceKey(canonical)); ok {
				rowVisibleAtWrite = true
			}
		}
		return orig(stateDir, canonical)
	}
	t.Cleanup(func() { writeDefaultWorkspaceFn = orig })

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})
	if out, rerr := runWorkspaceCmd(t, "register", ws, "--default"); rerr != nil {
		t.Fatalf("register --default: %v\noutput: %s", rerr, out)
	}

	if writeCalls != 1 {
		t.Fatalf("marker write called %d times, want exactly 1", writeCalls)
	}
	if !lockHeldAtWrite {
		t.Error("the registry lock was NOT held when the --default marker was written: a concurrent `workspace unregister` can delete the row and run its own marker clear in that gap, leaving the persisted default pointing at an unregistered workspace")
	}
	if !rowVisibleAtWrite {
		t.Error("the registry row was not committed yet when the --default marker was written")
	}

	// End state is still correct.
	stateDir := filepath.Dir(regPath)
	got, rerr := readDefaultWorkspace(stateDir)
	if rerr != nil {
		t.Fatalf("readDefaultWorkspace: %v", rerr)
	}
	if got != ws {
		t.Errorf("default marker = %q, want %q", got, ws)
	}
}

// TestWorkspaceRegisterSerena_ConcurrentUnregisterClearsOwnDefaultMarker covers
// the residual the ordering alone cannot: a row can also vanish through an
// actor that does NOT clear the marker on its way out (the GUI auto-prune
// sweeper's PruneWorkspace, an unregister whose own best-effort clear failed, a
// hand-edited workspaces.yaml). This command wrote the marker, so once the
// settled check CONFIRMS the row is gone it must take the marker back rather
// than leave no-path routing aimed at an unregistered workspace.
func TestWorkspaceRegisterSerena_ConcurrentUnregisterClearsOwnDefaultMarker(t *testing.T) {
	withSerenaDynamicPoolCatalog(t)
	withStateDir(t)
	useRealSerenaRegisterIntentCheck(t)

	healthyWS := t.TempDir()
	seedHealthySerenaIntentRow(t, healthyWS, 9150)

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})
	wsKey := api.WorkspaceKey(ws)

	orig := serenaRegisterReconcileFn
	t.Cleanup(func() { serenaRegisterReconcileFn = orig })
	serenaRegisterReconcileFn = func(ctx context.Context, apply bool) (api.ReconcileResponse, error) {
		// A row delete that does NOT clear the marker — the residual case.
		regPath, err := api.DefaultRegistryPath()
		if err != nil {
			t.Fatalf("resolve registry path inside stub: %v", err)
		}
		reg := api.NewRegistry(regPath)
		unlock, err := reg.Lock()
		if err != nil {
			t.Fatalf("lock registry inside stub: %v", err)
		}
		if err := reg.Load(); err != nil {
			t.Fatalf("load registry inside stub: %v", err)
		}
		reg.RemoveByBackend(wsKey, "serena")
		if err := reg.Save(); err != nil {
			t.Fatalf("save registry inside stub: %v", err)
		}
		unlock()
		return api.ReconcileResponse{DriftCount: 1, AppliedCount: 1}, nil
	}

	out, err := runWorkspaceCmd(t, "register", ws, "--default")
	if err == nil {
		t.Fatalf("register must NOT report success when the row was concurrently deleted; output: %s", out)
	}

	regPath, _ := api.DefaultRegistryPath()
	stateDir := filepath.Dir(regPath)
	marker, rerr := readDefaultWorkspace(stateDir)
	if rerr != nil {
		t.Fatalf("readDefaultWorkspace: %v", rerr)
	}
	if marker == ws {
		t.Fatalf("the default marker still points at %q, whose registry row is gone — no-path workspace routing is now aimed at an unregistered workspace", marker)
	}
	if marker != "" {
		t.Errorf("default marker = %q, want cleared", marker)
	}
	if !strings.Contains(err.Error(), "--default marker") {
		t.Errorf("the error should tell the operator the marker was taken back; got %q", err.Error())
	}
}

// A default marker naming a DIFFERENT workspace must survive: the compensating
// clear may only take back the marker this invocation wrote.
func TestWorkspaceRegisterSerena_CompensatingClearLeavesForeignDefaultAlone(t *testing.T) {
	withSerenaDynamicPoolCatalog(t)
	withStateDir(t)
	useRealSerenaRegisterIntentCheck(t)

	healthyWS := t.TempDir()
	seedHealthySerenaIntentRow(t, healthyWS, 9150)

	regPath, _ := api.DefaultRegistryPath()
	stateDir := filepath.Dir(regPath)
	otherDefault := filepath.Join(t.TempDir(), "someone-elses-workspace")
	if err := writeDefaultWorkspace(stateDir, otherDefault); err != nil {
		t.Fatalf("seed foreign default marker: %v", err)
	}

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})
	wsKey := api.WorkspaceKey(ws)

	orig := serenaRegisterReconcileFn
	t.Cleanup(func() { serenaRegisterReconcileFn = orig })
	serenaRegisterReconcileFn = func(ctx context.Context, apply bool) (api.ReconcileResponse, error) {
		reg := api.NewRegistry(regPath)
		unlock, err := reg.Lock()
		if err != nil {
			t.Fatalf("lock registry inside stub: %v", err)
		}
		if err := reg.Load(); err != nil {
			t.Fatalf("load registry inside stub: %v", err)
		}
		reg.RemoveByBackend(wsKey, "serena")
		if err := reg.Save(); err != nil {
			t.Fatalf("save registry inside stub: %v", err)
		}
		unlock()
		return api.ReconcileResponse{DriftCount: 1, AppliedCount: 1}, nil
	}

	// No --default: this invocation never wrote a marker, so it must not touch
	// the existing one even though its own row vanished.
	if out, err := runWorkspaceCmd(t, "register", ws); err == nil {
		t.Fatalf("register must NOT report success when the row was concurrently deleted; output: %s", out)
	}

	got, rerr := readDefaultWorkspace(stateDir)
	if rerr != nil {
		t.Fatalf("readDefaultWorkspace: %v", rerr)
	}
	if got != otherDefault {
		t.Errorf("default marker = %q, want the untouched foreign default %q", got, otherDefault)
	}
}

// ---------------------------------------------------------------------------
// IPC-unavailable recovery advice
// ---------------------------------------------------------------------------

// TestWorkspaceRegisterSerena_IPCUnavailableWithoutPool_AdvisesMigrate pins the
// corrected advice for a host whose serena dynamic pool has never been
// introduced: supervisor-intent.json carries no runtime_spec row, so the
// startup self-heal will DEFER, and telling the operator to just start the
// supervisor sends them into a loop (start -> defer -> still orphaned ->
// re-register rejected as already registered).
func TestWorkspaceRegisterSerena_IPCUnavailableWithoutPool_AdvisesMigrate(t *testing.T) {
	withSerenaDynamicPoolCatalog(t)
	withStateDir(t)
	useRealSerenaRegisterIntentCheck(t)

	// Deliberately NO seedHealthySerenaIntentRow: no runtime_spec row exists.
	stubSerenaRegisterReconcileNoRepair(t,
		fmt.Errorf("supervisor IPC reconcile: dial: %w", api.ErrSupervisorIPCUnavailable))

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})

	out, err := runWorkspaceCmd(t, "register", ws)
	if err == nil {
		t.Fatalf("register must NOT report success when the supervisor is unreachable; output: %s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "migrate serena legacy-to-dynamic-pool") {
		t.Errorf("advice must name the migrate that actually introduces the pool; got %q", msg)
	}
	if !strings.Contains(msg, "will NOT materialize") {
		t.Errorf("advice must say plainly that starting the supervisor alone is not enough; got %q", msg)
	}
	if strings.Contains(msg, "will pick up this registration automatically") {
		t.Errorf("advice still promises automatic startup repair on a host with no dynamic pool; got %q", msg)
	}
}

// The converse: once the pool IS introduced, the startup self-heal genuinely
// can append this row, so the original "just start the supervisor" advice is
// correct and must be kept (no gratuitous migrate instruction).
func TestWorkspaceRegisterSerena_IPCUnavailableWithPool_AdvisesStartSupervisor(t *testing.T) {
	withSerenaDynamicPoolCatalog(t)
	withStateDir(t)
	useRealSerenaRegisterIntentCheck(t)

	healthyWS := t.TempDir()
	seedHealthySerenaIntentRow(t, healthyWS, 9150)

	stubSerenaRegisterReconcileNoRepair(t,
		fmt.Errorf("supervisor IPC reconcile: dial: %w", api.ErrSupervisorIPCUnavailable))

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})

	out, err := runWorkspaceCmd(t, "register", ws)
	if err == nil {
		t.Fatalf("register must NOT report success when the supervisor is unreachable; output: %s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "mcphub supervise") {
		t.Errorf("advice should tell the operator to start the supervisor; got %q", msg)
	}
	if !strings.Contains(msg, "will pick up this registration automatically") {
		t.Errorf("advice should keep the (here accurate) automatic-startup-repair promise; got %q", msg)
	}
	if strings.Contains(msg, "migrate serena legacy-to-dynamic-pool") {
		t.Errorf("advice must not demand a migrate on a host whose pool is already introduced; got %q", msg)
	}
}

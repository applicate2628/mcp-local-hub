// Package cli — ordering tests for the unregister-side default-workspace marker
// clear.
//
// The clear used to run AFTER api.PruneWorkspacePhases had returned, i.e.
// outside every registry-lock hold, gated only on the --backend flag. A
// concurrent `mcphub workspace register --default` could therefore recreate BOTH
// the serena row and its marker inside that gap, and this older unregister would
// then wipe the NEW marker even though that registration had succeeded — the
// operator ends up registered with no default. It is the exact mirror of the
// register-side ordering defect closed by moving the marker WRITE inside the
// hold (workspace_cmd.go step 6a).
//
// Lock-hold state at the instant of the clear is not observable from outside the
// critical section, so these tests assert it through the
// clearDefaultWorkspaceIfMatchesFn seam: from inside the stub they TryLock the
// registry and require the acquisition to FAIL, which can only happen while the
// production code holds it. Nothing here races real timing.
package cli

import (
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
)

// stubClearDefaultMarker replaces the unregister-side marker-clear seam and
// records, for every invocation, the canonical path it was asked to clear and
// whether the registry flock was held at that moment.
func stubClearDefaultMarker(t *testing.T, regPath string) *[]clearMarkerCall {
	t.Helper()
	var calls []clearMarkerCall
	prev := clearDefaultWorkspaceIfMatchesFn
	clearDefaultWorkspaceIfMatchesFn = func(stateDir, canonical string) error {
		unlock, ok, err := api.NewRegistry(regPath).TryLock()
		if err != nil {
			t.Errorf("probe registry lock during the marker clear: %v", err)
		}
		if ok {
			unlock()
		}
		calls = append(calls, clearMarkerCall{
			stateDir:       stateDir,
			canonical:      canonical,
			registryLocked: !ok,
		})
		return prev(stateDir, canonical)
	}
	t.Cleanup(func() { clearDefaultWorkspaceIfMatchesFn = prev })
	return &calls
}

type clearMarkerCall struct {
	stateDir       string
	canonical      string
	registryLocked bool
}

// seedSerenaRowWithDefault registers a serena row for a fresh workspace and
// points the default marker at it. Returns (regPath, canonical workspace path).
func seedSerenaRowWithDefault(t *testing.T) (string, string) {
	t.Helper()
	withStateDir(t)
	ws := makeWorkspaceDir(t, t.TempDir(), []string{"go"})
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  api.WorkspaceKey(ws),
		WorkspacePath: ws,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          0,
		TaskName:      api.SerenaTaskNameForWorkspace(ws),
		Languages:     []string{"go"},
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}
	if err := api.WriteDefaultWorkspace(filepath.Dir(regPath), ws); err != nil {
		t.Fatalf("WriteDefaultWorkspace: %v", err)
	}
	return regPath, ws
}

// The clear must happen INSIDE DeleteSerenaRow's registry-lock hold, so a
// concurrent register --default cannot slip its row+marker write between the row
// delete and the clear.
func TestWorkspaceUnregister_ClearsDefaultMarkerInsideTheRegistryLockHold(t *testing.T) {
	regPath, ws := seedSerenaRowWithDefault(t)
	calls := stubClearDefaultMarker(t, regPath)
	stubSerenaSupervisorTeardown(t, func(string) (bool, error) { return true, nil })

	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "serena"); err != nil {
		t.Fatalf("unregister --backend serena: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("marker clear fired %d times, want exactly 1", len(*calls))
	}
	got := (*calls)[0]
	if !got.registryLocked {
		t.Error("the default-marker clear ran while the registry lock was FREE — outside the hold a concurrent `register --default` can recreate the row and marker in the gap, and this clear then wipes the new marker")
	}
	if got.canonical != ws {
		t.Errorf("clear canonical = %q, want %q", got.canonical, ws)
	}
	if want := filepath.Dir(regPath); got.stateDir != want {
		t.Errorf("clear stateDir = %q, want %q (the marker is co-located with workspaces.yaml)", got.stateDir, want)
	}

	// End state: the row is gone and the marker went with it.
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if _, ok := reg.GetSerena(api.WorkspaceKey(ws)); ok {
		t.Error("serena row survived a successful unregister")
	}
	marker, err := api.ReadDefaultWorkspace(filepath.Dir(regPath))
	if err != nil {
		t.Fatalf("ReadDefaultWorkspace: %v", err)
	}
	if marker != "" {
		t.Errorf("default marker = %q, want cleared — a stale default routes to an unregistered workspace", marker)
	}
}

// Ownership gate: only the process that ACTUALLY removed the serena row clears
// the marker naming it. A racing unregister that finds the row already gone
// (n == 0) is not the remover, and the marker it would clear may have just been
// legitimately written by a concurrent register --default.
//
// The already-gone state is produced deterministically through the existing
// supervisor-teardown seam, which runs after classification and BEFORE
// DeleteSerenaRow while holding no registry lock.
func TestWorkspaceUnregister_ZeroRowDeleteLeavesTheDefaultMarkerAlone(t *testing.T) {
	regPath, ws := seedSerenaRowWithDefault(t)
	calls := stubClearDefaultMarker(t, regPath)

	// Model the racing actor: between this command's classify and its row
	// delete, someone else removes the serena row (and the marker now belongs to
	// whoever re-registered it).
	stubSerenaSupervisorTeardown(t, func(string) (bool, error) {
		reg := api.NewRegistry(regPath)
		unlock, err := reg.Lock()
		if err != nil {
			t.Fatalf("lock registry inside the teardown seam: %v", err)
		}
		defer unlock()
		if err := reg.Load(); err != nil {
			t.Fatalf("load registry inside the teardown seam: %v", err)
		}
		if n := reg.RemoveByBackend(api.WorkspaceKey(ws), "serena"); n != 1 {
			t.Fatalf("precondition: removed %d serena rows, want 1", n)
		}
		if err := reg.Save(); err != nil {
			t.Fatalf("save registry inside the teardown seam: %v", err)
		}
		return true, nil
	})

	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "serena"); err != nil {
		t.Fatalf("unregister --backend serena: %v", err)
	}

	if len(*calls) != 0 {
		t.Errorf("marker clear fired %d times for a delete that removed 0 rows (%+v); only the process that actually removed the row may clear the marker naming it", len(*calls), *calls)
	}
	marker, err := api.ReadDefaultWorkspace(filepath.Dir(regPath))
	if err != nil {
		t.Fatalf("ReadDefaultWorkspace: %v", err)
	}
	if marker != ws {
		t.Errorf("default marker = %q, want %q preserved — this command removed nothing, so it owns no clear", marker, ws)
	}
}

// A marker naming a DIFFERENT workspace survives: the clear is
// clear-if-matches, so unregistering workspace A never drops workspace B's
// default.
func TestWorkspaceUnregister_LeavesAnUnrelatedDefaultMarkerIntact(t *testing.T) {
	regPath, ws := seedSerenaRowWithDefault(t)
	other := makeWorkspaceDir(t, t.TempDir(), []string{"go"})
	if err := api.WriteDefaultWorkspace(filepath.Dir(regPath), other); err != nil {
		t.Fatalf("WriteDefaultWorkspace(other): %v", err)
	}
	stubSerenaSupervisorTeardown(t, func(string) (bool, error) { return true, nil })

	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "serena"); err != nil {
		t.Fatalf("unregister --backend serena: %v", err)
	}

	marker, err := api.ReadDefaultWorkspace(filepath.Dir(regPath))
	if err != nil {
		t.Fatalf("ReadDefaultWorkspace: %v", err)
	}
	if marker != other {
		t.Errorf("default marker = %q, want %q — clearing is match-gated and must not touch another workspace's default", marker, other)
	}
}

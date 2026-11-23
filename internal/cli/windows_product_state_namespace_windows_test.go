package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/gui"
)

func TestWindowsProductBoundNamespaceOwners(t *testing.T) {
	base := apitest.HardenedTempDir(t)
	stateDir := apitest.HardenedDir(t, filepath.Join(base, "state"))
	scope, err := api.OpenWindowsProductStateNamespace(t.Context(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := os.Rename(base, base+"-replacement"); err == nil {
		t.Fatal("ancestor renamed before Product fence")
	}
	pidport := filepath.Join(stateDir, gui.PidportFileLeaf)
	hPath := filepath.Join(stateDir, "hub-mcp.lock")
	settings := filepath.Join(stateDir, "gui-preferences.yaml")
	if err := api.WriteStateFileBytesAtomic(settings, []byte("gui_server.hub_endpoint_enabled: true\n")); err != nil {
		t.Fatal(err)
	}
	// Existing owners use these paths in deploy. No live Product, GUI or Task
	// is needed to exercise the real lock owners and kernel exclusion.
	oldFence := flock.New(filepath.Join(stateDir, api.UpgradeTransactionLockFileLeaf))
	if err := oldFence.Lock(); err != nil {
		t.Fatalf("legacy fence setup: %v", err)
	}
	bounded, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	if _, err := api.AcquireUpgradeFenceBound(bounded, stateDir, scope); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("bound fence bypassed original: %v", err)
	}
	cancel()
	if err := oldFence.Unlock(); err != nil {
		t.Fatal(err)
	}
	f, err := api.AcquireUpgradeFenceBound(t.Context(), stateDir, scope)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Release(); err != nil {
			t.Error(err)
		}
	})
	if acquired, err := oldFence.TryLock(); err != nil || acquired {
		if acquired {
			_ = oldFence.Unlock()
		}
		t.Fatalf("legacy byte lock bypassed original bound Product descriptor: %v", err)
	}
	if other, acquired, err := api.TryAcquireUpgradeFence(t.Context(), stateDir); err != nil || acquired {
		if other != nil {
			_ = other.Release()
		}
		t.Fatalf("legacy fence bypassed bound owner: %v", err)
	}
	if err := os.Rename(stateDir, stateDir+"-replacement"); err == nil {
		t.Fatal("state directory renamed between Product fence and GUI reservation")
	}
	if _, err := gui.ReadGUIOwnerRecordBound(t.Context(), scope, pidport); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bound absent pidport: %v", err)
	}
	rawGUI := flock.New(pidport + ".lock")
	if err := rawGUI.Lock(); err != nil {
		t.Fatal(err)
	}
	if _, err := gui.AcquireSingleInstanceLockOnlyBoundAt(t.Context(), scope, pidport); !errors.Is(err, gui.ErrSingleInstanceBusy) {
		t.Errorf("bound GUI failed to observe legacy holder: %v", err)
	}
	if err := rawGUI.Unlock(); err != nil {
		t.Fatal(err)
	}
	heldGUI, err := gui.AcquireSingleInstanceLockOnlyBoundAt(t.Context(), scope, pidport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := heldGUI.ReleaseErr(); err != nil {
			t.Error(err)
		}
	})
	if ok, err := rawGUI.TryLock(); ok || err != nil {
		if ok {
			_ = rawGUI.Unlock()
		}
		t.Fatalf("legacy GUI bypassed bound singleton: %t %v", ok, err)
	}
	if _, err := os.Stat(pidport); !os.IsNotExist(err) {
		t.Fatalf("lock-only path materialized pidport: %v", err)
	}
	if err := os.Rename(base, base+"-replacement"); err == nil {
		t.Fatal("ancestor renamed between GUI and H scopes")
	}
	if err := os.Rename(pidport+".lock", pidport+"-replacement.lock"); err == nil {
		t.Fatal("original GUI lock leaf could be renamed")
	}
	hOld := flock.New(hPath)
	if err := hOld.Lock(); err != nil {
		t.Fatal(err)
	}
	bounded, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
	if err := scope.WithHubRouteState(bounded, func(*api.HubRouteState) error { t.Error("bound H bypassed legacy lock"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("bound legacy H contention %v", err)
	}
	cancel()
	if err := hOld.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := scope.WithHubRouteState(t.Context(), func(*api.HubRouteState) error {
		if ok, err := hOld.TryLock(); ok || err != nil {
			if ok {
				_ = hOld.Unlock()
			}
			t.Fatalf("legacy H bypassed bound H owner: %t %v", ok, err)
		}
		settingsOld := flock.New(settings + ".lock")
		if err := settingsOld.Lock(); err != nil {
			return err
		}
		bounded, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		err := api.NewAPI().WithSavedHubEndpointGateBoundIn(bounded, settings, scope, func(bool) error { t.Error("bound preferences bypassed legacy holder"); return nil })
		cancel()
		if releaseErr := settingsOld.Unlock(); releaseErr != nil {
			return releaseErr
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("bound legacy settings contention: %v", err)
		}
		foreign := apitest.HardenedTempDir(t)
		foreignSettings := filepath.Join(foreign, "gui-preferences.yaml")
		if err := api.NewAPI().WithSavedHubEndpointGateBoundIn(t.Context(), foreignSettings, scope, func(bool) error { t.Error("foreign settings selected"); return nil }); err == nil {
			t.Error("different physical settings parent accepted")
		}
		if _, err := os.Stat(foreignSettings + ".lock"); !os.IsNotExist(err) {
			t.Fatalf("foreign settings path had an effect: %v", err)
		}
		return api.NewAPI().WithSavedHubEndpointGateBoundIn(t.Context(), settings, scope, func(enabled bool) error {
			if !enabled {
				t.Error("bound owner did not see saved ON")
			}
			if ok, err := settingsOld.TryLock(); ok || err != nil {
				if ok {
					_ = settingsOld.Unlock()
				}
				t.Errorf("legacy settings bypassed bound owner: %t %v", ok, err)
			}
			return scope.Validate(t.Context())
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(hPath, hPath+"-replacement"); err == nil {
		t.Fatal("original H lock could be renamed between bound H borrows")
	}
	if err := heldGUI.ReleaseErr(); err != nil {
		t.Fatal(err)
	}
	if err := f.Release(); err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	if scope.Validate(t.Context()) == nil {
		t.Fatal("closed namespace remained usable")
	}
}

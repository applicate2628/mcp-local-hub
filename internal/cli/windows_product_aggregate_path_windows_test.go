package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/gui"
)

func TestWindowsProductBoundIssuerPathSelection(t *testing.T) {
	for _, seed := range []bool{false, true} {
		name := "absent_pidport_and_lock"
		if seed {
			name = "absent_lock_with_record"
		}
		t.Run(name, func(t *testing.T) {
			profile := t.TempDir()
			stateDir := apitest.HardenedDir(t, filepath.Join(profile, "mcp-local-hub"))
			t.Setenv("LOCALAPPDATA", profile)
			t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", "")
			scope, err := api.OpenWindowsProductStateNamespace(t.Context(), stateDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := scope.Close(); err != nil {
					t.Error(err)
				}
			})
			want := filepath.Join(stateDir, gui.PidportFileLeaf)
			legacy := []byte("123 43123\n")
			if seed {
				if err := api.SecureWriteClientConfig(want, legacy); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := resolveUpgradeGUIOwnerPath(); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("legacy strict resolver accepted missing leaf: %v", err)
			}
			got, err := resolveInstalledUpgradeGUIPidportPath()
			if err != nil || got != want {
				t.Fatalf("bound issuer selected (%q, %v), want %q", got, err, want)
			}
			if _, err := os.Stat(want + ".lock"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("path selection materialized GUI lease: %v", err)
			}
			record, readErr := gui.ReadGUIOwnerRecordBound(t.Context(), scope, got)
			if seed {
				if readErr != nil || record.PID != 123 || record.Port != 43123 {
					t.Fatalf("original existing pidport read = %+v, %v", record, readErr)
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("clean pidport absence was not recognized: %v", readErr)
			}
			lease, err := gui.AcquireSingleInstanceLockOnlyBoundAt(t.Context(), scope, got)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := lease.ReleaseErr(); err != nil {
					t.Error(err)
				}
			})
			original, err := os.Stat(want + ".lock")
			if err != nil || !original.Mode().IsRegular() || original.Size() != 0 {
				t.Fatalf("original lock-only reservation unavailable: %v", err)
			}
			other := flock.New(want + ".lock")
			if ok, err := other.TryLock(); ok || err != nil {
				if ok {
					_ = other.Unlock()
				}
				t.Fatalf("existing GUI locker bypassed bound lease: %t %v", ok, err)
			}
			raw, err := os.ReadFile(want)
			if seed {
				if err != nil || !bytes.Equal(raw, legacy) {
					t.Fatalf("reservation changed existing pidport: %v", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("reservation created pidport: %v", err)
			}
			if err := lease.ReleaseErr(); err != nil {
				t.Fatal(err)
			}
			if ok, err := other.TryLock(); !ok || err != nil {
				t.Fatalf("original GUI range unavailable after release: %v", err)
			}
			actual, err := os.Stat(want + ".lock")
			releaseErr := other.Unlock()
			if err != nil || releaseErr != nil || !os.SameFile(original, actual) {
				t.Fatalf("release did not preserve same physical GUI lock: %v %v", err, releaseErr)
			}
		})
	}
	t.Run("override_refused", func(t *testing.T) {
		profile := t.TempDir()
		t.Setenv("LOCALAPPDATA", profile)
		t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", t.TempDir())
		if got, err := resolveInstalledUpgradeGUIPidportPath(); got != "" || err == nil || !strings.Contains(err.Error(), "GUI pidport override cannot bind the installed owner") {
			t.Fatalf("installed override selection = %q, %v", got, err)
		}
		if _, err := os.Stat(filepath.Join(profile, "mcp-local-hub")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("override path had an effect: %v", err)
		}
	})
	t.Run("selected_foreign_parent_refused", func(t *testing.T) {
		original := apitest.HardenedTempDir(t)
		profile := t.TempDir()
		alien := apitest.HardenedDir(t, filepath.Join(profile, "mcp-local-hub"))
		t.Setenv("LOCALAPPDATA", profile)
		t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", "")
		scope, err := api.OpenWindowsProductStateNamespace(t.Context(), original)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		})
		got, err := resolveInstalledUpgradeGUIPidportPath()
		if err != nil || got != filepath.Join(alien, gui.PidportFileLeaf) {
			t.Fatalf("normal GUI resolver did not select controlled other parent: %q, %v", got, err)
		}
		if _, err := gui.ReadGUIOwnerRecordBound(t.Context(), scope, got); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("foreign missing leaf was accepted as original absence: %v", err)
		}
		if _, err := os.Stat(got + ".lock"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("foreign GUI path had an effect: %v", err)
		}
	})
}

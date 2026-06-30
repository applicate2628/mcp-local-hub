package api

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/clients"
)

// tempPrefsPath returns a gui-preferences.yaml path inside a fresh temp dir.
func tempPrefsPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "gui-preferences.yaml")
}

func TestDefaultInstallClientNames_AbsentFileNoOverride(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	override, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("override read: %v", err)
	}
	if override != nil {
		t.Fatalf("override = %v, want nil for absent file", override)
	}
	eff, err := a.DefaultInstallClientNamesEffectiveIn(path)
	if err != nil {
		t.Fatalf("effective read: %v", err)
	}
	if want := clients.DefaultInstallClientNames(); !reflect.DeepEqual(eff, want) {
		t.Fatalf("effective = %v, want compile-time default %v", eff, want)
	}
}

func TestSetAndReadDefaultInstallClientNames_RoundTrip(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	// Pick a set that differs from the compile-time trio: add an opt-in,
	// drop one default. Every name must be supported.
	want := []string{"claude-code", "vscode"}
	if err := a.SetDefaultInstallClientNamesIn(path, want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("override = %v, want %v", got, want)
	}
	eff, err := a.DefaultInstallClientNamesEffectiveIn(path)
	if err != nil {
		t.Fatalf("effective: %v", err)
	}
	if !reflect.DeepEqual(eff, want) {
		t.Fatalf("effective = %v, want override %v", eff, want)
	}
}

func TestSetDefaultInstallClientNames_PreservesOtherKeys(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	// Seed an unrelated settings key via the normal settings writer.
	if err := a.SettingsSetIn(path, "appearance.theme", "dark"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	if err := a.SetDefaultInstallClientNamesIn(path, []string{"cursor"}); err != nil {
		t.Fatalf("set clients: %v", err)
	}
	// The pre-existing setting must survive the override write.
	v, err := a.SettingsGetIn(path, "appearance.theme")
	if err != nil {
		t.Fatalf("get theme: %v", err)
	}
	if v != "dark" {
		t.Fatalf("appearance.theme = %q, want dark (override write clobbered it)", v)
	}
	got, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("read clients: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"cursor"}) {
		t.Fatalf("override = %v, want [cursor]", got)
	}
}

func TestSetDefaultInstallClientNamesReadsUnderStateFileFlock(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	if err := a.SettingsSetIn(path, "appearance.theme", "dark"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}

	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock settings state file: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- a.SetDefaultInstallClientNamesIn(path, []string{"vscode"})
	}()

	select {
	case err := <-done:
		_ = lock.Unlock()
		t.Fatalf("SetDefaultInstallClientNamesIn returned before the flock holder published the concurrent setting; err=%v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := WriteStateFileBytesLockHeld(path, []byte("appearance.theme: light\n")); err != nil {
		_ = lock.Unlock()
		t.Fatalf("publish concurrent setting under held flock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock settings state file: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("SetDefaultInstallClientNamesIn after flock release: %v", err)
	}
	raw, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if raw["appearance.theme"] != "light" {
		t.Fatalf("appearance.theme = %q, want light from concurrent writer", raw["appearance.theme"])
	}
	if raw[defaultInstallClientsKey] != "vscode" {
		t.Fatalf("%s = %q, want vscode", defaultInstallClientsKey, raw[defaultInstallClientsKey])
	}
}

func TestSetDefaultInstallClientNames_RejectsUnknownAndEmpty(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	if err := a.SetDefaultInstallClientNamesIn(path, []string{"not-a-client"}); err == nil {
		t.Fatal("expected error for unknown client, got nil")
	}
	if err := a.SetDefaultInstallClientNamesIn(path, []string{"   ", ""}); err == nil {
		t.Fatal("expected error for empty set, got nil")
	}
	// The rejected writes must not have created an override on disk.
	got, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("read after rejected writes: %v", err)
	}
	if got != nil {
		t.Fatalf("override = %v, want nil after rejected writes", got)
	}
}

func TestDefaultInstallClientNames_StaleUnknownNameDropped(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	// Hand-write a CSV containing an unknown name alongside a valid one,
	// simulating a stale gui-preferences.yaml from a build that knew a
	// client this build no longer understands.
	if err := WriteStateFileBytesAtomic(path, []byte("clients.default_install: cursor,ghost-client\n")); err != nil {
		t.Fatalf("write stale prefs: %v", err)
	}
	got, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"cursor"}) {
		t.Fatalf("override = %v, want [cursor] (unknown name should be dropped)", got)
	}
}

func TestDefaultInstallClientNames_AllUnknownFallsBackToCompileDefault(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	if err := WriteStateFileBytesAtomic(path, []byte("clients.default_install: ghost-a,ghost-b\n")); err != nil {
		t.Fatalf("write stale prefs: %v", err)
	}
	override, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("override read: %v", err)
	}
	if override != nil {
		t.Fatalf("override = %v, want nil when no name survives sanitize", override)
	}
	eff, err := a.DefaultInstallClientNamesEffectiveIn(path)
	if err != nil {
		t.Fatalf("effective read: %v", err)
	}
	if want := clients.DefaultInstallClientNames(); !reflect.DeepEqual(eff, want) {
		t.Fatalf("effective = %v, want compile-time default %v", eff, want)
	}
}

func TestClientInstallToggleView_OverrideAndDefault(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)

	// No override → compile-time trio selected, override inactive.
	snap, err := a.ClientInstallToggleViewIn(path)
	if err != nil {
		t.Fatalf("view (no override): %v", err)
	}
	if snap.OverrideActive {
		t.Fatal("OverrideActive = true, want false with no persisted override")
	}
	compileDefault := map[string]bool{}
	for _, n := range clients.DefaultInstallClientNames() {
		compileDefault[n] = true
	}
	for _, row := range snap.Rows {
		if row.Selected != compileDefault[row.Name] {
			t.Fatalf("row %q selected=%v, want %v (compile default)", row.Name, row.Selected, compileDefault[row.Name])
		}
		if row.CompileDefault != compileDefault[row.Name] {
			t.Fatalf("row %q compileDefault=%v, want %v", row.Name, row.CompileDefault, compileDefault[row.Name])
		}
	}
	if len(snap.Rows) != len(clients.SupportedClientNames()) {
		t.Fatalf("rows = %d, want %d (one per supported client)", len(snap.Rows), len(clients.SupportedClientNames()))
	}

	// Persist an override → those names selected, override active.
	if err := a.SetDefaultInstallClientNamesIn(path, []string{"vscode"}); err != nil {
		t.Fatalf("set override: %v", err)
	}
	snap2, err := a.ClientInstallToggleViewIn(path)
	if err != nil {
		t.Fatalf("view (override): %v", err)
	}
	if !snap2.OverrideActive {
		t.Fatal("OverrideActive = false, want true after persisting override")
	}
	for _, row := range snap2.Rows {
		wantSel := row.Name == "vscode"
		if row.Selected != wantSel {
			t.Fatalf("row %q selected=%v, want %v (override is [vscode])", row.Name, row.Selected, wantSel)
		}
	}
}

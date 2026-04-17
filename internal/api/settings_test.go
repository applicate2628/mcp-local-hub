package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "gui-preferences.yaml")

	a := NewAPI()
	if err := a.SettingsSetIn(path, "theme", "dark"); err != nil {
		t.Fatal(err)
	}
	if err := a.SettingsSetIn(path, "shell", "sidebar"); err != nil {
		t.Fatal(err)
	}

	all, err := a.SettingsListIn(path)
	if err != nil {
		t.Fatal(err)
	}
	if all["theme"] != "dark" || all["shell"] != "sidebar" {
		t.Errorf("round-trip: got %v, want {theme:dark, shell:sidebar}", all)
	}

	val, err := a.SettingsGetIn(path, "theme")
	if err != nil {
		t.Fatal(err)
	}
	if val != "dark" {
		t.Errorf("get theme: got %q, want dark", val)
	}
}

func TestSettingsGetMissingKey(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "gui-preferences.yaml")
	_ = os.WriteFile(path, []byte("theme: light\n"), 0600)

	a := NewAPI()
	_, err := a.SettingsGetIn(path, "nonexistent")
	if err == nil {
		t.Error("expected error for missing key, got nil")
	}
}

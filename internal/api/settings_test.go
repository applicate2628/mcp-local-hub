package api

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tmpSettings(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "gui-preferences.yaml")
}

func TestSettings_DefaultsResolve(t *testing.T) {
	a := &API{}
	all, err := a.SettingsListIn(tmpSettings(t))
	if err != nil {
		t.Fatal(err)
	}
	if all["appearance.theme"] != "system" {
		t.Errorf("expected default 'system', got %q", all["appearance.theme"])
	}
	if _, has := all["advanced.open_app_data_folder"]; has {
		t.Error("action keys must not appear in SettingsList output")
	}
}

func TestSettings_SetAndGet(t *testing.T) {
	a := &API{}
	path := tmpSettings(t)
	if err := a.SettingsSetIn(path, "appearance.theme", "dark"); err != nil {
		t.Fatal(err)
	}
	got, err := a.SettingsGetIn(path, "appearance.theme")
	if err != nil {
		t.Fatal(err)
	}
	if got != "dark" {
		t.Errorf("expected 'dark', got %q", got)
	}
}

func TestSettings_Set_RejectsUnknownKey(t *testing.T) {
	a := &API{}
	err := a.SettingsSetIn(tmpSettings(t), "no.such.key", "x")
	if err == nil || !contains(err.Error(), "unknown setting") {
		t.Fatalf("expected unknown-setting error, got %v", err)
	}
}

func TestSettings_Set_RejectsBadValue(t *testing.T) {
	a := &API{}
	err := a.SettingsSetIn(tmpSettings(t), "appearance.theme", "puce")
	if err == nil || !contains(err.Error(), "invalid value") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestSettings_Set_RejectsAction(t *testing.T) {
	a := &API{}
	err := a.SettingsSetIn(tmpSettings(t), "advanced.open_app_data_folder", "anything")
	if err == nil {
		t.Fatal("expected action-set rejection")
	}
}

func TestSettings_Set_PreservesUnknownKeys(t *testing.T) {
	// Codex r1 P2.1: a stale or future-unknown key must round-trip.
	a := &API{}
	path := tmpSettings(t)
	// Seed a file with a known + an unknown key.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	seeded := []byte("appearance.theme: dark\nfuture_unknown.key: hello\n")
	if err := os.WriteFile(path, seeded, 0600); err != nil {
		t.Fatal(err)
	}
	// Mutate a known key.
	if err := a.SettingsSetIn(path, "appearance.theme", "light"); err != nil {
		t.Fatal(err)
	}
	// Reload raw and assert the unknown key survived.
	raw, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatal(err)
	}
	if raw["appearance.theme"] != "light" {
		t.Errorf("known-key write lost: got %q", raw["appearance.theme"])
	}
	if raw["future_unknown.key"] != "hello" {
		t.Errorf("unknown-key NOT preserved on rewrite (Codex r1 P2.1): got %q", raw["future_unknown.key"])
	}
	// And ensure SettingsList still doesn't expose the unknown.
	all, err := a.SettingsListIn(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := all["future_unknown.key"]; has {
		t.Error("SettingsList must not expose unknown keys")
	}
}

func TestSettings_Concurrent_DistinctKeys(t *testing.T) {
	// Codex r1 P1.5 + r3 P2.2: 10 settable registry keys, 10 goroutines,
	// each writing one distinct key concurrently. After Wait, every key
	// must still be present in the file.
	a := &API{}
	path := tmpSettings(t)

	type kv struct{ k, v string }
	pairs := []kv{
		{"appearance.theme", "dark"},
		{"appearance.density", "compact"},
		{"appearance.shell", "bash"},
		{"appearance.default_home", "/home/x"},
		{"gui_server.browser_on_launch", "false"},
		{"gui_server.port", "9999"},
		{"gui_server.tray", "false"},
		{"daemons.weekly_schedule", "daily Mon 04:00"},
		{"daemons.retry_policy", "linear"},
		{"backups.keep_n", "12"},
	}
	var wg sync.WaitGroup
	for _, p := range pairs {
		wg.Add(1)
		go func(p kv) {
			defer wg.Done()
			if err := a.SettingsSetIn(path, p.k, p.v); err != nil {
				t.Errorf("set %q: %v", p.k, err)
			}
		}(p)
	}
	wg.Wait()
	raw, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pairs {
		if raw[p.k] != p.v {
			t.Errorf("lost write: %q expected %q got %q", p.k, p.v, raw[p.k])
		}
	}
}

func TestSettings_Concurrent_SameKey(t *testing.T) {
	// Codex r3 P2.2: 20 goroutines writing the same key; round-robin
	// through 3 valid enum values. File must always parse cleanly and
	// final value must be one of the 3.
	a := &API{}
	path := tmpSettings(t)
	values := []string{"light", "dark", "system"}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := a.SettingsSetIn(path, "appearance.theme", values[i%3]); err != nil {
				t.Errorf("set %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	raw, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatalf("file did not parse cleanly after concurrent writes: %v", err)
	}
	got := raw["appearance.theme"]
	ok := false
	for _, v := range values {
		if got == v {
			ok = true
			break
		}
	}
	if !ok {
		t.Errorf("final value %q not in %v (torn write?)", got, values)
	}
}

func TestSettings_Concurrent_ReadWriteAtomicity(t *testing.T) {
	// Codex PR #20 r3 P1: SettingsListIn must not observe partial YAML
	// during a concurrent SettingsSetIn write. Without the read lock,
	// os.WriteFile's truncate+write window allows yaml.Unmarshal to fail
	// on the half-written file. With settingsMu wrapping reads, the reader
	// either sees the old map or the new map, never partial.
	a := &API{}
	path := tmpSettings(t)
	if err := a.SettingsSetIn(path, "appearance.theme", "system"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var listErrs int64
	var setCount int64
	deadline := time.Now().Add(2 * time.Second)
	// 4 readers continuously list while 4 writers continuously toggle the theme.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if _, err := a.SettingsListIn(path); err != nil {
					atomic.AddInt64(&listErrs, 1)
				}
			}
		}()
	}
	values := []string{"light", "dark", "system"}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := 0
			for time.Now().Before(deadline) {
				_ = a.SettingsSetIn(path, "appearance.theme", values[n%3])
				n++
				atomic.AddInt64(&setCount, 1)
			}
		}(i)
	}
	wg.Wait()
	if listErrs != 0 {
		t.Errorf("SettingsListIn observed %d unmarshal failures across %d concurrent writes", listErrs, atomic.LoadInt64(&setCount))
	}
	if atomic.LoadInt64(&setCount) == 0 {
		t.Fatal("test did not exercise concurrent writes")
	}
}

func TestSettings_MigratesLegacyKeys(t *testing.T) {
	// Codex PR #20 r4 P2: pre-A4 gui-preferences.yaml files used
	// unqualified keys (theme, shell, default-home). After upgrade the
	// resolver expects canonical keys (appearance.theme etc.). Migration
	// happens transparently on first read and is persisted on next write.
	a := &API{}
	path := tmpSettings(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	seeded := []byte("theme: dark\nshell: bash\ndefault-home: /home/old\n")
	if err := os.WriteFile(path, seeded, 0600); err != nil {
		t.Fatal(err)
	}
	// SettingsListIn should expose the legacy values under canonical keys.
	all, err := a.SettingsListIn(path)
	if err != nil {
		t.Fatal(err)
	}
	if all["appearance.theme"] != "dark" {
		t.Errorf("expected appearance.theme=dark from legacy theme key, got %q", all["appearance.theme"])
	}
	if all["appearance.shell"] != "bash" {
		t.Errorf("expected appearance.shell=bash from legacy shell key, got %q", all["appearance.shell"])
	}
	if all["appearance.default_home"] != "/home/old" {
		t.Errorf("expected appearance.default_home=/home/old from legacy default-home key, got %q", all["appearance.default_home"])
	}
	// Trigger a write of an unrelated key — should persist migration.
	if err := a.SettingsSetIn(path, "appearance.density", "compact"); err != nil {
		t.Fatal(err)
	}
	raw, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy keys must be GONE from disk after the migration write.
	for _, legacy := range []string{"theme", "shell", "default-home"} {
		if _, has := raw[legacy]; has {
			t.Errorf("legacy key %q must be removed from disk after migration", legacy)
		}
	}
	// Canonical keys carry the migrated values.
	if raw["appearance.theme"] != "dark" {
		t.Errorf("canonical appearance.theme not persisted: %q", raw["appearance.theme"])
	}
}

func TestSettings_MigratesLegacyKeys_CanonicalWins(t *testing.T) {
	// If both legacy AND canonical key are present, canonical wins
	// (legacy is dropped). This is defensive — shouldn't happen in
	// practice but the migration must be deterministic.
	a := &API{}
	path := tmpSettings(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	seeded := []byte("theme: light\nappearance.theme: dark\n")
	if err := os.WriteFile(path, seeded, 0600); err != nil {
		t.Fatal(err)
	}
	all, err := a.SettingsListIn(path)
	if err != nil {
		t.Fatal(err)
	}
	if all["appearance.theme"] != "dark" {
		t.Errorf("expected dark (canonical wins over legacy), got %q", all["appearance.theme"])
	}
}

// contains is a tiny substring helper (avoids importing strings just here).
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		(len(haystack) > len(needle) && (haystack[:len(needle)] == needle ||
			haystack[len(haystack)-len(needle):] == needle ||
			indexOf(haystack, needle) >= 0)))
}
func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

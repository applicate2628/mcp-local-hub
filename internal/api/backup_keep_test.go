package api

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRawSettings plants a hand-edited gui-preferences.yaml that
// bypasses SettingsSet's validate() pass — useful for exercising the
// read-side fallback paths effectiveBackupKeepN must defend against.
func writeRawSettings(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write raw settings: %v", err)
	}
}

// TestEffectiveBackupKeepNFallbacks verifies the fallback chain when
// gui-preferences.yaml is missing, the key is unset, or the persisted
// value is invalid. The registry default (5) must win in each case so
// install/migrate never silently disable pruning because of a settings
// read hiccup.
func TestEffectiveBackupKeepNFallbacks(t *testing.T) {
	cases := []struct {
		name     string
		preset   string // empty = do not write the key
		writeBad bool   // write a non-integer to exercise atoi error path
		want     int
	}{
		{"missing key falls back to registry default", "", false, 5},
		{"explicit value 3 is honored", "3", false, 3},
		{"explicit value 0 disables pruning", "0", false, 0},
		{"non-integer falls back to registry default", "", true, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("LOCALAPPDATA", tmp)

			if tc.preset != "" {
				a := NewAPI()
				if err := a.SettingsSet("backups.keep_n", tc.preset); err != nil {
					t.Fatalf("SettingsSet: %v", err)
				}
			}
			if tc.writeBad {
				// Bypass SettingsSet's validate() to plant a bad value.
				// pre-A4 hand-edited files can land here; the registry
				// default must still win at read time.
				path := filepath.Join(tmp, "mcp-local-hub", "gui-preferences.yaml")
				writeRawSettings(t, path,"backups.keep_n: not-an-int\n")
			}

			a := NewAPI()
			if got := a.effectiveBackupKeepN(); got != tc.want {
				t.Errorf("effectiveBackupKeepN: got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEffectiveBackupKeepNRejectsNegative verifies that a negative
// persisted value (which validate() should have rejected, but a hand-
// edited yaml could carry) does not propagate down — backup_keep.go
// treats it as "use the safe default" rather than passing through to
// pruneOldTimestamped where keepN<0 would loop strangely.
func TestEffectiveBackupKeepNRejectsNegative(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	path := filepath.Join(tmp, "mcp-local-hub", "gui-preferences.yaml")
	writeRawSettings(t, path,"backups.keep_n: \"-1\"\n")

	a := NewAPI()
	if got := a.effectiveBackupKeepN(); got != 5 {
		t.Errorf("negative persisted value: got %d, want 5 (registry default)", got)
	}
}

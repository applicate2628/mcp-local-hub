// overlay_test.go — unit tests for Overlay struct + Load() parser
// (Task 2.2 of the v0.5.x Servers matrix revamp).
//
// Tests cover:
//   - Missing file → empty Overlay{Version:1, Daemons:{}} + nil error.
//   - YAML round-trip: write a YAML file with a single daemon row and
//     verify Load returns the parsed struct with the expected fields.

package daemon_env_overlay

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_MissingFile_ReturnsEmptyOverlay(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.yaml")

	got, err := Load(missing)
	if err != nil {
		t.Fatalf("Load(missing): unexpected error: %v", err)
	}
	if got == nil {
		t.Fatalf("Load(missing): got nil overlay, want empty overlay")
	}
	if got.Version != 1 {
		t.Errorf("Load(missing): Version = %d, want 1", got.Version)
	}
	if got.Daemons == nil {
		t.Errorf("Load(missing): Daemons is nil, want initialized empty map")
	}
	if len(got.Daemons) != 0 {
		t.Errorf("Load(missing): len(Daemons) = %d, want 0", len(got.Daemons))
	}
}

func TestLoad_YAMLRoundTrip_ParsesDaemonRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay.yaml")

	body := `version: 1
daemons:
  "\\mcp-local-hub-memory-default":
    env:
      Path: "C:/foo;${parent_path}"
    source: auto-discovery
    discovered_at: "2026-05-19T14:00:00Z"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load(path): unexpected error: %v", err)
	}
	if got == nil {
		t.Fatalf("Load(path): got nil overlay")
	}
	if got.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Version)
	}
	if len(got.Daemons) != 1 {
		t.Fatalf("len(Daemons) = %d, want 1", len(got.Daemons))
	}

	key := "\\mcp-local-hub-memory-default"
	row, ok := got.Daemons[key]
	if !ok {
		t.Fatalf("Daemons[%q] missing", key)
	}
	if got, want := row.Env["Path"], "C:/foo;${parent_path}"; got != want {
		t.Errorf("Env[Path] = %q, want %q", got, want)
	}
	if got, want := row.Source, "auto-discovery"; got != want {
		t.Errorf("Source = %q, want %q", got, want)
	}
	if got, want := row.DiscoveredAt, "2026-05-19T14:00:00Z"; got != want {
		t.Errorf("DiscoveredAt = %q, want %q", got, want)
	}
	if row.ModifiedAt != "" {
		t.Errorf("ModifiedAt = %q, want empty (omitted)", row.ModifiedAt)
	}
}


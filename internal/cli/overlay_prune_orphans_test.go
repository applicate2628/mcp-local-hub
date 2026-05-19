package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/api/daemon_env_overlay"
)

// TestPruneOrphanOverlayRowsRemovesOrphans seeds supervisor-intent.json
// with one daemon and overlay with TWO rows — one matching the intent
// and one stale orphan. After running prune, only the matching row
// must remain on disk.
func TestPruneOrphanOverlayRowsRemovesOrphans(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)

	intentJSON := `{
  "version": 1,
  "updated_at": "2026-05-19T12:00:00Z",
  "daemons": [
    {
      "task_name": "\\mcp-local-hub-foo-default",
      "server": "foo",
      "daemon": "default",
      "command": "foo.exe",
      "args": [],
      "port": 9001,
      "manifest_hash": ""
    }
  ],
  "strict_mode": false
}
`
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := os.WriteFile(intentPath, []byte(intentJSON), 0o600); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	overlayPath := filepath.Join(stateDir, "daemon-env-overrides.yaml")
	overlayYAML := `version: 1
daemons:
  \mcp-local-hub-foo-default:
    env:
      Path: "C:/foo/bin"
    source: operator
  \mcp-local-hub-zombie-default:
    env:
      Path: "C:/zombie/bin"
    source: operator
`
	if err := os.WriteFile(overlayPath, []byte(overlayYAML), 0o600); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	var buf bytes.Buffer
	if err := runOverlayPruneOrphans(stateDir, &buf); err != nil {
		t.Fatalf("runOverlayPruneOrphans: %v", err)
	}

	ov, err := daemon_env_overlay.Load(overlayPath)
	if err != nil {
		t.Fatalf("re-load overlay: %v", err)
	}
	if _, ok := ov.Daemons[`\mcp-local-hub-foo-default`]; !ok {
		t.Fatalf("foo row should survive; got keys %v", daemonKeys(ov.Daemons))
	}
	if _, ok := ov.Daemons[`\mcp-local-hub-zombie-default`]; ok {
		t.Fatalf("zombie row should have been pruned; got keys %v", daemonKeys(ov.Daemons))
	}

	out := buf.String()
	if !strings.Contains(out, "1 orphan") && !strings.Contains(out, "Removed 1") {
		t.Fatalf("output should mention 1 orphan removed; got: %q", out)
	}
	if !strings.Contains(out, "zombie") {
		t.Fatalf("output should list the pruned key; got: %q", out)
	}
}

// TestPruneOrphanOverlayRowsNoOp seeds intent and overlay with a matched
// pair. The command must detect no orphans and exit cleanly without
// rewriting the file.
func TestPruneOrphanOverlayRowsNoOp(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)

	intentJSON := `{
  "version": 1,
  "updated_at": "2026-05-19T12:00:00Z",
  "daemons": [
    {
      "task_name": "\\mcp-local-hub-foo-default",
      "server": "foo",
      "daemon": "default",
      "command": "foo.exe",
      "args": [],
      "port": 9001,
      "manifest_hash": ""
    }
  ],
  "strict_mode": false
}
`
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := os.WriteFile(intentPath, []byte(intentJSON), 0o600); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	overlayPath := filepath.Join(stateDir, "daemon-env-overrides.yaml")
	overlayYAML := `version: 1
daemons:
  \mcp-local-hub-foo-default:
    env:
      Path: "C:/foo/bin"
    source: operator
`
	if err := os.WriteFile(overlayPath, []byte(overlayYAML), 0o600); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	originalBytes, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatalf("read original overlay: %v", err)
	}

	var buf bytes.Buffer
	if err := runOverlayPruneOrphans(stateDir, &buf); err != nil {
		t.Fatalf("runOverlayPruneOrphans: %v", err)
	}

	if !strings.Contains(buf.String(), "nothing to prune") {
		t.Fatalf("expected 'nothing to prune' on no-op; got %q", buf.String())
	}

	afterBytes, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatalf("read overlay after: %v", err)
	}
	if !bytes.Equal(originalBytes, afterBytes) {
		t.Fatalf("overlay file must be unchanged on no-op")
	}
}

// TestPruneOrphanOverlayRowsMissingOverlay returns a clean message
// when no overlay file exists (and exits 0).
func TestPruneOrphanOverlayRowsMissingOverlay(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)

	var buf bytes.Buffer
	if err := runOverlayPruneOrphans(stateDir, &buf); err != nil {
		t.Fatalf("runOverlayPruneOrphans on empty state dir: %v", err)
	}
	if !strings.Contains(buf.String(), "no overlay") && !strings.Contains(buf.String(), "nothing to prune") {
		t.Fatalf("expected friendly message on missing overlay; got %q", buf.String())
	}
}

func daemonKeys(m map[string]daemon_env_overlay.DaemonRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

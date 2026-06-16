package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestParseGdbManifestNativeBridge pins the gdb manifest to the native Go
// GDB/MI bridge shape (command `mcphub gdb-bridge`, transport stdio-bridge).
// This replaced the former external GDB-MCP `uv run … server.py` python
// wrapper (commit eacb8f9): the wrapper's availability probe failed inside
// the console-less mcphub daemon and its lldb submodule needed python
// bindings absent from the uv venv. The old test asserted the python-wrapper
// args (command=uv, 6 base_args, an inline -c payload suppressing an LLDB
// warning); none of that exists anymore, so the assertions are rewritten to
// the native-bridge contract rather than left asserting a deleted shape.
func TestParseGdbManifestNativeBridge(t *testing.T) {
	b, err := os.ReadFile("../../servers/gdb/manifest.yaml")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	m, err := ParseManifest(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "gdb" {
		t.Errorf("name = %q, want gdb", m.Name)
	}
	if m.Command != "mcphub" {
		t.Errorf("command = %q, want mcphub (native bridge, not the old uv python wrapper)", m.Command)
	}
	if len(m.BaseArgs) != 1 || m.BaseArgs[0] != "gdb-bridge" {
		t.Fatalf("BaseArgs = %v, want [gdb-bridge]", m.BaseArgs)
	}
	if m.Transport != "stdio-bridge" {
		t.Errorf("transport = %q, want stdio-bridge", m.Transport)
	}
	// gdb must be a required binary — the bridge spawns it by absolute path.
	if !slices.Contains(m.RequiredBinaries, "gdb") {
		t.Errorf("RequiredBinaries = %v, want to contain gdb", m.RequiredBinaries)
	}
}

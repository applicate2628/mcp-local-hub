package config

import (
	"os"
	"strings"
	"testing"
)

func TestParseGdbManifestWithInlinePythonWrapper(t *testing.T) {
	b, err := os.ReadFile("../../servers/gdb/manifest.yaml")
	if err != nil { t.Fatalf("read: %v", err) }
	m, err := ParseManifest(strings.NewReader(string(b)))
	if err != nil { t.Fatalf("ParseManifest: %v", err) }
	if m.Name != "gdb" { t.Errorf("name = %q, want gdb", m.Name) }
	if m.Command != "uv" { t.Errorf("command = %q, want uv", m.Command) }
	// Expect 6 base_args: run, --directory, <path>, python, -c, <inline-python>
	if len(m.BaseArgs) != 6 {
		t.Fatalf("len(BaseArgs) = %d, want 6 (run, --directory, path, python, -c, <python>); got %v", len(m.BaseArgs), m.BaseArgs)
	}
	if m.BaseArgs[4] != "-c" {
		t.Errorf("BaseArgs[4] = %q, want -c", m.BaseArgs[4])
	}
	inline := m.BaseArgs[5]
	if strings.Contains(inline, "\n") {
		t.Errorf("BaseArgs[5] contains newline; want single-line -c payload for process snapshot safety: %q", inline)
	}
	for _, want := range []string{
				"LLDB Python module not available",
		"logging.basicConfig",
		"runpy.run_path",
	} {
		if !strings.Contains(inline, want) {
			t.Errorf("BaseArgs[5] missing %q; got %q", want, inline)
		}
	}
}

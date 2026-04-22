package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestGuiCmd_HelpIncludesFlags(t *testing.T) {
	cmd := newGuiCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"--port", "--no-browser", "--no-tray"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("--help missing %q; got %q", want, buf.String())
		}
	}
	// --force is intentionally hidden (Phase 3B-II placeholder); --help must NOT advertise it.
	if strings.Contains(buf.String(), "--force") {
		t.Errorf("--help unexpectedly advertises --force; should be hidden until take-over is implemented")
	}
}

// TestGuiCmd_ForceFlagStillParseable confirms `--force` is hidden but
// remains a valid flag (parseable, not removed). Hiding via MarkHidden
// keeps the wiring in place for Phase 3B-II without breaking any
// scripted callers that already pass --force.
func TestGuiCmd_ForceFlagStillParseable(t *testing.T) {
	cmd := newGuiCmdReal()
	if cmd.Flags().Lookup("force") == nil {
		t.Fatal("--force flag should still be defined (just hidden)")
	}
	if !cmd.Flags().Lookup("force").Hidden {
		t.Error("--force should be marked hidden")
	}
}

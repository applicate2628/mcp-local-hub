// internal/cli/symlink_consent_test.go
package cli

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestPromptInteractiveSymlinkConsent_Answers pins the [y/N] decision table:
// y/yes (any case) → true (follow); bare Enter, n, no, EOF, garbage → false
// (refuse, the default-N posture). Also asserts the prompt text names the
// client, the symlink path, and the pinned real target.
func TestPromptInteractiveSymlinkConsent_Answers(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"  y  \n", true},
		{"\n", false},   // bare Enter = default refuse
		{"n\n", false},
		{"no\n", false},
		{"", false},     // EOF with no input = refuse
		{"maybe\n", false},
	}
	for _, c := range cases {
		var out bytes.Buffer
		r := bufio.NewReader(strings.NewReader(c.in))
		got := promptInteractiveSymlinkConsent(&out, r, "codex-cli",
			"/home/u/.codex/config.toml", "/e/env/Agents/.codex/config.toml")
		if got != c.want {
			t.Errorf("input %q: got %v, want %v", c.in, got, c.want)
		}
		// Prompt must name client + symlink path + pinned target, and end [y/N].
		s := out.String()
		for _, frag := range []string{"codex-cli", "/home/u/.codex/config.toml", "/e/env/Agents/.codex/config.toml", "[y/N]"} {
			if !strings.Contains(s, frag) {
				t.Errorf("input %q: prompt %q missing %q", c.in, s, frag)
			}
		}
		// F2: the client name LEADS the line — no stray leading space from an
		// empty client subject.
		if !strings.HasPrefix(s, "codex-cli config ") {
			t.Errorf("input %q: prompt must lead with the client name, got %q", c.in, s)
		}
	}
}

// TestPromptInteractiveSymlinkConsent_EmptyClientNoLeadingSpace pins F2's
// defensive fallback: if the destination could not be attributed to a known
// client (derived client == ""), the line falls back to a generic "Client"
// subject and STILL has no stray leading space — never " config /path ...".
func TestPromptInteractiveSymlinkConsent_EmptyClientNoLeadingSpace(t *testing.T) {
	var out bytes.Buffer
	r := bufio.NewReader(strings.NewReader("n\n"))
	_ = promptInteractiveSymlinkConsent(&out, r, "",
		"/home/u/.codex/config.toml", "/e/env/Agents/.codex/config.toml")
	s := out.String()
	if strings.HasPrefix(s, " ") {
		t.Errorf("empty client produced a leading space: %q", s)
	}
	if !strings.HasPrefix(s, "Client config ") {
		t.Errorf("empty client must fall back to a generic \"Client\" subject, got %q", s)
	}
}

// TestInstallInteractiveSymlinkConsent_NonInteractive_InstallsNothing pins the
// automation-safety contract: a non-terminal stdin (a pipe / regular file)
// installs NO port — the existing refusal stands, and the restore is a no-op.
func TestInstallInteractiveSymlinkConsent_NonInteractive_InstallsNothing(t *testing.T) {
	// Ensure a clean baseline and restore after.
	prev := api.InteractiveSymlinkConsent
	t.Cleanup(func() { api.InteractiveSymlinkConsent = prev })
	api.InteractiveSymlinkConsent = nil

	// A regular file is not a terminal.
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	var out bytes.Buffer
	restore := installInteractiveSymlinkConsent(&out, f)
	if api.InteractiveSymlinkConsent != nil {
		t.Errorf("non-interactive stdin must NOT install the consent port (automation safety)")
	}
	restore() // must be a safe no-op
	if api.InteractiveSymlinkConsent != nil {
		t.Errorf("restore left a non-nil port")
	}
}

// TestInstallInteractiveSymlinkConsent_NilStdin_InstallsNothing pins that a nil
// stdin (defensive) installs nothing.
func TestInstallInteractiveSymlinkConsent_NilStdin_InstallsNothing(t *testing.T) {
	prev := api.InteractiveSymlinkConsent
	t.Cleanup(func() { api.InteractiveSymlinkConsent = prev })
	api.InteractiveSymlinkConsent = nil

	var out bytes.Buffer
	restore := installInteractiveSymlinkConsent(&out, nil)
	if api.InteractiveSymlinkConsent != nil {
		t.Errorf("nil stdin must NOT install the consent port")
	}
	restore()
}

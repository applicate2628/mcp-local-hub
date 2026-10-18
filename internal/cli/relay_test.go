package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

const (
	relayManifestContextHelperEnv = "MCPHUB_RELAY_MANIFEST_CONTEXT_HELPER"
	relayManifestCanonicalPathEnv = "MCPHUB_RELAY_MANIFEST_CANONICAL_PATH"
)

// TestResolveRelayURL_DirectURL accepts an explicit --url and returns
// it verbatim.
func TestResolveRelayURL_DirectURL(t *testing.T) {
	u, err := resolveRelayURL("", "", "http://localhost:9999/mcp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "http://localhost:9999/mcp" {
		t.Errorf("url = %q, want http://localhost:9999/mcp", u)
	}
}

// TestResolveRelayURL_MutuallyExclusive rejects mixed flag usage so
// misconfigured invocations fail fast with a clear message.
func TestResolveRelayURL_MutuallyExclusive(t *testing.T) {
	_, err := resolveRelayURL("serena", "claude", "http://x/mcp")
	if err == nil {
		t.Fatal("expected error for --url combined with --server/--daemon")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutual exclusion: %v", err)
	}
}

// TestResolveRelayURL_ResolvesFromEmbeddedManifest verifies that
// --server/--daemon reads the manifest from the binary's embedded FS.
// This is the regression guard for the original report: the relay
// failed with "open R:\Temp\servers\serena\manifest.yaml: no such
// file or directory" when the binary was invoked from %TEMP%, because
// the old implementation looked for manifests on disk relative to the
// executable path.
func TestResolveRelayURL_ResolvesFromEmbeddedManifest(t *testing.T) {
	// Post-2026-05-20 serena manifest consolidation: single daemon
	// "unified" on port 9121 with --context codex (see
	// servers/serena/manifest.yaml header for the architectural
	// rationale). Resolves through the embed FS.
	u, err := resolveRelayURL("serena", "unified", "")
	if err != nil {
		t.Fatalf("resolveRelayURL(serena, unified): %v", err)
	}
	if !strings.Contains(u, ":9121/mcp") {
		t.Errorf("url = %q, want ...:9121/mcp (serena.unified port)", u)
	}
}

// Catches relay selecting a disk manifest from the invoking executable's
// sibling/parent instead of the canonical API manifest owner. The two helper
// processes execute the same image through canonical-like and npm-like paths;
// conflicting npm-side decoys make an executable-relative fallback observable.
func TestResolveRelayURL_CanonicalManifestAcrossExecutableContexts(t *testing.T) {
	if os.Getenv(relayManifestContextHelperEnv) == "1" {
		canonical := os.Getenv(relayManifestCanonicalPathEnv)
		if canonical == "" {
			t.Fatal("helper canonical path is empty")
		}
		t.Cleanup(api.SetTestCanonicalMcphubPath(canonical))
		resolved, err := resolveRelayURL("relay-context-fixture", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("RELAY_CONTEXT_URL=%s\n", resolved)
		return
	}

	root := t.TempDir()
	canonicalExecutable := filepath.Join(root, "canonical", "bin", "mcphub-context-test.exe")
	npmExecutable := filepath.Join(root, "npm", "node_modules", "mcp-local-hub", "bin", "mcphub-context-test.exe")
	materializeRelayContextTestImage(t, canonicalExecutable)
	materializeRelayContextTestImage(t, npmExecutable)

	const canonicalPort = 9471
	writeRelayContextManifest(t, filepath.Join(filepath.Dir(canonicalExecutable), "servers"), canonicalPort)
	// Both npm executable-relative candidates exist with wrong ports. The
	// superseded resolver selects the sibling first; a parent fallback is also a
	// forbidden split-root candidate and stays covered by the fixture.
	writeRelayContextManifest(t, filepath.Join(filepath.Dir(npmExecutable), "servers"), 9472)
	writeRelayContextManifest(t, filepath.Join(filepath.Dir(npmExecutable), "..", "servers"), 9473)
	want := fmt.Sprintf("RELAY_CONTEXT_URL=http://127.0.0.1:%d/mcp", canonicalPort)

	for _, context := range []struct {
		name       string
		executable string
	}{
		{name: "canonical-like", executable: canonicalExecutable},
		{name: "npm-like", executable: npmExecutable},
	} {
		t.Run(context.name, func(t *testing.T) {
			cmd := exec.Command(context.executable, "-test.run=^TestResolveRelayURL_CanonicalManifestAcrossExecutableContexts$")
			cmd.Env = append(os.Environ(),
				relayManifestContextHelperEnv+"=1",
				relayManifestCanonicalPathEnv+"="+canonicalExecutable,
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("relay context helper: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), want) {
				t.Fatalf("relay context selected non-canonical manifest; want %q in:\n%s", want, output)
			}
		})
	}
}

// TestResolveRelayURL_MissingFlags rejects invocations with neither
// --url nor --server/--daemon set.
func TestResolveRelayURL_MissingFlags(t *testing.T) {
	cases := []struct {
		server, daemon string
	}{
		{"", ""},
		{"serena", ""},
		{"", "claude"},
	}
	for _, c := range cases {
		_, err := resolveRelayURL(c.server, c.daemon, "")
		if err == nil {
			t.Errorf("expected error for server=%q daemon=%q", c.server, c.daemon)
		}
	}
}

func writeRelayContextManifest(t *testing.T, manifestRoot string, port int) {
	t.Helper()
	dir := filepath.Join(manifestRoot, "relay-context-fixture")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`name: relay-context-fixture
kind: global
transport: stdio-bridge
command: go
base_args:
  - version
daemons:
  - name: default
    port: %d
`, port)
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func materializeRelayContextTestImage(t *testing.T, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, target); err == nil {
		return
	}
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

package clients

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestClientConfigSandboxEnvironment_ContainsEveryConstructedAdapter(t *testing.T) {
	root := t.TempDir()
	restoreEnv := ApplyClientConfigSandboxEnvironment(root)
	t.Cleanup(restoreEnv)

	for _, entry := range ClientConfigSandboxEnvironment(root) {
		value, set := os.LookupEnv(entry.Key)
		if entry.Unset {
			if set {
				t.Fatalf("%s remains set to %q, want descriptor to unset it", entry.Key, value)
			}
			continue
		}
		if !set || !withinSandboxRoot(normalizeSandboxPath(value), normalizeSandboxPath(root)) {
			t.Fatalf("%s=%q is not redirected under %q", entry.Key, value, root)
		}
	}

	escapes := installReportModeAudit(t, root)
	_ = AllClients()
	if got := escapes(); got != 0 {
		t.Fatalf("descriptor left %d adapter config path escape(s)", got)
	}
}

func TestClientConfigSandboxEnvironment_MissingRedirectFailsClosed(t *testing.T) {
	root := t.TempDir()
	restoreEnv := ApplyClientConfigSandboxEnvironment(root)
	t.Cleanup(restoreEnv)

	key := "XDG_CONFIG_HOME"
	if runtime.GOOS == "windows" {
		key = "APPDATA"
	}
	// This path is deliberately synthetic and never opened: ConfigPathForName
	// reaches the audit immediately after construction, before any client file
	// I/O. It proves a missing descriptor redirect fails closed without touching
	// an operator path.
	outside := filepath.Join(string(filepath.Separator), "mcp-local-hub-sandbox-negative", "outside")
	if runtime.GOOS == "windows" {
		outside = `Z:\mcp-local-hub-sandbox-negative\outside`
	}
	if err := os.Setenv(key, outside); err != nil {
		t.Fatalf("set synthetic missing redirect %s: %v", key, err)
	}

	escapes := installReportModeAudit(t, root)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ConfigPathForName("vscode")
	}()
	<-done
	if got := escapes(); got != 1 {
		t.Fatalf("missing %s redirect produced %d audit escapes, want 1", key, got)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("synthetic outside path was unexpectedly present or inaccessible after audit: %v", err)
	}
}

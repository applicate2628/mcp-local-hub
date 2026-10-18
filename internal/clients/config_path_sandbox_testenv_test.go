package clients

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestClientConfigSandboxEnvironment_ContainsEveryConstructedAdapter(t *testing.T) {
	root := t.TempDir()
	restoreEnv, err := ApplyClientConfigSandboxEnvironment(root)
	if err != nil {
		t.Fatal(err)
	}
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
		if info, err := os.Stat(entry.Value); err != nil || !info.IsDir() {
			t.Fatalf("declared sandbox directory %s not ready: %v", entry.Key, err)
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
	restoreEnv, err := ApplyClientConfigSandboxEnvironment(root)
	if err != nil {
		t.Fatal(err)
	}
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

func TestClientConfigSandboxEnvironment_UnsetsCODEXHOMEAndRestoresIt(t *testing.T) {
	outside := t.TempDir()
	t.Setenv("CODEX_HOME", outside)
	root := t.TempDir()
	restoreEnv, err := ApplyClientConfigSandboxEnvironment(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restoreEnv)

	if value, present := os.LookupEnv("CODEX_HOME"); present {
		t.Fatalf("CODEX_HOME remains set to %q inside sandbox", value)
	}
	client, err := NewCodexCLI()
	if err != nil {
		t.Fatalf("construct Codex adapter from sandbox: %v", err)
	}
	if got, want := client.ConfigPath(), filepath.Join(root, ".codex", "config.toml"); got != want {
		t.Fatalf("Codex config path = %q, want sandbox path %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(outside, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("outside CODEX_HOME config was unexpectedly touched: %v", err)
	}

	restoreEnv()
	if value, present := os.LookupEnv("CODEX_HOME"); !present || value != outside {
		t.Fatalf("CODEX_HOME after restore = %q, present=%v; want %q, true", value, present, outside)
	}
}

func TestClientConfigSandboxDirectories_PreparationAndFailureKeepParentEnvironment(t *testing.T) {
	before := os.Environ()
	root := t.TempDir()
	if err := PrepareClientConfigSandboxDirectories(root); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(os.Environ(), before) {
		t.Fatal("child namespace preparation mutated parent environment")
	}

	blockedRoot := t.TempDir()
	blocker := filepath.Join(blockedRoot, "AppData")
	if err := os.WriteFile(blocker, []byte("caller-owned-blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	restore, err := ApplyClientConfigSandboxEnvironment(blockedRoot)
	if restore != nil {
		t.Cleanup(restore)
	}
	if err == nil || restore != nil {
		t.Fatalf("blocked namespace was admitted: restore=%t error=%v", restore != nil, err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("namespace failure lost original filesystem error: %v", err)
	}
	if !reflect.DeepEqual(os.Environ(), before) {
		t.Fatal("failed namespace changed parent environment")
	}
	if raw, readErr := os.ReadFile(blocker); readErr != nil || string(raw) != "caller-owned-blocker" {
		t.Fatalf("preparation deleted or changed caller-owned root/content: %v", readErr)
	}
}

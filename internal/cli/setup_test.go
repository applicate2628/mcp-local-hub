package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestDirOnPath exercises the PATH-env-var parser used by the install
// command to decide whether ~/.local/bin is already on PATH. The real
// targetDirOnPath wraps this over os.Getenv; exercising the splitter
// directly keeps the test independent of the running process's PATH.
//
// Test inputs are built at runtime to match the host's PATH conventions
// (drive-letter colons collide with the POSIX `:` separator on Linux,
// so a literal `C:\...` entry would split inside the colon and break
// the assertion).
func TestDirOnPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	target := `C:\Users\dima_\.local\bin`
	other1, other2 := `C:\Go\bin`, `C:\Windows`
	mixedCaseEntry := `C:\Users\dima_/.local/bin`
	if runtime.GOOS != "windows" {
		// POSIX-style entries: `:` is the separator, so absolute POSIX
		// paths split cleanly. The function still needs to see a
		// non-trivial path for the "absent" / "single entry" cases.
		target = "/home/u/.local/bin"
		other1, other2 = "/usr/local/go/bin", "/usr/bin"
		mixedCaseEntry = "/home/u/.local/bin" // POSIX is case-sensitive; same string still matches
	}

	cases := []struct {
		name    string
		dir     string
		pathEnv string
		want    bool
	}{
		{
			name:    "present in middle",
			dir:     target,
			pathEnv: strings.Join([]string{other1, target, other2}, sep),
			want:    true,
		},
		{
			name:    "absent",
			dir:     target,
			pathEnv: strings.Join([]string{other1, other2}, sep),
			want:    false,
		},
		{
			name:    "empty PATH",
			dir:     target,
			pathEnv: "",
			want:    false,
		},
		{
			name:    "trailing separator (empty entry tolerated)",
			dir:     target,
			pathEnv: target + sep,
			want:    true,
		},
		{
			name:    "single entry match",
			dir:     target,
			pathEnv: target,
			want:    true,
		},
	}

	if runtime.GOOS == "windows" {
		// samePath folds case + slashes on Windows — a mixed-case /
		// separator entry must still match the canonical target.
		cases = append(cases, struct {
			name    string
			dir     string
			pathEnv string
			want    bool
		}{
			name:    "mixed slashes and case (Windows semantics)",
			dir:     target,
			pathEnv: mixedCaseEntry,
			want:    true,
		})
	}

	for _, tc := range cases {
		got := dirOnPath(tc.dir, tc.pathEnv)
		if got != tc.want {
			t.Errorf("%s: dirOnPath(%q, %q) = %v, want %v",
				tc.name, tc.dir, tc.pathEnv, got, tc.want)
		}
	}
}

func TestRunSetupLSPClientRouterWiring_ReportsEnsureResult(t *testing.T) {
	prior := setupLSPClientRouterFn
	defer func() { setupLSPClientRouterFn = prior }()

	var gotRollback bool
	setupLSPClientRouterFn = func(rollback bool) (*api.LSPClientRouterReport, error) {
		gotRollback = rollback
		return &api.LSPClientRouterReport{
			Backups: []api.LSPClientRouterBackup{{Client: "codex-cli", Path: "config.toml.bak-mcp-local-hub-test"}},
			Applied: []api.LSPClientRouterChange{{
				Client: "codex-cli", Language: "go", EntryName: "mcp-language-server-go", URL: "http://127.0.0.1:9125/lsp/go/mcp",
			}},
		}, nil
	}

	var out bytes.Buffer
	if err := runSetupLSPClientRouter(&out, false); err != nil {
		t.Fatalf("runSetupLSPClientRouter: %v", err)
	}
	if gotRollback {
		t.Fatal("setup wiring called rollback path")
	}
	text := out.String()
	for _, want := range []string{
		"backup before LSP router wiring",
		"codex-cli",
		"mcp-language-server-go",
		"http://127.0.0.1:9125/lsp/go/mcp",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func TestRunSetupLSPClientRouterRollback_ReportsRestoreResult(t *testing.T) {
	prior := setupLSPClientRouterFn
	defer func() { setupLSPClientRouterFn = prior }()

	var gotRollback bool
	setupLSPClientRouterFn = func(rollback bool) (*api.LSPClientRouterReport, error) {
		gotRollback = rollback
		return &api.LSPClientRouterReport{
			Backups: []api.LSPClientRouterBackup{{Client: "codex-cli", Path: "config.toml.bak-mcp-local-hub-rollback"}},
			Restored: []api.LSPClientRouterChange{{
				Client: "codex-cli", Language: "go", EntryName: "mcp-language-server-go",
			}},
		}, nil
	}

	var out bytes.Buffer
	if err := runSetupLSPClientRouter(&out, true); err != nil {
		t.Fatalf("runSetupLSPClientRouter rollback: %v", err)
	}
	if !gotRollback {
		t.Fatal("setup rollback did not call rollback path")
	}
	text := out.String()
	for _, want := range []string{
		"backup before LSP router rollback",
		"restored codex-cli entry mcp-language-server-go",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func TestSetupCommand_ContinuesToWatchdogWhenLSPRouterWiringFails(t *testing.T) {
	origBootstrap := setupBootstrapFn
	origRouter := setupLSPClientRouterFn
	origWatchdog := setupWatchdogFn
	t.Cleanup(func() {
		setupBootstrapFn = origBootstrap
		setupLSPClientRouterFn = origRouter
		setupWatchdogFn = origWatchdog
	})

	var bootstrapCalls int
	setupBootstrapFn = func(w io.Writer) error {
		bootstrapCalls++
		_, _ = io.WriteString(w, "bootstrap ok\n")
		return nil
	}
	setupLSPClientRouterFn = func(rollback bool) (*api.LSPClientRouterReport, error) {
		if rollback {
			t.Fatal("setup must not take rollback path")
		}
		return &api.LSPClientRouterReport{
			Failed: []api.LSPClientRouterFailure{{
				Client: "codex-cli", Language: "go", EntryName: "mcp-language-server-go", Op: "write", Err: "malformed config",
			}},
		}, errors.New("LSP router client config failed")
	}
	var watchdogCalls int
	setupWatchdogFn = func(w io.Writer, allowElevated bool) error {
		watchdogCalls++
		if allowElevated {
			t.Fatal("allowElevated = true, want false")
		}
		_, _ = io.WriteString(w, "watchdog ok\n")
		return nil
	}

	cmd := newSetupCmdReal()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("setup command returned error: %v", err)
	}
	if bootstrapCalls != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", bootstrapCalls)
	}
	if watchdogCalls != 1 {
		t.Fatalf("watchdog calls = %d, want 1 despite LSP router warning", watchdogCalls)
	}
	if !strings.Contains(stderr.String(), "warning: LSP router wiring failed") {
		t.Fatalf("stderr missing LSP router warning; got %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "watchdog ok") {
		t.Fatalf("stdout missing watchdog output; got %q", stdout.String())
	}
}

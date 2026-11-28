package cli

import (
	"bytes"
	"errors"
	"github.com/spf13/cobra"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestDirOnPath exercises the PATH-env-var parser used by the install
// command to decide whether ~/.local/bin is already on PATH. The real
// targetDirOnPath wraps this over os.Getenv; exercising the splitter
// directly keeps the test independent of the running process's PATH.
func TestDirOnPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	root := t.TempDir()
	target := filepath.Join(root, ".local", "bin")
	other1 := filepath.Join(root, "tools", "bin")
	other2 := filepath.Join(root, "system")

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
			pathEnv: strings.ToUpper(filepath.ToSlash(target)),
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

// setupTrustedRootTestEnv redirects the daemon state dir to a fresh
// per-test temp tree (0700, so the parent-DACL/mode read gate passes on
// POSIX and Windows alike — same posture as the api-package
// trustedRootsTestDir helper) and stubs the bootstrap + watchdog steps so
// `mcphub setup` neither copies a binary nor touches Task Scheduler. It
// returns the resolved store path so a test can load + assert the store
// after the command runs. NEVER touches the real %LOCALAPPDATA% store.
func setupTrustedRootTestEnv(t *testing.T) string {
	t.Helper()

	restore := api.SetDaemonStateRootForTest(t.TempDir())
	t.Cleanup(restore)

	origBootstrap := setupBootstrapFn
	origWatchdog := setupWatchdogFn
	origRouter := setupLSPClientRouterFn
	t.Cleanup(func() {
		setupBootstrapFn = origBootstrap
		setupWatchdogFn = origWatchdog
		setupLSPClientRouterFn = origRouter
	})
	setupBootstrapFn = func(io.Writer) error { return nil }
	setupWatchdogFn = func(io.Writer, bool) error { return nil }
	// Avoid touching real client configs / scanning the host.
	setupLSPClientRouterFn = func(bool) (*api.LSPClientRouterReport, error) {
		return &api.LSPClientRouterReport{}, nil
	}

	path, err := api.DefaultLSPTrustedRootsPath()
	if err != nil {
		t.Fatalf("resolve trusted-roots store path: %v", err)
	}
	return path
}

func stubUnelevatedSetup(t *testing.T) {
	t.Helper()
	prev := setupIsElevatedFn
	setupIsElevatedFn = func() (bool, error) { return false, nil }
	t.Cleanup(func() { setupIsElevatedFn = prev })
}

func runSetupCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	stubUnelevatedSetup(t)
	cmd := newSetupCmdReal()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errBuf.String(), err
}

func TestSetupCommand_TrustedRootBlessesAbsolutePath(t *testing.T) {
	storePath := setupTrustedRootTestEnv(t)

	// A real directory so canonicalization (EvalSymlinks best-effort)
	// resolves cleanly, matching a production workspace root.
	root := t.TempDir()

	stdout, _, err := runSetupCmd(t, "--trusted-root", root)
	if err != nil {
		t.Fatalf("setup --trusted-root %q: %v", root, err)
	}
	if !strings.Contains(stdout, "Blessed LSP trusted root") {
		t.Fatalf("stdout missing bless confirmation; got %q", stdout)
	}

	f, err := api.LoadDefaultLSPTrustedRoots()
	if err != nil {
		t.Fatalf("reload trusted-roots store: %v", err)
	}
	if !f.LSPWorkspaceRootTrusted(root) {
		t.Fatalf("root %q should be trusted after bless; store path %s, roots %v",
			root, storePath, f.Roots)
	}
}

func TestSetupCleanUsesExistingProductUpgradeOnly(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Product setup only")
	}
	previousUpgrade, previousBootstrap := upgradeDispatcher, setupBootstrapFn
	t.Cleanup(func() { upgradeDispatcher, setupBootstrapFn = previousUpgrade, previousBootstrap })
	cut := errors.New("owned clean setup handoff")
	calls := 0
	upgradeDispatcher = func(cmd *cobra.Command) error {
		calls++
		if cmd.Name() != "setup" {
			t.Error("clean setup changed caller")
		}
		return cut
	}
	setupBootstrapFn = func(io.Writer) error {
		t.Error("clean Product setup reached ordinary client-changing bootstrap")
		return nil
	}
	cmd := newSetupCmdReal()
	cmd.SetArgs([]string{"--clean"})
	if err := cmd.Execute(); !errors.Is(err, cut) || calls != 1 {
		t.Fatal("clean setup did not select the existing Product upgrade owner", err)
	}
	cmd = newSetupCmdReal()
	cmd.SetArgs([]string{"--clean", "--rollback-lsp-router"})
	if err := cmd.Execute(); err == nil || calls != 1 {
		t.Fatal("clean option allowed a client-changing setup operation", err)
	}
	if err := recoverUnpromotedUpgrade(t.Context(), UpgradeOpts{requirePriorAbsent: true}, 0, "", cut); !errors.Is(err, cut) {
		t.Fatal("failed clean preflight did not preserve absent predecessor state", err)
	}
}
func TestSetupCommand_TrustedRootRejectsRelativePath(t *testing.T) {
	storePath := setupTrustedRootTestEnv(t)

	_, _, err := runSetupCmd(t, "--trusted-root", "relative/path")
	if err == nil {
		t.Fatal("setup --trusted-root with a relative path must error")
	}
	if !strings.Contains(err.Error(), "LSP_TRUSTED_ROOTS_NOT_ABSOLUTE") {
		t.Fatalf("error should carry LSP_TRUSTED_ROOTS_NOT_ABSOLUTE code; got %v", err)
	}

	// The pre-flight validation rejects BEFORE any store write — the store
	// must not exist (no file created by a rejected command).
	if _, statErr := os.Stat(storePath); !os.IsNotExist(statErr) {
		t.Fatalf("rejected relative-path command must not write the store; stat err = %v", statErr)
	}
}

func TestSetupCommand_TrustedRootIdempotentRebless(t *testing.T) {
	setupTrustedRootTestEnv(t)
	root := t.TempDir()

	// First bless.
	if _, _, err := runSetupCmd(t, "--trusted-root", root); err != nil {
		t.Fatalf("first bless: %v", err)
	}
	first, err := api.LoadDefaultLSPTrustedRoots()
	if err != nil {
		t.Fatalf("reload after first bless: %v", err)
	}

	// Re-bless the same root — must be a no-op success (no duplicate row,
	// no error).
	if _, _, err := runSetupCmd(t, "--trusted-root", root); err != nil {
		t.Fatalf("idempotent re-bless: %v", err)
	}
	second, err := api.LoadDefaultLSPTrustedRoots()
	if err != nil {
		t.Fatalf("reload after re-bless: %v", err)
	}
	if len(second.Roots) != len(first.Roots) {
		t.Fatalf("re-bless changed root count: first %v, second %v", first.Roots, second.Roots)
	}
	if !second.LSPWorkspaceRootTrusted(root) {
		t.Fatalf("root %q should still be trusted after idempotent re-bless; roots %v", root, second.Roots)
	}
}

func TestSetupCommand_ContinuesToWatchdogWhenLSPRouterWiringFails(t *testing.T) {
	stubUnelevatedSetup(t)
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

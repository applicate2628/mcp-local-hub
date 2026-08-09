package clients

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installReportModeAudit installs the audit in report mode (never panics) for the
// duration of the test and returns the escape-count accessor. Report mode is used
// throughout this file because these tests DELIBERATELY drive escapes; fail-fast
// mode would abort the binary, which is exactly what it is supposed to do in a
// real fixture.
func installReportModeAudit(t *testing.T, roots ...string) func() int {
	t.Helper()
	t.Setenv(SandboxAuditModeEnv, "report")
	restore := EnforceSandboxedConfigPaths(roots...)
	var count int
	var restored bool
	t.Cleanup(func() {
		if !restored {
			count = restore()
		}
	})
	return func() int {
		if !restored {
			count = restore()
			restored = true
		}
		return count
	}
}

func TestSandboxAudit_FlagsAdapterResolvedOutsideSandbox(t *testing.T) {
	// A sandbox that contains nothing: every real adapter path is an escape.
	sandbox := t.TempDir()
	escapes := installReportModeAudit(t, sandbox)

	// AllClients is the seam production code uses; it must be audited.
	//
	// Run it on a SEPARATE goroutine: report mode aborts the escaping goroutine
	// with runtime.Goexit (see SandboxAuditModeEnv), which would end this test
	// before it could assert. Goexit runs the goroutine's defers, so the channel
	// close still fires.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = AllClients()
	}()
	<-done

	if n := escapes(); n == 0 {
		t.Fatal("audit recorded 0 escapes, but every registered adapter resolves outside an empty sandbox")
	}
}

func TestSandboxAudit_SilentWhenEveryAdapterIsSandboxed(t *testing.T) {
	home := t.TempDir()
	// Reproduce the isolation an internal/cli fixture installs: home roots plus
	// every non-home config root the registry reads. This is the POSITIVE
	// control — if it fires, the audit has a false-positive problem and the
	// guard is unusable.
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("ProgramData", filepath.Join(home, "ProgramData"))
	t.Setenv("MIMOCODE_TEST_MANAGED_CONFIG_DIR", filepath.Join(home, "ProgramData", "opencode"))
	for _, k := range []string{
		"COPILOT_HOME", "KIMI_CODE_HOME",
		"MIMOCODE_HOME", "MIMOCODE_CONFIG", "MIMOCODE_CONFIG_DIR", "MIMOCODE_CONFIG_CONTENT",
	} {
		t.Setenv(k, "")
	}

	escapes := installReportModeAudit(t, home)
	_ = AllClients()

	if n := escapes(); n != 0 {
		t.Errorf("audit recorded %d escape(s) for a fully sandboxed environment; "+
			"a false positive here would make every fixture unrunnable (see stderr dump above)", n)
	}
}

func TestSandboxAudit_MessageNamesAdapterPathEnvVarAndTestFrame(t *testing.T) {
	// The message is the whole deliverable: without adapter + path + env var +
	// test frame, the next developer cannot act on the failure.
	escaped := filepath.Join(string(filepath.Separator)+"nowhere", "Roaming", "Code", "User", "mcp.json")
	if runtime.GOOS == "windows" {
		escaped = `Z:\nowhere\Roaming\Code\User\mcp.json`
		t.Setenv("SANDBOX_AUDIT_PROBE_ROOT", `Z:\nowhere\Roaming`)
	} else {
		t.Setenv("SANDBOX_AUDIT_PROBE_ROOT", filepath.Dir(filepath.Dir(filepath.Dir(escaped))))
	}

	msg := formatSandboxEscape("vscode", escaped, []string{normalizeSandboxPath(t.TempDir())})

	for _, want := range []string{
		"vscode",                    // the adapter
		escaped,                     // the resolved path
		"SANDBOX_AUDIT_PROBE_ROOT",  // the env var responsible
		"config_path_sandbox_audit", // the pointer to the owning contract
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("escape message missing %q:\n%s", want, msg)
		}
	}
	// The _test.go frame is captured from the live call stack, so this very
	// function must appear.
	if !strings.Contains(msg, "config_path_sandbox_audit_test.go") {
		t.Errorf("escape message names no _test.go frame:\n%s", msg)
	}
}

func TestSandboxAudit_EnvCulpritIsDerivedNotEnumerated(t *testing.T) {
	// A brand-new env var no list in this repo knows about must still be named.
	// This is what makes the audit an allowlist rather than the denylist that
	// went stale twice.
	root := filepath.Join(t.TempDir(), "brand-new-client-root")
	t.Setenv("A_CLIENT_ENV_VAR_INVENTED_TODAY", root)

	got := sandboxEnvCulprits(filepath.Join(root, "config", "mcp.json"))
	var found bool
	for _, c := range got {
		if c.key == "A_CLIENT_ENV_VAR_INVENTED_TODAY" {
			found = true
		}
	}
	if !found {
		t.Errorf("culprit set %v does not name the unknown env var that owns the path", got)
	}
}

func TestSandboxAudit_WithinRootRespectsPathBoundary(t *testing.T) {
	sep := string(filepath.Separator)
	root := normalizeSandboxPath(filepath.Join("C:"+sep, "Temp"))
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join("C:"+sep, "Temp"), true},
		{filepath.Join("C:"+sep, "Temp", "x", "y.json"), true},
		{filepath.Join("C:"+sep, "Temp2", "y.json"), false},
		{filepath.Join("C:"+sep, "Other", "y.json"), false},
	}
	for _, c := range cases {
		if got := withinSandboxRoot(normalizeSandboxPath(c.path), root); got != c.want {
			t.Errorf("withinSandboxRoot(%q, %q) = %v, want %v", c.path, root, got, c.want)
		}
	}
}

func TestSandboxAudit_NilAuditorIsTheProductionDefault(t *testing.T) {
	// Production must never carry an installed auditor: the seam is one atomic
	// load, and ConfigPath() is not even called.
	if a := activeSandboxAuditor.Load(); a != nil {
		t.Fatal("an auditor is installed at package scope; production would pay for it and " +
			"internal/clients' own real-path derivation tests would fail")
	}
}

// BenchmarkAllClients_AuditDisabled is the production cost: AllClients with no
// auditor installed. BenchmarkAllClients_AuditEnabled is the test-binary cost.
// The delta is what the guard charges per AllClients() call.
func BenchmarkAllClients_AuditDisabled(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = AllClients()
	}
}

// BenchmarkConstructionSeam_Nil isolates what a RELEASE binary pays: 47 nil
// atomic loads per AllClients() call, and no ConfigPath() call at all.
func BenchmarkConstructionSeam_Nil(b *testing.B) {
	if activeSandboxAuditor.Load() != nil {
		b.Fatal("an auditor is installed; this benchmark measures the production path")
	}
	c, err := NewClaudeCode()
	if err != nil {
		b.Skipf("claude-code factory unavailable: %v", err)
	}
	n := len(clientRegistry()) // hoisted: building the registry would dominate
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < n; j++ {
			auditConstructedClient("claude-code", c)
		}
	}
}

func BenchmarkAllClients_AuditEnabled(b *testing.B) {
	// Root the sandbox at the filesystem root so every adapter passes the check
	// without recording — the happy path, which is the one that runs on every
	// call. (os.Getenv walking only happens on a violation.)
	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = filepath.VolumeName(os.Getenv("SystemDrive")) + string(filepath.Separator)
		if root == string(filepath.Separator) {
			root = `C:\`
		}
	}
	restore := EnforceSandboxedConfigPaths(root)
	defer restore()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = AllClients()
	}
}

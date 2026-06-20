//go:build linux

// Linux autostart backend tests. Every test injects a fake systemctl
// callback via systemctlFn so we never shell out to the real
// `systemctl --user` and never write into the developer's actual
// `~/.config/systemd/user/`.
//
// The unit-path resolver is also swapped to a `t.TempDir()` location
// via unitPathFn so multiple tests can run in parallel against
// isolated filesystems.
package autostart

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// systemctlCall records one invocation of `systemctl --user <args>`
// for assertions. The whole-argv slice is captured so the test can
// pin operator-visible behavior like `daemon-reload` ordering and
// `enable --now` argument composition.
type systemctlCall struct {
	Args   []string
	Stdout string
	Stderr string
	Err    error
}

// fakeSystemctl exposes a record-and-stub seam: tests preload
// responses by argv prefix; the runner records every call for
// assertion. Unknown prefixes default to exit-0 with empty output.
type fakeSystemctl struct {
	calls     []systemctlCall
	responses map[string]systemctlCall // key = strings.Join(args, " ")
}

func (f *fakeSystemctl) Run(args []string) (string, string, error) {
	key := strings.Join(args, " ")
	resp := systemctlCall{}
	if r, ok := f.responses[key]; ok {
		resp = r
	}
	f.calls = append(f.calls, systemctlCall{
		Args:   append([]string{}, args...),
		Stdout: resp.Stdout,
		Stderr: resp.Stderr,
		Err:    resp.Err,
	})
	return resp.Stdout, resp.Stderr, resp.Err
}

func newFakeSystemctl() *fakeSystemctl {
	return &fakeSystemctl{responses: map[string]systemctlCall{}}
}

// withFakeSystemctl installs the fake systemctl runner and a
// `t.TempDir()`-rooted unit-path resolver. Restores both seams on
// cleanup.
func withFakeSystemctl(t *testing.T, fs *fakeSystemctl) string {
	t.Helper()
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "mcphub-supervisor.service")

	prevExec := systemctlFn
	prevPath := unitPathFn
	prevLoginctl := loginctlFn
	systemctlFn = fs.Run
	unitPathFn = func() (string, error) { return unitPath, nil }
	// Stub loginctl so Enable's best-effort `loginctl enable-linger` (F3)
	// never shells out to the REAL loginctl during a test (which would enable
	// lingering for the CI user). Tests that assert the linger call override
	// loginctlFn themselves AFTER this helper.
	loginctlFn = func([]string) (string, string, error) { return "", "", nil }
	t.Cleanup(func() {
		systemctlFn = prevExec
		unitPathFn = prevPath
		loginctlFn = prevLoginctl
	})
	return unitPath
}

func TestLinuxBackend_EnableWritesUnit(t *testing.T) {
	fs := newFakeSystemctl()
	unitPath := withFakeSystemctl(t, fs)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{MCPHubPath: "/usr/local/bin/mcphub"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	s := string(body)
	must := []string{
		"[Unit]",
		"Description=MCP Local Hub supervisor",
		"After=default.target",
		"[Service]",
		"Type=simple",
		`ExecStart="/usr/local/bin/mcphub" supervise`,
		"Restart=on-failure",
		"RestartSec=5",
		"[Install]",
		"WantedBy=default.target",
	}
	for _, m := range must {
		if !strings.Contains(s, m) {
			t.Errorf("unit body missing %q\nbody:\n%s", m, s)
		}
	}
	if strings.Contains(s, "--strict-mode") {
		t.Errorf("non-strict Enable still wrote --strict-mode flag\nbody:\n%s", s)
	}
}

func TestLinuxBackend_EnableStrictMode(t *testing.T) {
	fs := newFakeSystemctl()
	unitPath := withFakeSystemctl(t, fs)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{StrictMode: true, MCPHubPath: "/usr/local/bin/mcphub"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `ExecStart="/usr/local/bin/mcphub" supervise --strict-mode`) {
		t.Errorf("strict-mode Enable did not encode the flag\nbody:\n%s", s)
	}
}

// TestRenderLinuxUnit_QuotesExecStartPathWithSpaces is the FIX 1 (bot r32 P3)
// regression guard. systemd.service(5) splits an unquoted ExecStart command
// line on whitespace, so a binary path containing a space
// (e.g. `/home/me/My Apps/mcphub`) would be parsed as exec
// `/home/me/My` with argv[1] `Apps/mcphub` — the unit cannot start. The fix
// double-quotes the path so the first argv token is the full path and
// ` supervise` stays OUTSIDE the quotes as a separate argument.
//
// Pre-fix (raw `"ExecStart=" + mcphubPath + " supervise"`) this test FAILS:
// the line would be `ExecStart=/home/me/My Apps/mcphub supervise` with the path
// unquoted.
func TestRenderLinuxUnit_QuotesExecStartPathWithSpaces(t *testing.T) {
	const spaced = "/home/me/My Apps/mcphub"

	// Non-strict: path double-quoted, ` supervise` outside the quotes.
	body := renderLinuxUnit(spaced, false)
	wantNonStrict := `ExecStart="/home/me/My Apps/mcphub" supervise`
	if !strings.Contains(body, wantNonStrict) {
		t.Fatalf("non-strict ExecStart not quoted; want line containing %q\nbody:\n%s", wantNonStrict, body)
	}
	// ` supervise` must be OUTSIDE the closing quote: the closing quote of the
	// path is immediately followed by ` supervise`, not engulfed by it.
	execLine := extractExecStartLine(t, body)
	if !strings.HasPrefix(execLine, `ExecStart="`+spaced+`"`) {
		t.Fatalf("ExecStart path not wrapped in a single quoted token: %q", execLine)
	}
	if !strings.HasSuffix(execLine, `" supervise`) {
		t.Fatalf("` supervise` not OUTSIDE the closing quote (it is a separate argv token): %q", execLine)
	}
	// The first argv token, when systemd unquotes it, is the full spaced path —
	// there is exactly one `"`-delimited segment and it equals the path.
	if first := firstSystemdToken(execLine); first != spaced {
		t.Fatalf("first ExecStart argv token = %q, want the full path %q", first, spaced)
	}

	// Strict-mode: ` --strict-mode` appended AFTER ` supervise`, both outside quotes.
	strictBody := renderLinuxUnit(spaced, true)
	wantStrict := `ExecStart="/home/me/My Apps/mcphub" supervise --strict-mode`
	if !strings.Contains(strictBody, wantStrict) {
		t.Fatalf("strict ExecStart not quoted with flag; want line containing %q\nbody:\n%s", wantStrict, strictBody)
	}

	// Negative control: a space-free path still renders a working ExecStart.
	// Quoting a space-free path is harmless — systemd unquotes it identically.
	plainBody := renderLinuxUnit("/usr/local/bin/mcphub", false)
	plainLine := extractExecStartLine(t, plainBody)
	if !strings.HasPrefix(plainLine, "ExecStart=") {
		t.Fatalf("space-free ExecStart line does not start with ExecStart=: %q", plainLine)
	}
	if !strings.Contains(plainLine, "/usr/local/bin/mcphub") || !strings.Contains(plainLine, " supervise") {
		t.Fatalf("space-free ExecStart missing path or ` supervise`: %q", plainLine)
	}
}

// extractExecStartLine returns the sole ExecStart= line from a rendered unit.
func extractExecStartLine(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			return line
		}
	}
	t.Fatalf("no ExecStart= line in unit body:\n%s", body)
	return ""
}

// firstSystemdToken extracts the first double-quoted argument value from an
// ExecStart line (the executable path), reversing the `\"` / `\\` escapes
// systemd would unescape. It assumes the path is quoted (the FIX 1 contract).
func firstSystemdToken(execLine string) string {
	rest := strings.TrimPrefix(execLine, "ExecStart=")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	var b strings.Builder
	i := 1
	for i < len(rest) {
		c := rest[i]
		if c == '\\' && i+1 < len(rest) {
			b.WriteByte(rest[i+1])
			i += 2
			continue
		}
		if c == '"' {
			break
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func TestLinuxBackend_EnableRunsSystemctl(t *testing.T) {
	fs := newFakeSystemctl()
	withFakeSystemctl(t, fs)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{MCPHubPath: "/usr/local/bin/mcphub"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	// Expected ordering: daemon-reload first, then enable --now.
	if len(fs.calls) < 2 {
		t.Fatalf("expected ≥2 systemctl calls, got %d: %+v", len(fs.calls), fs.calls)
	}
	first := strings.Join(fs.calls[0].Args, " ")
	second := strings.Join(fs.calls[1].Args, " ")
	if !strings.Contains(first, "daemon-reload") {
		t.Errorf("first systemctl call = %q, want daemon-reload", first)
	}
	if !strings.Contains(second, "enable") || !strings.Contains(second, "--now") || !strings.Contains(second, "mcphub-supervisor.service") {
		t.Errorf("second systemctl call = %q, want enable --now mcphub-supervisor.service", second)
	}
}

// TestLinuxBackend_EnableEnablesLinger asserts Enable runs `loginctl
// enable-linger` (F3) after enabling the unit, so the supervisor survives
// logout.
func TestLinuxBackend_EnableEnablesLinger(t *testing.T) {
	fs := newFakeSystemctl()
	withFakeSystemctl(t, fs) // installs a no-op loginctl stub; we override it next
	var lingerArgs []string
	prev := loginctlFn
	loginctlFn = func(args []string) (string, string, error) {
		lingerArgs = append([]string{}, args...)
		return "", "", nil
	}
	t.Cleanup(func() { loginctlFn = prev })

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{MCPHubPath: "/usr/local/bin/mcphub"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if len(lingerArgs) == 0 || lingerArgs[0] != "enable-linger" {
		t.Fatalf("expected `loginctl enable-linger ...` call, got %v", lingerArgs)
	}
}

// TestLinuxBackend_EnableLingerFailureNonFatal asserts a linger failure
// (missing loginctl / permission denied on a hardened host) does NOT fail
// Enable — the unit is already enabled; linger is best-effort.
func TestLinuxBackend_EnableLingerFailureNonFatal(t *testing.T) {
	fs := newFakeSystemctl()
	withFakeSystemctl(t, fs)
	prev := loginctlFn
	loginctlFn = func([]string) (string, string, error) {
		return "", "Failed to enable linger: Access denied", errors.New("exit status 1")
	}
	t.Cleanup(func() { loginctlFn = prev })

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{MCPHubPath: "/usr/local/bin/mcphub"}); err != nil {
		t.Fatalf("Enable must not fail on a best-effort linger error: %v", err)
	}
}

func TestLinuxBackend_DisableRemovesUnit(t *testing.T) {
	fs := newFakeSystemctl()
	unitPath := withFakeSystemctl(t, fs)
	// Pre-create unit so Disable has something to remove.
	if err := os.WriteFile(unitPath, []byte("# placeholder unit\n"), 0o600); err != nil {
		t.Fatalf("pre-write unit: %v", err)
	}

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unit still exists after Disable; stat err = %v", err)
	}
	// systemctl --user disable --now MUST run.
	found := false
	for _, c := range fs.calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "disable") && strings.Contains(joined, "--now") {
			found = true
		}
	}
	if !found {
		t.Errorf("systemctl disable --now never called; calls = %+v", fs.calls)
	}
}

func TestLinuxBackend_DisableIdempotent(t *testing.T) {
	fs := newFakeSystemctl()
	withFakeSystemctl(t, fs)
	// No pre-existing unit; Disable should still succeed.

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := b.Disable(); err != nil {
			t.Errorf("Disable iter %d: %v", i, err)
		}
	}
}

func TestLinuxBackend_StatusActive(t *testing.T) {
	fs := newFakeSystemctl()
	unitPath := withFakeSystemctl(t, fs)
	mustWriteUnit(t, unitPath, "/usr/local/bin/mcphub", false)
	fs.responses["--user is-active mcphub-supervisor.service"] = systemctlCall{Stdout: "active\n"}
	fs.responses["--user is-enabled mcphub-supervisor.service"] = systemctlCall{Stdout: "enabled\n"}

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: "/usr/local/bin/mcphub"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateEnabledRunning {
		t.Errorf("Status = %s, want %s", got, StateEnabledRunning)
	}
}

func TestLinuxBackend_StatusEnabledButInactive(t *testing.T) {
	fs := newFakeSystemctl()
	unitPath := withFakeSystemctl(t, fs)
	mustWriteUnit(t, unitPath, "/usr/local/bin/mcphub", false)
	// Non-zero exit is the systemctl idiom for "inactive": exit 3.
	fs.responses["--user is-active mcphub-supervisor.service"] = systemctlCall{
		Stdout: "inactive\n",
		Err:    &exitErr{code: 3},
	}
	fs.responses["--user is-enabled mcphub-supervisor.service"] = systemctlCall{Stdout: "enabled\n"}

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: "/usr/local/bin/mcphub"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateEnabledStopped {
		t.Errorf("Status = %s, want %s", got, StateEnabledStopped)
	}
}

func TestLinuxBackend_StatusAbsent(t *testing.T) {
	fs := newFakeSystemctl()
	withFakeSystemctl(t, fs) // unitPath in t.TempDir() but never created.

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: "/usr/local/bin/mcphub"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateAbsent {
		t.Errorf("Status = %s, want %s", got, StateAbsent)
	}
}

func TestLinuxBackend_StatusDriftedStrictMode(t *testing.T) {
	fs := newFakeSystemctl()
	unitPath := withFakeSystemctl(t, fs)
	mustWriteUnit(t, unitPath, "/usr/local/bin/mcphub", false) // recorded WITHOUT strict
	fs.responses["--user is-active mcphub-supervisor.service"] = systemctlCall{Stdout: "active\n"}
	fs.responses["--user is-enabled mcphub-supervisor.service"] = systemctlCall{Stdout: "enabled\n"}

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{StrictMode: true, MCPHubPath: "/usr/local/bin/mcphub"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateDrifted {
		t.Errorf("Status = %s, want %s (strict-mode flag missing from unit)", got, StateDrifted)
	}
}

func TestLinuxBackend_StatusDriftedPath(t *testing.T) {
	fs := newFakeSystemctl()
	unitPath := withFakeSystemctl(t, fs)
	mustWriteUnit(t, unitPath, "/old/mcphub", false)
	fs.responses["--user is-active mcphub-supervisor.service"] = systemctlCall{Stdout: "active\n"}
	fs.responses["--user is-enabled mcphub-supervisor.service"] = systemctlCall{Stdout: "enabled\n"}

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{MCPHubPath: "/new/mcphub"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateDrifted {
		t.Errorf("Status = %s, want %s (binary path mismatch)", got, StateDrifted)
	}
}

func TestLinuxBackend_StatusSnapshotSpecFingerprintTracksUnitAndEnabledButNotLiveness(t *testing.T) {
	fs := newFakeSystemctl()
	unitPath := withFakeSystemctl(t, fs)
	mustWriteUnit(t, unitPath, "/usr/local/bin/mcphub", false)
	fs.responses["--user is-enabled mcphub-supervisor.service"] = systemctlCall{Stdout: "enabled\n"}
	fs.responses["--user is-active mcphub-supervisor.service"] = systemctlCall{Stdout: "active\n"}

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opts := Options{MCPHubPath: "/usr/local/bin/mcphub"}
	first, err := b.StatusSnapshot(opts)
	if err != nil {
		t.Fatalf("StatusSnapshot first: %v", err)
	}
	if first.SpecFingerprint == "" {
		t.Fatal("StatusSnapshot SpecFingerprint is empty, want installed unit fingerprint")
	}
	second, err := b.StatusSnapshot(opts)
	if err != nil {
		t.Fatalf("StatusSnapshot second: %v", err)
	}
	if second.SpecFingerprint != first.SpecFingerprint {
		t.Fatalf("SpecFingerprint unstable across identical re-probes: first=%q second=%q", first.SpecFingerprint, second.SpecFingerprint)
	}

	mustWriteUnit(t, unitPath, "/usr/local/bin/mcphub", true)
	unitChanged, err := b.StatusSnapshot(opts)
	if err != nil {
		t.Fatalf("StatusSnapshot after unit change: %v", err)
	}
	if unitChanged.SpecFingerprint == first.SpecFingerprint {
		t.Fatal("SpecFingerprint did not change when only the unit ExecStart arguments changed")
	}

	mustWriteUnit(t, unitPath, "/usr/local/bin/mcphub", false)
	fs.responses["--user is-enabled mcphub-supervisor.service"] = systemctlCall{Stdout: "disabled\n"}
	enabledChanged, err := b.StatusSnapshot(opts)
	if err != nil {
		t.Fatalf("StatusSnapshot after enabled change: %v", err)
	}
	if enabledChanged.SpecFingerprint == first.SpecFingerprint {
		t.Fatal("SpecFingerprint did not change when only systemctl is-enabled changed")
	}

	fs.responses["--user is-enabled mcphub-supervisor.service"] = systemctlCall{Stdout: "enabled\n"}
	fs.responses["--user is-active mcphub-supervisor.service"] = systemctlCall{
		Stdout: "inactive\n",
		Err:    &exitErr{code: 3},
	}
	livenessChanged, err := b.StatusSnapshot(opts)
	if err != nil {
		t.Fatalf("StatusSnapshot after liveness change: %v", err)
	}
	if livenessChanged.SpecFingerprint != first.SpecFingerprint {
		t.Fatalf("SpecFingerprint changed for liveness-only change: base=%q inactive=%q", first.SpecFingerprint, livenessChanged.SpecFingerprint)
	}
}

// mustWriteUnit produces the same unit body the production renderer
// emits, so Status tests run against a representative on-disk shape.
func mustWriteUnit(t *testing.T, path string, mcphubPath string, strictMode bool) {
	t.Helper()
	body := renderLinuxUnit(mcphubPath, strictMode)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write unit: %v", err)
	}
}

// exitErr is a minimal error implementation that mimics the
// non-zero-exit shape `os/exec.ExitError` carries — autostart's
// Linux code only cares about err != nil, not the concrete type, but
// keeping a distinct type makes tests self-documenting.
type exitErr struct {
	code int
}

func (e *exitErr) Error() string {
	return errExitMsg(e.code)
}

func errExitMsg(code int) string {
	return "exit status " + strings.TrimSpace(strconv.Itoa(code))
}

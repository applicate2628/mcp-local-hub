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
	systemctlFn = fs.Run
	unitPathFn = func() (string, error) { return unitPath, nil }
	t.Cleanup(func() {
		systemctlFn = prevExec
		unitPathFn = prevPath
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
		"ExecStart=/usr/local/bin/mcphub supervise",
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
	if !strings.Contains(s, "ExecStart=/usr/local/bin/mcphub supervise --strict-mode") {
		t.Errorf("strict-mode Enable did not encode the flag\nbody:\n%s", s)
	}
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

//go:build darwin

// macOS autostart backend tests. Every test injects a fake launchctl
// callback via launchctlFn so we never shell out to the real launchctl
// and never write into the developer's actual `~/Library/LaunchAgents`.
package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type launchctlCall struct {
	Args   []string
	Stdout string
	Stderr string
	Err    error
}

type fakeLaunchctl struct {
	calls     []launchctlCall
	responses map[string]launchctlCall
}

func (f *fakeLaunchctl) Run(args []string) (string, string, error) {
	key := strings.Join(args, " ")
	resp := launchctlCall{}
	if r, ok := f.responses[key]; ok {
		resp = r
	}
	f.calls = append(f.calls, launchctlCall{
		Args:   append([]string{}, args...),
		Stdout: resp.Stdout,
		Stderr: resp.Stderr,
		Err:    resp.Err,
	})
	return resp.Stdout, resp.Stderr, resp.Err
}

func newFakeLaunchctl() *fakeLaunchctl {
	return &fakeLaunchctl{responses: map[string]launchctlCall{}}
}

func withFakeLaunchctl(t *testing.T, fl *fakeLaunchctl) string {
	t.Helper()
	dir := t.TempDir()
	plistPath := filepath.Join(dir, DarwinPlistFileName)

	prevExec := launchctlFn
	prevPath := plistPathFn
	prevUID := currentUIDFn
	launchctlFn = fl.Run
	plistPathFn = func() (string, error) { return plistPath, nil }
	currentUIDFn = func() string { return "501" }
	t.Cleanup(func() {
		launchctlFn = prevExec
		plistPathFn = prevPath
		currentUIDFn = prevUID
	})
	return plistPath
}

func TestDarwinBackend_EnableWritesPlist(t *testing.T) {
	fl := newFakeLaunchctl()
	plistPath := withFakeLaunchctl(t, fl)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{MCPHubPath: "/usr/local/bin/mcphub"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	body, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	s := string(body)
	must := []string{
		`<?xml version="1.0"`,
		`<!DOCTYPE plist`,
		`<plist version="1.0">`,
		`<key>Label</key>`,
		`<string>` + DarwinLabel + `</string>`,
		`<key>ProgramArguments</key>`,
		`<string>/usr/local/bin/mcphub</string>`,
		`<string>supervise</string>`,
		`<key>RunAtLoad</key>`,
		`<true/>`,
		`<key>KeepAlive</key>`,
	}
	for _, m := range must {
		if !strings.Contains(s, m) {
			t.Errorf("plist body missing %q\nbody:\n%s", m, s)
		}
	}
	if strings.Contains(s, "--strict-mode") {
		t.Errorf("non-strict Enable wrote --strict-mode\nbody:\n%s", s)
	}
}

func TestDarwinBackend_EnableStrictMode(t *testing.T) {
	fl := newFakeLaunchctl()
	plistPath := withFakeLaunchctl(t, fl)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{StrictMode: true, MCPHubPath: "/usr/local/bin/mcphub"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	body, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	if !strings.Contains(string(body), `<string>--strict-mode</string>`) {
		t.Errorf("strict-mode Enable did not encode the flag\nbody:\n%s", body)
	}
}

func TestDarwinBackend_EnableRunsLaunchctlBootstrap(t *testing.T) {
	fl := newFakeLaunchctl()
	withFakeLaunchctl(t, fl)

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Enable(Options{MCPHubPath: "/usr/local/bin/mcphub"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	found := false
	for _, c := range fl.calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "bootstrap") && strings.Contains(joined, "gui/501") {
			found = true
		}
	}
	if !found {
		t.Errorf("launchctl bootstrap gui/501 never called; calls = %+v", fl.calls)
	}
}

func TestDarwinBackend_DisableRemovesPlist(t *testing.T) {
	fl := newFakeLaunchctl()
	plistPath := withFakeLaunchctl(t, fl)
	if err := os.WriteFile(plistPath, []byte("<plist/>\n"), 0o600); err != nil {
		t.Fatalf("pre-write plist: %v", err)
	}

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("plist still exists after Disable; stat err = %v", err)
	}
	found := false
	for _, c := range fl.calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "bootout") {
			found = true
		}
	}
	if !found {
		t.Errorf("launchctl bootout never called; calls = %+v", fl.calls)
	}
}

func TestDarwinBackend_DisableIdempotent(t *testing.T) {
	fl := newFakeLaunchctl()
	withFakeLaunchctl(t, fl)

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

func TestDarwinBackend_StatusRunning(t *testing.T) {
	fl := newFakeLaunchctl()
	plistPath := withFakeLaunchctl(t, fl)
	body := renderDarwinPlist("/usr/local/bin/mcphub", false)
	if err := os.WriteFile(plistPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	fl.responses["print gui/501/"+DarwinLabel] = launchctlCall{Stdout: "state = running\n"}

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

func TestDarwinBackend_StatusEnabledStopped(t *testing.T) {
	fl := newFakeLaunchctl()
	plistPath := withFakeLaunchctl(t, fl)
	body := renderDarwinPlist("/usr/local/bin/mcphub", false)
	if err := os.WriteFile(plistPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	// launchctl print returns "state = not running" when the agent
	// is loaded but the process isn't currently alive.
	fl.responses["print gui/501/"+DarwinLabel] = launchctlCall{Stdout: "state = not running\n"}

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

func TestDarwinBackend_StatusAbsent(t *testing.T) {
	fl := newFakeLaunchctl()
	withFakeLaunchctl(t, fl) // plistPath never created.

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

func TestDarwinBackend_StatusDriftedStrictMode(t *testing.T) {
	fl := newFakeLaunchctl()
	plistPath := withFakeLaunchctl(t, fl)
	body := renderDarwinPlist("/usr/local/bin/mcphub", false) // recorded WITHOUT strict
	if err := os.WriteFile(plistPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	fl.responses["print gui/501/"+DarwinLabel] = launchctlCall{Stdout: "state = running\n"}

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Status(Options{StrictMode: true, MCPHubPath: "/usr/local/bin/mcphub"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != StateDrifted {
		t.Errorf("Status = %s, want %s (strict-mode flag mismatch)", got, StateDrifted)
	}
}

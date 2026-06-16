package cli

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/oneapi"
)

// pathFamilyValue returns the single PATH-family value from an
// os.Environ()-style env block using case-insensitive lookup on Windows.
// Fails the test if zero or >1 PATH-family entries exist (the merge must
// produce exactly one).
func pathFamilyValue(t *testing.T, env []string) string {
	t.Helper()
	var found []string
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		if strings.EqualFold(kv[:eq], "PATH") {
			found = append(found, kv[eq+1:])
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 PATH-family entry, got %d: %v (env=%v)", len(found), found, env)
	}
	return found[0]
}

// envValue returns the value for an exact key (case-sensitive) or "" + false.
func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		if kv[:eq] == key {
			return kv[eq+1:], true
		}
	}
	return "", false
}

// TestOneAPIInjectPrependsPATHAndSetsMKLROOT pins the core injection
// contract: a target daemon's composed env receives the oneAPI PATH
// PREPENDED (oneAPI dirs in front of the daemon's existing PATH, so the
// original tail survives) and the new MKLROOT, while every other base /
// overlay var is retained.
func TestOneAPIInjectPrependsPATHAndSetsMKLROOT(t *testing.T) {
	sep := ";"
	if runtime.GOOS != "windows" {
		sep = ":"
	}
	baselinePATH := "C:" + sep + "base"
	// Daemon's composed env BEFORE oneAPI (parent<manifest<overlay already
	// merged). Includes an operator-overlaid PATH and an unrelated overlay key.
	cmdEnv := []string{
		"PATH=" + baselinePATH,
		"OVERLAY_KEY=op",
		"BASE_KEY=keepme",
	}
	// Delta as setvars would produce: PATH = "<oneAPI dirs><sep><baselinePATH>"
	// (oneAPI prepended to the inherited PATH), plus a new MKLROOT.
	oneAPIDirs := "C:" + sep + "oneapi" + sep + "mkl" + sep + "bin"
	delta := map[string]string{
		"PATH":    oneAPIDirs + sep + baselinePATH,
		"MKLROOT": "C:" + sep + "oneapi" + sep + "mkl",
	}

	got, applied := injectOneAPIEnv(cmdEnv, delta, baselinePATH)

	// PATH must be oneAPI prefix + the daemon's (overlaid) PATH.
	wantPATH := oneAPIDirs + sep + baselinePATH
	if v := pathFamilyValue(t, got); v != wantPATH {
		t.Fatalf("injected PATH = %q, want %q (oneAPI prepended onto base tail)", v, wantPATH)
	}
	// MKLROOT must be set.
	if v, ok := envValue(got, "MKLROOT"); !ok || v != "C:"+sep+"oneapi"+sep+"mkl" {
		t.Fatalf("MKLROOT = (%q, %v), want set", v, ok)
	}
	// Overlay + base keys must survive untouched.
	if v, ok := envValue(got, "OVERLAY_KEY"); !ok || v != "op" {
		t.Fatalf("OVERLAY_KEY lost: (%q, %v)", v, ok)
	}
	if v, ok := envValue(got, "BASE_KEY"); !ok || v != "keepme" {
		t.Fatalf("BASE_KEY lost: (%q, %v)", v, ok)
	}
	// applied list must be exactly the keys we touched.
	wantApplied := map[string]bool{"PATH": true, "MKLROOT": true}
	if len(applied) != 2 || !wantApplied[applied[0]] || !wantApplied[applied[1]] {
		t.Fatalf("applied = %v, want [MKLROOT PATH] (sorted)", applied)
	}
}

// TestOneAPIInjectOperatorOverlayWinsForNonPATH asserts the documented
// precedence: a non-PATH oneAPI var is NOT injected when the operator
// overlay already set that key (overlay wins), but PATH is still
// prepend-merged.
func TestOneAPIInjectOperatorOverlayWinsForNonPATH(t *testing.T) {
	sep := ";"
	if runtime.GOOS != "windows" {
		sep = ":"
	}
	baselinePATH := "C:" + sep + "base"
	cmdEnv := []string{
		"PATH=" + baselinePATH,
		"MKLROOT=C:" + sep + "operator-mkl", // operator explicitly set this
	}
	oneAPIDirs := "C:" + sep + "oneapi" + sep + "bin"
	delta := map[string]string{
		"PATH":    oneAPIDirs + sep + baselinePATH,
		"MKLROOT": "C:" + sep + "oneapi-mkl", // oneAPI value — must NOT clobber operator
	}

	got, applied := injectOneAPIEnv(cmdEnv, delta, baselinePATH)

	if v, ok := envValue(got, "MKLROOT"); !ok || v != "C:"+sep+"operator-mkl" {
		t.Fatalf("MKLROOT = (%q, %v), want operator value preserved (overlay wins)", v, ok)
	}
	// PATH still gets the oneAPI prefix.
	wantPATH := oneAPIDirs + sep + baselinePATH
	if v := pathFamilyValue(t, got); v != wantPATH {
		t.Fatalf("PATH = %q, want %q (prepend even when other var collides)", v, wantPATH)
	}
	// applied must contain PATH but NOT MKLROOT (it was skipped).
	for _, k := range applied {
		if k == "MKLROOT" {
			t.Fatalf("applied includes MKLROOT but it should have been skipped (overlay collision): %v", applied)
		}
	}
}

// TestOneAPIInjectEmptyDeltaIsNoOp asserts an empty delta returns the env
// unchanged with no applied keys.
func TestOneAPIInjectEmptyDeltaIsNoOp(t *testing.T) {
	cmdEnv := []string{"PATH=C:/base", "X=1"}
	got, applied := injectOneAPIEnv(cmdEnv, nil, "C:/base")
	if len(applied) != 0 {
		t.Fatalf("applied = %v, want empty for nil delta", applied)
	}
	if len(got) != len(cmdEnv) {
		t.Fatalf("env changed for nil delta: %v", got)
	}
}

// TestOneAPIInjectorApplies pins the target-set + enabled gating: only
// enabled injectors with a non-empty delta and a target server apply.
func TestOneAPIInjectorApplies(t *testing.T) {
	delta := map[string]string{"MKLROOT": "x"}
	cases := []struct {
		name string
		inj  *oneAPIInjector
		srv  string
		want bool
	}{
		{"nil-injector", nil, "gdb", false},
		{"disabled", &oneAPIInjector{Enabled: false, Delta: delta, Targets: defaultOneAPITargetServers}, "gdb", false},
		{"enabled-gdb", &oneAPIInjector{Enabled: true, Delta: delta, Targets: defaultOneAPITargetServers}, "gdb", true},
		{"enabled-lldb", &oneAPIInjector{Enabled: true, Delta: delta, Targets: defaultOneAPITargetServers}, "lldb", true},
		{"enabled-nontarget", &oneAPIInjector{Enabled: true, Delta: delta, Targets: defaultOneAPITargetServers}, "memory", false},
		{"enabled-empty-delta", &oneAPIInjector{Enabled: true, Delta: nil, Targets: defaultOneAPITargetServers}, "gdb", false},
	}
	for _, c := range cases {
		if got := c.inj.applies(c.srv); got != c.want {
			t.Fatalf("%s: applies(%q) = %v, want %v", c.name, c.srv, got, c.want)
		}
	}
}

// TestOneAPIBuildInjectorDisabledByEnv asserts MCPHUB_DISABLE_ONEAPI_PATH=1
// produces a disabled injector even when setvars would be detectable.
func TestOneAPIBuildInjectorDisabledByEnv(t *testing.T) {
	t.Setenv(oneapi.DisableEnvVar, "1")
	restore := oneapi.SetSeamsForTest(
		func(string) bool { return true }, // setvars "exists"
		func(_ context.Context, _ string) (string, error) {
			t.Fatalf("CaptureEnvDelta must not run when disabled")
			return "", nil
		},
		func() []string { return []string{"PATH=C:/base"} },
	)
	defer restore()

	inj := buildOneAPIInjector(nil)
	if inj == nil || inj.Enabled {
		t.Fatalf("buildOneAPIInjector with disable env = %+v, want disabled", inj)
	}
	if inj.applies("gdb") {
		t.Fatalf("disabled injector still applies to gdb")
	}
}

// TestOneAPIBuildInjectorNotDetectedIsNoOp asserts a host without setvars
// yields a disabled injector (zero behavior change on non-oneAPI hosts).
func TestOneAPIBuildInjectorNotDetectedIsNoOp(t *testing.T) {
	t.Setenv(oneapi.DisableEnvVar, "")
	restore := oneapi.SetSeamsForTest(
		func(string) bool { return false }, // nothing on disk
		nil,
		nil,
	)
	defer restore()

	inj := buildOneAPIInjector(nil)
	if inj.Enabled {
		t.Fatalf("not-detected host: injector Enabled=true, want false")
	}
	if inj.applies("gdb") {
		t.Fatalf("not-detected host: applies(gdb)=true, want false")
	}
}

// TestOneAPIBuildInjectorCaptureFailureEmitsWarn asserts that a detected
// setvars whose capture fails yields a disabled injector AND emits the
// warn event so the operator sees the degradation.
func TestOneAPIBuildInjectorCaptureFailureEmitsWarn(t *testing.T) {
	t.Setenv(oneapi.DisableEnvVar, "")
	// On platforms where setvarsProbePaths is empty (POSIX), DetectSetvars
	// returns ("",false) regardless of the fileExists seam, so this test
	// only meaningfully exercises the capture-failure branch on Windows.
	if runtime.GOOS != "windows" {
		t.Skip("DetectSetvars has no probe candidates on non-Windows; capture branch unreachable")
	}
	t.Setenv("ONEAPI_ROOT", "C:\\fake-oneapi")
	restore := oneapi.SetSeamsForTest(
		func(string) bool { return true }, // setvars "exists"
		func(_ context.Context, _ string) (string, error) {
			return "", errors.New("setvars boom")
		},
		func() []string { return []string{"PATH=C:\\base"} },
	)
	defer restore()

	events := newCapturingEventLog(t)
	inj := buildOneAPIInjector(events.log)
	if inj.Enabled {
		t.Fatalf("capture failure: injector Enabled=true, want false (no inject)")
	}
	if !events.sawEvent("oneapi-env-capture-failed") {
		t.Fatalf("capture failure did not emit oneapi-env-capture-failed warn event")
	}
}

// --- tiny capturing event-log helper (file-backed, drained after) -----------

type capturingEvents struct {
	t    *testing.T
	log  *api.SupervisorEventLog
	path string
}

func newCapturingEventLog(t *testing.T) *capturingEvents {
	t.Helper()
	path := filepath.Join(t.TempDir(), "supervisor-events.log")
	log, err := api.OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatalf("OpenSupervisorEventLog: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return &capturingEvents{t: t, log: log, path: path}
}

// sawEvent reports whether an event with the given discriminator was
// written to the log file. The file is created lazily on first Emit, so an
// absent file means "no events emitted".
func (c *capturingEvents) sawEvent(name string) bool {
	data := readFileStringIfExists(c.t, c.path)
	return strings.Contains(data, "\"event\":\""+name+"\"")
}

package cli

import (
	"os"
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

// TestOneAPIInjectPrependsDLLDirsOntoPATH pins the core injection contract:
// a target daemon's composed env receives the oneAPI DLL dirs PREPENDED
// (oneAPI dirs in front, joined by the OS list separator, with the daemon's
// original PATH tail retained), and every other base / overlay var is
// untouched.
func TestOneAPIInjectPrependsDLLDirsOntoPATH(t *testing.T) {
	sep := string(os.PathListSeparator)
	baselinePATH := "C:" + sep + "base"
	// Daemon's composed env BEFORE oneAPI (parent<manifest<overlay already
	// merged). Includes an operator-overlaid PATH and an unrelated overlay key.
	cmdEnv := []string{
		"PATH=" + baselinePATH,
		"OVERLAY_KEY=op",
		"BASE_KEY=keepme",
	}
	dirs := []string{`C:\oneapi\mkl\bin`, `C:\oneapi\tbb\bin`}

	got, applied := injectOneAPIEnv(cmdEnv, dirs)

	// PATH must be oneAPI dirs (joined) + sep + the daemon's original PATH.
	wantPATH := dirs[0] + sep + dirs[1] + sep + baselinePATH
	if v := pathFamilyValue(t, got); v != wantPATH {
		t.Fatalf("injected PATH = %q, want %q (oneAPI dirs prepended onto base tail)", v, wantPATH)
	}
	// Other vars must survive untouched.
	if v, ok := envValue(got, "OVERLAY_KEY"); !ok || v != "op" {
		t.Fatalf("OVERLAY_KEY lost: (%q, %v)", v, ok)
	}
	if v, ok := envValue(got, "BASE_KEY"); !ok || v != "keepme" {
		t.Fatalf("BASE_KEY lost: (%q, %v)", v, ok)
	}
	// No spurious extra var (direct enumeration sets ONLY PATH).
	if len(got) != len(cmdEnv) {
		t.Fatalf("env length changed: got %d entries, want %d (only PATH rewritten): %v", len(got), len(cmdEnv), got)
	}
	// applied list must be exactly the dirs we prepended, in order.
	if len(applied) != 2 || applied[0] != dirs[0] || applied[1] != dirs[1] {
		t.Fatalf("applied = %v, want %v", applied, dirs)
	}
}

// TestOneAPIInjectIdempotentOnRespawn asserts a dir already present in the
// current PATH is NOT prepended again (idempotent across respawn / an
// overlay-supplied PATH that already carries the oneAPI dirs).
func TestOneAPIInjectIdempotentOnRespawn(t *testing.T) {
	sep := string(os.PathListSeparator)
	mkl := `C:\oneapi\mkl\bin`
	tbb := `C:\oneapi\tbb\bin`
	// Current PATH already has mkl at the head (e.g. a respawn after a
	// prior inject) but NOT tbb.
	cmdEnv := []string{"PATH=" + mkl + sep + "C:" + sep + "base"}

	got, applied := injectOneAPIEnv(cmdEnv, []string{mkl, tbb})

	// Only tbb should be prepended; mkl must not be duplicated.
	wantPATH := tbb + sep + mkl + sep + "C:" + sep + "base"
	if v := pathFamilyValue(t, got); v != wantPATH {
		t.Fatalf("PATH = %q, want %q (only missing dir prepended, no dup)", v, wantPATH)
	}
	if len(applied) != 1 || applied[0] != tbb {
		t.Fatalf("applied = %v, want [%q] (mkl already present)", applied, tbb)
	}
}

// TestOneAPIInjectEmptyDirsIsNoOp asserts an empty dir list returns the env
// unchanged with no applied dirs.
func TestOneAPIInjectEmptyDirsIsNoOp(t *testing.T) {
	cmdEnv := []string{"PATH=C:/base", "X=1"}
	got, applied := injectOneAPIEnv(cmdEnv, nil)
	if len(applied) != 0 {
		t.Fatalf("applied = %v, want empty for nil dirs", applied)
	}
	if len(got) != len(cmdEnv) {
		t.Fatalf("env changed for nil dirs: %v", got)
	}
}

// TestOneAPIInjectNoPATHCreatesOne asserts that when cmdEnv has no PATH
// entry (rare) one is created from the oneAPI dirs.
func TestOneAPIInjectNoPATHCreatesOne(t *testing.T) {
	sep := string(os.PathListSeparator)
	cmdEnv := []string{"X=1"}
	dirs := []string{`C:\oneapi\mkl\bin`}
	got, applied := injectOneAPIEnv(cmdEnv, dirs)
	if v := pathFamilyValue(t, got); v != dirs[0] {
		t.Fatalf("PATH = %q, want %q (created from oneAPI dirs)", v, dirs[0])
	}
	_ = sep
	if len(applied) != 1 || applied[0] != dirs[0] {
		t.Fatalf("applied = %v, want %v", applied, dirs)
	}
}

// TestOneAPIInjectorApplies pins the target-set + enabled gating: only
// enabled injectors with a non-empty dir list and a target server apply.
func TestOneAPIInjectorApplies(t *testing.T) {
	dirs := []string{`C:\oneapi\mkl\bin`}
	cases := []struct {
		name string
		inj  *oneAPIInjector
		srv  string
		want bool
	}{
		{"nil-injector", nil, "gdb", false},
		{"disabled", &oneAPIInjector{Enabled: false, Dirs: dirs, Targets: defaultOneAPITargetServers}, "gdb", false},
		{"enabled-gdb", &oneAPIInjector{Enabled: true, Dirs: dirs, Targets: defaultOneAPITargetServers}, "gdb", true},
		{"enabled-lldb", &oneAPIInjector{Enabled: true, Dirs: dirs, Targets: defaultOneAPITargetServers}, "lldb", true},
		{"enabled-nontarget", &oneAPIInjector{Enabled: true, Dirs: dirs, Targets: defaultOneAPITargetServers}, "memory", false},
		{"enabled-empty-dirs", &oneAPIInjector{Enabled: true, Dirs: nil, Targets: defaultOneAPITargetServers}, "gdb", false},
	}
	for _, c := range cases {
		if got := c.inj.applies(c.srv); got != c.want {
			t.Fatalf("%s: applies(%q) = %v, want %v", c.name, c.srv, got, c.want)
		}
	}
}

// TestOneAPIBuildInjectorDisabledByEnv asserts MCPHUB_DISABLE_ONEAPI_PATH=1
// produces a disabled injector even when a root would be detectable.
func TestOneAPIBuildInjectorDisabledByEnv(t *testing.T) {
	t.Setenv(oneapi.DisableEnvVar, "1")
	restore := oneapi.SetSeamsForTest(
		func(string) bool { return true }, // root "exists"
		func(string) bool {
			t.Fatalf("DLLDirs dll-check must not run when disabled")
			return false
		},
		func(string) []string {
			t.Fatalf("DLLDirs component-list must not run when disabled")
			return nil
		},
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

// TestOneAPIBuildInjectorNotDetectedIsNoOp asserts a host without a oneAPI
// root yields a disabled injector (zero behavior change on non-oneAPI hosts).
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

// TestOneAPIBuildInjectorNoDLLDirsEmitsWarn asserts that a detected root
// with NO component DLL dirs yields a disabled injector AND emits the warn
// event so the operator sees the degradation.
func TestOneAPIBuildInjectorNoDLLDirsEmitsWarn(t *testing.T) {
	t.Setenv(oneapi.DisableEnvVar, "")
	// On platforms where rootProbePaths is empty (POSIX), DetectRoot returns
	// ("",false) regardless of the dirExists seam, so this test only
	// meaningfully exercises the no-dll-dirs branch on Windows.
	if runtime.GOOS != "windows" {
		t.Skip("DetectRoot has no probe candidates on non-Windows; no-dll-dirs branch unreachable")
	}
	t.Setenv("ONEAPI_ROOT", `C:\fake-oneapi`)
	restore := oneapi.SetSeamsForTest(
		func(p string) bool {
			// Root "exists"; component bin dirs do NOT (so DLLDirs is empty).
			return p == `C:\fake-oneapi`
		},
		func(string) bool { return false }, // no *.dll anywhere
		func(string) []string { return nil },
	)
	defer restore()

	events := newCapturingEventLog(t)
	inj := buildOneAPIInjector(events.log)
	if inj.Enabled {
		t.Fatalf("no-dll-dirs: injector Enabled=true, want false (no inject)")
	}
	if !events.sawEvent("oneapi-no-dll-dirs") {
		t.Fatalf("no-dll-dirs did not emit oneapi-no-dll-dirs warn event")
	}
}

// TestOneAPIBuildInjectorEnumeratesDirs asserts a detected root with a
// dll-bearing component yields an ENABLED injector carrying that dir.
func TestOneAPIBuildInjectorEnumeratesDirs(t *testing.T) {
	t.Setenv(oneapi.DisableEnvVar, "")
	if runtime.GOOS != "windows" {
		t.Skip("DetectRoot has no probe candidates on non-Windows; enable branch unreachable")
	}
	root := `C:\fake-oneapi`
	mklBin := filepath.Join(root, "mkl", "latest", "bin")
	t.Setenv("ONEAPI_ROOT", root)
	restore := oneapi.SetSeamsForTest(
		func(p string) bool { return p == root || p == mklBin },
		func(p string) bool { return p == mklBin }, // mkl bin has a dll
		func(string) []string { return []string{"mkl"} },
	)
	defer restore()

	inj := buildOneAPIInjector(nil)
	if !inj.Enabled {
		t.Fatalf("dll-bearing root: injector Enabled=false, want true")
	}
	if len(inj.Dirs) != 1 || inj.Dirs[0] != mklBin {
		t.Fatalf("inj.Dirs = %v, want [%q]", inj.Dirs, mklBin)
	}
	if !inj.applies("gdb") {
		t.Fatalf("enabled dll-bearing injector does not apply to gdb")
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

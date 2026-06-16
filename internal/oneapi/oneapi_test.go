package oneapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
)

// TestDetectSetvarsAgainstFakeFS exercises DetectSetvars with an injected
// fileExists seam pointing at a temp dir, so detection is verified without
// a real oneAPI install. (setvarsProbePaths itself is platform-specific and
// driven by env vars, so we drive ONEAPI_ROOT to a temp dir and let the
// real fileExists check the temp file.)
func TestDetectSetvarsAgainstFakeFS(t *testing.T) {
	tmp := t.TempDir()
	setvars := filepath.Join(tmp, "setvars.bat")

	// Point ONEAPI_ROOT at the temp dir so the FIRST probe candidate is
	// "<tmp>/setvars.bat". On non-Windows setvarsProbePaths returns nil, so
	// inject a fake probe-list via the fileExists seam + a direct map check
	// instead.
	if os.Getenv("ONEAPI_ROOT") != "" {
		t.Setenv("ONEAPI_ROOT", tmp)
	} else {
		t.Setenv("ONEAPI_ROOT", tmp)
	}

	// Override fileExists so the test is platform-independent: it reports
	// existence for exactly the temp setvars path. We also override the
	// probe list indirectly by checking the candidate the seam is asked
	// about.
	restore := SetSeamsForTest(
		func(p string) bool { return p == setvars },
		nil,
		nil,
	)
	defer restore()

	// On Windows the probe list includes "<ONEAPI_ROOT>/setvars.bat"; on
	// POSIX setvarsProbePaths is empty, so DetectSetvars can never find it.
	// Make the test meaningful on both by asserting the platform contract:
	got, ok := DetectSetvars()
	if len(setvarsProbePaths()) == 0 {
		// POSIX: no candidates → always ("", false), regardless of fileExists.
		if ok {
			t.Fatalf("POSIX DetectSetvars returned ok=true with no probe candidates; got %q", got)
		}
		return
	}
	// Windows: the first candidate is "<ONEAPI_ROOT>/setvars.bat" which our
	// fake fileExists reports as present.
	if !ok {
		t.Fatalf("DetectSetvars: want found, got ok=false")
	}
	if filepath.Clean(got) != filepath.Clean(setvars) {
		t.Fatalf("DetectSetvars path = %q, want %q", got, setvars)
	}
}

// TestDetectSetvarsMissing asserts the ("",false) contract when no
// candidate exists.
func TestDetectSetvarsMissing(t *testing.T) {
	t.Setenv("ONEAPI_ROOT", filepath.Join(t.TempDir(), "nope"))
	restore := SetSeamsForTest(
		func(string) bool { return false }, // nothing exists
		nil,
		nil,
	)
	defer restore()

	got, ok := DetectSetvars()
	if ok || got != "" {
		t.Fatalf("DetectSetvars with nothing on disk = (%q, %v), want (\"\", false)", got, ok)
	}
}

// TestCaptureEnvDeltaComputesDelta injects a fake runner whose `set` output
// adds an oneAPI-prepended PATH and a new MKLROOT. The delta must contain
// PATH (changed) and MKLROOT (new), and must NOT contain unchanged vars.
func TestCaptureEnvDeltaComputesDelta(t *testing.T) {
	baseline := []string{
		"PATH=C:\\base",
		"WINDIR=C:\\Windows",
		"UNCHANGED=same",
	}
	// setvars output: PATH prepended with oneAPI dirs, MKLROOT new,
	// UNCHANGED identical, WINDIR identical.
	setOut := "PATH=C:\\oneapi\\mkl\\bin;C:\\base\r\n" +
		"MKLROOT=C:\\oneapi\\mkl\\latest\r\n" +
		"UNCHANGED=same\r\n" +
		"WINDIR=C:\\Windows\r\n"

	restore := SetSeamsForTest(
		func(string) bool { return true },
		func(_ context.Context, _ string) (string, error) { return setOut, nil },
		func() []string { return baseline },
	)
	defer restore()

	delta, err := CaptureEnvDelta("C:\\fake\\setvars.bat")
	if err != nil {
		t.Fatalf("CaptureEnvDelta unexpected error: %v", err)
	}

	wantPATH := "C:\\oneapi\\mkl\\bin;C:\\base"
	if got, ok := delta["PATH"]; !ok || got != wantPATH {
		t.Fatalf("delta[PATH] = (%q, present=%v), want %q present", got, ok, wantPATH)
	}
	if got, ok := delta["MKLROOT"]; !ok || got != "C:\\oneapi\\mkl\\latest" {
		t.Fatalf("delta[MKLROOT] = (%q, present=%v), want new MKLROOT", got, ok)
	}
	if _, ok := delta["UNCHANGED"]; ok {
		t.Fatalf("delta should NOT contain UNCHANGED (identical value), got %v", delta)
	}
	if _, ok := delta["WINDIR"]; ok {
		t.Fatalf("delta should NOT contain WINDIR (identical value), got %v", delta)
	}

	gotKeys := SortedKeys(delta)
	wantKeys := []string{"MKLROOT", "PATH"}
	sort.Strings(wantKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("SortedKeys = %v, want %v", gotKeys, wantKeys)
	}
}

// TestCaptureEnvDeltaRunnerError asserts a runner error → empty delta + a
// non-nil error (clean no-op).
func TestCaptureEnvDeltaRunnerError(t *testing.T) {
	restore := SetSeamsForTest(
		func(string) bool { return true },
		func(_ context.Context, _ string) (string, error) {
			return "", errors.New("boom")
		},
		func() []string { return []string{"PATH=C:\\base"} },
	)
	defer restore()

	delta, err := CaptureEnvDelta("C:\\fake\\setvars.bat")
	if err == nil {
		t.Fatalf("CaptureEnvDelta with failing runner: want error, got nil")
	}
	if len(delta) != 0 {
		t.Fatalf("CaptureEnvDelta with failing runner: want empty delta, got %v", delta)
	}
}

// TestCaptureEnvDeltaCachedAtMostOnce asserts setvars runs at most once
// across repeated CaptureEnvDelta calls (the cache contract — setvars is
// slow, run it once per process).
func TestCaptureEnvDeltaCachedAtMostOnce(t *testing.T) {
	var calls int32
	restore := SetSeamsForTest(
		func(string) bool { return true },
		func(_ context.Context, _ string) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "MKLROOT=C:\\oneapi\\mkl\r\n", nil
		},
		func() []string { return []string{"PATH=C:\\base"} },
	)
	defer restore()

	for i := 0; i < 5; i++ {
		if _, err := CaptureEnvDelta("C:\\fake\\setvars.bat"); err != nil {
			t.Fatalf("call %d unexpected error: %v", i, err)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("setvars runner invoked %d times, want exactly 1 (cached)", n)
	}
}

// TestCaptureEnvDeltaEmptyOutputIsError asserts that empty/unparseable
// setvars output yields an error + empty delta (the no-op-with-warn path).
func TestCaptureEnvDeltaEmptyOutputIsError(t *testing.T) {
	restore := SetSeamsForTest(
		func(string) bool { return true },
		func(_ context.Context, _ string) (string, error) { return "   \r\n\r\n", nil },
		func() []string { return []string{"PATH=C:\\base"} },
	)
	defer restore()

	delta, err := CaptureEnvDelta("C:\\fake\\setvars.bat")
	if err == nil {
		t.Fatalf("empty setvars output: want error, got nil")
	}
	if len(delta) != 0 {
		t.Fatalf("empty setvars output: want empty delta, got %v", delta)
	}
}

// TestCaptureEnvDeltaNoChangeIsCleanNoOp asserts that setvars producing
// only already-present identical vars yields an empty delta with NO error
// (benign no-op — e.g. supervisor already inside an oneAPI shell).
func TestCaptureEnvDeltaNoChangeIsCleanNoOp(t *testing.T) {
	restore := SetSeamsForTest(
		func(string) bool { return true },
		func(_ context.Context, _ string) (string, error) {
			return "PATH=C:\\base\r\nWINDIR=C:\\Windows\r\n", nil
		},
		func() []string { return []string{"PATH=C:\\base", "WINDIR=C:\\Windows"} },
	)
	defer restore()

	delta, err := CaptureEnvDelta("C:\\fake\\setvars.bat")
	if err != nil {
		t.Fatalf("no-change setvars: want nil error, got %v", err)
	}
	if len(delta) != 0 {
		t.Fatalf("no-change setvars: want empty delta, got %v", delta)
	}
}

// TestParseSetOutputSkipsMalformed asserts banner/garbage lines (no '=',
// empty-key) are skipped and KEY=VALUE with embedded '=' in the value is
// preserved.
func TestParseSetOutputSkipsMalformed(t *testing.T) {
	out := ":: Intel oneAPI banner line\r\n" +
		"\r\n" +
		"=novalue\r\n" + // empty key → skip
		"DSN=server=db;uid=x\r\n" + // value contains '=' → preserve fully
		"MKLROOT=C:\\mkl\r\n"
	m := parseSetOutput(out)
	if _, ok := m[EnvKey("DSN")]; !ok {
		t.Fatalf("DSN missing; parse dropped a valid var: %v", m)
	}
	if got := m[EnvKey("DSN")].Value; got != "server=db;uid=x" {
		t.Fatalf("DSN value = %q, want full %q (split on FIRST '=')", got, "server=db;uid=x")
	}
	if got := m[EnvKey("MKLROOT")].Value; got != "C:\\mkl" {
		t.Fatalf("MKLROOT value = %q", got)
	}
	// Banner + empty-key lines must not appear.
	if len(m) != 2 {
		t.Fatalf("parseSetOutput kept %d entries, want 2 (DSN, MKLROOT): %v", len(m), m)
	}
}

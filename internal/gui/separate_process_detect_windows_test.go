// internal/gui/separate_process_detect_windows_test.go
//go:build windows

package gui

import "testing"

// swapSepProcessSeams installs test doubles for the registry-read and
// warn-emit seams and restores the originals on cleanup.
func swapSepProcessSeams(t *testing.T, read func() bool, warn func(string, string, map[string]any) error) {
	t.Helper()
	prevRead, prevWarn := readSeparateProcessFn, sepProcessWarnFn
	readSeparateProcessFn, sepProcessWarnFn = read, warn
	t.Cleanup(func() { readSeparateProcessFn, sepProcessWarnFn = prevRead, prevWarn })
}

// TestSeparateProcess_WarnsOnceWhenSet verifies that with the registry
// value == 1, repeated detect calls emit the warning exactly once per
// Server (sync.Once gating).
func TestSeparateProcess_WarnsOnceWhenSet(t *testing.T) {
	warns := 0
	swapSepProcessSeams(t,
		func() bool { return true }, // SeparateProcess == 1
		func(level, event string, fields map[string]any) error {
			warns++
			if event != "explorer-separate-process-detected" {
				t.Errorf("unexpected event %q", event)
			}
			if level != "warn" {
				t.Errorf("expected warn level; got %q", level)
			}
			return nil
		},
	)

	s := &Server{}
	detectSeparateProcessOnce(s)
	detectSeparateProcessOnce(s)
	detectSeparateProcessOnce(s)

	if warns != 1 {
		t.Fatalf("SeparateProcess=1 must warn exactly once per process; got %d", warns)
	}
}

// TestSeparateProcess_SilentWhenUnset verifies that with the value 0 /
// missing / read-error (read seam returns false), zero warnings fire.
func TestSeparateProcess_SilentWhenUnset(t *testing.T) {
	warns := 0
	swapSepProcessSeams(t,
		func() bool { return false }, // value 0 / missing / error → fail-soft false
		func(level, event string, fields map[string]any) error { warns++; return nil },
	)

	s := &Server{}
	detectSeparateProcessOnce(s)
	detectSeparateProcessOnce(s)

	if warns != 0 {
		t.Fatalf("SeparateProcess unset must emit zero warnings; got %d", warns)
	}
}

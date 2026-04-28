package gui

import (
	"errors"
	"runtime"
	"testing"
)

func TestOpenPath_UsesPlatformCommand(t *testing.T) {
	// Capture spawn invocations.
	type invocation struct {
		name string
		args []string
	}
	var captured invocation
	orig := spawnProcess
	spawnProcess = func(name string, args ...string) error {
		captured = invocation{name, args}
		return nil
	}
	defer func() { spawnProcess = orig }()

	err := OpenPath("/some/dir")
	if err != nil {
		t.Fatalf("OpenPath returned: %v", err)
	}

	var wantName string
	switch runtime.GOOS {
	case "windows":
		wantName = "explorer.exe"
	case "darwin":
		wantName = "open"
	default:
		wantName = "xdg-open"
	}
	if captured.name != wantName {
		t.Errorf("expected %q, got %q", wantName, captured.name)
	}
	if len(captured.args) != 1 || captured.args[0] != "/some/dir" {
		t.Errorf("expected args=[/some/dir], got %v", captured.args)
	}
}

func TestOpenPath_PropagatesError(t *testing.T) {
	orig := spawnProcess
	spawnProcess = func(name string, args ...string) error {
		return errors.New("boom")
	}
	defer func() { spawnProcess = orig }()
	err := OpenPath("/x")
	if err == nil || err.Error() != "boom" {
		t.Errorf("expected error 'boom', got %v", err)
	}
}

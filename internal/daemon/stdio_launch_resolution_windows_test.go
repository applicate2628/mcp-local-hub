//go:build windows

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func preparedNativeCopy(t *testing.T, target, suffix string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	w, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		_ = r.Close()
		t.Fatal(err)
	}
	_, primary := io.Copy(w, r)
	if primary == nil && suffix != "" {
		_, primary = w.Write([]byte(suffix))
	}
	if err := errors.Join(primary, r.Close(), w.Close()); err != nil {
		t.Fatal(err)
	}
}

func preparedTestDirectory(t *testing.T, directory string) {
	t.Helper()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Error(err)
		}
	})
}

func TestPreparedStdioCapturedResolutionABA(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, directory := range []string{a, b, filepath.Join(a, "child")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	aImage, bImage := filepath.Join(a, "captured-tool.exe"), filepath.Join(b, "captured-tool.com")
	preparedNativeCopy(t, aImage, "capture-a")
	preparedNativeCopy(t, bImage, "capture-b")
	preparedTestDirectory(t, a)
	t.Setenv("PATH", a)
	t.Setenv("PATHEXT", ".EXE")
	t.Setenv("NoDefaultCurrentDirectoryInExePath", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := CaptureStdioLaunchContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The actual own test process changes to B only during construction, then
	// restores A. Production code never modifies global lookup inputs.
	if err := os.Setenv("PATH", b); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("PATHEXT", ".COM"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(b); err != nil {
		t.Fatal(err)
	}
	h, prepareErr := NewPreparedStdioHost(ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: "captured-tool", WorkingDir: "child", Env: map[string]string{"PATH": b}}})
	if err := os.Setenv("PATH", a); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("PATHEXT", ".EXE"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(a); err != nil {
		t.Fatal(err)
	}
	if prepareErr != nil {
		t.Fatal(prepareErr)
	}
	p, err := h.PreparedLaunch()
	if err != nil {
		t.Fatal(err)
	}
	if p.command.Path != aImage || p.image.path != aImage || p.command.Args[0] != aImage || p.command.Dir != filepath.Join(a, "child") {
		t.Fatal("transient live parent or child PATH/Dir replaced captured calculation")
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.command.Process != nil || h.original != nil {
		t.Fatal("ABA calculation acquired an actor")
	}
}

func TestPreparedStdioRelativeImageUsesCapturedParent(t *testing.T) {
	root := t.TempDir()
	parent, other := filepath.Join(root, "parent"), filepath.Join(root, "other")
	for _, directory := range []string{parent, other, filepath.Join(parent, "child")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	image := filepath.Join(parent, "relative.exe")
	preparedNativeCopy(t, image, "parent")
	preparedNativeCopy(t, filepath.Join(other, "relative.exe"), "other")
	preparedTestDirectory(t, parent)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := CaptureStdioLaunchContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	h, prepareErr := NewPreparedStdioHost(ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: `.\relative.exe`, WorkingDir: "child"}})
	if err := os.Chdir(parent); err != nil {
		t.Fatal(err)
	}
	if prepareErr != nil {
		t.Fatal(prepareErr)
	}
	p, err := h.PreparedLaunch()
	if err != nil {
		t.Fatal(err)
	}
	if p.command.Path != image || p.image.path != image || p.command.Dir != filepath.Join(parent, "child") {
		t.Fatal("relative image was anchored to live or child cwd")
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedStdioWindowsSelectionRefusals(t *testing.T) {
	root := t.TempDir()
	preparedNativeCopy(t, filepath.Join(root, "extensionless.exe"), "")
	preparedNativeCopy(t, filepath.Join(root, "dot-only.exe"), "")
	preparedTestDirectory(t, root)
	t.Setenv("PATH", root)
	t.Setenv("PATHEXT", ".EXE")
	t.Setenv("GODEBUG", "execerrdot=1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := CaptureStdioLaunchContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPreparedStdioHost(ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: `.\extensionless`}}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path-containing extensionless token = %v", err)
	}
	t.Setenv("NoDefaultCurrentDirectoryInExePath", "")
	t.Setenv("PATH", ".")
	snapshot, err = CaptureStdioLaunchContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPreparedStdioHost(ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: "dot-only"}}); !errors.Is(err, exec.ErrDot) {
		t.Fatalf("relative PATH ErrDot = %v", err)
	}
	if _, err := NewPreparedStdioHost(ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: "C:drive-relative.exe"}}); err == nil {
		t.Fatal("unbound drive-relative command was accepted")
	}
}

func TestPreparedStdioRejectsDotTokensBeforeSiblingSelection(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "A")
	child := filepath.Join(directory, "child")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	// Without the exact literal guard, both tokens can select this real native
	// sibling after Join cleans the name and PATHEXT appends .exe.
	preparedNativeCopy(t, directory+".exe", "")
	t.Setenv("PATHEXT", ".EXE")
	t.Setenv("NoDefaultCurrentDirectoryInExePath", "")
	for _, input := range []struct{ name, path string }{{".", directory}, {"..", child}} {
		t.Run(input.name, func(t *testing.T) {
			t.Setenv("PATH", input.path)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			snapshot, err := CaptureStdioLaunchContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			h, err := NewPreparedStdioHost(ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: input.name}})
			if !errors.Is(err, exec.ErrNotFound) {
				t.Fatalf("invalid literal lookup = %v", err)
			}
			var failure *StdioLaunchPreparationError
			if !errors.As(err, &failure) || failure.Unwrap() == nil || failure.PartialPreparation() == nil {
				t.Fatal("literal refusal lost its original cause/partial")
			}
			if h.original != nil || h.prepared.command.Process != nil || h.prepared.image.closeAttempted {
				t.Fatal("invalid literal reached image/creator acquisition")
			}
		})
	}
}

// Future execution requires Root's separate actor grant. Every successful
// start goes through the SAME existing original native creator and helper.
func TestPreparedStdioOriginalNativePositiveShapes(t *testing.T) {
	for _, shape := range []string{"bare-parent-path", "relative-parent-child-cwd", "absolute-image", "exact-extensionless-image"} {
		t.Run(shape, func(t *testing.T) {
			root := t.TempDir()
			parent, child := filepath.Join(root, "parent"), filepath.Join(root, "child")
			for _, directory := range []string{parent, child} {
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			name := "native-positive.exe"
			if shape == "exact-extensionless-image" {
				name = "native-positive"
			}
			image := filepath.Join(parent, name)
			preparedNativeCopy(t, image, "")
			preparedTestDirectory(t, parent)
			t.Setenv("PATH", parent)
			t.Setenv("PATHEXT", ".EXE")
			t.Setenv("NoDefaultCurrentDirectoryInExePath", "")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			snapshot, err := CaptureStdioLaunchContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			command := image
			if shape == "bare-parent-path" {
				command = "native-positive"
			}
			if shape == "relative-parent-child-cwd" {
				command = `.\native-positive.exe`
			}
			h, err := NewPreparedStdioHost(ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: command,
				WorkingDir: child, Args: []string{"-test.run=^TestStdioOriginalHelper$"},
				Env: map[string]string{stdioOriginalHelperMode: "final", stdioOriginalHelperRoot: root, "PATH": child}}})
			if err != nil {
				t.Fatal(err)
			}
			p, err := h.PreparedLaunch()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
				defer done()
				result, err := h.StopObserved(cleanup)
				if err != nil || !result.Complete() {
					t.Errorf("original creator settlement incomplete: %v", err)
				}
			})
			if err := h.StartPrepared(); err != nil {
				t.Fatal(err)
			}
			if h.cmd != p.command || h.original == nil {
				t.Fatal("Start replaced command or original creator")
			}
			originalTestBarrier(t, filepath.Join(root, "root-ready"))
			response := make(chan json.RawMessage, 1)
			h.pendingMu.Lock()
			h.pending[1] = response
			h.pendingMu.Unlock()
			if err := h.writeStdin([]byte(`{"jsonrpc":"2.0","id":1,"method":"test"}`)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-h.ChildExited():
			case <-ctx.Done():
				t.Fatal("prepared original child completion unavailable")
			}
			select {
			case reply := <-response:
				if !json.Valid(reply) {
					t.Fatal("original capture reply invalid")
				}
			default:
				t.Fatal("original reply capture missing")
			}
			cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
			result, err := h.StopObserved(cleanup)
			done()
			if err != nil || !result.Complete() || result.NoNativeAttempt || result.NativeSettlement == nil {
				t.Fatalf("native original completion = %v", err)
			}
		})
	}
}

//go:build windows && amd64 && cgo && ffi_loader_faults

package caller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// These are locally built benign loader fixtures, not replacement process
// owners. No fixture export except version/probe is called by these tests.
func loaderRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("MCPHUB_FFI_LOADER_FIXTURES")
	if !filepath.IsAbs(root) {
		t.Fatal("explicit owned loader fixture directory required")
	}
	if st, e := os.Stat(root); e != nil || !st.IsDir() {
		t.Fatalf("fixture directory unavailable: %v", e)
	}
	return root
}

// Error-dialog suppression applies only to this locked test thread and is
// restored before it is released. It does not change global loader policy.
func quietLoaderThread(t *testing.T) func() {
	t.Helper()
	runtime.LockOSThread()
	set := testKernel.NewProc("SetThreadErrorMode")
	var old uint32
	ok, _, err := set.Call(0x8003, uintptr(unsafe.Pointer(&old)))
	if ok == 0 {
		runtime.UnlockOSThread()
		t.Fatalf("thread error mode: %v", err)
	}
	return func() {
		ok, _, err := set.Call(uintptr(old), 0)
		runtime.UnlockOSThread()
		if ok == 0 {
			t.Errorf("restore thread error mode: %v", err)
		}
	}
}

// UNCHANGED_REFCOUNT makes this observation non-owning. Never FreeLibrary the
// returned value. Fixture loads/unloads are serialized in these tests; arbitrary
// concurrent third-party unloading is outside this observation's contract.
func loaderMapped(t *testing.T, path string) bool {
	t.Helper()
	name, e := syscall.UTF16FromString(path)
	if e != nil {
		t.Fatal(e)
	}
	var module uintptr
	ok, _, err := testKernel.NewProc("GetModuleHandleExW").Call(2, uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&module)))
	runtime.KeepAlive(name)
	if ok != 0 {
		if module == 0 {
			t.Fatal("module observation returned success with no value")
		}
		return true
	}
	if !errors.Is(err, syscall.Errno(126)) {
		t.Fatalf("module observation failed unexpectedly: %v", err)
	}
	return false
}
func assertNotMapped(t *testing.T, path string) {
	t.Helper()
	if loaderMapped(t, path) {
		t.Fatalf("unexpected live module reference after refusal/close: %s", filepath.Base(path))
	}
}
func refusedDLL(t *testing.T, path string, want syscall.Errno) {
	t.Helper()
	lib, e := Open(path)
	if lib != nil {
		if ce := lib.Close(); ce != nil {
			t.Error(ce)
		}
		t.Fatalf("incompatible fixture accepted: %s", filepath.Base(path))
	}
	if !errors.Is(e, want) {
		t.Fatalf("native loader error lost: got %v want %d", e, uint32(want))
	}
}

func TestLoaderRejectsEveryMissingExport(t *testing.T) {
	root := loaderRoot(t)
	symbols := []string{"version", "create", "admit", "stop", "finish", "release", "checked", "environment", "stdio"}
	for index, name := range symbols {
		t.Run(name, func(t *testing.T) {
			defer quietLoaderThread(t)()
			path := filepath.Join(root, fmt.Sprintf("missing-%d.dll", index))
			assertNotMapped(t, path)
			for i := 0; i < 3; i++ {
				refusedDLL(t, path, 127)
				assertNotMapped(t, path)
			}
		})
	}
}
func TestLoaderRejectsVersionAndDoesNotRetainModule(t *testing.T) {
	root := loaderRoot(t)
	for _, name := range []string{"version-0.dll", "version-2.dll", "version-max.dll"} {
		t.Run(name, func(t *testing.T) {
			defer quietLoaderThread(t)()
			path := filepath.Join(root, name)
			assertNotMapped(t, path)
			for i := 0; i < 3; i++ {
				refusedDLL(t, path, 1306)
				assertNotMapped(t, path)
			}
		})
	}
}
func TestLoaderPreservesNativeImageAndInitializationErrors(t *testing.T) {
	root := loaderRoot(t)
	for _, tc := range []struct {
		name string
		code syscall.Errno
	}{
		{"absent.dll", 126}, {"not-a-library.dll", 193}, {"init-refused.dll", 1114},
		{filepath.Join("missing-dependency", "needs-dependency.dll"), 126},
	} {
		t.Run(tc.name, func(t *testing.T) { defer quietLoaderThread(t)(); refusedDLL(t, filepath.Join(root, tc.name), tc.code) })
	}
}
func TestLoaderRefusalDoesNotReleaseAnotherOwnersReference(t *testing.T) {
	root := loaderRoot(t)
	for _, name := range []string{"missing-8.dll", "version-2.dll"} {
		t.Run(name, func(t *testing.T) {
			defer quietLoaderThread(t)()
			path := filepath.Join(root, name)
			assertNotMapped(t, path)
			independent, e := syscall.LoadDLL(path)
			if e != nil {
				t.Fatal(e)
			}
			released := false
			defer func() {
				if !released {
					if !loaderMapped(t, path) {
						t.Error("independent owner reference disappeared; refusing stale free")
						return
					}
					if e := independent.Release(); e != nil {
						t.Error(e)
					}
				}
			}()
			lib, e := Open(path)
			if lib != nil {
				_ = lib.Close()
				t.Fatal("incompatible fixture accepted")
			}
			expected := syscall.Errno(127)
			if name == "version-2.dll" {
				expected = 1306
			}
			if !errors.Is(e, expected) {
				t.Fatalf("refusal error: %v", e)
			}
			if !loaderMapped(t, path) {
				released = true
				t.Fatal("failed Open unloaded an independently owned module")
			}
			if e = independent.Release(); e != nil {
				t.Fatal(e)
			}
			released = true
			assertNotMapped(t, path) // proves failed Open did not retain an extra ref
		})
	}
}
func TestLoaderSeparateLibrariesKeepIndependentReferences(t *testing.T) {
	root := loaderRoot(t)
	defer quietLoaderThread(t)()
	path := filepath.Join(root, "valid.dll")
	assertNotMapped(t, path)
	first, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer first.Close()
	second, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close()
	if e = first.Close(); e != nil {
		t.Fatal(e)
	}
	if e = first.Close(); e != nil {
		t.Fatal(e)
	}
	if !loaderMapped(t, path) {
		t.Fatal("closing one library unloaded another live reference")
	}
	if e = second.Close(); e != nil {
		t.Fatal(e)
	}
	assertNotMapped(t, path)
}
func TestLoaderUnicodePathAndDllDirectoryDependency(t *testing.T) {
	root := loaderRoot(t)
	for _, name := range []string{filepath.Join("unicode-λ-🦀", "valid.dll"), filepath.Join("with-dependency", "needs-dependency.dll")} {
		t.Run(name, func(t *testing.T) {
			defer quietLoaderThread(t)()
			path := filepath.Join(root, name)
			assertNotMapped(t, path)
			dependency := filepath.Join(filepath.Dir(path), "mcphub_owned_loader_dependency.dll")
			withDependency := filepath.Base(path) == "needs-dependency.dll"
			if withDependency {
				assertNotMapped(t, dependency)
			}
			lib, e := Open(path)
			if e != nil {
				t.Fatal(e)
			}
			if !loaderMapped(t, path) {
				_ = lib.Close()
				t.Fatal("successful fixture was not mapped")
			}
			if withDependency && !loaderMapped(t, dependency) {
				_ = lib.Close()
				t.Fatal("expected local dependency was not mapped")
			}
			if e = lib.Close(); e != nil {
				t.Fatal(e)
			}
			assertNotMapped(t, path)
			if withDependency {
				assertNotMapped(t, dependency)
			}
		})
	}
}
func TestLoaderSearchDoesNotFallBackToWorkingDirectoryOrPath(t *testing.T) {
	root := loaderRoot(t)
	if os.Getenv("MCPHUB_FFI_LOADER_CHILD") == "1" {
		defer quietLoaderThread(t)()
		refusedDLL(t, filepath.Join(root, "missing-dependency", "needs-dependency.dll"), 126)
		fmt.Fprintln(os.Stdout, "DEPENDENCY_SEARCH_REFUSED")
		return
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestLoaderSearchDoesNotFallBackToWorkingDirectoryOrPath$", "-test.count=1", "-test.timeout=8s")
	// Only this finite owned child gets the fixture working directory/PATH.
	// Parent process globals, system configuration and user DLLs are untouched.
	cmd.Dir = filepath.Join(root, "dependency-only")
	for _, entry := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(entry), "PATH=") || strings.HasPrefix(strings.ToUpper(entry), "MCPHUB_FFI_LOADER_CHILD=") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "PATH="+cmd.Dir+string(os.PathListSeparator)+os.Getenv("PATH"), "MCPHUB_FFI_LOADER_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if e = cmd.Run(); e != nil {
		t.Fatalf("isolated search check failed: %v; %s", e, output.String())
	}
	if ctx.Err() != nil || !strings.Contains(output.String(), "DEPENDENCY_SEARCH_REFUSED") {
		t.Fatalf("missing completed observation: %v %s", ctx.Err(), output.String())
	}
}

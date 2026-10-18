package cbuild

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProjectOperationWaitIsCancellableAndIsolated(t *testing.T) {
	b := &builder{}
	source, other := t.TempDir(), t.TempDir()
	unlock, err := b.lockProjectOperation(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	otherUnlock, err := b.lockProjectOperation(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	otherUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if release, err := b.lockProjectOperation(ctx, filepath.Join(source, ".")); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("same-project wait = %v", err)
	}
	b.operationsMu.Lock()
	defer b.operationsMu.Unlock()
	if len(b.operations) != 1 {
		t.Fatalf("canceled waiter/other project leaked entries: %d", len(b.operations))
	}
}

func TestMutatingToolsWaitForProjectOperation(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), exeName("cmake"))
	copyExe(t, self, fake)
	copyExe(t, self, filepath.Join(filepath.Dir(fake), exeName("ctest")))
	t.Setenv("CMAKE_BIN", fake)
	t.Setenv("MCP_CBUILD_FAKE_ARGV", "1")
	copyExe(t, self, filepath.Join(filepath.Dir(fake), exeName("vcpkg")))
	t.Setenv("VCPKG_ROOT", filepath.Dir(fake))
	for _, operation := range []string{"configure", "build", "test", "clean", "purge", "workflow", "vcpkg_install"} {
		t.Run(operation, func(t *testing.T) {
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "vcpkg.json"), []byte(`{"name":"lock-test","version-string":"1"}`), 0600); err != nil {
				t.Fatal(err)
			}
			dev := writeCleanCache(t, source, "dev")
			b := &builder{configured: make(map[configuredCacheKey]string)}
			if _, err := b.settleConfiguredCache(source, "dev", "-- Build files have been written to: "+dev); err != nil {
				t.Fatal(err)
			}
			unlock, err := b.lockProjectOperation(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			tool := b.cmakeClean()
			switch operation {
			case "configure":
				tool = b.cmakeConfigure()
			case "workflow":
				tool = b.cmakeWorkflow()
			case "build":
				tool = b.cmakeBuild()
			case "test":
				tool = b.cmakeTest()
			case "vcpkg_install":
				tool = b.vcpkgInstall()
			}
			raw, err := json.Marshal(map[string]any{
				"working_dir": source, "preset": "dev", "purge_build_dir": operation == "purge",
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := tool.Call(ctx, raw); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s did not wait for project operation: %v", operation, err)
			}
			if _, err := os.Stat(filepath.Join(dev, "keep.txt")); err != nil {
				t.Fatalf("waiting operation changed build tree: %v", err)
			}
			if len(b.configured) != 1 {
				t.Fatal("waiting configure invalidated binding before acquiring operation lock")
			}
		})
	}
}

func TestConfigureInvalidatesBindingsBeforeUnsettledResult(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), exeName("cmake"))
	copyExe(t, self, fake)
	copyExe(t, self, filepath.Join(filepath.Dir(fake), exeName("ctest")))
	t.Setenv("CMAKE_BIN", fake)
	t.Setenv("MCP_CBUILD_FAKE_ARGV", "1")
	source := t.TempDir()
	dev := writeCleanCache(t, source, "dev")
	b := &builder{configured: make(map[configuredCacheKey]string)}
	if _, err := b.settleConfiguredCache(source, "dev", "-- Build files have been written to: "+dev); err != nil {
		t.Fatal(err)
	}
	// The fake exits successfully but has no CMake completion line. Neither
	// the same preset nor a potentially shared-directory alias remains valid.
	if _, err := callWithArgs(t, b.cmakeConfigure(), map[string]any{
		"working_dir": source, "preset": "release",
	}); err != nil {
		t.Fatal(err)
	}
	if len(b.configured) != 0 {
		t.Fatal("configure retained a stale binding")
	}
	if len(b.operations) != 0 {
		t.Fatal("completed configure leaked its operation lock")
	}
}

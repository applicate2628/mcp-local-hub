//go:build windows

package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// These are native traversal primitives only. A raw handle or data-only
// request does not issue namespace, publisher, or full generation authority.
func TestPreparedSerenaOwnedFileRetainedDirectoryPrimitive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	limits := DefaultSerenaTreeLimits()
	root := filepath.Join(t.TempDir(), "generation")
	if err := os.MkdirAll(filepath.Join(root, "tools"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tools", "receipt.toml")
	raw := []byte("[tool]\nname = \"retained-directory-fixture\"\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	expected, _, err := qualifyPreparedSerenaFile(ctx, path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := ntOpenStateReadRootMode(root, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.CloseHandle(handle); err != nil {
			t.Error(err)
		}
	})

	t.Run("generic-reader-retains-exclusive-ancestor-policy", func(t *testing.T) {
		if _, _, err := qualifyPreparedSerenaFile(ctx, path, 4096); !errors.Is(err, windows.STATUS_SHARING_VIOLATION) {
			t.Fatalf("generic reader error = %v, want sharing violation", err)
		}
	})
	t.Run("retained-root-read", func(t *testing.T) {
		actual, got, err := qualifyPreparedSerenaFileBeneath(ctx, root, handle, path, 4096, limits)
		if err != nil {
			t.Fatalf("read below retained DELETE directory: %T: %v", err, err)
		}
		if actual != expected || string(got) != string(raw) {
			t.Fatal("retained directory read changed identity or bytes")
		}
	})
	for _, tc := range []struct {
		name, path, want string
		cap              int64
	}{
		{"root-is-not-a-leaf", root, "SERENA_PREPARED_FILE_OUTSIDE_GENERATION", 4096},
		{"outside-root", filepath.Join(filepath.Dir(root), "outside"), "SERENA_PREPARED_FILE_OUTSIDE_GENERATION", 4096},
		{"unclean-parent", root + string(filepath.Separator) + "tools" + string(filepath.Separator) + ".." + string(filepath.Separator) + "receipt.toml", "SERENA_PREPARED_OWNED_FILE_INPUT_INVALID", 4096},
		{"named-stream", path + ":foreign", "SERENA_PREPARED_FILE_COMPONENT_INVALID", 4096},
		{"byte-cap", path, "SERENA_PREPARED_FILE_SIZE_INVALID", 1},
		{"nonpositive-cap", path, "SERENA_PREPARED_OWNED_FILE_INPUT_INVALID", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := qualifyPreparedSerenaFileBeneath(ctx, root, handle, tc.path, tc.cap, limits)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
	t.Run("depth-limit-before-open", func(t *testing.T) {
		bounded := limits
		bounded.Depth = 1
		if _, _, err := qualifyPreparedSerenaFileBeneath(ctx, root, handle, filepath.Join(root, "tools", "deeper", "missing"), 4096, bounded); err == nil || err.Error() != "SERENA_PREPARED_FILE_DEPTH_HANDLE_LIMIT" {
			t.Fatal(err)
		}
	})
	t.Run("handle-limit-before-open", func(t *testing.T) {
		bounded := limits
		bounded.OpenHandles = 4
		if _, _, err := qualifyPreparedSerenaFileBeneath(ctx, root, handle, path, 4096, bounded); err == nil || err.Error() != "SERENA_PREPARED_FILE_DEPTH_HANDLE_LIMIT" {
			t.Fatal(err)
		}
	})
	t.Run("directory-is-not-file", func(t *testing.T) {
		if _, _, err := qualifyPreparedSerenaFileBeneath(ctx, root, handle, filepath.Join(root, "tools"), 4096, limits); err == nil {
			t.Fatal("directory admitted as file")
		}
	})
	t.Run("canceled", func(t *testing.T) {
		canceled, stop := context.WithCancel(ctx)
		stop()
		if _, _, err := qualifyPreparedSerenaFileBeneath(canceled, root, handle, path, 4096, limits); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	})
	t.Run("invalid-handle", func(t *testing.T) {
		if _, _, err := qualifyPreparedSerenaFileBeneath(ctx, root, windows.InvalidHandle, path, 4096, limits); err == nil {
			t.Fatal("invalid borrowed handle admitted")
		}
	})
	t.Run("borrowed-root-remains-live-and-exclusive", func(t *testing.T) {
		if _, err := serenaTreeHandleIdentity(handle, nil); err != nil {
			t.Fatal("borrowed root was closed", err)
		}
		other, err := ntOpenStateReadRootMode(root, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
		if err == nil {
			t.Error("retained root no longer excludes delete")
			if closeErr := windows.CloseHandle(other); closeErr != nil {
				t.Error(closeErr)
			}
		} else if !errors.Is(err, windows.STATUS_SHARING_VIOLATION) {
			t.Fatal(err)
		}
	})
	t.Run("leaf-and-descendant-handles-closed", func(t *testing.T) {
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			t.Fatal("leaf still retained by reader", err)
		}
		if err := windows.CloseHandle(file); err != nil {
			t.Fatal(err)
		}
		directory, err := ntOpenStateReadRootMode(filepath.Join(root, "tools"), windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, 0)
		if err != nil {
			t.Fatal("descendant still retained by reader", err)
		}
		if err := windows.CloseHandle(directory); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPreparedSerenaOwnedFileRejectsUnissuedOwners(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	request := SerenaOwnedTreeRequest{Kind: "generation"}
	closeDebt := errors.New("actual-close-debt-fixture")
	for _, tc := range []struct {
		name string
		tree *SerenaOwnedTree
		want string
	}{
		{"nil", nil, "GENERATION_UNQUALIFIED"},
		{"attempt", &SerenaOwnedTree{request: SerenaOwnedTreeRequest{Kind: "attempt"}}, "GENERATION_UNQUALIFIED"},
		{"no-platform", &SerenaOwnedTree{request: request}, "ORIGINAL_GENERATION_REQUIRED"},
		{"published-reader", &SerenaOwnedTree{request: request, platform: &windowsSerenaOwnedTree{readExisting: true}}, "ORIGINAL_GENERATION_REQUIRED"},
		{"mismatched-platform", &SerenaOwnedTree{request: request, platform: &windowsSerenaOwnedTree{request: SerenaOwnedTreeRequest{Kind: "attempt"}}}, "GENERATION_BINDING_DRIFT"},
		{"closed", &SerenaOwnedTree{request: request, platform: &windowsSerenaOwnedTree{request: request, closeResult: SerenaTreeClose{Attempted: true, Closed: true}}}, "ROOT_UNAVAILABLE"},
		{"missing-handles", &SerenaOwnedTree{request: request, platform: &windowsSerenaOwnedTree{request: request}}, "ROOT_UNAVAILABLE"},
		{"close-debt", &SerenaOwnedTree{request: request, platform: &windowsSerenaOwnedTree{request: request, root: &serenaTreeHandle{}, parent: &serenaTreeHandle{}, inventoryCloseError: closeDebt}}, "actual-close-debt-fixture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, raw, err := qualifyPreparedSerenaOwnedFile(ctx, tc.tree, filepath.Join(t.TempDir(), "file"), 4096)
			if err == nil || !strings.Contains(err.Error(), tc.want) || file != (PreparedSerenaFile{}) || len(raw) != 0 {
				t.Fatalf("unissued owner result: file=%v raw=%d error=%v", file, len(raw), err)
			}
		})
	}
}

func TestPreparedSerenaOwnedGenerationRejectsBindings(t *testing.T) {
	request := preparedRequest()
	request.Namespace = filepath.Join(t.TempDir(), "namespace")
	request.WorkLimit = time.Minute
	request.WorkDeadline = time.Now().Add(time.Minute)
	request.AbsoluteCleanupDeadline = request.WorkDeadline.Add(request.CleanupLimit)
	key, err := request.Identity()
	if err != nil {
		t.Fatal(err)
	}
	base := SerenaOwnedTreeRequest{Namespace: request.Namespace, PackageIdentitySHA256: key, OperationID: request.OperationID, GenerationID: request.GenerationID, Kind: "generation", Limits: request.OwnedTreeLimits}
	ctx, cancel := context.WithDeadline(context.Background(), request.WorkDeadline)
	defer cancel()
	for _, tc := range []struct {
		name   string
		change func(*SerenaOwnedTreeRequest)
	}{
		{"namespace", func(r *SerenaOwnedTreeRequest) { r.Namespace = filepath.Join(r.Namespace, "foreign") }},
		{"package", func(r *SerenaOwnedTreeRequest) { r.PackageIdentitySHA256 = strings.Repeat("1", 64) }},
		{"operation", func(r *SerenaOwnedTreeRequest) { r.OperationID = "foreign" }},
		{"generation", func(r *SerenaOwnedTreeRequest) { r.GenerationID = "foreign" }},
		{"kind", func(r *SerenaOwnedTreeRequest) { r.Kind = "attempt" }},
		{"limits", func(r *SerenaOwnedTreeRequest) { r.Limits.Depth++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual := base
			tc.change(&actual)
			tree := &SerenaOwnedTree{request: actual}
			if err := validatePreparedSerenaGeneration(ctx, tree, request, PreparedSerenaFile{}); err == nil || err.Error() != "SERENA_PREPARED_GENERATION_BINDING_DRIFT" {
				t.Fatalf("foreign request admitted or reached native owner: %v", err)
			}
		})
	}
	t.Run("no-deadline", func(t *testing.T) {
		if err := validatePreparedSerenaGeneration(context.Background(), &SerenaOwnedTree{request: base}, request, PreparedSerenaFile{}); err == nil || err.Error() != "SERENA_PREPARED_GENERATION_WORK_DEADLINE_INVALID" {
			t.Fatal(err)
		}
	})
	t.Run("renewed-work-deadline", func(t *testing.T) {
		renewed, stop := context.WithDeadline(context.Background(), request.WorkDeadline.Add(time.Second))
		defer stop()
		if err := validatePreparedSerenaGeneration(renewed, &SerenaOwnedTree{request: base}, request, PreparedSerenaFile{}); err == nil || err.Error() != "SERENA_PREPARED_GENERATION_WORK_DEADLINE_INVALID" {
			t.Fatal(err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		canceled, stop := context.WithCancel(ctx)
		stop()
		if err := validatePreparedSerenaGeneration(canceled, &SerenaOwnedTree{request: base}, request, PreparedSerenaFile{}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestPreparedSerenaInvocationRejectsForeignGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, name := range []string{"foreign-operation", "closed-original"} {
		t.Run(name, func(t *testing.T) {
			request := SerenaOwnedTreeRequest{Kind: "generation", OperationID: "operation"}
			tree := &SerenaOwnedTree{request: request, platform: &windowsSerenaOwnedTree{request: request, closeResult: SerenaTreeClose{Attempted: true, Closed: true}}}
			if name == "foreign-operation" {
				tree.request.OperationID = "foreign"
			}
			invocation := preparedSerenaInvocation{executableTree: tree, OperationID: "operation", Executable: PreparedSerenaFile{Path: filepath.Join(t.TempDir(), "no-process.exe"), SHA256: strings.Repeat("a", 64), Device: "identity-only", FileID: "no-native-authority"}, Environment: []string{"PYTHONDONTWRITEBYTECODE=1"}, WorkingDir: t.TempDir(), PolicySHA256: strings.Repeat("b", 64), StdoutLimit: 4096, StderrLimit: 4096}
			result, err := runPreparedSerenaInvocation(ctx, invocation)
			if err == nil || result.Closure.nativeEvidence != nil || result.Closure.ReceiptSHA256 != "" || len(result.Stdout) != 0 || len(result.Stderr) != 0 {
				t.Fatalf("foreign owner reached contained execution: err=%v closure=%+v", err, result.Closure)
			}
		})
	}
}

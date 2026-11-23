//go:build windows

package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// These native handles exercise the stream query and decoder only. They do not
// issue Serena tree acquisition, namespace or deletion authority.
func TestSerenaOwnedTreeObserveStreamsNative(t *testing.T) {
	observer := windowsSerenaOwnedTree{request: SerenaOwnedTreeRequest{Limits: DefaultSerenaTreeLimits()}}
	open := func(t *testing.T, path string) windows.Handle {
		t.Helper()
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := windows.CreateFile(name,
			windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.DELETE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			t.Fatalf("open native stream fixture: %v", err)
		}
		t.Cleanup(func() {
			if err := windows.CloseHandle(handle); err != nil {
				t.Errorf("close native stream fixture: %v", err)
			}
		})
		return handle
	}

	for _, test := range []struct {
		name      string
		directory bool
		payload   string
		named     bool
	}{
		{name: "empty-directory", directory: true},
		{name: "empty-file"},
		{name: "nonempty-file", payload: "owned default data"},
		{name: "directory-named-stream", directory: true, named: true},
		{name: "file-named-stream", payload: "default data", named: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "object")
			if test.directory {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(test.payload), 0o600); err != nil {
				t.Fatal(err)
			}
			if test.named {
				if err := os.WriteFile(path+":owned-stream", []byte("named data"), 0o600); err != nil {
					t.Fatalf("create actual named stream: %v", err)
				}
			}
			handle := open(t, path)
			object, err := serenaTreeHandleIdentity(handle, nil)
			if err != nil || object.Directory != test.directory {
				t.Fatalf("native fixture identity: directory=%t err=%v", object.Directory, err)
			}
			streams, err := observer.observeStreams(t.Context(), handle)
			if test.named {
				if err == nil || !strings.Contains(err.Error(), "SERENA_TREE_NONDEFAULT_STREAM_REFUSED") {
					t.Fatalf("actual named stream was not refused: streams=%+v err=%v", streams, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ordinary stream query: %v; ERROR_HANDLE_EOF=%t", err, errors.Is(err, windows.ERROR_HANDLE_EOF))
			}
			if test.directory {
				if len(streams) != 0 {
					t.Fatalf("empty directory streams=%+v; want no streams", streams)
				}
				return
			}
			if object.Bytes != int64(len(test.payload)) || len(streams) != 1 ||
				streams[0].Name != "::$DATA" || streams[0].Bytes != object.Bytes {
				t.Fatalf("default stream differs from actual file size: file=%d streams=%+v", object.Bytes, streams)
			}
		})
	}

	t.Run("canceled-context", func(t *testing.T) {
		handle := open(t, t.TempDir())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		streams, err := observer.observeStreams(ctx, handle)
		if !errors.Is(err, context.Canceled) || len(streams) != 0 {
			t.Fatalf("canceled stream query: streams=%+v err=%v", streams, err)
		}
	})
	t.Run("invalid-handle", func(t *testing.T) {
		streams, err := observer.observeStreams(t.Context(), windows.InvalidHandle)
		if !errors.Is(err, windows.ERROR_INVALID_HANDLE) || len(streams) != 0 {
			t.Fatalf("invalid native handle became an empty success: streams=%+v err=%v", streams, err)
		}
	})
	t.Run("buffer-limit", func(t *testing.T) {
		handle := open(t, t.TempDir())
		bounded := observer
		bounded.request.Limits.EncodedPathBytes = 255
		streams, err := bounded.observeStreams(t.Context(), handle)
		if err == nil || !strings.Contains(err.Error(), "SERENA_TREE_STREAM_BUFFER_LIMIT") || len(streams) != 0 {
			t.Fatalf("stream buffer limit bypassed: streams=%+v err=%v", streams, err)
		}
	})
}

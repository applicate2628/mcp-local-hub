package cmaketrace

import (
	"io"
	"io/fs"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

type r21AtomicTraceFS struct {
	atomicCalls int
}

func (f *r21AtomicTraceFS) OpenRegular(string) (io.ReadCloser, fs.FileInfo, error) {
	f.atomicCalls++
	trace := "{\"version\":{\"major\":1,\"minor\":0}}\n" +
		"{\"file\":\"/p/CMakeLists.txt\",\"line\":1,\"cmd\":\"message\",\"args\":[\"ok\"]}\n"
	return io.NopCloser(strings.NewReader(trace)), regularTraceFileInfo{}, nil
}

func TestR21TraceUsesSameHandleForOpenAndRegularFileValidation(t *testing.T) {
	fsys := &r21AtomicTraceFS{}
	result := Trace(t.Context(), Args{TracePath: unusedTracePath(t)}, Deps{FS: fsys})
	if result.Status != evidence.StatusOK {
		t.Fatalf("Trace status=%s reason=%s, want ok", result.Status, result.Reason)
	}
	if fsys.atomicCalls != 1 {
		t.Fatalf("atomic calls=%d, want 1", fsys.atomicCalls)
	}
}

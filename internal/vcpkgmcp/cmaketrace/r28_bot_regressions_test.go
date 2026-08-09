package cmaketrace

import (
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

type r28TraceFS struct{ trace string }

func (f r28TraceFS) OpenRegular(string) (io.ReadCloser, fs.FileInfo, error) {
	return io.NopCloser(strings.NewReader(f.trace)), regularTraceFileInfo{}, nil
}

func TestR28DerivedTraceIndexesShareRetainedByteBudget(t *testing.T) {
	const file = "/p/a.cmake"
	const arg = "child.cmake"
	trace := "{\"version\":{\"major\":1,\"minor\":0}}\n" +
		"{\"file\":\"" + file + "\",\"line\":1,\"cmd\":\"include\",\"args\":[\"" + arg + "\"]}\n"
	recordBytes := retainedTraceRecordBytes(traceLine{File: file, Line: 1, Cmd: "include", Args: []string{arg}})
	result := Trace(t.Context(), Args{TracePath: unusedTracePath(t)}, Deps{
		FS:     r28TraceFS{trace: trace},
		Limits: Limits{MaxRetainedRecordBytes: recordBytes + 1},
	})
	if result.Status != evidence.StatusOK || len(result.Records) != 1 {
		t.Fatalf("status=%s records=%d, want positive admitted record", result.Status, len(result.Records))
	}
	if !result.InputIncomplete || !slices.Contains(result.InputIncompleteReasons, ReasonRetainedRecordLimit) {
		t.Fatalf("incomplete=%v reasons=%v, want bounded derived-index incompleteness", result.InputIncomplete, result.InputIncompleteReasons)
	}
}

package cmaketrace

import (
	"errors"
	"io"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// A Reader may return bytes and a non-EOF error together. The error happens
// once; the next read returns EOF, so accidentally continuing loses the cause.
type terminalErrorTraceReader struct {
	content  string
	err      error
	terminal bool
	closed   bool
}

func (r *terminalErrorTraceReader) Read(p []byte) (int, error) {
	if r.terminal {
		return 0, io.EOF
	}
	n := copy(p, r.content)
	r.content = r.content[n:]
	if len(r.content) == 0 {
		r.terminal = true
		return n, r.err
	}
	return n, nil
}

func (r *terminalErrorTraceReader) Close() error {
	r.closed = true
	return nil
}

func TestTraceReadErrorAtLimitIsUnknown(t *testing.T) {
	const prefix = `{"version":{"major":1,"minor":0}}` + "\n" +
		`{"file":"fixture.cmake","line":1,"cmd":"message","args":["valid"]}` + "\n"
	content := prefix + strings.Repeat("x", 256)
	for _, tc := range []struct {
		name   string
		limits Limits
	}{
		{name: "oversized_line", limits: Limits{MaxLineBytes: 128}},
		{name: "whole_input_limit", limits: Limits{MaxTraceBytes: int64(len(content) - 1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &terminalErrorTraceReader{content: content, err: errors.New("fixture read failure")}
			got := Trace(t.Context(), Args{TracePath: unusedTracePath(t)}, Deps{FS: fsReturning(reader), Limits: tc.limits})
			if !reader.terminal || !reader.closed {
				t.Fatalf("terminal=%v closed=%v, want observed failure and released reader", reader.terminal, reader.closed)
			}
			if got.Status != evidence.StatusUnknown || got.Reason != ReasonTraceUnreadable {
				t.Fatalf("status=%v reason=%v, want unknown/trace_unreadable", got.Status, got.Reason)
			}
			if len(got.Records) != 0 || len(got.ExecutedLines) != 0 || len(got.IncludeChain) != 0 {
				t.Fatalf("read failure published partial evidence: %+v", got)
			}
		})
	}
}

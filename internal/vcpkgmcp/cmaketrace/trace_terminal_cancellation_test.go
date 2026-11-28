package cmaketrace

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

type terminalCancelTraceReader struct {
	reader     *strings.Reader
	cancel     context.CancelFunc
	eofReads   int
	closeCalls int
}

func (r *terminalCancelTraceReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		r.eofReads++
		if r.cancel != nil {
			r.cancel()
		}
	}
	return n, err
}

func (r *terminalCancelTraceReader) Close() error {
	r.closeCalls++
	return nil
}

// The reader cancels inside the final Read, after readLine's pre-read context
// check. No terminal newline means that EOF also returns the last logical line.
func TestTraceCancellationAtTerminalRead(t *testing.T) {
	const header = `{"version":{"major":1,"minor":0}}`
	const record = `{"file":"/p/CMakeLists.txt","line":7,"cmd":"message","args":["owned"]}`
	for _, tc := range []struct {
		name       string
		content    string
		limits     Limits
		hasRecord  bool
		incomplete []Reason
	}{
		{name: "valid_record", content: header + "\n" + record, hasRecord: true},
		{name: "header_only", content: header},
		{
			name: "malformed_tail", content: header + "\n" + record + "\n{",
			hasRecord: true, incomplete: []Reason{ReasonInputMalformed},
		},
		{
			name: "oversized_tail", content: header + "\n" + record + "\n" + strings.Repeat("x", 256),
			limits: Limits{MaxLineBytes: 128}, hasRecord: true,
			incomplete: []Reason{ReasonInputMalformed, ReasonLineLimit},
		},
	} {
		for _, mode := range []struct {
			name   string
			cancel bool
		}{{name: "live"}, {name: "canceled", cancel: true}} {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				reader := &terminalCancelTraceReader{reader: strings.NewReader(tc.content)}
				if mode.cancel {
					reader.cancel = cancel
				}
				path := unusedTracePath(t)
				got := Trace(ctx, Args{TracePath: path}, Deps{FS: fsReturning(reader), Limits: tc.limits})
				if reader.eofReads != 1 || reader.closeCalls != 1 {
					t.Fatalf("EOF reads=%d closes=%d, want one terminal read and one close", reader.eofReads, reader.closeCalls)
				}

				want := Result{Evidence: evidence.Evidence{Paths: []string{path}}}
				if mode.cancel {
					if ctx.Err() != context.Canceled {
						t.Fatalf("terminal read did not cancel context: %v", ctx.Err())
					}
					want.Status, want.Reason = evidence.StatusUnknown, ReasonCanceled
				} else {
					if ctx.Err() != nil {
						t.Fatalf("live control was canceled: %v", ctx.Err())
					}
					want.Status = evidence.StatusOK
					want.VersionHeaderPresent = true
					if tc.hasRecord {
						want.Records = []Record{{File: "/p/CMakeLists.txt", Line: 7, Cmd: "message", Args: []string{"owned"}}}
						want.ExecutedLines = []FileLines{{File: "/p/CMakeLists.txt", Lines: []int{7}}}
						want.FilesInTrace = []string{"/p/CMakeLists.txt"}
					} else {
						want.Status, want.Reason = evidence.StatusUnknown, ReasonNoRecordsMatched
					}
					if len(tc.incomplete) != 0 {
						want.InputIncomplete = true
						want.InputIncompleteReasons = tc.incomplete
						want.MalformedLineCount = 1
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("Trace result=%+v, want %+v", got, want)
				}

				body, err := publicresult.MarshalIndent(got)
				if err != nil {
					t.Fatal(err)
				}
				var wire Result
				if err := json.Unmarshal(body, &wire); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(wire, want) {
					t.Fatalf("public result=%+v, want %+v", wire, want)
				}
			})
		}
	}
}

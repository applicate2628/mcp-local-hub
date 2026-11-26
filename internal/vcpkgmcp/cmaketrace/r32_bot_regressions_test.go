package cmaketrace

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestR32TraceRequiresPresentNonNullArgsArray(t *testing.T) {
	trace := strings.Join([]string{
		`{"version":{"major":1,"minor":0}}`,
		`{"file":"missing.cmake","line":1,"cmd":"include"}`,
		`{"file":"null.cmake","line":2,"cmd":"include","args":null}`,
		`{"file":"empty.cmake","line":3,"cmd":"message","args":[]}`,
	}, "\n") + "\n"
	parsed, err := parseTraceStream(t.Context(), strings.NewReader(trace), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.malformedCount != 2 {
		t.Fatalf("malformed=%d, want missing and null args rejected", parsed.malformedCount)
	}
	if len(parsed.records) != 1 || parsed.records[0].File != "empty.cmake" || parsed.records[0].Args == nil || len(parsed.records[0].Args) != 0 {
		t.Fatalf("records=%+v, want one valid record with a present empty args array", parsed.records)
	}
}

var errR32ReadAfterCancel = errors.New("reader called after cancellation")

type r32CancelAfterChunkReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *r32CancelAfterChunkReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads > 1 {
		return 0, errR32ReadAfterCancel
	}
	for i := range p {
		p[i] = 'x'
	}
	r.cancel()
	return len(p), nil
}

func TestR32OversizedLineDrainStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &r32CancelAfterChunkReader{cancel: cancel}
	limits := Limits{MaxTraceBytes: 1 << 20, MaxLineBytes: 1, MaxParsedRecords: 10, MaxRetainedRecordBytes: 1 << 20}

	_, err := parseTraceStream(ctx, reader, limits)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context cancellation while draining oversized line", err)
	}
	if errors.Is(err, errR32ReadAfterCancel) || reader.reads != 1 {
		t.Fatalf("reads=%d err=%v, parser read again after cancellation", reader.reads, err)
	}
}

var _ io.Reader = (*r32CancelAfterChunkReader)(nil)

type traceStallReader struct {
	remaining int
}

func (r *traceStallReader) Read([]byte) (int, error) {
	if r.remaining == 0 {
		return 0, errors.New("trace parser kept retrying a stalled reader")
	}
	r.remaining--
	return 0, nil
}

func TestParseTraceStreamReportsNoProgress(t *testing.T) {
	line := strings.NewReader(`{"file":"f","line":1,"cmd":"set","args":[]}` + "\n")
	reader := io.MultiReader(line, &traceStallReader{remaining: 1024})
	result, err := parseTraceStream(t.Context(), reader, Limits{})
	if !errors.Is(err, io.ErrNoProgress) || len(result.records) != 0 || result.sawAnyContent {
		t.Fatalf("stalled trace returned records=%d content=%t error=%v", len(result.records), result.sawAnyContent, err)
	}
}

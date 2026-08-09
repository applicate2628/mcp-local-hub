package lastfailure

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type streamTestFS struct {
	open      func() io.ReadCloser
	opens     int
	openError error
}

type streamRegularFileInfo struct{}

func (streamRegularFileInfo) Name() string       { return "stream.log" }
func (streamRegularFileInfo) Size() int64        { return 0 }
func (streamRegularFileInfo) Mode() os.FileMode  { return 0 }
func (streamRegularFileInfo) ModTime() time.Time { return time.Time{} }
func (streamRegularFileInfo) IsDir() bool        { return false }
func (streamRegularFileInfo) Sys() any           { return nil }

func (*streamTestFS) Stat(string) (os.FileInfo, error)  { return streamRegularFileInfo{}, nil }
func (*streamTestFS) OpenDir(string) (DirReader, error) { return nil, os.ErrNotExist }
func (f *streamTestFS) Open(string) (io.ReadCloser, error) {
	f.opens++
	if f.openError != nil {
		return nil, f.openError
	}
	return f.open(), nil
}

type generatedReader struct {
	remaining int64
	fill      byte
	reads     int
	closed    int
	closeErr  error
	readErr   error
	cancel    context.CancelFunc
}

type partialErrorReader struct {
	data   []byte
	err    error
	read   bool
	closed int
}

func (r *partialErrorReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	n := copy(p, r.data)
	return n, r.err
}

func (r *partialErrorReader) Close() error { r.closed++; return nil }

type onePathStreamFS struct {
	FS
	path   string
	reader io.ReadCloser
}

func (f *onePathStreamFS) Open(path string) (io.ReadCloser, error) {
	if path == f.path {
		return f.reader, nil
	}
	return f.FS.Open(path)
}

func (r *generatedReader) Read(p []byte) (int, error) {
	r.reads++
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	if r.readErr != nil {
		err := r.readErr
		r.readErr = nil
		return 0, err
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}
	for i := 0; i < n; i++ {
		p[i] = r.fill
	}
	r.remaining -= int64(n)
	return n, nil
}

func (r *generatedReader) Close() error {
	r.closed++
	return r.closeErr
}

func scanGenerated(t *testing.T, size, limit int64, fill byte) (phaseLogScanResult, *phaseLogStreamScanner, *generatedReader, error) {
	t.Helper()
	reader := &generatedReader{remaining: size, fill: fill}
	fsys := &streamTestFS{open: func() io.ReadCloser { return reader }}
	scanner := newPhaseLogStreamScanner()
	accumulator := newDiagnosticAccumulator(defaultResponseLimits.diagnosticsPerPhaseCell)
	result, err := scanner.scan(context.Background(), fsys,
		phaseLogFile{Phase: PhaseBuild, Config: "rel", Path: "generated.log"},
		limit, defaultResponseLimits.diagnosticsPerLogCell,
		defaultResponseLimits.commandBytes, defaultResponseLimits.logLineBytes, accumulator)
	return result, scanner, reader, err
}

func TestScanPhaseLogStream_OwnedBuffersDoNotScaleWithLogBytes(t *testing.T) {
	const size = int64(256 << 20)
	result, scanner, reader, err := scanGenerated(t, size, size, 'x')
	if err != nil {
		t.Fatalf("scan generated stream: %v", err)
	}
	if result.bytesRead != size || result.truncated {
		t.Fatalf("bytes=%d truncated=%v, want %d/false", result.bytesRead, result.truncated, size)
	}
	if result.logBufferBytes > defaultResponseLimits.logLineBytes+phaseLogReadChunkBytes {
		t.Fatalf("log_buffer_bytes=%d, want <= %d", result.logBufferBytes, defaultResponseLimits.logLineBytes+phaseLogReadChunkBytes)
	}
	owned := cap(scanner.readBuffer) + cap(scanner.lineBuffer)
	if result.logBufferBytes != owned {
		t.Fatalf("reported log_buffer_bytes=%d, owned read+framing capacity=%d", result.logBufferBytes, owned)
	}
	if reader.closed != 1 {
		t.Fatalf("close count=%d, want 1", reader.closed)
	}
}

func TestScanPhaseLogStream_LogBufferHighWaterWireShape(t *testing.T) {
	body, err := json.Marshal(ResourceReport{HighWater: HighWater{LogBufferBytes: 123}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	highWater, ok := decoded["high_water"].(map[string]any)
	if !ok || highWater["log_buffer_bytes"] != float64(123) {
		t.Fatalf("wire=%s, want high_water.log_buffer_bytes=123", body)
	}
	if _, leaked := highWater["LogBufferBytes"]; leaked {
		t.Fatalf("wire=%s leaks Go field name", body)
	}
}

func TestScanPhaseLogStream_PerLogAndTotalSentinelsFailClosed(t *testing.T) {
	const limit = int64(16)
	exact, _, _, err := scanGenerated(t, limit, limit, '\n')
	if err != nil || exact.truncated || exact.bytesRead != limit {
		t.Fatalf("exact limit: result=%+v err=%v", exact, err)
	}
	over, _, _, err := scanGenerated(t, limit+1, limit, '\n')
	if err != nil || !over.truncated || over.bytesRead != limit {
		t.Fatalf("limit+1: result=%+v err=%v", over, err)
	}

	root := writeInstallPhasePort(t, "bytes", 1, strings.Repeat("x", 40))
	limits := defaultResponseLimits
	limits.totalLogBytes, limits.logBytes = 16, 64
	res := lastFailureWithLimits(context.Background(),
		Args{Port: "bytes", Triplet: "cl", BuildtreesRoot: root},
		Deps{FS: DefaultFS(), Getenv: func(string) string { return "" }}, limits)
	if res.Status != Status("unknown") || res.Reason != ReasonArtifactLimitExceeded ||
		res.Resources.Completeness.LogBytes || res.Resources.HighWater.LogBytes != limits.totalLogBytes {
		t.Fatalf("total sentinel returned %+v", res)
	}
}

func TestScanPhaseLogStream_LineAndValueBounds(t *testing.T) {
	prefix := "source.cpp:1:1: error: "
	exactLine := prefix + strings.Repeat("x", defaultResponseLimits.logLineBytes-len(prefix)) + "\n"
	fsys := &streamTestFS{open: func() io.ReadCloser {
		return io.NopCloser(strings.NewReader(exactLine))
	}}
	scanner := newPhaseLogStreamScanner()
	accumulator := newDiagnosticAccumulator(defaultResponseLimits.diagnosticsPerPhaseCell)
	result, err := scanner.scan(context.Background(), fsys,
		phaseLogFile{Phase: PhaseBuild, Config: "rel", Path: "line.log"},
		int64(len(exactLine)), defaultResponseLimits.diagnosticsPerLogCell,
		defaultResponseLimits.commandBytes, defaultResponseLimits.logLineBytes, accumulator)
	if err != nil || result.diagnosticsIncomplete {
		t.Fatalf("4 MiB line: result=%+v err=%v", result, err)
	}
	ranked := accumulator.ranked()
	if len(ranked) != 1 || len(ranked[0].diagnostic.Text) > MaxDiagnosticTextBytes ||
		len(ranked[0].diagnostic.File) > MaxWirePathBytes {
		t.Fatalf("bounded diagnostic=%+v", ranked)
	}

	overlong := strings.Repeat("y", defaultResponseLimits.logLineBytes+1) + "\n" +
		"Run Build Command(s): " + strings.Repeat("z", MaxWireCommandBytes+1024) + "\n"
	fsys.open = func() io.ReadCloser { return io.NopCloser(strings.NewReader(overlong)) }
	accumulator = newDiagnosticAccumulator(defaultResponseLimits.diagnosticsPerPhaseCell)
	result, err = scanner.scan(context.Background(), fsys,
		phaseLogFile{Phase: PhaseBuild, Config: "rel", Path: "overlong.log"},
		int64(len(overlong)), defaultResponseLimits.diagnosticsPerLogCell,
		defaultResponseLimits.commandBytes, defaultResponseLimits.logLineBytes, accumulator)
	if err != nil || !result.diagnosticsIncomplete {
		t.Fatalf("4 MiB+1 line: result=%+v err=%v", result, err)
	}
	if len(result.buildCommand) > MaxWireCommandBytes {
		t.Fatalf("build command bytes=%d, want <=%d", len(result.buildCommand), MaxWireCommandBytes)
	}
}

func TestScanPhaseLogStream_ProducerAggregationCapsBeforeResultMaterialization(t *testing.T) {
	var lines []string
	for _, severity := range []string{SeverityError, "warning"} {
		for _, tier := range []DiagnosticTier{TierSpecific, TierAggregate} {
			for i := 0; i < 5; i++ {
				if severity == SeverityError && tier == TierSpecific {
					lines = append(lines, "a.cpp:1:1: error: specific")
				} else if severity == SeverityError {
					lines = append(lines, "FAILED: [code=1] target")
				} else if tier == TierSpecific {
					lines = append(lines, "a.cpp:1:1: warning: specific")
				} else {
					lines = append(lines, "tool.lib : warning LNK1120: 1 unresolved externals")
				}
			}
		}
	}
	content := strings.Join(lines, "\n") + "\n"
	fsys := &streamTestFS{open: func() io.ReadCloser { return io.NopCloser(strings.NewReader(content)) }}
	accumulator := newDiagnosticAccumulator(2)
	result, err := newPhaseLogStreamScanner().scan(context.Background(), fsys,
		phaseLogFile{Phase: PhaseBuild, Path: "rank.log"}, int64(len(content)), 3,
		defaultResponseLimits.commandBytes, defaultResponseLimits.logLineBytes, accumulator)
	if err != nil || result.diagnosticsIncomplete {
		t.Fatalf("scan result=%+v err=%v", result, err)
	}
	ranked := accumulator.ranked()
	if accumulator.highWater != 8 || len(ranked) != 8 || accumulator.dropped != 12 {
		t.Fatalf("highWater=%d retained=%d dropped=%d, want 8/8/12", accumulator.highWater, len(ranked), accumulator.dropped)
	}
	if ranked[0].diagnostic.Severity != SeverityError || ranked[0].diagnostic.Tier != TierSpecific {
		t.Fatalf("first candidate=%+v", ranked[0])
	}
}

func TestScanPhaseLogStream_CancelReadClosePaths(t *testing.T) {
	t.Run("pre-cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fsys := &streamTestFS{open: func() io.ReadCloser { return &generatedReader{remaining: 1} }}
		_, err := newPhaseLogStreamScanner().scan(ctx, fsys, phaseLogFile{Path: "x"}, 1, 1, 1, 1, newDiagnosticAccumulator(1))
		if !errors.Is(err, context.Canceled) || fsys.opens != 0 {
			t.Fatalf("err=%v opens=%d, want context.Canceled/0", err, fsys.opens)
		}
	})

	t.Run("mid-read-cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &generatedReader{remaining: 128, fill: 'x', cancel: cancel}
		fsys := &streamTestFS{open: func() io.ReadCloser { return reader }}
		_, err := newPhaseLogStreamScanner().scan(ctx, fsys, phaseLogFile{Path: "x"}, 128, 1, 1, 1, newDiagnosticAccumulator(1))
		if !errors.Is(err, context.Canceled) || reader.reads != 1 || reader.closed != 1 {
			t.Fatalf("err=%v reads=%d closes=%d", err, reader.reads, reader.closed)
		}
	})

	t.Run("read-error", func(t *testing.T) {
		boom := errors.New("read boom")
		reader := &generatedReader{remaining: 1, readErr: boom}
		fsys := &streamTestFS{open: func() io.ReadCloser { return reader }}
		_, err := newPhaseLogStreamScanner().scan(context.Background(), fsys, phaseLogFile{Path: "x"}, 1, 1, 1, 1, newDiagnosticAccumulator(1))
		if !errors.Is(err, boom) || reader.closed != 1 {
			t.Fatalf("err=%v closes=%d", err, reader.closed)
		}
	})

	t.Run("close-error", func(t *testing.T) {
		boom := errors.New("close boom")
		reader := &generatedReader{remaining: 1, fill: 'x', closeErr: boom}
		fsys := &streamTestFS{open: func() io.ReadCloser { return reader }}
		_, err := newPhaseLogStreamScanner().scan(context.Background(), fsys, phaseLogFile{Path: "x"}, 1, 1, 1, 1, newDiagnosticAccumulator(1))
		if !errors.Is(err, boom) || reader.closed != 1 {
			t.Fatalf("err=%v closes=%d", err, reader.closed)
		}
	})

	t.Run("orchestrator-rolls-back-partial-read", func(t *testing.T) {
		root := writeBuildPhasePort(t, "rollback", map[string]string{
			"cl.vcpkg_abi_info.txt": "abi\n",
			"build-cl-rel-err.log":  "placeholder\n",
		})
		path := filepath.Join(root, "rollback", "build-cl-rel-err.log")
		boom := errors.New("partial read boom")
		reader := &partialErrorReader{data: []byte("a.cpp:1:1: error: must be rolled back\n"), err: boom}
		fsys := &onePathStreamFS{FS: DefaultFS(), path: path, reader: reader}
		res := LastFailure(Args{Port: "rollback", Triplet: "cl", BuildtreesRoot: root},
			Deps{FS: fsys, Getenv: func(string) string { return "" }})
		if res.Status != Status("unknown") || res.Reason != ReasonPhaseLogUnreadable || len(res.Diagnostics) != 0 {
			t.Fatalf("partial read leaked a confident or retained diagnostic: %+v", res)
		}
		if reader.closed != 1 {
			t.Fatalf("close count=%d, want 1", reader.closed)
		}
	})
}

func TestScanPhaseLogStream_PreservesThreeConsumerLineSemantics(t *testing.T) {
	content := "progress\rUser interrupt\r\n" +
		"\x1b[31mansi.cpp:3:5: error: boom\x1b[0m\n" +
		"[3/9] Building a.cpp\rclang-cl: error: overwritten\n" +
		"Run Build Command(s): first --flag\n" +
		"Run Build Command(s): second\n" +
		strings.Repeat("q", defaultResponseLimits.logLineBytes+1) + "\n" +
		"ninja: build stopped: interrupted by user.\n"
	fsys := &streamTestFS{open: func() io.ReadCloser { return io.NopCloser(strings.NewReader(content)) }}
	accumulator := newDiagnosticAccumulator(defaultResponseLimits.diagnosticsPerPhaseCell)
	result, err := newPhaseLogStreamScanner().scan(context.Background(), fsys,
		phaseLogFile{Phase: PhaseBuild, Config: "rel", Path: "semantics.log"},
		int64(len(content)), defaultResponseLimits.diagnosticsPerLogCell,
		defaultResponseLimits.commandBytes, defaultResponseLimits.logLineBytes, accumulator)
	if err != nil {
		t.Fatal(err)
	}
	if !result.interrupted || !result.diagnosticsIncomplete || result.buildCommand != "first --flag" {
		t.Fatalf("result=%+v", result)
	}
	ranked := accumulator.ranked()
	if len(ranked) != 1 || ranked[0].diagnostic.File != "ansi.cpp" {
		t.Fatalf("diagnostics=%+v; CR-overwritten diagnostic must remain unmatched", ranked)
	}

	// DetectInterrupted historically normalizes and trims the entire CR/LF
	// segment. A line can exceed the diagnostic scanner's 4 MiB ceiling solely
	// through leading whitespace and still be exactly an interrupt marker after
	// TrimSpace; streaming must not lose that higher-precedence fact.
	paddedInterrupt := "FAILED: [code=1] x.obj\n" + strings.Repeat("\u2003", defaultResponseLimits.logLineBytes/3+1) +
		"\ufeff\x1b[31mUser interrupt\x1b[0m \t\n"
	fsys.open = func() io.ReadCloser { return io.NopCloser(strings.NewReader(paddedInterrupt)) }
	result, err = newPhaseLogStreamScanner().scan(context.Background(), fsys,
		phaseLogFile{Phase: PhaseBuild, Config: "rel", Path: "padded-interrupt.log"},
		int64(len(paddedInterrupt)), defaultResponseLimits.diagnosticsPerLogCell,
		defaultResponseLimits.commandBytes, defaultResponseLimits.logLineBytes,
		newDiagnosticAccumulator(defaultResponseLimits.diagnosticsPerPhaseCell))
	if err != nil || !result.interrupted || !result.diagnosticsIncomplete {
		t.Fatalf("overlong padded interrupt: result=%+v err=%v", result, err)
	}
}

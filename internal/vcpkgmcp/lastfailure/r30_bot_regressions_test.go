package lastfailure

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestR30StandaloneUserInterruptNeedsStructuralCorrelation(t *testing.T) {
	content := []byte("source.cpp:7:3: error: real compiler failure\nUser interrupt\n")
	if DetectInterrupted(content) {
		t.Fatal("standalone project output suppressed a corroborated compiler failure as build_interrupted")
	}
	if !DetectInterrupted([]byte("FAILED: [code=1] source.obj\nUser interrupt\n")) {
		t.Fatal("FAILED plus relayed User interrupt must remain a correlated interruption")
	}
}

func TestR30StreamingInterruptUsesTheSameCorrelationRule(t *testing.T) {
	scan := func(content string) phaseLogScanResult {
		t.Helper()
		fsys := &streamTestFS{open: func() io.ReadCloser {
			return io.NopCloser(strings.NewReader(content))
		}}
		result, err := newPhaseLogStreamScanner().scan(
			context.Background(), fsys, phaseLogFile{Phase: PhaseBuild, Path: "r30.log"},
			int64(len(content)), defaultResponseLimits.diagnosticsPerLogCell,
			defaultResponseLimits.commandBytes, defaultResponseLimits.logLineBytes,
			newDiagnosticAccumulator(defaultResponseLimits.diagnosticsPerPhaseCell),
		)
		if err != nil {
			t.Fatalf("scan(%q): %v", content, err)
		}
		return result
	}

	if result := scan("source.cpp:7:3: error: real compiler failure\nUser interrupt\n"); result.interrupted {
		t.Fatalf("standalone weak marker interrupted=%v, want false", result.interrupted)
	}
	if result := scan("FAILED: [code=1] source.obj\nUser interrupt\n"); !result.interrupted {
		t.Fatalf("correlated weak marker interrupted=%v, want true", result.interrupted)
	}
	if result := scan("ninja: build stopped: interrupted by user.\n"); !result.interrupted {
		t.Fatalf("strong ninja marker interrupted=%v, want true", result.interrupted)
	}
}

func TestR30SeparateUnsafeOptionValueIsRedacted(t *testing.T) {
	if got := redactCommandForWire("curl --user alice:secret"); got != redactedCommand {
		t.Fatalf("redactCommandForWire=%q, want %q for an unclassified separate option value", got, redactedCommand)
	}
	if got := redactCommandForWire("vcpkg install zlib --triplet x64-windows"); got == redactedCommand {
		t.Fatal("ordinary positional arguments and allowlisted option values must remain reproducible")
	}
}

func TestR30GCCDiagnosticWithoutColumn(t *testing.T) {
	diagnostics := ScanDiagnostics([]byte("source.cpp:17: error: column display disabled\n"))
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics=%+v, want one GCC diagnostic without a column", diagnostics)
	}
	if diagnostics[0].File != "source.cpp" || diagnostics[0].Line != 17 || diagnostics[0].Severity != SeverityError {
		t.Fatalf("diagnostic=%+v, want source.cpp:17 error", diagnostics[0])
	}
}

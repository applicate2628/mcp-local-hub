package lastfailure

import (
	"strings"
	"testing"
)

func TestR29CMakeErrorRetainsIndentedContinuation(t *testing.T) {
	diagnostics := ScanDiagnostics([]byte("CMake Error at CMakeLists.txt:1 (message):\n  boom\n\n"))
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics=%+v, want one CMake diagnostic", diagnostics)
	}
	if !strings.Contains(diagnostics[0].Text, "boom") {
		t.Fatalf("text=%q, want multiline CMake cause", diagnostics[0].Text)
	}
}

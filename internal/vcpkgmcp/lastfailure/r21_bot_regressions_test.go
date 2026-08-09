package lastfailure

import (
	"encoding/json"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR21ProjectionEnumeratesEveryOmittedSemanticField(t *testing.T) {
	result := Result{
		Status:        evidence.StatusFailed,
		Diagnostics:   []Diagnostic{{Text: "error"}},
		LogPaths:      []string{"build.log"},
		OverlayChain:  []string{"overlay"},
		ContextSource: []ContextSource{SourceBuildtrees},
		Notes:         []Note{NoteWrapperNotSupplied},
		Evidence:      evidence.Evidence{Paths: []string{"build.log"}},
	}
	body, err := json.Marshal(result.PublicResultProjection())
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	var decoded struct {
		ResultProjection struct {
			Omissions []struct {
				Field string `json:"field"`
			} `json:"omissions"`
		} `json:"result_projection"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode projection: %v", err)
	}
	got := map[string]bool{}
	for _, omission := range decoded.ResultProjection.Omissions {
		got[omission.Field] = true
	}
	for _, field := range []string{"diagnostics", "log_paths", "overlay_chain", "context_source", "notes", "evidence"} {
		if !got[field] {
			t.Fatalf("projection omissions=%v, missing %q", got, field)
		}
	}
}

func TestR21CMakeErrorIsRecognizedAsSpecificFailure(t *testing.T) {
	line := "CMake Error at CMakeLists.txt:10 (message):"
	diag, ok := matchDiagnosticLine(line)
	if !ok {
		t.Fatalf("CMake cause was not recognized: %q", line)
	}
	if diag.Severity != SeverityError || diag.Tier != TierSpecific {
		t.Fatalf("CMake cause=%+v, want error/specific", diag)
	}
}

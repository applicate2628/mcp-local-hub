package cmakegraph

import (
	"context"
	"testing"
)

func TestR31InvalidControlTerminatorStopsConfidentEdgeResolution(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "unmatched endif", content: "endif()\ninclude(later.cmake)\n"},
		{name: "unmatched endforeach", content: "endforeach()\ninclude(later.cmake)\n"},
		{name: "unmatched endwhile", content: "endwhile()\ninclude(later.cmake)\n"},
		{name: "mismatched terminator", content: "if(FLAG)\nendforeach()\ninclude(later.cmake)\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			entry := writeFile(t, root, "CMakeLists.txt", test.content)
			writeFile(t, root, "later.cmake", "# leaf\n")

			result, err := Walk(context.Background(), entry, root, DefaultOptions())
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if len(result.Edges) != 0 {
				t.Fatalf("edges=%+v, want no confidently resolved edge after invalid control flow", result.Edges)
			}
			if len(result.UnscannedFiles) != 1 || result.UnscannedFiles[0].Reason != CoverageControlFlowInvalid {
				t.Fatalf("unscanned_files=%+v, want one control_flow_invalid coverage hole", result.UnscannedFiles)
			}
		})
	}
}

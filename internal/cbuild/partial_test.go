package cbuild

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCTestReportsCaptureTruncation(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bindir := t.TempDir()
	copyExe(t, self, filepath.Join(bindir, exeName("ctest")))
	t.Setenv("CMAKE_BIN", filepath.Join(bindir, exeName("cmake")))
	for _, mode := range []string{"small", "large"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("MCP_CBUILD_FAKE_CTEST", mode)
			value, err := callWithArgs(t, toolNamed(t, "cmake_test"), map[string]any{
				"working_dir": t.TempDir(), "preset": "default",
			})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var got testResult
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			wantPartial := mode == "large"
			wantTotal := 2
			if wantPartial {
				wantTotal = 1
			}
			if got.ResultsPartial != wantPartial || got.OutputTruncated != wantPartial ||
				got.Total != wantTotal || got.Passed != wantTotal || len(got.Tests) != wantTotal {
				t.Fatalf("mode %s: counts/partial flags = %+v", mode, got)
			}
			if !got.Success || got.ExitCode != 0 {
				t.Fatalf("command success changed by capture metadata: %+v", got)
			}
		})
	}
}

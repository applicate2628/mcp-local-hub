package cmakegraph

import (
	"context"
	"testing"
)

func TestR28LowercaseOptionalIsNotCMakeOption(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "CMakeLists.txt", "include(missing.cmake optional)\n")
	result, err := Walk(context.Background(), root, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 1 || result.Edges[0].Status != StatusUnresolved || result.Edges[0].Reason != ReasonParseError {
		t.Fatalf("edges=%+v, want lowercase non-keyword tail to remain a parse error", result.Edges)
	}
}

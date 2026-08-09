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
	if len(result.Edges) != 1 || result.Edges[0].Status != StatusDangling {
		t.Fatalf("edges=%+v, want required missing include to be dangling", result.Edges)
	}
}

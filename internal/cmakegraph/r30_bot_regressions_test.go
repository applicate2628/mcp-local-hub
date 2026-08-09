package cmakegraph

import (
	"context"
	"path/filepath"
	"testing"
)

func TestR30CommandNameCannotCrossLineEnding(t *testing.T) {
	commands, truncated := scanTopLevelCommands([]byte("include\n(foo.cmake)\nadd_subdirectory\r\n(foo)\n"), 10)
	if truncated {
		t.Fatal("short malformed fixture unexpectedly hit the command budget")
	}
	if len(commands) != 0 {
		t.Fatalf("commands=%+v, want no CMake invocation across a line ending", commands)
	}
}

func TestR30MalformedIncludeTailIsUnresolved(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "CMakeLists.txt", "include(missing.cmake OPTIONAL junk)\n")
	result, err := Walk(context.Background(), root, dir, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(result.Edges) != 1 {
		t.Fatalf("edges=%+v, want one unresolved call site", result.Edges)
	}
	edge := result.Edges[0]
	if edge.Status != StatusUnresolved || edge.Reason != ReasonParseError {
		t.Fatalf("edge=%+v, want unresolved/%s rather than optional absence", edge, ReasonParseError)
	}
	if edge.ResolvedPath != "" && filepath.Clean(edge.ResolvedPath) != edge.ResolvedPath {
		t.Fatalf("unexpected malformed resolved path %q", edge.ResolvedPath)
	}
}

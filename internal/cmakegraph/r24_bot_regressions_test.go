package cmakegraph

import (
	"context"
	"path/filepath"
	"testing"
)

func TestR24DeferredCommandBodyRefusesBareRelativePath(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "CMakeLists.txt", "include(defs.cmake)\nadd_subdirectory(child)\n")
	writeFile(t, dir, "defs.cmake", "macro(load_target)\ninclude(target.cmake)\nendmacro()\n")
	writeFile(t, dir, filepath.Join("child", "CMakeLists.txt"), "load_target()\n")
	writeFile(t, dir, filepath.Join("child", "target.cmake"), "")
	writeFile(t, dir, "target.cmake", "")

	result, err := Walk(context.Background(), root, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range result.Edges {
		if filepath.Base(edge.FromFile) == "defs.cmake" && edge.RawArg == "target.cmake" {
			if edge.Status != StatusUnresolved || edge.Reason != ReasonDeferredMacroContext {
				t.Fatalf("deferred relative edge=%+v, want unresolved/%s", edge, ReasonDeferredMacroContext)
			}
			return
		}
	}
	t.Fatalf("deferred relative include edge missing: %+v", result.Edges)
}

func TestR24ResultVariableValueNamedOptionalIsNotAnOption(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "CMakeLists.txt", "include(missing.cmake RESULT_VARIABLE OPTIONAL)\n")
	result, err := Walk(context.Background(), root, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 1 || result.Edges[0].Status != StatusDangling {
		t.Fatalf("edges=%+v, want required missing include to be dangling", result.Edges)
	}
}

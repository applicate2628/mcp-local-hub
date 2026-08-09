package cmakegraph

import (
	"context"
	"path/filepath"
	"testing"
)

func TestR32AddSubdirectoryAcceptsOnlyDocumentedTrailingGrammar(t *testing.T) {
	valid := []string{
		"add_subdirectory(child)",
		"add_subdirectory(child build)",
		"add_subdirectory(child EXCLUDE_FROM_ALL)",
		"add_subdirectory(child SYSTEM)",
		"add_subdirectory(child build EXCLUDE_FROM_ALL)",
		"add_subdirectory(child build SYSTEM)",
		"add_subdirectory(child EXCLUDE_FROM_ALL SYSTEM)",
		"add_subdirectory(child build EXCLUDE_FROM_ALL SYSTEM)",
		"add_subdirectory(child [=[EXCLUDE_FROM_ALL]=])",
		"add_subdirectory(child [=[SYSTEM]=])",
		"add_subdirectory(child [=[EXCLUDE_FROM_ALL]=] [=[SYSTEM]=])",
	}
	for _, command := range valid {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			root := writeFile(t, dir, "CMakeLists.txt", command+"\n")
			child := writeFile(t, dir, filepath.Join("child", "CMakeLists.txt"), "# leaf\n")

			result, err := Walk(context.Background(), root, dir, DefaultOptions())
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if len(result.Edges) != 1 || result.Edges[0].Status != StatusResolved {
				t.Fatalf("edges=%+v, want one resolved add_subdirectory edge", result.Edges)
			}
			if !fileInResult(result, child) {
				t.Fatalf("files=%v, want valid child %q traversed", result.Files, child)
			}
		})
	}

	invalid := []string{
		"add_subdirectory(child build junk)",
		"add_subdirectory(child EXCLUDE_FROM_ALL build)",
		"add_subdirectory(child SYSTEM build)",
		"add_subdirectory(child SYSTEM EXCLUDE_FROM_ALL)",
		"add_subdirectory(child EXCLUDE_FROM_ALL EXCLUDE_FROM_ALL)",
		"add_subdirectory(child SYSTEM SYSTEM)",
	}
	for _, command := range invalid {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			root := writeFile(t, dir, "CMakeLists.txt", command+"\n")
			child := writeFile(t, dir, filepath.Join("child", "CMakeLists.txt"), "include(leaked.cmake)\n")
			writeFile(t, dir, filepath.Join("child", "leaked.cmake"), "# leaf\n")

			result, err := Walk(context.Background(), root, dir, DefaultOptions())
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if len(result.Edges) != 1 {
				t.Fatalf("edges=%+v, want only the invalid call site", result.Edges)
			}
			edge := result.Edges[0]
			if edge.Status != StatusUnresolved || edge.Reason != ReasonParseError {
				t.Fatalf("edge=%+v, want unresolved/%s", edge, ReasonParseError)
			}
			if fileInResult(result, child) {
				t.Fatalf("files=%v, invalid add_subdirectory traversed %q", result.Files, child)
			}
		})
	}
}

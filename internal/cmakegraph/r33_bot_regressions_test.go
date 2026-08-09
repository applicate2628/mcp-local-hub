package cmakegraph

import (
	"context"
	"path/filepath"
	"testing"
)

func TestR33UnterminatedControlFrameRetractsItsEdges(t *testing.T) {
	tests := []struct {
		name   string
		opener string
	}{
		{name: "if", opener: "if(FLAG)"},
		{name: "foreach", opener: "foreach(item IN ITEMS values)"},
		{name: "while", opener: "while(FLAG)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			root := writeFile(t, dir, "CMakeLists.txt",
				"include(before.cmake)\n"+test.opener+"\ninclude(inside.cmake)\n")
			writeFile(t, dir, "before.cmake", "# leaf\n")
			writeFile(t, dir, "inside.cmake", "include(leaked.cmake)\n")
			writeFile(t, dir, "leaked.cmake", "# leaf\n")

			result, err := Walk(context.Background(), root, dir, DefaultOptions())
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if len(result.Edges) != 1 || result.Edges[0].RawArg != "before.cmake" {
				t.Fatalf("edges=%+v, want only edge before unterminated %s frame", result.Edges, test.name)
			}
			if result.RetainedEdgeBytes != retainedEdgeBytes(result.Edges[0]) {
				t.Fatalf("retained_edge_bytes=%d, want accounting for only retained edge (%d)", result.RetainedEdgeBytes, retainedEdgeBytes(result.Edges[0]))
			}
			if len(result.UnscannedFiles) != 1 || result.UnscannedFiles[0].Reason != CoverageControlFlowInvalid {
				t.Fatalf("unscanned_files=%+v, want one control_flow_invalid coverage hole", result.UnscannedFiles)
			}
		})
	}
}

func TestR33IncludeDirectoryIsUnresolvedWrongType(t *testing.T) {
	for _, optional := range []bool{false, true} {
		t.Run(map[bool]string{false: "required", true: "optional"}[optional], func(t *testing.T) {
			dir := t.TempDir()
			command := "include(target/)\n"
			if optional {
				command = "include(target/ OPTIONAL)\n"
			}
			root := writeFile(t, dir, "CMakeLists.txt", command)
			writeFile(t, dir, filepath.Join("target", "child.txt"), "not a CMake include file\n")

			result, err := Walk(context.Background(), root, dir, DefaultOptions())
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if len(result.Edges) != 1 {
				t.Fatalf("edges=%+v, want one include edge", result.Edges)
			}
			edge := result.Edges[0]
			if edge.Status != StatusUnresolved || edge.Reason != Reason("target_wrong_type") {
				t.Fatalf("edge=%+v, want unresolved/target_wrong_type", edge)
			}
		})
	}
}

func TestR33IncludeSemicolonHonorsQuoteProvenance(t *testing.T) {
	t.Run("quoted semicolon is literal", func(t *testing.T) {
		dir := t.TempDir()
		root := writeFile(t, dir, "CMakeLists.txt", "include(\"a;b.cmake\")\n")
		writeFile(t, dir, "a;b.cmake", "# leaf\n")

		result, err := Walk(context.Background(), root, dir, DefaultOptions())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(result.Edges) != 1 || result.Edges[0].Status != StatusResolved || result.Edges[0].RawArg != "a;b.cmake" {
			t.Fatalf("edges=%+v, want quoted semicolon preserved as one resolved path", result.Edges)
		}
	})

	t.Run("unquoted semicolon expands argument list", func(t *testing.T) {
		dir := t.TempDir()
		root := writeFile(t, dir, "CMakeLists.txt", "include(missing.cmake;OPTIONAL)\n")

		result, err := Walk(context.Background(), root, dir, DefaultOptions())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(result.Edges) != 1 || result.Edges[0].RawArg != "missing.cmake" || result.Edges[0].Status != StatusOptionalAbsent {
			t.Fatalf("edges=%+v, want unquoted list split before include option parsing", result.Edges)
		}
	})

	t.Run("unquoted second list element uses legacy ignored position", func(t *testing.T) {
		dir := t.TempDir()
		root := writeFile(t, dir, "CMakeLists.txt", "include(a.cmake;b.cmake)\n")
		writeFile(t, dir, "a.cmake", "# CMake executes this first list element\n")
		writeFile(t, dir, "b.cmake", "# CMake ignores this legacy second position\n")
		writeFile(t, dir, "a.cmake;b.cmake", "# quoted-only literal spelling\n")

		result, err := Walk(context.Background(), root, dir, DefaultOptions())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(result.Edges) != 1 || result.Edges[0].RawArg != "a.cmake" || result.Edges[0].Status != StatusResolved {
			t.Fatalf("edges=%+v, want only the first expanded list element resolved", result.Edges)
		}
	})
}

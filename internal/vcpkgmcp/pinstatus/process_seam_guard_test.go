package pinstatus

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRunContainedStreamImportGraphGuard(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	importOwners := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(gotoken.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", name, err)
			}
			switch path {
			case "mcp-local-hub/internal/process":
				importOwners[name] = true
			case "syscall", "golang.org/x/sys/windows", "golang.org/x/sys/unix":
				t.Fatalf("platform process import %q leaked into pinstatus owner %s", path, name)
			}
		}
	}
	if !importOwners["remote.go"] || !importOwners["pinstatus.go"] || len(importOwners) != 2 {
		t.Fatalf("internal/process import owners=%v, want only remote.go and pinstatus.go", importOwners)
	}
}

func TestRunContainedStreamSiblingCallerSourceGuard(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	protected := []string{
		"internal/cli/supervise.go",
		"internal/daemon/process_host.go",
		"internal/daemon/host.go",
		"internal/daemon/http_host.go",
		"internal/drmemory/runner.go",
		"internal/oneapirun/handlers.go",
		"internal/vtune/runner.go",
	}
	for _, relative := range protected {
		path := filepath.Join(root, filepath.FromSlash(relative))
		file, err := parser.ParseFile(gotoken.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse protected caller %s: %v", relative, err)
		}
		found := false
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "RunContainedStream" {
				found = true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "RunContainedStream" {
				found = true
			}
			return true
		})
		if found {
			t.Fatalf("protected sibling %s migrated to RunContainedStream", relative)
		}
	}
}

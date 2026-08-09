package portname

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseLegalAndIllegalNames(t *testing.T) {
	for _, raw := range []string{"zlib", "mcp-language-server", "abseil", "libpng16", "a-b-c", "x264"} {
		t.Run("legal-"+raw, func(t *testing.T) {
			name, err := Parse(raw)
			if err != nil || name.String() != raw {
				t.Fatalf("Parse(%q) = %#v, %v", raw, name, err)
			}
		})
	}
	for _, raw := range []string{"", "UPPERCASE", "-leading", "trailing-", "under_score", "sub/nested", `..\windows-sibling`, "../escape", ".."} {
		t.Run("illegal-"+raw, func(t *testing.T) {
			_, err := Parse(raw)
			var invalid *InvalidNameError
			if !errors.As(err, &invalid) {
				t.Fatalf("Parse(%q) error = %v, want InvalidNameError", raw, err)
			}
		})
	}
}

func TestJoinContainsOpaqueNameAndRejectsZeroOrEscape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	name, err := Parse("zlib")
	if err != nil {
		t.Fatal(err)
	}
	joined, err := Join(root, name)
	if err != nil || joined != filepath.Join(root, "zlib") {
		t.Fatalf("Join(%q, %q) = %q, %v", root, name.String(), joined, err)
	}
	if rel, err := filepath.Rel(root, joined); err != nil || rel != "zlib" {
		t.Fatalf("joined path relation = %q, %v; want zlib", rel, err)
	}

	if _, err := Join(root, Name{}); err == nil {
		t.Fatal("Join accepted zero Name")
	} else {
		var invalid *InvalidNameError
		if !errors.As(err, &invalid) {
			t.Fatalf("zero Name error = %T %v, want InvalidNameError", err, err)
		}
	}

	if _, err := Join(root, Name{value: "../escape"}); err == nil {
		t.Fatal("Join accepted a forged escaping Name")
	} else {
		var escapes *EscapesRootError
		if !errors.As(err, &escapes) {
			t.Fatalf("escape error = %T %v, want EscapesRootError", err, err)
		}
	}
}

func TestPortNamePolicyHasOneProductionOwner(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	thisDir := filepath.Dir(thisFile)
	for _, relative := range []string{
		filepath.Join("..", "portresolution", "portresolution.go"),
		filepath.Join("..", "lastfailure", "buildtrees.go"),
	} {
		source, err := os.ReadFile(filepath.Clean(filepath.Join(thisDir, relative)))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"portNameRE", "errPortEscapesRoot", "func portDirWithin", `^[a-z0-9]+(?:-+[a-z0-9]+)*$`} {
			if strings.Contains(string(source), forbidden) {
				t.Fatalf("%s retains duplicate port-name policy %q", relative, forbidden)
			}
		}
	}

	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(thisDir, "portname.go"), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imported := range file.Imports {
		path := strings.Trim(imported.Path.Value, `"`)
		if strings.HasPrefix(path, "mcp-local-hub/") {
			t.Fatalf("portname must stay a standard-library leaf, imports %q", path)
		}
	}
}

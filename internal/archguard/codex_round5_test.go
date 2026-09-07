package archguard

import (
	"go/build/constraint"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceRootRejectsSymlinkedAncestor(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"target/internal/x.go": "package internal\n",
	})
	if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"link/internal"}
	_, err := Scan(t.Context(), ScanOptions{Root: root, Policy: policy})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error=%v, want symlinked-ancestor rejection", err)
	}
}

func TestPolicyRejectsInvalidConstructorSymbol(t *testing.T) {
	body := strings.Replace(validPolicyJSON, `"symbol": "NewAPI"`, `"symbol": "New API"`, 1)
	_, err := LoadPolicy(writeTempFile(t, body))
	if err == nil || !strings.Contains(err.Error(), "identifier") {
		t.Fatalf("error=%v, want constructor identifier validation", err)
	}
}

func TestConstructorFunctionValueReferenceIsDetected(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x
import api "mcp-local-hub/internal/api"
func F() {
	factory := api.NewAPI
	_ = factory
}
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, policy), KindAPIConstruction)
	if len(got) != 1 || got[0].Location.Symbol != "F" {
		t.Fatalf("got=%#v, want constructor function-value reference", got)
	}
}

func TestMutuallyExclusivePlatformPackageNamesRemainScannable(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/variant/ctor_linux.go": `package linuxctor
func New() any { return nil }
`,
		"internal/variant/ctor_windows.go": `package windowsctor
func New() any { return nil }
`,
		"internal/use/use_linux.go": `package use
import "mcp-local-hub/internal/variant"
func Linux() { _ = linuxctor.New() }
`,
		"internal/use/use_windows.go": `package use
import "mcp-local-hub/internal/variant"
func Windows() { _ = windowsctor.New() }
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.APIConstructors = []SymbolRule{{ImportPath: "mcp-local-hub/internal/variant", Symbol: "New"}}
	report, err := Scan(t.Context(), ScanOptions{Root: root, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	got := violationsOfKind(report, KindAPIConstruction)
	if len(got) != 2 {
		t.Fatalf("got=%#v, want one platform-specific finding per importer", got)
	}
	symbols := map[string]bool{}
	for _, violation := range got {
		symbols[violation.Location.Symbol] = true
	}
	if !symbols["Linux"] || !symbols["Windows"] {
		t.Fatalf("symbols=%#v, want Linux and Windows", symbols)
	}
}

func TestBuildCompatibilityUsesValidTargetAndCompilerDimensions(t *testing.T) {
	if buildConstraintsOverlap(&constraint.TagExpr{Tag: "js"}, &constraint.TagExpr{Tag: "amd64"}) {
		t.Fatal("js/wasm and amd64 constraints must not overlap")
	}
	if buildConstraintsOverlap(&constraint.TagExpr{Tag: "gc"}, &constraint.TagExpr{Tag: "gccgo"}) {
		t.Fatal("gc and gccgo compiler constraints must not overlap")
	}
	if !buildConstraintsOverlap(&constraint.TagExpr{Tag: "linux"}, &constraint.TagExpr{Tag: "amd64"}) {
		t.Fatal("linux and amd64 constraints must overlap on linux/amd64")
	}
}

func TestGoIgnoredSourceNamesAreNotScanned(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/real.go":     "package x\n",
		"internal/x/_ignored.go": "package x\nvar hidden = 1\n",
		"internal/x/.ignored.go": "package x\nvar dotted = 1\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	if got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal); len(got) != 0 {
		t.Fatalf("ignored Go files produced findings: %#v", got)
	}
}

func TestSymlinkedRepositoryRootIsRejected(t *testing.T) {
	realRoot := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": "package x\n",
	})
	parent := t.TempDir()
	link := filepath.Join(parent, "repo-link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	_, err := Scan(t.Context(), ScanOptions{Root: link, Policy: policy})
	if err == nil || !strings.Contains(err.Error(), "repository root") || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error=%v, want symlinked repository-root rejection", err)
	}
}

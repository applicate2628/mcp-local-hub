package archguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredSourceRootSymlinkIsRejected(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"target/x.go": "package target\n",
	})
	link := filepath.Join(root, "linked")
	if err := os.Symlink(filepath.Join(root, "target"), link); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"linked"}
	_, err := Scan(t.Context(), ScanOptions{Root: root, Policy: policy})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error=%v, want symlinked source-root rejection", err)
	}
}

func TestLeadingBlockCommentBeforeBuildConstraintRemainsTestOnly(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/test_support.go": `/* Copyright example */
//go:build test_state_path_env

package x

var mutable = 1
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.TestOnlyBuildTags = []string{"test_state_path_env"}
	got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal)
	if len(got) != 0 {
		t.Fatalf("got=%#v, valid build constraint after a block license must remain test-only", got)
	}
}

func TestWrappedConstructorCalleesAreDetected(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x
import ctor "example.com/ctor/v2"
func Paren() { _ = (ctor.New)() }
func Generic() { _ = ctor.New[int]() }
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.APIConstructors = []SymbolRule{{ImportPath: "example.com/ctor/v2", Symbol: "New"}}
	got := violationsOfKind(mustScan(t, root, policy), KindAPIConstruction)
	if len(got) != 2 {
		t.Fatalf("got=%#v, want parenthesized and generic constructor findings", got)
	}
	symbols := map[string]bool{}
	for _, violation := range got {
		symbols[violation.Location.Symbol] = true
	}
	if !symbols["Paren"] || !symbols["Generic"] {
		t.Fatalf("symbols=%#v, want Paren and Generic", symbols)
	}
}

func TestRootPackageImportRuleMatchesDotSelector(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"root.go": `package root
import _ "mcp-local-hub/internal/gui"
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"."}
	policy.ImportRules = []ImportRule{{From: []string{"."}, Deny: []string{"internal/gui/**"}}}
	got := violationsOfKind(mustScan(t, root, policy), KindImport)
	if len(got) != 1 || got[0].Location.Path != "root.go" {
		t.Fatalf("got=%#v, want denied root-package import", got)
	}
}

func TestGenericPackageNamesAreTrimmedAndValidated(t *testing.T) {
	replaceNames := func(value string) string {
		return strings.Replace(
			validPolicyJSON,
			`"generic_package_names": ["utils", "common"]`,
			`"generic_package_names": `+value,
			1,
		)
	}
	policy, err := LoadPolicy(writeTempFile(t, replaceNames(`[" common "]`)))
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.GenericPackageNames) != 1 || policy.GenericPackageNames[0] != "common" {
		t.Fatalf("generic_package_names=%#v, want canonical common", policy.GenericPackageNames)
	}
	if _, err := LoadPolicy(writeTempFile(t, replaceNames(`["common", " common "]`))); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate error=%v", err)
	}
	if _, err := LoadPolicy(writeTempFile(t, replaceNames(`["bad-name"]`))); err == nil || !strings.Contains(err.Error(), "identifier") {
		t.Fatalf("invalid-name error=%v", err)
	}
}

func TestReportRootUsesFilesystemPathSemantics(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": "package x\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	supplied := root + string(os.PathSeparator)
	report, err := Scan(t.Context(), ScanOptions{Root: supplied, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(supplied)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.ToSlash(filepath.Clean(absolute))
	if report.Root != want || report.Root == "" {
		t.Fatalf("root=%q, want %q", report.Root, want)
	}
}

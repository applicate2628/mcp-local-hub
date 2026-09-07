package archguard

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestProductionPackageNameEndingTestIsIndexed(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/dep/dep.go": `package dep_test

func New() {}
`,
		"internal/use/use.go": `package use

import "mcp-local-hub/internal/dep"

func Build() { dep_test.New() }
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.ProductionConstructors = []SymbolRule{{
		ImportPath:   "mcp-local-hub/internal/dep",
		Symbol:       "New",
		AllowedGlobs: []string{"internal/app/**"},
	}}
	got := violationsOfKind(mustScan(t, root, policy), KindProductionConstructor)
	if len(got) != 1 || got[0].Location.Symbol != "Build" || !strings.Contains(got[0].Message, "called outside") {
		t.Fatalf("got=%#v, want the resolved dep_test.New call", got)
	}
}

func TestScanRejectsEmptyProgrammaticSourceRoots(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{"internal/x/x.go": "package x\n"})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = nil
	_, err := Scan(context.Background(), ScanOptions{Root: root, Policy: policy})
	if err == nil || !strings.Contains(err.Error(), "source_roots") {
		t.Fatalf("error=%v, want empty source_roots rejection", err)
	}
}

func TestStringEvaluationPreservesASTSourceSpans(t *testing.T) {
	huge := strings.Repeat("x", 8192)
	source := `package x

import api "mcp-local-hub/internal/api"

const huge = ` + strconv.Quote(huge) + `
const alias = huge

func First() { api.NewAPI() }
func Second() { api.NewAPI() }
`
	root := newFixtureRepo(t, map[string]string{"internal/x/x.go": source})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.APIConstructors = []SymbolRule{{
		ImportPath:   "mcp-local-hub/internal/api",
		Symbol:       "NewAPI",
		AllowedGlobs: []string{"internal/app/**"},
	}}
	got := violationsOfKind(mustScan(t, root, policy), KindAPIConstruction)
	if len(got) != 2 {
		t.Fatalf("got=%#v, want one finding per function", got)
	}
	symbols := map[string]bool{}
	for _, violation := range got {
		symbols[violation.Location.Symbol] = true
	}
	if !symbols["First"] || !symbols["Second"] {
		t.Fatalf("symbols=%#v, expanded constants must not alter AST spans", symbols)
	}
}

func TestEmbeddedDocumentResolvesCompatibleBuildVariant(t *testing.T) {
	linuxHeading := "# Linux\n" + strings.Repeat("a", 60)
	windowsHeading := "# Windows\n" + strings.Repeat("w", 60)
	tail := "\n```\n" + strings.Repeat("b", 60)
	root := newFixtureRepo(t, map[string]string{
		"internal/x/heading_linux.go":   "package x\nconst heading = " + strconv.Quote(linuxHeading) + "\n",
		"internal/x/heading_windows.go": "package x\nconst heading = " + strconv.Quote(windowsHeading) + "\n",
		"internal/x/document_linux.go":  "package x\nconst tail = " + strconv.Quote(tail) + "\nconst document = heading + tail\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Location.Symbol != "document" || got[0].Metric != len(linuxHeading)+len(tail) {
		t.Fatalf("got=%#v, want the Linux-compatible assembled document", got)
	}
}

func TestRepeatedConstructorCallsIncreaseRatchetedMetric(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

import api "mcp-local-hub/internal/api"

func Build() {
	api.NewAPI()
	api.NewAPI()
}
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.APIConstructors = []SymbolRule{{
		ImportPath:   "mcp-local-hub/internal/api",
		Symbol:       "NewAPI",
		AllowedGlobs: []string{"internal/app/**"},
	}}
	report := mustScan(t, root, policy)
	got := violationsOfKind(report, KindAPIConstruction)
	if len(got) != 1 || got[0].Metric != 2 {
		t.Fatalf("got=%#v, want one count-ratcheted finding with metric 2", got)
	}
	baselineViolation := got[0]
	baselineViolation.Metric = 1
	baseline := Baseline{
		SchemaVersion: 1,
		GeneratedFrom: "base",
		Entries: []BaselineEntry{{
			Violation:   baselineViolation,
			MaxMetric:   1,
			Owner:       "architecture",
			WorkPackage: "WP-11A",
			RemoveBy:    "2027-01-01",
			Reason:      "legacy",
		}},
	}
	verification := Verify(report, baseline, Workers{SchemaVersion: 1}, VerifyOptions{Now: mustDate(t, "2026-08-25")})
	if len(verification.Grown) != 1 {
		t.Fatalf("verification=%#v, second constructor call must grow the baseline metric", verification)
	}
}

func TestUnicodeLiteralGlobMatchesImportRule(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/über/x.go": `package uber

import _ "mcp-local-hub/internal/gui"
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.ImportRules = []ImportRule{{
		From: []string{"internal/über/**"},
		Deny: []string{"internal/gui/**"},
	}}
	got := violationsOfKind(mustScan(t, root, policy), KindImport)
	if len(got) != 1 {
		t.Fatalf("got=%#v, Unicode literal glob must match the package path", got)
	}
}

func TestPolicyRejectsInvalidConstructorImportPaths(t *testing.T) {
	for _, importPath := range []string{
		"example.com/api/",
		"example.com/new api",
		"/absolute",
		"example.com//api",
		"CON/pkg",
		"example.com/foo~1",
	} {
		t.Run(importPath, func(t *testing.T) {
			body := strings.Replace(
				validPolicyJSON,
				`"import_path": "mcp-local-hub/internal/api"`,
				`"import_path": `+strconv.Quote(importPath),
				1,
			)
			_, err := LoadPolicy(writeTempFile(t, body))
			if err == nil || !strings.Contains(err.Error(), "import_path") {
				t.Fatalf("path=%q error=%v, want invalid Go import-path rejection", importPath, err)
			}
		})
	}
}

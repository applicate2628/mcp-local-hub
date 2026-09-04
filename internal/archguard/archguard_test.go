package archguard

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestApplyOwnersUsesFirstMatchAndMetricCeiling(t *testing.T) {
	v := sampleViolation("x", 42)
	owners := Owners{SchemaVersion: 1, Rules: []OwnerRule{
		{Globs: []string{"internal/**"}, Owner: "first", WorkPackage: "WP-1", RemoveBy: "2027-01-01", Reason: "first"},
		{Globs: []string{"**"}, Owner: "second", WorkPackage: "WP-2", RemoveBy: "2027-01-01", Reason: "second"},
	}}
	b, err := ApplyOwners(reportOf(v), owners)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Entries) != 1 || b.Entries[0].Owner != "first" || b.Entries[0].MaxMetric != 42 {
		t.Fatalf("baseline=%#v", b)
	}
}

func TestApplyOwnersRejectsUnmatched(t *testing.T) {
	v := sampleViolation("x", 0)
	_, err := ApplyOwners(reportOf(v), Owners{SchemaVersion: 1})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadPolicyAcceptsVersionOne(t *testing.T) {
	got, err := LoadPolicy(writeTempFile(t, validPolicyJSON))
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 1 {
		t.Fatalf("schema_version=%d, want 1", got.SchemaVersion)
	}
	if got.Module != "mcp-local-hub" {
		t.Fatalf("module=%q", got.Module)
	}
	if len(got.compiledAllowedGlobals) == 0 {
		t.Fatal("allowed global patterns were not compiled")
	}
}

func TestLoadPolicyRejectsMissingBudgets(t *testing.T) {
	_, err := LoadPolicy(writeTempFile(t, `{"schema_version":1,"module":"mcp-local-hub","source_roots":["internal"]}`))
	if err == nil || !strings.Contains(err.Error(), "file_budgets") {
		t.Fatalf("error=%v, want file_budgets validation", err)
	}
}

func TestLoadPolicyRejectsUnknownFields(t *testing.T) {
	path := writeTempFile(t, `{"schema_version":1,"module":"mcp-local-hub","source_roots":["internal"],"unknown":true,"embedded_document_min_bytes":1,"file_budgets":{"production_advisory_lines":1,"production_hard_lines":2,"test_review_lines":1}}`)
	_, err := LoadPolicy(path)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadPolicyRejectsEmptyConstructorSymbol(t *testing.T) {
	path := writeTempFile(t, `{"schema_version":1,"module":"mcp-local-hub","source_roots":["internal"],"api_constructors":[{"import_path":"mcp-local-hub/internal/api","symbol":"","allowed_globs":[]}],"embedded_document_min_bytes":1,"file_budgets":{"production_advisory_lines":1,"production_hard_lines":2,"test_review_lines":1}}`)
	_, err := LoadPolicy(path)
	if err == nil || !strings.Contains(err.Error(), "api_constructors") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadOwnersRejectsIncompleteRule(t *testing.T) {
	path := writeTempFile(t, `{"schema_version":1,"rules":[{"globs":["internal/**"],"owner":"","work_package":"WP-11","remove_by":"2027-01-01","reason":"legacy"}]}`)
	_, err := LoadOwners(path)
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("error=%v", err)
	}
}

func TestRenderJSONIsDeterministic(t *testing.T) {
	r := reportOf(sampleViolation("x", 2))
	a, err := RenderJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("non-deterministic")
	}
}

func TestRenderMarkdownLinksSummaryAndEscapesTables(t *testing.T) {
	v := sampleViolation("x", 0)
	v.Message = "a|b"
	r := reportOf(v)
	got := RenderMarkdown(r)
	if !strings.Contains(got, "# Architecture Report") || !strings.Contains(got, "a\\|b") || !strings.Contains(got, "## Violations") {
		t.Fatalf("got=%s", got)
	}
}

func TestRuleAllowedGlobalRequiresPolicyRegex(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x
import "errors"
var ErrX = errors.New("x")
var mutable = 1
`,
	})
	p := mustLoadPolicyForTest(t)
	p.SourceRoots = []string{"internal"}
	report := mustScan(t, root, p)
	globals := violationsOfKind(report, KindMutableGlobal)
	if len(globals) != 1 || globals[0].Location.Symbol != "mutable" {
		t.Fatalf("globals=%#v", globals)
	}
	p.AllowedGlobalNamePatterns = nil
	if err := p.compilePatterns(); err != nil {
		t.Fatal(err)
	}
	report = mustScan(t, root, p)
	globals = violationsOfKind(report, KindMutableGlobal)
	if len(globals) != 2 {
		t.Fatalf("globals without policy allowance=%#v", globals)
	}
}

func TestRuleBlankInterfaceAssertionIsNotMutableGlobal(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{"internal/x/x.go": `package x
import "io"
type T struct{}
func (*T) Read([]byte)(int,error){return 0,nil}
var _ io.Reader = (*T)(nil)
`})
	p := mustLoadPolicyForTest(t)
	p.SourceRoots = []string{"internal"}
	if got := violationsOfKind(mustScan(t, root, p), KindMutableGlobal); len(got) != 0 {
		t.Fatalf("got=%#v", got)
	}
}

func TestRuleGenericPackageDetectedWithoutImports(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{"internal/common/x.go": "package common\n"})
	p := mustLoadPolicyForTest(t)
	p.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, p), KindGenericPackage)
	if len(got) != 1 || got[0].Location.Path != "internal/common" {
		t.Fatalf("got=%#v", got)
	}
}

func TestRuleAPIConstructorResolvesAliasAndIgnoresOtherPackage(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x
import a "mcp-local-hub/internal/api"
import other "example.com/other"
func F(){ _ = a.NewAPI(); _ = other.NewAPI() }
`,
	})
	p := mustLoadPolicyForTest(t)
	p.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, p), KindAPIConstruction)
	if len(got) != 1 || !strings.Contains(got[0].Evidence, "mcp-local-hub/internal/api.NewAPI") {
		t.Fatalf("got=%#v", got)
	}
}

func TestRuleTestHookIgnoredInTestFile(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go":      "package x\nfunc SetClockForTest(){}\n",
		"internal/x/x_test.go": "package x\nfunc RestoreClockForTest(){}\n",
	})
	p := mustLoadPolicyForTest(t)
	p.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, p), KindProductionTestHook)
	if len(got) != 1 || got[0].Location.Symbol != "SetClockForTest" {
		t.Fatalf("got=%#v", got)
	}
}

func TestRuleGeneratedFileExcludedFromGlobalsCommentsAndBudget(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/generated.go": "// Code generated by test. DO NOT EDIT.\npackage x\n// PR #99\nvar mutable = 1\n" + strings.Repeat("\n", 2000),
	})
	p := mustLoadPolicyForTest(t)
	p.SourceRoots = []string{"internal"}
	p.FileBudgets = FileBudgets{ProductionAdvisoryLines: 2, ProductionHardLines: 3, TestReviewLines: 3}
	report := mustScan(t, root, p)
	for _, kind := range []ViolationKind{KindMutableGlobal, KindHistoryComment, KindFileBudget} {
		if got := violationsOfKind(report, kind); len(got) != 0 {
			t.Fatalf("kind=%s got=%#v", kind, got)
		}
	}
}

func TestRuleGoStatementsCaptureEnclosingFunctionAndCall(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{"internal/x/x.go": `package x
type T struct{}
func (T) Method(){}
func F(t T){ go func(){}(); go t.Method() }
`})
	p := mustLoadPolicyForTest(t)
	p.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, p), KindWorker)
	if len(got) != 2 {
		t.Fatalf("got=%#v", got)
	}
	for _, v := range got {
		if v.Location.Symbol != "F" {
			t.Fatalf("symbol=%q", v.Location.Symbol)
		}
	}
	if !(strings.Contains(got[0].Evidence, "func") || strings.Contains(got[1].Evidence, "func")) {
		t.Fatalf("missing func literal evidence: %#v", got)
	}
	if !(strings.Contains(got[0].Evidence, "t.Method") || strings.Contains(got[1].Evidence, "t.Method")) {
		t.Fatalf("missing method evidence: %#v", got)
	}
}

func TestRuleHistoryCommentAllowedOnlyByGlob(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": "package x\n// fixed in PR #42\nfunc F(){}\n",
		"docs/adr/x.go":   "package adr\n// fixed in PR #43\nfunc F(){}\n",
	})
	p := mustLoadPolicyForTest(t)
	p.SourceRoots = []string{"internal", "docs"}
	got := violationsOfKind(mustScan(t, root, p), KindHistoryComment)
	if len(got) != 1 || got[0].Location.Path != "internal/x/x.go" {
		t.Fatalf("got=%#v", got)
	}
}

func TestRuleEmbeddedDocumentThreshold(t *testing.T) {
	prefix := "# H\n|a|b|\n"
	small := prefix + strings.Repeat("x", 4095-len(prefix))
	large := small + "x"
	root := newFixtureRepo(t, map[string]string{
		"internal/x/small.go": "package x\nconst small = `" + small + "`\n",
		"internal/x/large.go": "package x\nconst large = `" + large + "`\n",
	})
	p := mustLoadPolicyForTest(t)
	p.SourceRoots = []string{"internal"}
	p.EmbeddedDocumentMinBytes = 4096
	got := violationsOfKind(mustScan(t, root, p), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Location.Symbol != "large" || got[0].Metric != 4096 {
		t.Fatalf("got=%#v", got)
	}
}

func TestRuleFileBudgetUsesStableClassEvidence(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{"internal/x/x.go": "package x\n\n\n\n\n"})
	p := mustLoadPolicyForTest(t)
	p.SourceRoots = []string{"internal"}
	p.FileBudgets = FileBudgets{ProductionAdvisoryLines: 2, ProductionHardLines: 4, TestReviewLines: 4}
	got := violationsOfKind(mustScan(t, root, p), KindFileBudget)
	if len(got) != 1 || got[0].Evidence != "production_hard_lines" || got[0].Metric != 5 {
		t.Fatalf("got=%#v", got)
	}
}

func TestScanDetectsEveryArchitectureCategory(t *testing.T) {
	workflow := "# Workflow\n\n| A | B |\n|---|---|\n| x | y |\n\n" + strings.Repeat("detail line\n", 400)
	root := newFixtureRepo(t, map[string]string{
		"internal/cli/bad.go": `package cli
import (
    "mcp-local-hub/internal/api"
    "mcp-local-hub/internal/gui"
)
var mutableHook = func() {}
func Run() { _ = api.NewAPI(); go mutableHook(); _ = gui.Config{} }
`,
		"internal/app/good.go": `package app
import "mcp-local-hub/internal/api"
func Build() { _ = api.NewAPI() }
`,
		"internal/gui/hooks.go":     "package gui\nfunc SetClockForTest(func()) {}\n",
		"internal/common/common.go": "package common\n",
		"internal/docs/workflow.go": "package docs\nconst workflow = `" + workflow + "`\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	report, err := Scan(context.Background(), ScanOptions{Root: root, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(report.Violations)
	for _, want := range []ViolationKind{KindImport, KindMutableGlobal, KindAPIConstruction, KindProductionTestHook, KindEmbeddedDocument, KindWorker, KindGenericPackage} {
		if !got[want] {
			t.Errorf("missing violation kind %s", want)
		}
	}
}

func TestFingerprintIgnoresLineMovement(t *testing.T) {
	a := Violation{Kind: KindMutableGlobal, Location: Location{Path: "x.go", Symbol: "hook", Line: 10}, Evidence: "var hook"}
	b := a
	b.Location.Line = 200
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatal("line movement changed fingerprint")
	}
}

func TestScanIsDeterministic(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{"internal/x/x.go": "package x\nvar mutable = 1\n"})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	opts := ScanOptions{Root: root, Policy: policy}
	a, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("reports differ\na=%#v\nb=%#v", a, b)
	}
}

func newFixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module mcp-local-hub\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mustScan(t *testing.T, root string, p Policy) Report {
	t.Helper()
	r, err := Scan(context.Background(), ScanOptions{Root: root, Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func violationsOfKind(r Report, k ViolationKind) []Violation {
	var out []Violation
	for _, v := range r.Violations {
		if v.Kind == k {
			out = append(out, v)
		}
	}
	return out
}

const validPolicyJSON = `{
  "schema_version": 1,
  "module": "mcp-local-hub",
  "source_roots": ["."],
  "exclude_globs": ["**/vendor/**"],
  "import_rules": [
    {"from": ["internal/cli"], "deny": ["mcp-local-hub/internal/gui"]}
  ],
  "api_constructors": [
    {"import_path": "mcp-local-hub/internal/api", "symbol": "NewAPI", "allowed_globs": ["internal/app/**", "**/*_test.go"]}
  ],
  "production_constructors": [],
  "allowed_global_name_patterns": ["^Err[A-Z]"],
  "test_hook_name_patterns": ["^(Set|Restore).*ForTest$"],
  "history_comment_patterns": ["PR #[0-9]+", "bot r[0-9]+", "round [0-9]+"],
  "history_allowed_globs": ["docs/adr/**", "work-items/archive/**"],
  "embedded_document_min_bytes": 4096,
  "file_budgets": {
    "production_advisory_lines": 1000,
    "production_hard_lines": 1500,
    "test_review_lines": 2000
  },
  "generic_package_names": ["utils", "common"]
}`

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustLoadPolicyForTest(t *testing.T) Policy {
	t.Helper()
	p, err := LoadPolicy(writeTempFile(t, validPolicyJSON))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func kinds(vs []Violation) map[ViolationKind]bool {
	out := map[ViolationKind]bool{}
	for _, v := range vs {
		out[v.Kind] = true
	}
	return out
}

func TestVerifyRejectsNewViolation(t *testing.T) {
	v := sampleViolation("a", 0)
	got := Verify(reportOf(v), Baseline{SchemaVersion: 1}, Workers{SchemaVersion: 1}, VerifyOptions{Now: mustDate(t, "2026-08-24")})
	if len(got.New) != 1 || got.OK() {
		t.Fatalf("got=%#v", got)
	}
}

func TestVerifyRejectsMetricGrowth(t *testing.T) {
	v := sampleViolation("a", 1601)
	b := baselineOf(v, "2027-01-01")
	b.Entries[0].MaxMetric = 1500
	got := Verify(reportOf(v), b, Workers{SchemaVersion: 1}, VerifyOptions{Now: mustDate(t, "2026-08-24")})
	if len(got.Grown) != 1 {
		t.Fatalf("got=%#v", got)
	}
}

func TestVerifyRejectsExpiredEntry(t *testing.T) {
	v := sampleViolation("a", 0)
	got := Verify(reportOf(v), baselineOf(v, "2026-08-23"), Workers{SchemaVersion: 1}, VerifyOptions{Now: mustDate(t, "2026-08-24")})
	if len(got.Expired) != 1 {
		t.Fatalf("got=%#v", got)
	}
}

func TestVerifyRejectsStaleBaselineEntry(t *testing.T) {
	v := sampleViolation("a", 0)
	got := Verify(Report{SchemaVersion: 1}, baselineOf(v, "2027-01-01"), Workers{SchemaVersion: 1}, VerifyOptions{Now: mustDate(t, "2026-08-24")})
	if len(got.Stale) != 1 {
		t.Fatalf("got=%#v", got)
	}
}

func TestVerifyRejectsUnownedEntry(t *testing.T) {
	v := sampleViolation("a", 0)
	b := baselineOf(v, "2027-01-01")
	b.Entries[0].Owner = ""
	got := Verify(reportOf(v), b, Workers{SchemaVersion: 1}, VerifyOptions{Now: mustDate(t, "2026-08-24")})
	if len(got.Unowned) != 1 {
		t.Fatalf("got=%#v", got)
	}
}

func TestVerifyAcceptsExactBaseline(t *testing.T) {
	v := sampleViolation("a", 2)
	b := baselineOf(v, "2027-01-01")
	b.Entries[0].MaxMetric = 2
	got := Verify(reportOf(v), b, Workers{SchemaVersion: 1}, VerifyOptions{Now: mustDate(t, "2026-08-24")})
	if !got.OK() {
		t.Fatalf("got=%#v", got)
	}
}

func TestVerifyRequiresWorkerRegistry(t *testing.T) {
	v := sampleViolation("worker", 0)
	v.Kind = KindWorker
	v.Fingerprint = Fingerprint(v)
	got := Verify(reportOf(v), baselineOf(v, "2027-01-01"), Workers{SchemaVersion: 1}, VerifyOptions{Now: mustDate(t, "2026-08-24")})
	if len(got.Workers) != 1 {
		t.Fatalf("got=%#v", got)
	}
}

func sampleViolation(sym string, metric int) Violation {
	v := Violation{Kind: KindMutableGlobal, Location: Location{Path: "internal/x.go", Symbol: sym}, Evidence: "var " + sym, Metric: metric, Message: "x"}
	v.Fingerprint = Fingerprint(v)
	return v
}

func reportOf(v Violation) Report {
	return Report{SchemaVersion: 1, Module: "mcp-local-hub", Violations: []Violation{v}, Summary: map[ViolationKind]int{v.Kind: 1}}
}

func baselineOf(v Violation, date string) Baseline {
	return Baseline{SchemaVersion: 1, Entries: []BaselineEntry{{Violation: v, Owner: "arch", WorkPackage: "WP-11", RemoveBy: date, Reason: "legacy"}}}
}

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

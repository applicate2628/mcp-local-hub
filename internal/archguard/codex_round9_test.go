package archguard

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestUnresolvedExternalConstructorCallsAreCounted(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

import "example.com/ctor"

func Build() {
	ctor.New()
	ctor.New()
}
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.ProductionConstructors = []SymbolRule{{
		ImportPath:   "example.com/ctor",
		Symbol:       "New",
		AllowedGlobs: []string{"internal/app/**"},
	}}
	got := violationsOfKind(mustScan(t, root, policy), KindProductionConstructor)
	var callFinding *Violation
	for i := range got {
		if got[i].Location.Symbol == "Build" {
			callFinding = &got[i]
			break
		}
	}
	if callFinding == nil || callFinding.Metric != 2 {
		t.Fatalf("got=%#v, want two counted ctor.New calls behind the unresolved import", got)
	}
}

func TestExplicitGoReleaseTagCanBeEnabledThroughTags(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/dep/new.go": "//go:build go1.26\n\npackage alpha\n",
		"internal/dep/old.go": "//go:build !go1.25\n\npackage beta\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	if _, err := Scan(context.Background(), ScanOptions{Root: root, Policy: policy}); err == nil {
		t.Fatal("an explicit go1.26 term can be supplied through -tags on a pre-go1.25 release and must overlap !go1.25")
	}
}

func TestGoBuildDirectiveWithoutBlankLineIsApplied(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/shim.go": "//go:build test_state_path_env\npackage x\nvar mutable = 1\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.TestOnlyBuildTags = []string{"test_state_path_env"}
	if got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal); len(got) != 0 {
		t.Fatalf("got=%#v, go:build file without a blank separator must remain test-only", got)
	}
}

func TestEmbeddedDocumentResolvesDefinedStringConversion(t *testing.T) {
	heading := "# Heading\n" + strings.Repeat("a", 60)
	body := "\n```\n" + strings.Repeat("b", 60)
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

type Markdown string

const heading = ` + strconv.Quote(heading) + `
const body = ` + strconv.Quote(body) + `
const document = Markdown(heading + body)
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Location.Symbol != "document" || got[0].Metric != len(heading)+len(body) {
		t.Fatalf("got=%#v, want the document converted to a defined string type", got)
	}
}

func TestOwnersRejectUnknownViolationKind(t *testing.T) {
	_, err := LoadOwners(writeTempFile(t, `{
  "schema_version": 1,
  "rules": [{
    "globs": ["internal/**"],
    "kinds": ["mutable_globals"],
    "owner": "architecture",
    "work_package": "WP-11",
    "remove_by": "2027-01-01",
    "reason": "legacy"
  }]
}`))
	if err == nil || !strings.Contains(err.Error(), "kinds") {
		t.Fatalf("error=%v, want unknown owner kind rejection", err)
	}
}

func TestRenderMarkdownUsesSafeCodeSpanForRoot(t *testing.T) {
	report := Report{SchemaVersion: 1, Module: "mcp-local-hub", Root: "dir``tick\nnext", Summary: map[ViolationKind]int{}}
	got := RenderMarkdown(report)
	if !strings.Contains(got, "- Root: ```dir``tick next```") {
		t.Fatalf("rendered report does not use a safe delimiter: %q", got)
	}
	if strings.Contains(got, "tick\nnext") {
		t.Fatalf("rendered report retained a structural newline: %q", got)
	}
}

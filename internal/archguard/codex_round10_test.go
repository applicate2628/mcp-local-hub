package archguard

import (
	"context"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestExplicitArchitectureFeatureTagCanBeEnabledThroughTags(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/dep/high.go": "//go:build amd64.v2\n\npackage alpha\n",
		"internal/dep/low.go":  "//go:build !amd64.v1\n\npackage beta\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	if _, err := Scan(context.Background(), ScanOptions{Root: root, Policy: policy}); err == nil {
		t.Fatal("an explicit amd64.v2 term can be supplied through -tags while automatic amd64.v1 remains false")
	}
}

func TestUnresolvedConstructorReferencesAreCounted(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

import "example.com/client/v2"

func Build() {
	first := client.New
	second := client.New
	_, _ = first, second
}
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.ProductionConstructors = []SymbolRule{{
		ImportPath:   "example.com/client/v2",
		Symbol:       "New",
		AllowedGlobs: []string{"internal/app/**"},
	}}
	got := violationsOfKind(mustScan(t, root, policy), KindProductionConstructor)
	var referenceFinding *Violation
	for i := range got {
		if got[i].Location.Symbol == "Build" {
			referenceFinding = &got[i]
			break
		}
	}
	if referenceFinding == nil || referenceFinding.Metric != 2 {
		t.Fatalf("got=%#v, want two counted client.New references", got)
	}
}

func TestNestedProductionHookFieldsAreDetected(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

type Config struct {
	Nested struct {
		SetClockForTest func()
		Deeper interface {
			RestoreClockForTest()
		}
	}
}
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, policy), KindProductionTestHook)
	symbols := make([]string, 0, len(got))
	for _, violation := range got {
		symbols = append(symbols, violation.Location.Symbol)
	}
	sort.Strings(symbols)
	want := []string{"Config.Nested.Deeper.RestoreClockForTest", "Config.Nested.SetClockForTest"}
	if len(symbols) != len(want) || strings.Join(symbols, "|") != strings.Join(want, "|") {
		t.Fatalf("symbols=%v, want %v", symbols, want)
	}
}

func TestConstructorUseInsideDefiningPackageIsDetected(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/api/api.go": `package api

func NewAPI() {}

func helper() {
	NewAPI()
	factory := NewAPI
	_ = factory
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
	got := violationsOfKind(mustScan(t, root, policy), KindAPIConstruction)
	if len(got) != 1 || got[0].Location.Symbol != "helper" || got[0].Metric != 2 {
		t.Fatalf("got=%#v, want one helper finding with call+reference metric 2", got)
	}
}

func TestPolicyRejectsDuplicateConstructorRules(t *testing.T) {
	for _, test := range []struct {
		name   string
		assign func(*Policy, []SymbolRule)
	}{
		{name: "api", assign: func(policy *Policy, rules []SymbolRule) { policy.APIConstructors = rules }},
		{name: "production", assign: func(policy *Policy, rules []SymbolRule) { policy.ProductionConstructors = rules }},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := mustLoadPolicyForTest(t)
			rule := SymbolRule{ImportPath: "mcp-local-hub/internal/api", Symbol: "NewAPI", AllowedGlobs: []string{"internal/app/**"}}
			test.assign(&policy, []SymbolRule{rule, rule})
			if err := policy.validate("policy"); err == nil || !strings.Contains(err.Error(), "duplicate constructor") {
				t.Fatalf("error=%v, want duplicate constructor rejection", err)
			}
		})
	}
}

func TestUnixBackslashPathRemainsDistinct(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is a path separator on Windows")
	}
	root := newFixtureRepo(t, map[string]string{
		"internal/x/a\\b.go": "package x\nvar mutable = 1\n",
		"internal/x/a/b.go":   "package b\nvar mutable = 1\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal)
	if len(got) != 2 {
		t.Fatalf("got=%#v, want both distinct files", got)
	}
	paths := []string{got[0].Location.Path, got[1].Location.Path}
	sort.Strings(paths)
	if paths[0] == paths[1] || !strings.Contains(paths[1], `\`) && !strings.Contains(paths[0], `\`) {
		t.Fatalf("paths=%q, want one literal-backslash path and one nested path", paths)
	}
	if got[0].Fingerprint == got[1].Fingerprint {
		t.Fatalf("fingerprints collided for distinct Unix paths: %#v", got)
	}
}

func TestRepeatedHistoryCommentsAreCounted(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

func F() {
	// fixed in PR #123
	// fixed again in PR #123
}
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.HistoryCommentPatterns = []string{`PR #[0-9]+`}
	got := violationsOfKind(mustScan(t, root, policy), KindHistoryComment)
	if len(got) != 1 || got[0].Location.Symbol != "F" || got[0].Metric != 2 {
		t.Fatalf("got=%#v, want one count-ratcheted history finding with metric 2", got)
	}
}

func TestLocalEmbeddedDocumentsAreQualifiedByFunction(t *testing.T) {
	first := "# First\n```\n" + strings.Repeat("a", 120)
	second := "# Second\n```\n" + strings.Repeat("b", 180)
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

func First() {
	const document = ` + strconv.Quote(first) + `
	_ = document
}

func Second() {
	const document = ` + strconv.Quote(second) + `
	_ = document
}
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 2 {
		t.Fatalf("got=%#v, want both local documents", got)
	}
	bySymbol := map[string]int{}
	for _, violation := range got {
		bySymbol[violation.Location.Symbol] = violation.Metric
	}
	if bySymbol["First.document"] != len(first) || bySymbol["Second.document"] != len(second) {
		t.Fatalf("bySymbol=%v, want function-qualified local documents", bySymbol)
	}
}

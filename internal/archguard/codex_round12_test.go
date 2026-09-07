package archguard

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestFunctionSignatureNestedTypesAreInspectedForProductionHooks(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

type Config struct {
	Factory func(SetClockForTest struct {
		RestoreClockForTest func()
	}) struct {
		SetClockForTest func()
	}
}
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, policy), KindProductionTestHook)
	bySymbol := make(map[string]struct{}, len(got))
	for _, violation := range got {
		bySymbol[violation.Location.Symbol] = struct{}{}
	}
	want := []string{
		"Config.Factory.RestoreClockForTest",
		"Config.Factory.SetClockForTest",
	}
	if len(bySymbol) != len(want) {
		t.Fatalf("got=%#v, want only nested signature type fields", got)
	}
	for _, symbol := range want {
		if _, ok := bySymbol[symbol]; !ok {
			t.Fatalf("symbols=%v, missing %s", bySymbol, symbol)
		}
	}
}

func TestEmbeddedDocumentResolvesIotaStringConversion(t *testing.T) {
	tail := " Heading\n```\n" + strings.Repeat("x", 160)
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

const (
	_ = iota
	document = string('A' + iota) + ` + strconv.Quote(tail) + `
)
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	want := "B" + tail
	if len(got) != 1 || got[0].Location.Symbol != "document" || got[0].Metric != len(want) {
		t.Fatalf("got=%#v, want iota-expanded document of %d bytes", got, len(want))
	}
}

func TestPolicyRejectsDuplicateHistoryPatterns(t *testing.T) {
	policy := mustLoadPolicyForTest(t)
	policy.HistoryCommentPatterns = []string{`PR #[0-9]+`, ` PR #[0-9]+ `}
	if err := policy.validate("policy"); err == nil || !strings.Contains(err.Error(), "duplicate history") {
		t.Fatalf("error=%v, want duplicate history pattern rejection", err)
	}
}

func TestUnusedConfiguredTestTagsDoNotPreventTestOnlyClassification(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": "//go:build test0\n\npackage x\n\nvar mutable = 1\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.TestOnlyBuildTags = make([]string, 12)
	for i := range policy.TestOnlyBuildTags {
		policy.TestOnlyBuildTags[i] = fmt.Sprintf("test%d", i)
	}
	got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal)
	if len(got) != 0 {
		t.Fatalf("got=%#v, want file requiring test0 classified as test-only", got)
	}
}

func TestWorkerOccurrencesAreNumberedInOneTraversalOrder(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

func work() {}
func other() {}

func F() {
	go work()
	go work()
	go other()
	go work()
}
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, policy), KindWorker)
	want := map[string]struct{}{
		"go work #1":  {},
		"go work #2":  {},
		"go work #3":  {},
		"go other #1": {},
	}
	if len(got) != len(want) {
		t.Fatalf("got=%#v, want four workers", got)
	}
	for _, violation := range got {
		if violation.Location.Symbol != "F" {
			t.Fatalf("got=%#v, want all workers scoped to F", got)
		}
		if _, ok := want[violation.Evidence]; !ok {
			t.Fatalf("unexpected worker evidence %q in %#v", violation.Evidence, got)
		}
		delete(want, violation.Evidence)
	}
	if len(want) != 0 {
		t.Fatalf("missing worker evidence: %v", want)
	}
}

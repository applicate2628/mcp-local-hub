package archguard

import (
	"strconv"
	"strings"
	"testing"
)

func TestEffectiveBuildConstraintTreatsExplicitPlatformTagAsProductionAlternative(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/shim_windows.go": `//go:build test_state_path_env || linux

package x

var mutable = 1
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.TestOnlyBuildTags = []string{"test_state_path_env"}
	got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal)
	if len(got) != 1 || got[0].Location.Symbol != "mutable" {
		t.Fatalf("got=%#v, explicit linux can be enabled through -tags even with a Windows filename", got)
	}
}

func TestEffectiveBuildConstraintRetainsRealPlatformAlternative(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/shim_windows.go": `//go:build test_state_path_env || windows

package x

var mutable = 1
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.TestOnlyBuildTags = []string{"test_state_path_env"}
	got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal)
	if len(got) != 1 || got[0].Location.Symbol != "mutable" {
		t.Fatalf("got=%#v, windows remains a production build alternative", got)
	}
}

func TestGlobCharacterClassesMatchPolicyPaths(t *testing.T) {
	for _, value := range []string{"internal/a", "internal/a/x.go", "internal/b/x.go"} {
		if !matchGlob("internal/[ab]/**", value) {
			t.Errorf("class pattern did not match %q", value)
		}
	}
	for _, value := range []string{"internal/c/x.go", "internal/[ab]"} {
		if matchGlob("internal/[ab]/**", value) {
			t.Errorf("class pattern matched excluded path %q", value)
		}
	}
	if !matchGlob("internal/[a-c]/**", "internal/c/x.go") {
		t.Fatal("range class did not match")
	}
	if !matchGlob("internal/[^c]/**", "internal/a/x.go") {
		t.Fatal("negated class did not match")
	}
}

func TestEmbeddedDocumentResolvesNamedStringConstants(t *testing.T) {
	heading := "# Heading\n" + strings.Repeat("a", 60)
	body := "\n```\n" + strings.Repeat("b", 60)
	source := "package x\n" +
		"const heading = " + strconv.Quote(heading) + "\n" +
		"const body = " + strconv.Quote(body) + "\n" +
		"const document = heading + body\n"
	root := newFixtureRepo(t, map[string]string{"internal/x/document.go": source})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Location.Symbol != "document" || got[0].Metric != len(heading)+len(body) {
		t.Fatalf("got=%#v, want the assembled document finding", got)
	}
}

func TestEmbeddedDocumentResolvesLocalNamedStringConstants(t *testing.T) {
	heading := "# Local\n" + strings.Repeat("a", 60)
	body := "\n```\n" + strings.Repeat("b", 60)
	source := "package x\nfunc F() {\n" +
		"const heading = " + strconv.Quote(heading) + "\n" +
		"const body = " + strconv.Quote(body) + "\n" +
		"const document = heading + body\n" +
		"_ = document\n}\n"
	root := newFixtureRepo(t, map[string]string{"internal/x/local_document.go": source})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Location.Symbol != "F.document" || got[0].Metric != len(heading)+len(body) {
		t.Fatalf("got=%#v, want the assembled local document finding", got)
	}
}

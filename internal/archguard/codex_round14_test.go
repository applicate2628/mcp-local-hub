package archguard

import (
	"go/build/constraint"
	"strconv"
	"strings"
	"testing"
)

func TestUnknownArchitectureFeatureSpellingRemainsCustomBuildTag(t *testing.T) {
	arm64 := &constraint.TagExpr{Tag: "arm64"}
	for _, tag := range []string{
		"amd64.foo",
		"amd64.v99",
		"arm.8",
		"ppc64.power11",
		"riscv64.rva21u64",
	} {
		t.Run(tag, func(t *testing.T) {
			custom := &constraint.TagExpr{Tag: tag}
			if !buildConstraintsOverlap(custom, arm64) {
				t.Fatalf("unknown spelling %q must remain a custom -tags value and overlap arm64", tag)
			}
		})
	}

	for _, tag := range []string{
		"amd64.v2",
		"arm.7",
		"ppc64.power10",
		"riscv64.rva23u64",
	} {
		t.Run("known_"+tag, func(t *testing.T) {
			known := &constraint.TagExpr{Tag: tag}
			if buildConstraintsOverlap(known, arm64) {
				t.Fatalf("recognized architecture feature %q must remain target-dependent", tag)
			}
		})
	}
}

func TestUnknownArchitectureFeatureSpellingMayBeConfiguredAsCustomTestTag(t *testing.T) {
	policy := mustLoadPolicyForTest(t)
	policy.TestOnlyBuildTags = []string{"amd64.foo"}
	if err := policy.validate("policy"); err != nil {
		t.Fatalf("unknown GOARCH-prefixed custom tag was rejected: %v", err)
	}

	policy.TestOnlyBuildTags = []string{"amd64.v3"}
	if err := policy.validate("policy"); err == nil {
		t.Fatal("recognized architecture feature tag must remain reserved")
	}
}

func TestEmbeddedDocumentResolvesDefinedIntegerConversion(t *testing.T) {
	tail := " Heading\n```\n" + strings.Repeat("x", 160)
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

type Marker rune

const document = string(Marker('#')) + ` + strconv.Quote(tail) + `
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	wantBytes := len("#" + tail)
	if len(got) != 1 || got[0].Location.Symbol != "document" || got[0].Metric != wantBytes {
		t.Fatalf("got=%#v, want defined-integer conversion document of %d bytes", got, wantBytes)
	}
}

func TestSiblingBlockEmbeddedDocumentsHaveDistinctSymbols(t *testing.T) {
	first := "# First\n```\n" + strings.Repeat("a", 140)
	second := "# Second\n```\n" + strings.Repeat("b", 180)
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

func F(flag bool) {
	if flag {
		const document = ` + strconv.Quote(first) + `
		_ = document
	}
	if !flag {
		const document = ` + strconv.Quote(second) + `
		_ = document
	}
}
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 2 {
		t.Fatalf("got=%#v, want two sibling-block documents", got)
	}
	seen := map[string]struct{}{}
	for _, violation := range got {
		if !strings.HasPrefix(violation.Location.Symbol, "F.document") {
			t.Fatalf("unexpected symbol %q in %#v", violation.Location.Symbol, got)
		}
		if _, exists := seen[violation.Location.Symbol]; exists {
			t.Fatalf("duplicate local document symbol %q in %#v", violation.Location.Symbol, got)
		}
		seen[violation.Location.Symbol] = struct{}{}
	}
}

func TestPolicyRejectsBoringCryptoAsTestOnlyTag(t *testing.T) {
	policy := mustLoadPolicyForTest(t)
	policy.TestOnlyBuildTags = []string{"boringcrypto"}
	if err := policy.validate("policy"); err == nil || !strings.Contains(err.Error(), "boringcrypto") {
		t.Fatalf("error=%v, want boringcrypto reserved-tag rejection", err)
	}
}

func TestDescendantTestdataIsSkippedButExplicitRootIsScanned(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go":                    "package x\n",
		"internal/x/testdata/fixture.go":     "package fixture\nvar mutable = 1\n",
		"internal/x/testdata/nested/more.go": "package nested\nvar another = 1\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	for _, violation := range mustScan(t, root, policy).Violations {
		if strings.Contains(violation.Location.Path, "/testdata/") {
			t.Fatalf("descendant testdata finding leaked into scan: %#v", violation)
		}
	}

	policy.SourceRoots = []string{"internal/x/testdata"}
	got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal)
	if len(got) != 2 {
		t.Fatalf("got=%#v, explicitly configured testdata root must be scanned", got)
	}
}

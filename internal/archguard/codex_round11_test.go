package archguard

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestExplicitARM64VersionTagsCanBeEnabledThroughTags(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/v8/high.go": "//go:build arm64.v8.8\n\npackage alpha\n",
		"internal/v8/low.go":  "//go:build !arm64.v8.7\n\npackage beta\n",
		"internal/v9/high.go": "//go:build arm64.v9.1\n\npackage gamma\n",
		"internal/v9/low.go":  "//go:build !arm64.v8.6\n\npackage delta\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	if _, err := Scan(context.Background(), ScanOptions{Root: root, Policy: policy}); err == nil {
		t.Fatal("explicit ARM64 version terms can be enabled through -tags independently of automatic version ordering")
	}
}

func TestRenderMarkdownEscapesBackslashBeforePipe(t *testing.T) {
	got := markdownCell(`internal/a\|b.go`)
	want := `internal/a\\\|b.go`
	if got != want {
		t.Fatalf("markdownCell()=%q, want %q", got, want)
	}
}

func TestEmbeddedDocumentResolvesIntegerToStringConversion(t *testing.T) {
	tail := " Heading\n```\n" + strings.Repeat("x", 160)
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

const headingRune = '#'
const document = string(headingRune) + ` + strconv.Quote(tail) + `
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Location.Symbol != "document" || got[0].Metric != len("#"+tail) {
		t.Fatalf("got=%#v, want resolved integer-to-string Markdown document", got)
	}
}

func TestPackageStringEvaluationKeysAreSorted(t *testing.T) {
	groups := map[string][]int{
		"internal/z\x00z": {2},
		"internal/a\x00a": {0},
		"internal/m\x00m": {1},
	}
	got := packageStringEvaluationKeys(groups)
	want := []string{"internal/a\x00a", "internal/m\x00m", "internal/z\x00z"}
	if len(got) != len(want) {
		t.Fatalf("keys=%q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys=%q, want %q", got, want)
		}
	}
}

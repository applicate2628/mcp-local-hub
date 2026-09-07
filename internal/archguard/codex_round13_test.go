package archguard

import (
	"strconv"
	"strings"
	"testing"
)

func TestImplicitRepeatedConstEmbeddedDocumentsAreScanned(t *testing.T) {
	document := "# Overview\n```go\nexample\n```\n" + strings.Repeat("x", 160)
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

const (
	first = ` + strconv.Quote(document) + `
	second
)
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100

	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	bySymbol := make(map[string]Violation, len(got))
	for _, violation := range got {
		bySymbol[violation.Location.Symbol] = violation
	}
	for _, symbol := range []string{"first", "second"} {
		violation, ok := bySymbol[symbol]
		if !ok {
			t.Fatalf("got=%#v, missing implicitly evaluated constant %s", got, symbol)
		}
		if violation.Metric != len(document) {
			t.Fatalf("%s metric=%d, want %d", symbol, violation.Metric, len(document))
		}
	}
	if len(got) != 2 {
		t.Fatalf("got=%#v, want exactly two embedded documents", got)
	}
}

func TestGenericTypeArgumentsAreInspectedForProductionHooks(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

type Box[T any] struct{}
type Pair[A, B any] struct{}

type Config struct {
	Value Box[struct {
		SetClockForTest func()
	}]
	Other Pair[int, interface {
		RestoreClockForTest()
	}]
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
	for _, symbol := range []string{
		"Config.Value.SetClockForTest",
		"Config.Other.RestoreClockForTest",
	} {
		if _, ok := bySymbol[symbol]; !ok {
			t.Fatalf("got=%#v, missing hook nested in generic argument %s", got, symbol)
		}
	}
}

func TestEmbeddedMarkdownMarkersRecognizeAllATXHeadingLevels(t *testing.T) {
	for level := 1; level <= 6; level++ {
		document := strings.Repeat("#", level) + " Heading\n```go\nexample\n```\n"
		if got := embeddedMarkdownMarkers(document); got != 2 {
			t.Fatalf("level %d markers=%d, want 2", level, got)
		}
	}
	if got := embeddedMarkdownMarkers("####### Not an ATX heading\n```\nexample\n```\n"); got != 1 {
		t.Fatalf("seven-hash line markers=%d, want only the code-fence marker", got)
	}
}

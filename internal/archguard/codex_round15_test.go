package archguard

import (
	"context"
	"go/build/constraint"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestWorkerEvidencePreservesLiteralBackslashes(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\nfunc F(workers map[string]func()) {\n\tgo workers[\"a\\\\b\"]()\n\tgo workers[\"a/b\"]()\n}\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, policy), KindWorker)
	if len(got) != 2 {
		t.Fatalf("got=%#v, want two distinct workers", got)
	}
	if got[0].Fingerprint == got[1].Fingerprint || got[0].Evidence == got[1].Evidence {
		t.Fatalf("worker evidence collapsed literal backslash and slash: %#v", got)
	}
}

func TestEmbeddedDocumentResolvesImportedStringType(t *testing.T) {
	body := "# Heading\n```\n" + strings.Repeat("x", 160)
	root := newFixtureRepo(t, map[string]string{
		"internal/markdown/type.go": "package markdown\n\ntype Text string\n",
		"internal/x/x.go": `package x

import "mcp-local-hub/internal/markdown"

const document = markdown.Text(` + strconv.Quote(body) + `)
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Location.Symbol != "document" || got[0].Metric != len(body) {
		t.Fatalf("got=%#v, want imported string type conversion", got)
	}
}

func TestEmbeddedDocumentResolvesImportedIntegerType(t *testing.T) {
	tail := " Heading\n```\n" + strings.Repeat("x", 160)
	root := newFixtureRepo(t, map[string]string{
		"internal/marker/type.go": "package marker\n\ntype Rune rune\n",
		"internal/x/x.go": `package x

import "mcp-local-hub/internal/marker"

const document = string(marker.Rune('#')) + ` + strconv.Quote(tail) + `
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Metric != len("#"+tail) {
		t.Fatalf("got=%#v, want imported integer type conversion", got)
	}
}

func TestEmbeddedDocumentResolvesImportedStringConstant(t *testing.T) {
	tail := "```\n" + strings.Repeat("x", 160)
	root := newFixtureRepo(t, map[string]string{
		"internal/docs/constants.go": "package docs\n\nconst Heading = \"# Heading\\n\"\n",
		"internal/x/x.go": `package x

import "mcp-local-hub/internal/docs"

const document = docs.Heading + ` + strconv.Quote(tail) + `
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Metric != len("# Heading\n"+tail) {
		t.Fatalf("got=%#v, want imported string constant", got)
	}
}

func TestEmbeddedMarkdownRecognizesTildeFenceAndSetextHeading(t *testing.T) {
	if embeddedMarkdownMarkers("# Heading\n~~~go\ncode\n~~~\n") < 2 {
		t.Fatal("tilde-fenced code block was not recognized")
	}
	if embeddedMarkdownMarkers("Heading\n=======\n```\ncode\n```\n") < 2 {
		t.Fatal("setext heading was not recognized")
	}
	if embeddedMarkdownMarkers("# Heading\ntext ~~~ inline\n") != 1 {
		t.Fatal("inline tildes must not be treated as a fenced block")
	}
}

func TestGo126TargetMatrixMatchesToolchain(t *testing.T) {
	want := []goTarget{
		{"aix", "ppc64"},
		{"android", "386"}, {"android", "amd64"}, {"android", "arm"}, {"android", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"dragonfly", "amd64"},
		{"freebsd", "386"}, {"freebsd", "amd64"}, {"freebsd", "arm"}, {"freebsd", "arm64"}, {"freebsd", "riscv64"},
		{"illumos", "amd64"},
		{"ios", "amd64"}, {"ios", "arm64"},
		{"js", "wasm"},
		{"linux", "386"}, {"linux", "amd64"}, {"linux", "arm"}, {"linux", "arm64"}, {"linux", "loong64"}, {"linux", "mips"}, {"linux", "mips64"}, {"linux", "mips64le"}, {"linux", "mipsle"}, {"linux", "ppc64"}, {"linux", "ppc64le"}, {"linux", "riscv64"}, {"linux", "s390x"}, {"linux", "sparc64"},
		{"netbsd", "386"}, {"netbsd", "amd64"}, {"netbsd", "arm"}, {"netbsd", "arm64"},
		{"openbsd", "386"}, {"openbsd", "amd64"}, {"openbsd", "arm"}, {"openbsd", "arm64"}, {"openbsd", "mips64"}, {"openbsd", "ppc64"}, {"openbsd", "riscv64"},
		{"plan9", "386"}, {"plan9", "amd64"}, {"plan9", "arm"},
		{"solaris", "amd64"},
		{"wasip1", "wasm"},
		{"windows", "386"}, {"windows", "amd64"}, {"windows", "arm64"},
	}
	if got := knownTargetValues(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Go 1.26 target matrix drifted:\ngot=%#v\nwant=%#v", got, want)
	}
}

func TestGo126KnownTagsExcludeRemovedPortsAndFutureFeatures(t *testing.T) {
	for _, tag := range []string{"hurd", "nacl", "zos", "amd64p32", "armbe", "arm64be", "mips64p32", "ppc", "riscv", "s390", "sparc"} {
		if isKnownGOOS(tag) || isKnownGOARCH(tag) {
			t.Fatalf("removed Go target %q must remain a custom build tag", tag)
		}
	}
	arm64 := &constraint.TagExpr{Tag: "arm64"}
	if buildConstraintsOverlap(&constraint.TagExpr{Tag: "amd64.v4"}, arm64) {
		t.Fatal("amd64.v4 is a defined target-specific Go 1.26 feature")
	}
	if !buildConstraintsOverlap(&constraint.TagExpr{Tag: "amd64.v5"}, arm64) {
		t.Fatal("unknown future-looking amd64.v5 must remain a custom tag")
	}
}

func TestGo126FeatureModesMatchToolchain(t *testing.T) {
	if buildConstraintsOverlap(
		&constraint.TagExpr{Tag: "386.sse2"},
		&constraint.TagExpr{Tag: "386.softfloat"},
	) {
		t.Fatal("GO386 modes are mutually exclusive")
	}
	wasmWithoutSatconv := &constraint.AndExpr{
		X: &constraint.TagExpr{Tag: "wasm"},
		Y: &constraint.NotExpr{X: &constraint.TagExpr{Tag: "wasm.satconv"}},
	}
	if buildConstraintsOverlap(wasmWithoutSatconv, nil) {
		t.Fatal("Go 1.26 always enables wasm.satconv")
	}
}

func TestKnownBrokenGoTargetsParticipateInOverlap(t *testing.T) {
	cases := []constraint.Expr{
		&constraint.AndExpr{X: &constraint.TagExpr{Tag: "linux"}, Y: &constraint.TagExpr{Tag: "sparc64"}},
		&constraint.AndExpr{X: &constraint.TagExpr{Tag: "openbsd"}, Y: &constraint.TagExpr{Tag: "mips64"}},
	}
	for _, target := range cases {
		if !buildConstraintsOverlap(target, nil) {
			t.Fatalf("valid Go target was omitted: %v", target)
		}
	}
}

func TestExplicitTestdataRootWinsOverBroaderSourceRoot(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go":                "package x\n",
		"internal/x/testdata/fixture.go": "package fixture\nvar mutable = 1\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal", "internal/x/testdata"}
	got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal)
	if len(got) != 1 || got[0].Location.Path != "internal/x/testdata/fixture.go" {
		t.Fatalf("got=%#v, explicitly configured testdata must be scanned even with a broad root", got)
	}
}

func TestDescendantVendorIsSkippedButExplicitRootIsScanned(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go":                  "package x\n",
		"internal/x/vendor/lib/fixture.go": "package lib\nvar mutable = 1\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	if got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal); len(got) != 0 {
		t.Fatalf("descendant vendor must be ignored: %#v", got)
	}
	policy.SourceRoots = []string{"internal/x/vendor"}
	if got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal); len(got) != 1 {
		t.Fatalf("explicit vendor root must be scanned: %#v", got)
	}
}

func TestNormalizeEvidencePreservesSourceBackslashes(t *testing.T) {
	got := normalizeEvidence(" workers[\"a\\\\b\"]() \n")
	if got != `workers["a\\b"]()` {
		t.Fatalf("got=%q, source backslash was rewritten", got)
	}
}

func TestImportedTypeResolutionRejectsShadowedAlias(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/markdown/type.go": "package markdown\n\ntype Text string\n",
		"internal/x/x.go": `package x

import "mcp-local-hub/internal/markdown"

type factory struct{}
func (factory) Text(string) string { return "" }

func F(markdown factory) {
	_ = markdown.Text("# Heading\n```\n" + "x")
}
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 1
	if got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument); len(got) != 0 {
		t.Fatalf("shadowed import alias was treated as a package conversion: %#v", got)
	}
}

func TestTargetSpecificPackageConflictCoversBrokenPorts(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/dep/a_linux_sparc64.go": "package alpha\n",
		"internal/dep/b_linux.go":         "package beta\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	if _, err := Scan(context.Background(), ScanOptions{Root: root, Policy: policy}); err == nil {
		t.Fatal("linux/sparc64 package-name conflict was not detected")
	}
}

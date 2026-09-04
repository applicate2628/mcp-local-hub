package archguard

import (
	"go/build/constraint"
	"strconv"
	"strings"
	"testing"
)

func TestBoringCryptoLegacyAliasSharesToolTagState(t *testing.T) {
	expr := &constraint.AndExpr{
		X: &constraint.TagExpr{Tag: "boringcrypto"},
		Y: &constraint.NotExpr{X: &constraint.TagExpr{Tag: "goexperiment.boringcrypto"}},
	}
	if buildConstraintsOverlap(expr, nil) {
		t.Fatal("boringcrypto and goexperiment.boringcrypto must be equivalent")
	}
}

func TestCgoTagHonorsTargetSupport(t *testing.T) {
	unsupported := &constraint.AndExpr{
		X: &constraint.AndExpr{X: &constraint.TagExpr{Tag: "js"}, Y: &constraint.TagExpr{Tag: "wasm"}},
		Y: &constraint.TagExpr{Tag: "cgo"},
	}
	if buildConstraintsOverlap(unsupported, nil) {
		t.Fatal("js/wasm cannot satisfy cgo")
	}
	supported := &constraint.AndExpr{
		X: &constraint.AndExpr{X: &constraint.TagExpr{Tag: "linux"}, Y: &constraint.TagExpr{Tag: "amd64"}},
		Y: &constraint.TagExpr{Tag: "cgo"},
	}
	if !buildConstraintsOverlap(supported, nil) {
		t.Fatal("linux/amd64 can satisfy cgo")
	}
}

func TestBuildCompatibleLocalConstantShadowsDotImport(t *testing.T) {
	local := "# Local\n"
	imported := "plain imported text\n"
	tail := "```\n" + strings.Repeat("x", 160)
	root := newFixtureRepo(t, map[string]string{
		"internal/docs/constants.go": "package docs\n\nconst Heading = " + strconv.Quote(imported) + "\n",
		"internal/x/local_linux.go":  "package x\n\nconst Heading = " + strconv.Quote(local) + "\n",
		"internal/x/document_linux.go": `package x

import . "mcp-local-hub/internal/docs"

const document = Heading + ` + strconv.Quote(tail) + `
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Metric != len(local+tail) {
		t.Fatalf("got=%#v, local build-compatible constant must shadow dot import", got)
	}
}

func TestIncompatibleLocalConstantDoesNotShadowDotImport(t *testing.T) {
	imported := "# Imported\n"
	tail := "```\n" + strings.Repeat("x", 160)
	root := newFixtureRepo(t, map[string]string{
		"internal/docs/constants.go": "package docs\n\nconst Heading = " + strconv.Quote(imported) + "\n",
		"internal/x/local_windows.go": "package x\n\nconst Heading = \"not markdown\"\n",
		"internal/x/document_linux.go": `package x

import . "mcp-local-hub/internal/docs"

const document = Heading + ` + strconv.Quote(tail) + `
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Metric != len(imported+tail) {
		t.Fatalf("got=%#v, incompatible local definition must not hide dot import", got)
	}
}

package patchesapply

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR31StoredCMakeValueIsNotRecursivelyExpanded(t *testing.T) {
	dir := writePort(t, `
set(PATCH_NAME [=[${OTHER}]=])
set(OTHER fix.patch)
vcpkg_from_github(PATCHES ${PATCH_NAME})
`, `${OTHER}`)

	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if res.Status != evidence.StatusOK || res.Reason != "" {
		t.Fatalf("result = %+v, want ok/empty", res)
	}
	if got := strings.Join(appliedNames(res), "|"); got != `${OTHER}` {
		t.Fatalf("applied = %q, want literal stored value %q", got, `${OTHER}`)
	}
}

func TestR31StoredLiteralReferenceDoesNotProtectSiblingValue(t *testing.T) {
	dir := writePort(t, `
set(MIXED [=[${LITERAL}]=] ${LATER})
set(LATER actual.patch)
vcpkg_from_github(PATCHES ${MIXED})
`, `${LITERAL}`, "actual.patch")

	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonOrphanScanIncomplete {
		t.Fatalf("result = %+v, want conservative unresolved-sibling verdict", res)
	}
	if got := strings.Join(appliedNames(res), "|"); got != `${LITERAL}` {
		t.Fatalf("applied = %q, want literal bracket item", got)
	}
	if got := strings.Join(undecidableNames(res), "|"); got != "actual.patch" {
		t.Fatalf("undecidable = %q, want separately resolved non-literal sibling", got)
	}
}

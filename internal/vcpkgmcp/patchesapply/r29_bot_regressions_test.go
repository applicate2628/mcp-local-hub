package patchesapply

import (
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR29UnsupportedGetFilenameComponentInvalidatesDestination(t *testing.T) {
	dir := writePort(t, `
set(PATCH_NAME dir/fix.patch)
get_filename_component(PATCH_NAME ${PATCH_NAME} NAME)
vcpkg_from_github(PATCHES ${PATCH_NAME})
`, "fix.patch")
	result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonPatchesExprUnparsable || len(result.Applied) != 0 || len(result.Missing) != 0 {
		t.Fatalf("result=%+v, want unsupported destination fail-closed", result)
	}
}

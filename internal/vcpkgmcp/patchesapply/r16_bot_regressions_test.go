package patchesapply

import (
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR16WalkStopsAfterDefinitelyActiveReturn(t *testing.T) {
	env := &varEnv{values: map[string]serializedValue{}}
	entries, saw, structural := walkPortfile(
		"return()\nvcpkg_from_github(REPO owner/repo PATCHES unreachable.patch)\n", env)
	if structural != parserStructuralNone || saw || len(entries) != 0 {
		t.Fatalf("entries=%v saw=%v structural=%v, want no extraction after active return", entries, saw, structural)
	}
}

func TestR16WalkFailsClosedAfterConditionallyActiveReturn(t *testing.T) {
	env := &varEnv{values: map[string]serializedValue{}}
	entries, saw, structural := walkPortfile(
		"if(UNKNOWN_RETURN_GUARD)\nreturn()\nendif()\nvcpkg_from_github(REPO owner/repo PATCHES uncertain.patch)\n", env)
	if structural == parserStructuralNone || saw || len(entries) != 0 {
		t.Fatalf("entries=%v saw=%v structural=%v, want structural uncertainty and no later extraction", entries, saw, structural)
	}
}

func TestR16ApplyOrderReportsConditionalReturnUncertainty(t *testing.T) {
	dir := writePort(t, "if(UNKNOWN_RETURN_GUARD)\nreturn()\nendif()\nvcpkg_from_github(REPO owner/repo PATCHES uncertain.patch)\n", "uncertain.patch")
	result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if result.Status != evidence.StatusUnknown || string(result.Reason) != "patches_execution_uncertain" {
		t.Fatalf("status/reason = %s/%s, want unknown/patches_execution_uncertain", result.Status, result.Reason)
	}
	if len(result.Applied) != 0 || len(result.Missing) != 0 || len(result.Orphaned) != 0 {
		t.Fatalf("conditional return leaked conclusions: %+v", result)
	}
}

func TestR16UnresolvedPatchIdentitySuppressesOrphanConclusions(t *testing.T) {
	dir := writePort(t, "vcpkg_from_github(REPO owner/repo PATCHES ${UNKNOWN_PATCH}.patch)\n", "real.patch")
	result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if len(result.Undecidable) != 1 {
		t.Fatalf("undecidable = %+v, want unresolved patch identity", result.Undecidable)
	}
	if len(result.Orphaned) != 0 {
		t.Fatalf("orphaned = %+v, want none while patch identity is unresolved", result.Orphaned)
	}
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonOrphanScanIncomplete || string(result.OrphanScanStopCause) != "unresolved_patch_identity" {
		t.Fatalf("status/reason/stop = %s/%s/%s, want unknown/%s/unresolved_patch_identity", result.Status, result.Reason, result.OrphanScanStopCause, ReasonOrphanScanIncomplete)
	}
}

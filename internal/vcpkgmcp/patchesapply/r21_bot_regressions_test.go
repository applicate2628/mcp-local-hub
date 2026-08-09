package patchesapply

import "testing"

func TestR21PortfileUnsetInvalidatesRetainedPatchVariable(t *testing.T) {
	dir := writePort(t, `
set(PATCHLIST fix.patch)
unset(PATCHLIST)
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO acme/widget
    PATCHES ${PATCHLIST}
)
`, "fix.patch")
	result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if got := appliedNames(result); len(got) != 0 {
		t.Fatalf("unset variable still produced applied patches: %v", got)
	}
}

func TestR21LogicalANDAndORShareLeftToRightPrecedence(t *testing.T) {
	got, unresolved := evalCondition("TRUE OR FALSE AND FALSE", newVarEnv("", "", "", nil, nil))
	if got != TriFalse || len(unresolved) != 0 {
		t.Fatalf("condition=True OR False AND False => %v unresolved=%v, want false", got, unresolved)
	}
}

func TestR21ConditionalTripletMutationInvalidatesEarlierFact(t *testing.T) {
	facts := parseTripletFacts(`
set(VCPKG_LIBRARY_LINKAGE static)
if(PORT STREQUAL foo)
  set(VCPKG_LIBRARY_LINKAGE dynamic)
endif()
`, "", "foo", "")
	if _, ok := facts["VCPKG_LIBRARY_LINKAGE"]; ok {
		t.Fatalf("conditional mutation left stale fact: %v", facts)
	}
}

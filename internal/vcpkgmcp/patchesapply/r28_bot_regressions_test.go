package patchesapply

import "testing"

func TestR28EqualParsesCMakeRealNumbers(t *testing.T) {
	env := newVarEnv("", "", "", nil, nil)
	got, unresolved := evalCondition("1.0 EQUAL 1", env)
	if got != TriTrue || len(unresolved) != 0 {
		t.Fatalf("condition=%v unresolved=%v, want true for equal C-double values", got, unresolved)
	}
}

func TestR28ActiveStringMutationInvalidatesStalePatchBinding(t *testing.T) {
	dir := writePort(t, `
set(PATCH_NAME old.patch)
string(APPEND PATCH_NAME .bak)
vcpkg_from_github(PATCHES ${PATCH_NAME})
`, "old.patch.bak")
	result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if len(result.Applied) != 0 || len(result.Missing) != 0 || len(result.Undecidable) != 1 {
		t.Fatalf("applied=%+v missing=%+v undecidable=%+v, want mutated destination fail-closed", result.Applied, result.Missing, result.Undecidable)
	}
}

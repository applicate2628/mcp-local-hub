package patchesapply

import "testing"

// An UNRESOLVED reference inside a quoted operand must never produce a definite
// verdict. Before the guard in operandFromExpansion, expandToken returned
// certainty=TriTrue with the verbatim `${NAME}` still in .text and the name in
// .unresolved, so a comparison on it answered DEFINITELY — and the caller filed
// the patch under `conditional_not_applied`, whose contract is "definitively
// FALSE for this triplet".
//
// The reachability is not exotic: WINSDK_VERSION comes from the MSVC toolchain
// scripts, never from a triplet file, so the default python3 portfile shape hit
// this on every call.
//
// The mirror case is the dangerous direction: a bare `if("${UNKNOWN}")` reported
// `applied` — a confident APPLY where real CMake evaluates FALSE.
func TestOperandFromExpansion_UnresolvedReferenceYieldsNoValue(t *testing.T) {
	cases := []struct {
		name string
		ex   expansion
	}{
		{
			name: "quoted comparison operand — the shape the operator hit",
			ex:   expansion{text: "${WINSDK_VERSION}", certainty: TriTrue, unresolved: []string{"WINSDK_VERSION"}},
		},
		{
			name: "reference embedded in surrounding text",
			ex:   expansion{text: "prefix-${UNKNOWN_FLAG}-suffix", certainty: TriTrue, unresolved: []string{"UNKNOWN_FLAG"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val, unresolved := operandFromExpansion(tc.ex)
			if val != nil {
				t.Fatalf("an operand carrying an unresolved reference must yield NO value, so the comparison "+
					"stays undecidable; got a definite operand %q, which is what let a fabricated verdict reach "+
					"conditional_not_applied", *val)
			}
			if len(unresolved) == 0 {
				t.Fatalf("the unresolved names must be propagated so the caller can report WHY it is undecidable; got none")
			}
		})
	}
}

// The complement: a fully resolved expansion must still yield its value, or the
// fix would have bought correctness by making every guard undecidable.
func TestOperandFromExpansion_ResolvedReferenceStillYieldsItsValue(t *testing.T) {
	val, unresolved := operandFromExpansion(expansion{text: "10.0.22621", certainty: TriTrue})
	if val == nil {
		t.Fatalf("a fully resolved expansion must yield its value; got nil, which would make every guard undecidable")
	}
	if *val != "10.0.22621" {
		t.Fatalf("value = %q, want %q", *val, "10.0.22621")
	}
	if len(unresolved) != 0 {
		t.Fatalf("a fully resolved expansion carries no unresolved names; got %v", unresolved)
	}
}

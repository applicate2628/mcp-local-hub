package patchesapply

import "testing"

func TestR33TripletTopLevelReturnStopsFactParsing(t *testing.T) {
	facts := parseTripletFacts(`
set(VCPKG_LIBRARY_LINKAGE static)
return()
set(VCPKG_LIBRARY_LINKAGE dynamic)
`, "", "demo", "")
	if got := facts["VCPKG_LIBRARY_LINKAGE"]; got != "static" {
		t.Fatalf("VCPKG_LIBRARY_LINKAGE = %q, want pre-return fact static; facts=%v", got, facts)
	}
}

func TestR33TripletConditionalReturnFailsClosed(t *testing.T) {
	for _, src := range []string{
		"set(VCPKG_LIBRARY_LINKAGE static)\nif(PORT STREQUAL demo)\nreturn()\nendif()\n",
		"set(VCPKG_LIBRARY_LINKAGE static)\nforeach(item IN ITEMS demo)\nreturn()\nendforeach()\n",
	} {
		if facts := parseTripletFacts(src, "", "demo", ""); facts != nil {
			t.Fatalf("parseTripletFacts(%q) = %v, want fail-closed nil facts", src, facts)
		}
	}
}

func TestR33TripletDeclarationBodyReturnIsNotExecuted(t *testing.T) {
	facts := parseTripletFacts(`
function(helper)
  return()
endfunction()
set(VCPKG_LIBRARY_LINKAGE static)
`, "", "demo", "")
	if got := facts["VCPKG_LIBRARY_LINKAGE"]; got != "static" {
		t.Fatalf("VCPKG_LIBRARY_LINKAGE = %q, want static; facts=%v", got, facts)
	}
}

func TestR33BareNonConstantConditionIsFalse(t *testing.T) {
	env := newVarEnv("", "", "", nil, nil)
	got, unresolved := evalCondition("1abc", env)
	if got != TriFalse || len(unresolved) != 0 {
		t.Fatalf("condition=%v unresolved=%v, want false with no unresolved variable", got, unresolved)
	}
}

func TestR33VariableDerivedNonConstantConditionIsTrue(t *testing.T) {
	env := newVarEnv("", "", "", map[string]string{"MODE": "1abc"}, nil)
	got, unresolved := evalCondition("MODE", env)
	if got != TriTrue || len(unresolved) != 0 {
		t.Fatalf("condition=%v unresolved=%v, want true from defined variable value", got, unresolved)
	}
}

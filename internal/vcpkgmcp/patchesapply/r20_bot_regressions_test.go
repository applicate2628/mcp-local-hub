package patchesapply

import "testing"

func TestR20DeclarationTerminatorMustMatchOpener(t *testing.T) {
	env := &varEnv{values: map[string]serializedValue{}}
	for _, src := range []string{
		"function(helper)\nendmacro()\nvcpkg_from_github(PATCHES escaped.patch)\n",
		"macro(helper)\nendfunction()\nvcpkg_from_github(PATCHES escaped.patch)\n",
		"endfunction()\nvcpkg_from_github(PATCHES escaped.patch)\n",
	} {
		entries, _, structural := walkPortfile(src, env)
		if structural != parserStructuralExpressionUnparsable || len(entries) != 0 {
			t.Fatalf("walkPortfile(%q) = entries=%+v structural=%v; malformed declaration nesting must fail closed", src, entries, structural)
		}
	}
}

func TestR20TripletUnsetInvalidatesFact(t *testing.T) {
	facts := parseTripletFacts(
		"set(VCPKG_LIBRARY_LINKAGE static)\nunset(VCPKG_LIBRARY_LINKAGE)\n",
		"", "demo", "",
	)
	if _, ok := facts["VCPKG_LIBRARY_LINKAGE"]; ok {
		t.Fatalf("unset triplet variable retained stale fact: %v", facts)
	}
}

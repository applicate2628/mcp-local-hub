package patchesapply

import "testing"

func TestWalkPortfileDefersPatchesInsideUnsupportedLoops(t *testing.T) {
	for _, src := range []string{
		"while(FALSE)\nvcpkg_from_github(REPO owner/repo PATCHES hidden.patch)\nendwhile()\n",
		"foreach(item IN ITEMS)\nvcpkg_from_github(REPO owner/repo PATCHES hidden.patch)\nendforeach()\n",
	} {
		env := &varEnv{values: map[string]serializedValue{}}
		entries, saw, structural := walkPortfile(src, env)
		if structural != parserStructuralDeferredBody || !saw || len(entries) != 0 {
			t.Fatalf("walkPortfile(%q) = entries=%v saw=%v structural=%v; want no unconditional entries and deferred-body signal", src, entries, saw, structural)
		}
	}
}

func TestParseTripletFactsRejectsMismatchedControlFlow(t *testing.T) {
	for _, src := range []string{
		"if(TRUE)\nset(VCPKG_TARGET_ARCHITECTURE x64)\nendwhile()\n",
		"endwhile()\nset(VCPKG_TARGET_ARCHITECTURE x64)\n",
		"foreach(x IN ITEMS a)\nset(VCPKG_TARGET_ARCHITECTURE x64)\nendif()\n",
		"if(TRUE)\nset(VCPKG_TARGET_ARCHITECTURE x64)\n",
	} {
		if facts := parseTripletFacts(src, "", "demo", ""); facts != nil {
			t.Fatalf("parseTripletFacts(%q) = %v; structurally mismatched triplet must establish no facts", src, facts)
		}
	}
}

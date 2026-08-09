package patchesapply

import "testing"

func TestR18WalkIgnoresPatchesKeywordOutsideSourceHelpers(t *testing.T) {
	env := &varEnv{values: map[string]serializedValue{}}
	entries, saw, structural := walkPortfile(
		"message(STATUS PATCHES decoy.patch)\nvcpkg_from_github(REPO owner/repo PATCHES real.patch)\n", env)
	if structural != parserStructuralNone || !saw {
		t.Fatalf("saw=%v structural=%v, want one recognized source-helper PATCHES list", saw, structural)
	}
	if len(entries) != 1 || entries[0].expanded != "real.patch" {
		t.Fatalf("entries=%+v, want only real.patch from vcpkg_from_github", entries)
	}
	for command := range patchAcceptingCommands {
		entries, saw, structural = walkPortfile(command+"(PATCHES real.patch)\n", env)
		if structural != parserStructuralNone || !saw || len(entries) != 1 || entries[0].expanded != "real.patch" {
			t.Errorf("%s PATCHES contract: entries=%+v saw=%v structural=%v", command, entries, saw, structural)
		}
	}
}

func TestR18QuotedNonBooleanConditionIsFalse(t *testing.T) {
	env := &varEnv{values: map[string]serializedValue{}}
	if got, unresolved := evalCondition(`"static"`, env); got != TriFalse || len(unresolved) != 0 {
		t.Fatalf("quoted non-boolean = %v unresolved=%v, want false", got, unresolved)
	}
	if got, unresolved := evalCondition(`"ON"`, env); got != TriTrue || len(unresolved) != 0 {
		t.Fatalf("quoted true constant = %v unresolved=%v, want true", got, unresolved)
	}
}

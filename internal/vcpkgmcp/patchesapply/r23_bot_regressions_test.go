package patchesapply

import "testing"

func TestR23ExecutableLoopMutationInvalidatesPatchList(t *testing.T) {
	for _, mutation := range []string{
		"set(PATCH_LIST loop.patch)",
		"unset(PATCH_LIST)",
		"list(APPEND PATCH_LIST loop.patch)",
	} {
		t.Run(mutation, func(t *testing.T) {
			env := newVarEnv("", "", "", nil, nil)
			source := "set(PATCH_LIST base.patch)\nforeach(item IN ITEMS a)\n" + mutation +
				"\nendforeach()\nvcpkg_from_github(PATCHES ${PATCH_LIST})\n"
			entries, saw, structural := walkPortfile(source, env)
			if len(entries) != 0 || structural != parserStructuralExpressionUnparsable {
				t.Fatalf("walkPortfile() = entries=%v saw=%v structural=%v, want fail-closed expression uncertainty", entries, saw, structural)
			}
		})
	}
}

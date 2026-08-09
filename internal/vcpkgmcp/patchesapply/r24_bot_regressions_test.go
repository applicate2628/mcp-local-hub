package patchesapply

import (
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR24ComparisonUsesUndefinedPlainOperandAsLiteral(t *testing.T) {
	env := newVarEnv("", "", "", map[string]string{"VCPKG_LIBRARY_LINKAGE": "static"}, nil)
	got, unresolved := evalCondition("VCPKG_LIBRARY_LINKAGE STREQUAL static", env)
	if got != TriTrue || len(unresolved) != 0 {
		t.Fatalf("condition=%v unresolved=%v, want true with literal RHS", got, unresolved)
	}
}

func TestR24InactiveSetReferenceIsNotAnOrphan(t *testing.T) {
	dir := writePort(t, `
if(WINDOWS)
  set(PATCHLIST win.patch)
else()
  set(PATCHLIST unix.patch)
endif()
vcpkg_from_github(PATCHES ${PATCHLIST})
`, "win.patch", "unix.patch")
	result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows", VarOverrides: map[string]string{"WINDOWS": "ON"}})
	if findOrphaned(result, "unix.patch") != nil {
		t.Fatalf("inactive alternate patch was classified orphaned: %+v", result.Orphaned)
	}
}

func TestR24ActiveExternalCMakeLoadFailsClosed(t *testing.T) {
	for _, command := range []string{"include(patches.cmake)", "add_subdirectory(patches)"} {
		t.Run(command, func(t *testing.T) {
			dir := writePort(t, command+"\n", "fix.patch")
			result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
			if result.Status != evidence.StatusUnknown || result.Reason != ReasonPatchesExecutionUncertain || len(result.Orphaned) != 0 {
				t.Fatalf("result=%+v, want unknown execution uncertainty with no orphan conclusion", result)
			}
		})
	}
}

func TestR24ParentScopeSetPreservesLocalBinding(t *testing.T) {
	env := newVarEnv("", "", "", nil, nil)
	entries, _, structural := walkPortfile("set(PATCHLIST old.patch)\nset(PATCHLIST new.patch PARENT_SCOPE)\nvcpkg_from_github(PATCHES ${PATCHLIST})\n", env)
	if structural != parserStructuralNone || len(entries) != 1 || entries[0].expanded != "old.patch" {
		t.Fatalf("entries=%+v structural=%v, want retained local old.patch", entries, structural)
	}
}

func TestR24GetFilenameComponentHonorsBaseDir(t *testing.T) {
	portDir := t.TempDir()
	baseDir := filepath.Join(portDir, "other")
	env := newVarEnv(portDir, "", "", nil, nil)
	entries, _, structural := walkPortfile("get_filename_component(PATCH file.patch ABSOLUTE BASE_DIR "+baseDir+")\nvcpkg_from_github(PATCHES ${PATCH})\n", env)
	want := filepath.Join(baseDir, "file.patch")
	if structural != parserStructuralNone || len(entries) != 1 || entries[0].expanded != want {
		t.Fatalf("entries=%+v structural=%v, want %q", entries, structural, want)
	}
}

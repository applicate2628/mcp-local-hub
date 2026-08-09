package patchesapply

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR22WalkRecognizesLegacyArchiveExtractionHelper(t *testing.T) {
	env := &varEnv{values: map[string]serializedValue{}}
	entries, saw, structural := walkPortfile("vcpkg_extract_source_archive_ex(PATCHES legacy.patch)\n", env)
	if structural != parserStructuralNone || !saw || len(entries) != 1 || entries[0].expanded != "legacy.patch" {
		t.Fatalf("entries=%+v saw=%v structural=%v, want legacy helper PATCHES declaration", entries, saw, structural)
	}
}

func TestR22WalkBoundsExpandedPatchDeclarations(t *testing.T) {
	const expectedDeclarationLimit = 4096
	value := strings.Repeat("a.patch;", expectedDeclarationLimit+1)
	env := &varEnv{values: map[string]serializedValue{}}
	entries, saw, structural := walkPortfile("set(PATCH_LIST "+value+")\nvcpkg_from_github(PATCHES ${PATCH_LIST})\n", env)
	if !saw || structural == parserStructuralNone {
		t.Fatalf("entries=%d saw=%v structural=%v, want explicit bounded-analysis stop", len(entries), saw, structural)
	}
	if len(entries) > expectedDeclarationLimit {
		t.Fatalf("retained entries=%d, want at most %d", len(entries), expectedDeclarationLimit)
	}
}

func TestR22DeclarationLimitIsExposedWithoutPatchProbes(t *testing.T) {
	portDir := t.TempDir()
	value := strings.Repeat("a.patch;", MaxPatchDeclarations+1)
	portfile := "set(PATCH_LIST " + value + ")\nvcpkg_from_github(PATCHES ${PATCH_LIST})\n"
	if err := os.WriteFile(filepath.Join(portDir, "portfile.cmake"), []byte(portfile), 0o600); err != nil {
		t.Fatal(err)
	}
	result := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows"})
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonPatchDeclarationLimitExceeded {
		t.Fatalf("status=%s reason=%s, want unknown/%s", result.Status, result.Reason, ReasonPatchDeclarationLimitExceeded)
	}
	if len(result.Applied) != 0 || len(result.Missing) != 0 || len(result.Undecidable) != 0 {
		t.Fatalf("partial declaration inventory reached patch probing: %+v", result)
	}
}

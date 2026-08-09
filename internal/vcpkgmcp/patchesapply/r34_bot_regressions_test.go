package patchesapply

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestR34TripletStringMutationInvalidatesRetainedFact(t *testing.T) {
	facts := parseTripletFacts(`
set(VCPKG_LIBRARY_LINKAGE static)
string(APPEND VCPKG_LIBRARY_LINKAGE x)
`, "", "demo", "")
	if _, ok := facts["VCPKG_LIBRARY_LINKAGE"]; ok {
		t.Fatalf("mutated VCPKG_LIBRARY_LINKAGE survived as a retained fact: %v", facts)
	}
}

func TestR34TripletStandardMutatorsInvalidateRetainedFacts(t *testing.T) {
	for name, command := range map[string]string{
		"list output":            "list(LENGTH INPUT VCPKG_LIBRARY_LINKAGE)",
		"file output":            "file(READ input.txt VCPKG_LIBRARY_LINKAGE)",
		"math output":            "math(EXPR VCPKG_LIBRARY_LINKAGE 1)",
		"filename output":        "get_filename_component(VCPKG_LIBRARY_LINKAGE input.txt NAME)",
		"filename secondary":     `get_filename_component(PROGRAM "tool --flag" PROGRAM PROGRAM_ARGS VCPKG_LIBRARY_LINKAGE)`,
		"list in-place mutation": "list(APPEND VCPKG_LIBRARY_LINKAGE x)",
	} {
		t.Run(name, func(t *testing.T) {
			facts := parseTripletFacts("set(VCPKG_LIBRARY_LINKAGE static)\n"+command+"\n", "", "demo", "")
			if _, ok := facts["VCPKG_LIBRARY_LINKAGE"]; ok {
				t.Fatalf("%s left a stale retained fact: %v", command, facts)
			}
		})
	}
}

func TestR34ProjectionAdmissionBoundsRetainedAggregateWithoutMarshal(t *testing.T) {
	oversized := Result{Applied: []AppliedPatch{{Filename: strings.Repeat("x", publicresult.MaxEncodedBytes+1)}}}
	if !oversized.PublicResultRequiresProjection(publicresult.MaxEncodedBytes) {
		t.Fatal("oversized retained source bytes bypassed pre-marshal projection admission")
	}

	many := Result{Orphaned: make([]OrphanedPatch, publicresult.MaxEncodedBytes+1)}
	if !many.PublicResultRequiresProjection(publicresult.MaxEncodedBytes) {
		t.Fatal("oversized retained element count bypassed pre-marshal projection admission")
	}

	if (Result{Triplet: "x64-windows"}).PublicResultRequiresProjection(publicresult.MaxEncodedBytes) {
		t.Fatal("bounded result was projected by the lower-bound admission check")
	}
}

func TestR34PortfileFileOutputInvalidatesRetainedPatchBinding(t *testing.T) {
	dir := writePort(t, `
set(PATCH old.patch)
file(RELATIVE_PATH PATCH "${CURRENT_PORT_DIR}" "${CURRENT_PORT_DIR}/new.patch")
vcpkg_from_github(PATCHES ${PATCH})
`, "old.patch", "new.patch")

	result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if len(result.Applied) != 0 || len(result.Missing) != 0 || len(result.Undecidable) != 1 {
		t.Fatalf("applied=%+v missing=%+v undecidable=%+v, want file() output fail-closed", result.Applied, result.Missing, result.Undecidable)
	}
}

func TestR34PortfileStandardOutputsInvalidateRetainedPatchBinding(t *testing.T) {
	for name, command := range map[string]string{
		"file read":          `file(READ "${CURRENT_PORT_DIR}/new.patch" PATCH)`,
		"list length":        `list(LENGTH INPUT PATCH)`,
		"math expr":          `math(EXPR PATCH "1 + 1")`,
		"filename secondary": `get_filename_component(PROGRAM "tool --flag" PROGRAM PROGRAM_ARGS PATCH)`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := writePort(t, "set(PATCH old.patch)\n"+command+"\nvcpkg_from_github(PATCHES ${PATCH})\n", "old.patch", "new.patch")
			result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
			if len(result.Applied) != 0 || len(result.Missing) != 0 {
				t.Fatalf("%s retained stale patch binding: applied=%+v missing=%+v", command, result.Applied, result.Missing)
			}
		})
	}
}

func TestR34PortfileHarmlessFileModePreservesBinding(t *testing.T) {
	dir := writePort(t, `
set(PATCH old.patch)
file(WRITE ignored.txt payload)
vcpkg_from_github(PATCHES ${PATCH})
`, "old.patch")
	result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if len(result.Applied) != 1 || result.Applied[0].Filename != "old.patch" {
		t.Fatalf("harmless file(WRITE) changed patch binding: %+v", result)
	}
}

func TestR34KnownVariableTruthPreservesWhitespace(t *testing.T) {
	env := newVarEnv("", "", "", map[string]string{"MODE": " off "}, nil)
	got, unresolved := evalCondition("MODE", env)
	if got != TriTrue || len(unresolved) != 0 {
		t.Fatalf("condition=%v unresolved=%v, want whitespace-bearing known value true", got, unresolved)
	}

	env = newVarEnv("", "", "", map[string]string{"MODE": "oFf"}, nil)
	got, unresolved = evalCondition("MODE", env)
	if got != TriFalse || len(unresolved) != 0 {
		t.Fatalf("condition=%v unresolved=%v, want exact case-folded OFF false", got, unresolved)
	}
}

func TestR34OrphanReferencesUseFilesystemIdentity(t *testing.T) {
	dir := writePort(t, "vcpkg_from_github(PATCHES alias.patch)\n", "original.patch")
	if err := os.Link(filepath.Join(dir, "original.patch"), filepath.Join(dir, "alias.patch")); err != nil {
		t.Fatalf("create hard-link fixture: %v", err)
	}

	result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if len(result.Orphaned) != 0 {
		t.Fatalf("same filesystem file was classified orphaned through another name: %+v", result.Orphaned)
	}
}

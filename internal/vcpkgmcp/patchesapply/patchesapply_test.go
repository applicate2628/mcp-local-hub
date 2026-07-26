package patchesapply

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// writeFixture creates a temp port directory with portfile.cmake plus any
// listed patch files (empty content — existence is all this package
// checks). Every test in this file uses t.TempDir() only, per the hard
// constraint that this package never reads a real vcpkg checkout.
func writeFixture(t *testing.T, portfileContent string, patchFiles ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "portfile.cmake"), []byte(portfileContent), 0o644); err != nil {
		t.Fatalf("write portfile.cmake: %v", err)
	}
	for _, f := range patchFiles {
		full := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", f, err)
		}
		if err := os.WriteFile(full, []byte("dummy patch content\n"), 0o644); err != nil {
			t.Fatalf("write patch fixture %s: %v", f, err)
		}
	}
	return dir
}

func findApplied(res Result, filename string) *AppliedPatch {
	for i := range res.Applied {
		if res.Applied[i].Filename == filename {
			return &res.Applied[i]
		}
	}
	return nil
}

func findConditional(res Result, filename string) *ConditionalPatch {
	for i := range res.ConditionalNotApplied {
		if res.ConditionalNotApplied[i].Filename == filename {
			return &res.ConditionalNotApplied[i]
		}
	}
	return nil
}

func findUndecidable(res Result, filename string) *UndecidablePatch {
	for i := range res.Undecidable {
		if res.Undecidable[i].Filename == filename {
			return &res.Undecidable[i]
		}
	}
	return nil
}

func findOrphaned(res Result, filename string) *OrphanedPatch {
	for i := range res.Orphaned {
		if res.Orphaned[i].Filename == filename {
			return &res.Orphaned[i]
		}
	}
	return nil
}

func filenames(entries []AppliedPatch) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Filename
	}
	return out
}

// --- 1. Flat literal PATCHES list (unconditional) ---------------------------

func TestApplyOrder_FlatLiteralList(t *testing.T) {
	portDir := writeFixture(t, `
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO foo/bar
    REF v1.0.0
    SHA512 0
    HEAD_REF main
    PATCHES
        first.patch
        second.patch
)
`, "first.patch", "second.patch")

	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "bar"})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	if got := filenames(res.Applied); len(got) != 2 || got[0] != "first.patch" || got[1] != "second.patch" {
		t.Fatalf("applied = %v, want [first.patch second.patch] in order", got)
	}
	for i, want := range []string{"first.patch", "second.patch"} {
		if res.Applied[i].Ordinal != i {
			t.Errorf("applied[%d] (%s) ordinal = %d, want %d", i, want, res.Applied[i].Ordinal, i)
		}
		if res.Applied[i].Existence != evidence.PresenceExists {
			t.Errorf("applied[%d] (%s) exists = false, want true", i, want)
		}
		if res.Applied[i].Guard != "" {
			t.Errorf("applied[%d] (%s) guard = %q, want empty (unconditional)", i, want, res.Applied[i].Guard)
		}
	}
	if len(res.ConditionalNotApplied) != 0 || len(res.Undecidable) != 0 || len(res.Missing) != 0 || len(res.Orphaned) != 0 {
		t.Errorf("expected every other bucket empty, got %+v", res)
	}
}

// --- 2. python3-style conditional accumulation across 3 triplets -----------

const python3StylePortfile = `
set(PATCHES
    "0001-base-01.patch"
    "0002-base-02.patch"
    "0003-base-03.patch"
    "0004-base-04.patch"
    "0005-base-05.patch"
    "0006-base-06.patch"
    "0007-base-07.patch"
    "0008-base-08.patch"
)
if(VCPKG_LIBRARY_LINKAGE STREQUAL "static" AND NOT VCPKG_TARGET_IS_MINGW)
    list(APPEND PATCHES "0009-static-library.patch")
endif()
if(VCPKG_TARGET_IS_MINGW)
    list(APPEND PATCHES "0021-mingw-maintained-stack.patch" "0022-mingw-cpython-main-head-fixes.patch")
endif()
if(VCPKG_TARGET_IS_WINDOWS AND NOT VCPKG_TARGET_IS_MINGW)
    if(VCPKG_CROSSCOMPILING)
        list(APPEND PATCHES "0016-fix-win-cross.patch")
    else()
        list(APPEND PATCHES "0017-fix-win.patch")
    endif()
    if("${WINSDK_VERSION}" VERSION_GREATER_EQUAL "10.0.22000")
        list(APPEND PATCHES "0007-workaround-windows-11-sdk-rc-compiler-error.patch")
    endif()
endif()

vcpkg_from_github(
    REPO python/cpython
    REF v3.11.0
    SHA512 0
    PATCHES ${PATCHES}
)
`

var python3StylePatchFiles = []string{
	"0001-base-01.patch",
	"0002-base-02.patch",
	"0003-base-03.patch",
	"0004-base-04.patch",
	"0005-base-05.patch",
	"0006-base-06.patch",
	"0007-base-07.patch",
	"0008-base-08.patch",
	"0009-static-library.patch",
	"0021-mingw-maintained-stack.patch",
	"0022-mingw-cpython-main-head-fixes.patch",
	"0016-fix-win-cross.patch",
	"0017-fix-win.patch",
	"0007-workaround-windows-11-sdk-rc-compiler-error.patch",
}

// python3Args builds the arguments for one python3-shape subtest.
//
// The triplet facts now come from the two mechanisms that actually carry
// them, instead of from the triplet NAME:
//
//   - VCPKG_LIBRARY_LINKAGE comes from a real TRIPLET FILE, because that is
//     what a triplet file genuinely sets (every builtin x64-windows*.cmake
//     does exactly this).
//   - VCPKG_TARGET_IS_MINGW / _IS_WINDOWS come from var_overrides, because
//     in real vcpkg they are NOT set by the triplet file at all: vcpkg's own
//     scripts derive them from VCPKG_CMAKE_SYSTEM_NAME at build time. A
//     static analyzer that never runs those scripts must be TOLD them —
//     which is precisely what the var_overrides escape hatch is for.
func python3Args(t *testing.T, portDir, triplet, linkage string, targetIs map[string]string) Args {
	t.Helper()
	tripletDir := writeTripletDir(t, triplet, "set(VCPKG_TARGET_ARCHITECTURE x64)\nset(VCPKG_LIBRARY_LINKAGE "+linkage+")\n")
	overrides := map[string]string{
		"VCPKG_CROSSCOMPILING": "OFF",
		"WINSDK_VERSION":       "10.0.22000",
	}
	for k, v := range targetIs {
		overrides[k] = v
	}
	return Args{
		PortDir: portDir, Triplet: triplet, PortName: "python3",
		OverlayTriplets: []string{tripletDir},
		VarOverrides:    overrides,
	}
}

func TestApplyOrder_Python3StyleConditionalAccumulation(t *testing.T) {
	base := []string{
		"0001-base-01.patch", "0002-base-02.patch", "0003-base-03.patch", "0004-base-04.patch",
		"0005-base-05.patch", "0006-base-06.patch", "0007-base-07.patch", "0008-base-08.patch",
	}

	t.Run("mingw_triplet", func(t *testing.T) {
		portDir := writeFixture(t, python3StylePortfile, python3StylePatchFiles...)
		res := applyOrder(python3Args(t, portDir, "x64-mingw-dynamic", "dynamic", map[string]string{
			"VCPKG_TARGET_IS_MINGW":   "ON",
			"VCPKG_TARGET_IS_WINDOWS": "OFF",
		}), DefaultDeps())
		if res.Status != evidence.StatusOK {
			t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
		}
		got := filenames(res.Applied)
		want := append(append([]string{}, base...), "0021-mingw-maintained-stack.patch", "0022-mingw-cpython-main-head-fixes.patch")
		if !equalStrings(got, want) {
			t.Errorf("mingw applied = %v, want %v", got, want)
		}
		// The static-linkage and windows-only patches must all be
		// definitively guard-false for a MinGW triplet, never undecidable —
		// VCPKG_TARGET_IS_MINGW is KNOWN here (supplied, not guessed) and
		// Kleene AND short-circuits through the nested CROSSCOMPILING/WINSDK
		// sub-guards.
		for _, f := range []string{"0009-static-library.patch", "0016-fix-win-cross.patch", "0017-fix-win.patch", "0007-workaround-windows-11-sdk-rc-compiler-error.patch"} {
			if findConditional(res, f) == nil {
				t.Errorf("expected %s in conditional_not_applied for mingw triplet, got result=%+v", f, res)
			}
		}
		if len(res.Undecidable) != 0 {
			t.Errorf("expected zero undecidable for mingw triplet (outer guard is definitively false), got %+v", res.Undecidable)
		}
		if len(res.Orphaned) != 0 {
			t.Errorf("base PATCHES plus guarded appends must leave zero false orphans, got %+v", res.Orphaned)
		}
	})

	t.Run("windows_non_mingw_triplet", func(t *testing.T) {
		portDir := writeFixture(t, python3StylePortfile, python3StylePatchFiles...)
		res := applyOrder(python3Args(t, portDir, "x64-windows", "dynamic", map[string]string{
			"VCPKG_TARGET_IS_MINGW":   "OFF",
			"VCPKG_TARGET_IS_WINDOWS": "ON",
		}), DefaultDeps())
		if res.Status != evidence.StatusOK {
			t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
		}
		got := filenames(res.Applied)
		want := append(append([]string{}, base...), "0017-fix-win.patch", "0007-workaround-windows-11-sdk-rc-compiler-error.patch")
		if !equalStrings(got, want) {
			t.Errorf("windows-non-mingw applied = %v, want %v", got, want)
		}
		if findConditional(res, "0016-fix-win-cross.patch") == nil {
			t.Errorf("expected 0016-fix-win-cross.patch in conditional_not_applied (CROSSCOMPILING=OFF), got %+v", res)
		}
		if findConditional(res, "0009-static-library.patch") == nil {
			t.Errorf("expected 0009-static-library.patch in conditional_not_applied (dynamic linkage), got %+v", res)
		}
		if len(res.Orphaned) != 0 {
			t.Errorf("base PATCHES plus guarded appends must leave zero false orphans, got %+v", res.Orphaned)
		}
	})

	t.Run("static_triplet", func(t *testing.T) {
		portDir := writeFixture(t, python3StylePortfile, python3StylePatchFiles...)
		// Linkage comes from the triplet FILE here, not from the "-static"
		// component in the name.
		res := applyOrder(python3Args(t, portDir, "x64-windows-static", "static", map[string]string{
			"VCPKG_TARGET_IS_MINGW":   "OFF",
			"VCPKG_TARGET_IS_WINDOWS": "ON",
		}), DefaultDeps())
		if res.Status != evidence.StatusOK {
			t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
		}
		got := filenames(res.Applied)
		want := append(append([]string{}, base...), "0009-static-library.patch", "0017-fix-win.patch", "0007-workaround-windows-11-sdk-rc-compiler-error.patch")
		if !equalStrings(got, want) {
			t.Errorf("static applied = %v, want %v", got, want)
		}
		if len(res.Orphaned) != 0 {
			t.Errorf("base PATCHES plus guarded appends must leave zero false orphans, got %+v", res.Orphaned)
		}
	})
}

func TestApplyOrder_NonliteralSetListThenAppend_LibmysqlShape(t *testing.T) {
	portDir := writeFixture(t, `
set(_libmysql_patches base-one.patch base-two.patch base-three.patch)
if(VCPKG_CROSSCOMPILING)
    list(APPEND _libmysql_patches cross-build.patch)
endif()
vcpkg_from_github(REPO mysql/mysql REF v1 SHA512 0 PATCHES ${_libmysql_patches})
`, "base-one.patch", "base-two.patch", "base-three.patch", "cross-build.patch")

	res := ApplyOrder(Args{
		PortDir: portDir, Triplet: "x64-windows", PortName: "libmysql",
		VarOverrides: map[string]string{"VCPKG_CROSSCOMPILING": "ON"},
	})
	want := []string{"base-one.patch", "base-two.patch", "base-three.patch", "cross-build.patch"}
	if got := filenames(res.Applied); !equalStrings(got, want) {
		t.Errorf("applied = %v, want %v", got, want)
	}
	if len(res.Orphaned) != 0 {
		t.Errorf("all _libmysql_patches entries are referenced, got false orphans %+v", res.Orphaned)
	}
}

// --- 3. Nested crosscompiling if/else picks exactly one of two patches -----

func TestApplyOrder_NestedCrossCompilingPicksExactlyOne(t *testing.T) {
	portfile := `
if(VCPKG_CROSSCOMPILING)
    list(APPEND PATCHES "cross.patch")
else()
    list(APPEND PATCHES "native.patch")
endif()
vcpkg_from_github(REPO a/b REF v1 SHA512 0 PATCHES ${PATCHES})
`
	t.Run("crosscompiling_on", func(t *testing.T) {
		portDir := writeFixture(t, portfile, "cross.patch", "native.patch")
		res := ApplyOrder(Args{
			PortDir: portDir, Triplet: "arm64-windows", PortName: "x",
			VarOverrides: map[string]string{"VCPKG_CROSSCOMPILING": "ON"},
		})
		if findApplied(res, "cross.patch") == nil {
			t.Errorf("expected cross.patch applied, got %+v", res)
		}
		if findConditional(res, "native.patch") == nil {
			t.Errorf("expected native.patch conditional_not_applied, got %+v", res)
		}
		if len(res.Applied) != 1 || len(res.ConditionalNotApplied) != 1 {
			t.Errorf("expected exactly one applied + one conditional_not_applied, got applied=%v conditional=%v", res.Applied, res.ConditionalNotApplied)
		}
	})

	t.Run("crosscompiling_off", func(t *testing.T) {
		portDir := writeFixture(t, portfile, "cross.patch", "native.patch")
		res := ApplyOrder(Args{
			PortDir: portDir, Triplet: "x64-windows", PortName: "x",
			VarOverrides: map[string]string{"VCPKG_CROSSCOMPILING": "OFF"},
		})
		if findApplied(res, "native.patch") == nil {
			t.Errorf("expected native.patch applied, got %+v", res)
		}
		if findConditional(res, "cross.patch") == nil {
			t.Errorf("expected cross.patch conditional_not_applied, got %+v", res)
		}
	})
}

// --- 4. licensepp shape: patch path resolved OUTSIDE the port directory ----

func TestApplyOrder_PathResolvedOutsidePortDir_LicensePPShape(t *testing.T) {
	// The overlay port directory itself (portDir) never contains the patch
	// file — it lives under a completely separate "builtin vcpkg root"
	// directory tree, reached only via $ENV{VCPKG_ROOT} + get_filename_component
	// chaining. A port-dir-relative assumption would report this as missing;
	// the correct answer is "applied, and it exists".
	portDir := writeFixture(t, `
set(_test_vcpkg_root "$ENV{VCPKG_ROOT}")
get_filename_component(_test_builtin_port_dir "${_test_vcpkg_root}/ports/${PORT}" ABSOLUTE)

vcpkg_from_github(
    REPO foo/licensepp
    REF v1.0
    SHA512 0
    PATCHES
        "${_test_builtin_port_dir}/add-stdint.diff"
)
`)

	builtinRoot := t.TempDir()
	builtinPatchDir := filepath.Join(builtinRoot, "ports", "licensepp")
	if err := os.MkdirAll(builtinPatchDir, 0o755); err != nil {
		t.Fatalf("mkdir builtin patch dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(builtinPatchDir, "add-stdint.diff"), []byte("diff\n"), 0o644); err != nil {
		t.Fatalf("write builtin patch: %v", err)
	}

	res := ApplyOrder(Args{
		PortDir: portDir, Triplet: "x64-windows",
		PortName: "licensepp", VcpkgRoot: builtinRoot,
	})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("expected exactly 1 applied patch, got %+v", res.Applied)
	}
	want := filepath.Clean(filepath.Join(builtinPatchDir, "add-stdint.diff"))
	if res.Applied[0].ResolvedPath != want {
		t.Errorf("resolved_path = %q, want %q (outside port dir %q)", res.Applied[0].ResolvedPath, want, portDir)
	}
	if res.Applied[0].Existence != evidence.PresenceExists {
		t.Errorf("exists = false, want true — this is the false-positive trap: a healthy port must not be reported missing")
	}
	if len(res.Missing) != 0 {
		t.Errorf("expected zero missing, got %+v", res.Missing)
	}

	// Sub-case: without VcpkgRoot, $ENV{VCPKG_ROOT} cannot resolve. The
	// package must preserve that uncertainty rather than asserting missing.
	t.Run("unresolvable_without_vcpkg_root", func(t *testing.T) {
		res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "licensepp"})
		if res.Status != evidence.StatusOK {
			t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
		}
		if len(res.Applied) != 0 {
			t.Errorf("unresolved path must not be asserted applied, got %+v", res.Applied)
		}
		if len(res.Missing) != 0 {
			t.Errorf("unresolved path must not be asserted missing, got %+v", res.Missing)
		}
		u := findUndecidable(res, "${_test_builtin_port_dir}/add-stdint.diff")
		if u == nil {
			t.Fatalf("expected unresolved path in undecidable, got %+v", res)
		}
		if !equalStrings(u.UnresolvedVars, []string{"$ENV{VCPKG_ROOT}"}) {
			t.Errorf("unresolved vars = %v, want [$ENV{VCPKG_ROOT}]", u.UnresolvedVars)
		}
	})
}

func TestApplyOrder_AllCapsExtensionlessPatchIsNotDropped(t *testing.T) {
	portDir := writeFixture(t, `
vcpkg_from_github(PATCHES LEGACYPATCH REPO a/b REF v1 SHA512 0)
`, "LEGACYPATCH")

	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "x"})
	if got := filenames(res.Applied); !equalStrings(got, []string{"LEGACYPATCH"}) {
		t.Errorf("ALL-CAPS extensionless PATCHES entry was dropped: applied = %v", got)
	}
}

func TestApplyOrder_BracketArgumentOpeningNewlineIgnored(t *testing.T) {
	portDir := writeFixture(t, "vcpkg_from_github(REPO a/b REF v1 SHA512 0 PATCHES [[\nnewline.patch]])\n", "newline.patch")

	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "x"})
	if got := filenames(res.Applied); !equalStrings(got, []string{"newline.patch"}) {
		t.Errorf("applied = %v, want [newline.patch] without an opening newline", got)
	}
	if len(res.Missing) != 0 || len(res.Orphaned) != 0 {
		t.Errorf("opening bracket newline must not create a missing/orphan pair, got missing=%+v orphaned=%+v", res.Missing, res.Orphaned)
	}
}

// --- 5. Orphaned file --------------------------------------------------------

func TestApplyOrder_OrphanedFile(t *testing.T) {
	portDir := writeFixture(t, `
vcpkg_from_github(REPO a/b REF v1 SHA512 0 PATCHES referenced.patch)
`, "referenced.patch", "orphan.patch")

	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "x"})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	if findApplied(res, "referenced.patch") == nil {
		t.Errorf("expected referenced.patch applied, got %+v", res.Applied)
	}
	if findOrphaned(res, "orphan.patch") == nil {
		t.Errorf("expected orphan.patch orphaned, got %+v", res.Orphaned)
	}
	if len(res.Missing) != 0 {
		t.Errorf("expected zero missing, got %+v", res.Missing)
	}
}

func TestApplyOrder_OrphanedNestedFile(t *testing.T) {
	portDir := writeFixture(t, `
vcpkg_from_github(REPO a/b REF v1 SHA512 0 PATCHES nested/live.patch)
`, "nested/live.patch", "nested/dead.patch")

	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "x"})
	if findOrphaned(res, "dead.patch") == nil {
		t.Errorf("expected nested/dead.patch orphaned, got %+v", res.Orphaned)
	}
}

// --- 6. Missing referenced patch --------------------------------------------

func TestApplyOrder_MissingReferencedPatch(t *testing.T) {
	// ghost.patch is referenced but never created on disk — a real defect,
	// distinct from an orphan (which is the reverse direction).
	portDir := writeFixture(t, `
vcpkg_from_github(REPO a/b REF v1 SHA512 0 PATCHES ghost.patch)
`)

	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "x"})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	applied := findApplied(res, "ghost.patch")
	if applied == nil {
		t.Fatalf("expected ghost.patch applied (guard is unconditional), got %+v", res.Applied)
	}
	if applied.Existence == evidence.PresenceExists {
		t.Errorf("expected exists=false for ghost.patch")
	}
	if len(res.Missing) != 1 || res.Missing[0].Filename != "ghost.patch" {
		t.Errorf("expected exactly one missing entry for ghost.patch, got %+v", res.Missing)
	}
	if len(res.Orphaned) != 0 {
		t.Errorf("expected zero orphaned (nothing physical on disk), got %+v", res.Orphaned)
	}
}

// --- 7. .diff extension (freexl shape) --------------------------------------

func TestApplyOrder_DiffExtension(t *testing.T) {
	portDir := writeFixture(t, `
vcpkg_from_github(REPO a/freexl REF v1 SHA512 0 PATCHES android-builtin-iconv.diff)
`, "android-builtin-iconv.diff", "stray.diff")

	res := ApplyOrder(Args{PortDir: portDir, Triplet: "arm64-android", PortName: "freexl"})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	applied := findApplied(res, "android-builtin-iconv.diff")
	if applied == nil || applied.Existence != evidence.PresenceExists {
		t.Fatalf("expected android-builtin-iconv.diff applied+exists, got %+v", res.Applied)
	}
	if findOrphaned(res, "stray.diff") == nil {
		t.Errorf("expected stray.diff (unreferenced .diff) orphaned, got %+v", res.Orphaned)
	}
}

// --- 8. No PATCHES declared at all (netgen shape) ---------------------------

func TestApplyOrder_NoPatchesDeclared_NetgenShape(t *testing.T) {
	portDir := writeFixture(t, `
vcpkg_from_github(
    REPO NGSolve/netgen
    REF v1
    SHA512 0
)
vcpkg_configure_cmake(SOURCE_PATH "${SOURCE_PATH}")
`, "142.diff", "cross-build.patch", "git-ver.patch")

	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "netgen"})
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonNoPatchesDeclared {
		t.Fatalf("status/reason = %v/%v, want unknown/no_patches_declared; result=%+v", res.Status, res.Reason, res)
	}
	if len(res.Applied) != 0 || len(res.ConditionalNotApplied) != 0 || len(res.Undecidable) != 0 {
		t.Errorf("expected every declared-entry bucket empty, got %+v", res)
	}
	if len(res.Orphaned) != 3 {
		t.Fatalf("expected all 3 on-disk patch files orphaned, got %+v", res.Orphaned)
	}
	for _, f := range []string{"142.diff", "cross-build.patch", "git-ver.patch"} {
		if findOrphaned(res, f) == nil {
			t.Errorf("expected %s orphaned, got %+v", f, res.Orphaned)
		}
	}
}

// --- 9. Guard referencing an unresolvable variable -> undecidable ----------

func TestApplyOrder_UnresolvableGuard_Undecidable(t *testing.T) {
	portDir := writeFixture(t, `
if(SOME_UNKNOWN_FLAG)
    list(APPEND PATCHES "conditional.patch")
endif()
vcpkg_from_github(REPO a/b REF v1 SHA512 0 PATCHES ${PATCHES})
`, "conditional.patch")

	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "x"})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	u := findUndecidable(res, "conditional.patch")
	if u == nil {
		t.Fatalf("expected conditional.patch undecidable, got %+v", res)
	}
	if len(u.UnresolvedVars) != 1 || u.UnresolvedVars[0] != "SOME_UNKNOWN_FLAG" {
		t.Errorf("unresolved_vars = %v, want [SOME_UNKNOWN_FLAG]", u.UnresolvedVars)
	}
	if len(res.Applied) != 0 || len(res.ConditionalNotApplied) != 0 {
		t.Errorf("expected zero applied/conditional_not_applied, got applied=%v conditional=%v", res.Applied, res.ConditionalNotApplied)
	}
}

// --- Input-validation / environment-condition edge cases -------------------

func TestApplyOrder_EmptyPortDir_Failed(t *testing.T) {
	res := ApplyOrder(Args{PortDir: "  ", Triplet: "x64-windows"})
	if res.Status != evidence.StatusFailed || res.Reason != ReasonEmptyPortDir {
		t.Errorf("status/reason = %v/%v, want failed/empty_port_dir", res.Status, res.Reason)
	}
}

func TestApplyOrder_EmptyTriplet_Failed(t *testing.T) {
	res := ApplyOrder(Args{PortDir: t.TempDir(), Triplet: ""})
	if res.Status != evidence.StatusFailed || res.Reason != ReasonEmptyTriplet {
		t.Errorf("status/reason = %v/%v, want failed/empty_triplet", res.Status, res.Reason)
	}
}

func TestApplyOrder_PortDirMissing_Unknown(t *testing.T) {
	res := ApplyOrder(Args{PortDir: filepath.Join(t.TempDir(), "does-not-exist"), Triplet: "x64-windows"})
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPortDirMissing {
		t.Errorf("status/reason = %v/%v, want unknown/port_dir_missing", res.Status, res.Reason)
	}
}

func TestApplyOrder_PortfileUnreadable_Unknown(t *testing.T) {
	dir := t.TempDir() // exists, but no portfile.cmake inside
	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPortfileUnreadable {
		t.Errorf("status/reason = %v/%v, want unknown/portfile_unreadable", res.Status, res.Reason)
	}
}

func TestApplyOrder_UnbalancedParens_Unparsable(t *testing.T) {
	portDir := writeFixture(t, `
vcpkg_from_github(
    REPO a/b
    PATCHES a.patch
`) // deliberately missing the closing paren
	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows"})
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPatchesExprUnparsable {
		t.Errorf("status/reason = %v/%v, want unknown/patches_expression_unparsable", res.Status, res.Reason)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Small direct unit tests for the Kleene logic + triplet derivation -----

func TestKleeneLogic_TruthTables(t *testing.T) {
	cases := []struct {
		name string
		fn   func() Tri
		want Tri
	}{
		{"and_true_true", func() Tri { return kleeneAnd(TriTrue, TriTrue) }, TriTrue},
		{"and_true_false", func() Tri { return kleeneAnd(TriTrue, TriFalse) }, TriFalse},
		{"and_unknown_false", func() Tri { return kleeneAnd(TriUnknown, TriFalse) }, TriFalse},
		{"and_unknown_true", func() Tri { return kleeneAnd(TriUnknown, TriTrue) }, TriUnknown},
		{"and_unknown_unknown", func() Tri { return kleeneAnd(TriUnknown, TriUnknown) }, TriUnknown},
		{"or_false_false", func() Tri { return kleeneOr(TriFalse, TriFalse) }, TriFalse},
		{"or_unknown_true", func() Tri { return kleeneOr(TriUnknown, TriTrue) }, TriTrue},
		{"or_unknown_false", func() Tri { return kleeneOr(TriUnknown, TriFalse) }, TriUnknown},
		{"not_true", func() Tri { return kleeneNot(TriTrue) }, TriFalse},
		{"not_unknown", func() Tri { return kleeneNot(TriUnknown) }, TriUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.fn(); got != c.want {
				t.Errorf("%s = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// --- CMake bracket-comment / bracket-argument / quoted-continuation shapes -
//
// Per cmake-language(7) (https://cmake.org/cmake/help/latest/manual/cmake-language.7.html):
// bracket comments and bracket arguments share the '[' '='* '[' ... ']' '='*
// ']' delimiter grammar, can span multiple lines, and match the CLOSE by
// equals-count (so a "]]" inside a "[=[...]=]" is just content, not a
// terminator); a bracket argument's content is never ${VAR}-expanded; and a
// quoted argument can continue across a line via a trailing backslash,
// which contributes NEITHER the backslash NOR the newline to the value.

func TestApplyOrder_BracketCommentDecoyIgnored(t *testing.T) {
	portDir := writeFixture(t, `
#[[
This whole block is a bracket comment, including a decoy statement that
must never be treated as real code:
vcpkg_from_github(REPO decoy/decoy REF v1 SHA512 0 PATCHES decoy.patch)
]]
vcpkg_from_github(REPO real/real REF v1 SHA512 0 PATCHES real.patch)
`, "real.patch", "decoy.patch")

	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "x"})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	if findApplied(res, "decoy.patch") != nil {
		t.Errorf("decoy.patch lives inside a bracket comment and must never be picked up, got %+v", res.Applied)
	}
	if findApplied(res, "real.patch") == nil {
		t.Errorf("expected real.patch applied, got %+v", res.Applied)
	}
	if len(res.Applied) != 1 {
		t.Errorf("expected exactly 1 applied entry (the decoy contributes zero), got %+v", res.Applied)
	}
	// decoy.patch physically exists on disk (created above) but is never
	// REFERENCED by real code, so it must show up as orphaned, not applied
	// and not missing.
	if findOrphaned(res, "decoy.patch") == nil {
		t.Errorf("expected decoy.patch orphaned (on disk, never referenced by real code), got %+v", res.Orphaned)
	}
}

func TestTokenize_BracketArgument_LiteralDoubleCloseBracketNotTerminator(t *testing.T) {
	toks := tokenize(`[=[abc]]def]=] tail`)
	if len(toks) != 2 {
		t.Fatalf("got %d tokens, want 2: %+v", len(toks), toks)
	}
	if !toks[0].Raw || !toks[0].Quoted {
		t.Errorf("bracket argument token should be Raw+Quoted, got %+v", toks[0])
	}
	want := "abc]]def"
	if toks[0].Text != want {
		t.Errorf("bracket content = %q, want %q — a literal ]] inside a [=[...]=] argument must not close it early", toks[0].Text, want)
	}
	if toks[1].Text != "tail" {
		t.Errorf("second token = %q, want %q", toks[1].Text, "tail")
	}
}

func TestApplyOrder_BracketArgumentNeverExpanded(t *testing.T) {
	portDir := writeFixture(t, `
vcpkg_from_github(REPO a/b REF v1 SHA512 0 PATCHES [[${NOT_A_REAL_VAR}.patch]])
`)
	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "x"})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("expected exactly 1 applied entry, got %+v", res.Applied)
	}
	wantLiteral := "${NOT_A_REAL_VAR}.patch"
	if res.Applied[0].Filename != wantLiteral {
		t.Errorf("filename = %q, want the LITERAL bracket content unexpanded: %q", res.Applied[0].Filename, wantLiteral)
	}
	if res.Applied[0].Existence == evidence.PresenceExists {
		t.Errorf("exists = true unexpectedly — the bracket content is a literal ${...} string that cannot match a real file; a true here would mean expansion silently happened")
	}
}

func TestApplyOrder_QuotedArgumentLineContinuation(t *testing.T) {
	// PATCHES "cont\<newline>inued.patch" — a quoted argument split across
	// two physical lines via a trailing backslash. Built via concatenation
	// (not a Go escape literal) so the actual byte sequence under test —
	// backslash immediately followed by a real newline — is unambiguous.
	portfile := `vcpkg_from_github(REPO a/b REF v1 SHA512 0 PATCHES "cont\` + "\n" + `inued.patch")` + "\n"
	portDir := writeFixture(t, portfile, "continued.patch")

	res := ApplyOrder(Args{PortDir: portDir, Triplet: "x64-windows", PortName: "x"})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok; result=%+v", res.Status, res)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("expected exactly 1 applied entry, got %+v", res.Applied)
	}
	if res.Applied[0].Filename != "continued.patch" {
		t.Errorf("filename = %q, want %q — the continuation must join the two fragments with nothing in between (no backslash, no newline)", res.Applied[0].Filename, "continued.patch")
	}
	if res.Applied[0].Existence != evidence.PresenceExists {
		t.Errorf("expected continued.patch to exist on disk")
	}
}

func TestUnescapeQuoted_EncodedEscapesAndContinuation(t *testing.T) {
	if got := unescapeQuoted(`a\tb`); got != "a\tb" {
		t.Errorf(`\t: got %q, want a literal TAB byte`, got)
	}
	if got := unescapeQuoted(`a\nb`); got != "a\nb" {
		t.Errorf(`\n: got %q, want a literal LF byte`, got)
	}
	if got := unescapeQuoted(`a\rb`); got != "a\rb" {
		t.Errorf(`\r: got %q, want a literal CR byte`, got)
	}
	if got := unescapeQuoted("a\\" + "\n" + "b"); got != "ab" {
		t.Errorf("quoted_continuation: got %q, want %q (backslash+newline drop entirely, not replaced by a newline)", got, "ab")
	}
	if got := unescapeQuoted(`a\"b`); got != `a"b` {
		t.Errorf(`escape_identity: got %q, want a"b`, got)
	}
}

// --- Triplet facts come from the triplet FILE, never from its name --------

// writeTripletDir creates an overlay-triplets root containing one triplet
// file with the given content.
func writeTripletDir(t *testing.T, triplet, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, triplet+".cmake"), []byte(content), 0o644); err != nil {
		t.Fatalf("write triplet file: %v", err)
	}
	return dir
}

// staticGuardPortfile guards a patch on VCPKG_LIBRARY_LINKAGE being static.
const staticGuardPortfile = `
if(VCPKG_LIBRARY_LINKAGE STREQUAL "static")
  list(APPEND PATCHES static-only.patch)
endif()
vcpkg_from_github(PATCHES ${PATCHES})
`

// TestApplyOrder_CustomTripletFileEstablishesLinkage is the mutation proof
// for F4. The triplet is named "corp-windows" — no "static" component — but
// its FILE sets VCPKG_LIBRARY_LINKAGE static. The name-derived model
// asserted "dynamic" and reported the static-only patch as not applied; the
// file-derived model gets it right.
func TestApplyOrder_CustomTripletFileEstablishesLinkage(t *testing.T) {
	portDir := writeFixture(t, staticGuardPortfile, "static-only.patch")
	tripletDir := writeTripletDir(t, "corp-windows", `
set(VCPKG_TARGET_ARCHITECTURE x64)
set(VCPKG_CRT_LINKAGE dynamic)
set(VCPKG_LIBRARY_LINKAGE static)
`)

	res := applyOrder(Args{
		PortDir:         portDir,
		Triplet:         "corp-windows",
		OverlayTriplets: []string{tripletDir},
	}, DefaultDeps())

	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v reason = %v, want ok; result = %+v", res.Status, res.Reason, res)
	}
	if res.TripletFile == "" {
		t.Fatal("triplet_file must name the file the facts were read from")
	}
	applied := findApplied(res, "static-only.patch")
	if applied == nil {
		t.Fatalf("static-only.patch not in applied — the triplet FILE sets VCPKG_LIBRARY_LINKAGE static, "+
			"whatever the triplet NAME suggests; applied=%v undecidable=%+v conditional=%+v",
			filenames(res.Applied), res.Undecidable, res.ConditionalNotApplied)
	}
	if applied.Existence != evidence.PresenceExists {
		t.Errorf("existence = %v, want exists", applied.Existence)
	}
}

// TestApplyOrder_NoTripletFile_LeavesGuardUndecidable is the other half of
// F4: with no triplet file reachable, VCPKG_LIBRARY_LINKAGE is genuinely
// unknown. The contract's answer is the undecidable bucket — NOT a guess
// derived from the triplet name, in either direction.
func TestApplyOrder_NoTripletFile_LeavesGuardUndecidable(t *testing.T) {
	portDir := writeFixture(t, staticGuardPortfile, "static-only.patch")

	// "x64-windows-static" is the name that most strongly invites the guess.
	res := applyOrder(Args{
		PortDir: portDir,
		Triplet: "x64-windows-static",
	}, DefaultDeps())

	if res.TripletFile != "" {
		t.Fatalf("triplet_file = %q, want empty (none was supplied)", res.TripletFile)
	}
	if findApplied(res, "static-only.patch") != nil {
		t.Fatal("static-only.patch was reported APPLIED from the triplet NAME alone — " +
			"no triplet file was read, so VCPKG_LIBRARY_LINKAGE is unknown")
	}
	if findConditional(res, "static-only.patch") != nil {
		t.Fatal("static-only.patch was reported NOT-APPLIED from the triplet name alone")
	}
	if findUndecidable(res, "static-only.patch") == nil {
		t.Fatalf("static-only.patch must be undecidable; result = %+v", res)
	}
}

// TestApplyOrder_VarOverridesStillWinOverTripletFile keeps the documented
// escape hatch working: an explicit caller override is a deliberate what-if
// and outranks the file.
func TestApplyOrder_VarOverridesStillWinOverTripletFile(t *testing.T) {
	portDir := writeFixture(t, staticGuardPortfile, "static-only.patch")
	tripletDir := writeTripletDir(t, "corp-windows", "set(VCPKG_LIBRARY_LINKAGE dynamic)\n")

	res := applyOrder(Args{
		PortDir:         portDir,
		Triplet:         "corp-windows",
		OverlayTriplets: []string{tripletDir},
		VarOverrides:    map[string]string{"VCPKG_LIBRARY_LINKAGE": "static"},
	}, DefaultDeps())

	if findApplied(res, "static-only.patch") == nil {
		t.Fatalf("explicit var_overrides must outrank the triplet file; result = %+v", res)
	}
}

// TestParseTripletFacts_ConditionalSetIsNotAFact: triplet files routinely
// carry port-specific overrides inside if() blocks. Whether such a branch
// runs depends on state this static evaluation does not have, so a variable
// set there must NOT be asserted for every port.
func TestParseTripletFacts_ConditionalSetIsNotAFact(t *testing.T) {
	facts := parseTripletFacts(`
set(VCPKG_CRT_LINKAGE dynamic)
if(PORT MATCHES "qt")
  set(VCPKG_LIBRARY_LINKAGE dynamic)
endif()
`, `C:\ports\foo`, "foo", "")

	if got, ok := facts["VCPKG_CRT_LINKAGE"]; !ok || got != "dynamic" {
		t.Errorf("VCPKG_CRT_LINKAGE = %q ok=%v, want the top-level set() to establish it", got, ok)
	}
	if got, ok := facts["VCPKG_LIBRARY_LINKAGE"]; ok {
		t.Errorf("VCPKG_LIBRARY_LINKAGE = %q was asserted from inside an if() block — "+
			"that branch's execution is unknown here, so the variable must stay unresolved", got)
	}
}

// TestParseTripletFacts_UnresolvedExpansionIsDropped: a half-expanded value
// is not a fact.
func TestParseTripletFacts_UnresolvedExpansionIsDropped(t *testing.T) {
	facts := parseTripletFacts("set(VCPKG_CHAINLOAD_TOOLCHAIN_FILE ${SOME_UNKNOWN}/tc.cmake)\n",
		`C:\ports\foo`, "foo", "")
	if got, ok := facts["VCPKG_CHAINLOAD_TOOLCHAIN_FILE"]; ok {
		t.Errorf("value %q retained an unresolved reference and must not be reported as a fact", got)
	}
}

// TestResolveTripletFile_OverlayPrecedesBuiltin mirrors vcpkg's own lookup
// order: --overlay-triplets first, then <root>/triplets, then community/.
func TestResolveTripletFile_OverlayPrecedesBuiltin(t *testing.T) {
	overlay := writeTripletDir(t, "cl", "set(VCPKG_LIBRARY_LINKAGE static)\n")

	root := t.TempDir()
	builtin := filepath.Join(root, "triplets")
	if err := os.MkdirAll(builtin, 0o755); err != nil {
		t.Fatalf("mkdir builtin triplets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(builtin, "cl.cmake"),
		[]byte("set(VCPKG_LIBRARY_LINKAGE dynamic)\n"), 0o644); err != nil {
		t.Fatalf("write builtin triplet: %v", err)
	}

	got, presence, err := resolveTripletFile(DefaultDeps(), "cl", []string{overlay}, root)
	if presence != evidence.PresenceExists || err != nil {
		t.Fatalf("presence = %v err = %v, want exists", presence, err)
	}
	if got != filepath.Join(overlay, "cl.cmake") {
		t.Fatalf("resolved %q, want the OVERLAY file to win over the builtin one", got)
	}

	// With no overlay supplied, the builtin root is used.
	got, presence, _ = resolveTripletFile(DefaultDeps(), "cl", nil, root)
	if presence != evidence.PresenceExists || got != filepath.Join(builtin, "cl.cmake") {
		t.Fatalf("resolved %q (%v), want the builtin triplets file", got, presence)
	}
}

// TestResolveTripletFile_TraversingNameFindsNothing: a triplet name is
// joined into a lookup path, so it must be bounded to one safe segment —
// the same class of guard as the port-name rule in the lastfailure package.
func TestResolveTripletFile_TraversingNameFindsNothing(t *testing.T) {
	overlay := t.TempDir()
	outside := filepath.Join(filepath.Dir(overlay), "escaped")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "evil.cmake"),
		[]byte("set(VCPKG_LIBRARY_LINKAGE static)\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, presence, _ := resolveTripletFile(DefaultDeps(),
		filepath.Join("..", "escaped", "evil"), []string{overlay}, "")
	if presence != evidence.PresenceAbsent || got != "" {
		t.Fatalf("resolved %q (%v) for a traversing triplet name; want absent — "+
			"the name must never escape the supplied roots", got, presence)
	}
}

// TestApplyOrder_RelativeOverlayTripletIsIgnored: a relative overlay root
// would bind to the daemon's working directory, so it can only produce a
// confident answer about an unrelated tree.
func TestApplyOrder_RelativeOverlayTripletIsIgnored(t *testing.T) {
	cands := tripletFileCandidates("cl", []string{"overlays/triplets"}, "")
	if len(cands) != 0 {
		t.Fatalf("candidates = %v, want none — a relative overlay root is not usable", cands)
	}
}

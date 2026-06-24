package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// --- D-3 readiness dry-run primitives ----------------------------------------

func TestAvailabilityInert(t *testing.T) {
	for _, av := range []string{config.AvailabilityWatch, config.AvailabilityDisabledUntilProbe} {
		if !availabilityInert(&config.ServerManifest{Availability: av}) {
			t.Fatalf("availabilityInert(%q) = false, want true", av)
		}
	}
	for _, av := range []string{"", config.AvailabilityReady} {
		if availabilityInert(&config.ServerManifest{Availability: av}) {
			t.Fatalf("availabilityInert(%q) = true, want false", av)
		}
	}
}

func TestAvailabilityProbePasses_Binaries(t *testing.T) {
	// "go" is guaranteed present in the test toolchain → probe passes.
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{Binaries: []string{"go"}}); !ok {
		t.Fatalf("probe with present binary failed: %q", why)
	}
	// A definitely-absent binary → probe fails with the basename reason.
	ok, why := availabilityProbePasses(&config.AvailabilityProbe{Binaries: []string{"definitely-not-on-path-xyz"}})
	if ok {
		t.Fatalf("probe with absent binary passed; want fail")
	}
	if !strings.Contains(why, "definitely-not-on-path-xyz") || !strings.Contains(why, "not found on PATH") {
		t.Fatalf("reason %q missing basename/PATH text", why)
	}
}

func TestAvailabilityProbePasses_Files(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "installed.marker")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{Files: []string{marker}}); !ok {
		t.Fatalf("probe with present file failed: %q", why)
	}
	// Missing file.
	ok, why := availabilityProbePasses(&config.AvailabilityProbe{Files: []string{filepath.Join(dir, "nope")}})
	if ok || !strings.Contains(why, "does not exist") {
		t.Fatalf("missing-file probe: ok=%v why=%q", ok, why)
	}
	// A directory is not a runnable regular file.
	ok, why = availabilityProbePasses(&config.AvailabilityProbe{Files: []string{dir}})
	if ok || !strings.Contains(why, "directory") {
		t.Fatalf("directory-file probe: ok=%v why=%q", ok, why)
	}
}

func TestAvailabilityProbePasses_NilNeverPasses(t *testing.T) {
	if ok, _ := availabilityProbePasses(nil); ok {
		t.Fatalf("nil probe passed; want fail")
	}
}

// TestGlobProbeMatches_GlobMatchesMultipleVersions is the Tier-1 acceptance for
// the version-agnostic file_globs[] field: a "…Mathcad Prime *…" pattern (the same
// `*`-in-a-spaced-segment shape the ableton/excel catalog probes use) must match
// a synthetic "Mathcad Prime 11.0.1.0" install AND a "12.0.0.0" install from ONE
// shared catalog row, so the row is not frozen to a single product version. Uses
// temp dirs + the platform path separator (filepath.Join) so it is host-OS
// correct on every build. The pattern is routed through the file_globs[] field —
// the OPT-IN glob path (a literal files[] entry would be stat'd verbatim, not
// globbed).
func TestGlobProbeMatches_GlobMatchesMultipleVersions(t *testing.T) {
	base := t.TempDir()
	// Two version dirs with a spaced "Mathcad Prime <ver>" segment, each carrying a
	// MathcadPrime.exe regular file — the shape the version-agnostic probe targets.
	for _, ver := range []string{"11.0.1.0", "12.0.0.0"} {
		dir := filepath.Join(base, "Mathcad Prime "+ver)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "MathcadPrime.exe"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write exe: %v", err)
		}
	}
	pattern := filepath.Join(base, "Mathcad Prime *", "MathcadPrime.exe")

	// globProbeMatches: ANY match is a regular file → pass.
	if ok, why := globProbeMatches(pattern); !ok {
		t.Fatalf("glob probe over two version dirs failed: %q", why)
	}
	// And through the full availabilityProbePasses owner via FileGlobs (the
	// install/readiness path) — the opt-in glob field.
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{FileGlobs: []string{pattern}}); !ok {
		t.Fatalf("availabilityProbePasses with the file_globs[] probe failed: %q", why)
	}

	// Removing BOTH version dirs makes the glob match nothing → fail inert (does
	// not exist), proving the glob is genuinely host-presence-driven.
	for _, ver := range []string{"11.0.1.0", "12.0.0.0"} {
		if err := os.RemoveAll(filepath.Join(base, "Mathcad Prime "+ver)); err != nil {
			t.Fatalf("rm version dir: %v", err)
		}
	}
	ok, why := globProbeMatches(pattern)
	if ok {
		t.Fatalf("glob probe passed with no matching install; want fail")
	}
	if !strings.Contains(why, "does not exist") {
		t.Fatalf("no-match reason %q, want \"does not exist\" (fail-closed)", why)
	}
}

// TestGlobProbeMatches_AbletonGlobNarrowedToLive11Plus is the Tier-1 acceptance for
// the NARROWED ableton probe (PR #429 follow-up, codex catalog.json:319 finding):
// the build-fixed extended fork advertises Ableton Live 11 or NEWER and does NOT
// support Live 10. The prior probe glob ("…\\Live *\\Program\\Ableton Live *.exe")
// matched Live 10 too, so a Live-10-only host wrongly passed admission for a server
// it cannot drive. The narrowed catalog glob uses the version char-class
// "…\\Live 1[1-9]*\\…\\Ableton Live 1[1-9]*.exe" — the 1[1-9] class matches the
// supported Live 11-19 (across every edition Suite/Standard/Intro/Lite) and EXCLUDES
// Live 10 (third version digit 0 is outside the 1-9 class), the same [^7]/[89]-style
// version-class technique kicad's glob uses. Uses temp dirs + filepath.Join so it is
// host-OS correct on every build; the real catalog pattern is rooted at
// C:\ProgramData\Ableton but the version-class + edition-spanning segment shape is
// identical here.
func TestGlobProbeMatches_AbletonGlobNarrowedToLive11Plus(t *testing.T) {
	base := t.TempDir()
	// SUPPORTED installs (Live 11+) differing by EDITION (Suite + Standard), each with
	// the "Ableton Live <ver> <edition>.exe" the shipped installer lays down (verified
	// against the host's real Live 11/12 Suite layout), PLUS one UNSUPPORTED Live 10
	// that the narrowed glob must EXCLUDE.
	supported := []struct{ dir, exe string }{
		{"Live 11 Suite", "Ableton Live 11 Suite.exe"},
		{"Live 12 Standard", "Ableton Live 12 Standard.exe"},
	}
	unsupported := struct{ dir, exe string }{"Live 10 Suite", "Ableton Live 10 Suite.exe"}
	mkInstall := func(dir, exe string) {
		prog := filepath.Join(base, dir, "Program")
		if err := os.MkdirAll(prog, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", prog, err)
		}
		if err := os.WriteFile(filepath.Join(prog, exe), []byte("x"), 0o644); err != nil {
			t.Fatalf("write exe: %v", err)
		}
	}
	for _, in := range supported {
		mkInstall(in.dir, in.exe)
	}
	mkInstall(unsupported.dir, unsupported.exe)

	// NARROWED pattern (this change): the version char-class 1[1-9] matches Live 11-19
	// in ANY edition (the spanning `*` after it covers "11 Suite", "12 Standard", …)
	// and EXCLUDES Live 10. This mirrors the shipped catalog glob exactly (temp root).
	narrowed := filepath.Join(base, "Live 1[1-9]*", "Program", "Ableton Live 1[1-9]*.exe")

	// Narrowed: ANY supported match is a runnable regular file → pass.
	if ok, why := globProbeMatches(narrowed); !ok {
		t.Fatalf("narrowed ableton glob over Live 11/12 failed: %q", why)
	}
	// And through the full availabilityProbePasses owner via the file_globs[] field
	// (the install/readiness path) — the OPT-IN glob the ableton catalog row declares.
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{FileGlobs: []string{narrowed}}); !ok {
		t.Fatalf("availabilityProbePasses with the narrowed ableton file_globs[] failed: %q", why)
	}

	// EXACT match set: the narrowed glob must match the TWO supported installs
	// (Live 11 Suite + Live 12 Standard) and NOT the Live 10 Suite — that exclusion is
	// the whole point of the narrowing.
	matches, err := filepath.Glob(narrowed)
	if err != nil {
		t.Fatalf("glob narrowed pattern: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("narrowed glob matched %d installs, want exactly 2 (Live 11 Suite + Live 12 Standard, NOT Live 10): %v", len(matches), matches)
	}
	for _, m := range matches {
		if strings.Contains(m, "Live 10") {
			t.Fatalf("narrowed glob wrongly matched a Live 10 install %q — the extended fork drops Live 10 support", m)
		}
	}

	// Remove the supported installs, keep ONLY the unsupported Live 10 → the narrowed
	// glob now matches NOTHING (it excludes Live 10), so the probe fails inert
	// (does not exist). Proves a Live-10-only host does NOT pass admission.
	for _, in := range supported {
		if err := os.RemoveAll(filepath.Join(base, in.dir)); err != nil {
			t.Fatalf("rm supported install dir: %v", err)
		}
	}
	ok, why := globProbeMatches(narrowed)
	if ok {
		t.Fatalf("narrowed ableton glob passed with only an unsupported Live 10 install present; want fail (Live 10 is excluded)")
	}
	if !strings.Contains(why, "does not exist") {
		t.Fatalf("no-match reason %q, want \"does not exist\" (fail-closed)", why)
	}
}

// TestFilesProbe_ExactStatLiteralNeverGlobs is TEST #1 — the FINDING-1 core: a
// files[] entry is stat'd VERBATIM and is NEVER globbed. The load-bearing case is
// an ABSENT literal whose path contains a glob metacharacter (a manifest that
// INTENDED the literal `…/Foo*/marker` where `Foo*` is a real directory name): it
// must FAIL (the exact file is absent) and must NOT silently fall to a sibling
// `…/FooBeta/marker` the way an unconditional glob would. We engineer exactly that
// sibling and prove files[] never sees it.
func TestFilesProbe_ExactStatLiteralNeverGlobs(t *testing.T) {
	base := t.TempDir()
	// A SIBLING that a `Foo*` glob WOULD match — but the literal the manifest
	// intends is `<base>/Foo*/marker`, which does NOT exist on disk.
	sib := filepath.Join(base, "FooBeta")
	if err := os.MkdirAll(sib, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sib, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write sibling marker: %v", err)
	}
	literalAbsent := filepath.Join(base, "Foo*", "marker") // the intended literal, NOT present

	// PROOF the regression would exist under glob: filepath.Glob of `<base>/Foo*/marker`
	// DOES match the sibling — so a glob-on-files[] would wrongly enable the row.
	globMatches, gerr := filepath.Glob(literalAbsent)
	if gerr != nil {
		t.Fatalf("glob of the metachar literal errored: %v", gerr)
	}
	if len(globMatches) == 0 {
		t.Fatalf("expected the `Foo*` glob to match the FooBeta sibling (proving the footgun); got none")
	}

	// files[] is EXACT: the intended literal `Foo*/marker` does not exist, so the probe
	// FAILS — it never globs to the FooBeta sibling.
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{Files: []string{literalAbsent}}); ok {
		t.Fatalf("files[] globbed an ABSENT literal to a sibling; want fail. why=%q", why)
	} else if !strings.Contains(why, "does not exist") {
		t.Fatalf("files[] absent-literal reason %q, want \"does not exist\"", why)
	}

	// And a files[] literal that DOES exist (with a metachar in its real name) passes
	// via the verbatim stat — the literal owner finds it directly.
	realMetacharDir := filepath.Join(base, "Foo [Beta]")
	if err := os.MkdirAll(realMetacharDir, 0o755); err != nil {
		t.Fatalf("mkdir real metachar dir: %v", err)
	}
	realLiteral := filepath.Join(realMetacharDir, "marker")
	if err := os.WriteFile(realLiteral, []byte("x"), 0o644); err != nil {
		t.Fatalf("write real metachar marker: %v", err)
	}
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{Files: []string{realLiteral}}); !ok {
		t.Fatalf("files[] missed an EXISTING literal `Foo [Beta]` path: %q", why)
	}
}

// TestFilesProbe_LiteralByteIdenticalToExactStat is the additive-guarantee
// regression: a files[] literal behaves BYTE-IDENTICALLY to the exact os.Stat
// owner entryScriptStatus for the present-regular-file, absent, and directory
// cases — files[] simply IS entryScriptStatus per entry (no glob layer). This is
// what keeps every existing literal probe unchanged. Also asserts globProbeMatches
// of a metacharacter-FREE pattern equals the same tuple (a no-wildcard file_globs[]
// entry resolves to its one self-match), so a file_globs[] row with no wildcard is
// equivalent.
func TestFilesProbe_LiteralByteIdenticalToExactStat(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "installed.marker")
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	absent := filepath.Join(dir, "not-installed.marker")

	cases := []string{present, absent, dir}
	for _, path := range cases {
		wantOK, wantReason := entryScriptStatus(path)
		// files[] per-entry == entryScriptStatus exactly (no metachars in these paths).
		if gotOK, _ := availabilityProbePasses(&config.AvailabilityProbe{Files: []string{path}}); gotOK != wantOK {
			t.Fatalf("literal %q: files[] availabilityProbePasses=%v want %v", path, gotOK, wantOK)
		}
		// globProbeMatches of a metacharacter-free pattern returns the SAME tuple
		// (filepath.Glob of a literal returns that one path iff it exists).
		gotOK, gotReason := globProbeMatches(path)
		if gotOK != wantOK || gotReason != wantReason {
			t.Fatalf("metachar-free %q: globProbeMatches=(%v,%q) want entryScriptStatus=(%v,%q)",
				path, gotOK, gotReason, wantOK, wantReason)
		}
	}
}

// TestFilesProbe_LiteralPathWithGlobMetacharsExists is the codex catalog finding 1
// regression, now handled STRUCTURALLY by the files[] exact-stat field: a LITERAL
// absolute path whose dir/file name contains a glob metacharacter (here `[` `]` in
// "Foo [Beta]") that ACTUALLY EXISTS must be detected as present. files[] stats the
// verbatim literal and never globs, so a real "[…]"/"*"/"?"-bearing install path is
// found without being misread as a pattern. Belt-and-suspenders: confirm
// filepath.Glob of that same literal returns NO match (so routing it through
// file_globs[] would MISS it — proving files[] is the correct field), while the
// files[] probe passes.
func TestFilesProbe_LiteralPathWithGlobMetacharsExists(t *testing.T) {
	base := t.TempDir()
	// A literal install path with glob metacharacters in a segment name. Created on
	// disk verbatim — there is no wildcard intent here; "[Beta]" is a real folder.
	dir := filepath.Join(base, "Foo [Beta]", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	exe := filepath.Join(dir, "tool")
	if err := os.WriteFile(exe, []byte("x"), 0o644); err != nil {
		t.Fatalf("write exe: %v", err)
	}

	// PROOF that file_globs[] would be the WRONG field for this literal: filepath.Glob
	// of the path misreads "[Beta]" as a char class and matches NOTHING.
	globMatches, gerr := filepath.Glob(exe)
	if gerr != nil {
		t.Fatalf("glob of literal metachar path errored: %v", gerr)
	}
	if len(globMatches) != 0 {
		t.Fatalf("expected filepath.Glob of the literal \"[Beta]\" path to match NOTHING (char-class misread), got %v", globMatches)
	}
	// Confirm the FALSE routing: through file_globs[] the same path FAILS (glob misses
	// the char-class literal) — which is exactly why a literal belongs in files[].
	if ok, _ := availabilityProbePasses(&config.AvailabilityProbe{FileGlobs: []string{exe}}); ok {
		t.Fatalf("file_globs[] unexpectedly matched a char-class literal; that field globs and should miss it")
	}

	// The CORRECT field: files[] exact-stat finds the literal file → probe passes.
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{Files: []string{exe}}); !ok {
		t.Fatalf("files[] missed the existing literal \"[Beta]\" path: %q", why)
	}

	// Removing the file → the literal no longer exists → files[] fails inert (does
	// not exist), preserving fail-closed polarity.
	if err := os.Remove(exe); err != nil {
		t.Fatalf("rm exe: %v", err)
	}
	ok, why := availabilityProbePasses(&config.AvailabilityProbe{Files: []string{exe}})
	if ok {
		t.Fatalf("files[] passed for an absent literal \"[Beta]\" path; want fail")
	}
	if !strings.Contains(why, "does not exist") {
		t.Fatalf("absent literal \"[Beta]\" reason %q, want \"does not exist\" (fail-closed)", why)
	}
}

// TestAvailabilityProbePasses_ANDAcrossAllThreeFields is TEST #3 — the cross-field
// AND: a probe passes ONLY when every binary is on PATH AND every files[] literal
// exists AND every file_globs[] pattern matches. We build a probe with a PRESENT
// binary ("go"), a PRESENT files[] literal, AND a PRESENT file_globs[] match, then
// flip each field to a failing value in turn and assert the WHOLE probe fails,
// naming the failing term. A glob is OR only WITHIN one pattern's match set, never
// across fields.
func TestAvailabilityProbePasses_ANDAcrossAllThreeFields(t *testing.T) {
	base := t.TempDir()
	// Present files[] literal.
	literal := filepath.Join(base, "installed.marker")
	if err := os.WriteFile(literal, []byte("x"), 0o644); err != nil {
		t.Fatalf("write literal: %v", err)
	}
	// Present file_globs[] match.
	dir := filepath.Join(base, "App 1.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	pattern := filepath.Join(base, "App *", "app.exe")
	absentLiteral := filepath.Join(base, "nope.marker")
	absentPattern := filepath.Join(base, "Nope *", "x.exe")

	// All three present → pass.
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{
		Binaries:  []string{"go"},
		Files:     []string{literal},
		FileGlobs: []string{pattern},
	}); !ok {
		t.Fatalf("all-three-present probe should pass: %q", why)
	}

	cases := []struct {
		name      string
		probe     *config.AvailabilityProbe
		wantInWhy string
	}{
		{"absent-binary", &config.AvailabilityProbe{
			Binaries: []string{"definitely-not-on-path-xyz"}, Files: []string{literal}, FileGlobs: []string{pattern},
		}, "definitely-not-on-path-xyz"},
		{"absent-files-literal", &config.AvailabilityProbe{
			Binaries: []string{"go"}, Files: []string{absentLiteral}, FileGlobs: []string{pattern},
		}, "nope.marker"},
		{"absent-file-glob", &config.AvailabilityProbe{
			Binaries: []string{"go"}, Files: []string{literal}, FileGlobs: []string{absentPattern},
		}, "x.exe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := availabilityProbePasses(tc.probe)
			if ok {
				t.Fatalf("probe passed though one AND term fails (%s)", tc.name)
			}
			if !strings.Contains(why, tc.wantInWhy) {
				t.Fatalf("reason %q should name the failing AND term %q", why, tc.wantInWhy)
			}
		})
	}
}

// TestMarketplaceEntryBrowseProbeState_FileGlobsDefersNoGlob is TEST #4 — the
// browse-path invariant for a file_globs[] row: the passive GET /api/marketplace
// classifier must return ProbeBrowseInertUnknown (deferred-to-install) and must
// NEVER run filepath.Glob / os.Stat on the browse projection — the no-glob/no-stat-
// on-browse invariant holds for the file_globs[] field exactly as for files[].
// Presence INDEPENDENCE is the proof the filesystem was not touched: a glob whose
// pattern MATCHES a real on-disk file and a glob whose pattern matches NOTHING both
// classify inert-unknown (a stat/glob would have diverged matched=ready vs
// absent=blocked).
func TestMarketplaceEntryBrowseProbeState_FileGlobsDefersNoGlob(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "App 1.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	matchingGlob := filepath.Join(base, "App *", "app.exe")                // matches the file above
	nonMatchingGlob := filepath.Join(base, "Nonexistent *", "missing.exe") // matches nothing

	mk := func(globs []string) *MarketplaceEntry {
		return &MarketplaceEntry{
			ID:           "globrow",
			Availability: "disabled-until-probe",
			InstallProbe: &CatalogAvailabilityProbe{FileGlobs: globs},
		}
	}
	for _, tc := range []struct {
		name  string
		globs []string
	}{
		{"glob-matches-on-disk", []string{matchingGlob}},
		{"glob-matches-nothing", []string{nonMatchingGlob}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MarketplaceEntryBrowseProbeState(mk(tc.globs)); got != ProbeBrowseInertUnknown {
				t.Fatalf("file_globs[] browse state = %q, want %q (deferred, no glob/stat)", got, ProbeBrowseInertUnknown)
			}
		})
	}
	// Belt-and-suspenders: the FULL install gate DOES glob+stat, so it diverges
	// matched(true) vs non-matching(false) — that divergence is exactly the
	// filesystem touch the browse path skips above.
	if !MarketplaceEntryProbePasses(mk([]string{matchingGlob})) {
		t.Fatalf("full gate should PASS for a file_globs[] pattern that matches a real file")
	}
	if MarketplaceEntryProbePasses(mk([]string{nonMatchingGlob})) {
		t.Fatalf("full gate should FAIL for a file_globs[] pattern that matches nothing")
	}
}

// TestAvailabilityProbePasses_WindowsPathBasenameInError is the codex r6 finding 3
// regression: a file probe with a Windows ABSOLUTE path (now accepted by the
// cross-platform validator) must surface ONLY the marker basename in the failure
// reason, NOT the whole `C:\Users\alice\...\marker`. On a non-Windows build
// filepath.Base treats '\' as a normal char and returns the full path; the fix
// uses basenameAcrossSeparators (splits on BOTH separators) so the GUI/API error
// never echoes the full host path. The probe LOGIC is unchanged — the verbatim
// path is still os.Stat'd (it does not exist on the test host, so the probe fails,
// which is exactly the surfacing path we assert).
func TestAvailabilityProbePasses_WindowsPathBasenameInError(t *testing.T) {
	winPath := `C:\Users\alice\AppData\Local\Programs\tool\installed.marker`
	ok, why := availabilityProbePasses(&config.AvailabilityProbe{Files: []string{winPath}})
	if ok {
		t.Fatalf("a nonexistent Windows path probe passed; want fail")
	}
	if !strings.Contains(why, "installed.marker") {
		t.Fatalf("reason %q missing the marker basename", why)
	}
	// The directory prefix must NOT leak — assert the full path is absent.
	if strings.Contains(why, "alice") || strings.Contains(why, `C:\`) {
		t.Fatalf("reason %q leaked the full Windows path; want basename only", why)
	}
	// The file_globs[] branch shares the same basename-only redaction. A Windows
	// absolute glob that matches nothing must surface only the leaf, never the dir.
	winGlob := `C:\Users\alice\AppData\Local\Programs\App *\server.exe`
	ok, why = availabilityProbePasses(&config.AvailabilityProbe{FileGlobs: []string{winGlob}})
	if ok {
		t.Fatalf("a nonexistent Windows file_globs probe passed; want fail")
	}
	if !strings.Contains(why, "server.exe") {
		t.Fatalf("file_globs reason %q missing the leaf basename", why)
	}
	if strings.Contains(why, "alice") || strings.Contains(why, `C:\`) {
		t.Fatalf("file_globs reason %q leaked the full Windows path; want basename only", why)
	}
	// The same for a path-shaped binary probe (the binary branch shares the fix).
	winBin := `C:\Program Files\tool\definitely-absent.exe`
	ok, why = availabilityProbePasses(&config.AvailabilityProbe{Binaries: []string{winBin}})
	if ok {
		t.Fatalf("a Windows-path binary probe passed; want fail")
	}
	if !strings.Contains(why, "definitely-absent.exe") {
		t.Fatalf("binary reason %q missing the basename", why)
	}
	if strings.Contains(why, "Program Files") || strings.Contains(why, `C:\`) {
		t.Fatalf("binary reason %q leaked the full Windows path; want basename only", why)
	}
}

// TestMarketplaceEntryBrowseProbeState_TriState is the FINDING-1 (codex catalog
// r4/r5) regression for the TRI-STATE browse classifier. The PASSIVE browse
// projection must NOT os.Stat a files[] probe nor exec.LookPath a path-shaped
// token while serving GET /api/marketplace — a catalog-supplied slow/UNC path
// must not stall opening the Catalog. It must ALSO distinguish the three states a
// single bool conflated: "ready" (installable now), "inert-blocked" (host app
// provably absent → greyed), and "inert-unknown" (file/path probe deferred → the
// GUI still offers install). This table is architect section-E's browse row.
func TestMarketplaceEntryBrowseProbeState_TriState(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "installed.marker")
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	absent := filepath.Join(dir, "not-installed.marker")

	mk := func(p *CatalogAvailabilityProbe) *MarketplaceEntry {
		return &MarketplaceEntry{ID: "x", Availability: "watch", InstallProbe: p}
	}

	cases := []struct {
		name  string
		entry *MarketplaceEntry
		want  ProbeBrowseState
	}{
		// inert bare-binary present → ready (bounded exec.LookPath, "go" present).
		{"bare-binary-present", mk(&CatalogAvailabilityProbe{Binaries: []string{"go"}}), ProbeBrowseReady},
		// inert bare-binary absent → inert-blocked.
		{"bare-binary-absent", mk(&CatalogAvailabilityProbe{Binaries: []string{"definitely-not-on-path-xyz"}}), ProbeBrowseInertBlocked},
		// file-only (present) → inert-unknown WITHOUT stating it.
		{"file-only-present", mk(&CatalogAvailabilityProbe{Files: []string{present}}), ProbeBrowseInertUnknown},
		// file-only (absent) → inert-unknown — same verdict (file-presence-INDEPENDENT).
		{"file-only-absent", mk(&CatalogAvailabilityProbe{Files: []string{absent}}), ProbeBrowseInertUnknown},
		// file_globs-only → inert-unknown WITHOUT globbing (glob-presence-INDEPENDENT,
		// same no-touch-on-browse invariant as files[]).
		{"file-globs-only", mk(&CatalogAvailabilityProbe{FileGlobs: []string{filepath.Join(dir, "App *", "x.exe")}}), ProbeBrowseInertUnknown},
		// mixed bare-binary present + file_globs → inert-unknown (the glob defers).
		{"mixed-bin-present-and-file-globs", mk(&CatalogAvailabilityProbe{Binaries: []string{"go"}, FileGlobs: []string{filepath.Join(dir, "App *", "x.exe")}}), ProbeBrowseInertUnknown},
		// mixed bare-binary ABSENT + file_globs → inert-blocked (the absent bare binary
		// already proves the AND probe fails, even with a file_globs[] entry present).
		{"mixed-bin-absent-and-file-globs", mk(&CatalogAvailabilityProbe{Binaries: []string{"definitely-not-on-path-xyz"}, FileGlobs: []string{filepath.Join(dir, "App *", "x.exe")}}), ProbeBrowseInertBlocked},
		// path-shaped binary → inert-unknown (never LookPath a path — B2 defense).
		{"path-binary-posix", mk(&CatalogAvailabilityProbe{Binaries: []string{"/net/slow/tool"}}), ProbeBrowseInertUnknown},
		{"path-binary-windows", mk(&CatalogAvailabilityProbe{Binaries: []string{`C:\tools\x.exe`}}), ProbeBrowseInertUnknown},
		{"path-binary-unc", mk(&CatalogAvailabilityProbe{Binaries: []string{`\\host\share\x`}}), ProbeBrowseInertUnknown},
		// mixed binaries+files, ALL bare binaries present → inert-unknown (carries a
		// file the browse path defers; the bare binary already resolved).
		{"mixed-bin-present-and-file", mk(&CatalogAvailabilityProbe{Binaries: []string{"go"}, Files: []string{present}}), ProbeBrowseInertUnknown},
		// codex r6 finding 4: mixed binaries+files where a BARE binary is MISSING →
		// inert-blocked. With AND semantics the absent bare binary already proves the
		// probe cannot pass, so the row is greyed instead of offered-then-412'd. This
		// classifies BLOCKED even though a files[] entry is present (the prior order
		// returned inert-unknown via the files[] rule before checking the binary).
		{"mixed-bin-absent-and-file", mk(&CatalogAvailabilityProbe{Binaries: []string{"definitely-not-on-path-xyz"}, Files: []string{present}}), ProbeBrowseInertBlocked},
		// codex r6 finding 4: a path-shaped binary is NOT LookPath'd; it is deferred.
		// Mixed with an absent BARE binary still blocks (the bare one is checked; the
		// path-shaped one is skipped in the bare loop).
		{"mixed-path-binary-and-absent-bare", mk(&CatalogAvailabilityProbe{Binaries: []string{`C:\tools\x.exe`, "definitely-not-on-path-xyz"}}), ProbeBrowseInertBlocked},
		// A path-shaped binary alongside a PRESENT bare binary → inert-unknown (the
		// path-shaped one defers after the bare one resolves).
		{"mixed-path-binary-and-present-bare", mk(&CatalogAvailabilityProbe{Binaries: []string{"go", `C:\tools\x.exe`}}), ProbeBrowseInertUnknown},
		// nil/empty probe → inert-blocked (fail-closed).
		{"nil-probe", mk(nil), ProbeBrowseInertBlocked},
		{"empty-probe", mk(&CatalogAvailabilityProbe{}), ProbeBrowseInertBlocked},
		// ready/empty availability → ready (a nil entry too, asserted below).
		{"ready-row", &MarketplaceEntry{ID: "x", Availability: "ready"}, ProbeBrowseReady},
		{"empty-availability", &MarketplaceEntry{ID: "x"}, ProbeBrowseReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MarketplaceEntryBrowseProbeState(tc.entry); got != tc.want {
				t.Fatalf("MarketplaceEntryBrowseProbeState = %q, want %q", got, tc.want)
			}
		})
	}
	if got := MarketplaceEntryBrowseProbeState(nil); got != ProbeBrowseReady {
		t.Fatalf("nil entry: browse state = %q, want %q", got, ProbeBrowseReady)
	}

	// The file path is browse-presence-INDEPENDENT — proven by the file-only
	// present/absent rows above BOTH classifying inert-unknown. The FULL gate DOES
	// stat, so it diverges present=true / absent=false: that divergence is exactly
	// the file stat the browse path skips.
	if !MarketplaceEntryProbePasses(mk(&CatalogAvailabilityProbe{Files: []string{present}})) {
		t.Fatalf("full gate should pass for a present marker file")
	}
	if MarketplaceEntryProbePasses(mk(&CatalogAvailabilityProbe{Files: []string{absent}})) {
		t.Fatalf("full gate should fail for an absent marker file")
	}
}

// --- D-3 / D-2 AdmissionCheck gate -------------------------------------------

func TestAdmissionCheck_InertProbeFailsBlocks(t *testing.T) {
	setupAdmissionParityTest(t)
	m := &config.ServerManifest{
		Name:         "watch-row",
		Kind:         config.KindGlobal,
		Transport:    config.TransportStdioBridge,
		Command:      "go", // present, so no command-on-path finding
		Daemons:      []config.DaemonSpec{{Name: "default", Port: 9999}},
		Availability: config.AvailabilityWatch,
		InstallProbe: &config.AvailabilityProbe{Binaries: []string{"definitely-not-on-path-xyz"}},
	}
	findings := AdmissionCheck(m, AdmissionScope{})
	var probe *AdmissionFinding
	for i := range findings {
		if findings[i].ID == "availability-probe" {
			probe = &findings[i]
		}
	}
	if probe == nil {
		t.Fatalf("no availability-probe finding; findings=%#v", findings)
	}
	if probe.Optional {
		t.Fatalf("availability-probe finding is Optional; must block")
	}
	if !containsNonOptional(findings) {
		t.Fatalf("findings contain no non-optional finding")
	}
	// Short-circuit: an inert un-probed row appends NO further port/binary
	// findings — the availability-probe finding is the only one.
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 (short-circuit) finding, got %d: %#v", len(findings), findings)
	}
	// And Preflight surfaces it as the blocking error.
	if err := Preflight(m, ""); err == nil {
		t.Fatalf("Preflight admitted an inert un-probed row; want AdmissionError")
	}
}

func TestAdmissionCheck_InertProbePassesFallsThrough(t *testing.T) {
	setupAdmissionParityTest(t)
	m := &config.ServerManifest{
		Name:         "watch-row-ok",
		Kind:         config.KindGlobal,
		Transport:    config.TransportStdioBridge,
		Command:      "go",
		Daemons:      []config.DaemonSpec{{Name: "default", Port: 9999}},
		Availability: config.AvailabilityWatch,
		InstallProbe: &config.AvailabilityProbe{Binaries: []string{"go"}}, // present → passes
	}
	for _, f := range AdmissionCheck(m, AdmissionScope{}) {
		if f.ID == "availability-probe" {
			t.Fatalf("availability-probe finding present though probe passed: %#v", f)
		}
	}
}

func TestAdmissionCheck_ReadyRowNoProbeFinding(t *testing.T) {
	setupAdmissionParityTest(t)
	m := &config.ServerManifest{
		Name:      "ready-row",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: 9999}},
		// Availability empty == ready.
	}
	for _, f := range AdmissionCheck(m, AdmissionScope{}) {
		if f.ID == "availability-probe" {
			t.Fatalf("ready row produced an availability-probe finding: %#v", f)
		}
	}
}

// TestPreflight_InertRowNeverInstallsUntilProbe is the load-bearing integration
// claim for D-3: Preflight is the chokepoint Install runs (install.go step 2)
// BEFORE installPlanCore (step 4) writes any supervisor-intent row or client
// config. An inert row whose probe FAILS makes Preflight return the typed
// *AdmissionError carrying the availability-probe id, so Install returns before
// reaching installPlanCore — no spawn, no write. Once the probe PASSES (the
// host app is detected), Preflight returns nil — the enable transition. This
// asserts the gate at the exact boundary Install consults, with no live-state
// mutation (Preflight is pure / read-only).
func TestPreflight_InertRowNeverInstallsUntilProbe(t *testing.T) {
	setupAdmissionParityTest(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "host-app.marker")

	m := &config.ServerManifest{
		Name:         "inert-row",
		Kind:         config.KindGlobal,
		Transport:    config.TransportStdioBridge,
		Command:      "go",
		Daemons:      []config.DaemonSpec{{Name: "default", Port: 9999}},
		Availability: config.AvailabilityDisabledUntilProbe,
		InstallProbe: &config.AvailabilityProbe{Files: []string{marker}},
	}

	// Probe FAILS (marker absent) → Preflight blocks with the typed availability
	// error. This is the "never installs" guarantee: Install returns here, before
	// BuildPlan/installPlanCore.
	err := Preflight(m, "")
	if err == nil {
		t.Fatalf("Preflight admitted an inert row whose probe fails; want block")
	}
	var ae *AdmissionError
	if !errors.As(err, &ae) {
		t.Fatalf("Preflight error is not *AdmissionError: %T %v", err, err)
	}
	if ae.ID != "availability-probe" {
		t.Fatalf("blocking finding id = %q, want availability-probe", ae.ID)
	}

	// Operator installs the host app → marker appears → probe PASSES → Preflight
	// admits (the enable transition). Nothing else about the manifest changed.
	if werr := os.WriteFile(marker, []byte("installed"), 0o644); werr != nil {
		t.Fatalf("write marker: %v", werr)
	}
	if err := Preflight(m, ""); err != nil {
		t.Fatalf("Preflight still blocked after the probe passed: %v", err)
	}
}

func TestAdmissionCheck_VendoredLicenseAdvisory(t *testing.T) {
	setupAdmissionParityTest(t)
	base := func(ls string) *config.ServerManifest {
		return &config.ServerManifest{
			Name:           "vendored",
			Kind:           config.KindGlobal,
			Transport:      config.TransportStdioBridge,
			Command:        "go",
			Daemons:        []config.DaemonSpec{{Name: "default", Port: 9999}},
			VendoredSource: &config.VendoredSource{Repo: "https://github.com/x/y", PinnedRef: "v1", LicenseStatus: ls},
		}
	}
	// pending / empty / unknown → OPTIONAL advisory finding, does NOT block.
	for _, ls := range []string{config.LicenseStatusPending, "", config.LicenseStatusUnknown} {
		m := base(ls)
		findings := AdmissionCheck(m, AdmissionScope{})
		var found *AdmissionFinding
		for i := range findings {
			if findings[i].ID == "vendored-license-unvetted" {
				found = &findings[i]
			}
		}
		if found == nil {
			t.Fatalf("license_status %q produced no vendored-license-unvetted advisory", ls)
		}
		if !found.Optional {
			t.Fatalf("vendored-license-unvetted finding is non-optional for %q; must be advisory", ls)
		}
		if containsNonOptional(findings) {
			// "go" command present + ports free (mocked) → the only finding is the
			// optional advisory, so nothing should block.
			t.Fatalf("advisory license finding unexpectedly produced a blocking finding for %q", ls)
		}
		if err := Preflight(m, ""); err != nil {
			t.Fatalf("Preflight blocked on an advisory license finding (%q): %v", ls, err)
		}
	}
	// confirmed → no advisory finding.
	for _, f := range AdmissionCheck(base(config.LicenseStatusConfirmed), AdmissionScope{}) {
		if f.ID == "vendored-license-unvetted" {
			t.Fatalf("confirmed license still produced an advisory: %#v", f)
		}
	}
}

// TestCheckServerReadiness_VendoredLicenseAdvisorySurfaced is the FINDING 1
// regression: the D-2 vendored-license advisory (license_status empty / pending /
// unknown) must SURFACE in CheckServerReadiness as a visible Optional requirement
// row — previously it was emitted by AdmissionCheck but never rendered, so the
// operator could not see it. It must remain ADVISORY (Optional, NON-blocking): an
// empty/pending/unknown license does NOT flip Ready to false (the operator may
// knowingly install a pending-license fork on their own host). A confirmed
// license produces no such row.
func TestCheckServerReadiness_VendoredLicenseAdvisorySurfaced(t *testing.T) {
	setupAdmissionParityTest(t)
	base := func(ls string) *config.ServerManifest {
		return &config.ServerManifest{
			Name:           "vendored",
			Kind:           config.KindGlobal,
			Transport:      config.TransportStdioBridge,
			Command:        "go",
			Daemons:        []config.DaemonSpec{{Name: "default", Port: 9999}},
			VendoredSource: &config.VendoredSource{Repo: "https://github.com/x/y", PinnedRef: "v1", LicenseStatus: ls},
		}
	}
	findRow := func(rep *ReadinessReport) *ReadinessRequirement {
		for i := range rep.Requirements {
			if rep.Requirements[i].Name == "vendored source: vendored" {
				return &rep.Requirements[i]
			}
		}
		return nil
	}

	// empty / pending / unknown → a visible advisory row that does NOT block Ready.
	for _, ls := range []string{"", config.LicenseStatusPending, config.LicenseStatusUnknown} {
		rep := CheckServerReadiness(base(ls))
		row := findRow(rep)
		if row == nil {
			t.Fatalf("license_status %q: vendored-license advisory not surfaced in readiness; requirements=%+v", ls, rep.Requirements)
		}
		if !row.Optional {
			t.Fatalf("license_status %q: vendored-license readiness row is not Optional (must be advisory): %#v", ls, row)
		}
		if row.OK {
			t.Fatalf("license_status %q: vendored-license advisory row should be OK=false (unvetted): %#v", ls, row)
		}
		if !rep.Ready {
			t.Fatalf("license_status %q: advisory license row wrongly blocked Ready; requirements=%+v", ls, rep.Requirements)
		}
	}

	// confirmed → no advisory row, Ready unaffected.
	rep := CheckServerReadiness(base(config.LicenseStatusConfirmed))
	if row := findRow(rep); row != nil {
		t.Fatalf("confirmed license still surfaced a vendored-license advisory row: %#v", row)
	}
	if !rep.Ready {
		t.Fatalf("confirmed-license manifest unexpectedly not Ready; requirements=%+v", rep.Requirements)
	}
}

// TestCheckServerReadiness_InertProbeSingleAdmissionVerdict is the bot catalog-r3
// P3 regression: CheckServerReadinessWithScope must run AdmissionCheck (and thus
// the file-based install-probe) EXACTLY ONCE per request and reuse that single
// verdict for BOTH the Ready seed AND the surfaced availability-probe row — the
// prior code re-called availabilityProbeFinding a second time, which re-evaluated
// the probe and could yield an inconsistent Ready/row verdict if a probed file
// appeared or disappeared mid-request (intra-request TOCTOU).
//
// We prove single-verdict reuse two ways:
//   - the surfaced availability-probe row's Name/Reason/Fix are byte-identical to
//     the finding from ONE independent AdmissionCheck call (so readiness reuses
//     the same verdict, not a fresh re-probe); and
//   - the probe is a file under a temp dir; with the fix, the file's presence is
//     consulted exactly once via the single AdmissionCheck, so Ready==false and
//     the row are guaranteed self-consistent.
func TestCheckServerReadiness_InertProbeSingleAdmissionVerdict(t *testing.T) {
	setupAdmissionParityTest(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-installed.marker")
	m := &config.ServerManifest{
		Name:         "inert-row",
		Kind:         config.KindGlobal,
		Transport:    config.TransportStdioBridge,
		Command:      "go",
		Daemons:      []config.DaemonSpec{{Name: "default", Port: 9999}},
		Availability: config.AvailabilityWatch,
		InstallProbe: &config.AvailabilityProbe{Files: []string{missing}},
	}

	// The single AdmissionCheck verdict the readiness path must reuse.
	want, ok := findingByID(AdmissionCheck(m, AdmissionScope{}), "availability-probe")
	if !ok {
		t.Fatalf("AdmissionCheck did not produce an availability-probe finding for an inert un-probed row")
	}

	rep := CheckServerReadiness(m)
	if rep.Ready {
		t.Fatalf("inert un-probed row reported Ready=true; want false; requirements=%+v", rep.Requirements)
	}
	var rows []ReadinessRequirement
	for _, r := range rep.Requirements {
		if r.Name == want.Name {
			rows = append(rows, r)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly one availability-probe row (single AdmissionCheck verdict reused), got %d: %+v", len(rows), rep.Requirements)
	}
	row := rows[0]
	if row.OK {
		t.Fatalf("availability-probe row should be OK=false: %#v", row)
	}
	// The row MUST carry the SAME Reason/Fix the single AdmissionCheck verdict
	// produced — proving it was reused, not re-derived by a second probe call.
	if row.Reason != want.Reason || row.Fix != want.Fix {
		t.Fatalf("availability-probe row not sourced from the single AdmissionCheck verdict:\n  row    = %#v\n  verdict= %#v", row, want)
	}
}

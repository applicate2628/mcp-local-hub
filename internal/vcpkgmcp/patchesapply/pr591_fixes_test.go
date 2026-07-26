package patchesapply

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// writePort writes portfile.cmake (plus any extra files) into a fresh temp
// port directory and returns its ABSOLUTE path.
func writePort(t *testing.T, portfile string, extraFiles ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "portfile.cmake"), []byte(portfile), 0o644); err != nil {
		t.Fatalf("write portfile: %v", err)
	}
	for _, f := range extraFiles {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("patch body\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	return dir
}

func undecidableNames(res Result) []string {
	var out []string
	for _, u := range res.Undecidable {
		out = append(out, u.Filename)
	}
	return out
}

func appliedNames(res Result) []string {
	var out []string
	for _, a := range res.Applied {
		out = append(out, a.Filename)
	}
	return out
}

// PR #591 P1 (walk.go handleSet): a set() under an UNDECIDED guard was
// installed into the scalar environment as plain text, with no record that its
// applicability was unknown. A patch reached through that scalar was then
// reported as a CERTAIN apply — and, if the file was absent, as a CERTAIN
// missing[] entry, i.e. a bug report against a port that may be perfectly
// fine. The list shape beside it already carried the tri-state; one shape
// honest and the other guessing is the worst of both.
func TestConditionallyAssignedScalarKeepsItsUnknownGuard(t *testing.T) {
	// The reference EMBEDS the variable rather than being exactly "${VAR}", so
	// it resolves through the SCALAR path (env.expand) instead of splicing the
	// list view — this is what isolates the scalar fix. The variable itself
	// resolves cleanly to "patches", so the entry is NOT forced undecidable by
	// the separate unresolved-path rule; the only thing that can make it
	// undecidable is the scalar's own guard.
	dir := writePort(t, `
if(SOME_UNDECIDABLE_VARIABLE)
    set(PATCH_DIR patches)
endif()
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO acme/widget
    PATCHES ${PATCH_DIR}/fix.patch
)
`)
	if err := os.MkdirAll(filepath.Join(dir, "patches"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "patches", "fix.patch"), []byte("body\n"), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}

	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})

	if got := appliedNames(res); len(got) != 0 {
		t.Fatalf("applied = %v, want none — a patch path built from a set() under an undecided if() is NOT a certain apply", got)
	}
	if got := undecidableNames(res); len(got) != 1 || got[0] != "${PATCH_DIR}/fix.patch" {
		t.Fatalf("undecidable = %v, want [${PATCH_DIR}/fix.patch] — the scalar shape must carry the same tri-state as a conditional list(APPEND)", got)
	}
	if len(res.Undecidable) == 1 {
		if !containsStr(res.Undecidable[0].UnresolvedVars, "SOME_UNDECIDABLE_VARIABLE") {
			t.Fatalf("unresolved_vars = %v, want it to name SOME_UNDECIDABLE_VARIABLE so the operator knows what to decide",
				res.Undecidable[0].UnresolvedVars)
		}
	}
}

// The equal-and-opposite guard: an UNCONDITIONAL set() must still produce a
// certain applied entry. Failing closed on every scalar would make the tool
// useless.
func TestUnconditionallyAssignedScalarStillApplies(t *testing.T) {
	dir := writePort(t, `
set(MY_PATCH fix.patch)
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO acme/widget
    PATCHES ${MY_PATCH}
)
`, "fix.patch")

	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if got := appliedNames(res); len(got) != 1 {
		t.Fatalf("applied = %v, want exactly the one unconditional patch", got)
	}
	if got := undecidableNames(res); len(got) != 0 {
		t.Fatalf("undecidable = %v, want none — an unconditional set() is certain", got)
	}
}

// A later UNCONDITIONAL set() genuinely does overwrite whatever a conditional
// one left, so the taint must be CLEARED rather than sticking to the name.
func TestUnconditionalReassignmentClearsAPriorConditionalTaint(t *testing.T) {
	dir := writePort(t, `
if(SOME_UNDECIDABLE_VARIABLE)
    set(MY_PATCH wrong.patch)
endif()
set(MY_PATCH fix.patch)
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO acme/widget
    PATCHES ${MY_PATCH}
)
`, "fix.patch")

	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if got := appliedNames(res); len(got) != 1 || got[0] != "fix.patch" {
		t.Fatalf("applied = %v, want [fix.patch] — an unconditional reassignment makes the entry certain again", got)
	}
	if got := undecidableNames(res); len(got) != 0 {
		t.Fatalf("undecidable = %v, want none — the prior conditional taint must be CLEARED, not stick to the name", got)
	}
}

// PR #591 P2 (lexer.go): "Command names are case-insensitive"
// (cmake-language(7)). walkPortfile's switch was case-SENSITIVE, so IF(...) /
// ENDIF() fell through to the default branch and were never treated as guards
// at all — every patch inside an upper-case conditional was attributed the
// WRONG guard (unconditionally applied) rather than being evaluated.
func TestUpperCaseIfIsAGuardNotAnOrdinaryCall(t *testing.T) {
	// A LITERAL patch filename inside an upper-case IF(): nothing here is an
	// unresolved variable, so the entry's bucket is decided purely by whether
	// the walker recognized IF() as a guard.
	dir := writePort(t, `
IF(SOME_UNDECIDABLE_VARIABLE)
    vcpkg_from_github(
        OUT_SOURCE_PATH SOURCE_PATH
        REPO acme/widget
        PATCHES fix.patch
    )
ENDIF()
`, "fix.patch")

	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if got := appliedNames(res); len(got) != 0 {
		t.Fatalf("applied = %v, want none — an upper-case IF() is a real guard, so the patch under it is undecidable, not a certain apply", got)
	}
	if got := undecidableNames(res); len(got) != 1 || got[0] != "fix.patch" {
		t.Fatalf("undecidable = %v, want [fix.patch] — the entry guarded by the upper-case IF()", got)
	}
}

// PR #591 P2 (patchesapply.go): the schema requires an absolute port_dir, but
// the value went straight to Stat — which resolves a relative path against the
// HUB DAEMON's working directory, not the caller's. That silently answers
// about a different port directory, and on a daemon whose cwd the caller
// cannot see it is not even diagnosable.
func TestRelativePortDirIsRefusedNotBoundToTheDaemonWorkingDirectory(t *testing.T) {
	res := ApplyOrder(Args{PortDir: "some/relative/port", Triplet: "x64-windows"})

	if res.Status != evidence.StatusFailed {
		t.Fatalf("status = %v, want failed — a relative port_dir is bad caller input, refused before use", res.Status)
	}
	if res.Reason != ReasonRelativePortDir {
		t.Fatalf("reason = %v, want %v — a relative path must never be silently resolved against the daemon's cwd",
			res.Reason, ReasonRelativePortDir)
	}
}

// unreadableStatDeps makes Stat on one exact path fail with a NON-ENOENT
// error — the shape a permission denial or sharing violation takes.
func unreadableStatDeps(lockedPath string) Deps {
	d := DefaultDeps()
	realStat := d.Stat
	d.Stat = func(path string) (os.FileInfo, error) {
		if filepath.Clean(path) == filepath.Clean(lockedPath) {
			return nil, fs.ErrPermission
		}
		return realStat(path)
	}
	return d
}

// PR #591 P2 (patchesapply.go): every Stat failure on the port dir collapsed
// into port_dir_missing, including permission and sharing errors. "Missing"
// tells an operator their port directory is GONE; the remedy for a locked one
// is entirely different. evidence.Presence is the shared owner of exactly this
// distinction and every other probe in the package already routes through it.
func TestUnreadablePortDirIsDistinctFromAMissingOne(t *testing.T) {
	dir := writePort(t, "# nothing\n")

	res := applyOrder(Args{PortDir: dir, Triplet: "x64-windows"}, unreadableStatDeps(dir))

	if res.Reason != ReasonPortDirUnreadable {
		t.Fatalf("reason = %v, want %v — an access-denied probe is NOT evidence that the directory is absent",
			res.Reason, ReasonPortDirUnreadable)
	}
	if res.Status != evidence.StatusUnknown {
		t.Fatalf("status = %v, want unknown", res.Status)
	}
	found := false
	for _, u := range res.Unreadable {
		if u.Kind == UnreadablePortDir && filepath.Clean(u.Path) == filepath.Clean(dir) {
			found = true
		}
	}
	if !found {
		t.Fatalf("unreadable = %+v, want the port dir listed with kind %q so the operator sees which path to fix",
			res.Unreadable, UnreadablePortDir)
	}
}

// A genuinely absent port dir must still report the VERIFIED absence, not the
// unreadable reason — the distinction has to cut both ways.
func TestAbsentPortDirStillReportsMissing(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "no-such-port")

	res := ApplyOrder(Args{PortDir: absent, Triplet: "x64-windows"})
	if res.Reason != ReasonPortDirMissing {
		t.Fatalf("reason = %v, want %v — a positively-reported absence is a verified negative", res.Reason, ReasonPortDirMissing)
	}
	if len(res.Unreadable) != 0 {
		t.Fatalf("unreadable = %+v, want empty — nothing declined to answer here", res.Unreadable)
	}
}

// containsStr is a local helper (the package's own dedupStrings does not
// expose membership).
func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if strings.EqualFold(s, needle) {
			return true
		}
	}
	return false
}

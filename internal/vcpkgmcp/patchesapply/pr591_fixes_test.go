package patchesapply

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/boundedio"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
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

// PR #591: VERSION_* is CMake's integer-component comparison, not semver.
// The cases here are anchored both in CMake's if() documentation and in a
// target-machine CMake 4.4.0 smoke probe; huge components also prove the
// implementation is not bounded by Go's native integer width.
func TestCompareVersionsMatchesCMakeComponentSemantics(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lhs, rhs   string
		comparison int
	}{
		{name: "omitted component is zero", lhs: "1.2.0", rhs: "1.2", comparison: 0},
		{name: "non-integer tail truncates", lhs: "1.0-rc1", rhs: "1.0", comparison: 0},
		{name: "truncated side has zero later component", lhs: "1.2a.9", rhs: "1.2.1", comparison: -1},
		{name: "components past tweak follow CMake runtime", lhs: "1.2.3.4.5", rhs: "1.2.3.4", comparison: 1},
		{name: "huge integer component does not overflow", lhs: "1.999999999999999999999999", rhs: "1.2", comparison: 1},
		{name: "leading zeros are ignored", lhs: "01.002.0003", rhs: "1.2.3", comparison: 0},
		{name: "non-integer first component truncates to zero", lhs: "x.9", rhs: "0", comparison: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareVersions(tc.lhs, tc.rhs); got != tc.comparison {
				t.Fatalf("compareVersions(%q, %q) = %d, want %d", tc.lhs, tc.rhs, got, tc.comparison)
			}
			if got := compareVersions(tc.rhs, tc.lhs); got != -tc.comparison {
				t.Fatalf("reverse compareVersions(%q, %q) = %d, want %d", tc.rhs, tc.lhs, got, -tc.comparison)
			}
		})
	}
}

func TestEveryVersionComparisonOperatorUsesCMakeOrdering(t *testing.T) {
	lower, higher := "1.2a.9", "1.2.1"
	for _, tc := range []struct {
		op   string
		want Tri
	}{
		{op: "VERSION_EQUAL", want: TriFalse},
		{op: "VERSION_GREATER", want: TriFalse},
		{op: "VERSION_GREATER_EQUAL", want: TriFalse},
		{op: "VERSION_LESS", want: TriTrue},
		{op: "VERSION_LESS_EQUAL", want: TriTrue},
	} {
		t.Run(tc.op, func(t *testing.T) {
			if got := evalComparison(tc.op, &lower, &higher); got != tc.want {
				t.Fatalf("evalComparison(%s, %q, %q) = %v, want %v", tc.op, lower, higher, got, tc.want)
			}
		})
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

type pr591ReadCloser struct {
	reader     *strings.Reader
	closed     bool
	maxRequest int
	totalRead  int
}

func (r *pr591ReadCloser) Read(p []byte) (int, error) {
	if len(p) > r.maxRequest {
		r.maxRequest = len(p)
	}
	n, err := r.reader.Read(p)
	r.totalRead += n
	return n, err
}

func (r *pr591ReadCloser) Close() error {
	r.closed = true
	return nil
}

// TestPR591_PortfileReadStopsAtPackageByteCap proves the parser receives only
// a complete bounded portfile: the cap-plus-one byte is a sentinel, not a
// parseable prefix, and the opened handle closes on the limit path.
func TestPR591_PortfileReadStopsAtPackageByteCap(t *testing.T) {
	dir := t.TempDir()
	portfilePath := filepath.Join(dir, "portfile.cmake")
	if err := os.WriteFile(portfilePath, []byte("# real file for Stat\n"), 0o644); err != nil {
		t.Fatalf("write portfile: %v", err)
	}

	reader := &pr591ReadCloser{reader: strings.NewReader(strings.Repeat("x", int(MaxPortfileBytes+1)))}
	deps := DefaultDeps()
	deps.Open = func(path string) (io.ReadCloser, error) {
		if filepath.Clean(path) != filepath.Clean(portfilePath) {
			t.Fatalf("Open(%q), want only %q", path, portfilePath)
		}
		return reader, nil
	}

	res := applyOrderContext(context.Background(), Args{PortDir: dir, Triplet: "x64-windows"}, deps)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPortfileSizeLimitExceeded {
		t.Fatalf("status/reason = %v/%v, want unknown/%v", res.Status, res.Reason, ReasonPortfileSizeLimitExceeded)
	}
	if !reader.closed {
		t.Fatal("portfile handle was not closed on the byte-cap path")
	}
	if reader.totalRead != int(MaxPortfileBytes+1) {
		t.Fatalf("portfile bytes read = %d, want exactly cap+1=%d", reader.totalRead, MaxPortfileBytes+1)
	}
	if reader.maxRequest > int(portfileReadBatchBytes) {
		t.Fatalf("largest read request = %d, batch limit = %d", reader.maxRequest, portfileReadBatchBytes)
	}
}

type pr591DirEntry struct {
	name string
	dir  bool
}

func (e pr591DirEntry) Name() string { return e.name }
func (e pr591DirEntry) IsDir() bool  { return e.dir }
func (e pr591DirEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e pr591DirEntry) Info() (fs.FileInfo, error) { return nil, nil }

type pr591DirReader struct {
	entries     []os.DirEntry
	next        int
	closed      bool
	maxRequest  int
	onFirstRead func()
}

func (r *pr591DirReader) ReadDir(n int) ([]os.DirEntry, error) {
	if n > r.maxRequest {
		r.maxRequest = n
	}
	if r.onFirstRead != nil {
		r.onFirstRead()
		r.onFirstRead = nil
	}
	if r.next == len(r.entries) {
		return nil, io.EOF
	}
	end := r.next + n
	if end > len(r.entries) {
		end = len(r.entries)
	}
	page := r.entries[r.next:end]
	r.next = end
	return page, nil
}

func (r *pr591DirReader) Close() error {
	r.closed = true
	return nil
}

// TestPR591_OrphanScanBudgetsAndCancellationAreBounded proves the paged
// walker closes directory resources and turns both a resource cap and a
// cancelled request into the visible incomplete-scan verdict.
func TestPR591_OrphanScanBudgetsAndCancellationAreBounded(t *testing.T) {
	t.Run("entry limit", func(t *testing.T) {
		dir := writePort(t, "# no patches\n")
		entries := make([]os.DirEntry, MaxOrphanScanEntries+1)
		for i := range entries {
			entries[i] = pr591DirEntry{name: "entry-" + strings.Repeat("x", i%3)}
		}
		reader := &pr591DirReader{entries: entries}
		deps := DefaultDeps()
		deps.OpenDir = func(path string) (boundedio.DirReader, error) {
			return reader, nil
		}

		res := applyOrderContext(context.Background(), Args{PortDir: dir, Triplet: "x64-windows"}, deps)
		if res.Status != evidence.StatusUnknown || res.Reason != ReasonOrphanScanIncomplete || res.OrphanScanStopCause != OrphanScanStopEntryLimit {
			t.Fatalf("result = %+v, want unknown/orphan_scan_incomplete/entry_limit_exceeded", res)
		}
		if !reader.closed {
			t.Fatal("directory handle was not closed after entry-limit sentinel")
		}
		if reader.maxRequest > OrphanScanReadBatchEntries {
			t.Fatalf("largest directory request = %d, batch limit = %d", reader.maxRequest, OrphanScanReadBatchEntries)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		dir := writePort(t, "# no patches\n")
		ctx, cancel := context.WithCancel(context.Background())
		reader := &pr591DirReader{entries: []os.DirEntry{pr591DirEntry{name: "ordinary.txt"}}, onFirstRead: cancel}
		deps := DefaultDeps()
		deps.OpenDir = func(path string) (boundedio.DirReader, error) {
			return reader, nil
		}

		res := applyOrderContext(ctx, Args{PortDir: dir, Triplet: "x64-windows"}, deps)
		if res.Status != evidence.StatusUnknown || res.Reason != ReasonOrphanScanIncomplete || res.OrphanScanStopCause != OrphanScanStopCancelled {
			t.Fatalf("result = %+v, want unknown/orphan_scan_incomplete/cancelled", res)
		}
		if !reader.closed {
			t.Fatal("directory handle was not closed after cancellation")
		}
	})
}

// TestPR591_DeferredFunctionAndMacroPatchesAreNotExecuted proves declarations
// cannot mutate the static environment or be modeled through a later call.
func TestPR591_DeferredFunctionAndMacroPatchesAreNotExecuted(t *testing.T) {
	dir := writePort(t, `
function(add_function_patch)
    set(PATCHES function.patch)
    vcpkg_from_github(PATCHES ${PATCHES})
endfunction()
macro(add_macro_patch)
    list(APPEND PATCHES macro.patch)
endmacro()
add_function_patch()
add_macro_patch()
vcpkg_from_github(PATCHES top-level.patch)
`, "function.patch", "macro.patch", "top-level.patch")

	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPatchesDeferredCommandBody {
		t.Fatalf("status/reason = %v/%v, want unknown/%v", res.Status, res.Reason, ReasonPatchesDeferredCommandBody)
	}
	if len(res.Applied) != 0 || len(res.Missing) != 0 || len(res.Orphaned) != 0 {
		t.Fatalf("deferred declaration leaked execution effects: %+v", res)
	}
}

// TestPR591_CMakeListTokenSemanticsForPatchEntries is rebaselined to CMake
// 4.4: assignment serializes arguments, so source quoting/escaping no longer
// protects semicolons when the value is later inserted unquoted.
func TestPR591_CMakeListTokenSemanticsForPatchEntries(t *testing.T) {
	dir := writePort(t, `
set(PATCH_LIST "quoted;set.patch" escaped\;set.patch set-one.patch;set-two.patch)
list(APPEND PATCH_LIST [=[bracket;append.patch]=] list-one.patch;list-two.patch)
vcpkg_from_github(
    PATCHES direct-one.patch;direct-two.patch "quoted;patch.patch" [=[bracket;patch.patch]=] escaped\;patch.patch ${PATCH_LIST}
)
`, "direct-one.patch", "direct-two.patch", "quoted;patch.patch", "bracket;patch.patch", "escaped;patch.patch", "quoted;set.patch", "escaped;set.patch", "set-one.patch", "set-two.patch", "bracket;append.patch", "list-one.patch", "list-two.patch")

	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if res.Status != evidence.StatusOK {
		t.Fatalf("status/reason = %v/%v, want ok", res.Status, res.Reason)
	}
	want := []string{"direct-one.patch", "direct-two.patch", "quoted;patch.patch", "bracket;patch.patch", "escaped;patch.patch", "quoted", "set.patch", "escaped", "set.patch", "set-one.patch", "set-two.patch", "bracket", "append.patch", "list-one.patch", "list-two.patch"}
	if got := appliedNames(res); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("applied = %v, want %v", got, want)
	}
}

func TestPR591_CMakeListBackslashAndBracketBalanceMatchesCMake440(t *testing.T) {
	env := newVarEnv(t.TempDir(), "port", "", nil, nil)
	items := func(source string) []semanticItem {
		toks := tokenize(source)
		if len(toks) != 1 {
			t.Fatalf("tokenize(%q) = %+v, want one token", source, toks)
		}
		items, resolution := semanticItemsFromToken(toks[0], env, TriTrue, "", nil)
		if resolution.failed() {
			t.Fatalf("semanticItemsFromToken(%q) resolution = %v, want success", source, resolution.issue)
		}
		return items
	}
	itemTexts := func(values []semanticItem) []string {
		out := make([]string, 0, len(values))
		for _, value := range values {
			out = append(out, value.text)
		}
		return out
	}

	for _, tc := range []struct {
		name, source, want string
	}{
		{name: "one direct backslash", source: `one\;held`, want: "one;held"},
		{name: "two direct backslashes", source: `two\\;held`, want: "two;held"},
		{name: "three direct backslashes", source: `three\\\;held`, want: `three\;held`},
		{name: "four direct backslashes", source: `four\\\\;held`, want: `four\;held`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := itemTexts(items(tc.source)); strings.Join(got, "|") != tc.want {
				t.Fatalf("items(%q) = %q, want %q", tc.source, got, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name, value, want string
	}{
		{name: "one serialized backslash", value: `one\;held`, want: "one;held"},
		{name: "two serialized backslashes", value: `two\\;split`, want: `two\;split`},
		{name: "three serialized backslashes", value: `three\\\;held`, want: `three\\;held`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env.setValue("VALUE", serializedValue{text: tc.value})
			if got := itemTexts(items(`${VALUE}`)); strings.Join(got, "|") != tc.want {
				t.Fatalf("items(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}

	env.setValue("BALANCED", serializedValue{text: `open[;held];split`})
	if got := itemTexts(items(`${BALANCED}`)); strings.Join(got, "|") != "open[;held]|split" {
		t.Fatalf("bracket-balanced list = %q, want [open[;held] split]", got)
	}
	env.setValue("EMPTY", serializedValue{text: ""})
	if got := items(`${EMPTY}`); len(got) != 0 {
		t.Fatalf("unquoted empty serialized value = %+v, want no argument", got)
	}
	if got := items(`""`); len(got) != 1 || got[0].text != "" {
		t.Fatalf("quoted empty value = %+v, want one empty argument", got)
	}
}

func TestPR591_ExactVariableCopyResplitsAndPreservesGuardProvenance(t *testing.T) {
	dir := writePort(t, `
if(SOME_UNDECIDABLE_VARIABLE)
    list(APPEND PATCHES "guard-one.patch;guard-two.patch")
endif()
vcpkg_from_github(PATCHES ${PATCHES})
`, "guard-one.patch", "guard-two.patch")

	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if len(res.Applied) != 0 || len(res.Missing) != 0 || len(res.Orphaned) != 0 {
		t.Fatalf("guarded serialized copy leaked a certain verdict: %+v", res)
	}
	if got := undecidableNames(res); strings.Join(got, "|") != "guard-one.patch|guard-two.patch" {
		t.Fatalf("undecidable = %v, want both re-split CMake arguments", got)
	}
	for _, entry := range res.Undecidable {
		if !strings.Contains(entry.Guard, "SOME_UNDECIDABLE_VARIABLE") || !containsStr(entry.UnresolvedVars, "SOME_UNDECIDABLE_VARIABLE") {
			t.Fatalf("provenance for %q = %+v, want the originating undecidable guard", entry.Filename, entry)
		}
	}
}

func TestPR591_DeclaredInvocationPatchesForwardingFailsClosed(t *testing.T) {
	deferred := func(t *testing.T, declaration, invocation string) {
		t.Helper()
		dir := writePort(t, declaration+"\n"+invocation+"\n", "forwarded.patch")
		res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
		if res.Status != evidence.StatusUnknown || res.Reason != ReasonPatchesDeferredCommandBody {
			t.Fatalf("status/reason = %v/%v, want unknown/%v", res.Status, res.Reason, ReasonPatchesDeferredCommandBody)
		}
		if len(res.Applied) != 0 || len(res.Missing) != 0 || len(res.Orphaned) != 0 {
			t.Fatalf("deferred invocation leaked patch probing or orphan classification: %+v", res)
		}
	}
	deferred(t, "function(fetch)\n  vcpkg_from_github(${ARGN})\nendfunction()", "fetch(PATCHES forwarded.patch)")
	deferred(t, "macro(fetch)\n  vcpkg_from_github(${ARGN})\nendmacro()", "fetch(PATCHES forwarded.patch)")
	deferred(t, "function(fetch)\n  vcpkg_from_github(${ARGN})\nendfunction()", `fetch("PATCHES" forwarded.patch)`)
	deferred(t, "set(KW PATCHES)\nfunction(fetch)\n  vcpkg_from_github(${ARGN})\nendfunction()", "fetch(${KW} forwarded.patch)")

	for _, invocation := range []string{"fetch(PATCHES_SUFFIX forwarded.patch)", "fetch(forwarded.patch)"} {
		dir := writePort(t, "function(fetch)\n  vcpkg_from_github(${ARGN})\nendfunction()\n"+invocation+"\n", "forwarded.patch")
		res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
		if res.Reason == ReasonPatchesDeferredCommandBody {
			t.Fatalf("%q incorrectly triggered deferred PATCHES classification: %+v", invocation, res)
		}
	}
	for _, invocation := range []string{"fetch(${UNKNOWN} forwarded.patch)", "fetch(${${DYNAMIC_NAME}} forwarded.patch)"} {
		deferred(t, "function(fetch)\n  vcpkg_from_github(${ARGN})\nendfunction()", invocation)
	}

	dir := writePort(t, "function(fetch)\n  vcpkg_from_github(${ARGN})\nendfunction()\nvcpkg_from_github(PATCHES top-level.patch)\n", "top-level.patch")
	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if got := appliedNames(res); strings.Join(got, "|") != "top-level.patch" {
		t.Fatalf("direct top-level PATCHES extraction = %v, want [top-level.patch]", got)
	}
}

func TestPR591_NestedVariableNameAndPatchValueResolve(t *testing.T) {
	dir := writePort(t, `
set(PATCH_NAME nested.patch)
set(PATCH_NAME_VARIABLE PATCH_NAME)
vcpkg_from_github(PATCHES ${${PATCH_NAME_VARIABLE}})
`, "nested.patch")

	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if res.Status != evidence.StatusOK || res.Reason != "" {
		t.Fatalf("status/reason = %v/%v, want ok/empty", res.Status, res.Reason)
	}
	if got := appliedNames(res); strings.Join(got, "|") != "nested.patch" {
		t.Fatalf("applied = %v, want [nested.patch]", got)
	}
	if len(res.Missing) != 0 || len(res.Orphaned) != 0 {
		t.Fatalf("nested resolution leaked missing/orphan evidence: %+v", res)
	}
}

func TestPR591_BalancedReferenceScannerFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		text  string
		state referenceScanState
		start int
		end   int
	}{
		{text: "literal", state: referenceScanNone},
		{text: "${NAME}", state: referenceScanFound, start: 0, end: 7},
		{text: "pre${${NAME}}post", state: referenceScanFound, start: 3, end: 13},
		{text: "pre${$ENV{${ENV_NAME}}}post", state: referenceScanFound, start: 3, end: 23},
		{text: "$ENV{${ENV_NAME}}", state: referenceScanFound, start: 0, end: 17},
		{text: "$ENV{$ENV{VCPKG_ROOT}}", state: referenceScanFound, start: 0, end: 22},
		{text: "pre${NAME}post", state: referenceScanFound, start: 3, end: 10},
		{text: "${${NAME}", state: referenceScanMalformed, start: 0},
		{text: "${$ENV{${ENV_NAME}}", state: referenceScanMalformed, start: 0},
		{text: "$ENV{VCPKG_ROOT", state: referenceScanMalformed, start: 0},
	} {
		t.Run(tc.text, func(t *testing.T) {
			scan := scanNextVariableReference(tc.text)
			if scan.state != tc.state {
				t.Fatalf("scan(%q) state=%v, want %v", tc.text, scan.state, tc.state)
			}
			if scan.state == referenceScanFound && (scan.reference.start != tc.start || scan.reference.end != tc.end) {
				t.Fatalf("scan(%q) span=%d:%d, want %d:%d", tc.text, scan.reference.start, scan.reference.end, tc.start, tc.end)
			}
		})
	}

	dir := writePort(t, "set(NAME inner.patch)\nvcpkg_from_github(PATCHES ${${NAME})\n", "inner.patch")
	assertPR591ExpressionWithoutProbes(t, applyOrder(Args{PortDir: dir, Triplet: "x64-windows"}, pr591NoProbeDeps(t)))
}

func TestPR591_EnvironmentReferenceNamesShareResolutionState(t *testing.T) {
	chain := func(depth int) *varEnv {
		env := newVarEnv(t.TempDir(), "port", "vcpkg-root", nil, nil)
		for i := 1; i <= depth; i++ {
			name := "E" + strconv.Itoa(i)
			value := "VCPKG_ROOT"
			if i < depth {
				value = "${E" + strconv.Itoa(i+1) + "}"
			}
			env.setValue(name, serializedValue{text: value})
		}
		return env
	}
	for _, depth := range []int{maxVariableDereferenceDepth - 1, maxVariableDereferenceDepth} {
		t.Run("depth "+strconv.Itoa(depth+1), func(t *testing.T) {
			env := chain(depth)
			token := tokenize("$ENV{${E1}}")[0]
			value := env.evaluateValue(token, certainProvenance())
			if depth+1 == maxVariableDereferenceDepth {
				if value.resolution.failed() || string(value.text) != "vcpkg-root" {
					t.Fatalf("environment depth %d = %+v, want resolved VCPKG_ROOT", depth+1, value)
				}
				return
			}
			if value.resolution.issue != valueResolutionDepthExceeded || len(value.text) != 0 || len(value.metas) != 0 || len(value.protectedSemicolon) != 0 || value.exactReference {
				t.Fatalf("environment depth %d = %+v, want cleared depth failure", depth+1, value)
			}
		})
	}
	for _, totalDepth := range []int{maxVariableDereferenceDepth, maxVariableDereferenceDepth + 1} {
		t.Run("mixed depth "+strconv.Itoa(totalDepth), func(t *testing.T) {
			env := chain(totalDepth - 2)
			env.setValue("vcpkg-root", serializedValue{text: "depth.patch"})
			value := env.evaluateValue(tokenize("${$ENV{${E1}}}")[0], certainProvenance())
			if totalDepth == maxVariableDereferenceDepth {
				if value.resolution.failed() || string(value.text) != "depth.patch" {
					t.Fatalf("mixed depth %d = %+v, want resolved patch", totalDepth, value)
				}
				return
			}
			if value.resolution.issue != valueResolutionDepthExceeded || len(value.text) != 0 || len(value.metas) != 0 || len(value.protectedSemicolon) != 0 || value.exactReference {
				t.Fatalf("mixed depth %d = %+v, want cleared depth failure", totalDepth, value)
			}
		})
	}

	dir := writePort(t, "vcpkg_from_github(PATCHES prefix$ENV{${MISSING}})\n")
	assertPR591ExpressionWithoutProbes(t, applyOrder(Args{PortDir: dir, Triplet: "x64-windows"}, pr591NoProbeDeps(t)))
}

func TestPR591_SharedReferenceDepthBudget(t *testing.T) {
	selectorExpression := func(depth int) (string, map[string]string) {
		values := make(map[string]string, depth)
		for i := 1; i <= depth; i++ {
			if i == depth {
				values["V"+strconv.Itoa(i)] = "depth.patch"
			} else {
				values["V"+strconv.Itoa(i)] = "V" + strconv.Itoa(i+1)
			}
		}
		return strings.Repeat("${", depth) + "V1" + strings.Repeat("}", depth), values
	}
	assertDepth := func(t *testing.T, expression string, values map[string]string, wantOK bool) {
		t.Helper()
		dir := writePort(t, "vcpkg_from_github(PATCHES "+expression+")\n", "depth.patch", "V1")
		if wantOK {
			res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows", VarOverrides: values})
			if res.Status != evidence.StatusOK || len(res.Applied) != 1 || res.Applied[0].Existence != evidence.PresenceExists {
				t.Fatalf("depth success result=%+v", res)
			}
			return
		}
		res := applyOrder(Args{PortDir: dir, Triplet: "x64-windows", VarOverrides: values}, pr591NoProbeDeps(t))
		assertPR591ExpressionWithoutProbes(t, res)
	}
	for _, depth := range []int{maxVariableDereferenceDepth, maxVariableDereferenceDepth + 1} {
		expression, values := selectorExpression(depth)
		assertDepth(t, expression, values, depth == maxVariableDereferenceDepth)
	}

	mixed := func(total int) (string, map[string]string) {
		const syntaxDepth = 16
		values := make(map[string]string, total)
		values["V1"] = "${S1}"
		serializedDepth := total - syntaxDepth
		for i := 1; i <= serializedDepth; i++ {
			if i == serializedDepth {
				values["S"+strconv.Itoa(i)] = "V1"
			} else {
				values["S"+strconv.Itoa(i)] = "${S" + strconv.Itoa(i+1) + "}"
			}
		}
		return strings.Repeat("${", syntaxDepth) + "V1" + strings.Repeat("}", syntaxDepth), values
	}
	for _, total := range []int{maxVariableDereferenceDepth, maxVariableDereferenceDepth + 1} {
		expression, values := mixed(total)
		assertDepth(t, expression, values, total == maxVariableDereferenceDepth)
	}

	env := newVarEnv(t.TempDir(), "port", "", map[string]string{"X": "sibling.patch"}, nil)
	tokens := tokenize("${X}${X}")
	if value := env.evaluateValue(tokens[0], certainProvenance()); value.resolution.failed() || string(value.text) != "sibling.patchsibling.patch" {
		t.Fatalf("flat sibling references = %+v, want both expansions without a depth failure", value)
	}
}

func TestPR591_NestedSelectorProvenanceMerge(t *testing.T) {
	call := provenanceMeta{source: "call-source", guard: TriUnknown, guardText: "CALL_GUARD", unresolvedVars: []string{"CALL_VAR"}, pathUnresolved: []string{"CALL_PATH"}}
	selector := provenanceMeta{source: "selector-source", guard: TriUnknown, guardText: "SELECTOR_GUARD", unresolvedVars: []string{"SELECTOR_VAR"}, pathUnresolved: []string{"SELECTOR_PATH"}}
	selected := provenanceMeta{source: "selected-source", guard: TriTrue, guardText: "SELECTED_GUARD", unresolvedVars: []string{"SELECTED_VAR"}, pathUnresolved: []string{"SELECTED_PATH"}}
	merged := mergeProvenance(mergeProvenance(call, selector, provenancePeer), selected, provenanceSelectedValue)
	if merged.source != "selected-source" || strings.Join(merged.sourceProvenance, "|") != "call-source|selector-source|selected-source" || merged.guard != TriUnknown || merged.guardText != "CALL_GUARD AND SELECTOR_GUARD AND SELECTED_GUARD" || strings.Join(merged.unresolvedVars, "|") != "CALL_VAR|SELECTOR_VAR|SELECTED_VAR" || strings.Join(merged.pathUnresolved, "|") != "CALL_PATH|SELECTOR_PATH|SELECTED_PATH" {
		t.Fatalf("merged selector provenance = %+v", merged)
	}

	dir := writePort(t, "if(SELECTOR_GUARD)\n  set(SELECTOR PATCH_NAME)\nendif()\nset(PATCH_NAME guarded.patch)\nvcpkg_from_github(PATCHES ${${SELECTOR}})\n", "guarded.patch")
	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if len(res.Applied) != 0 || strings.Join(undecidableNames(res), "|") != "guarded.patch" || len(res.Undecidable) != 1 || !strings.Contains(res.Undecidable[0].Guard, "SELECTOR_GUARD") || !containsStr(res.Undecidable[0].UnresolvedVars, "SELECTOR_GUARD") {
		t.Fatalf("nested selector result=%+v, want one selector-guarded undecidable patch", res)
	}

	env := newVarEnv(t.TempDir(), "port", "", nil, nil)
	env.setValue("SELECTOR", serializedValue{text: "PATCH", spans: []provenanceSpan{{start: 0, end: len("PATCH"), meta: provenanceMeta{source: "selector-source", guard: TriUnknown, guardText: "SELECTOR_GUARD"}}}})
	env.setValue("PATCH", serializedValue{text: "selected.patch", spans: []provenanceSpan{{start: 0, end: len("selected.patch"), meta: provenanceMeta{source: "selected-source"}}}})
	callMeta := provenanceMeta{guard: TriUnknown, guardText: "CALL_GUARD"}
	value := env.evaluateValue(tokenize("${${SELECTOR}}")[0], callMeta)
	if value.resolution.failed() || len(value.metas) != len(value.text) || value.metas[0].guardText != "CALL_GUARD AND SELECTOR_GUARD" || value.metas[0].source != "selected-source" {
		t.Fatalf("nested selector call metadata = %+v, want each guard once and selected source", value)
	}

	env.setValue("PATCH", serializedValue{text: "builtin.patch", spans: []provenanceSpan{{start: 0, end: len("builtin.patch"), meta: provenanceMeta{}}}})
	value = env.evaluateValue(tokenize("${${SELECTOR}}")[0], callMeta)
	if value.resolution.failed() || len(value.metas) != len(value.text) || value.metas[0].source != "" || strings.Join(value.metas[0].sourceProvenance, "|") != "selector-source" {
		t.Fatalf("source-less selected owner = %+v, want no scalar selector fallback", value)
	}
}

func TestPR591_ExactReferenceShapeControlsDisplay(t *testing.T) {
	env := newVarEnv(t.TempDir(), "port", "", nil, nil)
	env.setValue("X", serializedValue{text: "simple.patch", spans: []provenanceSpan{{start: 0, end: len("simple.patch"), meta: provenanceMeta{source: "simple-source", guard: TriTrue}}}})
	env.setValue("NAME", serializedValue{text: "X"})
	for _, tc := range []struct {
		source string
		want   string
	}{
		{source: "${X}", want: "simple-source"},
		{source: "${${NAME}}", want: "simple-source"},
		{source: "${X}${X}", want: "${X}${X}"},
		{source: "${X}suffix", want: "${X}suffix"},
		{source: "prefix${X}", want: "prefix${X}"},
		{source: "\"${X}\"", want: "${X}"},
		{source: "[=[${X}]=]", want: "${X}"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			tokens := tokenize(tc.source)
			items, resolution := semanticItemsFromToken(tokens[0], env, TriTrue, "", nil)
			if resolution.failed() || len(items) != 1 || items[0].display != tc.want {
				t.Fatalf("items(%q)=%+v resolution=%+v, want display %q", tc.source, items, resolution, tc.want)
			}
		})
	}

	dir := writePort(t, "set(X composite.patch)\nvcpkg_from_github(PATCHES ${X}${X})\n", "composite.patchcomposite.patch")
	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if strings.Join(appliedNames(res), "|") != "${X}${X}" {
		t.Fatalf("composite display=%v, want raw composite token", appliedNames(res))
	}
}

func TestPR591_CMakeLanguageCallClassificationFailsClosed(t *testing.T) {
	for _, invocation := range []string{
		"cmake_language(CALL fetch PATCHES forwarded.patch)",
		"set(COMMAND fetch)\ncmake_language(CALL ${COMMAND} PATCHES forwarded.patch)",
		"set(COMMAND fetch)\nset(COMMAND_NAME COMMAND)\ncmake_language(CALL ${${COMMAND_NAME}} PATCHES forwarded.patch)",
		"cmake_language(CALL external_dispatch PATCHES forwarded.patch)",
	} {
		dir := writePort(t, "function(fetch)\nendfunction()\n"+invocation+"\n", "forwarded.patch")
		assertPR591DeferredWithoutProbes(t, applyOrder(Args{PortDir: dir, Triplet: "x64-windows"}, pr591NoProbeDeps(t)))
	}

	for _, invocation := range []string{
		"cmake_language(CALL)",
		"cmake_language(CALL \"\")",
		"set(EMPTY)\ncmake_language(CALL ${EMPTY})",
		"cmake_language(CALL ${UNKNOWN})",
		"set(NAME MISSING)\ncmake_language(CALL ${${NAME}})",
		"cmake_language(CALL ${${NAME})",
		"cmake_language(CALL known ${UNKNOWN})",
		"cmake_language(CALL known ${${NAME})",
		"cmake_language(CALL known PATCHES forwarded.patch)",
		"set(KEY PATCHES)\ncmake_language(CALL known ${KEY} forwarded.patch)",
	} {
		dir := writePort(t, invocation+"\nvcpkg_from_github(PATCHES later.patch)\n", "later.patch", "forwarded.patch")
		assertPR591DeferredWithoutProbes(t, applyOrder(Args{PortDir: dir, Triplet: "x64-windows"}, pr591NoProbeDeps(t)))
	}

	dir := writePort(t, `
cmake_language(CALL known_command VALUE not-patches)
vcpkg_from_github(PATCHES top-level.patch)
`, "top-level.patch")
	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if res.Status != evidence.StatusOK || strings.Join(appliedNames(res), "|") != "top-level.patch" {
		t.Fatalf("known non-PATCHES CALL result = %+v, want only top-level.patch", res)
	}
	for _, invocation := range []string{
		"cmake_language(EVAL ${UNKNOWN})",
		"cmake_language(EVAL ${${MISSING})",
		"set(MODE CALL)\ncmake_language(${MODE} known PATCHES forwarded.patch)",
	} {
		dir := writePort(t, invocation+"\nvcpkg_from_github(PATCHES top-level.patch)\n", "top-level.patch")
		res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
		if res.Status != evidence.StatusOK || strings.Join(appliedNames(res), "|") != "top-level.patch" {
			t.Fatalf("non-CALL %q result=%+v, want later direct PATCHES normal", invocation, res)
		}
	}
}

func TestPR591_NestedExpansionFailuresFailClosed(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		dir := writePort(t, "set(A ${B})\nset(B ${A})\nvcpkg_from_github(PATCHES ${A})\n")
		assertPR591ExpressionWithoutProbes(t, applyOrder(Args{PortDir: dir, Triplet: "x64-windows"}, pr591NoProbeDeps(t)))
	})
	t.Run("unknown nested name", func(t *testing.T) {
		dir := writePort(t, "set(PATCH_NAME_VARIABLE MISSING)\nvcpkg_from_github(PATCHES ${${PATCH_NAME_VARIABLE}})\n")
		assertPR591ExpressionWithoutProbes(t, applyOrder(Args{PortDir: dir, Triplet: "x64-windows"}, pr591NoProbeDeps(t)))
	})
	t.Run("environment nested name clears first issue", func(t *testing.T) {
		env := newVarEnv(t.TempDir(), "port", "", nil, nil)
		value := env.evaluateValue(tokenize("literal$ENV{${MISSING}} ")[0], certainProvenance())
		if value.resolution.issue != valueResolutionNestedNameUnresolved || len(value.text) != 0 || len(value.metas) != 0 || len(value.protectedSemicolon) != 0 || value.exactReference {
			t.Fatalf("environment nested-name failure = %+v, want first issue and fully cleared value", value)
		}
		dir := writePort(t, "vcpkg_from_github(PATCHES literal$ENV{${MISSING}})\n")
		assertPR591ExpressionWithoutProbes(t, applyOrder(Args{PortDir: dir, Triplet: "x64-windows"}, pr591NoProbeDeps(t)))
	})
	t.Run("stored set and list issues", func(t *testing.T) {
		for _, portfile := range []string{
			"set(PATCH_NAME_VARIABLE MISSING)\nset(BAD ${${PATCH_NAME_VARIABLE}})\nvcpkg_from_github(PATCHES ${BAD})\n",
			"set(PATCH_NAME_VARIABLE MISSING)\nlist(APPEND PATCH_LIST ${${PATCH_NAME_VARIABLE}})\nvcpkg_from_github(PATCHES ${PATCH_LIST})\n",
		} {
			dir := writePort(t, portfile)
			assertPR591ExpressionWithoutProbes(t, applyOrder(Args{PortDir: dir, Triplet: "x64-windows"}, pr591NoProbeDeps(t)))
		}
	})
	t.Run("unused stored issue remains inert", func(t *testing.T) {
		dir := writePort(t, "set(PATCH_NAME_VARIABLE MISSING)\nset(BAD ${${PATCH_NAME_VARIABLE}})\nvcpkg_from_github(PATCHES good.patch)\n", "good.patch")
		res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
		if res.Status != evidence.StatusOK || strings.Join(appliedNames(res), "|") != "good.patch" {
			t.Fatalf("unused bad value changed a direct PATCHES result: %+v", res)
		}
	})
	t.Run("depth boundary", func(t *testing.T) {
		chain := func(depth int) map[string]string {
			overrides := make(map[string]string, depth)
			for i := 1; i <= depth; i++ {
				if i == depth {
					overrides["V"+strconv.Itoa(i)] = "depth.patch"
				} else {
					overrides["V"+strconv.Itoa(i)] = "${V" + strconv.Itoa(i+1) + "}"
				}
			}
			return overrides
		}
		success := writePort(t, "vcpkg_from_github(PATCHES ${V1})\n", "depth.patch")
		res := ApplyOrder(Args{PortDir: success, Triplet: "x64-windows", VarOverrides: chain(maxVariableDereferenceDepth)})
		if res.Status != evidence.StatusOK || len(res.Applied) != 1 || res.Applied[0].Existence != evidence.PresenceExists {
			t.Fatalf("depth 32 result = %+v, want applied depth.patch", res)
		}
		failure := writePort(t, "vcpkg_from_github(PATCHES ${V1})\n")
		assertPR591ExpressionWithoutProbes(t, applyOrder(Args{PortDir: failure, Triplet: "x64-windows", VarOverrides: chain(maxVariableDereferenceDepth + 1)}, pr591NoProbeDeps(t)))
	})
	t.Run("expanded byte budget", func(t *testing.T) {
		dir := writePort(t, "vcpkg_from_github(PATCHES ${EXPANDED}${EXPANDED})\n")
		res := applyOrder(Args{PortDir: dir, Triplet: "x64-windows", VarOverrides: map[string]string{"EXPANDED": strings.Repeat("x", int(MaxPortfileBytes/2)+1)}}, pr591NoProbeDeps(t))
		assertPR591ExpressionWithoutProbes(t, res)
	})
}

func pr591NoProbeDeps(t *testing.T) Deps {
	t.Helper()
	deps := DefaultDeps()
	openDir := deps.OpenDir
	deps.OpenDir = func(path string) (boundedio.DirReader, error) {
		t.Fatalf("unexpected orphan-directory probe: %s", path)
		return openDir(path)
	}
	stat := deps.Stat
	deps.Stat = func(path string) (os.FileInfo, error) {
		if strings.HasSuffix(filepath.ToSlash(path), ".patch") {
			t.Fatalf("unexpected patch-file probe: %s", path)
		}
		return stat(path)
	}
	return deps
}

func TestTripletReadBoundaryNAndNPlusOne(t *testing.T) {
	portDir := writePort(t, "# no patches\n")
	overlay := t.TempDir()
	triplet := filepath.Join(overlay, "x64-test.cmake")
	if err := os.WriteFile(triplet, []byte(strings.Repeat("x", int(MaxTripletFileBytes))), 0o644); err != nil {
		t.Fatal(err)
	}
	under := applyOrder(Args{PortDir: portDir, Triplet: "x64-test", OverlayTriplets: []string{overlay}}, DefaultDeps())
	if under.Reason == ReasonTripletFileSizeLimitExceeded {
		t.Fatalf("N-byte triplet unexpectedly limited: %+v", under)
	}
	if err := os.WriteFile(triplet, []byte(strings.Repeat("x", int(MaxTripletFileBytes+1))), 0o644); err != nil {
		t.Fatal(err)
	}
	over := applyOrder(Args{PortDir: portDir, Triplet: "x64-test", OverlayTriplets: []string{overlay}}, DefaultDeps())
	if over.Status != evidence.StatusUnknown || over.Reason != ReasonTripletFileSizeLimitExceeded || over.TripletFile != triplet {
		t.Fatalf("oversized triplet result=%+v, want unknown/size-limit with selected path", over)
	}
}

func TestWalkPortfileRejectsUnclosedIfFunctionAndMacro(t *testing.T) {
	for _, src := range []string{
		"if(ON)\nvcpkg_from_github(PATCHES one.patch)\n",
		"function(f)\nvcpkg_from_github(PATCHES one.patch)\n",
		"macro(f)\nvcpkg_from_github(PATCHES one.patch)\n",
	} {
		entries, saw, structural := walkPortfile(src, newVarEnv("", "", "", nil, nil))
		if structural != parserStructuralExpressionUnparsable || saw || len(entries) != 0 {
			t.Fatalf("walkPortfile(%q) = (%+v,%v,%v), want no entries/expression-unparsable", src, entries, saw, structural)
		}
	}
}

func TestApplyOrderUnclosedScopeStopsBeforePatchAndOrphanProbes(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"if", "if(ON)\nvcpkg_from_github(PATCHES one.patch)\n"},
		{"function", "function(f)\nvcpkg_from_github(PATCHES one.patch)\n"},
		{"macro", "macro(f)\nvcpkg_from_github(PATCHES one.patch)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writePort(t, tc.source, "one.patch", "tail.patch")
			res := applyOrder(Args{PortDir: dir, Triplet: "x64-windows"}, pr591NoProbeDeps(t))
			assertPR591ExpressionWithoutProbes(t, res)
		})
	}
}

func assertPR591ExpressionWithoutProbes(t *testing.T, res Result) {
	t.Helper()
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPatchesExprUnparsable {
		t.Fatalf("status/reason = %v/%v, want unknown/%v", res.Status, res.Reason, ReasonPatchesExprUnparsable)
	}
	assertPR591EmptyBuckets(t, res)
}

func assertPR591DeferredWithoutProbes(t *testing.T, res Result) {
	t.Helper()
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPatchesDeferredCommandBody {
		t.Fatalf("status/reason = %v/%v, want unknown/%v", res.Status, res.Reason, ReasonPatchesDeferredCommandBody)
	}
	assertPR591EmptyBuckets(t, res)
}

func assertPR591EmptyBuckets(t *testing.T, res Result) {
	t.Helper()
	if len(res.Applied) != 0 || len(res.Missing) != 0 || len(res.Orphaned) != 0 || len(res.Undecidable) != 0 || len(res.ConditionalNotApplied) != 0 {
		t.Fatalf("structural stop leaked result buckets: %+v", res)
	}
}

func TestPR591_MalformedBracketFailsClosed(t *testing.T) {
	dir := writePort(t, "vcpkg_from_github(PATCHES [=[unterminated.patch)\n", "unterminated.patch")
	res := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonPatchesExprUnparsable {
		t.Fatalf("malformed bracket status/reason = %v/%v, want unknown/%v", res.Status, res.Reason, ReasonPatchesExprUnparsable)
	}
	if len(res.Applied) != 0 || len(res.Missing) != 0 || len(res.Orphaned) != 0 {
		t.Fatalf("malformed bracket leaked partial classification: %+v", res)
	}
}

// TestPR591_PatchesProjectionBoundsOversizedIdentity proves package-local
// projection drops oversized identity fields with explicit field omissions and
// always fits the shared serializer's exact byte budget.
func TestPR591_PatchesProjectionBoundsOversizedIdentity(t *testing.T) {
	oversized := strings.Repeat("x", publicresult.MaxEncodedBytes)
	result := Result{Status: evidence.StatusUnknown, Reason: ReasonPortfileSizeLimitExceeded, Triplet: oversized, PortDir: oversized, TripletFile: oversized}
	projected, err := json.MarshalIndent(result.PublicResultProjection(), "", "  ")
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	if len(projected) > publicresult.MaxEncodedBytes {
		t.Fatalf("projected bytes=%d, limit=%d", len(projected), publicresult.MaxEncodedBytes)
	}

	var body struct {
		Status       evidence.Status `json:"status"`
		Reason       Reason          `json:"reason"`
		Triplet      string          `json:"triplet"`
		PatchResults json.RawMessage `json:"patch_results"`
		Projection   struct {
			Complete  bool                    `json:"complete"`
			Omissions []publicresult.Omission `json:"omissions"`
		} `json:"result_projection"`
	}
	if err := json.Unmarshal(projected, &body); err != nil {
		t.Fatalf("unmarshal projection: %v", err)
	}
	if body.Triplet != "" {
		t.Fatalf("oversized triplet survived projection: %d bytes", len(body.Triplet))
	}
	if body.Status != evidence.StatusUnknown || body.Reason != ReasonPortfileSizeLimitExceeded {
		t.Fatalf("projection status/reason = %v/%v, want unknown/%v", body.Status, body.Reason, ReasonPortfileSizeLimitExceeded)
	}
	if body.Projection.Complete {
		t.Fatal("result projection reported complete despite whole patch_results omission")
	}
	if body.PatchResults != nil {
		t.Fatalf("patch_results survived whole-result projection: %s", body.PatchResults)
	}
	omitted := map[string]publicresult.Omission{}
	for _, omission := range body.Projection.Omissions {
		omitted[omission.Field] = omission
	}
	for _, field := range []string{"triplet", "port_dir", "triplet_file"} {
		omission, ok := omitted[field]
		if !ok || omission.Reason != publicresult.InternalProjectionLimit || omission.Omitted == nil || *omission.Omitted != 1 {
			t.Fatalf("projection omissions = %+v, want %q/internal_projection_limit/1", body.Projection.Omissions, field)
		}
	}
}

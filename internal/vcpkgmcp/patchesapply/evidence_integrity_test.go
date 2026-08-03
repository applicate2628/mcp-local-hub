package patchesapply

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/boundedio"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Regression tests for the evidence-integrity findings in this package:
// cases where a filesystem FAILURE was reported as a verified fact about the
// port (a missing patch, or no orphans) under an overall `ok`.

var errDenied = fs.ErrPermission

// failingDeps wraps DefaultDeps, failing Stat and/or ReadDir for any path
// containing failSub with a NON-ErrNotExist error — the shape a permission
// denial, sharing violation or transient I/O error takes.
func failingDeps(failSub string, failStat, failReadDir bool) Deps {
	base := DefaultDeps()
	hit := func(p string) bool {
		return failSub != "" && strings.Contains(filepath.ToSlash(p), failSub)
	}
	d := base
	d.Stat = func(p string) (os.FileInfo, error) {
		if failStat && hit(p) {
			return nil, errDenied
		}
		return base.Stat(p)
	}
	d.OpenDir = func(p string) (boundedio.DirReader, error) {
		if failReadDir && hit(p) {
			return nil, errDenied
		}
		return base.OpenDir(p)
	}
	return d
}

func findUnreadable(res Result, kind UnreadableKind) *UnreadablePath {
	for i := range res.Unreadable {
		if res.Unreadable[i].Kind == kind {
			return &res.Unreadable[i]
		}
	}
	return nil
}

const oneUnconditionalPatchPortfile = `
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO foo/bar
    REF v1.0.0
    SHA512 0
    PATCHES
        only.patch
)
`

// --- F14: a Stat failure is not a missing patch --------------------------

// TestApplyOrder_UnreadablePatchPath_NotReportedAsMissing: the patch file
// EXISTS, but its Stat is denied. Reporting it in `missing` turns an
// environment problem (an ACL, a lock) into a false bug report against the
// port — and the call still returned ok, so nothing signalled the doubt.
func TestApplyOrder_UnreadablePatchPath_NotReportedAsMissing(t *testing.T) {
	portDir := writeFixture(t, oneUnconditionalPatchPortfile, "only.patch")

	// Baseline: fully readable -> ok, exists, nothing missing.
	base := applyOrder(Args{PortDir: portDir, Triplet: "x64-windows"}, DefaultDeps())
	if base.Status != evidence.StatusOK || len(base.Missing) != 0 {
		t.Fatalf("baseline: status=%v missing=%+v, want ok with nothing missing", base.Status, base.Missing)
	}
	if a := findApplied(base, "only.patch"); a == nil || a.Existence != evidence.PresenceExists {
		t.Fatalf("baseline: applied entry = %+v, want existence=exists", a)
	}

	res := applyOrder(Args{PortDir: portDir, Triplet: "x64-windows"}, failingDeps("only.patch", true, false))

	for _, m := range res.Missing {
		if strings.Contains(m.Filename, "only.patch") {
			t.Fatal("an UNREADABLE patch path was reported as missing — a permission error " +
				"is not evidence that the file is absent")
		}
	}
	if res.Status == evidence.StatusOK {
		t.Fatal("status stayed ok while a declared patch path could not be probed")
	}
	if res.Reason != ReasonPatchPathUnreadable {
		t.Fatalf("reason = %v, want %v", res.Reason, ReasonPatchPathUnreadable)
	}
	u := findUnreadable(res, UnreadablePatchPath)
	if u == nil {
		t.Fatalf("unreadable bucket = %+v, want the patch path recorded", res.Unreadable)
	}
	if !strings.Contains(u.Path, "only.patch") {
		t.Errorf("unreadable path = %q, want it to name only.patch", u.Path)
	}
	// The tri-state existence must say "unreadable", never "absent".
	if a := findApplied(res, "only.patch"); a == nil || a.Existence != evidence.PresenceUnreadable {
		t.Fatalf("applied entry = %+v, want existence=unreadable", a)
	}
}

// TestApplyOrder_GenuinelyAbsentPatchStillMissing: the distinction must cut
// both ways — a VERIFIED absence is still a real defect report.
func TestApplyOrder_GenuinelyAbsentPatchStillMissing(t *testing.T) {
	portDir := writeFixture(t, oneUnconditionalPatchPortfile) // no patch file written

	res := applyOrder(Args{PortDir: portDir, Triplet: "x64-windows"}, DefaultDeps())
	if len(res.Missing) != 1 || !strings.Contains(res.Missing[0].Filename, "only.patch") {
		t.Fatalf("missing = %+v, want the genuinely absent patch reported", res.Missing)
	}
	if len(res.Unreadable) != 0 {
		t.Errorf("unreadable = %+v, want empty for a verified absence", res.Unreadable)
	}
	if a := findApplied(res, "only.patch"); a == nil || a.Existence != evidence.PresenceAbsent {
		t.Fatalf("applied entry = %+v, want existence=absent", a)
	}
}

// --- F15: an unlistable directory is not "no orphans" --------------------

// TestApplyOrder_UnreadableOrphanDir_ReportsIncompleteScan: a nested
// directory holding unreferenced patches cannot be listed. Previously the
// walk returned early and the empty result was indistinguishable from a
// verified "no orphans", under an overall ok.
func TestApplyOrder_UnreadableOrphanDir_ReportsIncompleteScan(t *testing.T) {
	portDir := writeFixture(t, oneUnconditionalPatchPortfile,
		"only.patch",
		filepath.Join("nested", "stray.patch"),
	)

	// Baseline: the nested orphan IS found when the directory is readable.
	base := applyOrder(Args{PortDir: portDir, Triplet: "x64-windows"}, DefaultDeps())
	if findOrphaned(base, "stray.patch") == nil {
		t.Fatalf("baseline: nested orphan not found; orphaned=%+v", base.Orphaned)
	}
	if base.Status != evidence.StatusOK {
		t.Fatalf("baseline status = %v, want ok", base.Status)
	}

	res := applyOrder(Args{PortDir: portDir, Triplet: "x64-windows"}, failingDeps("/nested", false, true))

	if res.Status == evidence.StatusOK {
		t.Fatal("status stayed ok while part of the orphan scan could not be listed — " +
			"an empty orphan list then reads as a verified \"no orphans\"")
	}
	if res.Reason != ReasonOrphanScanIncomplete {
		t.Fatalf("reason = %v, want %v", res.Reason, ReasonOrphanScanIncomplete)
	}
	u := findUnreadable(res, UnreadableOrphanDir)
	if u == nil {
		t.Fatalf("unreadable bucket = %+v, want the unlistable directory recorded", res.Unreadable)
	}
	if !strings.Contains(filepath.ToSlash(u.Path), "/nested") {
		t.Errorf("unreadable path = %q, want it to name the nested directory", u.Path)
	}
	// The partial inventory must be preserved, not discarded.
	if len(res.Applied) == 0 {
		t.Error("the applied bucket must survive an incomplete orphan scan")
	}
}

// --- F4: an unreadable triplet file ---------------------------------------

// TestApplyOrder_UnreadableTripletFile_ReportsUnknown: the triplet file that
// governs every guard exists but cannot be read, so the guard verdicts rest
// on variables that are unknown for a FIXABLE reason. That must be visible.
func TestApplyOrder_UnreadableTripletFile_ReportsUnknown(t *testing.T) {
	portDir := writeFixture(t, staticGuardPortfile, "static-only.patch")
	tripletDir := writeTripletDir(t, "corp-windows", "set(VCPKG_LIBRARY_LINKAGE static)\n")

	args := Args{PortDir: portDir, Triplet: "corp-windows", OverlayTriplets: []string{tripletDir}}

	// Baseline: readable -> ok and the guard resolves.
	base := applyOrder(args, DefaultDeps())
	if base.Status != evidence.StatusOK || findApplied(base, "static-only.patch") == nil {
		t.Fatalf("baseline: status=%v result=%+v", base.Status, base)
	}

	// Stat succeeds (the file IS found on the lookup path) but ReadFile is
	// denied — the case where the tool knows exactly which file governs and
	// still cannot read it.
	deps := DefaultDeps()
	deps.ReadFile = func(p string) ([]byte, error) {
		if strings.Contains(filepath.ToSlash(p), "corp-windows.cmake") {
			return nil, errDenied
		}
		return DefaultDeps().ReadFile(p)
	}
	res := applyOrder(args, deps)

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonTripletFileUnreadable {
		t.Fatalf("got status=%v reason=%v, want unknown/triplet_file_unreadable; result=%+v",
			res.Status, res.Reason, res)
	}
	if findUnreadable(res, UnreadableTripletFile) == nil {
		t.Fatalf("unreadable = %+v, want the triplet file recorded", res.Unreadable)
	}
	// With the facts unknown, the guard must be undecidable — never guessed.
	if findApplied(res, "static-only.patch") != nil {
		t.Error("a guard was resolved despite the governing triplet file being unreadable")
	}
	if findUndecidable(res, "static-only.patch") == nil {
		t.Errorf("static-only.patch must be undecidable; result=%+v", res)
	}

	t.Run("parser stop retains triplet evidence and precedence", func(t *testing.T) {
		malformedPortDir := writeFixture(t, "vcpkg_from_github(PATCHES static-only.patch\n")
		parserStop := applyOrder(Args{PortDir: malformedPortDir, Triplet: "corp-windows", OverlayTriplets: []string{tripletDir}}, deps)
		if parserStop.Status != evidence.StatusUnknown || parserStop.Reason != ReasonTripletFileUnreadable {
			t.Fatalf("parser-stop status/reason = %v/%v, want unknown/%v", parserStop.Status, parserStop.Reason, ReasonTripletFileUnreadable)
		}
		if parserStop.TripletFile == "" || findUnreadable(parserStop, UnreadableTripletFile) == nil {
			t.Fatalf("parser-stop triplet metadata was lost: triplet_file=%q unreadable=%+v", parserStop.TripletFile, parserStop.Unreadable)
		}
		if !containsStr(parserStop.Evidence.Paths, parserStop.TripletFile) {
			t.Fatalf("parser-stop evidence paths = %v, want located triplet file %q", parserStop.Evidence.Paths, parserStop.TripletFile)
		}
		if len(parserStop.Applied) != 0 || len(parserStop.Orphaned) != 0 {
			t.Fatalf("parser stop probed patch or orphan buckets: %+v", parserStop)
		}
	})
}

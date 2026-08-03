package cmakegraph

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func fileInResult(res *Result, path string) bool {
	clean := filepath.Clean(path)
	for _, f := range res.Files {
		if strings.EqualFold(filepath.Clean(f), clean) {
			return true
		}
	}
	return false
}

func coverageReasonFor(res *Result, path string) (CoverageReason, bool) {
	clean := filepath.Clean(path)
	for _, u := range res.UnscannedFiles {
		if strings.EqualFold(filepath.Clean(u.Path), clean) {
			return u.Reason, true
		}
	}
	return "", false
}

// PR #591 P2 (cmakegraph.go walkRoot / resolve loop): a file was inserted into
// w.files — the "these were scanned" list — BEFORE readBounded was even
// attempted. A root that exceeded MaxFileBytes therefore appeared in files[]
// as scanned AND in unscanned_files[] as a coverage hole at the same time.
// files[] is what a caller trusts to mean "this was examined"; reporting a
// file as both is worse than either alone.
func TestOversizedRootIsNotReportedAsScanned(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("# padding comment line\n", 400) // comfortably over the cap below
	root := writeFile(t, dir, "CMakeLists.txt", big)

	opts := DefaultOptions()
	opts.MaxFileBytes = 64 // far below the file written above

	res, err := Walk(context.Background(), root, dir, opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if fileInResult(res, realpath(t, root)) {
		t.Fatalf("files = %v, want the over-cap root ABSENT — it was listed as scanned even though the read never succeeded", res.Files)
	}
	reason, ok := coverageReasonFor(res, realpath(t, root))
	if !ok {
		t.Fatalf("unscanned_files = %+v, want the over-cap root recorded as a coverage hole", res.UnscannedFiles)
	}
	if reason != CoverageByteCapExceeded {
		t.Fatalf("coverage reason = %v, want %v", reason, CoverageByteCapExceeded)
	}
}

// The same rule on an INCLUDE TARGET, not just the root: the resolve loop had
// its own w.files insertion with the same defect.
func TestOversizedIncludeTargetIsNotReportedAsScanned(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "CMakeLists.txt", "include(Big.cmake)\n")
	big := writeFile(t, dir, "Big.cmake", strings.Repeat("# padding comment line\n", 400))

	opts := DefaultOptions()
	// Large enough for the tiny root, far too small for Big.cmake.
	opts.MaxFileBytes = 64

	res, err := Walk(context.Background(), root, dir, opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if fileInResult(res, realpath(t, big)) {
		t.Fatalf("files = %v, want the over-cap include target ABSENT — it was listed as scanned though its read failed", res.Files)
	}
	if _, ok := coverageReasonFor(res, realpath(t, big)); !ok {
		t.Fatalf("unscanned_files = %+v, want the over-cap include target recorded as a coverage hole", res.UnscannedFiles)
	}
}

// The equal-and-opposite guard: a file that IS read successfully must still be
// reported as scanned. Emptying files[] would be its own defect.
func TestSuccessfullyReadFilesAreStillReportedAsScanned(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "CMakeLists.txt", "include(Small.cmake)\n")
	small := writeFile(t, dir, "Small.cmake", "# tiny\n")

	res, err := Walk(context.Background(), root, dir, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !fileInResult(res, realpath(t, root)) {
		t.Fatalf("files = %v, want the root listed as scanned", res.Files)
	}
	if !fileInResult(res, realpath(t, small)) {
		t.Fatalf("files = %v, want the successfully-read include target listed as scanned", res.Files)
	}
	if len(res.UnscannedFiles) != 0 {
		t.Fatalf("unscanned_files = %+v, want empty — nothing failed here", res.UnscannedFiles)
	}
}

// PR #591 P2 (cmakegraph.go readBounded): io.LimitReader(f, maxBytes) returns
// exactly maxBytes with a NIL error when the file grew after Stat, making a
// truncated read byte-for-byte indistinguishable from a complete one. The
// graph was then built from a silently cut file and reported as full coverage.
//
// The growth race cannot be reproduced through the filesystem deterministically
// — by the time a test could append, the read has happened — so this drives
// readBoundedFrom, the post-Stat read, with a reader that yields MORE than the
// preceding Stat promised. That is precisely what a grown file looks like from
// there, and it is the only way to prove the BOUND rather than merely
// re-asserting the Stat gate in front of it (which is why the pre-fix code
// passed a whole-file test while still being unable to detect truncation).
func TestReadBoundedFromDetectsAFileThatGrewPastTheCap(t *testing.T) {
	const capBytes = 64

	// Exactly at the cap: a clean, complete read.
	data, err := readBoundedFrom(strings.NewReader(strings.Repeat("a", capBytes)), "exact.cmake", capBytes)
	if err != nil {
		t.Fatalf("readBoundedFrom at exactly the cap returned %v, want a clean read", err)
	}
	if len(data) != capBytes {
		t.Fatalf("read %d bytes at the cap, want %d", len(data), capBytes)
	}

	// One byte over — the shape a file that grew after Stat presents.
	data, err = readBoundedFrom(strings.NewReader(strings.Repeat("a", capBytes+1)), "grown.cmake", capBytes)
	if err == nil {
		t.Fatalf("readBoundedFrom returned %d bytes and a NIL error for a reader that exceeded the cap: a truncated read is byte-for-byte indistinguishable from a complete one, so the graph is built from a silently cut file and reported as full coverage",
			len(data))
	}
	if readCoverageReason(err) != CoverageByteCapExceeded {
		t.Fatalf("coverage reason = %v, want %v", readCoverageReason(err), CoverageByteCapExceeded)
	}

	// And the whole-file path still rejects an over-cap file at the Stat gate.
	dir := t.TempDir()
	over := writeFile(t, dir, "over.cmake", strings.Repeat("a", capBytes+1))
	if _, err := readBounded(over, capBytes); err == nil {
		t.Fatalf("readBounded accepted a file already over the cap on disk")
	}
}

// PR #591 P2 (cmakegraph.go firstArgument): scanArgList understands both CMake
// comment forms, but firstArgument only skipped whitespace — so a well-formed
// call carrying a comment before its first argument yielded "#" as the
// argument and the edge was misparsed.
func TestFirstArgumentSkipsCommentsBeforeTheArgument(t *testing.T) {
	cases := []struct {
		name    string
		argText string
		want    string
	}{
		{"line comment before argument", "# pick the variant\n  Foo.cmake", "Foo.cmake"},
		{"bracket comment before argument", "#[[ explain ]] Foo.cmake", "Foo.cmake"},
		{"comment then quoted argument", "# note\n \"Foo.cmake\"", "Foo.cmake"},
		{"no comment at all", " Foo.cmake ", "Foo.cmake"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := firstArgument([]byte(tc.argText))
			if !ok {
				t.Fatalf("firstArgument(%q) reported malformed, want %q", tc.argText, tc.want)
			}
			if got != tc.want {
				t.Fatalf("firstArgument(%q) = %q, want %q — a comment is not an argument", tc.argText, got, tc.want)
			}
		})
	}
}

// End-to-end form of the same fix: the edge must actually RESOLVE, not just
// parse. This is what a caller sees.
func TestIncludeWithALeadingCommentResolves(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "CMakeLists.txt", "include( # pick the platform variant\n        Foo.cmake)\n")
	writeFile(t, dir, "Foo.cmake", "# target\n")

	res, err := Walk(context.Background(), root, dir, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %+v, want exactly 1", res.Edges)
	}
	if res.Edges[0].Status != StatusResolved {
		t.Fatalf("edge status = %v (raw_arg %q, reason %v), want resolved — the leading comment was parsed as the argument",
			res.Edges[0].Status, res.Edges[0].RawArg, res.Edges[0].Reason)
	}
	if res.Edges[0].RawArg != "Foo.cmake" {
		t.Fatalf("raw_arg = %q, want Foo.cmake", res.Edges[0].RawArg)
	}
}

func TestPR591_LoopNestedEdgesAreConditional(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "CMakeLists.txt", "foreach(item IN ITEMS one)\n"+
		"  include(loop.cmake)\n"+
		"  while(LOOP_AGAIN)\n"+
		"    add_subdirectory(child)\n"+
		"  endwhile()\n"+
		"endforeach()\n"+
		"include(outside.cmake)\n")
	writeFile(t, dir, "loop.cmake", "# leaf\n")
	writeFile(t, dir, "child/CMakeLists.txt", "# leaf\n")
	writeFile(t, dir, "outside.cmake", "# leaf\n")

	res, err := Walk(context.Background(), root, dir, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, line := range []int{2, 4} {
		e := edgeByLine(t, res, line)
		if !e.Conditional || e.Status != StatusResolved {
			t.Fatalf("edge on line %d = {%v, Conditional:%v}, want {Resolved, Conditional:true}; loop frames are lexical uncertainty, not executed", line, e.Status, e.Conditional)
		}
	}
	if e := edgeByLine(t, res, 7); e.Conditional || e.Status != StatusResolved {
		t.Fatalf("edge after endforeach on line 7 = {%v, Conditional:%v}, want {Resolved, Conditional:false}", e.Status, e.Conditional)
	}
}

type fakeWalkDirEntry struct {
	name string
	mode fs.FileMode
	dir  bool
}

func (e fakeWalkDirEntry) Name() string      { return e.name }
func (e fakeWalkDirEntry) IsDir() bool       { return e.dir }
func (e fakeWalkDirEntry) Type() fs.FileMode { return e.mode }
func (e fakeWalkDirEntry) Info() (fs.FileInfo, error) {
	return nil, errors.New("fake walk entry has no file info")
}

func TestPR591_DirectorySymlinkIsNotDescended(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "linked")
	var skipDirReturned bool

	res, err := walkTreeWithOperations(context.Background(), root, root, []string{"*.cmake"}, DefaultOptions(), treeOperations{
		walkDir: func(_ string, visit fs.WalkDirFunc) error {
			if err := visit(root, fakeWalkDirEntry{name: filepath.Base(root), dir: true}, nil); err != nil {
				return err
			}
			if err := visit(link, fakeWalkDirEntry{name: "linked", mode: fs.ModeSymlink, dir: true}, nil); err != nil {
				skipDirReturned = errors.Is(err, fs.SkipDir)
				return nil
			}
			return nil
		},
		isDirectorySymlink: func(path string) bool { return path == link },
		walkRoot:           (*walker).walkRoot,
	})
	if err != nil {
		t.Fatalf("walkTreeWithOperations: %v", err)
	}
	if !skipDirReturned {
		t.Fatal("directory symlink callback did not return fs.SkipDir; its subtree could be descended")
	}
	if reason, ok := coverageReasonFor(res, link); !ok || reason != CoverageSymlinkDirectorySkipped {
		t.Fatalf("coverage for %q = (%q, %t), want (%q, true)", link, reason, ok, CoverageSymlinkDirectorySkipped)
	}
}

func TestPR591_CancellationAfterFinalRootReturnsError(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "only.cmake")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	processed := false

	res, err := walkTreeWithOperations(ctx, root, root, []string{"*.cmake"}, DefaultOptions(), treeOperations{
		walkDir: func(_ string, visit fs.WalkDirFunc) error {
			if err := visit(root, fakeWalkDirEntry{name: filepath.Base(root), dir: true}, nil); err != nil {
				return err
			}
			return visit(entry, fakeWalkDirEntry{name: "only.cmake"}, nil)
		},
		isDirectorySymlink: func(string) bool { return false },
		walkRoot: func(_ *walker, start string) (string, error) {
			if start != entry {
				t.Fatalf("root operation received %q, want %q", start, entry)
			}
			processed = true
			defer cancel() // The only selected root succeeds, then the final boundary observes cancellation.
			return start, nil
		},
	})
	if !processed {
		t.Fatal("the injected final root operation was not called")
	}
	if res != nil {
		t.Fatalf("result = %+v, want nil after cancellation", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want wrapping context.Canceled after the final root", err)
	}
}

type readCountingReader struct{ reads int }

func (r *readCountingReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

func TestPR591_MaxInt64FileCapFailsBeforeIO(t *testing.T) {
	reader := &readCountingReader{}
	if _, err := readBoundedFrom(reader, "never-read.cmake", math.MaxInt64); err == nil {
		t.Fatal("readBoundedFrom accepted MaxInt64 even though maxBytes+1 is unrepresentable")
	}
	if reader.reads != 0 {
		t.Fatalf("reader calls = %d, want 0: an unrepresentable sentinel cap must fail before I/O", reader.reads)
	}

	root := t.TempDir()
	writeFile(t, root, "CMakeLists.txt", "# leaf\n")
	opts := DefaultOptions()
	opts.MaxFileBytes = math.MaxInt64
	if _, err := Walk(context.Background(), filepath.Join(root, "CMakeLists.txt"), root, opts); err == nil {
		t.Fatal("Walk admitted MaxInt64 MaxFileBytes even though its sentinel cannot be represented")
	}
}

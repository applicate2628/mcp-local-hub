package lastfailure

import (
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// oneFileFS serves exactly one path and refuses everything else, so the test
// exercises the wrapper branch alone.
type oneFileFS struct {
	path string
	data []byte
}

func (f oneFileFS) Stat(string) (os.FileInfo, error)  { return nil, fs.ErrNotExist }
func (f oneFileFS) OpenDir(string) (DirReader, error) { return nil, fs.ErrNotExist }

func (f oneFileFS) ReadFile(p string) ([]byte, error) {
	if p == f.path {
		return f.data, nil
	}
	return nil, fs.ErrNotExist
}

func (f oneFileFS) Open(p string) (io.ReadCloser, error) {
	if p == f.path {
		return io.NopCloser(strings.NewReader(string(f.data))), nil
	}
	return nil, fs.ErrNotExist
}

// A wrapper whose scan ABORTS must not be reported as malformed.
//
// ParseWrapperContent's bufio.Scanner caps a line at 4 MiB; past that it stops
// and returns an error, so the file was NOT read end to end. Discarding that
// error (`wrapperInfo, wrapperOK, _ = ParseWrapperContent(data)`) dropped the
// caller into NoteWrapperMalformed, whose contract in types.go is "the wrapper
// file WAS read end to end and nothing recognizable was recovered — a verified
// fact about content". That is a fabricated fact about a file abandoned mid-read.
//
// The package already documents why exactly this conflation is harmful, in
// NoteWrapperUnreadable's own comment: folding the two "sent the operator to fix
// a wrapper script when the actual remedy is an ACL".
func TestWrapperScanAbort_IsNotReportedAsMalformed(t *testing.T) {
	// One line past the scanner's 4 MiB ceiling, and unrecognizable, so `ok` is
	// false for BOTH possible reasons — only the error tells them apart.
	oversize := []byte(strings.Repeat("x", 5*1024*1024))

	if _, ok, err := ParseWrapperContent(oversize); ok || err == nil {
		t.Fatalf("precondition failed: a 5 MiB single line must parse !ok WITH an error, otherwise this test "+
			"cannot distinguish the two notes and would pass vacuously; got ok=%v err=%v", ok, err)
	}

	const wrapperPath = "buildtrees/wrap.log"
	res := LastFailure(
		Args{BuildFailedLog: wrapperPath},
		Deps{FS: oneFileFS{path: wrapperPath, data: oversize}},
	)

	var sawMalformed, sawLimit bool
	for _, n := range res.Notes {
		switch n {
		case NoteWrapperMalformed:
			sawMalformed = true
		case NoteProducerLimitEngaged:
			sawLimit = true
		}
	}
	if sawMalformed {
		t.Fatalf("an ABORTED scan was reported as %q, whose contract is that the file was read END TO END; notes=%v",
			NoteWrapperMalformed, res.Notes)
	}
	if res.Reason != ReasonMetadataLimitExceeded || !sawLimit {
		t.Fatalf("the production path must stop at the metadata sentinel as unknown(%s), before feeding an "+
			"oversize blob to the parser; status=%s reason=%s notes=%v", ReasonMetadataLimitExceeded,
			res.Status, res.Reason, res.Notes)
	}
}

// The complement: a genuinely readable but unrecognizable wrapper is STILL
// malformed, so the fix cannot buy correctness by relabelling every wrapper
// failure as unreadable.
func TestWrapperReadableButUnrecognizable_IsStillMalformed(t *testing.T) {
	small := []byte("nothing recognizable here\nsecond line\n")

	if _, ok, err := ParseWrapperContent(small); ok || err != nil {
		t.Fatalf("precondition failed: this blob must parse !ok with NO error; got ok=%v err=%v", ok, err)
	}

	const wrapperPath = "buildtrees/wrap.log"
	res := LastFailure(
		Args{BuildFailedLog: wrapperPath},
		Deps{FS: oneFileFS{path: wrapperPath, data: small}},
	)

	for _, n := range res.Notes {
		if n == NoteWrapperMalformed {
			return
		}
	}
	t.Fatalf("a file read end to end with nothing recognizable in it IS malformed; notes=%v", res.Notes)
}

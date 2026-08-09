package cmaketrace

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// recordingFS records every path it was asked to probe, so a test can assert
// that the tool did not look ANYWHERE — which is the whole difference between
// "you did not supply a path" and "the path you supplied is absent".
type recordingFS struct{ opened []string }

func (f *recordingFS) OpenRegular(p string) (io.ReadCloser, fs.FileInfo, error) {
	f.opened = append(f.opened, p)
	return nil, nil, fs.ErrNotExist
}

// An absent trace_path must be reported as a fact about the CALL, never as a
// verified absence on the filesystem.
//
// Before ReasonTracePathNotSupplied existed, an empty trace_path fell straight
// into deps.FS.Open(""), whose error IS fs.ErrNotExist on every supported
// platform, so Trace classified it as unknown(trace_not_found). That reason's
// contract — and the server README's own vocabulary rule — is that
// `..._not_found` is a "verified absence. This is a real finding.", while
// `..._not_supplied` means "you did not pass something. Pass it." The tool was
// handing the operator a real finding about a path they never named, and
// Evidence.Paths was EMPTY (AddPath("") is a no-op), so the answer did not even
// say which path was supposedly missing.
func TestTrace_TracePathNotSupplied_IsNotAVerifiedAbsence(t *testing.T) {
	// Precondition: the two cases really are indistinguishable at the FS
	// layer, otherwise this test would pass for the wrong reason.
	if _, err := os.Open(""); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("precondition failed: os.Open(\"\") must report fs.ErrNotExist for this defect to be reachable; got %v", err)
	}

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "field omitted / empty", path: ""},
		{name: "whitespace only names no file either", path: "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &recordingFS{}
			res := Trace(context.Background(), Args{TracePath: tc.path}, Deps{FS: spy})

			if res.Reason == ReasonTraceNotFound {
				t.Fatalf("an unsupplied trace_path was reported as %q, whose contract is a VERIFIED ABSENCE at a path "+
					"we looked for — the operator is sent to hunt for a file they never named", ReasonTraceNotFound)
			}
			if res.Reason != ReasonTracePathNotSupplied {
				t.Fatalf("Reason = %q, want %q", res.Reason, ReasonTracePathNotSupplied)
			}
			if res.Status != evidence.StatusUnknown {
				t.Fatalf("Status = %q, want unknown", res.Status)
			}
			if len(spy.opened) != 0 {
				t.Fatalf("the filesystem was consulted at %v; with no path supplied the tool must not look anywhere, "+
					"which is exactly what distinguishes not_supplied from a verified absence", spy.opened)
			}
		})
	}
}

// The complement: a path that WAS supplied and is genuinely absent must still
// be a verified absence, or the fix would have bought correctness by making
// every missing trace un-findable.
func TestTrace_SuppliedButAbsentPath_IsStillAVerifiedAbsence(t *testing.T) {
	spy := &recordingFS{}
	wanted := filepath.Join(t.TempDir(), "does-not-exist-trace.json")
	res := Trace(context.Background(), Args{TracePath: wanted}, Deps{FS: spy})

	if res.Reason != ReasonTraceNotFound {
		t.Fatalf("Reason = %q, want %q — a supplied, absent path IS a real finding", res.Reason, ReasonTraceNotFound)
	}
	if len(spy.opened) != 1 || spy.opened[0] != wanted {
		t.Fatalf("opened = %v, want exactly [%q]: the absence claim is only honest if we actually looked", spy.opened, wanted)
	}
	var sawPath bool
	for _, p := range res.Evidence.Paths {
		if p == wanted {
			sawPath = true
		}
	}
	if !sawPath {
		t.Fatalf("Evidence.Paths = %v, want it to name %q — an absence claim must say where it looked", res.Evidence.Paths, wanted)
	}
}

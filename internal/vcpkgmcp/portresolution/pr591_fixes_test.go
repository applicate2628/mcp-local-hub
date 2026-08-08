package portresolution

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/boundedio"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// PR #591 P2: only an EMPTY port was rejected, so a traversal name was
// normalised BY filepath.Join into a path outside the overlay root, and that
// path was then stat'ed, listed and reported as a candidate location. That
// turns a port LOOKUP into an arbitrary-directory probe against anything the
// hub daemon can read.
//
// The assertion that matters is not just the reason: it is that NO candidate
// path outside the granted root is ever recorded or probed.
//
// VACUOUS-TEST FIX (2026-07-27): that assertion used to be two loops over
// res.AllCandidates and res.Evidence.Paths. Both are EMPTY on the refusal path
// — measured: every one of the nine names below returns
// failed(invalid_port_name) with AllCandidates=0 and Evidence.Paths=0 — so the
// loop bodies never executed and the "security assertion" could not fail. It
// would have stayed green if the gate were deleted and the join relaxed, as
// long as the reason string was still produced.
//
// It now binds through a SPY Deps: the refusal must happen before the join, so
// the filesystem must never be consulted at all. That is the property the test
// is named for, and it is falsifiable.
//
// It binds on the PROPERTY at the consumer boundary. The shared portname leaf
// owns grammar and containment; this consumer test proves it invokes that owner
// before any filesystem access. Removing the consumer call makes it fail with
// the laundered path in the message, e.g.
//
//	port "sub/nested" caused 3 filesystem probe(s) despite being refused:
//	[stat <overlay> readdir <overlay> stat <overlay>\sub\nested]
//
// (the four `..`-shaped names are also rejected by the leaf containment check.)
func TestTraversalPortNameIsRefusedBeforeTheJoin(t *testing.T) {
	overlay := t.TempDir()

	for _, bad := range []string{
		"../../outside",
		"..",
		"../sibling",
		`..\windows-sibling`,
		"sub/nested",
		"UPPERCASE",
		"-leading-hyphen",
		"trailing-hyphen-",
		"under_score",
	} {
		t.Run(bad, func(t *testing.T) {
			var probed []string
			deps := DefaultDeps()
			realStat, realOpen, realOpenDir := deps.Stat, deps.Open, deps.OpenDir
			deps.Stat = func(p string) (os.FileInfo, error) {
				probed = append(probed, "stat "+p)
				return realStat(p)
			}
			deps.Open = func(p string) (io.ReadCloser, error) {
				probed = append(probed, "open "+p)
				return realOpen(p)
			}
			deps.OpenDir = func(p string) (boundedio.DirReader, error) {
				probed = append(probed, "opendir "+p)
				return realOpenDir(p)
			}

			res := ResolvePort(Args{Port: bad, OverlayPorts: []string{overlay}}, deps)

			// THE security assertion, and the one that binds: an illegal port
			// name is refused BEFORE the join, so nothing is probed at all.
			// filepath.Join would have normalised "../../outside" into a real
			// directory outside the granted root, and that directory would then
			// have been stat'ed and listed.
			if len(probed) != 0 {
				t.Fatalf("port %q caused %d filesystem probe(s) despite being refused: %v — the name reached "+
					"filepath.Join, which is exactly the arbitrary-directory probe this gate exists to prevent",
					bad, len(probed), probed)
			}

			if res.Status != evidence.StatusFailed {
				t.Fatalf("status = %v, want failed for port %q — an illegal port name is bad caller input", res.Status, bad)
			}
			if res.Reason != ReasonInvalidPortName {
				t.Fatalf("reason = %v, want %v for port %q", res.Reason, ReasonInvalidPortName, bad)
			}
			if res.InvalidPort != bad {
				t.Fatalf("invalid_port = %q, want %q echoed back", res.InvalidPort, bad)
			}
			// Nothing was recorded either. Asserted as a COUNT, not as a loop
			// over the (empty) slices: a loop body that never runs cannot
			// report a regression, which is how the original version of this
			// check passed vacuously.
			if len(res.AllCandidates) != 0 {
				t.Fatalf("refused port %q still recorded %d candidate location(s): %+v", bad, len(res.AllCandidates), res.AllCandidates)
			}
			if len(res.Evidence.Paths) != 0 {
				t.Fatalf("refused port %q still recorded %d evidence path(s): %v", bad, len(res.Evidence.Paths), res.Evidence.Paths)
			}
		})
	}
}

func TestResolvePortRejectsTooManyRawOverlayRootsBeforeAllocationOrFilesystem(t *testing.T) {
	called := false
	deps := Deps{Stat: func(string) (os.FileInfo, error) { called = true; return nil, os.ErrNotExist }}
	overlays := make([]string, MaxOverlayRoots+1)
	for i := range overlays {
		overlays[i] = " " // blanks are still raw request entries
	}
	res := ResolvePort(Args{Port: "zlib", OverlayPorts: overlays}, deps)
	if res.Status != evidence.StatusFailed || res.Reason != ReasonTooManyOverlayRoots || called {
		t.Fatalf("result=%+v filesystem_called=%v, want failed/too_many_overlay_roots/no filesystem", res, called)
	}
}

func TestInspectPortCandidateValidatesManifestTargetMatrix(t *testing.T) {
	makeLink := func(t *testing.T, target, link string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			if runtime.GOOS == "windows" && (errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.Errno(50)) || errors.Is(err, syscall.Errno(1314))) {
				t.Skipf("real symlink unsupported without privilege: %v", err)
			}
			t.Fatalf("create symlink: %v", err)
		}
	}

	for _, tc := range []struct {
		name      string
		configure func(*testing.T, string, string)
		want      probeKind
	}{
		{"regular target", func(t *testing.T, base, port string) {
			target := filepath.Join(base, "target.cmake")
			if err := os.WriteFile(target, []byte("# port"), 0o644); err != nil {
				t.Fatal(err)
			}
			makeLink(t, target, filepath.Join(port, "portfile.cmake"))
		}, probeFound},
		{"dangling target", func(t *testing.T, base, port string) {
			makeLink(t, filepath.Join(base, "missing.cmake"), filepath.Join(port, "portfile.cmake"))
		}, probeAbsent},
		{"directory target", func(t *testing.T, base, port string) {
			target := filepath.Join(base, "target-dir")
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
			makeLink(t, target, filepath.Join(port, "portfile.cmake"))
		}, probeAbsent},
		{"invalid then readable sibling", func(t *testing.T, base, port string) {
			makeLink(t, filepath.Join(base, "missing.cmake"), filepath.Join(port, "portfile.cmake"))
			if err := os.WriteFile(filepath.Join(port, "vcpkg.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, probeFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			port := filepath.Join(base, "port")
			if err := os.Mkdir(port, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.configure(t, base, port)
			if outcome := inspectPortCandidate(context.Background(), DefaultDeps(), port); outcome.kind != tc.want {
				t.Fatalf("outcome=%+v, want kind=%v", outcome, tc.want)
			}
		})
	}
}

type pr591ProbeFile struct {
	data       string
	requests   []int
	closeCount int
	readErr    error
	closeErr   error
}

func (f *pr591ProbeFile) Read(p []byte) (int, error) {
	f.requests = append(f.requests, len(p))
	if f.readErr != nil {
		return 0, f.readErr
	}
	if f.data == "" {
		return 0, io.EOF
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}
func (f *pr591ProbeFile) Close() error { f.closeCount++; return f.closeErr }

type pr591ProbeDir struct {
	requests   []int
	closeCount int
	closeErr   error
}

func (d *pr591ProbeDir) ReadDir(n int) ([]os.DirEntry, error) {
	d.requests = append(d.requests, n)
	return nil, io.EOF
}
func (d *pr591ProbeDir) Close() error { d.closeCount++; return d.closeErr }

func TestInspectRootReadabilityProbeRequestsOneEntryAndCloses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		closeErr error
		want     probeKind
	}{
		{"success", nil, probeFound}, {"close error", errors.New("close root"), probeUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := &pr591ProbeDir{closeErr: tc.closeErr}
			deps := Deps{Stat: func(string) (os.FileInfo, error) { return newFakeStat("root", true), nil }, OpenDir: func(string) (boundedio.DirReader, error) { return dir, nil }}
			if outcome := inspectRoot(context.Background(), deps, t.TempDir()); outcome.kind != tc.want {
				t.Fatalf("outcome=%+v, want %v", outcome, tc.want)
			}
			if len(dir.requests) != 1 || dir.requests[0] != 1 || dir.closeCount != 1 {
				t.Fatalf("requests=%v closes=%d, want [1]/1", dir.requests, dir.closeCount)
			}
		})
	}
}

func TestManifestProbeReadsOneSentinelByteAndCloses(t *testing.T) {
	readFailure, closeFailure := errors.New("read manifest"), errors.New("close manifest")
	for _, tc := range []struct {
		name              string
		readErr, closeErr error
		want              probeKind
	}{
		{"success", nil, nil, probeFound}, {"read error", readFailure, nil, probeUnreadable}, {"close error", nil, closeFailure, probeUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := filepath.Join(t.TempDir(), "port")
			file := &pr591ProbeFile{data: "x", readErr: tc.readErr, closeErr: tc.closeErr}
			deps := Deps{
				Stat: func(path string) (os.FileInfo, error) {
					if path == port {
						return newFakeStat("port", true), nil
					}
					if filepath.Base(path) == "portfile.cmake" {
						return newFakeStat("portfile.cmake", false), nil
					}
					return nil, os.ErrNotExist
				},
				Open: func(string) (io.ReadCloser, error) { return file, nil },
			}
			if outcome := inspectPortCandidate(context.Background(), deps, port); outcome.kind != tc.want {
				t.Fatalf("outcome=%+v, want %v", outcome, tc.want)
			}
			for _, request := range file.requests {
				if request > 1 {
					t.Fatalf("request=%d, want at most 1", request)
				}
			}
			if file.closeCount != 1 {
				t.Fatalf("close count=%d, want 1", file.closeCount)
			}
		})
	}
}

type pr591CancelFile struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (f *pr591CancelFile) Read(p []byte) (int, error) {
	n, err := f.ReadCloser.Read(p)
	f.cancel()
	return n, err
}

type pr591CancelCloseFile struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (f *pr591CancelCloseFile) Close() error { err := f.ReadCloser.Close(); f.cancel(); return err }

type pr591CancelDir struct {
	boundedio.DirReader
	cancel context.CancelFunc
}

func (d *pr591CancelDir) Close() error { err := d.DirReader.Close(); d.cancel(); return err }

func TestResolvePortContextCancellationNeverStartsNextProbe(t *testing.T) {
	for _, tc := range []struct {
		name, boundary string
		overlay        bool
	}{
		{"post overlay root", "overlay-root", true},
		{"post overlay manifest", "overlay-manifest", true},
		{"post builtin root without overlay", "builtin-root", false},
		{"post builtin root with overlay", "builtin-root", true},
		{"during builtin manifest without overlay", "builtin-manifest-read", false},
		{"during builtin manifest with overlay", "builtin-manifest-read", true},
		{"post builtin manifest without overlay", "builtin-manifest-close", false},
		{"post builtin manifest with overlay", "builtin-manifest-close", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			overlay, builtin := filepath.Join(base, "overlay"), filepath.Join(base, "vcpkg")
			for _, root := range []string{overlay, builtin} {
				if err := os.MkdirAll(filepath.Join(root, "ports", "zlib"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			overlayPort := filepath.Join(overlay, "zlib")
			if err := os.MkdirAll(overlayPort, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, manifest := range []string{filepath.Join(overlayPort, "portfile.cmake"), filepath.Join(builtin, "ports", "zlib", "portfile.cmake")} {
				if err := os.WriteFile(manifest, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var trace []string
			deps := DefaultDeps()
			realStat, realOpen, realOpenDir := deps.Stat, deps.Open, deps.OpenDir
			deps.Stat = func(path string) (os.FileInfo, error) { trace = append(trace, "stat "+path); return realStat(path) }
			deps.Open = func(path string) (io.ReadCloser, error) {
				trace = append(trace, "open "+path)
				reader, err := realOpen(path)
				if err != nil {
					return nil, err
				}
				if tc.boundary == "builtin-manifest-read" && path == filepath.Join(builtin, "ports", "zlib", "portfile.cmake") {
					return &pr591CancelFile{ReadCloser: reader, cancel: cancel}, nil
				}
				if (tc.boundary == "overlay-manifest" && path == filepath.Join(overlayPort, "portfile.cmake")) || (tc.boundary == "builtin-manifest-close" && path == filepath.Join(builtin, "ports", "zlib", "portfile.cmake")) {
					return &pr591CancelCloseFile{ReadCloser: reader, cancel: cancel}, nil
				}
				return reader, nil
			}
			deps.OpenDir = func(path string) (boundedio.DirReader, error) {
				trace = append(trace, "opendir "+path)
				reader, err := realOpenDir(path)
				if err != nil {
					return nil, err
				}
				if (tc.boundary == "overlay-root" && path == overlay) || (tc.boundary == "builtin-root" && path == builtin) {
					return &pr591CancelDir{DirReader: reader, cancel: cancel}, nil
				}
				return reader, nil
			}
			args := Args{Port: "zlib", VcpkgRoot: builtin}
			if tc.overlay {
				args.OverlayPorts = []string{overlay}
			}
			res := ResolvePortContext(ctx, args, deps)
			if res.Status != evidence.StatusUnknown || res.Reason != ReasonRootUnreadable || res.Winner != nil {
				t.Fatalf("result=%+v trace=%v, want unknown/root_unreadable/nil winner", res, trace)
			}
			if len(res.AllCandidates) == 0 || res.AllCandidates[len(res.AllCandidates)-1].State != CandidateStateUnreadable {
				t.Fatalf("active canceled candidate was not retained as unreadable: %+v", res.AllCandidates)
			}
			if len(trace) == 0 {
				t.Fatal("empty trace")
			}
			last := trace[len(trace)-1]
			wantLast := map[string]string{
				"overlay-root":           "opendir " + overlay,
				"overlay-manifest":       "open " + filepath.Join(overlayPort, "portfile.cmake"),
				"builtin-root":           "opendir " + builtin,
				"builtin-manifest-read":  "open " + filepath.Join(builtin, "ports", "zlib", "portfile.cmake"),
				"builtin-manifest-close": "open " + filepath.Join(builtin, "ports", "zlib", "portfile.cmake"),
			}[tc.boundary]
			if last != wantLast {
				t.Fatalf("trace ended at %q, want %q; full=%v", last, wantLast, trace)
			}
			if strings.HasPrefix(tc.boundary, "builtin-manifest") {
				for _, op := range trace {
					if strings.Contains(op, "vcpkg.json") {
						t.Fatalf("sibling manifest probed after cancellation: %v", trace)
					}
				}
			}
		})
	}
}

func TestBuiltinUnreadabilityRespectsWinnerPrecedence(t *testing.T) {
	unreadable := errors.New("ordinary unreadable")
	for _, tc := range []struct {
		name, failure string
		overlay       bool
		wantStatus    evidence.Status
		wantReason    Reason
	}{
		{"root no winner", "root", false, evidence.StatusUnknown, ReasonRootUnreadable},
		{"root after overlay", "root", true, evidence.StatusOK, ""},
		{"manifest no winner", "manifest", false, evidence.StatusUnknown, ReasonRootUnreadable},
		{"manifest after overlay", "manifest", true, evidence.StatusOK, ""},
		{"absent no winner", "absent", false, evidence.StatusUnknown, ReasonPortNotFound},
		{"absent after overlay", "absent", true, evidence.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			overlay, builtin := filepath.Join(base, "overlay"), filepath.Join(base, "vcpkg")
			overlayPort, builtinPort := filepath.Join(overlay, "zlib"), filepath.Join(builtin, "ports", "zlib")
			if err := os.MkdirAll(overlayPort, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(overlayPort, "portfile.cmake"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(builtin, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.failure == "manifest" {
				if err := os.MkdirAll(builtinPort, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(builtinPort, "portfile.cmake"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			deps := DefaultDeps()
			realStat, realOpen := deps.Stat, deps.Open
			deps.Stat = func(path string) (os.FileInfo, error) {
				if tc.failure == "root" && path == builtin {
					return nil, unreadable
				}
				return realStat(path)
			}
			deps.Open = func(path string) (io.ReadCloser, error) {
				if tc.failure == "manifest" && path == filepath.Join(builtinPort, "portfile.cmake") {
					return nil, unreadable
				}
				return realOpen(path)
			}
			args := Args{Port: "zlib", VcpkgRoot: builtin}
			if tc.overlay {
				args.OverlayPorts = []string{overlay}
			}
			res := ResolvePort(args, deps)
			if res.Status != tc.wantStatus || res.Reason != tc.wantReason {
				t.Fatalf("status/reason=%v/%v want %v/%v; result=%+v", res.Status, res.Reason, tc.wantStatus, tc.wantReason, res)
			}
			if tc.overlay && res.Status == evidence.StatusOK && (res.Winner == nil || res.Winner.Directory != overlayPort) {
				t.Fatalf("overlay winner not preserved: %+v", res)
			}
			if (tc.failure == "root" || tc.failure == "manifest") && (len(res.AllCandidates) == 0 || res.AllCandidates[len(res.AllCandidates)-1].State != CandidateStateUnreadable) {
				t.Fatalf("builtin unreadability not retained: %+v", res.AllCandidates)
			}
		})
	}
}

// The equal-and-opposite guard: every LEGAL vcpkg port name must still
// resolve. A gate that rejects real port names would be worse than the hole.
func TestLegalPortNamesAreStillAccepted(t *testing.T) {
	for _, good := range []string{"zlib", "mcp-language-server", "abseil", "libpng16", "a-b-c", "x264"} {
		t.Run(good, func(t *testing.T) {
			res := ResolvePort(Args{Port: good, OverlayPorts: []string{t.TempDir()}}, DefaultDeps())
			if res.Reason == ReasonInvalidPortName {
				t.Fatalf("port %q was rejected as an invalid name, but it is a legal vcpkg port name", good)
			}
		})
	}
}

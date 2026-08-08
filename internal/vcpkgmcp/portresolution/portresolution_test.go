package portresolution

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/vcpkgmcp/boundedio"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// fakeDeps creates a testable Deps with in-memory filesystem simulation.
type fakeDeps struct {
	// files maps absolute paths to their stat info. Key is the full path
	// to either a directory or file.
	files map[string]os.FileInfo
	// portManifests maps absolute port directory paths to whether they
	// contain a manifest. This simplifies testing.
	portManifests map[string]bool
	// readDirErrors injects unreadable-directory failures by absolute path.
	readDirErrors map[string]error
}

func (fd *fakeDeps) Stat(path string) (os.FileInfo, error) {
	if fi, ok := fd.files[path]; ok {
		return fi, nil
	}
	if fd.portManifests[filepath.Dir(path)] && (filepath.Base(path) == "portfile.cmake" || filepath.Base(path) == "vcpkg.json") {
		return newFakeStat(filepath.Base(path), false), nil
	}
	return nil, os.ErrNotExist
}

func (fd *fakeDeps) Open(path string) (io.ReadCloser, error) {
	if err := fd.readDirErrors[path]; err != nil {
		return nil, err
	}
	if err := fd.readDirErrors[filepath.Dir(path)]; err != nil {
		return nil, err
	}
	if _, err := fd.Stat(path); err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (fd *fakeDeps) OpenDir(path string) (boundedio.DirReader, error) {
	if err := fd.readDirErrors[path]; err != nil {
		return nil, err
	}
	if _, ok := fd.files[path]; !ok {
		return nil, os.ErrNotExist
	}
	return &fakePagedDir{}, nil
}

type fakePagedDir struct{}

func (*fakePagedDir) ReadDir(int) ([]os.DirEntry, error) { return nil, io.EOF }
func (*fakePagedDir) Close() error                       { return nil }

func (fd *fakeDeps) Abs(path string) (string, error) {
	// For testing, assume paths are already absolute (starting with / or C:\).
	// If a path is relative, prefix it with a fake root.
	if filepath.IsAbs(path) {
		return path, nil
	}
	return filepath.Join("C:\\fake", path), nil
}

// toDeps converts a fakeDeps to a Deps value by wrapping methods in closures.
func (fd *fakeDeps) toDeps() Deps {
	return Deps{
		Stat:    fd.Stat,
		Open:    fd.Open,
		OpenDir: fd.OpenDir,
		Abs:     fd.Abs,
	}
}

type fakeDir struct {
	name  string
	isDir bool
}

func (fd *fakeDir) Name() string {
	return fd.name
}

func (fd *fakeDir) IsDir() bool {
	return fd.isDir
}

func (fd *fakeDir) Type() os.FileMode {
	if fd.isDir {
		return os.ModeDir
	}
	return 0
}

func (fd *fakeDir) Info() (os.FileInfo, error) {
	return nil, nil
}

// newFakeStat returns a mock os.FileInfo.
func newFakeStat(name string, isDir bool) os.FileInfo {
	return &fakeStat{name: name, isDir: isDir}
}

type fakeStat struct {
	name  string
	isDir bool
}

func (fs *fakeStat) Name() string {
	return fs.name
}

func (fs *fakeStat) Size() int64 {
	return 0
}

func (fs *fakeStat) Mode() os.FileMode {
	if fs.isDir {
		return os.ModeDir | 0755
	}
	return 0644
}

func (fs *fakeStat) ModTime() time.Time {
	return time.Time{}
}

func (fs *fakeStat) IsDir() bool {
	return fs.isDir
}

func (fs *fakeStat) Sys() interface{} {
	return nil
}

// Test: builtin-only — port in builtin, no overlays.
func TestBuiltinOnly(t *testing.T) {
	tmpDir := t.TempDir()
	builtinPortDir := filepath.Join(tmpDir, "ports", "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:                         newFakeStat("tmpdir", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:                 newFakeStat("myport", true),
		},
		portManifests: map[string]bool{
			builtinPortDir: true,
		},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		VcpkgRoot:    tmpDir,
		OverlayPorts: []string{},
	}, deps.toDeps())

	if res.Status != evidence.StatusOK {
		t.Errorf("expected StatusOK, got %v", res.Status)
	}
	if res.Winner == nil {
		t.Errorf("expected a winner, got nil")
	}
	if res.Winner.Directory != builtinPortDir {
		t.Errorf("expected winner directory %q, got %q", builtinPortDir, res.Winner.Directory)
	}
	if len(res.AllCandidates) != 1 || res.AllCandidates[0].State != CandidateStateFound {
		t.Errorf("expected one found builtin candidate, got %+v", res.AllCandidates)
	}
	if len(res.Shadows) != 0 {
		t.Errorf("expected no shadows, got %d", len(res.Shadows))
	}
}

// Test: single overlay wins over builtin.
func TestOverlayWinsOverBuiltin(t *testing.T) {
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "overlay1")
	overlayPortDir := filepath.Join(overlayDir, "myport")
	builtinPortDir := filepath.Join(tmpDir, "ports", "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:                         newFakeStat("tmpdir", true),
			overlayDir:                     newFakeStat("overlay1", true),
			overlayPortDir:                 newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:                 newFakeStat("myport", true),
		},
		portManifests: map[string]bool{
			overlayPortDir: true,
			builtinPortDir: true,
		},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		VcpkgRoot:    tmpDir,
		OverlayPorts: []string{overlayDir},
	}, deps.toDeps())

	if res.Status != evidence.StatusOK {
		t.Errorf("expected StatusOK, got %v", res.Status)
	}
	if res.Winner == nil {
		t.Errorf("expected a winner, got nil")
	}
	if res.Winner.Directory != overlayPortDir {
		t.Errorf("expected winner directory %q, got %q", overlayPortDir, res.Winner.Directory)
	}
	if len(res.Shadows) != 1 {
		t.Errorf("expected 1 shadow (builtin), got %d", len(res.Shadows))
	}
	if res.Shadows[0].Directory != builtinPortDir {
		t.Errorf("expected shadowed directory %q, got %q", builtinPortDir, res.Shadows[0].Directory)
	}
}

// Test: FIRST overlay wins when two overlays both define the port.
func TestFirstOverlayWinsOverSecond(t *testing.T) {
	tmpDir := t.TempDir()
	overlay1Dir := filepath.Join(tmpDir, "overlay1")
	overlay1PortDir := filepath.Join(overlay1Dir, "myport")
	overlay2Dir := filepath.Join(tmpDir, "overlay2")
	overlay2PortDir := filepath.Join(overlay2Dir, "myport")
	builtinPortDir := filepath.Join(tmpDir, "ports", "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:                         newFakeStat("tmpdir", true),
			overlay1Dir:                    newFakeStat("overlay1", true),
			overlay1PortDir:                newFakeStat("myport", true),
			overlay2Dir:                    newFakeStat("overlay2", true),
			overlay2PortDir:                newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:                 newFakeStat("myport", true),
		},
		portManifests: map[string]bool{
			overlay1PortDir: true,
			overlay2PortDir: true,
			builtinPortDir:  true,
		},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		VcpkgRoot:    tmpDir,
		OverlayPorts: []string{overlay1Dir, overlay2Dir},
	}, deps.toDeps())

	if res.Status != evidence.StatusOK {
		t.Errorf("expected StatusOK, got %v", res.Status)
	}
	if res.Winner == nil {
		t.Errorf("expected a winner, got nil")
	}
	if res.Winner.Directory != overlay1PortDir {
		t.Errorf("expected winner directory %q (overlay1), got %q", overlay1PortDir, res.Winner.Directory)
	}
	if len(res.Shadows) != 2 {
		t.Errorf("expected 2 shadows (overlay2 + builtin), got %d", len(res.Shadows))
	}
	if res.OverlayToOverlayShadowingOccurred != true {
		t.Errorf("expected OverlayToOverlayShadowingOccurred=true")
	}
}

// Test: port absent everywhere → unknown(port_not_found).
func TestPortNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "overlay1")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:                         newFakeStat("tmpdir", true),
			overlayDir:                     newFakeStat("overlay1", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
		},
		portManifests: map[string]bool{},
	}

	res := ResolvePort(Args{
		Port:         "nonexistent",
		VcpkgRoot:    tmpDir,
		OverlayPorts: []string{overlayDir},
	}, deps.toDeps())

	if res.Status != evidence.StatusUnknown {
		t.Errorf("expected StatusUnknown, got %v", res.Status)
	}
	if res.Reason != ReasonPortNotFound {
		t.Errorf("expected ReasonPortNotFound, got %q", res.Reason)
	}
	if res.Winner != nil {
		t.Errorf("expected no winner, got %v", res.Winner)
	}
}

// Test: no roots supplied → unknown(no_roots_supplied).
func TestNoRootsSupplied(t *testing.T) {
	res := ResolvePort(Args{
		Port:         "myport",
		VcpkgRoot:    "",
		OverlayPorts: []string{},
	}, DefaultDeps())

	if res.Status != evidence.StatusUnknown {
		t.Errorf("expected StatusUnknown, got %v", res.Status)
	}
	if res.Reason != ReasonNoRootsSupplied {
		t.Errorf("expected ReasonNoRootsSupplied, got %q", res.Reason)
	}
}

func TestPR591_BlankOnlyOverlaysAreNoRootsWithZeroIO(t *testing.T) {
	dependencyCalls := 0
	deps := Deps{
		Stat: func(string) (os.FileInfo, error) {
			dependencyCalls++
			return nil, errors.New("unexpected Stat call")
		},
		Open: func(string) (io.ReadCloser, error) {
			dependencyCalls++
			return nil, errors.New("unexpected Open call")
		},
		OpenDir: func(string) (boundedio.DirReader, error) {
			dependencyCalls++
			return nil, errors.New("unexpected OpenDir call")
		},
		Abs: func(string) (string, error) {
			dependencyCalls++
			return "", errors.New("unexpected Abs call")
		},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		VcpkgRoot:    " \t ",
		OverlayPorts: []string{"", " \n\t "},
	}, deps)

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonNoRootsSupplied {
		t.Fatalf("expected unknown no_roots_supplied, got status=%v reason=%q", res.Status, res.Reason)
	}
	if dependencyCalls != 0 {
		t.Fatalf("expected no dependency calls, got %d", dependencyCalls)
	}
}

func TestPR591_MixedBlankOverlaysPreserveSourceIndices(t *testing.T) {
	tmpDir := t.TempDir()
	overlay := filepath.Join(tmpDir, "overlay")
	portDir := filepath.Join(overlay, "myport")
	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:  newFakeStat("tmpdir", true),
			overlay: newFakeStat("overlay", true),
			portDir: newFakeStat("myport", true),
		},
		portManifests: map[string]bool{
			portDir: true,
		},
	}

	roots := normalizedOverlayRoots([]string{"", overlay, " \t ", overlay})
	if len(roots) != 2 || roots[0].sourceIndex != 1 || roots[1].sourceIndex != 3 {
		t.Fatalf("expected retained source indices [1 3], got %+v", roots)
	}

	res := ResolvePort(Args{
		Port:         "myport",
		OverlayPorts: []string{"", overlay, " \t ", overlay},
	}, deps.toDeps())

	if res.Status != evidence.StatusOK || res.Winner == nil || res.Winner.Directory != portDir {
		t.Fatalf("expected first non-blank overlay to win, got status=%v winner=%+v", res.Status, res.Winner)
	}
	if res.Winner.Source != "overlay-01: "+overlay {
		t.Fatalf("expected literal winner source from supplied index 1, got %q", res.Winner.Source)
	}
	if len(res.Shadows) != 1 || res.Shadows[0].Directory != portDir || res.Shadows[0].Source != "overlay-03: "+overlay {
		t.Fatalf("expected same supplied overlay at index 3 as the only shadow, got %+v", res.Shadows)
	}
}

func TestPR591_WinnerSourceDocsMatchFormatter(t *testing.T) {
	tmpDir := t.TempDir()
	overlay := filepath.Join(tmpDir, "overlay")
	portDir := filepath.Join(overlay, "myport")
	deps := &fakeDeps{
		files:         map[string]os.FileInfo{tmpDir: newFakeStat("tmpdir", true), overlay: newFakeStat("overlay", true), portDir: newFakeStat("myport", true)},
		portManifests: map[string]bool{portDir: true},
	}
	res := ResolvePort(Args{Port: "myport", OverlayPorts: []string{"", overlay, "", overlay}}, deps.toDeps())
	if res.Winner == nil || res.Winner.Source != "overlay-01: "+overlay || len(res.Shadows) != 1 || res.Shadows[0].Source != "overlay-03: "+overlay {
		t.Fatalf("live overlay source formatting = winner:%+v shadows:%+v, want original indices 01 and 03", res.Winner, res.Shadows)
	}
	doc := winnerSourceDoc(t)
	if !strings.Contains(doc, "overlay-%02d: <path>") || !strings.Contains(doc, "original") || !strings.Contains(doc, "index") {
		t.Fatalf("Winner.Source doc = %q, want exact formatter form and original-index meaning", doc)
	}
}

func winnerSourceDoc(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), thisFile[:len(thisFile)-len("_test.go")]+".go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse portresolution.go: %v", err)
	}
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "Winner" {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				t.Fatal("Winner is not a struct")
			}
			for _, field := range structType.Fields.List {
				if len(field.Names) == 1 && field.Names[0].Name == "Source" && field.Doc != nil {
					return field.Doc.Text()
				}
			}
		}
	}
	t.Fatal("Winner.Source doc not found")
	return ""
}

// Test: candidate dir exists but holds neither manifest file.
func TestCandidateDirWithoutManifest(t *testing.T) {
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "overlay1")
	overlayPortDir := filepath.Join(overlayDir, "myport")
	builtinPortDir := filepath.Join(tmpDir, "ports", "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:                         newFakeStat("tmpdir", true),
			overlayDir:                     newFakeStat("overlay1", true),
			overlayPortDir:                 newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:                 newFakeStat("myport", true),
		},
		portManifests: map[string]bool{
			overlayPortDir: false, // exists but no manifest
			builtinPortDir: true,  // has manifest
		},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		VcpkgRoot:    tmpDir,
		OverlayPorts: []string{overlayDir},
	}, deps.toDeps())

	if res.Status != evidence.StatusOK {
		t.Errorf("expected StatusOK, got %v", res.Status)
	}
	if res.Winner == nil {
		t.Errorf("expected a winner, got nil")
	}
	if res.Winner.Directory != builtinPortDir {
		t.Errorf("expected builtin to win since overlay has no manifest, got %q", res.Winner.Directory)
	}

	// Verify that the overlay candidate is recorded as checked but not found.
	found := false
	for _, cand := range res.AllCandidates {
		if cand.Directory == overlayPortDir && !cand.PortDirFound {
			found = true
			if cand.State != CandidateStateAbsent {
				t.Errorf("expected absent candidate state, got %q", cand.State)
			}
			if cand.Reason == "" {
				t.Errorf("expected non-empty reason for rejected overlay candidate")
			}
		}
	}
	if !found {
		t.Errorf("expected overlay candidate in AllCandidates with PortDirFound=false")
	}
}

// Test: vcpkg_root absent so builtin was never checked.
func TestBuiltinNotCheckedWhenVcpkgRootAbsent(t *testing.T) {
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "overlay1")
	overlayPortDir := filepath.Join(overlayDir, "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:         newFakeStat("tmpdir", true),
			overlayDir:     newFakeStat("overlay1", true),
			overlayPortDir: newFakeStat("myport", true),
		},
		portManifests: map[string]bool{
			overlayPortDir: true,
		},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		VcpkgRoot:    "", // No vcpkg_root
		OverlayPorts: []string{overlayDir},
	}, deps.toDeps())

	if res.Status != evidence.StatusOK {
		t.Errorf("expected StatusOK, got %v", res.Status)
	}
	if res.Winner == nil {
		t.Errorf("expected a winner, got nil")
	}
	if res.Winner.Directory != overlayPortDir {
		t.Errorf("expected overlay to win, got %q", res.Winner.Directory)
	}

	// Verify that builtin was NOT checked, but that fact is structured rather
	// than omitted from the serialized result.
	if len(res.AllCandidates) != 2 {
		t.Fatalf("expected overlay and builtin-not-checked candidates, got %d", len(res.AllCandidates))
	}
	builtin := res.AllCandidates[1]
	if builtin.Source != "builtin" || builtin.State != CandidateStateNotChecked {
		t.Errorf("expected builtin not_checked metadata, got %+v", builtin)
	}
}

// Test: empty port name is a failed input error.
func TestEmptyPort(t *testing.T) {
	tmpDir := t.TempDir()

	res := ResolvePort(Args{
		Port:      "",
		VcpkgRoot: tmpDir,
	}, DefaultDeps())

	if res.Status != evidence.StatusFailed {
		t.Errorf("expected StatusFailed, got %v", res.Status)
	}
	if res.Reason != ReasonEmptyPort {
		t.Errorf("expected ReasonEmptyPort, got %q", res.Reason)
	}
}

// Test: port with whitespace is trimmed.
func TestPortWithWhitespace(t *testing.T) {
	tmpDir := t.TempDir()
	builtinPortDir := filepath.Join(tmpDir, "ports", "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:                         newFakeStat("tmpdir", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:                 newFakeStat("myport", true),
		},
		portManifests: map[string]bool{
			builtinPortDir: true,
		},
	}

	res := ResolvePort(Args{
		Port:      "  myport  ",
		VcpkgRoot: tmpDir,
	}, deps.toDeps())

	if res.Status != evidence.StatusOK {
		t.Errorf("expected StatusOK, got %v", res.Status)
	}
	if res.Winner == nil {
		t.Errorf("expected a winner, got nil")
	}
}

// Test: shadow list is correct and ordered.
func TestShadowListOrder(t *testing.T) {
	tmpDir := t.TempDir()
	overlay1Dir := filepath.Join(tmpDir, "overlay1")
	overlay1PortDir := filepath.Join(overlay1Dir, "myport")
	overlay2Dir := filepath.Join(tmpDir, "overlay2")
	overlay2PortDir := filepath.Join(overlay2Dir, "myport")
	overlay3Dir := filepath.Join(tmpDir, "overlay3")
	overlay3PortDir := filepath.Join(overlay3Dir, "myport")
	builtinPortDir := filepath.Join(tmpDir, "ports", "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:                         newFakeStat("tmpdir", true),
			overlay1Dir:                    newFakeStat("overlay1", true),
			overlay1PortDir:                newFakeStat("myport", true),
			overlay2Dir:                    newFakeStat("overlay2", true),
			overlay2PortDir:                newFakeStat("myport", true),
			overlay3Dir:                    newFakeStat("overlay3", true),
			overlay3PortDir:                newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:                 newFakeStat("myport", true),
		},
		portManifests: map[string]bool{
			overlay1PortDir: true,
			overlay2PortDir: true,
			overlay3PortDir: true,
			builtinPortDir:  true,
		},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		VcpkgRoot:    tmpDir,
		OverlayPorts: []string{overlay1Dir, overlay2Dir, overlay3Dir},
	}, deps.toDeps())

	if res.Status != evidence.StatusOK {
		t.Errorf("expected StatusOK, got %v", res.Status)
	}

	// Should have 3 shadows (overlay2, overlay3, builtin), in that order.
	if len(res.Shadows) != 3 {
		t.Errorf("expected 3 shadows, got %d", len(res.Shadows))
	}

	expectedOrder := []string{overlay2PortDir, overlay3PortDir, builtinPortDir}
	for i, expected := range expectedOrder {
		if i < len(res.Shadows) && res.Shadows[i].Directory != expected {
			t.Errorf("shadow %d: expected %q, got %q", i, expected, res.Shadows[i].Directory)
		}
	}
}

// Test: all candidates are recorded.
func TestAllCandidatesRecorded(t *testing.T) {
	tmpDir := t.TempDir()
	overlay1Dir := filepath.Join(tmpDir, "overlay1")
	overlay1PortDir := filepath.Join(overlay1Dir, "myport")
	overlay2Dir := filepath.Join(tmpDir, "overlay2")
	overlay2PortDir := filepath.Join(overlay2Dir, "myport")
	builtinPortDir := filepath.Join(tmpDir, "ports", "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:                         newFakeStat("tmpdir", true),
			overlay1Dir:                    newFakeStat("overlay1", true),
			overlay1PortDir:                newFakeStat("myport", true),
			overlay2Dir:                    newFakeStat("overlay2", true),
			overlay2PortDir:                newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:                 newFakeStat("myport", true),
		},
		portManifests: map[string]bool{
			overlay1PortDir: true,
			overlay2PortDir: true,
			builtinPortDir:  true,
		},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		VcpkgRoot:    tmpDir,
		OverlayPorts: []string{overlay1Dir, overlay2Dir},
	}, deps.toDeps())

	// AllCandidates should have 3 entries: overlay1, overlay2, builtin.
	if len(res.AllCandidates) != 3 {
		t.Errorf("expected 3 candidates, got %d", len(res.AllCandidates))
	}

	// Verify order: overlay1, overlay2, builtin.
	expectedDirs := []string{overlay1PortDir, overlay2PortDir, builtinPortDir}
	for i, expected := range expectedDirs {
		if i < len(res.AllCandidates) && res.AllCandidates[i].Directory != expected {
			t.Errorf("candidate %d: expected %q, got %q", i, expected, res.AllCandidates[i].Directory)
		}
	}
}

// Test: evidence includes all paths probed.
func TestEvidenceIncludesPaths(t *testing.T) {
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "overlay1")
	overlayPortDir := filepath.Join(overlayDir, "myport")
	builtinPortDir := filepath.Join(tmpDir, "ports", "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:                         newFakeStat("tmpdir", true),
			overlayDir:                     newFakeStat("overlay1", true),
			overlayPortDir:                 newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:                 newFakeStat("myport", true),
		},
		portManifests: map[string]bool{
			overlayPortDir: true,
			builtinPortDir: true,
		},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		VcpkgRoot:    tmpDir,
		OverlayPorts: []string{overlayDir},
	}, deps.toDeps())

	// Evidence should include both overlay and builtin paths.
	pathFound := false
	for _, p := range res.Evidence.Paths {
		if p == overlayPortDir || p == builtinPortDir {
			pathFound = true
			break
		}
	}
	if !pathFound {
		t.Errorf("expected evidence paths, got %v", res.Evidence.Paths)
	}
}

// Test: overlay-to-overlay shadowing flag is set correctly.
func TestOverlayToOverlayShadowingFlag(t *testing.T) {
	tmpDir := t.TempDir()
	overlay1Dir := filepath.Join(tmpDir, "overlay1")
	overlay1PortDir := filepath.Join(overlay1Dir, "myport")
	overlay2Dir := filepath.Join(tmpDir, "overlay2")
	overlay2PortDir := filepath.Join(overlay2Dir, "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:          newFakeStat("tmpdir", true),
			overlay1Dir:     newFakeStat("overlay1", true),
			overlay1PortDir: newFakeStat("myport", true),
			overlay2Dir:     newFakeStat("overlay2", true),
			overlay2PortDir: newFakeStat("myport", true),
		},
		portManifests: map[string]bool{
			overlay1PortDir: true,
			overlay2PortDir: true,
		},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		VcpkgRoot:    "", // no builtin
		OverlayPorts: []string{overlay1Dir, overlay2Dir},
	}, deps.toDeps())

	if !res.OverlayToOverlayShadowingOccurred {
		t.Errorf("expected OverlayToOverlayShadowingOccurred=true when multiple overlays have port")
	}
}

// Test: overlay-to-overlay shadowing flag is false when no overlap.
func TestNoOverlayToOverlayShadowingWhenUnique(t *testing.T) {
	tmpDir := t.TempDir()
	overlay1Dir := filepath.Join(tmpDir, "overlay1")
	overlay1PortDir := filepath.Join(overlay1Dir, "port1")
	overlay2Dir := filepath.Join(tmpDir, "overlay2")
	overlay2PortDir := filepath.Join(overlay2Dir, "port2")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:          newFakeStat("tmpdir", true),
			overlay1Dir:     newFakeStat("overlay1", true),
			overlay1PortDir: newFakeStat("port1", true),
			overlay2Dir:     newFakeStat("overlay2", true),
			overlay2PortDir: newFakeStat("port2", true),
		},
		portManifests: map[string]bool{
			overlay1PortDir: true,
		},
	}

	res := ResolvePort(Args{
		Port:         "port1", // only in overlay1
		VcpkgRoot:    "",
		OverlayPorts: []string{overlay1Dir, overlay2Dir},
	}, deps.toDeps())

	if res.OverlayToOverlayShadowingOccurred {
		t.Errorf("expected OverlayToOverlayShadowingOccurred=false when port appears in only one overlay")
	}
}

// Test: an unreadable higher-precedence overlay means a later match cannot be
// reported as the definitive winner.
func TestHigherPrecedenceUnreadableOverlayFailsClosed(t *testing.T) {
	tmpDir := t.TempDir()
	missingOverlay := filepath.Join(tmpDir, "missing-overlay")
	lowerOverlay := filepath.Join(tmpDir, "lower-overlay")
	lowerPortDir := filepath.Join(lowerOverlay, "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:       newFakeStat("tmpdir", true),
			lowerOverlay: newFakeStat("lower-overlay", true),
			lowerPortDir: newFakeStat("myport", true),
		},
		portManifests: map[string]bool{lowerPortDir: true},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		OverlayPorts: []string{missingOverlay, lowerOverlay},
	}, deps.toDeps())

	if res.Status != evidence.StatusUnknown {
		t.Fatalf("expected unknown because higher-precedence overlay %q was unreadable, got %v", missingOverlay, res.Status)
	}
	if res.Reason != ReasonHigherPrecedenceOverlayUnreadable {
		t.Fatalf("expected higher_precedence_overlay_unreadable, got %q", res.Reason)
	}
	if res.BlockingCandidate == nil || res.BlockingCandidate.Source != "overlay-00: "+missingOverlay {
		t.Fatalf("expected blocking candidate for %q, got %+v", missingOverlay, res.BlockingCandidate)
	}
	if res.BlockingCandidate.State != CandidateStateUnreadable {
		t.Fatalf("expected unreadable blocking state, got %q", res.BlockingCandidate.State)
	}
	if res.Winner != nil {
		t.Fatalf("expected no definitive winner, got %+v", res.Winner)
	}
}

// Test: a permission failure while inspecting a higher-precedence port
// candidate is unreadable, not an absence that a lower overlay can bypass.
func TestHigherPrecedencePortCandidateReadErrorFailsClosed(t *testing.T) {
	tmpDir := t.TempDir()
	higherOverlay := filepath.Join(tmpDir, "higher-overlay")
	higherPortDir := filepath.Join(higherOverlay, "myport")
	lowerOverlay := filepath.Join(tmpDir, "lower-overlay")
	lowerPortDir := filepath.Join(lowerOverlay, "myport")
	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:        newFakeStat("tmpdir", true),
			higherOverlay: newFakeStat("higher-overlay", true),
			higherPortDir: newFakeStat("myport", true),
			lowerOverlay:  newFakeStat("lower-overlay", true),
			lowerPortDir:  newFakeStat("myport", true),
		},
		portManifests: map[string]bool{higherPortDir: true, lowerPortDir: true},
		readDirErrors: map[string]error{higherPortDir: errors.New("permission denied")},
	}

	res := ResolvePort(Args{Port: "myport", OverlayPorts: []string{higherOverlay, lowerOverlay}}, deps.toDeps())
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonHigherPrecedenceOverlayUnreadable {
		t.Fatalf("expected unknown higher-precedence unreadable result, got %v (%q)", res.Status, res.Reason)
	}
	if res.BlockingCandidate == nil || res.BlockingCandidate.State != CandidateStateUnreadable {
		t.Fatalf("expected unreadable blocking candidate, got %+v", res.BlockingCandidate)
	}
}

// Test: omitting vcpkg_root is disclosed in serialized candidate metadata.
func TestBuiltinNotCheckedIsStructuredCandidateMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "overlay")
	overlayPortDir := filepath.Join(overlayDir, "myport")
	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:         newFakeStat("tmpdir", true),
			overlayDir:     newFakeStat("overlay", true),
			overlayPortDir: newFakeStat("myport", true),
		},
		portManifests: map[string]bool{overlayPortDir: true},
	}

	res := ResolvePort(Args{Port: "myport", OverlayPorts: []string{overlayDir}}, deps.toDeps())
	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"source":"builtin"`) || !strings.Contains(string(wire), `"state":"not_checked"`) {
		t.Fatalf("expected a structured builtin not_checked disclosure, got %s", wire)
	}
}

// Test: roots are contractually absolute; relative inputs must not be made
// dependent on the process working directory.
func TestRelativeRootsAreRejected(t *testing.T) {
	for _, tc := range []struct {
		args        Args
		invalidRoot string
	}{
		{args: Args{Port: "myport", OverlayPorts: []string{"relative-overlay"}}, invalidRoot: "relative-overlay"},
		{args: Args{Port: "myport", VcpkgRoot: "relative-vcpkg"}, invalidRoot: "relative-vcpkg"},
	} {
		t.Run("relative root", func(t *testing.T) {
			res := ResolvePort(tc.args, DefaultDeps())
			if res.Status != evidence.StatusFailed {
				t.Fatalf("expected failed for relative root, got %v (%q)", res.Status, res.Reason)
			}
			if res.Reason != ReasonRelativeRoot || res.InvalidRoot != tc.invalidRoot {
				t.Fatalf("expected named relative-root error for %q, got reason=%q root=%q", tc.invalidRoot, res.Reason, res.InvalidRoot)
			}
		})
	}
}

// Test: validation trims overlay roots, and the inspected path must use that
// same normalized value rather than the whitespace-padded input spelling.
func TestWhitespacePaddedAbsoluteOverlayResolves(t *testing.T) {
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "overlay")
	overlayPortDir := filepath.Join(overlayDir, "myport")
	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:         newFakeStat("tmpdir", true),
			overlayDir:     newFakeStat("overlay", true),
			overlayPortDir: newFakeStat("myport", true),
		},
		portManifests: map[string]bool{overlayPortDir: true},
	}

	res := ResolvePort(Args{
		Port:         "myport",
		OverlayPorts: []string{"  " + overlayDir + "  "},
	}, deps.toDeps())
	if res.Status != evidence.StatusOK || res.Winner == nil || res.Winner.Directory != overlayPortDir {
		t.Fatalf("expected padded absolute overlay to resolve %q, got status=%v winner=%+v", overlayPortDir, res.Status, res.Winner)
	}
}

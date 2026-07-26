package portresolution

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/evidence"
)

// fakeDeps creates a testable Deps with in-memory filesystem simulation.
type fakeDeps struct {
	// files maps absolute paths to their stat info. Key is the full path
	// to either a directory or file.
	files map[string]os.FileInfo
	// portManifests maps absolute port directory paths to whether they
	// contain a manifest. This simplifies testing.
	portManifests map[string]bool
}

func (fd *fakeDeps) Stat(path string) (os.FileInfo, error) {
	if fi, ok := fd.files[path]; ok {
		return fi, nil
	}
	return nil, os.ErrNotExist
}

func (fd *fakeDeps) ReadDir(path string) ([]os.DirEntry, error) {
	// For simplicity in testing, we pre-populate portManifests.
	// This avoids complex directory structure simulation.
	if fd.portManifests[path] {
		// Return dummy entries indicating a manifest exists.
		return []os.DirEntry{
			&fakeDir{name: "portfile.cmake", isDir: false},
		}, nil
	}

	// Check if path exists; if not, return not found.
	if _, ok := fd.files[path]; !ok {
		return nil, os.ErrNotExist
	}

	// Path exists but no manifest.
	return []os.DirEntry{}, nil
}

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
		ReadDir: fd.ReadDir,
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
			tmpDir:              newFakeStat("tmpdir", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:      newFakeStat("myport", true),
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
			tmpDir:              newFakeStat("tmpdir", true),
			overlayDir:          newFakeStat("overlay1", true),
			overlayPortDir:      newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:      newFakeStat("myport", true),
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
			tmpDir:              newFakeStat("tmpdir", true),
			overlay1Dir:         newFakeStat("overlay1", true),
			overlay1PortDir:     newFakeStat("myport", true),
			overlay2Dir:         newFakeStat("overlay2", true),
			overlay2PortDir:     newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:      newFakeStat("myport", true),
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
			tmpDir:     newFakeStat("tmpdir", true),
			overlayDir: newFakeStat("overlay1", true),
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

// Test: candidate dir exists but holds neither manifest file.
func TestCandidateDirWithoutManifest(t *testing.T) {
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "overlay1")
	overlayPortDir := filepath.Join(overlayDir, "myport")
	builtinPortDir := filepath.Join(tmpDir, "ports", "myport")

	deps := &fakeDeps{
		files: map[string]os.FileInfo{
			tmpDir:              newFakeStat("tmpdir", true),
			overlayDir:          newFakeStat("overlay1", true),
			overlayPortDir:      newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:      newFakeStat("myport", true),
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

	// Verify that builtin was NOT checked (should have only overlay candidate).
	for _, cand := range res.AllCandidates {
		if cand.Source == "builtin" || cand.Source != overlayDir {
			// If any candidate has "builtin" in source, that's a problem.
			if cand.Source != overlayDir {
				t.Errorf("unexpected candidate source %q, expected only overlay", cand.Source)
			}
		}
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
			tmpDir:              newFakeStat("tmpdir", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:      newFakeStat("myport", true),
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
			tmpDir:              newFakeStat("tmpdir", true),
			overlay1Dir:         newFakeStat("overlay1", true),
			overlay1PortDir:     newFakeStat("myport", true),
			overlay2Dir:         newFakeStat("overlay2", true),
			overlay2PortDir:     newFakeStat("myport", true),
			overlay3Dir:         newFakeStat("overlay3", true),
			overlay3PortDir:     newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:      newFakeStat("myport", true),
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
			tmpDir:              newFakeStat("tmpdir", true),
			overlay1Dir:         newFakeStat("overlay1", true),
			overlay1PortDir:     newFakeStat("myport", true),
			overlay2Dir:         newFakeStat("overlay2", true),
			overlay2PortDir:     newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:      newFakeStat("myport", true),
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
			tmpDir:              newFakeStat("tmpdir", true),
			overlayDir:          newFakeStat("overlay1", true),
			overlayPortDir:      newFakeStat("myport", true),
			filepath.Join(tmpDir, "ports"): newFakeStat("ports", true),
			builtinPortDir:      newFakeStat("myport", true),
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

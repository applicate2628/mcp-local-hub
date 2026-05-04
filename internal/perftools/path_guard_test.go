package perftools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestValidateBinaryInsideRoot_ResolvesRealPath asserts the canonical
// happy path: a real file inside the project_root resolves to its
// own real path (after EvalSymlinks).
func TestValidateBinaryInsideRoot_ResolvesRealPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "build", "app.exe")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target, []byte("fake"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := validateBinaryInsideRoot(root, target, "binary")
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	wantInfo, _ := os.Stat(target)
	gotInfo, _ := os.Stat(got)
	if !os.SameFile(wantInfo, gotInfo) {
		t.Fatalf("resolved %q does not identify same file as %q", got, target)
	}
}

func TestValidateBinaryInsideRoot_RejectsMissingProjectRoot(t *testing.T) {
	_, err := validateBinaryInsideRoot("", "x", "binary")
	if err == nil {
		t.Fatal("expected missing-project_root error")
	}
	if !strings.Contains(err.Error(), "missing required parameter: project_root") {
		t.Fatalf("got: %v", err)
	}
}

func TestValidateBinaryInsideRoot_RejectsMissingTarget(t *testing.T) {
	root := t.TempDir()
	_, err := validateBinaryInsideRoot(root, "", "binary")
	if err == nil {
		t.Fatal("expected missing-target error")
	}
	if !strings.Contains(err.Error(), "missing required parameter: binary") {
		t.Fatalf("got: %v", err)
	}
}

// TestValidateBinaryInsideRoot_RejectsFilesystemRoot guards against
// callers accidentally passing "/" or a Windows drive root: a project
// boundary that wide effectively disables the path guard.
func TestValidateBinaryInsideRoot_RejectsFilesystemRoot(t *testing.T) {
	var root string
	if runtime.GOOS == "windows" {
		// Use the system drive's root — actually a filesystem root.
		root = filepath.VolumeName(os.Getenv("SystemRoot")) + `\`
		if root == `\` || root == "" {
			t.Skip("could not determine system drive root")
		}
	} else {
		root = "/"
	}
	_, err := validateBinaryInsideRoot(root, "etc/passwd", "binary")
	if err == nil {
		t.Fatal("expected filesystem-root rejection")
	}
	if !strings.Contains(err.Error(), "filesystem root") {
		t.Fatalf("expected filesystem-root error, got: %v", err)
	}
}

func TestValidateBinaryInsideRoot_RejectsDirectoryTarget(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := validateBinaryInsideRoot(root, sub, "binary")
	if err == nil {
		t.Fatal("expected directory-target rejection")
	}
	if !strings.Contains(err.Error(), "must be a file") {
		t.Fatalf("expected must-be-a-file error, got: %v", err)
	}
}

// TestValidateBinaryInsideRoot_RejectsParentTraversal stresses the
// inside-root assertion with relative ".." sequences. EvalSymlinks +
// pathInsideRoot must catch these even though they don't involve
// symlinks.
func TestValidateBinaryInsideRoot_RejectsParentTraversal(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	siblingFile := filepath.Join(parent, "sibling.bin")
	if err := os.WriteFile(siblingFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	// "../sibling.bin" relative to root resolves outside root.
	_, err := validateBinaryInsideRoot(root, filepath.Join("..", "sibling.bin"), "binary")
	if err == nil {
		t.Fatal("expected parent-traversal rejection")
	}
	if !strings.Contains(err.Error(), "must be inside project_root") {
		t.Fatalf("expected boundary error, got: %v", err)
	}
}

// TestValidateBinaryInsideRoot_KindWordingPropagated asserts the
// per-tool noun ("binary" vs "file") makes it into the error so each
// caller can keep its own user-facing language.
func TestValidateBinaryInsideRoot_KindWordingPropagated(t *testing.T) {
	root := t.TempDir()
	_, err := validateBinaryInsideRoot(root, "missing.cpp", "file")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `invalid file "missing.cpp"`) {
		t.Fatalf("expected file-noun error, got: %v", err)
	}
}

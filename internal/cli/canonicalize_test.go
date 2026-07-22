package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestCanonicalizeBinaryToTarget exercises the pure core of the binary-only
// canonicalize command across its four branches: absent target, differing
// target (real replace), byte-identical target (no-op / no aside churn), and
// the running-is-canonical self-copy guard. Using small temp files keeps it
// fast and independent of os.Executable().
func TestCanonicalizeBinaryToTarget(t *testing.T) {
	writeFile := func(p string, content []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, content, 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	readFile := func(p string) []byte {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return b
	}
	asides := func(dir string) []string {
		t.Helper()
		m1, _ := filepath.Glob(filepath.Join(dir, "mcphub.old-*"))
		m2, _ := filepath.Glob(filepath.Join(dir, "mcphub.exe.old-*"))
		return append(m1, m2...)
	}

	t.Run("absent target is created", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src", "mcphub")
		target := filepath.Join(dir, "bin", "mcphub")
		writeFile(src, []byte("FRESH-v2"))

		var w bytes.Buffer
		if err := canonicalizeBinaryToTarget(&w, src, target); err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		if got := readFile(target); !bytes.Equal(got, []byte("FRESH-v2")) {
			t.Fatalf("target content = %q, want FRESH-v2", got)
		}
	})

	t.Run("differing target is replaced", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src", "mcphub")
		target := filepath.Join(dir, "bin", "mcphub")
		writeFile(src, []byte("FRESH-v2-larger-content"))
		writeFile(target, []byte("STALE-v1"))

		var w bytes.Buffer
		if err := canonicalizeBinaryToTarget(&w, src, target); err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		if got := readFile(target); !bytes.Equal(got, []byte("FRESH-v2-larger-content")) {
			t.Fatalf("target content = %q, want fresh", got)
		}
		if runtime.GOOS == "windows" {
			// The Windows lock-safe path renames the prior binary aside.
			if got := asides(filepath.Dir(target)); len(got) == 0 {
				t.Fatalf("expected a .old-<ts> aside after windows replace, found none")
			}
		}
	})

	t.Run("byte-identical target is a no-op without aside churn", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src", "mcphub")
		target := filepath.Join(dir, "bin", "mcphub")
		writeFile(src, []byte("SAME-BYTES"))
		writeFile(target, []byte("SAME-BYTES"))

		var w bytes.Buffer
		if err := canonicalizeBinaryToTarget(&w, src, target); err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		if got := readFile(target); !bytes.Equal(got, []byte("SAME-BYTES")) {
			t.Fatalf("target content changed: %q", got)
		}
		if got := asides(filepath.Dir(target)); len(got) != 0 {
			t.Fatalf("identical content must not create an aside, got %v", got)
		}
	})

	t.Run("running-is-canonical self-copy guard", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "bin", "mcphub")
		writeFile(target, []byte("RUNNING"))

		var w bytes.Buffer
		if err := canonicalizeBinaryToTarget(&w, target, target); err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		if got := readFile(target); !bytes.Equal(got, []byte("RUNNING")) {
			t.Fatalf("self-copy guard must not modify the target, got %q", got)
		}
	})
}

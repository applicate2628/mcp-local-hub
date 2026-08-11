package cli

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"mcp-local-hub/internal/binaryadmission"
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
	writeGUIPEFile := func(p, tag string) []byte {
		t.Helper()
		// Keep this cross-platform test on the same minimal PE32+ GUI fixture
		// shape as the binary-admission tests. The CLI admission helper itself
		// is Windows-tagged, while this canonicalize test also runs on Linux.
		content := make([]byte, 0x200)
		copy(content, "MZ")
		binary.LittleEndian.PutUint32(content[0x3c:0x40], 0x80)
		copy(content[0x80:], "PE\x00\x00")
		binary.LittleEndian.PutUint16(content[0x80+20:0x80+22], 0xf0)
		binary.LittleEndian.PutUint16(content[0x80+24:0x80+26], 0x20b)
		binary.LittleEndian.PutUint16(content[0x80+24+68:0x80+24+70], binaryadmission.WindowsGUISubsystem)
		content = append(content, tag...)
		writeFile(p, content)
		return content
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
		want := writeGUIPEFile(src, "FRESH-v2")

		var w bytes.Buffer
		if err := canonicalizeBinaryToTarget(&w, src, target); err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		if got := readFile(target); !bytes.Equal(got, want) {
			t.Fatalf("target differs from admitted source: got %d bytes, want %d", len(got), len(want))
		}
	})

	t.Run("differing target is replaced", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src", "mcphub")
		target := filepath.Join(dir, "bin", "mcphub")
		want := writeGUIPEFile(src, "FRESH-v2-larger-content")
		writeGUIPEFile(target, "STALE-v1")

		var w bytes.Buffer
		if err := canonicalizeBinaryToTarget(&w, src, target); err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		if got := readFile(target); !bytes.Equal(got, want) {
			t.Fatalf("target differs from admitted source: got %d bytes, want %d", len(got), len(want))
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
		want := writeGUIPEFile(src, "SAME-BYTES")
		writeFile(target, want)

		var w bytes.Buffer
		if err := canonicalizeBinaryToTarget(&w, src, target); err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		if got := readFile(target); !bytes.Equal(got, want) {
			t.Fatalf("target content changed: got %d bytes, want %d", len(got), len(want))
		}
		if got := asides(filepath.Dir(target)); len(got) != 0 {
			t.Fatalf("identical content must not create an aside, got %v", got)
		}
	})

	t.Run("running-is-canonical self-copy guard", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "bin", "mcphub")
		want := writeGUIPEFile(target, "RUNNING")

		var w bytes.Buffer
		if err := canonicalizeBinaryToTarget(&w, target, target); err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		if got := readFile(target); !bytes.Equal(got, want) {
			t.Fatalf("self-copy guard modified the target: got %d bytes, want %d", len(got), len(want))
		}
	})
}

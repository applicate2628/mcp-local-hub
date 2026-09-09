//go:build !windows

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapProductToTargetRepairsIdenticalTargetExecuteBitsWithoutCopy(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-mcphub")
	target := filepath.Join(dir, "mcphub")
	body := []byte("identical product bytes")
	if err := os.WriteFile(source, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, body, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := bootstrapProductToTarget(&out, source, target); err != nil {
		t.Fatalf("bootstrap identical target: %v", err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("byte-identical target was replaced instead of repaired in place")
	}
	if got := after.Mode().Perm(); got != 0o751 {
		t.Fatalf("target mode=%#o, want existing 0640 permissions plus execute bits (0751)", got)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("target bytes=%q err=%v", got, err)
	}
	if !strings.Contains(out.String(), "byte-identical; no copy needed") {
		t.Fatalf("output=%q", out.String())
	}

	firstMode := after.Mode().Perm()
	beforeSecond, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrapProductToTarget(&out, source, target); err != nil {
		t.Fatalf("idempotent bootstrap: %v", err)
	}
	afterSecond, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeSecond, afterSecond) || afterSecond.Mode().Perm() != firstMode {
		t.Fatalf("second bootstrap changed target identity or mode: same=%v mode=%#o want=%#o", os.SameFile(beforeSecond, afterSecond), afterSecond.Mode().Perm(), firstMode)
	}
}

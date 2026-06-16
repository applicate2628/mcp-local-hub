//go:build windows

package oneapirun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommandExtensions_WindowsUsesChildPathExt verifies the Windows
// extension-probe list comes from the CHILD env's PATHEXT (not the process
// env), with the bare-name probe first.
func TestCommandExtensions_WindowsUsesChildPathExt(t *testing.T) {
	env := []string{"PATHEXT=.EXE;.BAT"}
	got := commandExtensions("icx-cl", env)
	// "" (bare) first, then the two PATHEXT extensions in order.
	want := []string{"", ".EXE", ".BAT"}
	if len(got) != len(want) {
		t.Fatalf("commandExtensions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ext[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestCommandExtensions_DefaultWhenPathExtAbsent falls back to the cmd.exe
// default PATHEXT set when the child env carries none.
func TestCommandExtensions_DefaultWhenPathExtAbsent(t *testing.T) {
	got := commandExtensions("icx-cl", []string{"PATH=C:\\x"})
	// Must include .EXE somewhere (the load-bearing default extension).
	foundExe := false
	for _, e := range got {
		if e == ".EXE" {
			foundExe = true
		}
	}
	if !foundExe {
		t.Errorf("default PATHEXT probe set missing .EXE: %v", got)
	}
	if got[0] != "" {
		t.Errorf("bare-name probe must be first, got %v", got)
	}
}

// TestCommandExtensions_CommandAlreadyHasExtension returns only the bare-name
// probe so we never produce "icx-cl.exe.EXE".
func TestCommandExtensions_CommandAlreadyHasExtension(t *testing.T) {
	got := commandExtensions("icx-cl.exe", []string{"PATHEXT=.EXE;.BAT"})
	if len(got) != 1 || got[0] != "" {
		t.Errorf("commandExtensions for an already-extensioned command = %v, want just the bare probe", got)
	}
}

// TestResolveCommandPath_WindowsPathExtMatching proves a command passed
// WITHOUT an extension resolves to a real .exe via the PATHEXT probe (the
// Windows half of the BUG 1 fix).
func TestResolveCommandPath_WindowsPathExtMatching(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "icx-cl.exe")
	if err := os.WriteFile(exePath, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	childEnv := []string{"PATH=" + dir, "PATHEXT=.COM;.EXE;.BAT;.CMD"}
	got := resolveCommandPath("icx-cl", childEnv)
	// NTFS is case-insensitive, so the candidate "icx-cl.EXE" (built from the
	// .EXE PATHEXT token) legitimately resolves to the on-disk "icx-cl.exe".
	if !strings.EqualFold(got, exePath) {
		t.Errorf("resolveCommandPath(icx-cl) = %q, want %q (PATHEXT .EXE match)", got, exePath)
	}
}

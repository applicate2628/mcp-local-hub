//go:build windows

package cli

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/binaryadmission"
)

func writeEditorPEFixture(t *testing.T, subsystem uint16) string {
	t.Helper()
	data := make([]byte, 0x200)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:0x40], 0x80)
	copy(data[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[0x80+20:0x80+22], 0xf0)
	binary.LittleEndian.PutUint16(data[0x80+24:0x80+26], 0x20b)
	binary.LittleEndian.PutUint16(data[0x80+24+68:0x80+24+70], subsystem)
	path := filepath.Join(t.TempDir(), "editor.exe")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func installEditorTestCommand(t *testing.T, _ string, edit func(string) error) string {
	t.Helper()
	originalRun := editorCommandRun
	t.Cleanup(func() { editorCommandRun = originalRun })
	editorCommandRun = func(cmd *exec.Cmd) error {
		if len(cmd.Args) != 2 {
			t.Fatalf("editor argv=%q, want executable plus edited path", cmd.Args)
		}
		return edit(cmd.Args[1])
	}
	return writeEditorPEFixture(t, binaryadmission.WindowsGUISubsystem)
}

func TestWindowsEditorRequiresGUI(t *testing.T) {
	editedPath := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(editedPath, []byte("stable bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(editedPath)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(before)

	originalRun := editorCommandRun
	t.Cleanup(func() { editorCommandRun = originalRun })
	runCalls := 0
	editorCommandRun = func(cmd *exec.Cmd) error {
		runCalls++
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
			t.Fatalf("GUI editor attributes=%+v, want HideWindow and CREATE_NO_WINDOW", cmd.SysProcAttr)
		}
		return nil
	}

	gui := writeEditorPEFixture(t, 2)
	if err := runEditorForFile(gui, editedPath, nil, nil, nil); err != nil {
		t.Fatalf("GUI editor rejected: %v", err)
	}
	if runCalls != 1 {
		t.Fatalf("GUI editor run calls=%d, want 1", runCalls)
	}

	for _, tc := range []struct {
		name   string
		editor string
	}{
		{"CUI", writeEditorPEFixture(t, 3)},
		{"indeterminate", filepath.Join(t.TempDir(), "missing.cmd")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeCalls := runCalls
			err := runEditorForFile(tc.editor, editedPath, nil, nil, nil)
			if err == nil || !strings.Contains(err.Error(), WindowsConsoleChildRequiresGUIID) {
				t.Fatalf("error=%v, want %s", err, WindowsConsoleChildRequiresGUIID)
			}
			if runCalls != beforeCalls {
				t.Fatalf("rejected editor spawned: calls before=%d after=%d", beforeCalls, runCalls)
			}
			got, readErr := os.ReadFile(editedPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if gotHash := sha256.Sum256(got); gotHash != wantHash {
				t.Fatalf("rejected editor changed file hash: got %x want %x", gotHash, wantHash)
			}
		})
	}
}

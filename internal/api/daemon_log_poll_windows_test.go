package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPollDaemonLogWindowsRenameWithHeldReader(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	dir := DefaultLogDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "demo-default.log")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	reader, err := openDaemonLog(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := os.Rename(path, filepath.Join(dir, "rotated.log")); err != nil {
		t.Fatalf("rename while reader is open: %v", err)
	}
	if err := os.WriteFile(path, []byte("new content"), 0600); err != nil {
		t.Fatal(err)
	}
	var old [3]byte
	if _, err := reader.ReadAt(old[:], 0); err != nil || string(old[:]) != "old" {
		t.Fatalf("held reader got %q, %v", old, err)
	}
	if got, err := PollDaemonLog("demo", "default", 3, false); err != nil || got != (DaemonLogPoll{Offset: 11, Suffix: " content"}) {
		t.Fatalf("replacement poll got %+v, %v", got, err)
	}
}

func TestPollDaemonLogWindowsPathForms(t *testing.T) {
	temp := t.TempDir()
	parent := filepath.Join(temp, strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100))
	path := filepath.Join(parent, "mcp-local-hub", "logs", "demo-default.log")
	if len(path) <= 260 {
		t.Fatal("fixture is below MAX_PATH")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(temp)
	relative, err := filepath.Rel(temp, parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{parent, relative, "\\\\?\\" + parent} {
		t.Setenv("LOCALAPPDATA", dir)
		got, err := PollDaemonLog("demo", "default", 0, false)
		if err != nil || got != (DaemonLogPoll{Offset: 5, Suffix: "line\n"}) {
			t.Fatalf("dir %q: got %+v, %v", dir, got, err)
		}
	}
	// Check long UNC spelling without opening a remote share.
	unc := "\\\\host\\share\\" + strings.Repeat("a", 120) + "\\" + strings.Repeat("b", 140) + "\\demo-default.log"
	if got := daemonLogWindowsPath(unc); got != "\\\\?\\UNC\\"+unc[2:] {
		t.Fatalf("extended UNC name %q", got)
	}
}

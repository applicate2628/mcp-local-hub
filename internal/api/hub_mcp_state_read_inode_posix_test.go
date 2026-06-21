//go:build !windows

package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadStateFileInodeAnchored_FileModeReadBroadenedDefaultRelaxesStrictRejects(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "supervisor-intent.json")
	want := []byte(`{"strict_mode":false}`)
	if err := os.WriteFile(target, want, 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod target read-broadened: %v", err)
	}

	got, err := readStateFileInodeAnchoredWithStrictPolicy(target, func() bool { return false })
	if err != nil {
		t.Fatalf("default mode must read group/world-readable state file via inode-anchored fd: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("read payload = %q, want %q", got, want)
	}

	_, err = readStateFileInodeAnchoredWithStrictPolicy(target, func() bool { return true })
	if err == nil {
		t.Fatalf("strict mode must reject group/world-readable state file")
	}
	if !errors.Is(err, ErrTooLoose) {
		t.Fatalf("strict mode err = %v, want ErrTooLoose", err)
	}
}

func TestReadStateFileInodeAnchored_FileModeWriteBroadenedDefaultRejects(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "supervisor-intent.json")
	if err := os.WriteFile(target, []byte(`{"strict_mode":false}`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Chmod(target, 0o622); err != nil {
		t.Fatalf("chmod target write-broadened: %v", err)
	}

	_, err := readStateFileInodeAnchoredWithStrictPolicy(target, func() bool { return false })
	if err == nil {
		t.Fatalf("default mode must reject group/world-writable state file")
	}
	if !errors.Is(err, ErrTooLoose) {
		t.Fatalf("err = %v, want ErrTooLoose", err)
	}
}

func TestStateFileReadRemediationSingleQuotesShellMetacharacterPath(t *testing.T) {
	path := filepath.Join("/tmp", "mcp hub", "$state", "it's`bad`", "secrets.age")
	quoted := "'/tmp/mcp hub/$state/it'\\''s`bad`/secrets.age'"

	got := stateFileReadRemediation(path)
	if !strings.Contains(got, "chmod 600 "+quoted) {
		t.Fatalf("remediation must single-quote chmod path; got %q, want path %q", got, quoted)
	}
	if !strings.Contains(got, "chown $USER "+quoted) {
		t.Fatalf("remediation must single-quote chown path; got %q, want path %q", got, quoted)
	}
	if strings.Contains(got, `"`+path+`"`) {
		t.Fatalf("remediation must not double-quote shell-expanded path: %q", got)
	}
}

func TestReadStateFileInodeAnchored_ENOTDIRPreserved(t *testing.T) {
	dir := hardenedTempDir(t)
	notDir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write non-directory component: %v", err)
	}

	_, err := readStateFileInodeAnchoredWithStrictPolicy(filepath.Join(notDir, "supervisor-intent.json"), func() bool { return false })
	if err == nil {
		t.Fatal("read through a non-directory path component returned nil; want ENOTDIR")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ENOTDIR was reported as missing-file absent semantics: %v", err)
	}
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("err = %v, want errors.Is(syscall.ENOTDIR)", err)
	}
}

func TestReadStateFileInodeAnchored_FIFOFailsClosedWithoutBlocking(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "supervisor-intent.json")
	if err := syscall.Mkfifo(target, 0o600); err != nil {
		t.Skipf("mkfifo unsupported in this environment: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := readStateFileInodeAnchoredWithStrictPolicy(target, func() bool { return false })
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrIrregularFile) {
			t.Fatalf("FIFO read err = %v, want ErrIrregularFile", err)
		}
	case <-time.After(200 * time.Millisecond):
		// Pre-fix, openat(O_RDONLY) blocks on a FIFO until a writer appears.
		// Open a writer to release the stuck reader before failing the test.
		if wfd, err := syscall.Open(target, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = syscall.Close(wfd)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("reader stayed blocked on FIFO even after a writer opened the pipe")
		}
		t.Fatal("FIFO read blocked before failing closed")
	}
}

package api

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestReadStateFileInodeAnchored_WriteBroadenedParent_DefaultMode pins
// the v0.4.6 inode-anchored-read invariant: a parent directory whose
// mode grants group/world WRITE permission no longer blocks the read
// under default-relax (POSIX mode 0o022 mirrors the Windows
// CodexSandboxUsers / orphan-SID DACL pattern that motivated this
// change). The inode-anchored reader uses the openat fd directly,
// closing the path-read swap window — so the old rejection is no longer
// needed and demigrate / managed-entries-marker reads succeed on
// solo-developer hosts with broadened parent ACLs.
func TestReadStateFileInodeAnchored_WriteBroadenedParent_DefaultMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only mode probe; Windows-side coverage is the synthesis test in hub_mcp_state_dacl_windows_test.go")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	dir := t.TempDir()
	// Broaden the parent to grant group+other write (mirrors corp
	// %LOCALAPPDATA% CodexSandboxUsers Modify ACE on Windows).
	if err := os.Chmod(dir, 0o722); err != nil {
		t.Fatalf("chmod parent 0o722: %v", err)
	}
	target := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(target, []byte(`{"k":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readStateFileInodeAnchored(target)
	if err != nil {
		t.Fatalf("readStateFileInodeAnchored with write-broadened parent (default mode): %v", err)
	}
	if string(got) != `{"k":"v"}` {
		t.Errorf("content roundtrip: got %q, want %q", got, `{"k":"v"}`)
	}
}

// TestReadStateFileInodeAnchored_WriteBroadenedParent_StrictMode pins
// strict-mode precedence over the inode-anchored relax: when
// MCPHUB_REQUIRE_SINGLE_USER_HOME=1 is set, write-broadened parent
// rejects regardless of the inode-anchored read. Multi-tenant /
// corp-managed hosts keep the strict no-broadening invariant.
func TestReadStateFileInodeAnchored_WriteBroadenedParent_StrictMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only mode probe")
	}
	t.Setenv(RequireSingleUserHomeEnv, "1")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o722); err != nil {
		t.Fatalf("chmod parent 0o722: %v", err)
	}
	target := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(target, []byte(`{"k":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readStateFileInodeAnchored(target)
	if err == nil {
		t.Fatalf("strict mode must reject write-broadened parent even under inode-anchored read")
	}
}

// TestReadStateFileInodeAnchored_TightParent_DefaultMode pins the
// happy path that didn't regress: a tight parent (0o700) plus an
// owner-only file passes the gate and returns the bytes. Sanity
// guard against accidentally weakening every read.
func TestReadStateFileInodeAnchored_TightParent_DefaultMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only mode probe")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod parent 0o700: %v", err)
	}
	target := filepath.Join(dir, "tokens.json")
	want := []byte(`{"happy":"path"}`)
	if err := os.WriteFile(target, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readStateFileInodeAnchored(target)
	if err != nil {
		t.Fatalf("readStateFileInodeAnchored happy path: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("content roundtrip: got %q, want %q", got, want)
	}
}

func TestReadStateFileInodeAnchored_FileReadBroadenedDefaultModeRefusesSecretState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only mode probe; Windows DACL case lives in hub_mcp_state_dacl_windows_test.go")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}

	nonSecret := filepath.Join(dir, supervisorIntentFileLeaf)
	if err := os.WriteFile(nonSecret, []byte(`{"strict_mode":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readStateFileInodeAnchored(nonSecret); err != nil {
		t.Fatalf("default mode must still relax read-broadened non-secret state file: %v", err)
	} else if string(got) != `{"strict_mode":false}` {
		t.Fatalf("non-secret payload = %q", got)
	}

	secret := filepath.Join(dir, hubMcpTokensFileLeaf)
	if err := os.WriteFile(secret, []byte(`{"tokens":{"claude-code":"`+strings.Repeat("a", 64)+`"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readStateFileInodeAnchored(secret); err == nil {
		t.Fatalf("default mode must refuse read-broadened secret-bearing state file %s", hubMcpTokensFileLeaf)
	} else if !errors.Is(err, ErrTooLoose) {
		t.Fatalf("secret read-broadened error = %v, want ErrTooLoose", err)
	} else {
		got := err.Error()
		for _, want := range []string{"Remediate:", "chmod 600", secret} {
			if !strings.Contains(got, want) {
				t.Fatalf("secret read-broadened error missing %q: %v", want, err)
			}
		}
	}

	secretWrite := filepath.Join(dir, "secrets.age")
	if err := os.WriteFile(secretWrite, []byte(`age-encrypted-placeholder`), 0o622); err != nil {
		t.Fatal(err)
	}
	if _, err := readStateFileInodeAnchored(secretWrite); err == nil {
		t.Fatalf("default mode must refuse write-broadened secret-bearing state file")
	} else if !errors.Is(err, ErrTooLoose) {
		t.Fatalf("secret write-broadened error = %v, want ErrTooLoose", err)
	} else {
		got := err.Error()
		for _, want := range []string{"Remediate:", "chmod 600", secretWrite} {
			if !strings.Contains(got, want) {
				t.Fatalf("secret write-broadened error missing %q: %v", want, err)
			}
		}
	}
}

// TestReadStateFileInodeAnchored_RejectsSymlinkTarget pins the
// symlink-refusal invariant on the inode-anchored read: an openat
// with O_NOFOLLOW fails on a symlink, so the function returns
// ErrIrregularFile.
func TestReadStateFileInodeAnchored_RejectsSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only path; Windows symlink test in hub_mcp_state_dacl_windows_test.go")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err := readStateFileInodeAnchored(link)
	if err == nil {
		t.Fatalf("readStateFileInodeAnchored must refuse symlink target")
	}
}

// TestReadStateFileInodeAnchored_EmptyFile pins the io.ReadAll-style
// invariant: a successful read of an empty regular file returns a
// non-nil empty slice, NOT nil. Avoids ambiguity between "file did
// not exist" (err != nil) and "file exists but is empty" (err == nil,
// bytes == []byte{}).
func TestReadStateFileInodeAnchored_EmptyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only mode probe")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	target := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(target, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readStateFileInodeAnchored(target)
	if err != nil {
		t.Fatalf("readStateFileInodeAnchored on empty file: %v", err)
	}
	if got == nil {
		t.Errorf("readStateFileInodeAnchored on empty file returned nil, want empty slice")
	}
	if len(got) != 0 {
		t.Errorf("readStateFileInodeAnchored on empty file returned %d bytes, want 0", len(got))
	}
}

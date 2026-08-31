package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func symlinkStateFileForInodeAnchorTest(t *testing.T, leaf string, data []byte) string {
	t.Helper()
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "target-"+leaf)
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	link := filepath.Join(dir, leaf)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}
	return link
}

func TestInodeAnchorTrustedRootsRejectsSymlink(t *testing.T) {
	path := symlinkStateFileForInodeAnchorTest(t, LSPTrustedRootsFileLeaf, []byte(`{"version":1,"roots":["/tmp"]}`))

	if _, err := LoadLSPTrustedRoots(path); err == nil {
		t.Fatalf("LoadLSPTrustedRoots followed a symlink; want inode-anchor refusal")
	}
}

func TestInodeAnchorSettingsRejectsSymlink(t *testing.T) {
	path := symlinkStateFileForInodeAnchorTest(t, "gui-preferences.yaml", []byte("appearance.theme: dark\n"))

	if _, err := readRawSettingsMap(path); err == nil {
		t.Fatalf("readRawSettingsMap followed a symlink; want inode-anchor refusal")
	}
}

func TestInodeAnchorStrictModeIntentSymlinkFailsClosed(t *testing.T) {
	statePathsHelper(t)
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir

	target := filepath.Join(stateDir, "target-"+supervisorIntentFileLeaf)
	raw, err := json.Marshal(&SupervisorIntentFile{Version: 1, StrictMode: false})
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	link := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}

	if !readStrictModeFromIntentBestEffort() {
		t.Fatalf("readStrictModeFromIntentBestEffort followed a symlinked false intent; want fail-closed strict")
	}
}

func TestInodeAnchorSupervisorLockOwnerRejectsSymlink(t *testing.T) {
	raw, err := json.Marshal(SupervisorLockOwner{PID: 123, StartedAt: "2026-06-20T00:00:00Z"})
	if err != nil {
		t.Fatalf("marshal owner: %v", err)
	}
	path := symlinkStateFileForInodeAnchorTest(t, "supervisor.lock.owner.json", raw)
	lockPath := path[:len(path)-len(".owner.json")]

	if _, err := ReadSupervisorLockOwner(lockPath); err == nil {
		t.Fatalf("ReadSupervisorLockOwner followed a symlink; want inode-anchor refusal")
	}
}

func TestInodeAnchorIntentCollapseMergeRejectsSymlink(t *testing.T) {
	path := symlinkStateFileForInodeAnchorTest(t, "daemon-intent.json", []byte(`{"tasks":{}}`))

	if _, err := readDaemonIntentForMerge(path); err == nil {
		t.Fatalf("readDaemonIntentForMerge followed a symlink; want inode-anchor refusal")
	}
}

func TestInodeAnchorIntentCollapseBackupRejectsSymlink(t *testing.T) {
	src := symlinkStateFileForInodeAnchorTest(t, "daemon-intent.json", []byte(`{"tasks":{}}`))
	dst := filepath.Join(filepath.Dir(src), "backup-daemon-intent.json")

	if err := copyFileForBackup(src, dst); err == nil {
		t.Fatalf("copyFileForBackup followed a symlink; want inode-anchor refusal")
	}
}

func TestSerenaReconcilePidportRequiresCompositionReader(t *testing.T) {
	path := symlinkStateFileForInodeAnchorTest(t, "gui.pidport", []byte("123 456\n"))
	prior := readPidportFn
	readPidportFn = func(string) (int, int, error) { return 0, 0, fmt.Errorf("reader unavailable") }
	defer func() { readPidportFn = prior }()
	if _, _, err := readPidportFn(path); err == nil {
		t.Fatalf("missing composition reader accepted a pidport")
	}
}

func TestInodeAnchorMissingSemanticsPreserved(t *testing.T) {
	dir := hardenedTempDir(t)

	roots, err := LoadLSPTrustedRoots(filepath.Join(dir, LSPTrustedRootsFileLeaf))
	if err != nil {
		t.Fatalf("LoadLSPTrustedRoots missing err = %v", err)
	}
	if roots == nil || roots.Version != lspTrustedRootsVersion || len(roots.Roots) != 0 {
		t.Fatalf("LoadLSPTrustedRoots missing = %#v, want empty versioned roots", roots)
	}

	settings, err := readRawSettingsMap(filepath.Join(dir, "gui-preferences.yaml"))
	if err != nil {
		t.Fatalf("readRawSettingsMap missing err = %v", err)
	}
	if len(settings) != 0 {
		t.Fatalf("readRawSettingsMap missing = %#v, want empty map", settings)
	}

	intent, err := readDaemonIntentForMerge(filepath.Join(dir, "daemon-intent.json"))
	if err != nil {
		t.Fatalf("readDaemonIntentForMerge missing err = %v", err)
	}
	if intent != nil {
		t.Fatalf("readDaemonIntentForMerge missing = %#v, want nil intent", intent)
	}

	if err := copyFileForBackup(filepath.Join(dir, "no-such.json"), filepath.Join(dir, "backup.json")); err != nil {
		t.Fatalf("copyFileForBackup missing err = %v", err)
	}

	if _, err := ReadSupervisorLockOwner(filepath.Join(dir, "supervisor.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadSupervisorLockOwner missing err = %v, want ErrNotExist", err)
	}
}

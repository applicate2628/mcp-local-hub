//go:build windows

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestReadOccupiedSupervisorLockOwnerPrivateReadOnly(t *testing.T) {
	dir := hardenedTempDir(t)
	restore := SetDaemonStateRootForTest(dir)
	ResetStrictModeIntentCacheForTest()
	t.Cleanup(func() {
		restore()
		ResetStrictModeIntentCacheForTest()
	})
	t.Setenv(RequireSingleUserHomeEnv, "0")
	if OperatorRequiresSingleUserHome() {
		t.Fatal("private test state did not select the existing default-relax policy")
	}

	lockPath := filepath.Join(dir, "supervisor.lock")
	sidecar := lockPath + ".owner.json"
	if got, err := ReadOccupiedSupervisorLockOwner(dir); err == nil || got != (SupervisorLockOwner{}) {
		t.Fatalf("missing singleton had an owner: %+v error=%v", got, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("missing probe created a lock, marker or audit file: %v error=%v", entries, err)
	}
	t.Log("missing singleton probe created no marker, lock leaf or audit")

	lock, err := AcquireSupervisorLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Release)
	expected := lock.Owner()
	if got, err := ReadSupervisorLockOwner(lockPath); err != nil || got != expected {
		t.Fatalf("legacy owner reader differs: %+v error=%v", got, err)
	}

	applyFileDACLWithAuthUsersReadACE(t, sidecar)
	f, err := os.Open(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	strictErr := verifyWindowsDACLFromHandle(windows.Handle(f.Fd()))
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(strictErr, ErrDaclOutsideAllowlist) {
		t.Fatalf("fixture sidecar did not establish a read-broadened DACL: %v", strictErr)
	}

	logPath := filepath.Join(dir, "hub-mcp.log")
	logBefore, logReadErr := os.ReadFile(logPath)
	if logReadErr != nil && !errors.Is(logReadErr, os.ErrNotExist) {
		t.Fatal(logReadErr)
	}
	initialSidecar, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	lockLeaf := lockPath + ".lock"
	leafInfo, err := os.Stat(lockLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ReadOccupiedSupervisorLockOwner(dir); err != nil || got != expected {
		t.Fatalf("private occupied read-only broadened sidecar differs: %+v error=%v", got, err)
	}
	gotSidecar, err := os.ReadFile(sidecar)
	if err != nil || !bytes.Equal(gotSidecar, initialSidecar) {
		t.Fatalf("occupied read changed the owner marker: %v", err)
	}

	bad := expected
	bad.StartedAt = "malformed-fixture-birth"
	malformed, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadOccupiedSupervisorLockOwner(dir); !errors.Is(err, ErrSupervisorIPCUnavailable) || got != (SupervisorLockOwner{}) {
		t.Fatalf("occupied probe accepted malformed birth: %+v error=%v", got, err)
	}
	gotSidecar, err = os.ReadFile(sidecar)
	if err != nil || !bytes.Equal(gotSidecar, malformed) {
		t.Fatalf("refusal changed the malformed owner marker: %v", err)
	}
	afterLeaf, err := os.Stat(lockLeaf)
	if err != nil || !os.SameFile(afterLeaf, leafInfo) ||
		afterLeaf.Size() != leafInfo.Size() || !afterLeaf.ModTime().Equal(leafInfo.ModTime()) {
		t.Fatalf("read-only probe changed the occupied lock leaf: %v", err)
	}
	logAfter, afterLogErr := os.ReadFile(logPath)
	if errors.Is(logReadErr, os.ErrNotExist) {
		if !errors.Is(afterLogErr, os.ErrNotExist) {
			t.Fatalf("read-only probe created the audit log: %v", afterLogErr)
		}
	} else if afterLogErr != nil || !bytes.Equal(logAfter, logBefore) {
		t.Fatalf("read-only probe changed the audit log: %v", afterLogErr)
	}
	t.Log("private occupied broadened sidecar read without effects; malformed birth refused; legacy owner preserved")
}

//go:build linux

package process

import (
	"os"
	"testing"
)

func TestProcessOwnerMatchesCurrentLinux_CurrentPID(t *testing.T) {
	matches, err := ProcessOwnerMatchesCurrent(os.Getpid())
	if err != nil || !matches {
		t.Fatalf("ProcessOwnerMatchesCurrent(current PID) = %v, %v; want true, nil", matches, err)
	}
}

func TestProcessOwnerUIDMatchesCurrentLinux(t *testing.T) {
	uid := os.Getuid()
	if !processOwnerUIDMatchesCurrent(uid, uid) {
		t.Fatal("equal non-negative UIDs did not match")
	}
	other := uid + 1
	if other == uid {
		other = uid - 1
	}
	if processOwnerUIDMatchesCurrent(other, uid) {
		t.Fatalf("distinct UIDs %d and %d matched", other, uid)
	}
	if processOwnerUIDMatchesCurrent(-1, uid) {
		t.Fatal("negative target UID matched")
	}
}

func TestProcessOwnerMatchesCurrentLinux_InvalidOrMissingPID(t *testing.T) {
	for _, pid := range []int{0, -1, int(^uint32(0) >> 1)} {
		if matches, err := ProcessOwnerMatchesCurrent(pid); err == nil || matches {
			t.Fatalf("ProcessOwnerMatchesCurrent(%d) = %v, %v; want false, error", pid, matches, err)
		}
	}
}

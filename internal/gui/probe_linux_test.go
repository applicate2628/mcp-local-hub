//go:build linux

package gui

import (
	"os"
	"testing"
)

func TestRetainedProcessID_LinuxSelfPinsStablePIDFD(t *testing.T) {
	identity, err := retainedProcessIDImpl(os.Getpid())
	if err != nil {
		t.Fatalf("retainedProcessIDImpl(self): %v", err)
	}
	if !identity.Alive || identity.Denied || identity.Handle == 0 {
		t.Fatalf("retained self identity = %+v, want live permitted pidfd", identity)
	}
	if err := identity.Close(); err != nil {
		t.Fatalf("close retained self identity: %v", err)
	}
	if err := identity.Close(); err != nil {
		t.Fatalf("second close retained self identity: %v", err)
	}
}

func TestSameLinuxProcessIdentity_RequiresExactStableSnapshot(t *testing.T) {
	base := ProcessIdentity{Alive: true, ImagePath: "/proc/self/exe", Cmdline: []string{"mcphub", "gui"}}
	if !sameLinuxProcessIdentity(base, base) {
		t.Fatal("identical snapshots did not compare equal")
	}
	changed := base
	changed.Cmdline = []string{"mcphub", "daemon"}
	if sameLinuxProcessIdentity(base, changed) {
		t.Fatal("different argv snapshots compared equal")
	}
}

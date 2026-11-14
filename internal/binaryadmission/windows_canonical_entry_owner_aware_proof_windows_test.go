//go:build windows

package binaryadmission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// This synthetic receiver tests the native boundary. Production uses only the
// existing private actor/journal/fence receiver, never this fixture as authority.
type canonicalRecoverySourceFixture struct {
	targets          WindowsCanonicalRecoveryTargetsV1
	prior, successor WindowsCanonicalRecoveryLocationsV1
	validationError  error
}

func (s *canonicalRecoverySourceFixture) WithOriginalRecoveryInput(ctx context.Context, side WindowsCanonicalRecoverySideV1, receive func(WindowsCanonicalRecoveryTargetsV1, WindowsCanonicalRecoveryLocationsV1) error) error {
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return err
	}
	switch side {
	case WindowsCanonicalRecoveryPriorV1:
		return receive(s.targets, s.prior)
	case WindowsCanonicalRecoverySuccessorV1:
		return receive(s.targets, s.successor)
	}
	return errors.New("fixture received invalid recovery side")
}
func (s *canonicalRecoverySourceFixture) ValidateOriginalRecovery(ctx context.Context) error {
	return errors.Join(ctx.Err(), s.validationError)
}
func canonicalRecoveryFixture(root string, prior, successor WindowsCanonicalRecoveryLocationsV1) *canonicalRecoverySourceFixture {
	return &canonicalRecoverySourceFixture{targets: WindowsCanonicalRecoveryTargetsV1{root, WindowsCanonicalEntryRelativePathV1, WindowsCanonicalProductRelativePathV1, WindowsCanonicalIdentityRelativePathV1}, prior: prior, successor: successor}
}

func TestOriginalOwnerAwareImageProofUsesActualCurrentProcessHandle(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	locations := WindowsCanonicalRecoveryLocationsV1{Launcher: WindowsCanonicalRecoveryMemberV1{true, filepath.Base(executable), canonicalEntrySHA256(body)}}
	source := canonicalRecoveryFixture(filepath.Dir(executable), locations, WindowsCanonicalRecoveryLocationsV1{})
	owner, err := AcquireWindowsCanonicalEntryForRecoveryV1(context.Background(), source, WindowsCanonicalRecoveryPriorV1)
	if owner != nil {
		t.Cleanup(func() {
			if err := owner.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	var originalProcess windows.Handle
	if err = windows.DuplicateHandle(windows.CurrentProcess(), windows.CurrentProcess(), windows.CurrentProcess(), &originalProcess, windows.PROCESS_QUERY_LIMITED_INFORMATION, false, 0); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.CloseHandle(originalProcess); err != nil {
			t.Error(err)
		}
	}()
	owner.original.mu.Lock()
	err = verifyCanonicalEntryOriginalProductProcessHandle(owner.original.members[0], uintptr(originalProcess), owner.original)
	owner.original.mu.Unlock()
	if err != nil {
		t.Fatalf("actual current image proof conflicts with OWN original DELETE guard: %v", err)
	}
	// The ordinary acquisition path remains strict under the same live guard.
	strict := &WindowsCanonicalEntrySetV1{}
	_, strictErr := strict.openAbsolute(executable, false)
	if closeErr := strict.Close(); closeErr != nil {
		t.Error(closeErr)
	}
	if strictErr == nil {
		t.Fatal("general acquisition unexpectedly accepted an existing DELETE owner")
	}
	otherRoot := t.TempDir()
	otherName := "other-image.exe"
	if err = os.WriteFile(filepath.Join(otherRoot, otherName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	otherSource := canonicalRecoveryFixture(otherRoot, WindowsCanonicalRecoveryLocationsV1{Launcher: WindowsCanonicalRecoveryMemberV1{true, otherName, canonicalEntrySHA256(body)}}, WindowsCanonicalRecoveryLocationsV1{})
	other, err := AcquireWindowsCanonicalEntryForRecoveryV1(context.Background(), otherSource, WindowsCanonicalRecoveryPriorV1)
	if other != nil {
		t.Cleanup(func() {
			if err := other.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	other.original.mu.Lock()
	err = verifyCanonicalEntryOriginalProductProcessHandle(other.original.members[0], uintptr(originalProcess), other.original)
	other.original.mu.Unlock()
	if err == nil {
		t.Fatal("same bytes under a different actual native file ID supplied current image proof")
	}
	if err = owner.Close(); err != nil {
		t.Fatal(err)
	}
	owner.original.mu.Lock()
	err = verifyCanonicalEntryOriginalProductProcessHandle(owner.original.members[0], uintptr(originalProcess), owner.original)
	owner.original.mu.Unlock()
	if err == nil {
		t.Fatal("closed original owner supplied image proof")
	}
}

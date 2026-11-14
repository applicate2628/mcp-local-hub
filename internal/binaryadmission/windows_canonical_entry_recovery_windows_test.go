//go:build windows

package binaryadmission

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalRecoveryOriginalAsideRestoreAndBorrowedRuntimeFence(t *testing.T) {
	root := t.TempDir()
	aside := ".prior-product.aside"
	fixture := writeWindowsPEVersionFixture(t, WindowsArtifactRoleProduct, "0.4.41", strings.Repeat("d", 40), "2026-11-10T12:34:56Z", false)
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, aside), body, 0o600); err != nil {
		t.Fatal(err)
	}
	source := canonicalRecoveryFixture(root, WindowsCanonicalRecoveryLocationsV1{Launcher: WindowsCanonicalRecoveryMemberV1{true, aside, canonicalEntrySHA256(body)}}, WindowsCanonicalRecoveryLocationsV1{})
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
	present, received, hash, err := owner.ReadOriginal("launcher")
	if err != nil || !present || !bytes.Equal(received, body) || hash != canonicalEntrySHA256(body) {
		t.Fatalf("original recovery receiving: present=%t hash=%s error=%v", present, hash, err)
	}
	view, err := owner.RuntimeProduct(context.Background())
	if err != nil || view == nil || view.Artifact().Path != filepath.Join(root, aside) {
		t.Fatalf("borrowed actual aside runtime=%v error=%v", view, err)
	}
	result, err := owner.MoveOriginal(context.Background(), "launcher", WindowsCanonicalEntryRelativePathV1)
	if err != nil || !result.Applied || !result.Readback {
		t.Fatalf("native original restore=%+v error=%v", result, err)
	}
	restoredPresent, restored, restoredHash, err := owner.ReadOriginal("launcher")
	if err != nil || !restoredPresent || !bytes.Equal(restored, body) || restoredHash != canonicalEntrySHA256(body) {
		t.Fatalf("restored original receiving: present=%t hash=%s error=%v", restoredPresent, restoredHash, err)
	}
	if err = view.ValidateOriginal(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.validationError = errors.New("original fixture fence lost")
	if err = view.ValidateOriginal(context.Background()); err == nil {
		t.Fatal("borrowed recovery view outlived its original source/fence")
	}
	result, err = owner.MoveOriginal(context.Background(), "launcher", ".must-not-move")
	if err == nil || result.Attempted || result.Applied {
		t.Fatalf("effect after original source/fence failure: %+v %v", result, err)
	}
	if err = owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err = view.ValidateOriginal(context.Background()); err == nil {
		t.Fatal("borrowed recovery view survived owner Close")
	}
}

func TestCanonicalRecoveryRefusesMissingHashMismatchAndCrossSideAlias(t *testing.T) {
	for _, mode := range []string{"missing", "hash-mismatch", "cross-side-alias"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			relative := ".owned-prior.aside"
			body := []byte("original preimage")
			if mode != "missing" {
				if err := os.WriteFile(filepath.Join(root, relative), body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			hash := canonicalEntrySHA256(body)
			if mode == "hash-mismatch" {
				hash = strings.Repeat("e", 64)
			}
			locations := WindowsCanonicalRecoveryLocationsV1{Launcher: WindowsCanonicalRecoveryMemberV1{true, relative, hash}}
			opposite := WindowsCanonicalRecoveryLocationsV1{}
			if mode == "cross-side-alias" {
				opposite = locations
			}
			source := canonicalRecoveryFixture(root, locations, opposite)
			owner, err := AcquireWindowsCanonicalEntryForRecoveryV1(context.Background(), source, WindowsCanonicalRecoveryPriorV1)
			if err == nil || owner == nil {
				t.Fatalf("invalid classified source owner=%v error=%v", owner, err)
			}
			if err = owner.Close(); err != nil {
				t.Fatal(err)
			}
			if mode != "missing" {
				file, err := os.OpenFile(filepath.Join(root, relative), os.O_WRONLY, 0)
				if err != nil {
					t.Fatalf("partial recovery guard not settled: %v", err)
				}
				if err = file.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCanonicalRecoverySelectedAbsenceDoesNotClaimFinalVacancy(t *testing.T) {
	root := t.TempDir()
	body := []byte("known successor remains present")
	if err := os.WriteFile(filepath.Join(root, WindowsCanonicalEntryRelativePathV1), body, 0o600); err != nil {
		t.Fatal(err)
	}
	successor := WindowsCanonicalRecoveryLocationsV1{Launcher: WindowsCanonicalRecoveryMemberV1{true, WindowsCanonicalEntryRelativePathV1, canonicalEntrySHA256(body)}}
	source := canonicalRecoveryFixture(root, WindowsCanonicalRecoveryLocationsV1{}, successor)
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
	present, received, hash, err := owner.ReadOriginal("launcher")
	if err != nil || present || received != nil || hash != "" {
		t.Fatal("selected-side absence was misreported")
	}
	result, err := owner.DiscardOriginal(context.Background(), "launcher", "")
	if err != nil || result.Attempted || result.Applied || result.Readback {
		t.Fatalf("selected absence falsely claimed physical deletion/vacancy: %+v %v", result, err)
	}
	if result, err = owner.MoveOriginal(context.Background(), "launcher", ".must-not-exist"); err == nil || result.Applied {
		t.Fatal("absent selected member moved a known successor")
	}
	actual, err := os.ReadFile(filepath.Join(root, WindowsCanonicalEntryRelativePathV1))
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatal("selected absent-side operation changed the successor")
	}
}

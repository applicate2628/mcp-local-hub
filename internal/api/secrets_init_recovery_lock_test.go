package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
)

func TestSecretsInitRecoveryLockSettlement(t *testing.T) {
	primary := errors.New("initial operation failed")
	recovery := errors.New("recovery operation failed")
	releaseFailure := errors.New("recovery lock release failed")
	for _, operation := range []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"primary", primary},
		{"primary-and-recovery", errors.Join(primary, recovery)},
	} {
		for _, failRelease := range []bool{false, true} {
			name := operation.name + "/release-success"
			if failRelease {
				name = operation.name + "/release-failure"
			}
			t.Run(name, func(t *testing.T) {
				owner, _ := recoveryFixture(t)
				lockPath := filepath.Join(owner.root, "secrets-init-recovery.lock")
				previous := flockUnlockFn
				var retained *flock.Flock
				unlockCalls := 0
				flockUnlockFn = func(lock *flock.Flock) error {
					if lock.Path() != lockPath {
						return previous(lock)
					}
					unlockCalls++
					if failRelease {
						retained = lock
						return releaseFailure
					}
					return previous(lock)
				}
				t.Cleanup(func() {
					flockUnlockFn = previous
					if retained != nil {
						if err := retained.Unlock(); err != nil {
							t.Errorf("release retained fixture lock: %v", err)
						}
					}
					unconfirmedLockReleasesMu.Lock()
					delete(unconfirmedLockReleases, lockPath)
					unconfirmedLockReleasesMu.Unlock()
				})
				called := 0
				err := owner.locked(func() error { called++; return operation.err })
				if called != 1 {
					t.Fatalf("operation calls = %d, want 1; err=%v", called, err)
				}
				for _, cause := range []error{primary, recovery} {
					if errors.Is(err, cause) != errors.Is(operation.err, cause) {
						t.Fatalf("operation cause %v changed: %v", cause, err)
					}
				}
				if errors.Is(err, releaseFailure) != failRelease || errors.Is(err, ErrLockReleaseUnconfirmed) != failRelease {
					t.Fatalf("release outcome=%v, failure=%t", err, failRelease)
				}
				if IsAppliedLockReleaseUnconfirmed(err) {
					t.Fatalf("generic recovery lock claimed a durable mutation: %v", err)
				}
				if err != nil {
					// The API's recovery envelope must retain the release cause even
					// when the initialization failure already occupies InitError.
					routed := recoveryPersistenceFailure(err, primary, PublicRecoveryReference{Status: "incomplete"})
					if !errors.Is(routed, primary) || errors.Is(routed, recovery) != errors.Is(operation.err, recovery) ||
						errors.Is(routed, releaseFailure) != failRelease || errors.Is(routed, ErrLockReleaseUnconfirmed) != failRelease {
						t.Fatalf("API recovery envelope lost causes: %v", routed)
					}
				}
				if !failRelease {
					if err != operation.err {
						t.Fatalf("successful release changed original result: %v", err)
					}
					if again := owner.locked(func() error { return nil }); again != nil {
						t.Fatalf("released leaf cannot be acquired: %v", again)
					}
					return
				}
				// Failure left the exact native lock retained. The next request must
				// observe that ledger entry before trying the operating-system lock.
				againCalled := false
				again := owner.locked(func() error { againCalled = true; return nil })
				if againCalled || unlockCalls != 1 || !errors.Is(again, ErrLockReleaseUnconfirmed) || !errors.Is(again, releaseFailure) {
					t.Fatalf("reentry after failed release: called=%t unlocks=%d err=%v", againCalled, unlockCalls, again)
				}
			})
		}
	}
}

func TestSecretsInitRecoveryLockAcquireFailure(t *testing.T) {
	owner, _ := recoveryFixture(t)
	lockPath := filepath.Join(owner.root, "secrets-init-recovery.lock")
	if err := os.Mkdir(lockPath, 0700); err != nil {
		t.Fatal(err)
	}
	called := false
	err := owner.locked(func() error { called = true; return nil })
	if err == nil || called || errors.Is(err, ErrLockReleaseUnconfirmed) {
		t.Fatalf("failed acquisition = %v, operation called=%t", err, called)
	}
}

func TestSecretsInitRecoveryLockReleasesOnPanic(t *testing.T) {
	owner, _ := recoveryFixture(t)
	marker := new(int)
	func() {
		defer func() {
			if got := recover(); got != marker {
				t.Errorf("panic = %v, want original marker", got)
			}
		}()
		_ = owner.locked(func() error { panic(marker) })
	}()
	if err := owner.locked(func() error { return nil }); err != nil {
		t.Fatalf("panic stranded recovery lock: %v", err)
	}
}

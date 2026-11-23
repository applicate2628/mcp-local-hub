package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestWindowsProductNamespaceLockCreation(t *testing.T) {
	for _, collision := range []bool{false, true} {
		name := "restrictive_create"
		if collision {
			name = "no_replace_receives_original_winner"
		}
		t.Run(name, func(t *testing.T) {
			root := hardenedTempDir(t)
			path := filepath.Join(root, UpgradeTransactionLockFileLeaf)
			scope, err := OpenWindowsProductStateNamespace(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := scope.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("exercise needs genuine initial absence")
			}
			var winner os.FileInfo
			if collision {
				scope.native.beforeLockCreate = func(leaf string) error {
					if leaf != UpgradeTransactionLockFileLeaf {
						return errors.New("unexpected lock leaf")
					}
					contender := flock.New(path)
					if err := contender.Lock(); err != nil {
						return err
					}
					t.Cleanup(func() {
						if err := contender.Unlock(); err != nil {
							t.Error(err)
						}
					})
					var err error
					winner, err = os.Stat(path)
					return err
				}
				bounded, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				if _, err := AcquireUpgradeFenceBound(bounded, root, scope); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("winner was neither received nor busy: %v", err)
				}
			} else {
				held, err := AcquireUpgradeFenceBound(t.Context(), root, scope)
				if err != nil {
					t.Fatal(err)
				}
				if err := held.Release(); err != nil {
					t.Fatal(err)
				}
			}
			actual, err := os.Stat(path)
			if err != nil || actual.Size() != 0 || !actual.Mode().IsRegular() {
				t.Fatalf("unproved original lock creation: %v", err)
			}
			guard := scope.native.locks[UpgradeTransactionLockFileLeaf]
			if guard.handle == 0 || guard.identity.FileID == ([16]byte{}) {
				t.Fatal("verified actual marker guard missing")
			}
			if collision && (winner == nil || !os.SameFile(winner, actual)) {
				t.Fatal("no-replace changed the winning native object")
			}
			if !collision {
				if err := verifyWindowsDACLFromHandle(guard.handle); err != nil {
					t.Fatal("lock was not born restrictive", err)
				}
			}
		})
	}
}

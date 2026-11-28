package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
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

func TestWindowsProductNamespaceStreamsOriginalPrivateLeaf(t *testing.T) {
	root := hardenedTempDir(t)
	const leaf = "archive"
	path := filepath.Join(root, leaf)
	body := bytes.Repeat([]byte("inert archive block"), 100000)
	created, err := SecureCreateOwnerOnlyFile(path, body)
	if err != nil || !created {
		t.Fatalf("archive fixture create: %v, %v", created, err)
	}
	scope, err := OpenWindowsProductStateNamespace(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}()
	var saved io.Reader
	received := sha256.New()
	err = scope.WithBoundFileReader(t.Context(), leaf, func(r io.Reader) error {
		saved = r
		if err := scope.Close(); err == nil {
			t.Fatal("active reader released its parent")
		}
		if err := os.Rename(path, filepath.Join(root, "replacement")); err == nil {
			t.Fatal("held archive was renameable")
		}
		n, err := io.Copy(received, r)
		if n != int64(len(body)) {
			t.Fatalf("read %d bytes, want %d", n, len(body))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(body)
	if !bytes.Equal(received.Sum(nil), want[:]) {
		t.Fatal("archive digest changed")
	}
	if n, err := saved.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("retained reader was not revoked: %d, %v", n, err)
	}
	if err := os.Rename(path, filepath.Join(root, "after-read")); err != nil {
		t.Fatalf("archive stream held a leaked leaf: %v", err)
	}
}

func TestWindowsProductNamespaceRefusesIncompleteArchiveRead(t *testing.T) {
	root := hardenedTempDir(t)
	const leaf = "archive"
	path := filepath.Join(root, leaf)
	created, err := SecureCreateOwnerOnlyFile(path, []byte("controlled incomplete stream"))
	if err != nil || !created {
		t.Fatalf("archive fixture create: %v, %v", created, err)
	}
	scope, err := OpenWindowsProductStateNamespace(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}()
	err = scope.WithBoundFileReader(t.Context(), leaf, func(r io.Reader) error {
		_, err := io.ReadFull(r, make([]byte, 1))
		return err
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("incomplete callback was accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = scope.WithBoundFileReader(ctx, leaf, func(r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		cancel()
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled callback was accepted: %v", err)
	}
	for _, refused := range []string{"missing", "..\\archive", leaf + ":alternate"} {
		if err := scope.WithBoundFileReader(t.Context(), refused, func(io.Reader) error {
			t.Fatal("missing or unsafe leaf reached a reader")
			return nil
		}); err == nil {
			t.Fatal("missing or unsafe leaf was accepted")
		}
	}
}

func TestWindowsProductNamespaceRetainsUnconfirmedArchiveClose(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	readErr := errors.New("fixture original read error")
	closeErr := errors.New("fixture original close error")
	native := &productStateNamespaceNative{borrows: 1}
	scope := &WindowsProductStateNamespace{native: native}
	err := native.finishBoundFileRead(ctx, readErr, closeErr)
	if !errors.Is(err, readErr) || !errors.Is(err, closeErr) || !errors.Is(err, context.Canceled) || native.borrows != 0 {
		t.Fatalf("original settlement incomplete: %v", err)
	}
	if err := scope.Validate(t.Context()); !errors.Is(err, closeErr) {
		t.Fatalf("unconfirmed close left the namespace reusable: %v", err)
	}
	if err := scope.Close(); !errors.Is(err, closeErr) || native.closed {
		t.Fatalf("unconfirmed close claimed final settlement: %v", err)
	}
}

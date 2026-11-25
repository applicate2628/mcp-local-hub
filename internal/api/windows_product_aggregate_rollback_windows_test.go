//go:build windows

package api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsProductAggregateRollbackContinuation(t *testing.T) {
	for _, name := range []string{"same_file", "equal_bytes_replaced_after_receipt", "mixed_legacy", "destination_exists", "late_rename_error", "changed_source", "receipt_from_another_directory", "released_fence"} {
		t.Run(name, func(t *testing.T) {
			root := hardenedTempDir(t)
			ns, err := OpenWindowsProductStateNamespace(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := ns.Close(); err != nil {
					t.Error(err)
				}
			})
			fence, err := AcquireUpgradeFenceBound(t.Context(), root, ns)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := fence.Release(); err != nil {
					t.Error(err)
				}
			})
			raw := []byte("received terminal Product journal fixture")
			create := func(leaf string, data []byte) {
				t.Helper()
				created, err := SecureCreateOwnerOnlyFile(filepath.Join(root, leaf), data)
				if err != nil || !created {
					t.Fatal("create original", leaf, err)
				}
			}
			create(productRecoveryJournalLeaf, raw)
			identity := func(path string) ConfigParentDirectoryIdentityV1 {
				t.Helper()
				h := openWindowsPathForNamespaceTest(t, path, windows.FILE_READ_ATTRIBUTES, false)
				id, readErr := queryConfigParentDirectoryIdentity(h)
				if err := errors.Join(readErr, windows.CloseHandle(h)); err != nil {
					t.Fatal(err)
				}
				return id
			}
			original := identity(filepath.Join(root, productRecoveryJournalLeaf))
			want := raw
			fault := errors.New("late rename fault")
			switch name {
			case "mixed_legacy":
				create(productPairRecoveryJournalLeaf, raw)
			case "destination_exists":
				create(WindowsProductAggregateRollbackLeaf, raw)
			case "late_rename_error":
				ns.native.afterRollbackRename = func() error { return fault }
			case "changed_source":
				want = bytes.Repeat([]byte("x"), len(raw))
			}
			if rejected, err := ns.TransferAggregateRollback(t.Context(), nil, want); err == nil || rejected != nil {
				t.Fatal("bare namespace authorised transfer", err)
			}
			if name == "released_fence" {
				if err := fence.Release(); err != nil {
					t.Fatal(err)
				}
			}
			moved, err := ns.TransferAggregateRollback(t.Context(), fence, want)
			switch name {
			case "mixed_legacy", "destination_exists", "changed_source", "released_fence":
				if err == nil || moved != nil {
					t.Fatal("unqualified transfer", err)
				}
				if identity(filepath.Join(root, productRecoveryJournalLeaf)) != original {
					t.Fatal("lost original active V3")
				}
				return
			case "late_rename_error":
				if !errors.Is(err, fault) || moved == nil {
					t.Fatal("lost original error or actual move", err)
				}
			default:
				if err != nil || moved == nil {
					t.Fatal("transfer", err)
				}
			}
			pending := filepath.Join(root, WindowsProductAggregateRollbackLeaf)
			if identity(pending) != original {
				t.Fatal("renamed object was replaced")
			}
			if _, err := os.Stat(filepath.Join(root, productRecoveryJournalLeaf)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("active journal after transfer", err)
			}
			received, err := ns.ReceiveAggregateRollback(t.Context(), int64(len(raw)))
			if err != nil || !moved.matches(received) {
				t.Fatal("fresh receipt", err)
			}
			if name == "equal_bytes_replaced_after_receipt" {
				if err := os.Rename(pending, pending+".original"); err != nil {
					t.Fatal(err)
				}
				create(WindowsProductAggregateRollbackLeaf, raw)
				if ns.RetireAggregateRollback(t.Context(), fence, received) == nil {
					t.Fatal("retired equal-byte replacement")
				}
				if _, err := os.Stat(pending); err != nil {
					t.Fatal("replacement disappeared", err)
				}
				return
			}
			if name == "receipt_from_another_directory" {
				otherRoot := hardenedTempDir(t)
				other, err := OpenWindowsProductStateNamespace(t.Context(), otherRoot)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := other.Close(); err != nil {
						t.Error(err)
					}
				}()
				if _, err := SecureCreateOwnerOnlyFile(filepath.Join(otherRoot, WindowsProductAggregateRollbackLeaf), raw); err != nil {
					t.Fatal(err)
				}
				if other.RetireAggregateRollback(t.Context(), fence, received) == nil {
					t.Fatal("accepted foreign parent")
				}
			}
			if err := ns.RetireAggregateRollback(t.Context(), fence, received); err != nil {
				t.Fatal(err)
			}
			if _, err := ns.ReceiveAggregateRollback(t.Context(), int64(len(raw))); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("retirement unconfirmed", err)
			}
		})
	}
}

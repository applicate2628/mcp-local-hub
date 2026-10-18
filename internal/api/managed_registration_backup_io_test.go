package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

// TestW10_BackupIOOwnerOnlyCreateAndPinnedRead catches a managed backup bridge
// that bypasses the current API security owners or treats an existing backup as
// fresh evidence for a client mutation.
func TestW10_BackupIOOwnerOnlyCreateAndPinnedRead(t *testing.T) {
	root := t.TempDir()
	contents := []byte("frozen client config")
	digest := w10BackupDigest(contents)
	io := managedRegistrationBackupIO{}

	receipt, err := io.Create(context.Background(), root, "config.json.managed-backup", contents, digest)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if receipt != (clients.ManagedRegistrationBackupCreateReceipt{Disposition: clients.ManagedRegistrationBackupCommitted, CleanupDisposition: clients.ManagedRegistrationBackupCleanupNotRequired}) {
		t.Fatalf("Create receipt = %#v", receipt)
	}
	got, err := io.Read(context.Background(), root, "config.json.managed-backup", digest)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(contents) {
		t.Fatalf("Read = %q, want %q", got, contents)
	}
	got[0] = 'X'
	reloaded, err := io.Read(context.Background(), root, "config.json.managed-backup", digest)
	if err != nil || string(reloaded) != string(contents) {
		t.Fatalf("fresh pinned Read = %q, %v", reloaded, err)
	}

	receipt, err = io.Create(context.Background(), root, "config.json.managed-backup", contents, digest)
	if !errors.Is(err, clients.ErrManagedRegistrationBackupExists) {
		t.Fatalf("second Create error = %v, want ErrManagedRegistrationBackupExists", err)
	}
	if receipt != (clients.ManagedRegistrationBackupCreateReceipt{Disposition: clients.ManagedRegistrationBackupNotCreated, CleanupDisposition: clients.ManagedRegistrationBackupCleanupNotRequired}) {
		t.Fatalf("second Create receipt = %#v", receipt)
	}
}

// TestW10_BackupIORejectsBeforeCreate catches a bridge that writes despite a
// cancelled operation or an unpinned/mismatched borrowed config byte slice.
func TestW10_BackupIORejectsBeforeCreate(t *testing.T) {
	root := t.TempDir()
	io := managedRegistrationBackupIO{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	receipt, err := io.Create(ctx, root, "cancelled", []byte("x"), w10BackupDigest([]byte("x")))
	if !errors.Is(err, context.Canceled) || receipt != (clients.ManagedRegistrationBackupCreateReceipt{Disposition: clients.ManagedRegistrationBackupNotCreated, CleanupDisposition: clients.ManagedRegistrationBackupCleanupNotRequired}) {
		t.Fatalf("cancelled Create = %#v, %v", receipt, err)
	}

	path := filepath.Join(root, "mismatch")
	receipt, err = io.Create(context.Background(), root, "mismatch", []byte("x"), w10BackupDigest([]byte("different")))
	if err == nil || receipt != (clients.ManagedRegistrationBackupCreateReceipt{Disposition: clients.ManagedRegistrationBackupNotCreated, CleanupDisposition: clients.ManagedRegistrationBackupCleanupNotRequired}) {
		t.Fatalf("mismatched Create = %#v, %v", receipt, err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("mismatched Create left %s: %v", path, statErr)
	}
}

// TestW10_BackupIOPostCreateOutcomesRetainCandidate catches an adapter that
// rewrites a post-publication cancellation or create error into no-create, or
// deletes the candidate before recovery can checksum-read it.
func TestW10_BackupIOPostCreateOutcomesRetainCandidate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		creatorErr  error
		cancelAfter bool
		wantReceipt clients.ManagedRegistrationBackupCreateReceipt
		wantErr     error
	}{
		{
			name:        "cancellation after committed create",
			cancelAfter: true,
			wantReceipt: clients.ManagedRegistrationBackupCreateReceipt{Disposition: clients.ManagedRegistrationBackupCommitted, CleanupDisposition: clients.ManagedRegistrationBackupCleanupNotRequired},
			wantErr:     context.Canceled,
		},
		{
			name:        "error after candidate publication",
			creatorErr:  errors.New("injected post-publication error"),
			wantReceipt: clients.ManagedRegistrationBackupCreateReceipt{Disposition: clients.ManagedRegistrationBackupCommitUncertain, CleanupDisposition: clients.ManagedRegistrationBackupCleanupUnconfirmed},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			contents := []byte("retained candidate")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			io := managedRegistrationBackupIO{
				createOwnerOnly: func(path string, contents []byte) (bool, error) {
					created, err := SecureCreateOwnerOnlyFile(path, contents)
					if err != nil || !created {
						return created, err
					}
					if tc.cancelAfter {
						cancel()
					}
					return created, tc.creatorErr
				},
			}
			receipt, err := io.Create(ctx, root, "candidate", contents, w10BackupDigest(contents))
			if receipt != tc.wantReceipt {
				t.Fatalf("Create receipt = %#v, want %#v", receipt, tc.wantReceipt)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Create error = %v, want %v", err, tc.wantErr)
				}
			} else if err == nil {
				t.Fatal("Create error = nil, want injected post-publication error")
			}
			retained, readErr := os.ReadFile(filepath.Join(root, "candidate"))
			if readErr != nil || string(retained) != string(contents) {
				t.Fatalf("retained candidate = %q, %v", retained, readErr)
			}
		})
	}
}

func w10BackupDigest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

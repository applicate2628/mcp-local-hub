package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"mcp-local-hub/internal/clients"
)

// managedRegistrationBackupIO is an operation-scoped bridge from the locked
// client mutation to the existing API-owned owner-only create and no-follow
// read primitives. It retains no context, path, or config bytes.
type managedRegistrationBackupIO struct {
	// createOwnerOnly is an operation-local deterministic test seam. A zero
	// value always calls the one production owner below; it is not exported,
	// stored globally, or retained beyond this operation value.
	createOwnerOnly func(path string, contents []byte) (created bool, err error)
}

func (io managedRegistrationBackupIO) Create(ctx context.Context, parentDir, leaf string, contents []byte, expectedSHA256 string) (clients.ManagedRegistrationBackupCreateReceipt, error) {
	notCreated := clients.ManagedRegistrationBackupCreateReceipt{Disposition: clients.ManagedRegistrationBackupNotCreated, CleanupDisposition: clients.ManagedRegistrationBackupCleanupNotRequired}
	if ctx == nil {
		return notCreated, newManagedRegistrationFailure(ManagedRegistrationFailureBackupReferenceInvalid, nil)
	}
	if err := ctx.Err(); err != nil {
		return notCreated, err
	}
	if !managedRegistrationBackupDigestMatches(contents, expectedSHA256) {
		return notCreated, newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, nil)
	}
	creator := io.createOwnerOnly
	if creator == nil {
		creator = SecureCreateOwnerOnlyFile
	}
	created, err := creator(filepath.Join(parentDir, leaf), contents)
	if err != nil {
		return clients.ManagedRegistrationBackupCreateReceipt{Disposition: clients.ManagedRegistrationBackupCommitUncertain, CleanupDisposition: clients.ManagedRegistrationBackupCleanupUnconfirmed}, err
	}
	if !created {
		return notCreated, clients.ErrManagedRegistrationBackupExists
	}
	committed := clients.ManagedRegistrationBackupCreateReceipt{Disposition: clients.ManagedRegistrationBackupCommitted, CleanupDisposition: clients.ManagedRegistrationBackupCleanupNotRequired}
	if err := ctx.Err(); err != nil {
		return committed, err
	}
	return committed, nil
}

func (managedRegistrationBackupIO) Read(ctx context.Context, parentDir, leaf string, expectedSHA256 string) ([]byte, error) {
	if ctx == nil {
		return nil, newManagedRegistrationFailure(ManagedRegistrationFailureBackupReferenceInvalid, nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !managedRegistrationBackupDigestValid(expectedSHA256) {
		return nil, newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, nil)
	}
	return ReadClientConfigBackupBeneathRootNoFollow(ctx, parentDir, []string{leaf}, expectedSHA256)
}

func managedRegistrationBackupDigestMatches(contents []byte, expectedSHA256 string) bool {
	if !managedRegistrationBackupDigestValid(expectedSHA256) {
		return false
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]) == expectedSHA256
}

func managedRegistrationBackupDigestValid(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

var _ clients.ManagedRegistrationBackupIO = managedRegistrationBackupIO{}

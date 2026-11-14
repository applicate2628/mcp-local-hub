package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"mcp-local-hub/internal/binaryadmission"
)

func runEntrySetValidation(options validatorOptions, stdout, stderr io.Writer) int {
	binding, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(options.expectedEntrySetBuildID, options.expectedCanonicalEntrySHA256, options.canonicalEntryReverseProjection)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	owned, err := binaryadmission.AdmitWindowsCanonicalEntrySetV1(ctx, options.validateEntrySet, binding)
	if err != nil {
		if owned != nil {
			err = errors.Join(err, owned.Close())
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	if owned == nil {
		fmt.Fprintln(stderr, "complete entry-set owner is absent")
		return 1
	}
	snapshot, receivedErr := owned.Snapshot()
	if receivedErr == nil {
		identityHash := sha256.Sum256(snapshot.IdentityBytes)
		artifact := snapshot.ProductArtifact
		if snapshot.Schema != "windows-canonical-entry-set-v1" || snapshot.BuildID != binding.BuildID() || snapshot.CanonicalEntry.SHA256 != binding.ExpectedCanonicalEntrySHA256() || artifact.SHA256 != snapshot.Product.SHA256 || artifact.Version != options.expectedVersion || artifact.Commit != options.expectedCommit || artifact.BuildDate != options.expectedBuildDate || artifact.Role != binaryadmission.WindowsArtifactRoleProduct || artifact.Subsystem != 2 || artifact.Machine != snapshot.Machine || snapshot.Machine == 0 || len(snapshot.IdentityBytes) == 0 || hex.EncodeToString(identityHash[:]) != snapshot.Identity.SHA256 {
			receivedErr = errors.New("complete entry-set snapshot does not match the original build")
		}
	}
	// Preserve the first receiving error and the actual cached close outcome.
	if err := errors.Join(receivedErr, owned.Close()); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result := struct {
		Schema                   string `json:"schema"`
		BuildID                  string `json:"build_id"`
		RegistryProjectionSHA256 string `json:"registry_projection_sha256"`
		CanonicalEntrySHA256     string `json:"canonical_entry_sha256"`
		ProductSHA256            string `json:"product_sha256"`
		IdentitySHA256           string `json:"identity_sha256"`
	}{snapshot.Schema, snapshot.BuildID, snapshot.RegistryProjectionSHA256, snapshot.CanonicalEntry.SHA256, snapshot.Product.SHA256, snapshot.Identity.SHA256}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

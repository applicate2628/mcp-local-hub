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
	"path"

	"mcp-local-hub/internal/binaryadmission"
)

// This checks build DATA after all compiler writers have returned. It never
// acquires a native installation/staging owner and never falls back from one.
func receiveEntrySetBuildObservation(options validatorOptions, binding binaryadmission.WindowsCanonicalEntryBuildBindingV1, observation binaryadmission.WindowsCanonicalEntryBuildObservationV1) error {
	snapshot := observation.EntrySet
	artifact := snapshot.ProductArtifact
	identityHash := sha256.Sum256(snapshot.IdentityBytes)
	if snapshot.Schema != "windows-canonical-entry-set-v1" || snapshot.BuildID != binding.BuildID() || snapshot.CanonicalEntry.SHA256 != binding.ExpectedCanonicalEntrySHA256() || snapshot.ReverseProjection != binding.ReverseProjection() || snapshot.CanonicalEntryRelativePath != "mcphub.exe" || snapshot.ProductRelativePath != "mcphub-product/mcphub.exe" || snapshot.IdentityRelativePath != "windows-product-identity.json" || artifact.SHA256 != snapshot.Product.SHA256 || artifact.Version != options.expectedVersion || artifact.Commit != options.expectedCommit || artifact.BuildDate != options.expectedBuildDate || artifact.Role != binaryadmission.WindowsArtifactRoleProduct || artifact.Subsystem != 2 || artifact.Machine != snapshot.Machine || snapshot.Machine == 0 || len(snapshot.IdentityBytes) == 0 || hex.EncodeToString(identityHash[:]) != snapshot.Identity.SHA256 {
		return errors.New("portable entry-set DATA does not match the original build")
	}
	if options.expectedSourceEpoch != "" && observation.SourceEpoch != options.expectedSourceEpoch {
		return errors.New("portable entry-set DATA does not match the original source epoch")
	}
	return nil
}

type entrySetBuildSummary struct {
	Schema                   string `json:"schema"`
	BuildID                  string `json:"build_id"`
	RegistryProjectionSHA256 string `json:"registry_projection_sha256"`
	CanonicalEntrySHA256     string `json:"canonical_entry_sha256"`
	ProductSHA256            string `json:"product_sha256"`
	IdentitySHA256           string `json:"identity_sha256"`
	SourceEpoch              string `json:"source_epoch"`
}

func runEntrySetBuildValidation(options validatorOptions, stdout, stderr io.Writer) int {
	binding, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(options.expectedEntrySetBuildID, options.expectedCanonicalEntrySHA256, options.canonicalEntryReverseProjection)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if options.runtimePayload != "" || options.sourceRoot != "" || options.sourceSnapshot != "" {
		return runCurrentEntrySetBuildValidation(ctx, options, binding, stdout, stderr)
	}
	observation, err := binaryadmission.CheckWindowsCanonicalEntryBuildV1(ctx, options.validateEntrySetBuild, binding)
	if err == nil {
		err = receiveEntrySetBuildObservation(options, binding, observation)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	snapshot := observation.EntrySet
	result := entrySetBuildSummary{"windows-canonical-entry-build-observation-v1", snapshot.BuildID, snapshot.RegistryProjectionSHA256, snapshot.CanonicalEntry.SHA256, snapshot.Product.SHA256, snapshot.Identity.SHA256, observation.SourceEpoch}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runCurrentEntrySetBuildValidation(ctx context.Context, options validatorOptions, binding binaryadmission.WindowsCanonicalEntryBuildBindingV1, stdout, stderr io.Writer) int {
	source, registry, files, err := binaryadmission.ReadWindowsProductSourceV4(ctx, options.sourceRoot, options.sourceSnapshot)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if source.SourceEpoch != options.expectedSourceEpoch {
		fmt.Fprintln(stderr, "PUBLISHER_SOURCE_EPOCH_MISMATCH")
		return 1
	}
	if path.Join(path.Dir(registry.Artifacts[0].Filename), options.runtimePayload) != registry.RuntimePayload.Filename {
		fmt.Fprintln(stderr, "RUNTIME_BUILD_INDEX_LOCATOR_MISMATCH")
		return 1
	}
	data, err := binaryadmission.CheckWindowsCurrentBundleBuild(ctx, options.validateEntrySetBuild, binding, source, registry)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !data.Complete || data.Identity.Version != options.expectedVersion || data.Identity.Commit != options.expectedCommit || data.Identity.BuildDate != options.expectedBuildDate {
		fmt.Fprintln(stderr, "portable current bundle DATA does not match the original build")
		return 1
	}
	if err := binaryadmission.ValidateWindowsProductRuntimeSourceV4(data.Runtime.Payload, registry, files); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	summary := entrySetBuildSummary{"windows-canonical-entry-build-observation-v1", data.BuildID, data.RegistryProjectionSHA256, data.CanonicalEntrySHA256, data.ProductSHA256, data.IdentitySHA256, data.SourceEpoch}
	if err := json.NewEncoder(stdout).Encode(summary); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

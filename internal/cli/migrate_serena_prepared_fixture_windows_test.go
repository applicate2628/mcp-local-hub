//go:build windows && serena_qualification && test_state_path_env

package cli

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

// The executing qualification publisher and the ordinary runtime Product have
// distinct roles. These tests retain their real preparation, fan-out, write and
// lock oracles; their existing reap/start seams never launch the Product.
func migrateSerenaPreparedTestEnv(t *testing.T) (stateDir, manifestDir string) {
	t.Helper()
	productPath := os.Getenv("MCPHUB_SERENA_QUALIFICATION_PRODUCT")
	productSHA := os.Getenv("MCPHUB_SERENA_QUALIFICATION_PRODUCT_SHA256")
	if !filepath.IsAbs(productPath) || len(productSHA) != 64 {
		t.Fatal("SERENA_QUALIFICATION_PRODUCT_REQUIRED: set an absolute MCPHUB_SERENA_QUALIFICATION_PRODUCT and its exact MCPHUB_SERENA_QUALIFICATION_PRODUCT_SHA256")
	}
	if _, err := hex.DecodeString(productSHA); err != nil {
		t.Fatalf("SERENA_QUALIFICATION_PRODUCT_REQUIRED: invalid Product SHA-256: %v", err)
	}
	if _, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{
		Path: productPath, Role: binaryadmission.WindowsArtifactRoleProduct, SHA256: productSHA,
	}); err != nil {
		t.Fatalf("admit Serena qualification runtime Product: %v", err)
	}

	stateDir, manifestDir = migrateSerenaTestEnv(t)
	policy := api.DefaultSerenaPreparationPolicy()
	deadline := time.Now().Add(policy.WorkLimit + policy.CleanupLimit + time.Minute)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Before(deadline) {
		deadline = testDeadline
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	scope, err := api.OpenSerenaQualificationNamespace(ctx, stateDir)
	if err != nil {
		cancel()
		t.Fatalf("open private Serena qualification namespace: %v", err)
	}
	restoreProduct := api.SetTestCanonicalMcphubPath(productPath)
	t.Cleanup(func() {
		// The observed namespace closes while its context and the ordinary
		// state-root binding are still live. Earlier fixture cleanups restore
		// the state/environment only after this cleanup has returned.
		closeErr := scope.Close()
		restoreProduct()
		cancel()
		if closeErr != nil {
			t.Errorf("close private Serena qualification namespace: %v", closeErr)
		}
	})
	return stateDir, manifestDir
}

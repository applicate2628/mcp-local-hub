//go:build windows && serena_qualification && test_state_path_env

package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

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

	runRoot := os.Getenv("MCPHUB_SERENA_QUALIFICATION_RUN_ROOT")
	volume := filepath.VolumeName(runRoot)
	if !filepath.IsAbs(runRoot) || filepath.Clean(runRoot) != runRoot || len(volume) != 2 || volume[1] != ':' || runRoot == volume+string(filepath.Separator) {
		t.Fatal("SERENA_QUALIFICATION_RUN_ROOT_REQUIRED: set a canonical absolute private run root")
	}
	// The runner must first provision and verify a private, non-reparse run root.
	// Open below retains and rechecks its identity before any package child starts.
	stateDir, err := os.MkdirTemp(runRoot, "s-")
	if err != nil {
		if pathErr, ok := err.(*os.PathError); ok {
			err = pathErr.Err
		}
		t.Fatalf("create retained Serena qualification case: %v", err)
	}
	// The outer qualification runner owns retirement after process settlement.
	// Keep this case outside t.TempDir so a failure retains its exact native path.
	t.Logf("Serena qualification case retained for runner review: %s", filepath.Base(stateDir))
	if err := validateSerenaQualificationWorkingDirectoryBudget(stateDir); err != nil {
		t.Fatal(err)
	}
	stateDir, manifestDir = migrateSerenaTestEnv(t, migrateSerenaTestEnvOptions{stateDir: stateDir})
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

// Include the longest Hub-selected child CWD, the trailing separator Windows
// may append, and its terminating NUL in the CreateProcessW MAX_PATH budget.
func validateSerenaQualificationWorkingDirectoryBudget(stateDir string) error {
	packageSHA := strings.Repeat("0", 64)
	qualificationID := "qualification-" + strings.Repeat("0", 64)
	qualificationDir := filepath.Join(stateDir, packageSHA, "qualifications", qualificationID)
	units := len(utf16.Encode([]rune(qualificationDir))) + 2
	if units > 260 {
		return fmt.Errorf("SERENA_QUALIFICATION_CWD_TOO_LONG: longest child working directory needs %d UTF-16 code units including trailing separator and NUL; use a shorter qualification run root (limit 260)", units)
	}
	return nil
}

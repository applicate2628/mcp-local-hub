//go:build windows && serena_qualification && test_state_path_env

package api

import (
	"testing"

	"mcp-local-hub/internal/binaryadmission"
)

func TestSerenaPublisherRolePolicyQualification(t *testing.T) {
	if !serenaPublisherRoleAllowed(binaryadmission.WindowsArtifactRoleSerenaQualification) {
		t.Fatal("dedicated qualification role was refused in test runtime")
	}
	for _, role := range []binaryadmission.WindowsArtifactRole{
		"", binaryadmission.WindowsArtifactRoleCLI,
		binaryadmission.WindowsArtifactRoleWindowless,
		binaryadmission.WindowsArtifactRoleProduct,
	} {
		if serenaPublisherRoleAllowed(role) {
			t.Fatalf("role %q acquired qualification publisher authority", role)
		}
	}
}

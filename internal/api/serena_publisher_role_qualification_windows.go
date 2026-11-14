//go:build windows && serena_qualification && test_state_path_env

package api

import (
	"testing"

	"mcp-local-hub/internal/binaryadmission"
)

// This role qualifies only the dedicated integration test publisher. Ordinary
// product admission and the RuntimeSpec executable remain separate.
func serenaPublisherRoleAllowed(role binaryadmission.WindowsArtifactRole) bool {
	return testing.Testing() && role == binaryadmission.WindowsArtifactRoleSerenaQualification
}

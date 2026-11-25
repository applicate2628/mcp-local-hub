//go:build !windows || !serena_qualification || !test_state_path_env

package api

import "mcp-local-hub/internal/binaryadmission"

func serenaPublisherRoleAllowed(role binaryadmission.WindowsArtifactRole) bool {
	return role == binaryadmission.WindowsArtifactRoleProduct
}

//go:build !windows

package cli

import (
	"context"
	"errors"
	"fmt"

	"mcp-local-hub/internal/api"
)

func ensureSupervisorControlCompatibility(ctx context.Context) error {
	_, err := api.ProbeSupervisorControlCapabilities(ctx)
	if err == nil || errors.Is(err, api.ErrSupervisorIPCUnavailable) {
		return nil
	}
	if errors.Is(err, api.ErrSupervisorCapabilityUnsupported) {
		return fmt.Errorf("legacy supervisor control protocol is unsupported on this platform; refusing Stop/Restart before intent mutation")
	}
	return fmt.Errorf("probe authenticated supervisor control capabilities before mutation: %w", err)
}

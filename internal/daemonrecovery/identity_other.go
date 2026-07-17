//go:build !windows

package daemonrecovery

import (
	"context"

	"mcp-local-hub/internal/process"
)

func productionIdentityLookup() func(context.Context, int) (process.ProcessIdentity, error) {
	return nil
}

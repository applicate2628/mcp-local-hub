//go:build windows

package daemonrecovery

import (
	"context"
	"time"

	"mcp-local-hub/internal/process"
)

const identityLookupDeadline = 12 * time.Second

func productionIdentityLookup() func(context.Context, int) (process.ProcessIdentity, error) {
	return func(ctx context.Context, pid int) (process.ProcessIdentity, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, cancel := context.WithTimeout(ctx, identityLookupDeadline)
		defer cancel()
		return process.LookupProcessIdentityContext(ctx, pid)
	}
}

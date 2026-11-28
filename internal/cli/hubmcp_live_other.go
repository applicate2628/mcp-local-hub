//go:build !windows

package cli

import (
	"context"

	"mcp-local-hub/internal/api"
)

func queryInstalledAggregateStatus(context.Context, windowsCanonicalEntryCommandInput) (api.RouteAggregateStatus, error) {
	return api.RouteAggregateStatus{}, api.ErrSupervisorControlPeerUnsupported
}

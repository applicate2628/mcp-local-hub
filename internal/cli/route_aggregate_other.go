//go:build !windows

package cli

import "mcp-local-hub/internal/api"

func newRouteAggregateControl(api.RouteAggregateLaunch, windowsCanonicalEntryCommandInput) api.RouteAggregateControl {
	return api.RouteAggregateControl{Status: func() api.RouteAggregateStatus {
		return api.RouteAggregateStatus{Ownership: api.RouteAggregateNotOwner}
	}}
}

func productUsesSupervisedAggregate() bool { return false }

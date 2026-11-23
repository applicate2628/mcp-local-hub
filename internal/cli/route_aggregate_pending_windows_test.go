package cli

import (
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/mcproute"
)

func TestRouteAggregateApplyPreservesPendingListener(t *testing.T) {
	pending := &mcproute.HubListener{}
	owner := &routeAggregateOwner{listener: pending}
	previous := owner.set("error")
	challenge := api.RouteAggregateStatusChallenge{Apply: &api.RouteAggregateApply{}}
	received := owner.apply(t.Context(), t.Context(), challenge)
	if received != previous || owner.listener != pending || owner.current() != previous {
		t.Fatalf("pending cleanup lost attachment custody: before=%+v after=%+v", previous, received)
	}
}

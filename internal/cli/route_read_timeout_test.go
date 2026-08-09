package cli

import (
	"net/http"
	"testing"
)

func TestRouteHTTPServerBoundsRequestBodyRead(t *testing.T) {
	srv := newRouteHTTPServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if srv.ReadTimeout != routeRequestReadTimeout || srv.ReadTimeout <= 0 {
		t.Fatalf("route ReadTimeout = %v, want positive owner value %v", srv.ReadTimeout, routeRequestReadTimeout)
	}
}

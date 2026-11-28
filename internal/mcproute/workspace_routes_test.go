package mcproute

import (
	"net/http"
	"time"
)

func newTestWorkspaceRoutes(port int) *WorkspaceRoutes {
	return NewWorkspaceRoutes(func() int { return port }, time.Now())
}

// The router fixtures send synthetic requests directly to the mux. Listener
// guard behavior is exercised at RouteHandler and through the GUI adapter.
func workspaceTestMux(s *WorkspaceRoutes) *http.ServeMux {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

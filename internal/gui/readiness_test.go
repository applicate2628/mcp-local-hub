package gui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

func sameOriginGet(s *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	return rr
}

func TestReadinessHandler_ByName_EmbeddedServer(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	rr := sameOriginGet(s, "/api/server/readiness?server=memory")
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var rep api.ReadinessReport
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Server != "memory" {
		t.Errorf("Server = %q, want memory", rep.Server)
	}
	if len(rep.Requirements) == 0 {
		t.Errorf("no requirements in report for memory")
	}
}

func TestReadinessHandler_AllServers(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	rr := sameOriginGet(s, "/api/server/readiness")
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var reps []api.ReadinessReport
	if err := json.Unmarshal(rr.Body.Bytes(), &reps); err != nil {
		t.Fatal(err)
	}
	if len(reps) < 5 {
		t.Errorf("got %d reports, want >= 5 embedded servers", len(reps))
	}
}

func TestReadinessHandler_UnknownServer404(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	rr := sameOriginGet(s, "/api/server/readiness?server=no-such-zzz")
	if rr.Code != 404 {
		t.Fatalf("got %d, want 404: %s", rr.Code, rr.Body.String())
	}
}

func TestReadinessHandler_RejectsCrossOrigin(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	req := httptest.NewRequest("GET", "/api/server/readiness?server=memory", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code == 200 {
		t.Errorf("cross-origin request accepted (code %d); requireSameOrigin should reject", rr.Code)
	}
}

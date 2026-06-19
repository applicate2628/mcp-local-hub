package gui

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
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

func sameOriginPostJSON(s *Server, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	return rr
}

func TestReadinessHandler_DraftPOST(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	// A complete stdio-bridge DRAFT manifest the Add-server screen would compose
	// before saving (command `go` is on PATH in every test env).
	yaml := "name: draftdemo\nkind: global\ntransport: stdio-bridge\ncommand: go\n" +
		"daemons:\n  - name: default\n    port: 9321\n" +
		"client_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n"
	body, _ := json.Marshal(map[string]string{"yaml": yaml})
	rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var rep api.ReadinessReport
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Server != "draftdemo" {
		t.Errorf("Server = %q, want draftdemo", rep.Server)
	}
	if len(rep.Requirements) == 0 {
		t.Error("draft readiness returned no requirements")
	}
}

func TestReadinessHandler_DraftPOST_ReservedNameBlocks(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	// "con" is a Windows-reserved device name the frontend regex still allows but
	// CheckManifestName (the Save & Install storage gate) rejects. Draft readiness
	// must surface it as a hard blocker rather than show "Ready to install" and
	// then fail the create before install starts (Codex #378 r4).
	yaml := "name: con\nkind: global\ntransport: stdio-bridge\ncommand: go\n" +
		"daemons:\n  - name: default\n    port: 9322\n" +
		"client_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n"
	body, _ := json.Marshal(map[string]string{"yaml": yaml})
	rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var rep api.ReadinessReport
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Ready {
		t.Error("reserved name 'con' reported Ready=true; the storage-name gate should block it")
	}
	found := false
	for _, r := range rep.Requirements {
		if r.Name == "server name" && !r.OK {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'server name' blocker in report for reserved name; requirements=%+v", rep.Requirements)
	}
}

func TestReadinessHandler_DraftPOST_DoubleUnderscoreNameBlocks(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	// A `__` substring passes config.ParseManifest AND CheckManifestName but is
	// rejected by the strict-mode gate Save & Install runs (ManifestValidateMode
	// Strict). Draft readiness must mirror that gate so it does not show "Ready to
	// install" then fail the create (Codex #378 r6).
	yaml := "name: foo__bar\nkind: global\ntransport: stdio-bridge\ncommand: go\n" +
		"daemons:\n  - name: default\n    port: 9323\n" +
		"client_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n"
	body, _ := json.Marshal(map[string]string{"yaml": yaml})
	rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var rep api.ReadinessReport
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Ready {
		t.Error("name with '__' reported Ready=true; strict-mode gate should block it")
	}
	found := false
	for _, r := range rep.Requirements {
		if r.Name == "server name" && !r.OK {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'server name' blocker for a '__' name; requirements=%+v", rep.Requirements)
	}
}

func TestReadinessHandler_DraftPOST_Unparseable400(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	body, _ := json.Marshal(map[string]string{"yaml": ":::not a manifest:::"})
	rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
	if rr.Code != 400 {
		t.Fatalf("got %d, want 400 for an unparseable draft: %s", rr.Code, rr.Body.String())
	}
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

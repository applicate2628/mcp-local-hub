package gui

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeManifestPresence map[string]bool

func (f fakeManifestPresence) ManifestExists(name string) (bool, error) {
	return f[name], nil
}

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

func TestReadinessHandler_DraftPOST_BlockingValidationWarningsBlock(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	// Save/create rejects this storage-blocking warning through
	// validateManifestForStorageName/manifestBlockingWarnings even though
	// ManifestValidateMode(Strict) returns a nil error.
	yaml := "name: nodaemons\nkind: global\ntransport: stdio-bridge\ncommand: go\n"
	body, _ := json.Marshal(map[string]string{"yaml": yaml, "mode": "create"})
	rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var rep api.ReadinessReport
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Ready {
		t.Error("draft with storage-blocking warning reported Ready=true; Save/create would reject it")
	}
	found := false
	for _, r := range rep.Requirements {
		if !r.OK && strings.Contains(r.Reason, "no daemons declared") {
			found = true
		}
	}
	if !found {
		t.Errorf("no storage-blocking validation warning in report; requirements=%+v", rep.Requirements)
	}
}

func TestReadinessHandler_DraftPOST_CreateModeExistingManifestBlocks(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	s.manifestPresence = fakeManifestPresence{"saved": true}
	yaml := "name: saved\nkind: global\ntransport: stdio-bridge\ncommand: go\n" +
		"daemons:\n  - name: default\n    port: 9324\n" +
		"client_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n"
	body, _ := json.Marshal(map[string]string{"yaml": yaml, "mode": "create"})
	rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var rep api.ReadinessReport
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Ready {
		t.Error("fresh create draft for an existing manifest reported Ready=true; create would reject it")
	}
	found := false
	for _, r := range rep.Requirements {
		if r.Name == "manifest exists" && !r.OK && strings.Contains(r.Reason, `manifest "saved" already exists`) {
			found = true
		}
	}
	if !found {
		t.Errorf("no existing-manifest blocker in report; requirements=%+v", rep.Requirements)
	}
}

// Test 6 (decision 2026-07-03) — the Save & Install dry-run mirrors the embed
// collision refusal: a create-mode draft under a shipped (built-in) server name
// reports Ready=false with a "shipped server name" blocker instead of showing
// "Ready to install" and then failing the create at ManifestCreateIn.
func TestReadinessHandler_DraftPOST_EmbeddedShippedNameBlocks(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	// "wolfram" is a shipped/embedded server. The create write gate refuses a
	// disk manifest under it (embed reads win); readiness must mirror that.
	yaml := "name: wolfram\nkind: global\ntransport: stdio-bridge\ncommand: go\n" +
		"daemons:\n  - name: default\n    port: 9420\n" +
		"client_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n"
	body, _ := json.Marshal(map[string]string{"yaml": yaml, "mode": "create"})
	rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var rep api.ReadinessReport
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Ready {
		t.Error("shipped-server name 'wolfram' reported Ready=true; the embed-collision gate should block it")
	}
	found := false
	for _, r := range rep.Requirements {
		if r.Name == "shipped server name" && !r.OK {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'shipped server name' blocker in report; requirements=%+v", rep.Requirements)
	}
}

func TestReadinessHandler_DraftPOST_EditTargetExistingManifestDoesNotBlock(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	s.manifestPresence = fakeManifestPresence{"saved": true}
	yaml := "name: saved\nkind: global\ntransport: stdio-bridge\ncommand: go\n" +
		"daemons:\n  - name: default\n    port: 9325\n" +
		"client_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n"
	body, _ := json.Marshal(map[string]string{"yaml": yaml, "mode": "edit", "edit_name": "saved"})
	rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var rep api.ReadinessReport
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Requirements {
		if r.Name == "manifest exists" && !r.OK {
			t.Fatalf("edit target existence should not block readiness; requirements=%+v", rep.Requirements)
		}
	}
}

func TestReadinessHandler_DraftPOST_EditTargetMissingManifestBlocks(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	s.manifestPresence = fakeManifestPresence{}
	yaml := "name: saved\nkind: global\ntransport: stdio-bridge\ncommand: go\n" +
		"daemons:\n  - name: default\n    port: 9326\n" +
		"client_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n"
	body, _ := json.Marshal(map[string]string{"yaml": yaml, "mode": "edit", "edit_name": "saved"})
	rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var rep api.ReadinessReport
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Ready {
		t.Error("edit draft for a missing manifest reported Ready=true; edit would reject it")
	}
	found := false
	for _, r := range rep.Requirements {
		if r.Name == "manifest missing" && !r.OK && !r.Optional && strings.Contains(r.Reason, `manifest "saved" no longer exists`) {
			found = true
		}
	}
	if !found {
		t.Errorf("no missing-manifest blocker in report; requirements=%+v", rep.Requirements)
	}
}

func TestReadinessHandler_DraftPOST_MirrorsManifestWriteGate(t *testing.T) {
	tmpState := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmpState)
	t.Setenv("XDG_DATA_HOME", tmpState)

	stubMcphub := filepath.Join(t.TempDir(), api.MCPHubBinaryName())
	if err := os.WriteFile(stubMcphub, []byte("stub"), 0755); err != nil {
		t.Fatalf("write mcphub stub: %v", err)
	}
	t.Cleanup(api.SetTestCanonicalMcphubPath(stubMcphub))

	validYAML := func(name string) string {
		return fmt.Sprintf(
			"name: %s\nkind: global\ntransport: stdio-bridge\ncommand: go\ndaemons:\n  - name: default\n    port: %d\n",
			name,
			freeTCPPort(t),
		)
	}

	cases := []struct {
		name      string
		mode      string
		server    string
		yaml      string
		present   bool
		editName  string
		writeFunc func(a *api.API, dir, server, yaml string) error
	}{
		{
			name:    "create valid manifest",
			mode:    "create",
			server:  "draftok",
			yaml:    validYAML("draftok"),
			present: false,
			writeFunc: func(a *api.API, dir, server, yaml string) error {
				return a.ManifestCreateIn(dir, server, yaml)
			},
		},
		{
			name:    "create rejects existing manifest",
			mode:    "create",
			server:  "saved",
			yaml:    validYAML("saved"),
			present: true,
			writeFunc: func(a *api.API, dir, server, yaml string) error {
				return a.ManifestCreateIn(dir, server, yaml)
			},
		},
		{
			name:    "create rejects reserved server name",
			mode:    "create",
			server:  "con",
			yaml:    validYAML("con"),
			present: false,
			writeFunc: func(a *api.API, dir, server, yaml string) error {
				return a.ManifestCreateIn(dir, server, yaml)
			},
		},
		{
			name:    "create rejects storage-blocking validation warning",
			mode:    "create",
			server:  "nodaemons",
			yaml:    "name: nodaemons\nkind: global\ntransport: stdio-bridge\ncommand: go\n",
			present: false,
			writeFunc: func(a *api.API, dir, server, yaml string) error {
				return a.ManifestCreateIn(dir, server, yaml)
			},
		},
		{
			name:     "edit valid manifest",
			mode:     "edit",
			server:   "editok",
			yaml:     validYAML("editok"),
			present:  true,
			editName: "editok",
			writeFunc: func(a *api.API, dir, server, yaml string) error {
				_, err := a.ManifestEditInWithHash(dir, server, yaml, "")
				return err
			},
		},
		{
			name:     "edit rejects missing target",
			mode:     "edit",
			server:   "gone",
			yaml:     validYAML("gone"),
			present:  false,
			editName: "gone",
			writeFunc: func(a *api.API, dir, server, yaml string) error {
				_, err := a.ManifestEditInWithHash(dir, server, yaml, "")
				return err
			},
		},
		{
			name:     "edit rejects strict-mode name",
			mode:     "edit",
			server:   "foo__bar",
			yaml:     validYAML("foo__bar"),
			present:  true,
			editName: "foo__bar",
			writeFunc: func(a *api.API, dir, server, yaml string) error {
				_, err := a.ManifestEditInWithHash(dir, server, yaml, "")
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.present {
				targetDir := filepath.Join(dir, tc.server)
				if err := os.MkdirAll(targetDir, 0755); err != nil {
					t.Fatalf("seed manifest dir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(targetDir, "manifest.yaml"), []byte(validYAML(tc.server)), 0644); err != nil {
					t.Fatalf("seed manifest: %v", err)
				}
			}
			a := api.NewAPI()
			writeOK := tc.writeFunc(a, dir, tc.server, tc.yaml) == nil

			s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
			presence := fakeManifestPresence{}
			if tc.present {
				presence[tc.server] = true
			}
			s.manifestPresence = presence
			body, _ := json.Marshal(map[string]string{"yaml": tc.yaml, "mode": tc.mode, "edit_name": tc.editName})
			rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
			if rr.Code != 200 {
				t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
			}
			var rep api.ReadinessReport
			if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
				t.Fatal(err)
			}
			if rep.Ready != writeOK {
				t.Fatalf("readiness Ready=%v, write gate would-succeed=%v; requirements=%+v", rep.Ready, writeOK, rep.Requirements)
			}
		})
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate TCP port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestReadinessHandler_DraftPOST_Unparseable400(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	body, _ := json.Marshal(map[string]string{"yaml": ":::not a manifest:::"})
	rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
	if rr.Code != 400 {
		t.Fatalf("got %d, want 400 for an unparseable draft: %s", rr.Code, rr.Body.String())
	}
}

func TestReadinessHandler_DraftPOST_TrailingGarbageRejected(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	yaml := "name: drafttrail\nkind: global\ntransport: stdio-bridge\ncommand: go\n" +
		"daemons:\n  - name: default\n    port: 9327\n" +
		"client_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n"
	body, err := json.Marshal(map[string]string{"yaml": yaml})
	if err != nil {
		t.Fatalf("marshal readiness request: %v", err)
	}
	body = append(body, []byte(strings.Repeat("Z", int(maxManifestBodyBytes)+1))...)
	rr := sameOriginPostJSON(s, "/api/server/readiness", string(body))
	if rr.Code != http.StatusBadRequest && rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 400 or 413 for trailing garbage; body=%q", rr.Code, rr.Body.String())
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

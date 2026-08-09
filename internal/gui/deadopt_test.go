package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

const guiDeAdoptTestPort = 9330 // inert URL data only; these tests never bind a TCP port

func setupGUIDeAdoptAdopted(t *testing.T, name, nativeConfig string) string {
	t.Helper()
	codexPath, _, _ := setupGUIAdoptTestEnv(t, name, nativeConfig)
	rec := postAdoptTest(t, "/api/adopt", fmt.Sprintf(
		`{"entry":%q,"client":"codex-cli","port":%d}`, name, guiDeAdoptTestPort,
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed adopt status=%d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	return codexPath
}

func restoreGUIDeAdoptNativeConfig(t *testing.T, codexPath, nativeConfig string) {
	t.Helper()
	if err := os.WriteFile(codexPath, []byte(nativeConfig), 0o600); err != nil {
		t.Fatalf("restore native codex config: %v", err)
	}
}

func seedGUIDeAdoptGateOn(t *testing.T) {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), ".claude.json")
	body := `{"mcpServers":{"mcphub-hub":{"url":"http://127.0.0.1:3439/clients/claude-code/mcp","type":"http"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed gate-ON aggregate: %v", err)
	}
}

func requestGUIDeAdopt(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := newEphemeralServer(t, Config{})
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeGUIDeAdoptJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v\n%s", err, rec.Body.String())
	}
	return body
}

func TestDeAdoptPlanRouteRoundTripsAdoptedPlan(t *testing.T) {
	name := "gui-deadopt-plan"
	native := fmt.Sprintf("[mcp_servers.%s]\ncommand = \"go\"\nargs = [\"version\"]\n", name)
	codexPath := setupGUIDeAdoptAdopted(t, name, native)
	restoreGUIDeAdoptNativeConfig(t, codexPath, native)

	rec := requestGUIDeAdopt(t, http.MethodPost, "/api/deadopt/plan", fmt.Sprintf(`{"server":%q}`, name))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeGUIDeAdoptJSON(t, rec)
	if body["ManifestName"] != name || body["Routing"] != string("FRESH") {
		t.Fatalf("plan identity/routing=%#v/%#v, want %q/FRESH", body["ManifestName"], body["Routing"], name)
	}
	eligibility, _ := body["Eligibility"].(map[string]any)
	if eligibility["AdoptOwned"] != true || eligibility["GateOn"] != false || eligibility["Eligible"] != true {
		t.Fatalf("Eligibility=%#v, want adopted + gate-OFF + eligible", eligibility)
	}
}

func TestDeAdoptRoutesRequireSameOrigin(t *testing.T) {
	name := "gui-deadopt-origin"
	native := fmt.Sprintf("[mcp_servers.%s]\ncommand = \"go\"\nargs = [\"version\"]\n", name)
	setupGUIDeAdoptAdopted(t, name, native)

	tests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/deadopt/plan", fmt.Sprintf(`{"server":%q}`, name)},
		{http.MethodPost, "/api/deadopt", fmt.Sprintf(`{"server":%q}`, name)},
		{http.MethodGet, "/api/deadopt/eligible?server=" + url.QueryEscape(name), ""},
		{http.MethodGet, "/api/deadopt/recoverable", ""},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			s := newEphemeralServer(t, Config{})
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
			}
			if body := decodeGUIDeAdoptJSON(t, rec); body["code"] != "CROSS_ORIGIN" {
				t.Fatalf("code=%#v, want CROSS_ORIGIN", body["code"])
			}
		})
	}
}

func TestDeAdoptRecoverableRouteListsOnlyDeAdoptingNames(t *testing.T) {
	name := "gui-deadopt-recoverable"
	native := fmt.Sprintf("[mcp_servers.%s]\ncommand = \"go\"\nargs = [\"version\"]\n", name)
	setupGUIDeAdoptAdopted(t, name, native)
	if err := api.MarkAdoptProvenanceDeAdopting(name); err != nil {
		t.Fatalf("mark de-adopting: %v", err)
	}

	rec := requestGUIDeAdopt(t, http.MethodGet, "/api/deadopt/recoverable", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var names []string
	if err := json.Unmarshal(rec.Body.Bytes(), &names); err != nil {
		t.Fatalf("decode names: %v; body=%s", err, rec.Body.String())
	}
	if len(names) != 1 || names[0] != name {
		t.Fatalf("names=%#v, want [%q]", names, name)
	}
}

func TestDeAdoptPlanRouteNeverSerializesExecutionState(t *testing.T) {
	name := "gui-deadopt-wire"
	const secret = "literal-deadopt-snapshot-secret"
	native := fmt.Sprintf("[mcp_servers.%s]\ncommand = \"go\"\nargs = [%q]\n", name, secret)
	codexPath := setupGUIDeAdoptAdopted(t, name, native)
	restoreGUIDeAdoptNativeConfig(t, codexPath, native)

	rec := requestGUIDeAdopt(t, http.MethodPost, "/api/deadopt/plan", fmt.Sprintf(`{"server":%q}`, name))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	for _, forbidden := range []string{secret, "snapshotBytes", "snapshot_bytes", "provenance", "SnapshotRef", "snapshot_ref"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("plan JSON leaked wire-unsafe field/value %q:\n%s", forbidden, raw)
		}
	}
	for _, expected := range []string{name, "FRESH", "restore-done", "Eligibility"} {
		if !strings.Contains(raw, expected) {
			t.Fatalf("plan JSON omitted expected redaction-safe value %q:\n%s", expected, raw)
		}
	}
}

func TestDeAdoptEligibleRouteUsesPlannerEligibility(t *testing.T) {
	t.Run("adopted and gate off", func(t *testing.T) {
		name := "gui-deadopt-eligible-off"
		native := fmt.Sprintf("[mcp_servers.%s]\ncommand = \"go\"\nargs = [\"version\"]\n", name)
		codexPath := setupGUIDeAdoptAdopted(t, name, native)
		restoreGUIDeAdoptNativeConfig(t, codexPath, native)

		rec := requestGUIDeAdopt(t, http.MethodGet, "/api/deadopt/eligible?server="+url.QueryEscape(name), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := decodeGUIDeAdoptJSON(t, rec)
		if body["eligible"] != true || body["adopt_owned"] != true || body["gate_on"] != false {
			t.Fatalf("eligibility=%#v, want eligible adopted gate-OFF", body)
		}
	})

	t.Run("adopted and gate on", func(t *testing.T) {
		name := "gui-deadopt-eligible-on"
		native := fmt.Sprintf("[mcp_servers.%s]\ncommand = \"go\"\nargs = [\"version\"]\n", name)
		setupGUIDeAdoptAdopted(t, name, native)
		seedGUIDeAdoptGateOn(t)

		rec := requestGUIDeAdopt(t, http.MethodGet, "/api/deadopt/eligible?server="+url.QueryEscape(name), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := decodeGUIDeAdoptJSON(t, rec)
		if body["eligible"] != false || body["adopt_owned"] != true || body["gate_on"] != true {
			t.Fatalf("eligibility=%#v, want ineligible adopted gate-ON", body)
		}
		clients, _ := body["gate_on_clients"].([]any)
		if len(clients) == 0 {
			t.Fatalf("gate_on_clients=%#v, want at least one gated client", body["gate_on_clients"])
		}
	})

	t.Run("unknown server", func(t *testing.T) {
		setupGUIAdoptTestEnv(t, "gui-deadopt-unknown", "[profile.default]\nmodel = \"gpt-5\"\n")
		rec := requestGUIDeAdopt(t, http.MethodGet, "/api/deadopt/eligible?server=gui-deadopt-unknown", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := decodeGUIDeAdoptJSON(t, rec)
		if body["eligible"] != false || body["adopt_owned"] != false || body["gate_on"] != false {
			t.Fatalf("eligibility=%#v, want ineligible unowned gate-OFF", body)
		}
	})
}

func TestDeAdoptRouteReturnsReportAndRedactsRefusal(t *testing.T) {
	t.Run("execute", func(t *testing.T) {
		name := "gui-deadopt-execute"
		native := fmt.Sprintf("[mcp_servers.%s]\ncommand = \"go\"\nargs = [\"version\"]\n", name)
		codexPath := setupGUIDeAdoptAdopted(t, name, native)
		restoreGUIDeAdoptNativeConfig(t, codexPath, native)

		s := newEphemeralServer(t, Config{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		events := s.Broadcaster().Subscribe(ctx)
		req := httptest.NewRequest(http.MethodPost, "/api/deadopt", strings.NewReader(fmt.Sprintf(`{"server":%q,"accept_conflict_clients":[]}`, name)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := decodeGUIDeAdoptJSON(t, rec)
		restored, _ := body["restored"].([]any)
		failed, _ := body["failed"].([]any)
		if len(restored) != 1 || restored[0] != "codex-cli" || len(failed) != 0 {
			t.Fatalf("report=%#v, want restored=[codex-cli], failed=[]", body)
		}
		select {
		case event := <-events:
			if event.Type != "operator-action" || event.Body["action"] != "deadopt" {
				t.Fatalf("audit event=%#v, want deadopt operator-action", event)
			}
			if event.Body["server"] != name || event.Body["failed_count"] != 0 {
				t.Fatalf("audit body=%#v, want server %q and failed_count 0", event.Body, name)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no operator-action event published after de-adopt")
		}
	})

	t.Run("redacted refusal", func(t *testing.T) {
		name := "gui-deadopt-refuse"
		native := fmt.Sprintf("[mcp_servers.%s]\ncommand = \"go\"\nargs = [\"version\"]\n", name)
		setupGUIDeAdoptAdopted(t, name, native)
		seedGUIDeAdoptGateOn(t)

		rec := requestGUIDeAdopt(t, http.MethodPost, "/api/deadopt", fmt.Sprintf(`{"server":%q}`, name))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		body := decodeGUIDeAdoptJSON(t, rec)
		if body["code"] != "DEADOPT_FAILED" || body["error"] != "internal error" {
			t.Fatalf("error envelope=%#v, want redacted DEADOPT_FAILED", body)
		}
		if strings.Contains(rec.Body.String(), name) || strings.Contains(rec.Body.String(), "gate") {
			t.Fatalf("refusal leaked backend details: %s", rec.Body.String())
		}
	})
}

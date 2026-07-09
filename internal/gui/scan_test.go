// internal/gui/scan_test.go
package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestWriteAPIErrorRedacted_HidesRawPathKeepsCode pins the G16 P2 contract
// for the central redaction helper: the response body must NOT echo the
// raw err.Error() (which can embed the operator's absolute home path —
// C:\Users\<name>\... — revealing the AD username on corp hosts), but MUST
// still carry the stable, code-keyed signal the frontend switches on.
func TestWriteAPIErrorRedacted_HidesRawPathKeepsCode(t *testing.T) {
	leaky := errors.New("open C:\\Users\\alice\\AppData\\Local\\mcp-local-hub\\supervisor-intent.json: permission denied")
	rec := httptest.NewRecorder()
	writeAPIErrorRedacted(rec, leaky, http.StatusInternalServerError, "STATE_READ_FAILED", "/api/test")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "C:\\Users\\alice") || strings.Contains(body, "permission denied") {
		t.Errorf("response body leaks filesystem path or raw error: %q", body)
	}
	var out struct{ Error, Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Error != "internal error" {
		t.Errorf("error=%q, want generic \"internal error\"", out.Error)
	}
	if out.Code != "STATE_READ_FAILED" {
		t.Errorf("code=%q, want STATE_READ_FAILED (stable code preserved)", out.Code)
	}
}

type fakeScanner struct {
	result *api.ScanResult
	err    error
}

func (f fakeScanner) Scan() (*api.ScanResult, error) { return f.result, f.err }

func TestScan_ReturnsJSONWrappingAPIResult(t *testing.T) {
	r := &api.ScanResult{}
	s := NewServer(Config{})
	s.scanner = fakeScanner{result: r}
	req := httptest.NewRequest(http.MethodGet, "/api/scan", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// At minimum, scan result should serialize to a JSON object.
	// Exact keys depend on api.ScanResult shape; assert that we got
	// SOMETHING (not "null").
	if out == nil {
		t.Errorf("response decoded to nil map")
	}
}

func TestScan_BlocksCrossOrigin(t *testing.T) {
	s := NewServer(Config{})
	s.scanner = fakeScanner{result: &api.ScanResult{}}
	req := httptest.NewRequest(http.MethodGet, "/api/scan", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestScan_AllowsSameOrigin(t *testing.T) {
	s := NewServer(Config{})
	s.port.Store(7777)
	s.scanner = fakeScanner{result: &api.ScanResult{}}
	req := httptest.NewRequest(http.MethodGet, "/api/scan", nil)
	req.Header.Set("Origin", "http://127.0.0.1:7777")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestScan_RedactsRawClientConfig(t *testing.T) {
	s := NewServer(Config{})
	s.scanner = fakeScanner{result: &api.ScanResult{Entries: []api.ScanEntry{{
		Name: "demo",
		ClientPresence: map[string]api.ClientEntry{
			"claude-code": {
				Transport: "stdio",
				Endpoint:  "node",
				Raw: map[string]any{
					"env": map[string]any{"OPENAI_API_KEY": "sk-secret"},
				},
			},
		},
	}}}}
	req := httptest.NewRequest(http.MethodGet, "/api/scan", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var out api.ScanResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := out.Entries[0].ClientPresence["claude-code"].Raw; got != nil {
		t.Fatalf("raw should be redacted, got: %#v", got)
	}
}

// TestSanitizeScanResult_StripsLegacyConflictRaw is the finding-2 guard: the
// Raw config blob on a LegacyConflict entry (the stdio donor row
// classifyLSPEntries moves into a hub row) must be nil'd on the wire, not just
// the ClientPresence Raw. Before the fix, sanitizeScanResult cleared only
// ClientPresence, so the LegacyConflict entry's raw config leaked.
func TestSanitizeScanResult_StripsLegacyConflictRaw(t *testing.T) {
	in := &api.ScanResult{Entries: []api.ScanEntry{{
		Name: "mcp-language-server",
		ClientPresence: map[string]api.ClientEntry{
			"codex-cli": {
				Transport: "http",
				Endpoint:  "http://127.0.0.1:9121/lsp/go/mcp",
				Raw:       map[string]any{"url": "http://127.0.0.1:9121/lsp/go/mcp"},
			},
		},
		LegacyConflict: map[string]api.ClientEntry{
			"codex-cli": {
				Transport: "stdio",
				Endpoint:  "mcp-language-server",
				Disabled:  true,
				Raw: map[string]any{
					"command": "mcp-language-server",
					"env":     map[string]any{"SECRET_TOKEN": "leak-me"},
				},
			},
		},
	}}}

	out := sanitizeScanResult(in)
	if out == nil || len(out.Entries) != 1 {
		t.Fatalf("sanitizeScanResult returned unexpected shape: %#v", out)
	}
	if got := out.Entries[0].ClientPresence["codex-cli"].Raw; got != nil {
		t.Errorf("ClientPresence Raw should be stripped, got: %#v", got)
	}
	if got := out.Entries[0].LegacyConflict["codex-cli"].Raw; got != nil {
		t.Errorf("LegacyConflict Raw should be stripped, got: %#v", got)
	}
	// The non-Raw fields of the LegacyConflict entry survive (only Raw nil'd).
	if out.Entries[0].LegacyConflict["codex-cli"].Transport != "stdio" {
		t.Errorf("LegacyConflict entry transport corrupted: %#v", out.Entries[0].LegacyConflict["codex-cli"])
	}
	if !out.Entries[0].LegacyConflict["codex-cli"].Disabled {
		t.Errorf("LegacyConflict entry disabled flag was stripped: %#v", out.Entries[0].LegacyConflict["codex-cli"])
	}
	// Input must NOT be mutated (sanitize deep-copies).
	if in.Entries[0].LegacyConflict["codex-cli"].Raw == nil {
		t.Errorf("input LegacyConflict Raw was mutated; sanitize must deep-copy")
	}

	// Defense-in-depth: no raw secret survives anywhere in the serialized body.
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "leak-me") {
		t.Fatalf("serialized scan body still contains the raw secret: %s", body)
	}
}

// TestSanitizeScanResult_NilLegacyConflictStaysAbsent pins the omitempty wire
// shape: a nil LegacyConflict (the common no-coexistence case) must remain nil
// after sanitize so the omitempty field stays ABSENT from the JSON rather than
// becoming an empty object.
func TestSanitizeScanResult_NilLegacyConflictStaysAbsent(t *testing.T) {
	in := &api.ScanResult{Entries: []api.ScanEntry{{
		Name: "demo",
		ClientPresence: map[string]api.ClientEntry{
			"claude-code": {Transport: "stdio", Endpoint: "node", Raw: map[string]any{"x": 1}},
		},
		// LegacyConflict deliberately nil.
	}}}
	out := sanitizeScanResult(in)
	if out.Entries[0].LegacyConflict != nil {
		t.Errorf("nil LegacyConflict should stay nil, got: %#v", out.Entries[0].LegacyConflict)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "legacy_conflict") {
		t.Errorf("omitempty broken: serialized body contains legacy_conflict for a nil field: %s", body)
	}
}

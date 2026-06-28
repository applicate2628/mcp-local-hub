package gui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// docsOnlyEntry is a canned full docs-only catalog entry the refusal tests
// resolve. It carries the pointer payload (homepage/readme/summary/manual_install)
// and NO install fields — exactly the shape validateMarketplaceEntry accepts.
func docsOnlyEntry(id string) *api.MarketplaceEntry {
	return &api.MarketplaceEntry{
		ID:            id,
		Name:          "Cubase MCP server (docs-only)",
		Summary:       "Drive Cubase from an AI client.",
		Homepage:      "https://github.com/hedidjs/cubase-mcp",
		ReadmeURL:     "https://raw.githubusercontent.com/hedidjs/cubase-mcp/main/README.md",
		Transport:     "docs-only",
		ManualInstall: "git clone the repo and configure a virtual-MIDI port in Cubase.",
	}
}

// TestMarketplaceInstall_DocsOnly_HubRefused pins the S4 install guard: a
// transport:"docs-only" row is refused with 400 DOCS_ONLY_NOT_INSTALLABLE in HUB
// mode, BEFORE any port pick / name presence / manifest create. The pointer text
// rides the body so the operator/API caller sees the manual-install steps.
func TestMarketplaceInstall_DocsOnly_HubRefused(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: docsOnlyEntry("cubase"), found: true}
	picker := &fakeGlobalPortPicker{}
	presence := &fakeServerNamePresence{}
	creator := &fakeManifestCreator{}
	installer := &fakeInstaller{}
	s := newMarketplaceInstallTestServer(loader, picker, presence, &fakeDirectClientWriter{}, creator, installer)

	rec := postInstall(t, s, `{"id":"cubase","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != "DOCS_ONLY_NOT_INSTALLABLE" {
		t.Errorf("code = %q, want DOCS_ONLY_NOT_INSTALLABLE", body.Code)
	}
	// The pointer text rides the body (so the caller sees where to go).
	if !strings.Contains(body.Error, "DOCS-ONLY pointer") {
		t.Errorf("error body = %q, want it to carry the pointer text", body.Error)
	}
	// NONE of the install seams may have been touched — a docs-only row can never
	// produce a manifest/daemon/client write.
	if picker.called {
		t.Error("port picker must NOT be called for a docs-only row")
	}
	if creator.called {
		t.Error("manifest creator must NOT be called for a docs-only row")
	}
	if installer.called {
		t.Error("installer must NOT be called for a docs-only row")
	}
}

// TestMarketplaceInstall_DocsOnly_DirectRefused pins the same guard for DIRECT
// mode: a docs-only row never reaches the direct client-config writer.
func TestMarketplaceInstall_DocsOnly_DirectRefused(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: docsOnlyEntry("cubase"), found: true}
	writer := &fakeDirectClientWriter{}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, writer, &fakeManifestCreator{}, &fakeInstaller{})

	rec := postInstall(t, s, `{"id":"cubase","mode":"direct","clients":["claude-code"]}`, "same-origin")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != "DOCS_ONLY_NOT_INSTALLABLE" {
		t.Errorf("code = %q, want DOCS_ONLY_NOT_INSTALLABLE", body.Code)
	}
	if writer.called {
		t.Error("direct client writer must NOT be called for a docs-only row")
	}
}

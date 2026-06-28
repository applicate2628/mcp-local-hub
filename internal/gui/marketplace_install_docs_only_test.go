package gui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// docsOnlyTestEntry is a canned api.DocsOnlyEntry the refusal tests resolve via
// LoadDocsOnly. A docs_only row carries the pointer payload and (by type) no
// install fields — exactly the shape validateDocsOnlyEntry accepts.
func docsOnlyTestEntry(id string) *api.DocsOnlyEntry {
	return &api.DocsOnlyEntry{
		ID:            id,
		Name:          "Cubase MCP server (docs-only)",
		Summary:       "Drive Cubase from an AI client.",
		Homepage:      "https://github.com/hedidjs/cubase-mcp",
		ReadmeURL:     "https://raw.githubusercontent.com/hedidjs/cubase-mcp/main/README.md",
		ManualInstall: "git clone the repo and configure a virtual-MIDI port in Cubase.",
	}
}

// TestMarketplaceInstall_DocsOnly_HubRefused pins the S4 install guard for HUB
// mode: a docs_only id is NOT in entries[] (LoadEntry found=false), so the handler
// consults LoadDocsOnly and refuses with 400 DOCS_ONLY_NOT_INSTALLABLE + the pointer
// text — BEFORE any port pick / name presence / manifest create. None of the install
// seams may be touched.
func TestMarketplaceInstall_DocsOnly_HubRefused(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{
		// Not an installable entry...
		entry: nil, found: false,
		// ...but it IS a docs_only pointer.
		docsEntry: docsOnlyTestEntry("cubase"), docsFound: true,
	}
	picker := &fakeGlobalPortPicker{}
	creator := &fakeManifestCreator{}
	installer := &fakeInstaller{}
	s := newMarketplaceInstallTestServer(loader, picker, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, creator, installer)

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
	if !strings.Contains(body.Error, "DOCS-ONLY pointer") {
		t.Errorf("error body = %q, want it to carry the pointer text", body.Error)
	}
	if !loader.docsCalled || loader.docsSeenID != "cubase" {
		t.Errorf("LoadDocsOnly not consulted for the not-found entry id (docsCalled=%v seenID=%q)", loader.docsCalled, loader.docsSeenID)
	}
	if picker.called || creator.called || installer.called {
		t.Errorf("install seams must NOT be touched for a docs_only row (picker=%v creator=%v installer=%v)", picker.called, creator.called, installer.called)
	}
}

// TestMarketplaceInstall_DocsOnly_DirectRefused pins the same guard for DIRECT
// mode: a docs_only id never reaches the direct client-config writer.
func TestMarketplaceInstall_DocsOnly_DirectRefused(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{
		entry: nil, found: false,
		docsEntry: docsOnlyTestEntry("cubase"), docsFound: true,
	}
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
		t.Error("direct client writer must NOT be called for a docs_only row")
	}
}

// TestMarketplaceInstall_UnknownId_Still404 proves the guard is precise: an id in
// NEITHER entries[] nor docs_only[] still returns 404 ENTRY_NOT_FOUND (not a
// docs-only refusal).
func TestMarketplaceInstall_UnknownId_Still404(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: nil, found: false, docsEntry: nil, docsFound: false}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, &fakeManifestCreator{}, &fakeInstaller{})

	rec := postInstall(t, s, `{"id":"nope","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Code != "ENTRY_NOT_FOUND" {
		t.Errorf("code = %q, want ENTRY_NOT_FOUND", body.Code)
	}
}

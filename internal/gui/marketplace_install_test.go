package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// ---- fakes for the four marketplace-install seams ----

type fakeMarketplaceEntryLoader struct {
	entry  *api.MarketplaceEntry
	found  bool
	err    error
	seenID string
	called bool
}

func (f *fakeMarketplaceEntryLoader) LoadEntry(_ context.Context, id string) (*api.MarketplaceEntry, bool, error) {
	f.called = true
	f.seenID = id
	return f.entry, f.found, f.err
}

type fakeGlobalPortPicker struct {
	port        int
	err         error
	seenRequest int
	called      bool
}

func (f *fakeGlobalPortPicker) PickGlobalPort(requested int) (int, error) {
	f.called = true
	f.seenRequest = requested
	return f.port, f.err
}

type fakeServerNamePresence struct {
	exists   bool
	err      error
	seenName string
}

func (f *fakeServerNamePresence) ServerExists(name string) (bool, error) {
	f.seenName = name
	return f.exists, f.err
}

type fakeDirectClientWriter struct {
	updated     []string
	failed      []directFailure
	seenClients []string
	seenEntry   *api.MarketplaceEntry
	called      bool
}

func (f *fakeDirectClientWriter) WriteDirect(entry *api.MarketplaceEntry, clientNames []string) ([]string, []directFailure) {
	f.called = true
	f.seenEntry = entry
	f.seenClients = clientNames
	return f.updated, f.failed
}

type fakeInstaller struct {
	seenName    string
	seenGUIPort int
	called      bool
	err         error
}

func (f *fakeInstaller) Install(name string, guiPort int) error {
	f.called = true
	f.seenName = name
	f.seenGUIPort = guiPort
	return f.err
}

// installTestServer wires the four install seams plus the reused
// manifestCreator + installer seams. Any unset seam is given a no-op default
// so a stray code path cannot nil-deref.
func newMarketplaceInstallTestServer(
	loader *fakeMarketplaceEntryLoader,
	picker *fakeGlobalPortPicker,
	presence *fakeServerNamePresence,
	writer *fakeDirectClientWriter,
	creator *fakeManifestCreator,
	installer *fakeInstaller,
) *Server {
	s := &Server{
		mux:                      http.NewServeMux(),
		marketplaceInstallLoader: loader,
		marketplacePortPicker:    picker,
		marketplaceNamePresence:  presence,
		marketplaceDirectWriter:  writer,
		manifestCreator:          creator,
		installer:                installer,
	}
	registerMarketplaceInstallRoutes(s)
	return s
}

func postInstall(t *testing.T, s *Server, body, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/marketplace/install", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", origin)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// stdioEntry is the canned full catalog entry the hub-mode tests resolve.
func stdioEntry(id string) *api.MarketplaceEntry {
	return &api.MarketplaceEntry{
		ID:        id,
		Name:      "Filesystem",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-filesystem"},
	}
}

func nativeHTTPEntry(id string) *api.MarketplaceEntry {
	return &api.MarketplaceEntry{
		ID:        id,
		Name:      "Serena",
		Transport: "native-http",
		Command:   "uvx",
		Args:      []string{"serena", "start-mcp-server", "--transport", "streamable-http"},
	}
}

// ---- hub mode ----

func TestMarketplaceInstall_HubHappyPath(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: stdioEntry("filesystem"), found: true}
	picker := &fakeGlobalPortPicker{port: 9207}
	presence := &fakeServerNamePresence{exists: false}
	creator := &fakeManifestCreator{}
	installer := &fakeInstaller{}
	s := newMarketplaceInstallTestServer(loader, picker, presence, &fakeDirectClientWriter{}, creator, installer)

	rec := postInstall(t, s, `{"id":"filesystem","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%q", rec.Code, rec.Body.String())
	}
	if creator.name != "filesystem" {
		t.Errorf("ManifestCreate name = %q, want filesystem", creator.name)
	}
	if !installer.called || installer.seenName != "filesystem" {
		t.Errorf("Install seenName=%q called=%v, want filesystem/true", installer.seenName, installer.called)
	}
	// The resolved port must reach the persisted YAML (substituted into
	// daemons[0].port) and the response body.
	if !strings.Contains(creator.yaml, "9207") {
		t.Errorf("manifest YAML missing resolved port 9207:\n%s", creator.yaml)
	}
	if strings.Contains(creator.yaml, "port: 0") {
		t.Errorf("manifest YAML still has the placeholder port:0:\n%s", creator.yaml)
	}
	var body struct {
		Name string `json:"name"`
		Port int    `json:"port"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Name != "filesystem" || body.Port != 9207 || body.Mode != "hub" {
		t.Errorf("response = %+v", body)
	}
}

func TestMarketplaceInstall_HubNativeHTTPPicksPort(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: nativeHTTPEntry("serena"), found: true}
	picker := &fakeGlobalPortPicker{port: 9211}
	creator := &fakeManifestCreator{}
	installer := &fakeInstaller{}
	s := newMarketplaceInstallTestServer(loader, picker, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, creator, installer)
	s.port.Store(9125)

	rec := postInstall(t, s, `{"id":"serena","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%q", rec.Code, rec.Body.String())
	}
	if !picker.called {
		t.Fatal("native-http hub install must pick a daemon port")
	}
	if !strings.Contains(creator.yaml, "transport: native-http") {
		t.Errorf("manifest YAML missing native-http transport:\n%s", creator.yaml)
	}
	if !strings.Contains(creator.yaml, "port: 9211") {
		t.Errorf("manifest YAML missing resolved port 9211:\n%s", creator.yaml)
	}
	if strings.Contains(creator.yaml, "transport: stdio-bridge") {
		t.Errorf("native-http install downgraded to stdio-bridge:\n%s", creator.yaml)
	}
	if !installer.called || installer.seenName != "serena" {
		t.Errorf("Install seenName=%q called=%v, want serena/true", installer.seenName, installer.called)
	}
	if installer.seenGUIPort != 9125 {
		t.Errorf("Install guiPort=%d, want 9125", installer.seenGUIPort)
	}
}

// The auto-pick path: ?port omitted → picker called with requested=0 and its
// chosen port flows into the manifest + response.
func TestMarketplaceInstall_HubAutoPicksPortWhenDraftPortZero(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: stdioEntry("git"), found: true}
	picker := &fakeGlobalPortPicker{port: 9201}
	creator := &fakeManifestCreator{}
	installer := &fakeInstaller{}
	s := newMarketplaceInstallTestServer(loader, picker, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, creator, installer)

	rec := postInstall(t, s, `{"id":"git","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%q", rec.Code, rec.Body.String())
	}
	if !picker.called || picker.seenRequest != 0 {
		t.Errorf("picker called=%v seenRequest=%d, want true/0 (auto-pick)", picker.called, picker.seenRequest)
	}
	if !strings.Contains(creator.yaml, "9201") {
		t.Errorf("auto-picked port 9201 not substituted into YAML:\n%s", creator.yaml)
	}
}

// An explicit ?port override is forwarded to the picker verbatim.
func TestMarketplaceInstall_HubForwardsRequestedPort(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: stdioEntry("git"), found: true}
	picker := &fakeGlobalPortPicker{port: 9250}
	s := newMarketplaceInstallTestServer(loader, picker, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, &fakeManifestCreator{}, &fakeInstaller{})

	rec := postInstall(t, s, `{"id":"git","mode":"hub","port":9250}`, "same-origin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if picker.seenRequest != 9250 {
		t.Errorf("picker saw requested=%d, want 9250", picker.seenRequest)
	}
}

func TestMarketplaceInstall_HubNameConflict409(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: stdioEntry("memory"), found: true}
	presence := &fakeServerNamePresence{exists: true}
	creator := &fakeManifestCreator{}
	installer := &fakeInstaller{}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, presence, &fakeDirectClientWriter{}, creator, installer)

	rec := postInstall(t, s, `{"id":"memory","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		ErrorCode     string `json:"error_code"`
		SuggestedName string `json:"suggested_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.ErrorCode != "NAME_CONFLICT" {
		t.Errorf("error_code = %q, want NAME_CONFLICT", body.ErrorCode)
	}
	if body.SuggestedName != "memory-2" {
		t.Errorf("suggested_name = %q, want memory-2", body.SuggestedName)
	}
	// On conflict the install MUST NOT proceed.
	if creator.name != "" || installer.called {
		t.Error("conflict must short-circuit before ManifestCreate/Install")
	}
}

// An explicit ?name override is what the conflict gate + manifest use.
func TestMarketplaceInstall_HubHonorsNameOverride(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: stdioEntry("filesystem"), found: true}
	presence := &fakeServerNamePresence{exists: false}
	creator := &fakeManifestCreator{}
	installer := &fakeInstaller{}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{port: 9202}, presence, &fakeDirectClientWriter{}, creator, installer)

	rec := postInstall(t, s, `{"id":"filesystem","mode":"hub","name":"fs-prod"}`, "same-origin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if presence.seenName != "fs-prod" {
		t.Errorf("conflict gate saw name=%q, want fs-prod", presence.seenName)
	}
	if creator.name != "fs-prod" || installer.seenName != "fs-prod" {
		t.Errorf("override name not used: create=%q install=%q", creator.name, installer.seenName)
	}
}

func TestMarketplaceInstall_HubPortPickerError_409(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: stdioEntry("git"), found: true}
	picker := &fakeGlobalPortPicker{err: errors.New("hub global band fully claimed")}
	creator := &fakeManifestCreator{}
	s := newMarketplaceInstallTestServer(loader, picker, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, creator, &fakeInstaller{})

	rec := postInstall(t, s, `{"id":"git","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "PORT_UNAVAILABLE") {
		t.Errorf("body missing PORT_UNAVAILABLE code: %q", rec.Body.String())
	}
	if creator.name != "" {
		t.Error("port failure must short-circuit before ManifestCreate")
	}
}

// http transport hub install: GenerateDraftManifest emits a remote-http
// manifest with NO daemons block, so the port picker must NOT be consulted and
// the response port is 0.
func TestMarketplaceInstall_HubHTTPTransportNoPortPick(t *testing.T) {
	httpEntry := &api.MarketplaceEntry{
		ID:        "qt-docs",
		Name:      "Qt Docs",
		Transport: "http",
		URL:       "https://example.com/qt-docs/mcp",
	}
	loader := &fakeMarketplaceEntryLoader{entry: httpEntry, found: true}
	picker := &fakeGlobalPortPicker{port: 9999} // must NOT be used
	creator := &fakeManifestCreator{}
	installer := &fakeInstaller{}
	s := newMarketplaceInstallTestServer(loader, picker, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, creator, installer)

	rec := postInstall(t, s, `{"id":"qt-docs","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if picker.called {
		t.Error("remote-http install must NOT call the daemon port picker")
	}
	if !installer.called || creator.name != "qt-docs" {
		t.Errorf("http hub install did not create+install: create=%q install=%v", creator.name, installer.called)
	}
	var body struct {
		Port int `json:"port"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Port != 0 {
		t.Errorf("remote-http response port = %d, want 0", body.Port)
	}
}

// http hub install with a ?name override: the in-YAML name must be rewritten
// to the override so ManifestCreate's name/YAML-name match gate accepts it.
func TestMarketplaceInstall_HubHTTPNameOverrideRewritesYAMLName(t *testing.T) {
	httpEntry := &api.MarketplaceEntry{
		ID:        "qt-docs",
		Name:      "Qt Docs",
		Transport: "http",
		URL:       "https://example.com/qt-docs/mcp",
	}
	loader := &fakeMarketplaceEntryLoader{entry: httpEntry, found: true}
	creator := &fakeManifestCreator{}
	installer := &fakeInstaller{}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, creator, installer)

	rec := postInstall(t, s, `{"id":"qt-docs","mode":"hub","name":"qt-docs-prod"}`, "same-origin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if creator.name != "qt-docs-prod" {
		t.Errorf("ManifestCreate storage name = %q, want qt-docs-prod", creator.name)
	}
	// The YAML body's top-level name must match the storage name (otherwise
	// the real ManifestCreate's parseManifestForName gate would reject it).
	if !strings.Contains(creator.yaml, "name: qt-docs-prod") {
		t.Errorf("YAML name not rewritten to override:\n%s", creator.yaml)
	}
	if strings.Contains(creator.yaml, "name: qt-docs\n") {
		t.Errorf("stale catalog-id name left in YAML:\n%s", creator.yaml)
	}
}

// ---- direct mode ----

func TestMarketplaceInstall_DirectHTTP_WritesToClients(t *testing.T) {
	httpEntry := &api.MarketplaceEntry{
		ID:        "qt-docs",
		Name:      "Qt Docs",
		Transport: "http",
		URL:       "https://example.com/qt-docs/mcp",
	}
	loader := &fakeMarketplaceEntryLoader{entry: httpEntry, found: true}
	writer := &fakeDirectClientWriter{updated: []string{"claude-code"}}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, writer, &fakeManifestCreator{}, &fakeInstaller{})

	rec := postInstall(t, s, `{"id":"qt-docs","mode":"direct","clients":["claude-code"]}`, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%q", rec.Code, rec.Body.String())
	}
	if !writer.called || len(writer.seenClients) != 1 || writer.seenClients[0] != "claude-code" {
		t.Errorf("direct writer saw clients=%v called=%v", writer.seenClients, writer.called)
	}
	if writer.seenEntry == nil || writer.seenEntry.ID != "qt-docs" {
		t.Errorf("direct writer saw entry=%+v", writer.seenEntry)
	}
	var body struct {
		ClientsUpdated []string        `json:"clients_updated"`
		ClientsFailed  []directFailure `json:"clients_failed"`
		Mode           string          `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.ClientsUpdated) != 1 || body.ClientsUpdated[0] != "claude-code" || body.Mode != "direct" {
		t.Errorf("body = %+v", body)
	}
}

func TestMarketplaceInstall_DirectPartialFailure_207(t *testing.T) {
	// Direct mode is http-only; the 207 partial-write path is transport-agnostic.
	httpE := &api.MarketplaceEntry{ID: "filesystem", Transport: "http", URL: "https://example.com/mcp"}
	loader := &fakeMarketplaceEntryLoader{entry: httpE, found: true}
	writer := &fakeDirectClientWriter{
		updated: []string{"claude-code"},
		failed:  []directFailure{{Client: "codex-cli", Error: "client config not writable"}},
	}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, writer, &fakeManifestCreator{}, &fakeInstaller{})

	rec := postInstall(t, s, `{"id":"filesystem","mode":"direct","clients":["claude-code","codex-cli"]}`, "same-origin")
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207, body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		ClientsFailed []directFailure `json:"clients_failed"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.ClientsFailed) != 1 || body.ClientsFailed[0].Client != "codex-cli" {
		t.Errorf("clients_failed = %+v", body.ClientsFailed)
	}
}

func TestMarketplaceInstall_DirectNoClients_400(t *testing.T) {
	httpE := &api.MarketplaceEntry{ID: "filesystem", Transport: "http", URL: "https://example.com/mcp"}
	loader := &fakeMarketplaceEntryLoader{entry: httpE, found: true}
	writer := &fakeDirectClientWriter{}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, writer, &fakeManifestCreator{}, &fakeInstaller{})

	rec := postInstall(t, s, `{"id":"filesystem","mode":"direct","clients":[]}`, "same-origin")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if writer.called {
		t.Error("direct writer must NOT be called with no clients")
	}
}

// TestMarketplaceInstall_DirectStdio_400 pins the batch-2 review fix: direct
// mode is http-only, so a stdio catalog entry is rejected with 400 BEFORE any
// client write (stdio's native shape varies per client — mcpServers/servers/
// context_servers/mcp/mcp_servers/TOML — so a single hardcoded direct write
// would silently land in the wrong key). stdio servers install via hub mode.
func TestMarketplaceInstall_DirectStdio_400(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: stdioEntry("filesystem"), found: true}
	writer := &fakeDirectClientWriter{}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, writer, &fakeManifestCreator{}, &fakeInstaller{})

	rec := postInstall(t, s, `{"id":"filesystem","mode":"direct","clients":["claude-code"]}`, "same-origin")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (direct-mode stdio unsupported), body=%q", rec.Code, rec.Body.String())
	}
	if writer.called {
		t.Error("direct writer must NOT be called for a stdio entry (rejected before write)")
	}
}

// ---- guards (same-origin, mode, not-found) ----

func TestMarketplaceInstall_RejectsCrossOrigin(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: stdioEntry("x"), found: true}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, &fakeManifestCreator{}, &fakeInstaller{})
	rec := postInstall(t, s, `{"id":"x","mode":"hub"}`, "cross-site")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if loader.called {
		t.Error("loader must NOT run on a CSRF-rejected request")
	}
}

func TestMarketplaceInstall_RejectsNonPOST(t *testing.T) {
	s := newMarketplaceInstallTestServer(&fakeMarketplaceEntryLoader{}, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, &fakeManifestCreator{}, &fakeInstaller{})
	req := httptest.NewRequest(http.MethodGet, "/api/marketplace/install", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func TestMarketplaceInstall_UnknownMode_400(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{entry: stdioEntry("x"), found: true}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, &fakeManifestCreator{}, &fakeInstaller{})
	rec := postInstall(t, s, `{"id":"x","mode":"sideways"}`, "same-origin")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%q", rec.Code, rec.Body.String())
	}
	if loader.called {
		t.Error("unknown mode must be rejected before the catalog load")
	}
}

func TestMarketplaceInstall_EntryNotFound_404(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{found: false}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, &fakeManifestCreator{}, &fakeInstaller{})
	rec := postInstall(t, s, `{"id":"ghost","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ENTRY_NOT_FOUND") {
		t.Errorf("body missing ENTRY_NOT_FOUND: %q", rec.Body.String())
	}
}

func TestMarketplaceInstall_LoaderError_502(t *testing.T) {
	loader := &fakeMarketplaceEntryLoader{err: errors.New("dial tcp: i/o timeout")}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, &fakeManifestCreator{}, &fakeInstaller{})
	rec := postInstall(t, s, `{"id":"x","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%q", rec.Code, rec.Body.String())
	}
	// The raw fetch error must not leak into the body.
	if strings.Contains(rec.Body.String(), "i/o timeout") {
		t.Errorf("body leaks fetch error: %q", rec.Body.String())
	}
}

func TestMarketplaceInstall_MissingID_400(t *testing.T) {
	s := newMarketplaceInstallTestServer(&fakeMarketplaceEntryLoader{}, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, &fakeManifestCreator{}, &fakeInstaller{})
	rec := postInstall(t, s, `{"id":"  ","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

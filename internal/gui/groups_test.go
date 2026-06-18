// groups_test.go — groups Phase 5b-1 (/api/groups CRUD authoring endpoint).
//
// Tests-first contract for the GUI authoring endpoint (decision
// work-items/decisions/2026-06-18-groups-namespaces-tool-visibility.md
// §"DECISION (2026-06-18)"):
//
//   - POST creates a group → GET returns it → on-disk groups.yaml has it
//     (real disk round-trip via api.SetDaemonStateRootForTest).
//   - POST with an invalid name (':'-bearing / empty) → 400.
//   - POST referencing an unknown server → 400 (authoring-boundary
//     strictness; the snapshot builder skips, the authoring endpoint
//     refuses).
//   - DELETE removes it → GET no longer returns it → 404 on re-delete.
//   - Same-origin guard rejects a cross-origin request.
//   - Live re-publish: after POST while the gate-ON hub listener is live,
//     the published snapshot carries Bindings["g:<name>"] (driving the
//     real publishResolverSnapshotForHubBind seam); restart_required is
//     false. When the hub is NOT live, the write persists and
//     restart_required is true.
//   - Method-guard (only GET/POST/DELETE; others → 405).
//
// State-safety: every disk-touching test installs its OWN per-test state
// root via api.SetDaemonStateRootForTest(t.TempDir()) (nesting under the
// internal/gui TestMain global), so no live supervisor / hub state is
// touched and tests are mutually isolated.

package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// fakeGroupsAPI is the stub groupsAPI for handler-logic tests that must
// not touch disk (validation, method-guard, same-origin). The disk
// round-trip tests use the production realGroupsAPI against a per-test
// state root instead.
type fakeGroupsAPI struct {
	cfg        api.GroupsConfig
	available  []string
	loadErr    error
	writeErr   error
	serversErr error
	// pruneErr, when non-nil AND the callback reports a deleted set, is
	// returned AFTER the write is recorded as applied — emulating the real
	// ReadModifyWriteGroups contract where the groups.yaml write committed but
	// the post-write token-prune failed (api.ErrTokenPruneFailed). It must be
	// wrapped in api.ErrTokenPruneFailed by the test so the handler's
	// errors.Is(err, api.ErrTokenPruneFailed) branch fires.
	pruneErr error

	// instanceID / groupTokens back the B4 connection-detail surface.
	// instanceID empty ⇒ HubInstanceID reports not-present; a group absent
	// from groupTokens ⇒ GroupToken reports not-present.
	instanceID  string
	groupTokens map[string]string

	writeCalls  int
	lastWrite   api.GroupsConfig
	lastDeleted []string
}

func (f *fakeGroupsAPI) LoadGroups() (api.GroupsConfig, error) {
	if f.loadErr != nil {
		return api.GroupsConfig{}, f.loadErr
	}
	return f.cfg, nil
}

// ReadModifyWriteGroups emulates the real atomic load→mutate→write: it runs
// the callback against the current cfg, records the write, applies it, and
// remembers the deleted-group set (the token-prune is a no-op in-fake — the
// fake holds no token table). loadErr / writeErr inject failure at the load
// and write steps respectively.
func (f *fakeGroupsAPI) ReadModifyWriteGroups(mutate func(cfg *api.GroupsConfig) ([]string, error)) error {
	if f.loadErr != nil {
		return f.loadErr
	}
	cfg := f.cfg
	deleted, err := mutate(&cfg)
	if err != nil {
		return err
	}
	f.writeCalls++
	f.lastWrite = cfg
	f.lastDeleted = deleted
	if f.writeErr != nil {
		return f.writeErr
	}
	f.cfg = cfg
	// Post-write token-prune failure: the groups.yaml write committed (f.cfg
	// updated above), but pruning the deleted token row failed. Mirrors the
	// real RMW returning ErrTokenPruneFailed AFTER a successful write.
	if f.pruneErr != nil && len(deleted) > 0 {
		return f.pruneErr
	}
	return nil
}

func (f *fakeGroupsAPI) AvailableServers() ([]string, error) {
	if f.serversErr != nil {
		return nil, f.serversErr
	}
	return f.available, nil
}

// HubInstanceID / GroupToken back the B4 connection-detail surface. The fake
// returns the injected instanceID (empty ⇒ not present) and looks the group
// token up in groupTokens (absent ⇒ not present), so a handler test can drive
// the available / not-available connection paths without touching live state.
func (f *fakeGroupsAPI) HubInstanceID() (string, bool) {
	if f.instanceID == "" {
		return "", false
	}
	return f.instanceID, true
}
func (f *fakeGroupsAPI) GroupToken(group string) (string, bool) {
	tok, ok := f.groupTokens[group]
	if !ok || tok == "" {
		return "", false
	}
	return tok, true
}

// groupsTestServer wires a fresh Server with the /api/groups route and
// injects the supplied groupsAPI. The republish seam is stubbed to a no-op
// by default (per-Server s.groupsRepublishFn field) so a handler-logic test
// never reaches the real hub snapshot; individual tests override
// s.groupsRepublishFn as needed.
func groupsTestServer(t *testing.T, g groupsAPI) *Server {
	t.Helper()
	s := NewServer(Config{})
	s.groups = g
	s.groupsRepublishFn = func(_ context.Context, _ *api.API) error { return nil }
	return s
}

func doJSON(t *testing.T, s *Server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rdr)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeListResp(t *testing.T, rec *httptest.ResponseRecorder) groupsListResponse {
	t.Helper()
	var resp groupsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list response %q: %v", rec.Body.String(), err)
	}
	return resp
}

func decodeMutationResp(t *testing.T, rec *httptest.ResponseRecorder) groupMutationResponse {
	t.Helper()
	var resp groupMutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode mutation response %q: %v", rec.Body.String(), err)
	}
	return resp
}

// --- GET ---

func TestGroups_GetEmpty(t *testing.T) {
	s := groupsTestServer(t, &fakeGroupsAPI{available: []string{"memory", "time"}})
	rec := doJSON(t, s, http.MethodGet, "/api/groups", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	resp := decodeListResp(t, rec)
	if resp.Groups == nil {
		t.Fatal("groups must be a non-nil array even when empty")
	}
	if len(resp.Groups) != 0 {
		t.Fatalf("groups = %d, want 0 on empty config", len(resp.Groups))
	}
	if len(resp.AvailableServers) != 2 {
		t.Fatalf("available_servers = %v, want [memory time]", resp.AvailableServers)
	}
}

// --- POST validation ---

func TestGroups_PostInvalidName(t *testing.T) {
	s := groupsTestServer(t, &fakeGroupsAPI{available: []string{"memory"}})

	// ':'-bearing name (would forge the "g:" kind prefix) → 400.
	rec := doJSON(t, s, http.MethodPost, "/api/groups", map[string]any{
		"name": "bad:name", "servers": []string{"memory"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, want 400 for ':'-bearing name", rec.Code, rec.Body.String())
	}

	// Empty name → 400.
	rec = doJSON(t, s, http.MethodPost, "/api/groups", map[string]any{
		"name": "   ", "servers": []string{"memory"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, want 400 for empty name", rec.Code, rec.Body.String())
	}
}

func TestGroups_PostUnknownServer(t *testing.T) {
	g := &fakeGroupsAPI{available: []string{"memory", "time"}}
	s := groupsTestServer(t, g)
	rec := doJSON(t, s, http.MethodPost, "/api/groups", map[string]any{
		"name": "frontend", "servers": []string{"memory", "ghost"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, want 400 for unknown server", rec.Code, rec.Body.String())
	}
	if g.writeCalls != 0 {
		t.Fatalf("WriteGroups called %d times despite unknown-server rejection — must not persist", g.writeCalls)
	}
}

func TestGroups_PostToolsHiddenNonMember(t *testing.T) {
	g := &fakeGroupsAPI{available: []string{"memory", "time"}}
	s := groupsTestServer(t, g)
	// tools_hidden names a server NOT in the group's own servers → 400.
	rec := doJSON(t, s, http.MethodPost, "/api/groups", map[string]any{
		"name":         "frontend",
		"servers":      []string{"memory"},
		"tools_hidden": map[string][]string{"time": {"now"}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, want 400 for tools_hidden non-member server", rec.Code, rec.Body.String())
	}
	if g.writeCalls != 0 {
		t.Fatalf("WriteGroups called despite non-member tools_hidden rejection")
	}
}

func TestGroups_PostDuplicateUpdatesInPlace(t *testing.T) {
	g := &fakeGroupsAPI{
		available: []string{"memory", "time"},
		cfg: api.GroupsConfig{Version: 1, Groups: []api.Group{
			{Name: "frontend", Servers: []string{"memory"}},
		}},
	}
	s := groupsTestServer(t, g)
	// POST the same name with a different server set → UPDATE in place, not
	// a duplicate row (WriteGroups would reject a dup anyway, but the
	// handler must do an in-place replace).
	rec := doJSON(t, s, http.MethodPost, "/api/groups", map[string]any{
		"name": "frontend", "servers": []string{"time"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200", rec.Code, rec.Body.String())
	}
	if n := len(g.lastWrite.Groups); n != 1 {
		t.Fatalf("after upsert there are %d groups, want 1 (in-place update, no dup)", n)
	}
	if got := g.lastWrite.Groups[0].Servers; len(got) != 1 || got[0] != "time" {
		t.Fatalf("upsert did not replace servers: %+v", got)
	}
}

// --- method + same-origin guards ---

func TestGroups_MethodNotAllowed(t *testing.T) {
	s := groupsTestServer(t, &fakeGroupsAPI{available: []string{"memory"}})
	rec := doJSON(t, s, http.MethodPut, "/api/groups", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405 for PUT", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, POST, DELETE" {
		t.Fatalf("Allow=%q, want 'GET, POST, DELETE'", allow)
	}
}

func TestGroups_CrossOriginRejected(t *testing.T) {
	s := groupsTestServer(t, &fakeGroupsAPI{available: []string{"memory"}})
	req := httptest.NewRequest(http.MethodGet, "/api/groups", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for cross-site request", rec.Code)
	}
}

// --- DELETE ---

func TestGroups_DeleteRequiresName(t *testing.T) {
	s := groupsTestServer(t, &fakeGroupsAPI{available: []string{"memory"}})
	rec := doJSON(t, s, http.MethodDelete, "/api/groups", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, want 400 for missing name", rec.Code, rec.Body.String())
	}
}

func TestGroups_DeleteAbsentIs404(t *testing.T) {
	g := &fakeGroupsAPI{available: []string{"memory"}}
	s := groupsTestServer(t, g)
	rec := doJSON(t, s, http.MethodDelete, "/api/groups?name=ghost", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%q, want 404 for absent group", rec.Code, rec.Body.String())
	}
	if g.writeCalls != 0 {
		t.Fatalf("WriteGroups called on a 404 delete — must not persist")
	}
}

// TestGroups_DeleteTokenPruneFailStillRepublishes pins B3 (bot R3): when the
// groups.yaml write committed but the post-write token-prune failed
// (api.ErrTokenPruneFailed), the DELETE must NOT 500. It is a degraded SUCCESS:
// the handler still republishes the snapshot (dropping the deleted group so its
// /g/ route 404s) and returns 200 with restart_required=true (forced, so the
// operator restarts the hub to clear the stale token row). A 500 here would
// skip the republish and strand a routable deleted group.
func TestGroups_DeleteTokenPruneFailStillRepublishes(t *testing.T) {
	g := &fakeGroupsAPI{
		available: []string{"memory"},
		cfg:       api.GroupsConfig{Version: 1, Groups: []api.Group{{Name: "frontend", Servers: []string{"memory"}}}},
		// Inject a post-write prune failure wrapped in the sentinel.
		pruneErr: fmt.Errorf("%w: simulated DACL gate failure", api.ErrTokenPruneFailed),
	}
	s := groupsTestServer(t, g)
	// Mark the gate-ON hub listener live so HubMcpEndpointActive() is true and
	// the republish path runs.
	comp := &HubListenerComponents{port: 9200}
	comp.alive.Store(true)
	s.hubMcpComp.Store(comp)
	republishCalled := false
	s.groupsRepublishFn = func(_ context.Context, _ *api.API) error { republishCalled = true; return nil }

	rec := doJSON(t, s, http.MethodDelete, "/api/groups?name=frontend", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200 (token-prune fail is a degraded success, NOT a 500)", rec.Code, rec.Body.String())
	}
	resp := decodeMutationResp(t, rec)
	if !resp.RestartRequired {
		t.Fatal("restart_required must be forced true on a token-prune-failure delete (stale token row needs a hub restart)")
	}
	if !republishCalled {
		t.Fatal("republish MUST still fire on a token-prune-failure delete — the snapshot must drop the deleted group so its /g/ route 404s")
	}
	if g.writeCalls != 1 {
		t.Fatalf("WriteGroups calls=%d, want 1 (the groups.yaml delete persisted)", g.writeCalls)
	}
}

// TestGroups_GetConnectionDetailsWhenHubLive pins B4 (bot R3): the GET list
// path surfaces a usable /g/<group>/mcp connection triple (url + token +
// instance id) when the gate-ON hub is live and the auth seam is primed.
func TestGroups_GetConnectionDetailsWhenHubLive(t *testing.T) {
	g := &fakeGroupsAPI{
		available:   []string{"memory"},
		cfg:         api.GroupsConfig{Version: 1, Groups: []api.Group{{Name: "frontend", Servers: []string{"memory"}}}},
		instanceID:  "inst-abc",
		groupTokens: map[string]string{"frontend": "tok-frontend"},
	}
	s := groupsTestServer(t, g)
	comp := &HubListenerComponents{port: 9201}
	comp.alive.Store(true)
	s.hubMcpComp.Store(comp)

	rec := doJSON(t, s, http.MethodGet, "/api/groups", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	resp := decodeListResp(t, rec)
	if len(resp.Groups) != 1 {
		t.Fatalf("groups=%d want 1", len(resp.Groups))
	}
	conn := resp.Groups[0].Connection
	if conn == nil {
		t.Fatal("connection must be present on the GET list path when the hub is live")
	}
	if !conn.Available {
		t.Fatalf("connection.available=false, want true (hub live + token + instance present): %+v", conn)
	}
	wantURL := "http://127.0.0.1:9201/g/frontend/mcp"
	if conn.URL != wantURL {
		t.Errorf("connection.url=%q want %q", conn.URL, wantURL)
	}
	if conn.InstanceID != "inst-abc" {
		t.Errorf("connection.instance_id=%q want inst-abc", conn.InstanceID)
	}
	if conn.Token != "tok-frontend" {
		t.Errorf("connection.token=%q want tok-frontend", conn.Token)
	}
}

// TestGroups_GetConnectionPlaceholderWhenHubOff pins B4: when the hub is
// gate-OFF / not bound, the connection is NOT available and carries a hint
// instead of a dead URL + a real token.
func TestGroups_GetConnectionPlaceholderWhenHubOff(t *testing.T) {
	g := &fakeGroupsAPI{
		available:   []string{"memory"},
		cfg:         api.GroupsConfig{Version: 1, Groups: []api.Group{{Name: "frontend", Servers: []string{"memory"}}}},
		instanceID:  "inst-abc",
		groupTokens: map[string]string{"frontend": "tok-frontend"},
	}
	s := groupsTestServer(t, g)
	// No hubMcpComp → HubMcpBoundPort() returns (0,false) → gate-OFF.

	rec := doJSON(t, s, http.MethodGet, "/api/groups", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	resp := decodeListResp(t, rec)
	conn := resp.Groups[0].Connection
	if conn == nil {
		t.Fatal("connection must still be present (carrying the not-available placeholder)")
	}
	if conn.Available {
		t.Fatal("connection.available must be false when the hub is not bound")
	}
	if conn.URL != "" || conn.Token != "" {
		t.Errorf("a not-available connection must NOT carry a URL or token: %+v", conn)
	}
	if conn.Hint == "" {
		t.Error("a not-available connection must carry a hint telling the operator to start the hub")
	}
}

// --- restart_required wiring (hub not live) ---

func TestGroups_PostRestartRequiredWhenHubNotLive(t *testing.T) {
	g := &fakeGroupsAPI{available: []string{"memory"}}
	s := groupsTestServer(t, g)
	// No hubMcpComp stored → HubMcpEndpointActive() is false → gate-OFF.
	republishCalled := false
	s.groupsRepublishFn = func(_ context.Context, _ *api.API) error { republishCalled = true; return nil }

	rec := doJSON(t, s, http.MethodPost, "/api/groups", map[string]any{
		"name": "frontend", "servers": []string{"memory"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200", rec.Code, rec.Body.String())
	}
	resp := decodeMutationResp(t, rec)
	if !resp.RestartRequired {
		t.Fatal("restart_required must be true when hub is not live")
	}
	if resp.HubLive {
		t.Fatal("hub_live must be false when no listener is up")
	}
	if republishCalled {
		t.Fatal("republish must NOT fire when the hub is not live")
	}
	if g.writeCalls != 1 {
		t.Fatalf("WriteGroups calls=%d, want 1 (write persists regardless of gate state)", g.writeCalls)
	}
	if resp.Group == nil || resp.Group.Name != "frontend" {
		t.Fatalf("returned group = %+v, want frontend", resp.Group)
	}
}

// --- DISK ROUND-TRIP: POST → GET → on-disk groups.yaml (real api) ---

func TestGroups_DiskRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

	s := NewServer(Config{})
	// Use the REAL groups api so the write hits disk; pin the available
	// server set deterministically (the host's embedded manifests are
	// irrelevant — we only need "memory" to be known).
	s.groups = diskTestGroupsAPI{available: []string{"memory", "time"}}
	s.groupsRepublishFn = func(_ context.Context, _ *api.API) error { return nil }

	// POST creates the group.
	rec := doJSON(t, s, http.MethodPost, "/api/groups", map[string]any{
		"name": "frontend", "description": "JS/TS", "servers": []string{"memory", "time"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%q", rec.Code, rec.Body.String())
	}

	// On-disk groups.yaml carries it.
	dir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "groups.yaml")); err != nil {
		t.Fatalf("groups.yaml not written to disk: %v", err)
	}
	onDisk, err := api.LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	if len(onDisk.Groups) != 1 || onDisk.Groups[0].Name != "frontend" {
		t.Fatalf("on-disk groups = %+v, want one 'frontend'", onDisk.Groups)
	}
	if len(onDisk.Groups[0].Servers) != 2 {
		t.Fatalf("on-disk servers = %v, want [memory time]", onDisk.Groups[0].Servers)
	}

	// GET returns it.
	rec = doJSON(t, s, http.MethodGet, "/api/groups", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%q", rec.Code, rec.Body.String())
	}
	list := decodeListResp(t, rec)
	if len(list.Groups) != 1 || list.Groups[0].Name != "frontend" {
		t.Fatalf("GET groups = %+v, want one 'frontend'", list.Groups)
	}
	if list.Groups[0].Description != "JS/TS" {
		t.Fatalf("description not round-tripped: %q", list.Groups[0].Description)
	}

	// DELETE removes it.
	rec = doJSON(t, s, http.MethodDelete, "/api/groups?name=frontend", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%q", rec.Code, rec.Body.String())
	}
	afterDelete, err := api.LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups after delete: %v", err)
	}
	if len(afterDelete.Groups) != 0 {
		t.Fatalf("group not removed from disk: %+v", afterDelete.Groups)
	}

	// GET no longer returns it.
	rec = doJSON(t, s, http.MethodGet, "/api/groups", nil)
	list = decodeListResp(t, rec)
	if len(list.Groups) != 0 {
		t.Fatalf("GET still returns a deleted group: %+v", list.Groups)
	}

	// Re-DELETE → 404.
	rec = doJSON(t, s, http.MethodDelete, "/api/groups?name=frontend", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete status=%d, want 404", rec.Code)
	}
}

// diskTestGroupsAPI uses the REAL api LoadGroups/ReadModifyWriteGroups (so
// the atomic RMW hits the per-test state root on disk) but pins the
// available-server set to a deterministic list (independent of the host's
// embedded manifests).
type diskTestGroupsAPI struct{ available []string }

func (diskTestGroupsAPI) LoadGroups() (api.GroupsConfig, error) { return api.LoadGroups() }
func (diskTestGroupsAPI) ReadModifyWriteGroups(mutate func(cfg *api.GroupsConfig) ([]string, error)) error {
	return api.ReadModifyWriteGroups(mutate)
}
func (d diskTestGroupsAPI) AvailableServers() ([]string, error) { return d.available, nil }
func (diskTestGroupsAPI) HubInstanceID() (string, bool) {
	ep, err := api.LoadHubEndpoint()
	if err != nil || ep.InstanceID == "" {
		return "", false
	}
	return ep.InstanceID, true
}
func (diskTestGroupsAPI) GroupToken(group string) (string, bool) {
	tbl := api.CurrentTokenTable()
	tok, ok := tbl.Tokens[api.GroupScopeKey(group)]
	if !ok || tok == "" {
		return "", false
	}
	return tok, true
}

// --- LIVE RE-PUBLISH: published snapshot carries Bindings["g:<name>"] ---

// TestGroups_LiveRepublishCarriesGroupBinding drives the REAL
// publishResolverSnapshotForHubBind seam (groupsRepublishFn left nil) with
// the gate-ON hub listener marked live, and asserts the published
// ResolverSnapshot gains the new group's kind-namespaced binding key. This
// proves a GUI group edit is LIVE immediately (Phase 3b staleness closed
// for GUI edits) and that restart_required is false.
func TestGroups_LiveRepublishCarriesGroupBinding(t *testing.T) {
	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

	// Reset the package-level resolver snapshot so this test's published
	// snapshot is observed in isolation.
	api.PublishResolverSnapshot(nil)
	t.Cleanup(func() { api.PublishResolverSnapshot(nil) })

	s := NewServer(Config{})
	// Real groups api so WriteGroups + the republish's LoadGroups see the
	// same on-disk file. Pin the available set to a server that has a real
	// embedded manifest so the snapshot builder resolves its daemons.
	s.groups = diskTestGroupsAPI{available: mustEmbeddedServerName(t)}

	// Mark the gate-ON hub listener live so HubMcpEndpointActive() is true.
	comp := &HubListenerComponents{}
	comp.alive.Store(true)
	s.hubMcpComp.Store(comp)

	// Leave s.groupsRepublishFn nil → the handler calls the REAL
	// publishResolverSnapshotForHubBind(s.api) seam.
	s.groupsRepublishFn = nil

	member := s.groups.(diskTestGroupsAPI).available[0]
	rec := doJSON(t, s, http.MethodPost, "/api/groups", map[string]any{
		"name": "frontend", "servers": []string{member},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%q", rec.Code, rec.Body.String())
	}
	resp := decodeMutationResp(t, rec)
	if !resp.HubLive {
		t.Fatal("hub_live must be true with a live listener")
	}
	if resp.RestartRequired {
		t.Fatal("restart_required must be false after a successful live re-publish")
	}

	// The published snapshot must now carry the kind-namespaced group key.
	snap := api.LoadResolverSnapshot()
	if snap == nil {
		t.Fatal("no snapshot published after a live group edit")
	}
	key := api.GroupScopeKey("frontend")
	refs, ok := snap.Bindings[key]
	if !ok {
		t.Fatalf("published snapshot has no Bindings[%q]; keys=%v", key, snapKeys(snap))
	}
	if len(refs) == 0 {
		t.Fatalf("group %q published with zero daemon refs (member %q resolved nothing)", key, member)
	}
}

// snapKeys returns the Bindings keys of a snapshot for diagnostic messages.
func snapKeys(snap *api.ResolverSnapshot) []string {
	out := make([]string, 0, len(snap.Bindings))
	for k := range snap.Bindings {
		out = append(out, k)
	}
	return out
}

// mustEmbeddedServerName returns the first embedded server name whose
// manifest resolves at least one daemon — matching the resolution path
// publishResolverSnapshotForHubBind itself uses (ManifestGet +
// config.ParseManifest, then daemon refs). Resolving against the SAME path
// keeps the test robust to embedded-manifest order / content churn: it
// never assumes index 0 has a daemon. Skips when no embedded manifest with
// a daemon exists on this host (the live-republish daemon-resolution
// assertion is then unprovable here).
func mustEmbeddedServerName(t *testing.T) []string {
	t.Helper()
	a := api.NewAPI()
	names, err := a.ManifestList()
	if err != nil {
		t.Fatalf("ManifestList: %v", err)
	}
	for _, name := range names {
		yamlStr, gerr := a.ManifestGet(name)
		if gerr != nil {
			continue
		}
		m, perr := config.ParseManifest(strings.NewReader(yamlStr))
		if perr != nil {
			continue
		}
		if len(m.Daemons) > 0 {
			return []string{name}
		}
	}
	t.Skip("no embedded manifest with a daemon on this host; cannot drive live-republish daemon resolution")
	return nil
}

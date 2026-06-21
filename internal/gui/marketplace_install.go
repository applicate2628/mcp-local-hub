package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// ---------------------------------------------------------------------------
// POST /api/marketplace/install — one-click marketplace install (roadmap §B
// #1, slices 1+2). Two modes:
//
//   - mode="hub": draft a manifest from the catalog entry (api.GenerateDraftManifest),
//     resolve a free hub-band daemon port, persist the manifest (ManifestCreate),
//     and install it (Install) so the server runs as a hub-owned daemon. This is
//     the process-tail-compression path: ONE hub daemon every client routes to.
//   - mode="direct": write the client-native entry straight into each selected
//     client's config (the SecureWriteClientConfig pipeline) with NO manifest,
//     NO daemon, NO supervisor-intent row. The classic "each client spawns its
//     own copy" path, offered for operators who want it.
//
// All live-fleet-touching work is behind Server-local interfaces so the handler
// test injects fakes and never reaches the real fleet (mirrors install.go /
// manifest.go).
// ---------------------------------------------------------------------------

// marketplaceEntryLoader loads the FULL api.MarketplaceEntry for an id (the
// install handler needs transport/command/args/url/env, which the read-only
// GET /api/marketplace DTO omits). Returns (entry, found, err): found=false
// means the id is not in the catalog (handler → 404); err is reserved for a
// fetch/parse failure (handler → 502). Production loads the curated catalog
// via api.LoadMarketplaceCatalog — the same catalog-load path GET
// /api/marketplace and the refresh handler use.
type marketplaceEntryLoader interface {
	LoadEntry(ctx context.Context, id string) (*api.MarketplaceEntry, bool, error)
}

// globalPortPicker resolves the daemon port for a hub-mode install. It honors
// a non-zero requested port when it is in-band AND free, otherwise auto-picks
// the lowest free hub-band port (scanning installed manifests' daemon ports +
// the OS bind probe via api.AllocateSingleGlobalPort). Returns a typed error
// the handler maps to a 4xx/5xx.
type globalPortPicker interface {
	PickGlobalPort(requested int) (int, error)
}

// serverNamePresence reports whether a server name already has an installed
// manifest — the NAME_CONFLICT 409 gate. Production consults ManifestList.
type serverNamePresence interface {
	ServerExists(name string) (bool, error)
}

// directClientWriter writes the client-native entry for a direct-mode install
// into each named client config (the SecureWriteClientConfig pipeline via the
// clients package). Returns the per-client updated / failed split so the
// handler can 200 (all updated) or 207 (partial). It never touches the hub
// daemon set or supervisor-intent.
type directClientWriter interface {
	WriteDirect(entry *api.MarketplaceEntry, clientNames []string) (updated []string, failed []directFailure)
}

// directFailure pairs a client name with the reason its direct write failed,
// surfaced in the 207 body so the operator sees which client to fix.
type directFailure struct {
	Client string `json:"client"`
	Error  string `json:"error"`
}

// marketplaceInstallRequest is the POST body. mode is required; name/port are
// optional hub-mode overrides; clients is the direct-mode target list.
type marketplaceInstallRequest struct {
	ID      string   `json:"id"`
	Mode    string   `json:"mode"`
	Name    string   `json:"name"`
	Port    int      `json:"port"`
	Clients []string `json:"clients"`
}

// registerMarketplaceInstallRoutes wires POST /api/marketplace/install onto the
// mux. Same requireSameOrigin guard as the other mutating /api/* routes.
func registerMarketplaceInstallRoutes(s *Server) {
	s.mux.HandleFunc("/api/marketplace/install", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req marketplaceInstallRequest
		if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
			writeDecodeBodyError(w, err, "BAD_REQUEST")
			return
		}
		id := strings.TrimSpace(req.ID)
		if id == "" {
			writeAPIError(w, fmt.Errorf("id required"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		mode := strings.TrimSpace(req.Mode)
		if mode != "hub" && mode != "direct" {
			writeAPIError(w, fmt.Errorf("mode must be \"hub\" or \"direct\""), http.StatusBadRequest, "BAD_REQUEST")
			return
		}

		// Load the FULL catalog entry once; both modes need it.
		entry, found, err := s.marketplaceInstallLoader.LoadEntry(r.Context(), id)
		if err != nil {
			// A fetch/parse failure is upstream-degraded, not the client's
			// fault. Log server-side; return a stable 502 (no raw error leak).
			log.Printf("/api/marketplace/install LoadEntry id=%q: %v", id, err)
			writeAPIError(w, errors.New("marketplace catalog unavailable"), http.StatusBadGateway, "CATALOG_UNAVAILABLE")
			return
		}
		if !found {
			writeAPIError(w, fmt.Errorf("marketplace entry %q not found", id), http.StatusNotFound, "ENTRY_NOT_FOUND")
			return
		}

		switch mode {
		case "hub":
			s.handleMarketplaceHubInstall(w, &req, entry)
		case "direct":
			s.handleMarketplaceDirectInstall(w, &req, entry)
		}
	}))
}

// handleMarketplaceHubInstall drafts → port-resolves → ManifestCreate → Install.
func (s *Server) handleMarketplaceHubInstall(w http.ResponseWriter, req *marketplaceInstallRequest, entry *api.MarketplaceEntry) {
	// The server name defaults to the catalog id; an explicit ?name overrides.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = entry.ID
	}

	// NAME_CONFLICT gate: refuse if a server with this name is already
	// installed, suggesting a "-2" variant so the frontend can offer a
	// one-click retry under a fresh name.
	exists, err := s.marketplaceNamePresence.ServerExists(name)
	if err != nil {
		log.Printf("/api/marketplace/install ServerExists name=%q: %v", name, err)
		writeAPIError(w, errors.New("internal error checking server name"), http.StatusInternalServerError, "INSTALL_FAILED")
		return
	}
	if exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error_code":     "NAME_CONFLICT",
			"suggested_name": name + "-2",
		})
		return
	}

	// Draft the manifest YAML from the catalog entry. The generator emits a
	// global daemon manifest with daemons[0].port:0 for stdio/native-http
	// transports (which manifest-create rejects), or a remote-http manifest
	// with no daemon for http transport.
	draft, _, err := api.GenerateDraftManifest(entry, api.GenerateOpts{})
	if err != nil {
		// A generator refusal (hostile registry runes, empty url, getwd
		// failure) is a bad-entry condition; surface it as a 400 with the
		// generator's own message — it names the catalog defect.
		writeAPIError(w, err, http.StatusBadRequest, "BAD_ENTRY")
		return
	}

	finalYAML := draft
	resolvedPort := 0
	if entry.Transport == "stdio" || entry.Transport == "native-http" {
		// daemon drafts carry daemons[0].port:0 — substitute the resolved
		// hub-band port AND rewrite the top-level name to the operator's
		// final choice (ManifestCreate rejects a name/YAML mismatch).
		port, err := s.marketplacePortPicker.PickGlobalPort(req.Port)
		if err != nil {
			writeAPIError(w, fmt.Errorf("resolve hub daemon port: %w", err), http.StatusConflict, "PORT_UNAVAILABLE")
			return
		}
		substituted, err := substituteDraftDaemonPort(draft, name, port)
		if err != nil {
			log.Printf("/api/marketplace/install substitute port name=%q: %v", name, err)
			writeAPIError(w, errors.New("internal error preparing manifest"), http.StatusInternalServerError, "INSTALL_FAILED")
			return
		}
		finalYAML = substituted
		resolvedPort = port
	} else {
		// remote-http drafts have no daemons block (no port to resolve), but
		// the in-YAML name still defaults to the catalog id — rewrite it to
		// the operator's final name so ManifestCreate's name/YAML-name match
		// gate (parseManifestForName) accepts the write under an override.
		rewritten, err := rewriteDraftName(draft, name)
		if err != nil {
			log.Printf("/api/marketplace/install rewrite name name=%q: %v", name, err)
			writeAPIError(w, errors.New("internal error preparing manifest"), http.StatusInternalServerError, "INSTALL_FAILED")
			return
		}
		finalYAML = rewritten
	}

	if err := s.manifestCreator.ManifestCreate(name, finalYAML); err != nil {
		log.Printf("/api/marketplace/install ManifestCreate name=%q: %v", name, err)
		writeAPIError(w, errors.New("internal error creating manifest"), http.StatusInternalServerError, "INSTALL_FAILED")
		return
	}
	if err := s.installer.Install(name, s.Port()); err != nil {
		log.Printf("/api/marketplace/install Install name=%q: %v", name, err)
		writeAPIError(w, errors.New("internal error installing server"), http.StatusInternalServerError, "INSTALL_FAILED")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name": name,
		"port": resolvedPort,
		"mode": "hub",
	})
}

// handleMarketplaceDirectInstall writes the client-native entry into each
// selected client config — no manifest, no daemon, no supervisor row.
func (s *Server) handleMarketplaceDirectInstall(w http.ResponseWriter, req *marketplaceInstallRequest, entry *api.MarketplaceEntry) {
	// Direct mode writes the server straight into client configs with no hub
	// daemon. Only http (remote-URL) entries have ONE client-native shape every
	// adapter owns (via AddEntry). A stdio entry's native shape varies per
	// client — mcpServers (claude/cursor), servers (vscode), context_servers
	// (zed), mcp (opencode), mcp_servers (hermes), TOML (codex) — so a single
	// hardcoded direct-stdio write would silently land in the wrong key for
	// several clients (batch-2 review finding). Until the clients adapter
	// interface grows a per-client stdio writer, direct mode is http-only;
	// stdio servers install correctly via hub mode (one shared daemon every
	// client routes to). Fail loud so the frontend can gate the toggle.
	if entry.Transport != "http" {
		writeAPIError(w, fmt.Errorf("direct-mode install supports http servers only (this entry is transport=%q) — use hub mode for daemon-backed servers", entry.Transport), http.StatusBadRequest, "DIRECT_MODE_UNSUPPORTED_TRANSPORT")
		return
	}

	clientNames := make([]string, 0, len(req.Clients))
	for _, c := range req.Clients {
		if c = strings.TrimSpace(c); c != "" {
			clientNames = append(clientNames, c)
		}
	}
	if len(clientNames) == 0 {
		writeAPIError(w, fmt.Errorf("clients required for direct-mode install"), http.StatusBadRequest, "BAD_REQUEST")
		return
	}

	updated, failed := s.marketplaceDirectWriter.WriteDirect(entry, clientNames)
	if updated == nil {
		updated = []string{}
	}
	if failed == nil {
		failed = []directFailure{}
	}
	status := http.StatusOK
	if len(failed) > 0 {
		status = http.StatusMultiStatus // 207 — partial direct write
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"clients_updated": updated,
		"clients_failed":  failed,
		"mode":            "direct",
	})
}

// substituteDraftDaemonPort parses the stdio draft YAML, replaces the
// daemons[0].port (0 → the resolved hub-band port), optionally renames the
// top-level `name` to the operator-chosen final name, and re-marshals. The
// generator's leading `# ...` comment header is dropped — it was an operator
// reminder for the manual CLI flow; the one-click install path resolves those
// edits itself, so the persisted manifest is the clean resolved YAML.
func substituteDraftDaemonPort(draft, name string, port int) (string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(draft), &doc); err != nil {
		return "", fmt.Errorf("parse draft manifest: %w", err)
	}
	doc["name"] = name
	daemonsRaw, ok := doc["daemons"].([]any)
	if !ok || len(daemonsRaw) == 0 {
		return "", fmt.Errorf("draft manifest has no daemons block to assign a port")
	}
	first, ok := daemonsRaw[0].(map[string]any)
	if !ok {
		return "", fmt.Errorf("draft manifest daemons[0] is not a mapping")
	}
	first["port"] = port
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("re-marshal manifest: %w", err)
	}
	return string(out), nil
}

// rewriteDraftName parses a draft manifest YAML, sets its top-level `name` to
// the operator-chosen final name, and re-marshals. Used for the remote-http
// hub-install path (which has no daemons block to substitute): the generator
// defaults `name:` to the catalog id, but ManifestCreate's parseManifestForName
// gate rejects any YAML whose name differs from the requested storage name, so
// an explicit ?name override would otherwise be refused. The leading `# ...`
// comment header is dropped (same rationale as substituteDraftDaemonPort).
func rewriteDraftName(draft, name string) (string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(draft), &doc); err != nil {
		return "", fmt.Errorf("parse draft manifest: %w", err)
	}
	doc["name"] = name
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("re-marshal manifest: %w", err)
	}
	return string(out), nil
}

// sortedClientNames returns clientNames sorted so the direct-write order (and
// thus the response order) is deterministic across runs.
func sortedClientNames(clientNames []string) []string {
	out := append([]string(nil), clientNames...)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Production adapters
// ---------------------------------------------------------------------------

// realMarketplaceEntryLoader loads the full catalog entry by id from the
// curated registry (the same api.LoadMarketplaceCatalog path GET
// /api/marketplace uses). A 24h-TTL cache hit returns immediately.
type realMarketplaceEntryLoader struct{}

func (realMarketplaceEntryLoader) LoadEntry(ctx context.Context, id string) (*api.MarketplaceEntry, bool, error) {
	cat, _, err := api.LoadMarketplaceCatalog(ctx, defaultMarketplaceRegistryURL)
	if err != nil {
		return nil, false, err
	}
	for i := range cat.Entries {
		if cat.Entries[i].ID == id {
			e := cat.Entries[i]
			return &e, true, nil
		}
	}
	return nil, false, nil
}

// realGlobalPortPicker resolves a hub-band daemon port. It honors a non-zero
// requested port when it is in-band AND not already claimed by an installed
// manifest daemon AND OS-free; otherwise it auto-picks the lowest free band
// port. The installed-daemon-port set is scanned from every installed
// manifest's daemons[] so a one-click install never collides with an existing
// hub daemon.
type realGlobalPortPicker struct{}

func (realGlobalPortPicker) PickGlobalPort(requested int) (int, error) {
	taken, err := installedGlobalDaemonPorts()
	if err != nil {
		return 0, err
	}
	if requested != 0 {
		if !api.PortInGlobalDaemonBand(requested) {
			return 0, fmt.Errorf("requested port %d is outside the hub daemon band", requested)
		}
		if taken[requested] {
			return 0, fmt.Errorf("requested port %d is already used by an installed daemon", requested)
		}
		// An OS-level bind collision on the requested port is caught loud at
		// Install time (api.Install fails when the daemon cannot bind), so a
		// second probe here would only duplicate that check.
		return requested, nil
	}
	return api.AllocateSingleGlobalPort(taken)
}

// installedGlobalDaemonPorts scans every installed manifest's daemons[] and
// returns the set of ports they declare, so the allocator skips them. A
// manifest that fails to load/parse is skipped (best-effort) — a single
// malformed dev manifest must not block a one-click install.
func installedGlobalDaemonPorts() (map[int]bool, error) {
	a := api.NewAPI()
	names, err := a.ManifestList()
	if err != nil {
		return nil, err
	}
	taken := map[int]bool{}
	for _, name := range names {
		raw, err := a.ManifestGet(name)
		if err != nil {
			continue
		}
		m, err := parseManifestPorts(raw)
		if err != nil {
			continue
		}
		for _, p := range m {
			if p > 0 {
				taken[p] = true
			}
		}
	}
	return taken, nil
}

// realServerNamePresence reports name collisions via ManifestList.
type realServerNamePresence struct{}

func (realServerNamePresence) ServerExists(name string) (bool, error) {
	names, err := api.NewAPI().ManifestList()
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// realDirectClientWriter writes the client-native entry into each selected
// client's config through the clients package (whose WriteConfigFile is the
// production SecureWriteClientConfig pipeline). http entries become a remote
// URL entry via the adapter's AddEntry; stdio entries become a
// {command,args,env} entry written into the JSON family's `mcpServers` map.
type realDirectClientWriter struct{}

func (realDirectClientWriter) WriteDirect(entry *api.MarketplaceEntry, clientNames []string) (updated []string, failed []directFailure) {
	all := clients.AllClients()
	for _, name := range sortedClientNames(clientNames) {
		c, ok := all[name]
		if !ok {
			failed = append(failed, directFailure{Client: name, Error: "unknown or unavailable client"})
			continue
		}
		if err := writeDirectEntry(c, entry); err != nil {
			failed = append(failed, directFailure{Client: name, Error: err.Error()})
			continue
		}
		updated = append(updated, name)
	}
	return updated, failed
}

// internal/gui/projects.go
//
// Per-project-GUI Phase 2a (Model B path-reparam) — READ-ONLY project-scoped
// scan. GET /api/projects/scan?root=<abs> scans a single project's LOCAL
// client config files (.mcp.json / .cursor/mcp.json / .vscode/mcp.json) and
// returns the same api.ScanResult DTO the global Servers matrix uses, but
// resolved from project paths instead of the OS-global ones.
//
// SCAN ISOLATION (the key invariant): this is a SEPARATE ScanFrom call with a
// DISJOINT ConfigPaths resolver (clients.ProjectScanConfigPaths) from the
// global scan's clients.DefaultScanConfigPaths. scan.go's ScanFrom /
// probeClientConfigPresence / DefaultScanConfigPaths / ScanOpts are UNTOUCHED —
// the global matrix's scan output stays byte-identical. Two disjoint resolvers
// make global↔project leakage structurally impossible.
//
// READ-ONLY: this handler never writes — no manifest, no intent, no
// client-config mutation. ProjectScanConfigPaths only stats the root;
// ScanFrom only reads config files + manifest names + the (best-effort)
// workspace registry.
package gui

import (
	"encoding/json"
	"net/http"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

func registerProjectsRoutes(s *Server) {
	s.mux.HandleFunc("/api/projects/scan", s.requireSameOrigin(s.projectsScanHandler))
}

// projectsScanHandler implements GET /api/projects/scan?root=<abs>.
//
// On a bad/missing/traversal root it returns 400 with a generic, leak-safe
// body: the error envelope carries a stable code and a fixed message, never
// the caller's raw root nor any resolved internal path (the precise reason is
// logged server-side via writeAPIErrorRedacted for host-side diagnosis).
func (s *Server) projectsScanHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	root := r.URL.Query().Get("root")

	// Validate + resolve the untrusted root into per-client project config
	// paths. clients.ProjectScanConfigPaths owns the path-safety contract
	// (absolute, clean, existing directory, symlink-contained, fixed RelFile
	// joins). Its error messages are already leak-safe, but we still redact at
	// the boundary so neither the resolver message nor any wrapped path reaches
	// the wire — the client gets a stable code + fixed body.
	configPaths, err := clients.ProjectScanConfigPaths(root)
	if err != nil {
		writeAPIErrorRedacted(w, err, http.StatusBadRequest, "PROJECT_ROOT_INVALID", "/api/projects/scan")
		return
	}

	// SEPARATE ScanFrom call with the project-scoped ConfigPaths. GUIPort is
	// threaded with the SAME value the global GUI scan uses (realScanner →
	// s.Port, server.go) so scan.go's classifier (IsLiveSerenaRouterURL /
	// classifyLSPEntries, scan.go) can tell a project config's LIVE serena
	// /serena/mcp (or /lsp/) router URL from a STALE old-GUI-port one: without
	// it (GUIPort 0), the live-port check degrades to port-agnostic and a stale
	// router URL is misclassified `via-hub` instead of `external`/re-migratable.
	// No ManifestDir wiring needed for a read-only project view.
	result, err := api.NewAPI().ScanFrom(api.ScanOpts{ConfigPaths: configPaths, GUIPort: s.Port()})
	if err != nil {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECT_SCAN_FAILED", "/api/projects/scan")
		return
	}

	// P2b: fold in the claude-code LOCAL scope (~/.claude.json → projects.<root>)
	// — the SEPARATE private-to-user server set (ScanResult.ProjectScope) + the
	// per-entry .mcp.json enabled/disabled reconciliation (ScanEntry.ProjectEnabled).
	// READ-ONLY: reads the fixed ~/.claude.json once, mutates the in-memory result,
	// writes nothing. `root` here is the raw (already validated by
	// ProjectScanConfigPaths above) query value; the local-scope match canonicalizes
	// it against the projects.<key> form, and it is only a comparison key against the
	// fixed home file — not a filesystem-read surface.
	if err := api.EnrichProjectClaudeLocalScope(result, root); err != nil {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECT_SCAN_FAILED", "/api/projects/scan")
		return
	}

	// Reuse the global scan's sanitizer so the per-entry Raw config blobs are
	// stripped before serialization (no client-config internals on the wire).
	result = sanitizeScanResult(result)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

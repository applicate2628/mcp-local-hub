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
	//
	// realRoot is the SINGLE symlink-resolved root the scan keys off (its
	// EvalSymlinks output). We thread it — not the raw query `root` — into the
	// claude-local lookup below so a symlinked project root is matched against
	// ~/.claude.json under the SAME real path the scan used (Claude Code writes
	// projects.<key> at the real path; the unresolved root would silently miss).
	realRoot, configPaths, err := clients.ProjectScanConfigPaths(root)
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
	// writes nothing. We pass `realRoot` — the symlink-resolved root the scan keyed
	// off — NOT the raw query value, so a symlinked project root is matched against
	// the projects.<key> form Claude Code writes (the real path). The lookup
	// canonicalizes it for comparison and it is only a comparison key against the
	// fixed home file — not a filesystem-read surface.
	if err := api.EnrichProjectClaudeLocalScope(result, realRoot); err != nil {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECT_SCAN_FAILED", "/api/projects/scan")
		return
	}

	// P3a finding 5 (bot PR #433): preserve a RE-ENABLE value source for
	// object-member project substrates (cursor/vscode/claude-code Project)
	// BEFORE the sanitizer nils Raw, so the P3b frontend can echo the full
	// member value back on a re-enable toggle. Copies Raw → ToggleValue only for
	// object-member-scope clients; no new surface (the same verbatim member the
	// scan already read), no resolved secrets (Raw carries secret:<key> refs
	// verbatim).
	preserveProjectToggleValues(result)

	// Reuse the global scan's sanitizer so the per-entry Raw config blobs are
	// stripped before serialization (no client-config internals on the wire).
	result = sanitizeScanResult(result)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// preserveProjectToggleValues copies each OBJECT-MEMBER-scope client entry's
// verbatim Raw config fragment into ClientEntry.ToggleValue so it survives the
// sanitizeScanResult Raw-stripping and reaches the P3b frontend as the re-enable
// value source (bot PR #433 finding 5). It is a PROJECT-read-only enrichment —
// the GLOBAL Servers-matrix scan never calls it, so the global wire shape stays
// byte-identical (golden invariant); ToggleValue's omitempty keeps it absent
// there.
//
// Scope discrimination uses the SINGLE owner clients.ProjectToggleOwner: a
// client gets a ToggleValue only when ProjectToggleOwner(client,
// ScopeProjectObjectMember) == OwnerProjectObjectMember (cursor / vscode /
// claude-code). claude-code's LOCAL array-move scope is NOT object-member, so it
// is correctly excluded (re-enable there needs no value). The function mutates
// the in-memory result in place BEFORE sanitization; callers MUST run it before
// sanitizeScanResult (which nils Raw, the source it reads).
//
// NO-LEAK posture (finding 5): ToggleValue is the entry's own Raw — the same
// verbatim object-member fragment the project scan already read from the user's
// own config (same-origin localhost). The scan never resolves secrets, so a
// secret:<key> / ${env:...} reference is copied verbatim, never resolved.
func preserveProjectToggleValues(result *api.ScanResult) {
	if result == nil {
		return
	}
	for i := range result.Entries {
		presence := result.Entries[i].ClientPresence
		for client, ce := range presence {
			if clients.ProjectToggleOwner(client, clients.ScopeProjectObjectMember) != clients.OwnerProjectObjectMember {
				continue
			}
			if ce.Raw == nil {
				continue
			}
			ce.ToggleValue = ce.Raw
			presence[client] = ce
		}
	}
}

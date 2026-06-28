package gui

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"mcp-local-hub/internal/api"
)

// defaultMarketplaceRegistryURL re-exports the canonical catalog URL owned by
// internal/api (api.DefaultMarketplaceRegistryURL). The GUI already imports
// internal/api (LoadMarketplaceCatalog) but must NOT import internal/cli, so the
// shared lower api layer is the single owner of the literal — collapsing the
// prior bump-both-or-drift footgun across cli+gui to one bump point in api
// (work-items/bugs/2026-06-24-marketplace-url-duplication.md).
const defaultMarketplaceRegistryURL = api.DefaultMarketplaceRegistryURL

// marketplaceLister is the pin-point subset of api.LoadMarketplaceCatalog
// backing GET /api/marketplace (§10 v2b — read-only marketplace browse).
// Same Server-local-interface idiom as catalogLister so marketplace_test.go
// can swap a fake catalog loader without a live network fetch. It returns
// the parsed entries (or a best-effort empty slice on cache/fetch miss) so
// the handler never 500s the Catalog screen — a marketplace fetch failure
// is a degraded-but-usable state, not an error.
type marketplaceLister interface {
	// MarketplaceEntries returns BOTH the installable entries[] and the separate
	// top-level docs_only[] pointer rows (S4), so the browse handler can render both
	// in one round-trip. A fetch/cache miss returns (nil, nil, err) and the handler
	// degrades to empty arrays.
	MarketplaceEntries(ctx context.Context) ([]api.MarketplaceEntry, []api.DocsOnlyEntry, error)
}

// realMarketplaceLister is the production adapter for GET /api/marketplace.
// It loads the curated catalog through api.LoadMarketplaceCatalog (24h TTL,
// ETag revalidate, HTTPS-only) against the default registry URL. Per the
// best-effort contract it returns (nil, err) only so the handler can LOG
// the failure; the handler still responds 200 with an empty array so a
// transient registry outage never breaks the page.
type realMarketplaceLister struct{}

func (realMarketplaceLister) MarketplaceEntries(ctx context.Context) ([]api.MarketplaceEntry, []api.DocsOnlyEntry, error) {
	cat, _, err := api.LoadMarketplaceCatalog(ctx, defaultMarketplaceRegistryURL)
	if err != nil {
		return nil, nil, err
	}
	return cat.Entries, cat.DocsOnly, nil
}

// marketplaceRefresher is the pin-point subset backing POST
// /api/marketplace/refresh (roadmap §B). It triggers the SAME
// force-refresh the CLI `mcphub marketplace refresh` runs — an
// unconditional GET that bypasses the 24h TTL + ETag and rewrites the
// cache — then returns the refreshed entries. The interface seam lets
// marketplace_test.go exercise the handler without a live network
// fetch, the same idiom as marketplaceLister.
type marketplaceRefresher interface {
	// RefreshMarketplaceEntries returns BOTH the installable entries[] and the
	// separate top-level docs_only[] pointer rows (S4) after a force-refresh, so the
	// refresh body carries the docs_only rows too (matching the GET browse shape).
	RefreshMarketplaceEntries(ctx context.Context) ([]api.MarketplaceEntry, []api.DocsOnlyEntry, error)
}

// RefreshMarketplaceEntries reuses api.RefreshMarketplaceCatalog — the
// exact force-refresh code path the CLI `refresh` subcommand calls
// (internal/cli/marketplace.go newMarketplaceRefreshCmd →
// api.RefreshMarketplaceCatalogWithClient). It is implemented on the
// same realMarketplaceLister adapter so production wires one struct for
// both the GET (cached) and POST (force-refresh) marketplace routes.
func (realMarketplaceLister) RefreshMarketplaceEntries(ctx context.Context) ([]api.MarketplaceEntry, []api.DocsOnlyEntry, error) {
	cat, err := api.RefreshMarketplaceCatalog(ctx, defaultMarketplaceRegistryURL)
	if err != nil {
		return nil, nil, err
	}
	return cat.Entries, cat.DocsOnly, nil
}

// marketplaceEntry is the read-only wire shape for one marketplace catalog
// row exposed to the Catalog screen. It projects the fields the browse view
// renders ({id, name, summary, categories, homepage, availability}) PLUS `transport` so
// the one-click-install frontend can decide between hub mode (stdio entries
// → a hub daemon) and direct mode (a client-native entry, no daemon) without
// a second round-trip. The heavier install details (command/args/url/env)
// are deliberately still omitted from the browse DTO — POST
// /api/marketplace/install re-loads the FULL api.MarketplaceEntry by id
// server-side, so they never need to cross the read wire. Defined here (not
// by serializing api.MarketplaceEntry) so the GUI HTTP contract owns its own
// JSON shape.
type marketplaceEntry struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Summary    string   `json:"summary"`
	Categories []string `json:"categories"`
	Homepage   string   `json:"homepage"`
	// Transport is the catalog entry's transport discriminator ("stdio", "http",
	// or "docs-only"). The frontend reads it to choose the install mode affordance;
	// a "docs-only" row suppresses install entirely and renders the manual-install
	// pointer instead; an unknown/empty value renders as a non-installable row.
	Transport string `json:"transport"`
	// Availability (D-3, Tier-0) is the read-only catalog-row lifecycle gate
	// ("" / "ready" / "watch" / "disabled-until-probe"). The frontend greys a
	// watch / disabled-until-probe row and labels it "probe to enable". Empty /
	// "ready" renders exactly as today. Purely additive wire field.
	Availability string `json:"availability,omitempty"`
	// ProbeState (D-3, Tier-0 — mirror-gate) is the TRI-STATE browse-time host-probe
	// verdict the backend reaches for this entry RIGHT NOW: "ready" (installable
	// now — a ready/empty row, or an inert binary-only row whose binaries resolve
	// on PATH), "inert-blocked" (provably not installable yet — the GUI greys it
	// "probe to enable"), or "inert-unknown" (carries a files[] / path-shaped
	// probe the browse path deliberately does NOT touch — the GUI still offers
	// install, and the real probe runs at the install-time gate). This REPLACES the
	// 3-state-conflating bool. Always emitted (not omitempty) so the frontend can
	// distinguish a real value from "field absent on an older backend".
	ProbeState string `json:"probe_state"`
	// ProbePasses is the DEPRECATED bool alias of ProbeState, kept for ONE release
	// so an un-regenerated older frontend bundle degrades safely: it is true iff
	// ProbeState == "ready". A frontend reading probe_passes alone treats both
	// inert-blocked and inert-unknown as "not passing" (fail-closed grey), the
	// prior conservative behavior. New frontends read probe_state. Remove next
	// release.
	ProbePasses bool `json:"probe_passes"`
}

// marketplaceDocsOnlyEntry (S4) is the read-only wire shape for ONE manual-install
// POINTER row from the catalog's separate top-level docs_only[] array. It carries
// the pointer payload the Catalog renders (id/name/summary/categories/homepage +
// the raw README link + the verbatim manual_install steps) and DELIBERATELY no
// transport/probe/install fields — a docs_only row is install-inert, so the
// frontend renders a DOCS-ONLY badge + readme link + a "view setup" block and never
// an install affordance. Defined here (not by serializing api.DocsOnlyEntry) so the
// GUI HTTP contract owns its own JSON shape.
type marketplaceDocsOnlyEntry struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Summary       string   `json:"summary"`
	Categories    []string `json:"categories"`
	Homepage      string   `json:"homepage"`
	ReadmeURL     string   `json:"readme_url,omitempty"`
	ManualInstall string   `json:"manual_install,omitempty"`
}

type marketplaceListResponse struct {
	// Entries is always a JSON array — never null — so the frontend can
	// map over it without a null guard. A fetch/cache miss is 200
	// {"entries":[]}, the same "empty is normal" posture as /api/catalog.
	Entries []marketplaceEntry `json:"entries"`
	// DocsOnly (S4) is the parallel array of manual-install POINTER rows from the
	// catalog's separate top-level docs_only[] array. Always a JSON array (never
	// null) so the frontend maps without a guard; empty when the catalog carries no
	// docs_only rows (every pre-S4 catalog).
	DocsOnly []marketplaceDocsOnlyEntry `json:"docs_only"`
}

// registerMarketplaceRoutes wires GET /api/marketplace onto the server's
// mux (§10 v2b — read-only marketplace browse). Same requireSameOrigin
// guard as the other /api/* GETs.
//
// Best-effort contract: a marketplace fetch/cache failure NEVER 500s. The
// handler logs the error server-side and returns 200 {"entries":[]} so the
// Catalog screen's marketplace section renders an empty/degraded state
// rather than breaking the whole page. This mirrors the CLAUDE.md
// requirement: "best-effort: empty array on fetch/cache miss, never 500
// the page".
func registerMarketplaceRoutes(s *Server) {
	s.mux.HandleFunc("/api/marketplace", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Bound the upstream fetch so a slow/hung registry can't stall the
		// request goroutine indefinitely. A cache hit returns immediately;
		// only a TTL-expired entry forces the network round-trip.
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		entries, docsOnly, err := s.marketplaceLister.MarketplaceEntries(ctx)
		if err != nil {
			// Best-effort: log + return empty arrays (200), NEVER 500.
			// A degraded marketplace must not break the Catalog screen.
			log.Printf("/api/marketplace: %v", err)
			entries = nil
			docsOnly = nil
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(marketplaceListResponse{
			Entries:  projectMarketplaceEntries(entries),
			DocsOnly: projectMarketplaceDocsOnly(docsOnly),
		})
	}))

	// POST /api/marketplace/refresh (roadmap §B) forces an unconditional
	// re-fetch of the catalog (bypassing the 24h TTL + ETag, rewriting the
	// cache) — the SAME force-refresh the CLI `mcphub marketplace refresh`
	// runs — then returns the refreshed entries in the SAME body shape as
	// GET /api/marketplace. It is a mutating route (it rewrites the cache),
	// so it carries the same requireSameOrigin guard as the other mutating
	// /api/* routes and only accepts POST.
	//
	// Unlike the best-effort GET (which degrades to an empty array on a
	// fetch miss), this route is an EXPLICIT operator-triggered re-fetch:
	// a refresh failure is surfaced as 500 so the operator knows the cache
	// was NOT updated, rather than silently serving an empty list.
	s.mux.HandleFunc("/api/marketplace/refresh", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Bound the upstream fetch so a slow/hung registry can't stall the
		// request goroutine indefinitely.
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		entries, docsOnly, err := s.marketplaceRefresher.RefreshMarketplaceEntries(ctx)
		if err != nil {
			// Log the raw error server-side; return a sanitized envelope so
			// the upstream fetch/network detail does not leak into the body.
			log.Printf("/api/marketplace/refresh: %v", err)
			writeAPIError(w, errors.New("marketplace refresh failed"), http.StatusInternalServerError, "MARKETPLACE_REFRESH_FAILED")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(marketplaceListResponse{
			Entries:  projectMarketplaceEntries(entries),
			DocsOnly: projectMarketplaceDocsOnly(docsOnly),
		})
	}))
}

// projectMarketplaceEntries maps api.MarketplaceEntry values onto the
// read-only browse wire shape shared by GET /api/marketplace and POST
// /api/marketplace/refresh. It projects only {id, name, summary,
// categories, homepage, transport, availability} — the heavier install-only
// command fields stay server-side — and normalizes a nil Categories to [] so
// the JSON is never null (the frontend maps without a guard). The
// returned slice is always non-nil so an empty catalog serializes as [],
// not null.
func projectMarketplaceEntries(entries []api.MarketplaceEntry) []marketplaceEntry {
	rows := make([]marketplaceEntry, 0, len(entries))
	for _, e := range entries {
		cats := e.Categories
		if cats == nil {
			cats = []string{}
		}
		// e is a fresh per-iteration value (Go 1.22+), so &e is this row's entry.
		probeState := api.MarketplaceEntryBrowseProbeState(&e)
		rows = append(rows, marketplaceEntry{
			ID:           e.ID,
			Name:         e.Name,
			Summary:      e.Summary,
			Categories:   cats,
			Homepage:     e.Homepage,
			Transport:    e.Transport,
			Availability: e.Availability,
			// PASSIVE browse-time TRI-STATE host-probe verdict. The full gate
			// (api.MarketplaceEntryProbePasses / AvailabilityAdmissionEntry) os.Stats
			// every files[] probe — fine on the operator-initiated install path, but
			// running it here, while merely SERVING the browse list, would let a
			// catalog-provided file-probe path (slow automount, UNC share) stall
			// opening/refreshing the Catalog and touch an external location before the
			// operator chooses install. So the browse projection classifies WITHOUT a
			// stat / a path LookPath: "ready" (a ready/empty row, or a binary-only
			// inert row whose binaries resolve on PATH), "inert-blocked" (provably not
			// installable yet → greyed "probe to enable"), or "inert-unknown" (carries
			// a files[]/path-shaped probe the browse path defers — the GUI still offers
			// install). The real file probe runs at install admission
			// (gui/marketplace_install.go → AvailabilityAdmissionEntry), so an
			// inert-unknown row is still installable once the operator clicks install.
			// probe_passes is the deprecated bool alias (true iff state==ready) so an
			// un-regenerated older bundle degrades safely.
			ProbeState:  string(probeState),
			ProbePasses: probeState == api.ProbeBrowseReady,
		})
	}
	return rows
}

// projectMarketplaceDocsOnly maps the catalog's separate top-level docs_only[]
// rows (S4) onto the read-only browse pointer wire shape, shared by GET
// /api/marketplace and POST /api/marketplace/refresh. It carries the pointer
// payload (id/name/summary/categories/homepage/readme/manual steps) and normalizes
// a nil Categories to [] so the JSON is never null. The returned slice is always
// non-nil so an empty (or pre-S4) catalog serializes docs_only as [], not null.
func projectMarketplaceDocsOnly(docsOnly []api.DocsOnlyEntry) []marketplaceDocsOnlyEntry {
	rows := make([]marketplaceDocsOnlyEntry, 0, len(docsOnly))
	for _, d := range docsOnly {
		cats := d.Categories
		if cats == nil {
			cats = []string{}
		}
		rows = append(rows, marketplaceDocsOnlyEntry{
			ID:            d.ID,
			Name:          d.Name,
			Summary:       d.Summary,
			Categories:    cats,
			Homepage:      d.Homepage,
			ReadmeURL:     d.ReadmeURL,
			ManualInstall: d.ManualInstall,
		})
	}
	return rows
}

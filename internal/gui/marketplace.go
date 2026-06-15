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

// defaultMarketplaceRegistryURL is the curated catalog served from this
// repo's master branch — the same single registry the CLI defaults to
// (internal/cli.DefaultMarketplaceRegistryURL). It is duplicated here as
// a GUI-local constant rather than imported from internal/cli because the
// GUI layer must not depend on the CLI command package; both reference the
// same canonical source URL (CLAUDE.md "Marketplace (G5, v0.3.0)").
const defaultMarketplaceRegistryURL = "https://raw.githubusercontent.com/applicate2628/mcp-local-hub/master/marketplace/v1/catalog.json"

// marketplaceLister is the pin-point subset of api.LoadMarketplaceCatalog
// backing GET /api/marketplace (§10 v2b — read-only marketplace browse).
// Same Server-local-interface idiom as catalogLister so marketplace_test.go
// can swap a fake catalog loader without a live network fetch. It returns
// the parsed entries (or a best-effort empty slice on cache/fetch miss) so
// the handler never 500s the Catalog screen — a marketplace fetch failure
// is a degraded-but-usable state, not an error.
type marketplaceLister interface {
	MarketplaceEntries(ctx context.Context) ([]api.MarketplaceEntry, error)
}

// realMarketplaceLister is the production adapter for GET /api/marketplace.
// It loads the curated catalog through api.LoadMarketplaceCatalog (24h TTL,
// ETag revalidate, HTTPS-only) against the default registry URL. Per the
// best-effort contract it returns (nil, err) only so the handler can LOG
// the failure; the handler still responds 200 with an empty array so a
// transient registry outage never breaks the page.
type realMarketplaceLister struct{}

func (realMarketplaceLister) MarketplaceEntries(ctx context.Context) ([]api.MarketplaceEntry, error) {
	cat, _, err := api.LoadMarketplaceCatalog(ctx, defaultMarketplaceRegistryURL)
	if err != nil {
		return nil, err
	}
	return cat.Entries, nil
}

// marketplaceRefresher is the pin-point subset backing POST
// /api/marketplace/refresh (roadmap §B). It triggers the SAME
// force-refresh the CLI `mcphub marketplace refresh` runs — an
// unconditional GET that bypasses the 24h TTL + ETag and rewrites the
// cache — then returns the refreshed entries. The interface seam lets
// marketplace_test.go exercise the handler without a live network
// fetch, the same idiom as marketplaceLister.
type marketplaceRefresher interface {
	RefreshMarketplaceEntries(ctx context.Context) ([]api.MarketplaceEntry, error)
}

// RefreshMarketplaceEntries reuses api.RefreshMarketplaceCatalog — the
// exact force-refresh code path the CLI `refresh` subcommand calls
// (internal/cli/marketplace.go newMarketplaceRefreshCmd →
// api.RefreshMarketplaceCatalogWithClient). It is implemented on the
// same realMarketplaceLister adapter so production wires one struct for
// both the GET (cached) and POST (force-refresh) marketplace routes.
func (realMarketplaceLister) RefreshMarketplaceEntries(ctx context.Context) ([]api.MarketplaceEntry, error) {
	cat, err := api.RefreshMarketplaceCatalog(ctx, defaultMarketplaceRegistryURL)
	if err != nil {
		return nil, err
	}
	return cat.Entries, nil
}

// marketplaceEntry is the read-only wire shape for one marketplace catalog
// row exposed to the Catalog screen. It projects the fields the browse view
// renders ({id, name, summary, categories, homepage}) PLUS `transport` so
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
	// Transport is the catalog entry's transport discriminator ("stdio" or
	// "http"). The frontend reads it to choose the install mode affordance;
	// an unknown/empty value renders as a non-installable row.
	Transport string `json:"transport"`
}

type marketplaceListResponse struct {
	// Entries is always a JSON array — never null — so the frontend can
	// map over it without a null guard. A fetch/cache miss is 200
	// {"entries":[]}, the same "empty is normal" posture as /api/catalog.
	Entries []marketplaceEntry `json:"entries"`
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
		entries, err := s.marketplaceLister.MarketplaceEntries(ctx)
		if err != nil {
			// Best-effort: log + return an empty array (200), NEVER 500.
			// A degraded marketplace must not break the Catalog screen.
			log.Printf("/api/marketplace: %v", err)
			entries = nil
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(marketplaceListResponse{Entries: projectMarketplaceEntries(entries)})
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
		entries, err := s.marketplaceRefresher.RefreshMarketplaceEntries(ctx)
		if err != nil {
			// Log the raw error server-side; return a sanitized envelope so
			// the upstream fetch/network detail does not leak into the body.
			log.Printf("/api/marketplace/refresh: %v", err)
			writeAPIError(w, errors.New("marketplace refresh failed"), http.StatusInternalServerError, "MARKETPLACE_REFRESH_FAILED")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(marketplaceListResponse{Entries: projectMarketplaceEntries(entries)})
	}))
}

// projectMarketplaceEntries maps api.MarketplaceEntry values onto the
// read-only browse wire shape shared by GET /api/marketplace and POST
// /api/marketplace/refresh. It projects only {id, name, summary,
// categories, homepage} — the install/transport/command fields stay
// CLI-only — and normalizes a nil Categories to [] so the JSON is never
// null (the frontend maps without a guard). The returned slice is always
// non-nil so an empty catalog serializes as [], not null.
func projectMarketplaceEntries(entries []api.MarketplaceEntry) []marketplaceEntry {
	rows := make([]marketplaceEntry, 0, len(entries))
	for _, e := range entries {
		cats := e.Categories
		if cats == nil {
			cats = []string{}
		}
		rows = append(rows, marketplaceEntry{
			ID:         e.ID,
			Name:       e.Name,
			Summary:    e.Summary,
			Categories: cats,
			Homepage:   e.Homepage,
		})
	}
	return rows
}

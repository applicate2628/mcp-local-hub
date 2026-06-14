package gui

import (
	"context"
	"encoding/json"
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

// marketplaceEntry is the read-only wire shape for one marketplace catalog
// row exposed to the Catalog screen. It projects only the fields the
// browse view renders ({id, name, summary, categories, homepage}) — the
// install/transport/command details stay CLI-only because stdio-wrapper
// generation is a `mcphub marketplace generate <id>` flow, not a GUI
// install. Defined here (not by serializing api.MarketplaceEntry) so the
// GUI HTTP contract owns its own JSON shape.
type marketplaceEntry struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Summary    string   `json:"summary"`
	Categories []string `json:"categories"`
	Homepage   string   `json:"homepage"`
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(marketplaceListResponse{Entries: rows})
	}))
}

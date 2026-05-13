// internal/cli/marketplace.go — G5 Marketplace CLI subcommands.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"CLI surface". Four leaves: search, show, generate, refresh.
//
// Output discipline (mirrors G7's codex P2 fix on PR #160): table +
// YAML + status output all go to stdout; warnings (stale-cache
// fallback, sensitive-env redaction, G6 deferral) go to stderr via
// cmd.ErrOrStderr().
//
// HTTPS-only via api.MarketplaceClientForCmd() — production returns
// the canonical client; tests inject a TLS-trusting client through
// api.InstallMarketplaceTestClientForCLI (mutex-guarded). No
// CLI-visible flag exposes the test hook (the
// --insecure-tls-for-tests idea was rejected as a footgun per
// plan v2 §Phase 4 prelude).

package cli

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

// DefaultMarketplaceRegistryURL is the curated catalog served from
// this repo's master branch. v1 ships with a single registry to
// keep trust management simple; v0.4.x can grow to multi-registry.
const DefaultMarketplaceRegistryURL = "https://raw.githubusercontent.com/applicate2628/mcp-local-hub/master/marketplace/v1/catalog.json"

func newMarketplaceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "marketplace",
		Short: "Discover MCP servers from a curated registry",
		Long: `Discover MCP servers from a curated registry and project entries
into draft mcphub manifests. The catalog defaults to:

  ` + DefaultMarketplaceRegistryURL + `

Zero auto-install side effects — drafts are printed to stdout; the
operator runs ` + "`mcphub manifest create`" + ` + ` + "`mcphub install`" + ` separately.

Subcommands:
  search [query]     List catalog entries matching query (empty = list all).
  show <id>          Print one entry's metadata block + Readme URL.
  generate <id>      Print draft manifest YAML to stdout (operator must edit).
  refresh            Force unconditional re-fetch of the catalog.`,
		// Mirror NewRootCmd: callers like the G6 deferral path
		// return an actionable error message via stderr — usage
		// banners on top of that would pollute stdout (which
		// MUST stay empty on G6 deferral per plan §"Phase 4").
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.AddCommand(newMarketplaceSearchCmd())
	c.AddCommand(newMarketplaceShowCmd())
	c.AddCommand(newMarketplaceGenerateCmd())
	c.AddCommand(newMarketplaceRefreshCmd())
	return c
}

func newMarketplaceSearchCmd() *cobra.Command {
	var registry string
	c := &cobra.Command{
		Use:   "search [query]",
		Short: "List catalog entries matching query (empty = list all)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRegistryURL(registry); err != nil {
				return err
			}
			cat, src, err := api.LoadMarketplaceCatalogWithClient(cmd.Context(), api.MarketplaceClientForCmd(), registry)
			if err != nil {
				return err
			}
			warnIfStale(cmd, src)
			q := strings.ToLower(strings.Join(args, " "))
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tTRANSPORT\tCATEGORIES\tSUMMARY")
			for i := range cat.Entries {
				e := &cat.Entries[i]
				if q != "" && !entryMatches(e, q) {
					continue
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					e.ID, e.Name, e.Transport, strings.Join(e.Categories, ","), e.Summary)
			}
			return w.Flush()
		},
	}
	c.Flags().StringVar(&registry, "registry", DefaultMarketplaceRegistryURL, "catalog URL (https:// only)")
	return c
}

func newMarketplaceShowCmd() *cobra.Command {
	var registry string
	c := &cobra.Command{
		Use:   "show <id>",
		Short: "Print one entry's metadata block + Readme URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRegistryURL(registry); err != nil {
				return err
			}
			cat, src, err := api.LoadMarketplaceCatalogWithClient(cmd.Context(), api.MarketplaceClientForCmd(), registry)
			if err != nil {
				return err
			}
			warnIfStale(cmd, src)
			for i := range cat.Entries {
				e := &cat.Entries[i]
				if e.ID != args[0] {
					continue
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "ID:         %s\n", e.ID)
				fmt.Fprintf(out, "Name:       %s\n", e.Name)
				fmt.Fprintf(out, "Transport:  %s\n", e.Transport)
				if e.Command != "" {
					cmdLine := e.Command
					if len(e.Args) > 0 {
						cmdLine += " " + strings.Join(e.Args, " ")
					}
					fmt.Fprintf(out, "Command:    %s\n", cmdLine)
				}
				if e.URL != "" {
					fmt.Fprintf(out, "URL:        %s\n", e.URL)
				}
				if e.Homepage != "" {
					fmt.Fprintf(out, "Homepage:   %s\n", e.Homepage)
				}
				if e.License != "" {
					fmt.Fprintf(out, "License:    %s\n", e.License)
				}
				if e.Summary != "" {
					fmt.Fprintf(out, "Summary:    %s\n", e.Summary)
				}
				if len(e.Categories) > 0 {
					fmt.Fprintf(out, "Categories: %s\n", strings.Join(e.Categories, ", "))
				}
				// codex r1 P1 closure: print readme_url STRING ONLY.
				// We do NOT fetch the README body — the operator can
				// open the URL in a browser. Avoids an unbounded
				// arbitrary-URL fetch from inside `show`, which would
				// otherwise require its own HTTPS-only + size-cap +
				// content-type guarding. Empty readme_url omits the
				// line entirely to keep the block tidy.
				if e.ReadmeURL != "" {
					fmt.Fprintf(out, "Readme URL: %s\n", e.ReadmeURL)
				}
				return nil
			}
			return fmt.Errorf("entry %q not found in catalog", args[0])
		},
	}
	c.Flags().StringVar(&registry, "registry", DefaultMarketplaceRegistryURL, "catalog URL (https:// only)")
	return c
}

func newMarketplaceGenerateCmd() *cobra.Command {
	var registry, workspace string
	c := &cobra.Command{
		Use:   "generate <id>",
		Short: "Print draft manifest YAML for an entry to stdout (no write side effects)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRegistryURL(registry); err != nil {
				return err
			}
			cat, src, err := api.LoadMarketplaceCatalogWithClient(cmd.Context(), api.MarketplaceClientForCmd(), registry)
			if err != nil {
				return err
			}
			warnIfStale(cmd, src)
			if workspace == "" {
				if wd, gerr := os.Getwd(); gerr == nil {
					workspace = wd
				}
			}
			for i := range cat.Entries {
				e := &cat.Entries[i]
				if e.ID != args[0] {
					continue
				}
				draft, warnings, gerr := api.GenerateDraftManifest(e, api.GenerateOpts{WorkspaceFolder: workspace})
				if gerr != nil {
					// G6-deferral path (http entry) goes through here.
					// stdout MUST stay empty; the error message is
					// the operator-facing G6 explanation.
					fmt.Fprintln(cmd.ErrOrStderr(), "WARN:", gerr)
					return gerr
				}
				// Emit sensitive-env warnings to stderr BEFORE
				// stdout — operator sees them above the YAML when
				// `mcphub marketplace generate foo > /tmp/draft.yaml`
				// is run interactively.
				for _, line := range warnings {
					fmt.Fprintln(cmd.ErrOrStderr(), "WARN:", line)
				}
				fmt.Fprint(cmd.OutOrStdout(), draft)
				return nil
			}
			return fmt.Errorf("entry %q not found in catalog", args[0])
		},
	}
	c.Flags().StringVar(&registry, "registry", DefaultMarketplaceRegistryURL, "catalog URL (https:// only)")
	c.Flags().StringVar(&workspace, "workspace", "", "value to substitute for ${workspaceFolder} placeholders (default: $PWD)")
	return c
}

func newMarketplaceRefreshCmd() *cobra.Command {
	var registry string
	c := &cobra.Command{
		Use:   "refresh",
		Short: "Force unconditional re-fetch of the catalog",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRegistryURL(registry); err != nil {
				return err
			}
			cat, err := api.RefreshMarketplaceCatalogWithClient(cmd.Context(), api.MarketplaceClientForCmd(), registry)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Refreshed catalog: %d entries.\n", len(cat.Entries))
			return nil
		},
	}
	c.Flags().StringVar(&registry, "registry", DefaultMarketplaceRegistryURL, "catalog URL (https:// only)")
	return c
}

// entryMatches is a case-insensitive substring match across the
// human-readable fields. Catalog ID + summary + categories carry
// most of the operator's search intent; we glue them with a single
// separator so a query string spanning fields ("fs sandbox") still
// matches when both tokens appear in different fields.
func entryMatches(e *api.MarketplaceEntry, q string) bool {
	hay := strings.ToLower(strings.Join([]string{
		e.ID, e.Name, e.Summary, strings.Join(e.Categories, " "),
	}, " "))
	return strings.Contains(hay, q)
}

// warnIfStale emits one stderr line when the cache fell back to
// stale data. The age hint helps the operator decide whether the
// served snapshot is acceptable for the workflow at hand.
func warnIfStale(cmd *cobra.Command, src api.MarketplaceSource) {
	if src != api.MarketplaceSourceStaleFallback {
		return
	}
	age := api.MarketplaceCacheAge()
	hours := int(age / time.Hour)
	fmt.Fprintf(cmd.ErrOrStderr(),
		"WARN: catalog fetch failed; using cached copy from %dh ago. Run `mcphub marketplace refresh` when network returns.\n",
		hours)
}

// validateRegistryURL parses the --registry value client-side and
// rejects non-https schemes with a tidier error than the lib-level
// rejection from MarketplaceFetchWithClient (which says "marketplace
// url must be https:// (got scheme \"...\")" — informative but the
// operator gets it before the lib path runs, which is friendlier
// than waiting for an HTTP layer to refuse).
func validateRegistryURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("--registry must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("--registry %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("--registry must use https:// (got scheme %q)", u.Scheme)
	}
	return nil
}

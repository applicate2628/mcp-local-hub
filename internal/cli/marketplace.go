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
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

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
				// codex deep-sec PR #163 lane 3 P2 closure: catalog
				// fields are untrusted strings — sanitize C0/C1
				// control bytes and ESC before writing to a terminal
				// so a hostile catalog cannot inject escape sequences
				// or corrupt scripts that parse the table.
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					sanitizeCatalogField(e.ID),
					sanitizeCatalogField(e.Name),
					sanitizeCatalogField(e.Transport),
					sanitizeCatalogField(strings.Join(e.Categories, ",")),
					sanitizeCatalogField(e.Summary),
				)
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
				// codex deep-sec PR #163 lane 3 P2 closure: each
				// catalog string is run through sanitizeCatalogField
				// before reaching stdout so a hostile registry cannot
				// inject ANSI/OSC escape sequences or terminal
				// control characters via name/summary/etc.
				fmt.Fprintf(out, "ID:         %s\n", sanitizeCatalogField(e.ID))
				fmt.Fprintf(out, "Name:       %s\n", sanitizeCatalogField(e.Name))
				fmt.Fprintf(out, "Transport:  %s\n", sanitizeCatalogField(e.Transport))
				if e.Command != "" {
					cmdLine := sanitizeCatalogField(e.Command)
					if len(e.Args) > 0 {
						sanArgs := make([]string, len(e.Args))
						for i, a := range e.Args {
							sanArgs[i] = sanitizeCatalogField(a)
						}
						cmdLine += " " + strings.Join(sanArgs, " ")
					}
					fmt.Fprintf(out, "Command:    %s\n", cmdLine)
				}
				if e.URL != "" {
					fmt.Fprintf(out, "URL:        %s\n", sanitizeCatalogField(e.URL))
				}
				if e.Homepage != "" {
					fmt.Fprintf(out, "Homepage:   %s\n", sanitizeCatalogField(e.Homepage))
				}
				if e.License != "" {
					fmt.Fprintf(out, "License:    %s\n", sanitizeCatalogField(e.License))
				}
				if e.Summary != "" {
					fmt.Fprintf(out, "Summary:    %s\n", sanitizeCatalogField(e.Summary))
				}
				if len(e.Categories) > 0 {
					sanCats := make([]string, len(e.Categories))
					for i, c := range e.Categories {
						sanCats[i] = sanitizeCatalogField(c)
					}
					fmt.Fprintf(out, "Categories: %s\n", strings.Join(sanCats, ", "))
				}
				// codex r6 P2 closure (PR #163): surface entry.Env so
				// operators can inspect required / suspicious env vars
				// (especially ${env:*} placeholders that `generate`
				// will leave verbatim under the sensitive-env policy)
				// at metadata-inspection time rather than discovering
				// them only when running `generate`. Key ordering is
				// deterministic so the output is reproducible across
				// invocations and parseable by scripts.
				if len(e.Env) > 0 {
					envKeys := make([]string, 0, len(e.Env))
					for k := range e.Env {
						envKeys = append(envKeys, k)
					}
					sort.Strings(envKeys)
					fmt.Fprintln(out, "Env:")
					for _, k := range envKeys {
						fmt.Fprintf(out, "  %s=%s\n",
							sanitizeCatalogField(k),
							sanitizeCatalogField(e.Env[k]),
						)
					}
				}
				// codex r1 P1 closure: print readme_url STRING ONLY.
				// We do NOT fetch the README body — the operator can
				// open the URL in a browser. Avoids an unbounded
				// arbitrary-URL fetch from inside `show`, which would
				// otherwise require its own HTTPS-only + size-cap +
				// content-type guarding. Empty readme_url omits the
				// line entirely to keep the block tidy.
				if e.ReadmeURL != "" {
					fmt.Fprintf(out, "Readme URL: %s\n", sanitizeCatalogField(e.ReadmeURL))
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
				// codex deep-sec PR #163 lane 3 P3 closure: surface
				// os.Getwd failures instead of silently letting the
				// draft carry an empty ${workspaceFolder} (which the
				// operator would not notice until the daemon ran
				// with no working directory).
				wd, gerr := os.Getwd()
				if gerr != nil {
					return fmt.Errorf("resolve --workspace default via os.Getwd: %w (pass --workspace=<path> explicitly)", gerr)
				}
				workspace = wd
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

// sanitizeCatalogField scrubs untrusted catalog string fields before
// they reach the operator's terminal. The threat model is a custom
// HTTPS registry (--registry) that smuggles ANSI/OSC escape sequences,
// terminal control characters, or whitespace tricks into name/
// summary/categories/etc. — which would corrupt the search table,
// confuse scripts parsing stdout, or inject hyperlinks.
//
// Rules:
//   - U+001B (ESC, 0x1B) → '?' (defeats CSI/OSC escape sequences).
//   - Other C0 controls (0x00-0x1F including TAB, LF, CR): replaced
//     with a single space so the tabwriter doesn't see embedded
//     \r/\n that would break row alignment.
//   - DEL (0x7F) and C1 controls (0x80-0x9F): replaced with '?'.
//   - Invalid UTF-8 bytes (raw 0x80-0xFF that don't form a valid
//     rune): replaced with '?' to avoid letting a hostile catalog
//     hide raw C1 bytes behind a UTF-8 decode failure.
//   - Everything else (printable ASCII + UTF-8 above U+009F) passes
//     through unchanged.
//
// We iterate byte-by-byte with utf8.DecodeRuneInString so invalid
// UTF-8 sequences (e.g. a raw 0x9B byte) are caught explicitly
// instead of yielding U+FFFD that the range form gives.
//
// codex deep-sec PR #163 lane 3 P2 closure.
func sanitizeCatalogField(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Raw byte that doesn't form valid UTF-8 — almost
			// certainly a smuggled control byte. Drop it.
			b.WriteByte('?')
			i++
			continue
		}
		switch {
		case r == 0x1B:
			b.WriteByte('?')
		case r < 0x20:
			b.WriteByte(' ')
		case r == 0x7F:
			b.WriteByte('?')
		case r >= 0x80 && r <= 0x9F:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
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
//
// codex r6 P1 closure (PR #163): mirrors the URL.User rejection
// done at the lib level. Even though the lib rejects, doing it
// here too gives the operator the tidier early error and prevents
// a credential-bearing URL from being logged anywhere downstream.
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
	if u.User != nil {
		return fmt.Errorf("--registry must not embed credentials (https://user:pass@host/...); marketplace fetches are unauthenticated GETs")
	}
	return nil
}

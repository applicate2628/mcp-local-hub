package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newScanCmdReal() *cobra.Command {
	var jsonOut, withProcs bool
	c := &cobra.Command{
		Use:   "scan",
		Short: "Scan client configs: which MCP servers are hub-routed, can-migrate, unknown, or per-session",
		Long: `Walk every managed client config (claude-code, codex-cli, gemini-cli,
antigravity) and classify each MCP server entry into one of five buckets:

  via-hub        — already routed through mcp-local-hub (HTTP or relay)
  can-migrate    — has a manifest but still stdio in this client;
                   'mcphub migrate' can switch it
  unknown        — stdio entry with no matching manifest under servers/
  per-session    — intentionally NOT hub-shareable (playwright, etc.)
  not-installed  — manifest exists but no client references it yet

Per-entry column encodes which clients reference the server:
  cc=<transport>  Claude Code
  cx=<transport>  Codex CLI
  gm=<transport>  Gemini CLI
  ag=<transport>  Antigravity (relay = hub-managed stdio relay)

Examples:
  mcphub scan                 # pretty table
  mcphub scan --json          # machine-readable
  mcphub scan --with-procs    # include process count per server (wmic)

See also: migrate, manifest list, install.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			result, err := a.ScanFrom(api.ScanOpts{
				ClaudeConfigPath:      filepath.Join(home, ".claude.json"),
				CodexConfigPath:       filepath.Join(home, ".codex", "config.toml"),
				GeminiConfigPath:      filepath.Join(home, ".gemini", "settings.json"),
				AntigravityConfigPath: filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
				ManifestDir:           scanManifestDir(),
				WithProcessCount:      withProcs,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}
			groups := map[string][]api.ScanEntry{}
			for _, e := range result.Entries {
				groups[e.Status] = append(groups[e.Status], e)
			}
			for _, status := range []string{"via-hub", "can-migrate", "unknown", "per-session", "not-installed"} {
				items := groups[status]
				if len(items) == 0 {
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "\n%s (%d):\n", status, len(items))
				for _, e := range items {
					procs := ""
					if withProcs && e.ProcessCount > 0 {
						procs = fmt.Sprintf("  · %d process(es)", e.ProcessCount)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "  %-25s %s%s\n", e.Name, presenceSummary(e), procs)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	c.Flags().BoolVar(&withProcs, "processes", false, "count live processes matching each server (slower; uses wmic)")
	return c
}

func presenceSummary(e api.ScanEntry) string {
	var parts []string
	for _, c := range []string{"claude-code", "codex-cli", "gemini-cli", "antigravity"} {
		if p, ok := e.ClientPresence[c]; ok {
			parts = append(parts, fmt.Sprintf("%s=%s", shortClient(c), p.Transport))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

func shortClient(c string) string {
	switch c {
	case "claude-code":
		return "cc"
	case "codex-cli":
		return "cx"
	case "gemini-cli":
		return "gm"
	case "antigravity":
		return "ag"
	}
	return c
}

// scanManifestDir returns "" to tell the api layer to use the
// production embed-first resolution path (servers.Manifests embed FS
// union on-disk defaultManifestDir). Retained as a named seam rather
// than inlining "" at every call site, so if we ever need per-install
// overrides they plug in here.
//
// Tests that want hermetic fixtures pass an explicit ManifestDir
// (typically t.TempDir()); those callers never go through this helper.
func scanManifestDir() string { return "" }

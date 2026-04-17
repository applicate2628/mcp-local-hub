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

// scanManifestDir returns path to `servers/` next to the running binary.
func scanManifestDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "servers"
	}
	return filepath.Join(filepath.Dir(exe), "servers")
}

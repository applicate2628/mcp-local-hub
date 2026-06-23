package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"

	"github.com/spf13/cobra"
)

// scanStatusBuckets is the ORDERED, EXHAUSTIVE list of scan-status buckets the
// pretty (non-JSON) `mcphub scan` output prints, one section per non-empty
// bucket. It MUST cover every status api.classify can emit — a missing bucket
// silently DROPS those rows from the human-readable output while --json still
// carries them (the bot finding #3 class: via-hub-inherited rows hidden). When
// a new ScanEntry.Status value is added, append it here too.
//
// NOTE: "external" is intentionally absent here as a PRE-EXISTING gap — it was
// added to classify() in an earlier PR but never wired into this bucket list,
// so external remotes are likewise dropped from the pretty output today. That
// is tracked separately (adjacent finding) and is out of scope for this fix,
// which closes only the via-hub-inherited drop.
var scanStatusBuckets = []string{
	"via-hub",
	"via-hub-inherited",
	"can-migrate",
	"unknown",
	"per-session",
	"not-installed",
}

// renderScanGroups writes the pretty (non-JSON) bucketed scan output: one
// "<status> (<count>):" section per non-empty bucket in scanStatusBuckets
// order, each listing its servers with a per-client presence summary. Extracted
// as a pure function (writer + result + flag) so the bucket coverage is unit-
// testable without the OS-config-path plumbing the cobra RunE resolves.
func renderScanGroups(w io.Writer, result *api.ScanResult, withProcs bool) {
	groups := map[string][]api.ScanEntry{}
	for _, e := range result.Entries {
		groups[e.Status] = append(groups[e.Status], e)
	}
	for _, status := range scanStatusBuckets {
		items := groups[status]
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s (%d):\n", status, len(items))
		for _, e := range items {
			procs := ""
			if withProcs && e.ProcessCount > 0 {
				procs = fmt.Sprintf("  · %d process(es)", e.ProcessCount)
			}
			fmt.Fprintf(w, "  %-25s %s%s\n", e.Name, presenceSummary(e), procs)
		}
	}
}

func newScanCmdReal() *cobra.Command {
	var jsonOut, withProcs bool
	c := &cobra.Command{
		Use:   "scan",
		Short: "Scan client configs: which MCP servers are hub-routed, can-migrate, unknown, or per-session",
		Long: `Walk every managed client config (claude-code, codex-cli, cursor,
vscode, gemini-cli, qwen-cli, antigravity) and classify each MCP server entry
into one of these buckets:

  via-hub            — already routed through mcp-local-hub (HTTP or relay)
  via-hub-inherited  — hub-routed but READ-ONLY: the entry is inherited from a
                       config layer mcphub never wrote (e.g. the ~/.claude.json
                       mcpServers import, or a lower config.json layer in a
                       multi-layer client). mcphub cannot demigrate it — edit
                       the source config to remove it.
  can-migrate        — has a manifest but still stdio in this client;
                       'mcphub migrate' can switch it
  unknown            — stdio entry with no matching manifest under servers/
  per-session        — intentionally NOT hub-shareable (playwright, etc.)
  not-installed      — manifest exists but no client references it yet

Per-entry column encodes which clients reference the server:
  cc=<transport>  Claude Code
  cx=<transport>  Codex CLI
  cu=<transport>  Cursor
  vs=<transport>  VS Code
  gm=<transport>  Gemini CLI
  qw=<transport>  Qwen CLI
  ag=<transport>  Antigravity (relay = hub-managed stdio relay)

Examples:
  mcphub scan                 # pretty table
  mcphub scan --json          # machine-readable
  mcphub scan --with-procs    # include process count per server (wmic)

See also: migrate, manifest list, install.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			result, err := a.ScanFrom(api.ScanOpts{
				// §9.2 drift-prevention: the per-client config-path set is
				// DERIVED from the canonical registry
				// (api.DefaultScanConfigPaths → clients.SupportedClientNames
				// + ConfigPathForName), identical to the api.Scan production
				// path. A new registry client is scanned by `mcphub scan`
				// automatically with ZERO edits here — closing the §9.2
				// drift where this call site had its own hand-listed copy of
				// the 15 named fields that fell out of lockstep with the GUI/
				// API surface every time a client was added.
				ConfigPaths:      api.DefaultScanConfigPaths(),
				ManifestDir:      scanManifestDir(),
				WithProcessCount: withProcs,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}
			renderScanGroups(cmd.OutOrStdout(), result, withProcs)
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	c.Flags().BoolVar(&withProcs, "processes", false, "count live processes matching each server (slower; uses wmic)")
	return c
}

func presenceSummary(e api.ScanEntry) string {
	var parts []string
	for _, c := range clients.SupportedClientNames() {
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
	case "cursor":
		return "cu"
	case "vscode":
		return "vs"
	case "gemini-cli":
		return "gm"
	case "qwen-cli":
		return "qw"
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

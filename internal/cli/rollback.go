package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"

	"github.com/spf13/cobra"
)

func newRollbackCmdReal() *cobra.Command {
	var original bool
	c := &cobra.Command{
		Use:   "rollback",
		Short: "Restore the latest mcp-local-hub backup for each client",
		Long: `Restore client config files (claude-code / codex-cli / gemini-cli /
antigravity) from their most recent '.bak-mcp-local-hub-<timestamp>'
sibling. Useful for 'I just installed and it broke something, revert
everything' workflows.

Two modes:
  Default:    restore the LATEST timestamped backup (pre-last-install state)
  --original: restore the PRISTINE '-original' sentinel (pre-any-install-ever
              state — the config as it was the very first time mcphub
              touched it). The sentinel is written exactly once per client,
              on the first-ever backup, and never overwritten.

Examples:
  mcphub rollback              # undo the most recent install/uninstall
  mcphub rollback --original   # nuclear option: go back to day 0

What rollback does NOT do:
  - Delete backup files (they remain on disk — inspect with 'backups list')
  - Stop running daemons (use 'stop --all' separately if needed)
  - Remove scheduler tasks (use 'uninstall' for that)

After rollback, 'claude mcp list' / equivalent should show the pre-install
state. Most MCP clients cache the config at startup, so restart the client
session to see the restored config.

See also: backups list, backups show, uninstall.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if original {
				a := api.NewAPI()
				results, err := a.RollbackOriginal()
				if err != nil {
					return err
				}
				for _, r := range results {
					if r.Err != "" {
						fmt.Fprintf(cmd.OutOrStderr(), "\u2717 %s: %s\n", r.Client, r.Err)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "\u2713 Restored %s \u2192 %s\n", r.Client, r.Restored)
					}
				}
				return nil
			}
			allClients := clients.AllClients()
			restored := 0
			for name, c := range allClients {
				if !c.Exists() {
					continue
				}
				bak, err := findLatestBackup(c.ConfigPath())
				if err != nil {
					cmd.Printf("\u26a0 %s: %v\n", name, err)
					continue
				}
				if bak == "" {
					cmd.Printf("  %s: no backup found, skipping\n", name)
					continue
				}
				if err := c.Restore(bak); err != nil {
					cmd.Printf("\u26a0 %s restore: %v\n", name, err)
					continue
				}
				cmd.Printf("\u2713 %s restored from %s\n", name, bak)
				restored++
			}
			cmd.Printf("\nRolled back %d clients. Scheduler tasks untouched \u2014 run `mcp uninstall --server <name>` for each to remove tasks.\n", restored)
			return nil
		},
	}
	c.Flags().BoolVar(&original, "original", false, "restore from the pristine (first-ever) backup rather than the most recent")
	return c
}

// findLatestBackup locates the newest timestamped
// `<configPath>.bak-mcp-local-hub-YYYYMMDD-HHMMSS` sibling. The pristine
// sentinel `<configPath>.bak-mcp-local-hub-original` written on the very
// first backup shares the same prefix but must NOT be returned here —
// `--original` / RollbackOriginal is the only caller that should see it.
// Without the exclusion the lexicographic sort would rank "original"
// (letters, ASCII ≥97) after every digit-prefixed timestamp and
// findLatestBackup would silently hand back the pristine sentinel.
func findLatestBackup(configPath string) (string, error) {
	dir := filepath.Dir(configPath)
	base := filepath.Base(configPath) + ".bak-mcp-local-hub-"
	const sentinel = "original"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var backups []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		if name[len(base):] == sentinel {
			continue
		}
		backups = append(backups, filepath.Join(dir, name))
	}
	if len(backups) == 0 {
		return "", nil
	}
	sort.Strings(backups) // lexicographic == chronological due to timestamp format
	return backups[len(backups)-1], nil
}

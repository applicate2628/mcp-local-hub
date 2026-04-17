package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mcp-local-hub/internal/clients"

	"github.com/spf13/cobra"
)

func newRollbackCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback",
		Short: "Restore the latest mcp-local-hub backup for each client",
		RunE: func(cmd *cobra.Command, args []string) error {
			allClients := clients.AllClients()
			restored := 0
			for name, c := range allClients {
				if !c.Exists() {
					continue
				}
				bak, err := findLatestBackup(c.ConfigPath())
				if err != nil {
					cmd.Printf("⚠ %s: %v\n", name, err)
					continue
				}
				if bak == "" {
					cmd.Printf("  %s: no backup found, skipping\n", name)
					continue
				}
				if err := c.Restore(bak); err != nil {
					cmd.Printf("⚠ %s restore: %v\n", name, err)
					continue
				}
				cmd.Printf("✓ %s restored from %s\n", name, bak)
				restored++
			}
			cmd.Printf("\nRolled back %d clients. Scheduler tasks untouched — run `mcp uninstall --server <name>` for each to remove tasks.\n", restored)
			return nil
		},
	}
}

// findLatestBackup locates the newest `<configPath>.bak-mcp-local-hub-*` sibling.
func findLatestBackup(configPath string) (string, error) {
	dir := filepath.Dir(configPath)
	base := filepath.Base(configPath) + ".bak-mcp-local-hub-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var backups []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), base) {
			backups = append(backups, filepath.Join(dir, e.Name()))
		}
	}
	if len(backups) == 0 {
		return "", nil
	}
	sort.Strings(backups) // lexicographic == chronological due to timestamp format
	return backups[len(backups)-1], nil
}

func _unused_error_wrap(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("rollback: %w", err)
}

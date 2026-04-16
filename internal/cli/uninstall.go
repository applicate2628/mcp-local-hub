package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"

	"github.com/spf13/cobra"
)

func newUninstallCmdReal() *cobra.Command {
	var server string
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove an installed MCP server (scheduler + client bindings)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			manifestPath := filepath.Join("servers", server, "manifest.yaml")
			f, err := os.Open(manifestPath)
			if err != nil {
				return err
			}
			defer f.Close()
			m, err := config.ParseManifest(f)
			if err != nil {
				return err
			}
			sch, err := scheduler.New()
			if err != nil {
				return err
			}
			// Delete all tasks that begin with our prefix.
			prefix := "mcp-local-hub-" + m.Name
			tasks, err := sch.List(prefix)
			if err != nil {
				return err
			}
			for _, t := range tasks {
				if err := sch.Delete(t.Name); err != nil {
					cmd.Printf("⚠ delete %s: %v\n", t.Name, err)
				} else {
					cmd.Printf("✓ Deleted task: %s\n", t.Name)
				}
			}
			// Remove client entries.
			allClients := mustAllClients()
			for _, b := range m.ClientBindings {
				client := allClients[b.Client]
				if client == nil || !client.Exists() {
					continue
				}
				if err := client.RemoveEntry(m.Name); err != nil {
					cmd.Printf("⚠ remove %s from %s: %v\n", m.Name, b.Client, err)
					continue
				}
				cmd.Printf("✓ Removed %s from %s\n", m.Name, b.Client)
			}
			cmd.Println("Uninstall complete. Client config backups (.bak-mcp-local-hub-*) remain on disk.")
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	return c
}

package cli

import (
	"encoding/json"
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newBackupsCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "backups",
		Short: "List, clean, or show client config backups",
	}
	root.AddCommand(newBackupsListCmd())
	root.AddCommand(newBackupsCleanCmd())
	root.AddCommand(newBackupsShowCmd())
	return root
}

func newBackupsListCmd() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List all client config backups with timestamps and sizes",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			list, err := a.BackupsList()
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(list)
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No backups found.")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-14s %-14s %-10s %-10s %s\n", "CLIENT", "KIND", "SIZE(B)", "MODIFIED", "PATH")
			for _, b := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%-14s %-14s %-10d %-10s %s\n",
					b.Client, b.Kind, b.SizeByte, b.ModTime.Format("01-02 15:04"), b.Path)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}

func newBackupsCleanCmd() *cobra.Command {
	var keep int
	c := &cobra.Command{
		Use:   "clean",
		Short: "Remove old timestamped backups, keeping only the N most recent per client",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			removed, err := a.BackupsClean(keep)
			if err != nil {
				return err
			}
			if len(removed) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing to clean.")
				return nil
			}
			for _, p := range removed {
				fmt.Fprintf(cmd.OutOrStdout(), "\u2713 Removed %s\n", p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n%d file(s) removed.\n", len(removed))
			return nil
		},
	}
	c.Flags().IntVar(&keep, "keep", 5, "number of most recent timestamped backups to retain per client")
	return c
}

func newBackupsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <path>",
		Short: "Print the contents of a backup file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			content, err := a.BackupShow(args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), content)
			return nil
		},
	}
}

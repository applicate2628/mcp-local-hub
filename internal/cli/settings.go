package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newSettingsCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "settings",
		Short: "Read/write GUI preferences (theme, shell, default-home, etc.)",
	}
	root.AddCommand(newSettingsListCmd())
	root.AddCommand(newSettingsGetCmd())
	root.AddCommand(newSettingsSetCmd())
	return root
}

func newSettingsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print all current settings as key=value pairs",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			all, err := a.SettingsList()
			if err != nil {
				return err
			}
			if len(all) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no settings yet — defaults apply)")
				return nil
			}
			for k, v := range all {
				fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", k, v)
			}
			return nil
		},
	}
}

func newSettingsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print the value for a setting",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			val, err := a.SettingsGet(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), val)
			return nil
		},
	}
}

func newSettingsSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Write a setting value",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			if err := a.SettingsSet(args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s=%s\n", args[0], args[1])
			return nil
		},
	}
}

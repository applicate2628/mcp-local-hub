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
		Long: `Manage persistent key/value preferences under
%LOCALAPPDATA%\mcp-local-hub\gui-preferences.yaml (or equivalent XDG path).

Schema is authoritative in the Go registry (internal/api/settings_registry.go).
Keys, types, defaults, and validation rules come from there.

Subcommands:
  settings list      # all known settings, grouped by section
  settings get <k>   # print one value (registry default if unset)
  settings set <k> <v>  # write one validated value`,
	}
	root.AddCommand(newSettingsListCmd())
	root.AddCommand(newSettingsGetCmd())
	root.AddCommand(newSettingsSetCmd())
	return root
}

func newSettingsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print all known settings, grouped by section",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			values, err := a.SettingsList()
			if err != nil {
				return err
			}
			// Group by section, in registry order.
			currentSection := ""
			for _, def := range api.SettingsRegistry {
				if def.Section != currentSection {
					if currentSection != "" {
						fmt.Fprintln(cmd.OutOrStdout())
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s:\n", def.Section)
					currentSection = def.Section
				}
				if def.Type == api.TypeAction {
					marker := ""
					if def.Deferred {
						marker = "  [deferred — coming in A4-b]"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "  %s = <action>%s\n", def.Key, marker)
					continue
				}
				v, has := values[def.Key]
				if !has {
					v = def.Default
				}
				marker := ""
				if def.Deferred {
					marker = "  [deferred]"
				}
				if def.Key == "gui_server.port" {
					marker = "  [restart required]"
				}
				if v == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s = <empty>  (default: %q)%s\n", def.Key, def.Default, marker)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s = %s  (default: %s)%s\n", def.Key, v, def.Default, marker)
				}
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
			def := lookupRegistry(args[0])
			if def == nil {
				return fmt.Errorf("unknown setting %s", args[0])
			}
			if def.Type == api.TypeAction {
				return fmt.Errorf("%s is an action; use 'mcp settings invoke' (coming in A4-b)", def.Key)
			}
			a := api.NewAPI()
			// Use def.Key (canonical) so legacy aliases like "theme" correctly
			// fetch "appearance.theme" from the persisted YAML (Codex r13 P2).
			val, err := a.SettingsGet(def.Key)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), val)
			if def.Deferred {
				fmt.Fprintf(cmd.ErrOrStderr(), "[deferred — this field is reserved for A4-b]\n")
			}
			return nil
		},
	}
}

func newSettingsSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Write a setting value (validated against registry)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			value := args[1]
			def := lookupRegistry(args[0])
			if def == nil {
				return fmt.Errorf("unknown setting %s", args[0])
			}
			if def.Type == api.TypeAction {
				return fmt.Errorf("cannot set action key %s", def.Key)
			}
			a := api.NewAPI()
			// Use def.Key (canonical) so legacy aliases like "shell" correctly
			// write "appearance.shell" to the persisted YAML (Codex r13 P2).
			if err := a.SettingsSet(def.Key, value); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s=%s\n", def.Key, value)
			if def.Deferred {
				fmt.Fprintf(cmd.ErrOrStderr(), "setting accepted; this field is deferred to A4-b and has no effect yet\n")
			}
			return nil
		},
	}
}

// lookupRegistry returns a pointer into api.SettingsRegistry for key, or nil.
// Codex PR #20 r13 P2: resolves pre-A4 legacy key names (theme, shell,
// default-home) to their canonical equivalents before lookup, so callers
// that use def.Key for downstream API calls automatically operate on the
// canonical name without any additional translation.
func lookupRegistry(key string) *api.SettingDef {
	canonical := api.ResolveLegacyKey(key)
	for i := range api.SettingsRegistry {
		if api.SettingsRegistry[i].Key == canonical {
			return &api.SettingsRegistry[i]
		}
	}
	return nil
}

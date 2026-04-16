package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/secrets"

	"github.com/spf13/cobra"
)

// Repo-relative paths — resolved at every call so the binary is portable.
func defaultKeyPath() string   { return filepath.Join(".", ".age-key") }
func defaultVaultPath() string { return filepath.Join(".", "secrets.age") }

func newSecretsCmdReal() *cobra.Command {
	root := &cobra.Command{Use: "secrets", Short: "Manage encrypted secrets"}
	root.AddCommand(newSecretsInitCmd())
	root.AddCommand(newSecretsSetCmd())
	root.AddCommand(newSecretsGetCmd())
	root.AddCommand(newSecretsListCmd())
	root.AddCommand(newSecretsDeleteCmd())
	return root
}

func newSecretsInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Generate identity and empty vault",
		RunE: func(cmd *cobra.Command, args []string) error {
			keyPath := defaultKeyPath()
			vaultPath := defaultVaultPath()
			if err := secrets.InitVault(keyPath, vaultPath); err != nil {
				return err
			}
			cmd.Printf("✓ Wrote %s (keep safe, gitignored)\n", keyPath)
			cmd.Printf("✓ Wrote %s (safe to commit; encrypted)\n", vaultPath)
			return nil
		},
	}
}

func newSecretsSetCmd() *cobra.Command {
	var valueFlag string
	var fromStdin bool
	c := &cobra.Command{
		Use:   "set <key>",
		Short: "Create or replace a secret value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			var value string
			switch {
			case valueFlag != "":
				value = valueFlag
			case fromStdin:
				b, err := readAllStdin()
				if err != nil {
					return err
				}
				value = strings.TrimRight(string(b), "\r\n")
			default:
				// Interactive prompt with hidden input.
				v, err := promptHidden(cmd.ErrOrStderr(), "Enter value for "+key+": ")
				if err != nil {
					return err
				}
				value = v
			}
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			if err := v.Set(key, value); err != nil {
				return err
			}
			cmd.Printf("✓ Stored %s\n", key)
			return nil
		},
	}
	c.Flags().StringVar(&valueFlag, "value", "", "provide value on command line (non-interactive)")
	c.Flags().BoolVar(&fromStdin, "from-stdin", false, "read value from stdin")
	return c
}

func newSecretsGetCmd() *cobra.Command {
	var show bool
	c := &cobra.Command{
		Use:   "get <key>",
		Short: "Retrieve a secret (clipboard by default)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			val, err := v.Get(args[0])
			if err != nil {
				return err
			}
			if show {
				cmd.Println(val)
				return nil
			}
			if err := copyToClipboard(val); err != nil {
				return fmt.Errorf("clipboard: %w (use --show to print instead)", err)
			}
			cmd.Printf("✓ Copied %s to clipboard\n", args[0])
			return nil
		},
	}
	c.Flags().BoolVar(&show, "show", false, "print value to stdout instead of clipboard")
	return c
}

func newSecretsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List secret keys (not values)",
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			keys := v.List()
			if len(keys) == 0 {
				cmd.Println("(vault is empty)")
				return nil
			}
			for _, k := range keys {
				cmd.Println(k)
			}
			return nil
		},
	}
}

func newSecretsDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <key>",
		Short: "Remove a secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			if err := v.Delete(args[0]); err != nil {
				return err
			}
			cmd.Printf("✓ Deleted %s\n", args[0])
			return nil
		},
	}
}

func readAllStdin() ([]byte, error) {
	r := bufio.NewReader(os.Stdin)
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			if errors.Is(err, os.ErrInvalid) || err.Error() == "EOF" {
				break
			}
			return out, nil
		}
	}
	return out, nil
}

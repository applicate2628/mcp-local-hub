package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/secrets"

	"github.com/spf13/cobra"
)

// Secret-path helpers moved to internal/secrets/paths.go so non-cli
// packages (e.g. api.Preflight) can share the same resolution.
// Keep package-local aliases so call-site diffs stay minimal.

func defaultKeyPath() string   { return secrets.DefaultKeyPath() }
func defaultVaultPath() string { return secrets.DefaultVaultPath() }

func newSecretsCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "secrets",
		Short: "Manage encrypted secrets",
		Long: `Manage the age-encrypted key/value vault used by manifests to inject
environment variables at daemon startup (e.g. 'env: WOLFRAM_APP_ID:
secret:wolfram_app_id' in wolfram's manifest).

Storage locations (per-user, OS-canonical — independent of repo/binary):
  Windows:  %LOCALAPPDATA%\mcp-local-hub\{.age-key,secrets.age}
  Linux:    $XDG_DATA_HOME/mcp-local-hub/...
  macOS:    ~/Library/Application Support/mcp-local-hub/...

.age-key is your private identity (like an SSH private key). Lose it,
lose access. Copy via password manager / encrypted USB / trusted scp
when moving to a new machine.

Subcommands:
  secrets init                      # generate .age-key + empty secrets.age
  secrets set <key> --value <val>   # add or update a secret
  secrets get <key>                 # print value (clipboard by default)
  secrets get <key> --show          # print to stdout
  secrets list                      # list keys (not values)
  secrets delete <key>              # remove a key
  secrets edit                      # open decrypted vault in $EDITOR
  secrets migrate --from-client X   # scan client configs for API keys,
                                    # interactively import into vault

Manifest env-reference prefixes:
  secret:KEY   — look up in encrypted vault (this)
  file:KEY     — look up in config.local.yaml (gitignored)
  $VAR         — read OS environment variable
  anything-else — literal value

See also: install (fails preflight if a secret: reference is missing).`,
	}
	root.AddCommand(newSecretsInitCmd())
	root.AddCommand(newSecretsSetCmd())
	root.AddCommand(newSecretsGetCmd())
	root.AddCommand(newSecretsListCmd())
	root.AddCommand(newSecretsDeleteCmd())
	root.AddCommand(newSecretsEditCmd())
	root.AddCommand(newSecretsMigrateCmd())
	return root
}

func newSecretsInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Generate identity and empty vault",
		RunE: func(cmd *cobra.Command, args []string) error {
			keyPath := defaultKeyPath()
			vaultPath := defaultVaultPath()
			// Ensure the parent directory exists. On a fresh machine the
			// canonical %LOCALAPPDATA%\mcp-local-hub\ (or XDG equivalent)
			// may not exist yet; 0700 matches .ssh convention for keys.
			if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
				return fmt.Errorf("create secret dir: %w", err)
			}
			if err := secrets.InitVault(keyPath, vaultPath); err != nil {
				return err
			}
			cmd.Printf("✓ Wrote %s (private, never transfer via git)\n", keyPath)
			cmd.Printf("✓ Wrote %s (encrypted; transfer via password manager or secure channel)\n", vaultPath)
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

func newSecretsEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open decrypted vault in $EDITOR and re-encrypt on save",
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			yamlBytes, err := v.ExportYAML()
			if err != nil {
				return err
			}
			// Temp file lives INSIDE the user-private UserDataDir
			// (%LOCALAPPDATA%\mcp-local-hub on Windows, $XDG_DATA_HOME
			// or ~/.local/share/mcp-local-hub on Unix). The old
			// implementation used os.CreateTemp("", ...) which picks
			// the system-global temp — /tmp on Unix (world-readable
			// in some distros) or %TEMP% on Windows (typically
			// user-private but shared across apps). Moving the file
			// into the app's own data dir keeps it on the same ACL
			// boundary as secrets.age itself.
			editDir := secrets.UserDataDir()
			if err := os.MkdirAll(editDir, 0o700); err != nil {
				return fmt.Errorf("create edit dir: %w", err)
			}
			tmp, err := os.CreateTemp(editDir, "mcp-secrets-*.yaml")
			if err != nil {
				return err
			}
			tmpPath := tmp.Name()
			defer func() {
				// Secure wipe sized to the actual file length (the previous
				// implementation overwrote only the first 4 KB; a larger
				// edited vault would leak every byte past that). Grows as
				// needed, single syscall, then delete.
				if st, err := os.Stat(tmpPath); err == nil {
					if f, err := os.OpenFile(tmpPath, os.O_WRONLY, 0o600); err == nil {
						n := st.Size()
						const chunk = 64 * 1024
						buf := make([]byte, chunk)
						for remaining := n; remaining > 0; {
							w := int64(chunk)
							if remaining < w {
								w = remaining
							}
							_, _ = f.Write(buf[:w])
							remaining -= w
						}
						_ = f.Sync()
						f.Close()
					}
				}
				_ = os.Remove(tmpPath)
			}()
			if _, err := tmp.Write(yamlBytes); err != nil {
				tmp.Close()
				return err
			}
			tmp.Close()

			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "notepad" // Windows fallback
			}
			c := exec.Command(editor, tmpPath)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("editor: %w", err)
			}
			updated, err := os.ReadFile(tmpPath)
			if err != nil {
				return err
			}
			if err := v.ImportYAML(updated); err != nil {
				return err
			}
			cmd.Println("✓ Re-encrypted secrets.age")
			return nil
		},
	}
}

func newSecretsMigrateCmd() *cobra.Command {
	var fromClient string
	c := &cobra.Command{
		Use:   "migrate",
		Short: "Import hardcoded secrets from a client config",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := clientConfigPath(fromClient)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			candidates := secrets.ScanConfigText(string(data))
			if len(candidates) == 0 {
				cmd.Println("No candidates found.")
				return nil
			}
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			in := bufio.NewReader(os.Stdin)
			imported := 0
			for _, cand := range candidates {
				cmd.Printf("Found %s = %s (from %s)\n", cand.Key, maskValue(cand.Value), path)
				cmd.Print("Import? [y/N]: ")
				line, _ := in.ReadString('\n')
				line = strings.TrimSpace(strings.ToLower(line))
				if line == "y" || line == "yes" {
					if err := v.Set(cand.Key, cand.Value); err != nil {
						return err
					}
					imported++
				}
			}
			cmd.Printf("✓ Imported %d secrets. Original file NOT modified — run `mcp install` to apply.\n", imported)
			return nil
		},
	}
	c.Flags().StringVar(&fromClient, "from-client", "", "client name: claude-code | codex-cli | gemini-cli | antigravity")
	_ = c.MarkFlagRequired("from-client")
	return c
}

func clientConfigPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch name {
	case "claude-code":
		return filepath.Join(home, ".claude.json"), nil
	case "codex-cli":
		return filepath.Join(home, ".codex", "config.toml"), nil
	case "gemini-cli":
		return filepath.Join(home, ".gemini", "settings.json"), nil
	case "antigravity":
		return filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"), nil
	default:
		return "", fmt.Errorf("unknown client %q (expected claude-code | codex-cli | gemini-cli | antigravity)", name)
	}
}

func maskValue(v string) string {
	if len(v) <= 4 {
		return "***"
	}
	return v[:2] + strings.Repeat("*", len(v)-4) + v[len(v)-2:]
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

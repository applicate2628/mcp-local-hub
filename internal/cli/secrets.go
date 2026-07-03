package cli

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
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
  secrets init                          # generate .age-key + empty secrets.age
  secrets set <key>                     # interactive prompt (hidden input)
  secrets set <key> --from-stdin        # read value from stdin (scripts/pipes)
  secrets get <key>                     # print value (clipboard by default)
  secrets get <key> --show              # print to stdout
  secrets list                          # list keys (not values)
  secrets delete <key>                  # remove a key
  secrets edit                          # open decrypted vault in $EDITOR
  secrets migrate --from-client X       # scan client configs for API keys,
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
			// Flock so a concurrent GUI/api SecretsInit or set cannot
			// interleave with this fresh-vault create (cross-process race;
			// the api layer holds the same flock via secrets.WithVaultLock).
			if err := secrets.WithVaultLock(vaultPath, func() error {
				return secrets.InitVault(keyPath, vaultPath)
			}); err != nil {
				return err
			}
			cmd.Printf("✓ Wrote %s (private, never transfer via git)\n", keyPath)
			cmd.Printf("✓ Wrote %s (encrypted; transfer via password manager or secure channel)\n", vaultPath)
			return nil
		},
	}
}

func newSecretsSetCmd() *cobra.Command {
	var fromStdin bool
	c := &cobra.Command{
		Use:   "set <key>",
		Short: "Create or replace a secret value",
		Long: `Create or replace a secret value in the encrypted vault.

By default the value is read from an interactive hidden prompt so it
never appears on the command line, in shell history, or in process
listings (ps/wmic). For non-interactive use, pipe the value to stdin
with --from-stdin:

  printf '%s' "$VAL" | mcphub secrets set my_key --from-stdin

The previous --value flag was removed because argv-delivered secrets
leak into shell history files (~/.bash_history, PSReadLine), process
listings visible to other local users, and the running process's own
environment after argument parsing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			var value string
			switch {
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
			// Flock the OpenVault → Set(save) RMW (value was already obtained
			// from the prompt/stdin above, so the locked section is tight and
			// non-interactive). The api layer holds the same flock, so a GUI
			// write and this CLI write cannot lose each other's update.
			if err := secrets.WithVaultLock(defaultVaultPath(), func() error {
				v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
				if err != nil {
					return err
				}
				return v.Set(key, value)
			}); err != nil {
				return err
			}
			// P2.4 audit trail: emit ONLY after the committed write above (a
			// failed/aborted set returned in the err branch and never reaches
			// here). Routes through the SHARED api audit owner so this CLI path
			// records the exact same value-free secret-rotated event the GUI
			// SecretsRotate path does — `value` is never passed in.
			api.EmitSecretRotatedAudit(key, map[string]any{"source": "cli"})
			cmd.Printf("✓ Stored %s\n", key)
			return nil
		},
	}
	c.Flags().BoolVar(&fromStdin, "from-stdin", false, "read value from stdin (use this for scripts/pipes)")
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
			// Flock the OpenVault → Delete(save) RMW (cross-process: the api
			// layer holds the same flock via secrets.WithVaultLock).
			if err := secrets.WithVaultLock(defaultVaultPath(), func() error {
				v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
				if err != nil {
					return err
				}
				return v.Delete(args[0])
			}); err != nil {
				return err
			}
			// P2.4 audit trail: emit ONLY after the committed delete above (a
			// failed delete returned in the err branch and never reaches here).
			// Routes through the SHARED api audit owner so this CLI path records
			// the same value-free secret-deleted event the GUI SecretsDelete
			// path does.
			api.EmitSecretDeletedAudit(args[0], map[string]any{"source": "cli"})
			cmd.Printf("✓ Deleted %s\n", args[0])
			return nil
		},
	}
}

// secureCreateOwnerOnlyFileFn is the owner-only secure-create used by
// `mcphub secrets edit` to write the decrypted vault into the temp the
// operator's editor opens. Production = api.SecureCreateOwnerOnlyFile,
// which installs a PROTECTED allowlist-only DACL (Windows) / 0600
// O_NOFOLLOW (POSIX) at create time so the cleartext is owner-only
// regardless of the parent dir's ACL — NEVER os.CreateTemp, which on
// Windows would inherit a broadened %LOCALAPPDATA% ACL. It is a seam
// ONLY so tests can spy that the edit temp routes through the hardened
// primitive; production callers never reassign it.
var secureCreateOwnerOnlyFileFn = api.SecureCreateOwnerOnlyFile

// secretsEditTempMaxNameAttempts bounds the crypto/rand unique-name
// retry loop below. With 128 bits of entropy a collision is
// astronomically unlikely; the bound exists only so a pathological
// condition (e.g. a directory saturated with attacker-planted
// same-prefix names) fails loud instead of spinning forever.
const secretsEditTempMaxNameAttempts = 8

// secureCreateSecretsEditTemp writes `contents` (the decrypted vault
// YAML) into a freshly-created, owner-only temp file under editDir and
// returns its path. The file is created via secureCreateOwnerOnlyFileFn
// (PROTECTED owner-only DACL / 0600 O_NOFOLLOW), so the cleartext bytes
// only ever land in an owner-only inode — never an inheriting one.
//
// The name is mcp-secrets-<hex>.yaml with a 128-bit crypto/rand suffix.
// Because the secure-create is O_EXCL / create-if-missing, a name
// already taken returns created=false with no error; we retry with a
// fresh name rather than reuse (or edit) a file we did not write. A hard
// error is surfaced immediately (no retry).
func secureCreateSecretsEditTemp(editDir string, contents []byte) (string, error) {
	for attempt := 0; attempt < secretsEditTempMaxNameAttempts; attempt++ {
		var suffix [16]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("secrets edit temp: crypto/rand: %w", err)
		}
		path := filepath.Join(editDir, "mcp-secrets-"+hex.EncodeToString(suffix[:])+".yaml")
		created, err := secureCreateOwnerOnlyFileFn(path, contents)
		if err != nil {
			return "", fmt.Errorf("secrets edit temp: secure create %s: %w", path, err)
		}
		if created {
			return path, nil
		}
		// created=false, err=nil: a regular file already occupies this
		// (random) name. Do NOT edit it — it is not our owner-only vault
		// temp. Retry with a fresh name.
	}
	return "", fmt.Errorf("secrets edit temp: exhausted %d unique-name attempts under %s", secretsEditTempMaxNameAttempts, editDir)
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
			// or ~/.local/share/mcp-local-hub on Unix). It carries the
			// ENTIRE decrypted vault in cleartext for the whole editor
			// session, so it MUST be owner-only regardless of the parent
			// directory's ACL.
			//
			// It is created via the vault's own owner-only secure-create
			// pipeline (api.SecureCreateOwnerOnlyFile): a PROTECTED
			// allowlist-only DACL is installed on the file HANDLE at
			// create time on Windows (SE_DACL_PROTECTED blocks inherited
			// ACEs) / O_CREAT|O_EXCL|O_NOFOLLOW mode 0600 on POSIX, and
			// the cleartext is written into that owner-only handle — so
			// it is readable ONLY by the current user, independent of the
			// parent ACL.
			//
			// The previous implementation used os.CreateTemp(editDir,
			// ...), which does NOT install a restrictive DACL on Windows:
			// Go does not translate the 0600 mode arg into a Windows DACL,
			// so the temp INHERITED the parent %LOCALAPPDATA% ACL. On a
			// sandbox-broadened %LOCALAPPDATA% (Wave\CodexSandboxUsers /
			// orphan AD SID — the exact scenario the vault read-hardening
			// exists for), a co-resident principal covered by the
			// inherited ACE could read all cleartext secrets for the full
			// editor session, bypassing BOTH the age encryption AND the
			// .age-key owner-only DACL. The old comment here claimed the
			// temp was "on the same ACL boundary as secrets.age itself" —
			// false whenever the parent ACL is broadened, which is exactly
			// when the vault files' own PROTECTED DACLs matter.
			//
			// Residual (documented, not silently left): after the editor
			// SAVES, the read-back below re-opens by path. In-place editors
			// (notepad — the default fallback — truncate+write) PRESERVE
			// the owner-only DACL. A write-new-then-rename editor (vim with
			// backupcopy=no, etc.) replaces the inode with a fresh file
			// that inherits the parent ACL; the saved cleartext then sits
			// parent-ACL-readable for the brief window between the editor's
			// own write and our wipe+delete. That window is inherent to how
			// such editors persist (they create the replacement inode
			// themselves; we cannot harden a file they have not written
			// yet) and is milliseconds versus the multi-minute session the
			// owner-only create closes. $EDITOR is operator-controlled;
			// operators who point it at a rename-style editor also accept
			// its swap sidecars (vim .swp / <name>~) landing in the
			// broadened dir — mcphub does not manage editor sidecar files.
			// The read-back below still fails closed on a symlink / non-
			// regular entry planted at the temp path.
			editDir := secrets.UserDataDir()
			if err := os.MkdirAll(editDir, 0o700); err != nil {
				return fmt.Errorf("create edit dir: %w", err)
			}
			tmpPath, err := secureCreateSecretsEditTemp(editDir, yamlBytes)
			if err != nil {
				return err
			}
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
			// The decrypted vault was already written into the owner-only
			// temp by secureCreateSecretsEditTemp; nothing more to write.

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
			// Read-back residual defense (fail closed): refuse to read
			// through a symlink or any non-regular entry sitting at the
			// temp path after the editor session. The random temp name
			// makes an in-session plant near-impossible, but following a
			// planted symlink on read would redirect the re-encrypt input
			// to attacker-chosen content — refuse rather than follow.
			if fi, lerr := os.Lstat(tmpPath); lerr != nil {
				return fmt.Errorf("secrets edit: stat edited temp: %w", lerr)
			} else if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
				return fmt.Errorf("secrets edit: refusing to read edited temp %s (not a regular file: mode %s)", tmpPath, fi.Mode())
			}
			updated, err := os.ReadFile(tmpPath)
			if err != nil {
				return err
			}
			// Flock ONLY the ImportYAML write — NOT the interactive editor
			// run above — so a concurrent vault write does not block for the
			// whole edit session. ImportYAML is a wholesale replace from the
			// editor's output (the operator's edit is authoritative). The api
			// layer holds the same flock via secrets.WithVaultLock.
			//
			// P2.4 audit-baseline correctness (bot r4 finding 2): the
			// BEFORE/AFTER diff that drives the per-key audit events MUST be
			// computed atomically with the import. Re-open the vault INSIDE the
			// lock to read the CURRENT on-disk baseline (not the stale snapshot
			// from before the editor session, which a concurrent rotate/delete
			// could have invalidated — that would make a concurrently-rotated
			// key look unchanged or a concurrent add look like ours), then
			// ImportYAML, then capture the after-state. All three steps happen
			// under one held lock. The captured maps' VALUES are used only to
			// detect change in memory — never logged.
			var beforeKV, afterKV map[string]string
			if err := secrets.WithVaultLock(defaultVaultPath(), func() error {
				locked, oerr := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
				if oerr != nil {
					return oerr
				}
				beforeKV = snapshotVaultKV(locked)
				if ierr := locked.ImportYAML(updated); ierr != nil {
					return ierr
				}
				afterKV = snapshotVaultKV(locked)
				return nil
			}); err != nil {
				return err
			}
			// Emit OUTSIDE the lock (best-effort observability; keeps the log
			// write off the critical section). The diff is over the locked
			// baseline captured above: added/changed → secret-rotated; removed
			// → secret-deleted. Only key NAMES reach the emit.
			emitBulkEditAuditEvents(beforeKV, afterKV)
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
			// bot r7 finding 4: do NOT open the vault once before the loop and
			// reuse that stale in-memory snapshot across many locked Set writes —
			// a concurrent vault write between the open and each Set would be
			// silently clobbered (last-writer-wins on the stale base), the same
			// TOCTOU class as the round-4 edit-baseline fix. Instead, RE-OPEN the
			// vault INSIDE each WithVaultLock so every Set is applied to the
			// CURRENT on-disk state. The audited cand.Key is then the key
			// actually committed under that same lock.
			in := bufio.NewReader(os.Stdin)
			imported := 0
			for _, cand := range candidates {
				cmd.Printf("Found %s = %s (from %s)\n", cand.Key, maskValue(cand.Value), path)
				cmd.Print("Import? [y/N]: ")
				line, _ := in.ReadString('\n')
				line = strings.TrimSpace(strings.ToLower(line))
				if line == "y" || line == "yes" {
					// Flock each individual Set write (NOT the interactive
					// y/N loop) so a concurrent vault write is not blocked
					// across the prompts. Open+Set under one lock so the write
					// is on the current on-disk state, not a pre-loop snapshot.
					if err := secrets.WithVaultLock(defaultVaultPath(), func() error {
						v, oerr := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
						if oerr != nil {
							return oerr
						}
						return v.Set(cand.Key, cand.Value)
					}); err != nil {
						return err
					}
					// Audit the committed credential import (key name only, never
					// the value) — same trail as `secrets set`/`delete`, so the
					// migrate surface isn't an audit blind spot. Emitted AFTER the
					// committed write, OUTSIDE the lock.
					api.EmitSecretRotatedAudit(cand.Key, map[string]any{"source": "cli-migrate"})
					imported++
				}
			}
			cmd.Printf("✓ Imported %d secrets. Original file NOT modified — run `mcp install` to apply.\n", imported)
			return nil
		},
	}
	c.Flags().StringVar(&fromClient, "from-client", "", "client name: claude-code | codex-cli | cursor | vscode | gemini-cli | qwen-cli | antigravity")
	_ = c.MarkFlagRequired("from-client")
	return c
}

// snapshotVaultKV reads the current key→value map from an open vault using
// only the exported Get/List surface. Used in-memory by the `secrets edit`
// bulk-diff (P2.4 finding 3); the values it captures are compared to detect
// changed secrets but are NEVER logged.
func snapshotVaultKV(v *secrets.Vault) map[string]string {
	out := map[string]string{}
	for _, k := range v.List() {
		if val, err := v.Get(k); err == nil {
			out[k] = val
		}
	}
	return out
}

// emitBulkEditAuditEvents diffs the before/after vault key→value maps from a
// `secrets edit` bulk import and emits accurate per-key audit events through
// the SHARED api owner: a key that is new OR whose value changed → a
// secret-rotated event; a key removed by the edit → a secret-deleted event.
// An unchanged key emits nothing. Only key NAMES are passed to the emit —
// the value comparison happens here in memory and never reaches the log.
func emitBulkEditAuditEvents(before, after map[string]string) {
	for k, newVal := range after {
		oldVal, existed := before[k]
		if !existed || oldVal != newVal {
			extra := map[string]any{"source": "cli-edit"}
			if !existed {
				extra["created"] = true
			}
			api.EmitSecretRotatedAudit(k, extra)
		}
	}
	for k := range before {
		if _, stillPresent := after[k]; !stillPresent {
			api.EmitSecretDeletedAudit(k, map[string]any{"source": "cli-edit"})
		}
	}
}

func clientConfigPath(name string) (string, error) {
	return clients.ConfigPathForName(name)
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
			if errors.Is(err, os.ErrInvalid) || errors.Is(err, io.EOF) {
				break
			}
			return out, err
		}
	}
	return out, nil
}

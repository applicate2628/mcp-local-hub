// internal/cli/trust.go — the `mcphub trust` verb group (area-5 gap-b).
//
// A thin, PURE cobra adapter over the EXISTING trusted-roots owners in the
// api package — it adds ZERO new store logic, validation, canonicalization,
// or file I/O of its own. The trusted-roots store, its file shape, the
// canonical predicate, the containment check, and the bless/remove
// read-modify-write under flock all live in internal/api/lsp_trusted_roots.go
// and are REUSED verbatim:
//
//   - `mcphub trust <path>`    → api.BlessDefaultTrustedRoot   (idempotent add)
//   - `mcphub untrust <path>`  → api.RemoveDefaultTrustedRoot  (idempotent remove)
//   - `mcphub trust list`      → api.LoadDefaultLSPTrustedRoots (+ print store path)
//
// SERVER-NEUTRAL by design: a trusted root authorizes first-touch
// auto-register for BOTH the LSP router (internal/gui/lsp_router.go) AND the
// serena router (the area-5 serena trust gate in
// internal/api/serena_auto_register.go). The store is the single shared
// authorization boundary, so the verb is named `trust`, not `lsp-trust`.
//
// Validation mirrors `mcphub setup --trusted-root` (validateTrustedRootArgs in
// internal/cli/setup.go) and the GUI POST /api/lsp/trusted-roots handler so
// CLI and GUI reject identical input identically:
//   - trust:   path must be NON-EMPTY and ABSOLUTE (a relative path resolves
//     against the caller's cwd, which is not a stable trust anchor).
//   - untrust: path must be NON-EMPTY only. Removal is by canonical equality;
//     an absolute requirement is unnecessary (removing an absent root is an
//     idempotent no-op success anyway) and an over-strict gate would block an
//     operator from removing a hand-edited relative entry by name.
package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

// newTrustCmd builds the `mcphub trust` command group: a default
// `trust <path>` (bless) plus a `trust list` subcommand. `untrust` is a
// separate top-level verb (newUntrustCmd) so the inverse reads naturally.
func newTrustCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "trust <path>",
		Short: "Bless a workspace folder as trusted for first-touch auto-register",
		Long: `Add an ABSOLUTE workspace path to the trusted-roots store.

A trusted root authorizes first-touch auto-register for trusted workspace
folders (both the LSP router and the serena router). The FIRST workspace
under any tree must be trusted explicitly (by this command, an explicit
` + "`mcphub workspace register`" + `, ` + "`mcphub setup --trusted-root`" + `, or the GUI
Settings → Trusted Roots panel); after that, sibling/child workspaces under
that root auto-register transparently. An untrusted tool-call path with no
trusted ancestor is refused.

This writes the SAME store the GUI Trusted Roots panel writes. Idempotent —
re-trusting an already-trusted root is a no-op success.

Examples:
  mcphub trust D:\dev\PaperPane
  mcphub trust list`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrustAdd(cmd.OutOrStdout(), args[0])
		},
	}
	c.AddCommand(newTrustListCmd())
	return c
}

// newUntrustCmd builds the `mcphub untrust <path>` inverse verb.
func newUntrustCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "untrust <path>",
		Short: "Remove a workspace folder from the trusted-roots store",
		Long: `Remove a path from the trusted-roots store (the inverse of ` + "`mcphub trust`" + `).

Removal is by canonical equality and only ever SHRINKS trust, so it can never
re-open a vulnerability. Idempotent — removing a root that is not present is a
no-op success. This writes the SAME store the GUI Trusted Roots panel writes.

Examples:
  mcphub untrust D:\dev\PaperPane`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUntrust(cmd.OutOrStdout(), args[0])
		},
	}
}

// newTrustListCmd builds `mcphub trust list`.
func newTrustListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List trusted workspace folders and the store path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrustList(cmd.OutOrStdout())
		},
	}
}

// validateTrustArg enforces the trust-side input contract (non-empty +
// absolute), mirroring validateTrustedRootArgs / the GUI add-root handler so
// CLI and GUI reject identical input identically. Done as a pre-flight pass so
// the command fails BEFORE any store write.
func validateTrustArg(raw string) (string, error) {
	root := strings.TrimSpace(raw)
	if root == "" {
		return "", fmt.Errorf("trust: path is required (LSP_TRUSTED_ROOTS_INVALID)")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("trust: %q must be an absolute path (LSP_TRUSTED_ROOTS_NOT_ABSOLUTE)", root)
	}
	return root, nil
}

// validateUntrustArg enforces the untrust-side contract (non-empty only).
func validateUntrustArg(raw string) (string, error) {
	root := strings.TrimSpace(raw)
	if root == "" {
		return "", fmt.Errorf("untrust: path is required (LSP_TRUSTED_ROOTS_INVALID)")
	}
	return root, nil
}

// runTrustAdd validates then blesses the root via the api owner. Pure adapter:
// the canonicalization + idempotent flock-protected append all live in
// api.BlessDefaultTrustedRoot.
func runTrustAdd(out io.Writer, raw string) error {
	root, err := validateTrustArg(raw)
	if err != nil {
		return err
	}
	if err := api.BlessDefaultTrustedRoot(root); err != nil {
		return fmt.Errorf("trust %q: %w", root, err)
	}
	fmt.Fprintf(out, "Trusted workspace folder: %s\n", root)
	return nil
}

// runUntrust validates then removes the root via the api owner. Pure adapter:
// the canonical-equality removal under flock lives in
// api.RemoveDefaultTrustedRoot (idempotent no-op when absent).
func runUntrust(out io.Writer, raw string) error {
	root, err := validateUntrustArg(raw)
	if err != nil {
		return err
	}
	if err := api.RemoveDefaultTrustedRoot(root); err != nil {
		return fmt.Errorf("untrust %q: %w", root, err)
	}
	fmt.Fprintf(out, "Untrusted workspace folder: %s\n", root)
	return nil
}

// runTrustList loads the store via the api owner and prints the store path
// plus every trusted root. An absent store is NOT an error (the loader returns
// an empty file), so a fresh host prints the path + "(none)".
func runTrustList(out io.Writer) error {
	path, err := api.DefaultLSPTrustedRootsPath()
	if err != nil {
		return fmt.Errorf("trust list: resolve store path: %w", err)
	}
	f, err := api.LoadDefaultLSPTrustedRoots()
	if err != nil {
		return fmt.Errorf("trust list: load store: %w", err)
	}
	fmt.Fprintf(out, "Trusted-roots store: %s\n", path)
	if f == nil || len(f.Roots) == 0 {
		fmt.Fprintln(out, "(none)")
		return nil
	}
	for _, r := range f.Roots {
		fmt.Fprintln(out, r)
	}
	return nil
}

package cli

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

// newCanonicalizeCmdReal returns the hidden `mcphub canonicalize` command: a
// minimal, non-interactive, BINARY-ONLY copy of the currently-running mcphub
// into the canonical ~/.local/bin. It exists so the npm package's postinstall
// hook (npm/scripts/postinstall.js) can invoke the FRESHLY-installed platform
// binary to canonicalize itself, so `npm install -g mcp-local-hub@<newer>`
// refreshes the authoritative ~/.local/bin binary directly (bug
// 2026-07-22-canonical-local-bin-binary-not-auto-updated-from-npm).
//
// Unlike `mcphub setup`, this command deliberately does NOT:
//   - register PATH (that is a one-time setup concern; the canonical path has
//     been on PATH since the operator's first `mcphub setup`);
//   - install the supervisor-liveness scheduled task;
//   - rewrite client MCP configs or probe/mutate the OS ephemeral range;
//   - prompt, request elevation, or refuse when elevated;
//   - reap or restart the running fleet (it is NOT `install --upgrade`).
//
// It reuses the SAME copy owner (copyExe) and canonical-path owner
// (setupTargetPath) `mcphub setup` uses, and the SAME crash-safe rename-aside
// swap (api.RenameAsideReplace) `mcphub install --upgrade` uses for the
// Windows file-lock case — no copy logic is reinvented.
func newCanonicalizeCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "canonicalize",
		Short: "Copy this binary into the canonical ~/.local/bin (no PATH, no tasks, no fleet restart)",
		Long: `Copy the CURRENTLY-RUNNING mcphub binary into the canonical
~/.local/bin/mcphub.exe (Windows) or ~/.local/bin/mcphub (Linux/macOS), and
do nothing else.

Unlike 'mcphub setup', this command does NOT register PATH, install the
supervisor-liveness scheduled task, rewrite client MCP configs, probe the OS
ephemeral range, prompt, request elevation, or reap/restart the running fleet.

It is the minimal, non-interactive canonicalize step the npm package's
postinstall hook (npm/scripts/postinstall.js) runs on the freshly installed
platform binary, so 'npm install -g mcp-local-hub@<newer>' refreshes the
authoritative ~/.local/bin binary directly. The copy is lock-safe on Windows:
if the running fleet holds the canonical binary, the prior binary is renamed
aside to '.old-<ts>' (the same crash-safe swap 'mcphub install --upgrade'
uses) and the new binary takes effect on the next fleet/supervisor restart.

Idempotent: a no-op when the running binary already IS the canonical target,
or when the canonical binary is already byte-identical to this one.

For hosts installed with 'npm install --ignore-scripts' (which skips the
postinstall), the manual equivalent is 'mcphub setup'.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return canonicalizeBinaryOnly(cmd.OutOrStdout())
		},
	}
}

// canonicalizeBinaryOnly copies the currently-running executable to the
// canonical ~/.local/bin target. It resolves both endpoints and delegates the
// (testable) core to canonicalizeBinaryToTarget.
func canonicalizeBinaryOnly(w io.Writer) error {
	target, err := setupTargetPath()
	if err != nil {
		return fmt.Errorf("resolve canonical target: %w", err)
	}
	curExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	return canonicalizeBinaryToTarget(w, curExe, target)
}

// canonicalizeBinaryToTarget is the pure core of canonicalizeBinaryOnly,
// parameterized on src/target so it is directly unit-testable without
// depending on os.Executable(). It applies two no-op guards before copying:
//
//  1. self-copy guard — src IS the canonical target (mirrors
//     bootstrapCopyOnly's samePath short-circuit); and
//  2. content guard — the canonical binary is already byte-identical to src,
//     so a repeated `npm install` of the SAME version does not churn a
//     rename-aside `.old-<ts>` on every reinstall.
func canonicalizeBinaryToTarget(w io.Writer, src, target string) error {
	if samePath(src, target) {
		fmt.Fprintf(w, "✓ mcphub already canonical at %s (running binary is the target)\n", target)
		return nil
	}
	// sameFileContents errors when target is absent (first install) or
	// unreadable; treat that as "not identical" and proceed to copy.
	if same, err := sameFileContents(src, target); err == nil && same {
		fmt.Fprintf(w, "✓ mcphub already up to date at %s\n", target)
		return nil
	}
	return canonicalizeReplace(w, src, target)
}

// canonicalizeReplace performs the actual, lock-safe binary replacement.
//
// On Windows a running canonical binary is held with FILE_SHARE_DELETE but not
// FILE_SHARE_WRITE, so a plain copy (copyExe removes the destination first)
// fails while the fleet is up. When the target already exists on Windows we use
// the crash-safe rename-aside swap `install --upgrade` uses: rename the running
// binary to `.old-<ts>` (permitted because the image is FILE_SHARE_DELETE),
// then move the freshly-staged binary into place. The new binary takes effect
// on the next fleet/supervisor restart; running processes keep their mapped
// `.old-<ts>` image until they exit.
//
// On POSIX a rename over a running executable is safe (the executing image
// keeps its inode open), and a Windows FIRST install has no running target to
// lock, so both take the plain copyExe path.
func canonicalizeReplace(w io.Writer, src, target string) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	_, statErr := os.Stat(target)
	targetExists := statErr == nil

	if runtime.GOOS == "windows" && targetExists {
		staged := target + ".new"
		if err := copyExe(src, staged); err != nil {
			return fmt.Errorf("stage fresh binary at %s: %w", staged, err)
		}
		if err := api.RenameAsideReplace(target, staged); err != nil {
			_ = os.Remove(staged)
			return err
		}
		// Bound the `.old-<ts>` rollback chain (newest 5 / older than 7 days).
		// Non-fatal — a still-mapped image just gets swept on the next pass.
		_ = api.SweepOldBinaries(dir)
		fmt.Fprintf(w, "✓ mcphub canonicalized at %s (prior binary kept as .old-<ts>; takes effect on next fleet/supervisor restart)\n", target)
		return nil
	}

	if err := copyExe(src, target); err != nil {
		return err
	}
	fmt.Fprintf(w, "✓ mcphub canonicalized at %s\n", target)
	return nil
}

// sameFileContents reports whether a and b are byte-identical regular files.
// A size mismatch short-circuits before any hashing. Any stat/open error
// (notably a missing target on first install) propagates so the caller can
// treat it as "not identical".
func sameFileContents(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if !ai.Mode().IsRegular() || !bi.Mode().IsRegular() {
		return false, nil
	}
	if ai.Size() != bi.Size() {
		return false, nil
	}
	ah, err := hashFile(a)
	if err != nil {
		return false, err
	}
	bh, err := hashFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ah, bh), nil
}

func hashFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

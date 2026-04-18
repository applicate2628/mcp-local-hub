package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// mcphubShortName is the bare executable name that scheduler tasks and relay
// entries reference. PATH resolution picks the correct binary from whatever
// directory the user has on PATH (usually ~/.local/bin after `mcphub setup`).
var mcphubShortName = func() string {
	if runtime.GOOS == "windows" {
		return "mcphub.exe"
	}
	return "mcphub"
}()

// setupTargetDir returns the canonical install directory for the current
// user: <home>/.local/bin on all platforms.
func setupTargetDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// setupTargetPath returns the canonical install path for the mcphub binary.
func setupTargetPath() (string, error) {
	dir, err := setupTargetDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, mcphubShortName), nil
}

// samePath returns true when a and b refer to the same absolute filesystem
// location. Case-insensitive on Windows (NTFS/ReFS are case-preserving but
// case-insensitive by default); case-sensitive elsewhere.
func samePath(a, b string) bool {
	ac, err := filepath.Abs(filepath.Clean(a))
	if err != nil {
		return false
	}
	bc, err := filepath.Abs(filepath.Clean(b))
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ac, bc)
	}
	return ac == bc
}

// copyExe copies src to dst via a tempfile + rename so a failed copy never
// leaves a partial exe at dst. On Windows an existing dst must be removed
// first because os.Rename refuses to overwrite; if dst is locked by a
// running process we surface a friendly hint.
func copyExe(src, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".*.tmp")
	if err != nil {
		return fmt.Errorf("tempfile in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Copy + close; on any failure make sure the tempfile does not survive.
	_, copyErr := io.Copy(tmp, in)
	closeErr := tmp.Close()
	if copyErr != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("copy to tempfile: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close tempfile: %w", closeErr)
	}
	// Preserve executable bit on non-Windows; Windows ignores mode bits here.
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	// Windows: os.Rename over an existing file fails. Remove first; if that
	// fails because the target is held open by a running daemon, give a clear
	// hint instead of the raw sharing-violation error.
	if _, err := os.Stat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf(
				"target %s is in use — stop running daemons first with `mcphub stop --all`, then re-run setup: %w",
				dst, err)
		}
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename tempfile to %s: %w", dst, err)
	}
	return nil
}

// Bootstrap installs the currently-running mcphub to ~/.local/bin and ensures
// that directory is on the user's PATH. Idempotent: a second call makes no
// changes if the target already matches the current exe and PATH is set up.
//
// Exported so `mcphub install` can invoke the same flow when it detects that
// mcphub is not yet on PATH and stdin is a terminal.
func Bootstrap(w io.Writer) error {
	target, err := setupTargetPath()
	if err != nil {
		return fmt.Errorf("resolve target path: %w", err)
	}
	targetDir := filepath.Dir(target)

	curExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}

	if samePath(curExe, target) {
		fmt.Fprintf(w, "\u2713 mcphub already at %s (no copy needed)\n", target)
	} else {
		if err := copyExe(curExe, target); err != nil {
			return err
		}
		fmt.Fprintf(w, "\u2713 mcphub installed at %s\n", target)
	}

	// Platform-specific PATH registration; prints its own success line.
	return ensureOnPath(w, targetDir)
}

// newSetupCmdReal returns the `mcphub setup` command.
func newSetupCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Install mcphub to ~/.local/bin and register on user PATH",
		Long: `Install the currently-running mcphub binary to the canonical user-local
location (~/.local/bin) and make sure that directory is on the user PATH.

This lets scheduler tasks and Antigravity relay entries reference mcphub
by short name rather than an absolute path, so you can move or rebuild
the binary without breaking previously-registered integrations.

Idempotent: a second run produces no changes if the target is already
up to date.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return Bootstrap(cmd.OutOrStdout())
		},
	}
}

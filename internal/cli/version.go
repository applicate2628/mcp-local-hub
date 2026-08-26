package cli

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/buildinfo"
)

// normalizeExePath resolves symlinks and cleans an executable path so two
// references to the same on-disk binary compare equal. Best-effort: a
// resolve error returns the cleaned-but-unresolved path so the caller can
// still compare, and the caller treats a normalize failure as "no warning".
func normalizeExePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

// pathShadowDiagnostic compares the running executable against the `mcphub`
// the shell PATH resolves to. Different paths with the same bytes are an
// informational alternate path. Different or unreadable identities are an
// operator warning. Paths are compared case-insensitively only on Windows,
// where executable paths are case-insensitive; POSIX keeps exact case so
// case-only distinct paths on case-sensitive filesystems still compare bytes.
func pathShadowDiagnostic(runningExe, pathResolved string) string {
	if runningExe == "" || pathResolved == "" {
		return ""
	}
	a, errA := normalizeExePath(runningExe)
	b, errB := normalizeExePath(pathResolved)
	if errA != nil || errB != nil {
		return ""
	}
	if sameExePath(a, b) {
		return ""
	}
	runningSHA256, errRunning := executableSHA256(a)
	pathSHA256, errPath := executableSHA256(b)
	if errRunning != nil || errPath != nil {
		return fmt.Sprintf("identity-unverified: the 'mcphub' on your PATH (%s) cannot be verified against this running binary (%s); "+
			"a fresh shell may run a different binary. Run: %s", b, a, shadowReconciliationCommand())
	}
	if runningSHA256 == pathSHA256 {
		return fmt.Sprintf("equivalent alternate path: the 'mcphub' on your PATH (%s) is byte-identical to this running binary (%s).", b, a)
	}
	return fmt.Sprintf("binary identity differs: the 'mcphub' on your PATH (%s) is NOT this running binary (%s); "+
		"a fresh shell may run the shadowed one. Run: %s", b, a, shadowReconciliationCommand())
}

func executableSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func shadowReconciliationCommand() string {
	return "mcphub setup"
}

func sameExePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// SetBuildInfo retains the cli package's existing public surface so
// main.main() doesn't need to switch to a new import path. It
// forwards into the canonical buildinfo store, which both this
// package's `mcphub version` command and the gui's /api/version
// handler read from.
func SetBuildInfo(version, commit, date string) {
	buildinfo.Set(version, commit, date)
}

func newVersionCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, build metadata, and project homepage",
		Long: `Print build metadata: semantic version, short git commit, build date,
Go toolchain version, target platform, plus homepage / issue tracker
/ commercial licensing / license / author links.

Values are baked in at build time via build.sh / build.ps1 (which
injects ldflags). On Windows, only 'pwsh ./build.ps1' produces an
installable product binary and runs PE subsystem admission; bare
'go build' is a non-product compile check.

Example:
  mcphub version
  → mcp-local-hub 0.3.0
      commit:     38f6349
      build date: 2026-04-18T20:56:14Z
      go version: go1.26.2
      platform:   windows/amd64`,
		Run: func(cmd *cobra.Command, args []string) {
			version, commit, date := buildinfo.Get()
			cmd.Printf("mcp-local-hub %s\n", version)
			cmd.Printf("  commit:     %s\n", commit)
			cmd.Printf("  build date: %s\n", date)
			cmd.Printf("  go version: %s\n", runtime.Version())
			cmd.Printf("  platform:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
			// Running-binary location + PATH-shadow diagnostic: surfaces the
			// case where a stale `mcphub` on PATH (e.g. an npm-global install)
			// shadows a fresher dev deploy (~/.local/bin). Best-effort — a
			// lookup failure prints nothing extra.
			runningExe, _ := os.Executable()
			if runningExe != "" {
				cmd.Printf("  running:    %s\n", runningExe)
			}
			if resolved, lerr := exec.LookPath("mcphub"); lerr == nil {
				if diagnostic := pathShadowDiagnostic(runningExe, resolved); diagnostic != "" {
					prefix := "⚠ shadow"
					if strings.HasPrefix(diagnostic, "equivalent alternate path:") {
						prefix = "ℹ shadow"
					}
					cmd.Printf("  %s:   %s\n", prefix, diagnostic)
				}
			}
			cmd.Println()
			cmd.Println("  homepage:   https://github.com/applicate2628/mcp-local-hub")
			cmd.Println("  issues:     https://github.com/applicate2628/mcp-local-hub/issues")
			cmd.Println("  commercial: https://github.com/applicate2628/mcp-local-hub/blob/master/README.md#license")
			cmd.Println("  license:    MPL-2.0")
			cmd.Println("  author:     Dmitry Denisenko (@applicate2628)")
		},
	}
}

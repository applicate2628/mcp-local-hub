//go:build !windows

package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ensureOnPath on non-Windows prints a one-liner the user can paste into
// their shell rc. We do NOT modify ~/.bashrc or ~/.zshrc automatically —
// silently mutating shell startup files is too invasive for an install step.
func ensureOnPath(w io.Writer, dir string) error {
	if pathEnvContains(os.Getenv("PATH"), dir) {
		fmt.Fprintf(w, "\u2713 %s already on PATH\n", dir)
		return nil
	}
	fmt.Fprintf(w, "\u2139 %s is NOT on PATH. Add this line to your shell rc (e.g. ~/.bashrc or ~/.zshrc):\n", dir)
	fmt.Fprintf(w, "    export PATH=\"%s:$PATH\"\n", dir)
	return nil
}

// pathEnvContains reports whether the ':'-separated PATH string includes dir.
// Exact match per entry after trimming whitespace; non-Windows filesystems
// are case-sensitive so no case folding.
func pathEnvContains(pathValue, dir string) bool {
	for _, entry := range strings.Split(pathValue, ":") {
		if strings.TrimSpace(entry) == dir {
			return true
		}
	}
	return false
}

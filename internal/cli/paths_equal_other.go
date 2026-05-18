//go:build !windows

package cli

import "path/filepath"

func pathsEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

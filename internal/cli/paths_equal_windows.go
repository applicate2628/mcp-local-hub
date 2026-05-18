//go:build windows

package cli

import (
	"path/filepath"
	"strings"
)

func pathsEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

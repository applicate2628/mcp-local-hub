//go:build linux

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func pidMatchesMcphub(pid int) bool {
	if pid <= 0 {
		return false
	}
	target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return false
	}
	base := filepath.Base(target)
	return strings.EqualFold(base, "mcphub") || strings.EqualFold(base, "mcphub.exe")
}

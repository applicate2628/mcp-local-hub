package cli

import (
	"os"
	"path/filepath"
	"sync"
)

// canonicalMcphubPath returns the absolute, symlink-resolved path of
// the running mcphub binary. Cached after first call because
// os.Executable() can do a syscall on each invocation. Empty string
// is returned on resolution failure (caller treats as identity gate
// failure -> fail-closed).
var (
	canonicalPathOnce sync.Once
	canonicalPathStr  string
)

func canonicalMcphubPath() string {
	canonicalPathOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		if abs, err := filepath.Abs(exe); err == nil {
			exe = abs
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		canonicalPathStr = exe
	})
	return canonicalPathStr
}

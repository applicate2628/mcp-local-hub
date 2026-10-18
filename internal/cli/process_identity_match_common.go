package cli

import (
	"mcp-local-hub/internal/api"
)

// canonicalMcphubPath returns the absolute, symlink-resolved path of
// the running mcphub binary. Cached after first call because
// os.Executable() can do a syscall on each invocation. Empty string
// is returned on resolution failure (caller treats as identity gate
// failure -> fail-closed).
func canonicalMcphubPath() string {
	path, err := api.SelfMcphubExecutable()
	if err != nil {
		return ""
	}
	return path
}

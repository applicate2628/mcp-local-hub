package api

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LogsOpts controls a LogsGet call.
type LogsOpts struct {
	LogDir string // e.g., %LOCALAPPDATA%\mcp-local-hub\logs
	Server string
	Daemon string // "default" for single-daemon manifests; daemon name otherwise
	Tail   int    // 0 = all lines
}

// LogsGetFrom reads the log file for (server, daemon) and returns the last
// Tail lines. Exposed (rather than LogsGet) so tests can pass a custom dir.
func (a *API) LogsGetFrom(opts LogsOpts) (string, error) {
	path := filepath.Join(opts.LogDir, fmt.Sprintf("%s-%s.log", opts.Server, opts.Daemon))
	data, err := os.ReadFile(path)
	if err != nil {
		// An absent log file is the common case for healthy stdio-only
		// servers that never write to stderr (the only stream tee'd to
		// the log) — perftools, time, sequential-thinking, embedded Go
		// servers with no diagnostics to emit. Return a human-readable
		// placeholder instead of an OS error so `mcphub logs perftools`
		// doesn't look like the daemon is broken when it's fine.
		if os.IsNotExist(err) {
			return fmt.Sprintf("(no log output yet — %s-%s daemon hasn't written to stderr, which is normal for healthy stdio-only servers)\n",
				opts.Server, opts.Daemon), nil
		}
		return "", err
	}
	if opts.Tail <= 0 {
		return string(data), nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) <= opts.Tail {
		return string(data), nil
	}
	tail := lines[len(lines)-opts.Tail:]
	return strings.Join(tail, "\n") + "\n", nil
}

// LogsGet is the production entry point using the OS-default log dir.
func (a *API) LogsGet(server, daemon string, tail int) (string, error) {
	return a.LogsGetFrom(LogsOpts{
		LogDir: defaultLogDir(),
		Server: server,
		Daemon: daemon,
		Tail:   tail,
	})
}

// LogsStream is reserved for Phase 3A.3; stub returns error.
func (a *API) LogsStream(server, daemon string, out *bufio.Writer) error {
	return fmt.Errorf("LogsStream not yet implemented — use LogsGet with Tail")
}

func defaultLogDir() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "logs")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "mcp-local-hub", "logs")
}

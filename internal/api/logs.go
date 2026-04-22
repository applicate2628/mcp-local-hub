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

// LogPlaceholderPrefix is the fixed leading substring of the human-readable
// body returned by LogsGet/LogsGetFrom when the log file for (server,daemon)
// does not exist yet. The full body continues with the server and daemon
// names and closing explanatory text. Callers that need to distinguish
// "placeholder" from real log content should prefer LogPlaceholderFor and
// compare with full-string equality — prefix matching would misclassify
// real log content that happens to start with this phrase and silently
// drop it from streaming output.
//
// The GUI SSE tail-follow (internal/gui/logs.go) depends on this: if it
// primed its cursor from the placeholder's length, the first bytes of the
// real log file — once the daemon finally writes to stderr — would look
// already-emitted and be silently skipped.
const LogPlaceholderPrefix = "(no log output yet"

// LogPlaceholderFor returns the exact placeholder string LogsGet/LogsGetFrom
// emits when no log file exists for (server, daemon). Exposed so the GUI's
// SSE log streamer (internal/gui/logs.go) can compare by exact-match (==)
// rather than strings.HasPrefix — real log content whose first line happens
// to start with LogPlaceholderPrefix would otherwise be misclassified as
// the placeholder and silently dropped from the SSE stream.
//
// Both LogsGetFrom's not-exist branch and the GUI's isLogPlaceholder call
// this helper: single source of truth for the placeholder's exact byte
// sequence (trailing newline included).
func LogPlaceholderFor(server, daemon string) string {
	return fmt.Sprintf("%s — %s-%s daemon hasn't written to stderr, which is normal for healthy stdio-only servers)\n",
		LogPlaceholderPrefix, server, daemon)
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
			return LogPlaceholderFor(opts.Server, opts.Daemon), nil
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
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "logs")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "mcp-local-hub", "logs")
}

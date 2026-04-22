// Package gui — /api/logs/:server route.
//
// Exposes daemon log text to the GUI in two shapes:
//
//   - GET /api/logs/:server[?tail=N]           plain text snapshot
//   - GET /api/logs/:server/stream             SSE tail-follow
//
// The streaming path is a deliberately minimal MVP: it re-reads the log
// file every 500ms and emits any new suffix as "log-line" SSE events.
// This is accurate-enough for an interactive "Follow" checkbox on the
// Logs screen without adding an fsnotify dependency. Phase 3B-II may
// replace it with filesystem-event-driven streaming if latency or CPU
// cost becomes visible.
package gui

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
)

// validNameRe is the safe charset for server + daemon names that flow
// into the log-file path composed by api.LogsGet, which reads
// "<logDir>/<server>-<daemon>.log". Without this gate a query like
// ?daemon=../etc/passwd would escape logDir via the composed filename.
//
// The charset mirrors api.validManifestName (^[a-z0-9][a-z0-9._-]*$),
// which is the canonical server-name validator: alphanumerics plus
// ".", "_", "-". If this GUI regex were tighter than the manifest
// validator, servers with legal dotted names (e.g. paper-search-mcp.io,
// foo.bar) would be valid everywhere except /api/logs/:server, which
// would 404 them. We intentionally allow uppercase here as a superset
// because path separators and traversal sequences are what matter for
// log-file safety; the underlying manifest layer still enforces its
// own lowercase rule when the name is actually used to load a
// manifest. The explicit ".." check in validName keeps path-traversal
// blocked even though dots are otherwise legal.
var validNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validName returns true iff s is a non-empty string of safe name
// characters. Callers use it to gate untrusted path-segment and
// query-param input before composing a log-file path.
//
// Two secondary rules backstop the charset once "." is allowed:
//
//   - reject s == "." or s == "..": single- and double-dot standalone
//     segments have filesystem meaning (current / parent directory).
//     The mux canonicalizes literal ".." in the URL path before the
//     handler runs, but the daemon query parameter does not go through
//     that canonicalization, so the gate has to fire here.
//   - reject any s containing "..": sequences like "foo..bar" or
//     "..hidden" would satisfy the charset once "." is legal. The
//     charset already excludes "/" and "\\", so combined with this
//     rule no "." form can escape the log directory.
//
// Dotted identifiers like paper-search-mcp.io still pass both checks.
func validName(s string) bool {
	if s == "" || !validNameRe.MatchString(s) {
		return false
	}
	if s == "." || s == ".." {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	return true
}

// registerLogsRoutes wires the /api/logs/ prefix. The suffix after the
// server name decides the mode — absent = snapshot, "stream" = SSE.
// Spec §4.6.
func registerLogsRoutes(s *Server) {
	s.mux.HandleFunc("/api/logs/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/logs/")
		parts := strings.SplitN(rest, "/", 2)
		server := parts[0]
		// validName gates the server path segment before it reaches
		// api.LogsGet. A percent-encoded value like "..%2Fetc"
		// decodes to URL.Path="../etc" and would slip past the
		// ServeMux redirect path (which only canonicalizes literal
		// ".."). Reject anything outside [A-Za-z0-9_-]+ with 404 so
		// the handler never composes a log-file path from untrusted
		// input. See validNameRe above.
		if !validName(server) {
			http.NotFound(w, r)
			return
		}
		// A trailing path segment other than "stream" (e.g.
		// /api/logs/foo/bar) is not a valid route. Silently falling
		// through to the snapshot branch with server="foo" would
		// misattribute the request; return 404 instead.
		if len(parts) == 2 && parts[1] != "" && parts[1] != "stream" {
			http.NotFound(w, r)
			return
		}
		streaming := len(parts) == 2 && parts[1] == "stream"
		// daemon is optional — an empty string lets the logsProvider
		// adapter fall back to "default" for single-daemon servers.
		// Multi-daemon servers (serena: claude + codex) require the
		// explicit daemon name the UI picker selected. When non-empty,
		// it must pass validName for the same log-path-composition
		// reason the server segment does.
		daemon := r.URL.Query().Get("daemon")
		if daemon != "" && !validName(daemon) {
			writeAPIError(w, fmt.Errorf("invalid daemon name"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if streaming {
			streamLogs(s, server, daemon, w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
		if tail <= 0 {
			tail = 500
		}
		body, err := s.logs.Logs(server, daemon, tail)
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "LOGS_FAILED")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
}

// streamLogs implements the SSE tail-follow. Each tick we ask the
// logsProvider for the full log text (tail=0) and emit the new suffix
// since lastLen as individual "log-line" events. Empty trailing lines
// after a Split on "\n" are skipped so a trailing newline does not
// produce a blank SSE event.
//
// daemon is threaded through to every logsProvider call (both the
// initial prime and the per-tick fetch) so multi-daemon servers like
// serena can follow the correct daemon's log file. An empty daemon is
// forwarded as-is and the logsProvider adapter falls back to "default".
//
// The handler exits when the client disconnects (r.Context cancellation)
// or when the provider returns an error (sent as an "error" SSE event).
func streamLogs(s *Server, server, daemon string, w http.ResponseWriter, r *http.Request) {
	// Spec §4.6 defines only GET /api/logs/:server/stream. Enforce that
	// here rather than after the flusher type-assert: the outer route
	// dispatches to streamLogs based solely on the trailing path segment,
	// so without this gate POST /api/logs/foo/stream (or any non-GET
	// method) would open a long-lived SSE response. Reject with 405 +
	// Allow: GET before any SSE headers are written.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher.Flush()

	// No explicit SSE keepalive is sent: the server binds 127.0.0.1 only
	// (spec §2.2 non-goal: remote access) and browsers hold idle localhost
	// SSE connections indefinitely. If Phase 3B-II ever exposes this
	// stream through a reverse proxy, add a `event: ping\ndata: \n\n`
	// heartbeat every 20–30s to defeat proxy idle timeouts.
	ctx := r.Context()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	// Prime lastLen with the current log size before the loop. Without
	// this the first tick emits the ENTIRE current log as live
	// "log-line" events — duplicating what /api/logs/:server already
	// rendered when the UI first opened the Logs screen.
	//
	// Skip the prime when the body is api.LogsGet's "no log file yet"
	// placeholder (file does not exist and the API returns nil error +
	// human-readable text). Seeding the cursor from the placeholder's
	// length would silently discard the first len(placeholder) bytes of
	// the real log once the daemon finally writes to stderr — the user
	// would permanently miss the start of that first session. See
	// api.LogPlaceholderFor for the shared exact-match sentinel.
	var lastLen int
	// pendingLine holds any unterminated trailing fragment (no "\n") that
	// arrived on the previous tick's suffix. A daemon often writes one
	// logical line in multiple writes — e.g. "Loading..." then " done\n"
	// — so without carrying the fragment across ticks the first write's
	// suffix splits into a log-line event for "Loading..." and the next
	// tick emits another event for " done". The UI then sees corrupted
	// line boundaries. Holding the unterminated fragment until its
	// newline arrives merges the writes back into one event.
	var pendingLine string
	if body, err := s.logs.Logs(server, daemon, 0); err == nil && !isLogPlaceholder(body, server, daemon) {
		lastLen = len(body)
		// We deliberately do NOT seed pendingLine from the prime. The
		// prime is the initial-state snapshot already rendered by
		// /api/logs/:server when the UI opened the Logs screen; it is
		// not re-emitted as SSE log-line events. Carrying a fragment
		// from it into the first tick would risk prepending prime
		// bytes to a live suffix. The rotation branch below applies
		// the same rule for the same reason.
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			body, err := s.logs.Logs(server, daemon, 0)
			if err != nil {
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
				flusher.Flush()
				return
			}
			if isLogPlaceholder(body, server, daemon) {
				// The daemon hasn't written anything yet (or the log
				// file was removed and api.LogsGet is again returning
				// the "no log file yet" placeholder). Do NOT emit it as
				// a log-line event and do NOT advance lastLen. Emitting
				// would push human-readable placeholder text into the
				// UI's line stream; advancing would seed the cursor
				// from the placeholder's length (~100+ chars), and then
				// on the next tick the real log — typically shorter
				// than the placeholder — would hit the
				// `len(body) < lastLen` rotation branch, which resets
				// the cursor to the new size and `continue`s, silently
				// skipping the first bytes of the daemon's first
				// session. See api.LogPlaceholderFor for the shared
				// exact-match sentinel and the prime block above for
				// the matching guard on the initial fetch.
				continue
			}
			if len(body) < lastLen {
				// Log rotated or truncated — the file is smaller than
				// our cursor. Reset to the current size instead of
				// replaying the new file from the start; continuing to
				// gate on `len(body) > lastLen` would freeze emission
				// until the new file grew past the old length. Also
				// drop any pendingLine: it belonged to the previous
				// file's trailing fragment, which has no relationship
				// to the rotated file's contents. Carrying it forward
				// would prepend dead text onto the first real line of
				// the new file.
				lastLen = len(body)
				pendingLine = ""
				continue
			}
			if len(body) > lastLen {
				// Prepend any unterminated fragment held from the
				// previous tick, then clear it — if this tick's suffix
				// also ends mid-line, the new fragment is reassigned
				// to pendingLine below.
				suffix := pendingLine + body[lastLen:]
				pendingLine = ""

				lines := strings.Split(suffix, "\n")
				// strings.Split("a\nb\n", "\n") -> ["a", "b", ""].
				// strings.Split("a\nb",   "\n") -> ["a", "b"].
				// A non-empty last element means the suffix ended
				// without a newline — that fragment is unterminated
				// and must be held until the next tick supplies its
				// continuation. An empty last element means the
				// suffix ended cleanly on a newline; drop the empty
				// tail so it isn't emitted as a blank line event.
				if n := len(lines); n > 0 && lines[n-1] != "" {
					pendingLine = lines[n-1]
					lines = lines[:n-1]
				} else if n > 0 && lines[n-1] == "" {
					lines = lines[:n-1]
				}
				for _, line := range lines {
					if line == "" {
						// Interior blank lines inside the suffix
						// (e.g. two consecutive newlines) are not
						// emitted as blank SSE events; keep the
						// pre-R22 behavior of skipping them.
						continue
					}
					fmt.Fprintf(w, "event: log-line\ndata: %s\n\n", line)
				}
				lastLen = len(body)
				flusher.Flush()
			}
		}
	}
}

// isLogPlaceholder reports whether body is the exact "no log file yet"
// placeholder api.LogsGet returns for (server, daemon) when the log file
// does not exist. That body is human-readable text, not log content, so
// streamLogs must NOT seed its lastLen cursor from its length — doing so
// would silently skip the first len(placeholder) bytes of the real log
// once the daemon eventually writes to stderr.
//
// Matches by exact full-string equality against api.LogPlaceholderFor,
// not by prefix against api.LogPlaceholderPrefix. A prefix check would
// misclassify any real log line whose leading text happens to start with
// "(no log output yet" — Follow would then permanently drop that line
// from the SSE stream. api.LogPlaceholderFor is the single source of
// truth for the exact bytes LogsGet emits for a given (server, daemon);
// this helper is the consumer side of that contract.
//
// An empty daemon is normalized to "default" here to match realLogs.Logs,
// which makes the same substitution before calling api.LogsGet — so the
// placeholder produced by the API layer always uses "default" in that
// branch, and this side must compare against the same value.
func isLogPlaceholder(body, server, daemon string) bool {
	if daemon == "" {
		daemon = "default"
	}
	return body == api.LogPlaceholderFor(server, daemon)
}

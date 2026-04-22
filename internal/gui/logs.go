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
)

// validNameRe is the safe charset for server + daemon names that flow
// into the log-file path composed by api.LogsGet, which reads
// "<logDir>/<server>-<daemon>.log". Without this gate a query like
// ?daemon=../etc/passwd would escape logDir via the composed filename.
// Only alphanumerics, "-", and "_" are allowed — rejecting path
// separators (/, \), ".." traversal, whitespace, NUL, and every other
// filesystem-significant character. This mirrors the task-name
// character class used elsewhere in the hub (e.g. workspace keys are
// lowercase hex and languages are simple identifiers).
var validNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validName returns true iff s is a non-empty string of safe name
// characters. Callers use it to gate untrusted path-segment and
// query-param input before composing a log-file path.
func validName(s string) bool {
	return s != "" && validNameRe.MatchString(s)
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
	var lastLen int
	if body, err := s.logs.Logs(server, daemon, 0); err == nil {
		lastLen = len(body)
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
			if len(body) < lastLen {
				// Log rotated or truncated — the file is smaller than
				// our cursor. Reset to the current size instead of
				// replaying the new file from the start; continuing to
				// gate on `len(body) > lastLen` would freeze emission
				// until the new file grew past the old length.
				lastLen = len(body)
				continue
			}
			if len(body) > lastLen {
				suffix := body[lastLen:]
				for _, line := range strings.Split(suffix, "\n") {
					if line == "" {
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

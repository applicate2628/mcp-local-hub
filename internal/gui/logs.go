// Package gui — /api/logs/:server route.
//
// Exposes daemon log text to the GUI in two shapes:
//
//   - GET /api/logs/:server[?tail=N]           plain text snapshot
//   - GET /api/logs/:server/stream             SSE tail-follow
//
// The streaming path polls the log every 500 ms and emits new complete lines
// as "log-line" events.
package gui

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
)

// maxPendingLineBytes caps the unterminated-fragment buffer in
// streamLogs. A daemon that writes a long line without a newline (or
// hangs after a partial write) cannot grow this string without limit;
// once exceeded, the held fragment is emitted truncated and only the
// last maxPendingLineBytes are retained so the eventual newline arrival
// can still complete a coherent line.
const maxPendingLineBytes = 64 * 1024 // 64 KiB

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
	// Open-logs-folder lives at a sibling path (not a child of /api/logs/)
	// so it can never collide with a server name — the prefix catch-all
	// uses parts[0] as the server name, and a manifest legitimately
	// named "open-folder" would race with an in-prefix exact match.
	// Codex bot review on PR #47 P2 ("Avoid shadowing logs for server
	// named open-folder"): sibling path is the cleanest fix — same
	// requireSameOrigin gate, no prefix collision, no special-case
	// reservation in the manifest validator.
	s.mux.HandleFunc("/api/logs-folder", s.requireSameOrigin(s.logsOpenFolderHandler))
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

// logsOpenFolderHandler opens the OS file manager at the daemon-log
// directory (api.DefaultLogDir resolves the canonical per-platform
// path). POST-only, same-origin gate, mkdir-on-demand for first-run
// hosts where the daemon hasn't written any log yet.
//
// Mirrors the settings.go advanced.open_app_data_folder flow (Codex
// PR #20 r3 P2): explorer / xdg-open / open all fail on a non-
// existent path, so the directory is created if missing before the
// spawn. Best-effort spawn — failures surface as 500 with a reason
// the UI can render inline.
func (s *Server) logsOpenFolderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dir := api.DefaultLogDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  "mkdir_failed",
			"reason": err.Error(),
		})
		return
	}
	if err := OpenPath(dir); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  "spawn_failed",
			"reason": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"opened": dir})
}

// streamLogs follows newly appended daemon log bytes at 500 ms intervals.
// It keeps a trailing fragment across polls until a newline arrives, and
// exits on request cancellation or after sending a provider error.

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

	// The snapshot already displayed the existing bytes. Skip them without
	// keeping its trailing fragment. A prime error is reported by the first
	// tick if the file still cannot be read.
	var offset int64
	if poll, err := s.logs.PollDaemonLog(server, daemon, offset, true); err == nil {
		offset = poll.Offset
	}
	var pendingLine string

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll, err := s.logs.PollDaemonLog(server, daemon, offset, false)
			if err != nil {
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
				flusher.Flush()
				return
			}
			if poll.Offset < offset {
				// Rotation or truncation leaves any pending fragment behind.
				pendingLine = ""
			}
			offset = poll.Offset
			if poll.Suffix == "" {
				continue
			}
			lines := strings.Split(pendingLine+poll.Suffix, "\n")
			pendingLine = lines[len(lines)-1]
			for _, line := range lines[:len(lines)-1] {
				if line != "" {
					fmt.Fprintf(w, "event: log-line\ndata: %s\n\n", line)
				}
			}

			// Emit overflow after the complete lines to preserve event order.
			if len(pendingLine) > maxPendingLineBytes {
				truncated := pendingLine[:maxPendingLineBytes]
				fmt.Fprintf(w, "event: log-line\ndata: %s [truncated unterminated line]\n\n", truncated)
				pendingLine = pendingLine[len(pendingLine)-maxPendingLineBytes:]
			}
			// Keep only the fragment's storage across polls.
			pendingLine = strings.Clone(pendingLine)
			flusher.Flush()
		}
	}
}

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
	"strconv"
	"strings"
	"time"
)

// registerLogsRoutes wires the /api/logs/ prefix. The suffix after the
// server name decides the mode — absent = snapshot, "stream" = SSE.
// Spec §4.6.
func registerLogsRoutes(s *Server) {
	s.mux.HandleFunc("/api/logs/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/logs/")
		parts := strings.SplitN(rest, "/", 2)
		server := parts[0]
		if server == "" {
			http.NotFound(w, r)
			return
		}
		streaming := len(parts) == 2 && parts[1] == "stream"
		if streaming {
			streamLogs(s, server, w, r)
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
		body, err := s.logs.Logs(server, tail)
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
// The handler exits when the client disconnects (r.Context cancellation)
// or when the provider returns an error (sent as an "error" SSE event).
func streamLogs(s *Server, server string, w http.ResponseWriter, r *http.Request) {
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
	var lastLen int
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			body, err := s.logs.Logs(server, 0)
			if err != nil {
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
				flusher.Flush()
				return
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

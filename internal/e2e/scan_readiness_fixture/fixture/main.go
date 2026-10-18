// This executable is a test-only native-HTTP MCP server for an isolated guest.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"
)

const maxEvents = 64

type event struct {
	Kind    string `json:"kind"`
	Session string `json:"session,omitempty"`
}

type fixtureSnapshot struct {
	Released    bool    `json:"released"`
	Held        int     `json:"held"`
	Canceled    int     `json:"canceled"`
	Releases    int     `json:"releases"`
	Lists       int     `json:"lists"`
	Deletes     int     `json:"deletes"`
	Outstanding int     `json:"outstanding"`
	Dropped     int     `json:"dropped_events"`
	Events      []event `json:"events"`
}

type fixture struct {
	mu       sync.Mutex
	state    fixtureSnapshot
	sessions map[string]struct{}
}

func newFixture() *fixture { return &fixture{sessions: make(map[string]struct{})} }

func (f *fixture) record(kind, session string) {
	if len(f.state.Events) == maxEvents {
		copy(f.state.Events, f.state.Events[1:])
		f.state.Events = f.state.Events[:maxEvents-1]
		f.state.Dropped++
	}
	f.state.Events = append(f.state.Events, event{Kind: kind, Session: session})
}

func (f *fixture) snapshot() fixtureSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.state
	out.Outstanding = len(f.sessions)
	out.Events = append([]event(nil), f.state.Events...)
	return out
}

func loopbackRemote(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func (f *fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/fixture/release", "/fixture/events":
		if !loopbackRemote(r) {
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/fixture/events" {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(f.snapshot())
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		if f.state.Released {
			f.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.state.Released = true
		f.state.Releases++
		f.record("released", "")
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "/mcp":
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case http.MethodDelete:
			sid := r.Header.Get("Mcp-Session-Id")
			f.mu.Lock()
			_, exists := f.sessions[sid]
			if exists {
				delete(f.sessions, sid)
				f.state.Deletes++
				f.record("deleted", sid)
			}
			f.mu.Unlock()
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			f.postMCP(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fixture) postMCP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil || request.JSONRPC != "2.0" || len(request.ID) == 0 {
		http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	switch request.Method {
	case "initialize":
		f.mu.Lock()
		closed := !f.state.Released
		if closed {
			f.state.Held++
			f.record("initialize_held", "")
		}
		f.mu.Unlock()
		if closed {
			<-r.Context().Done()
			f.mu.Lock()
			f.state.Canceled++
			f.record("initialize_canceled", "")
			f.mu.Unlock()
			return
		}
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			http.Error(w, "session creation failed", http.StatusInternalServerError)
			return
		}
		sid := hex.EncodeToString(token[:])
		f.mu.Lock()
		f.sessions[sid] = struct{}{}
		f.record("initialized", sid)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", sid)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "scan-readiness-fixture", "version": "1"}}})
	case "tools/list":
		sid := r.Header.Get("Mcp-Session-Id")
		f.mu.Lock()
		_, exists := f.sessions[sid]
		if exists {
			f.state.Lists++
			f.record("tools_list", sid)
		}
		f.mu.Unlock()
		if !exists {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"tools": []any{map[string]any{"name": "fixture_ping", "description": "Synthetic readiness tool", "inputSchema": map[string]any{"type": "object"}}}}})
	default:
		http.Error(w, "unsupported method", http.StatusBadRequest)
	}
}

func main() {
	port := flag.Int("port", 0, "guest-private internal loopback port")
	flag.Parse()
	if flag.NArg() != 0 || *port < 1 || *port > 65535 {
		log.Fatal("fixture requires one valid --port")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: newFixture(), ReadHeaderTimeout: 5 * time.Second}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, os.Interrupt)
	defer signal.Stop(stopped)
	go func() { <-stopped; _ = server.Close() }()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

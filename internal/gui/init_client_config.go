// internal/gui/init_client_config.go
//
// POST /api/init-client-config drives the v0.4.5 "Initialize <client>"
// affordance surfaced in the Servers matrix header for clients whose
// MCP config file is absent BUT whose immediate parent directory
// exists (`client_config_presence == "missing-init-possible"`).
//
// The endpoint:
//   - validates the requested client name against the known adapter
//     catalog (`clients.AllClients()`), returning 404 / UNKNOWN_CLIENT
//     for unrecognized names so a typo in the GUI cannot fan into
//     arbitrary filesystem writes;
//   - re-verifies that the parent directory still exists (defense in
//     depth against a stale scan snapshot — the user could have
//     uninstalled the client between Refresh and Init click),
//     returning 412 / PARENT_MISSING so the matrix can refresh
//     instead of seeding an empty `~/.cursor/` tree on a host
//     where the client is genuinely not installed;
//   - invokes the adapter's `InitEmpty()` which writes the
//     per-client empty stub through the SecureWriteClientConfig
//     pipeline (handle-relative + DACL-bound) and is idempotent
//     (concurrent clicks from two browser tabs converge to a
//     single empty stub via atomic rename, no corruption).
//
// The structured success response surfaces `created` (true when this
// call wrote the stub, false when the file already existed by the
// time InitEmpty checked) so the frontend can avoid an unnecessary
// post-init "we initialized your config" toast on idempotent retries.
package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/clients"
)

// initClientConfigRequest is the /api/init-client-config POST body.
type initClientConfigRequest struct {
	Client string `json:"client"`
}

// InitClientConfigResult is the structured success payload.
//
// `Path` echoes the absolute path that was checked / written so the
// frontend can render it in a confirmation toast and operators have
// a precise artifact location for `mcphub scan --refresh` follow-up.
// `Created` is true when this call's WriteConfigFile actually fired;
// false when the adapter found the file already present (idempotent
// no-op — common for double-clicks or two-tab races).
type InitClientConfigResult struct {
	Client  string `json:"client"`
	Path    string `json:"path"`
	Created bool   `json:"created"`
}

// errUnknownClient is returned when the request names a client not in
// `clients.AllClients()`. Mapped to HTTP 404 / UNKNOWN_CLIENT.
var errUnknownClient = errors.New("unknown client")

// errParentMissing is returned when the client's parent directory
// does not exist (or is not a directory). Mapped to HTTP 412 /
// PARENT_MISSING — the GUI is expected to refresh its scan.
var errParentMissing = errors.New("client config parent directory missing")

// clientInitializer is the narrow interface that the
// /api/init-client-config handler consumes. realClientInitializer
// is the production adapter; tests inject their own.
type clientInitializer interface {
	Init(client string) (*InitClientConfigResult, error)
}

type realClientInitializer struct{}

// Init looks up the adapter for `client`, verifies the immediate
// parent directory exists, and dispatches to `InitEmpty()`. The
// `Created` flag distinguishes "wrote a new empty stub" from
// "already existed (idempotent retry)" by stat'ing the path before
// the InitEmpty call.
func (realClientInitializer) Init(client string) (*InitClientConfigResult, error) {
	all := clients.AllClients()
	adapter, ok := all[client]
	if !ok {
		return nil, fmt.Errorf("%w: %q", errUnknownClient, client)
	}
	path := adapter.ConfigPath()
	parent := filepath.Dir(path)
	if parent == "" || parent == "." {
		return nil, fmt.Errorf("%w: %s", errParentMissing, path)
	}
	st, err := os.Stat(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", errParentMissing, parent)
		}
		return nil, fmt.Errorf("stat parent %s: %w", parent, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: %s exists but is not a directory", errParentMissing, parent)
	}
	// Stat the file BEFORE InitEmpty so we can distinguish a true
	// creation from an idempotent no-op. A race with concurrent
	// adapter writes can swap the outcome (we observe "missing" then
	// InitEmpty observes "present"), but the Created field is
	// advisory UI state — never a security or correctness signal —
	// so the race is acceptable.
	preexisting := false
	if _, statErr := os.Stat(path); statErr == nil {
		preexisting = true
	}
	if err := adapter.InitEmpty(); err != nil {
		return nil, fmt.Errorf("init %s: %w", client, err)
	}
	return &InitClientConfigResult{
		Client:  client,
		Path:    path,
		Created: !preexisting,
	}, nil
}

func registerInitClientConfigRoutes(s *Server) {
	s.mux.HandleFunc("/api/init-client-config", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req initClientConfigRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if req.Client == "" {
			writeAPIError(w, errors.New("client field is required"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		result, err := s.clientInit.Init(req.Client)
		if err != nil {
			switch {
			case errors.Is(err, errUnknownClient):
				writeAPIError(w, err, http.StatusNotFound, "UNKNOWN_CLIENT")
			case errors.Is(err, errParentMissing):
				writeAPIError(w, err, http.StatusPreconditionFailed, "PARENT_MISSING")
			default:
				writeAPIError(w, err, http.StatusInternalServerError, "INIT_FAILED")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}))
}

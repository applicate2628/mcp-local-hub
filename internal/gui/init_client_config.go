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

	"mcp-local-hub/internal/api"
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

// errStrictModeRefused is returned when MCPHUB_REQUIRE_SINGLE_USER_HOME=1
// is set. The Init endpoint refuses to seed an empty stub through the
// non-hardened temp+hardlink path because strict-mode operators
// explicitly opted into the SecureWriteClientConfig pipeline's
// parent-DACL gate + allowlist-only file DACL. Mapped to HTTP 412 /
// STRICT_MODE_REFUSED so the frontend surfaces the operational code.
// See CLAUDE.md "Init-time stubs" subsection for the v0.4.6+
// follow-up plan.
var errStrictModeRefused = errors.New("strict-mode refused")

// clientInitializer is the narrow interface that the
// /api/init-client-config handler consumes. realClientInitializer
// is the production adapter; tests inject their own.
type clientInitializer interface {
	Init(client string) (*InitClientConfigResult, error)
}

type realClientInitializer struct{}

// Init looks up the adapter for `client`, refuses if strict-mode
// hardening is enabled (v0.4.5 deep-sec Lane C #1 — see CLAUDE.md
// "Hardened client-config writes" section for the gap rationale),
// verifies the immediate parent directory exists, and dispatches to
// `adapter.InitEmpty()` which returns an honest "created" flag (true
// iff this call wrote the stub bytes; false iff a regular file was
// already present — covers second-click and publish-race-lost cases
// without the pre-stat race window of the previous implementation).
func (realClientInitializer) Init(client string) (*InitClientConfigResult, error) {
	all := clients.AllClients()
	adapter, ok := all[client]
	if !ok {
		return nil, fmt.Errorf("%w: %q", errUnknownClient, client)
	}
	// v0.4.5 deep-sec Lane C #1 closure: in strict mode the operator
	// has explicitly opted into the SecureWriteClientConfig pipeline's
	// parent-DACL gate + allowlist-DACL on every write. The Init
	// helper bypasses that pipeline because it has to (atomic
	// create-new is mutually exclusive with the pipeline's replacing
	// rename), so in strict mode the Init affordance silently
	// degrades to a non-hardened path — non-allowlisted principals
	// inheriting write rights from the parent could then modify the
	// newly seeded config and inject client-consumed MCP entries.
	// Refuse explicitly until v0.4.6+ ships a strict-mode-compatible
	// SecureCreateClientConfigIfMissing helper. The operator still
	// has `mcphub register` / `mcphub install` which route through
	// the hardened pipeline.
	// Use the canonical strict-mode predicate from internal/api so the
	// endpoint accepts the same env-var values the secure-write
	// pipeline does ("1" or "true", case-insensitive, trimmed). A
	// narrow `== "1"` check would fail-open on values the canonical
	// reader treats as enabled. Deep-sec PR #208 Lane C #2 closure.
	if api.OperatorRequiresSingleUserHome() {
		return nil, fmt.Errorf(
			"%w: strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME enabled) is active; "+
				"Initialize affordance routes through a strict-mode-compatible "+
				"SecureCreateClientConfigIfMissing helper that ALSO enforces "+
				"the parent-DACL gate. Use `mcphub register` or `mcphub install` "+
				"to seed %s through the hardened SecureWriteClientConfig pipeline.",
			errStrictModeRefused, client,
		)
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
	created, initErr := adapter.InitEmpty()
	if initErr != nil {
		// Deep-sec PR #208 Lane A round 3 P2 closure: the round 2 fix
		// correctly stopped the adapter from silently re-creating a
		// deleted parent (MkdirAll removed from EnsureClientConfigStub),
		// but a parent that disappears between the endpoint pre-stat
		// and the adapter call now surfaces ENOENT from
		// `os.CreateTemp(parent, ...)`. The handler's default branch
		// would map that to 500 INIT_FAILED, suggesting an internal
		// server bug to the operator; the accurate status is 412
		// PARENT_MISSING, signaling that the operator should refresh
		// the scan and either pick a different client or restore the
		// parent dir before retrying.
		if os.IsNotExist(initErr) {
			return nil, fmt.Errorf(
				"%w: parent %s disappeared between stat and init: %v",
				errParentMissing, parent, initErr,
			)
		}
		return nil, fmt.Errorf("init %s: %w", client, initErr)
	}
	return &InitClientConfigResult{
		Client:  client,
		Path:    path,
		Created: created,
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
			case errors.Is(err, errStrictModeRefused):
				writeAPIError(w, err, http.StatusPreconditionFailed, "STRICT_MODE_REFUSED")
			default:
				writeAPIError(w, err, http.StatusInternalServerError, "INIT_FAILED")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}))
}

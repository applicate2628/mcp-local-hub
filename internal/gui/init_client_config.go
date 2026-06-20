// internal/gui/init_client_config.go
//
// POST /api/init-client-config drives the v0.4.5 "Initialize <client>"
// affordance surfaced in the Servers matrix header for clients whose
// MCP config file is absent. It serves both the legacy
// `missing-init-possible` state (parent directory already exists) and
// the G17 `missing-init-creatable` state (parent directory absent but
// the path is under the user home and securely creatable).
//
// The endpoint:
//   - validates the requested client name against the known adapter
//     catalog (`clients.AllClients()`), returning 404 / UNKNOWN_CLIENT
//     for unrecognized names so a typo in the GUI cannot fan into
//     arbitrary filesystem writes;
//   - re-inspects the immediate parent directory (defense in depth
//     against a stale scan snapshot):
//   - parent is a real directory → proceed (legacy path);
//   - parent is a symlink / non-directory → return 412 /
//     PARENT_MISSING so the matrix can refresh instead of seeding
//     through an attacker-followable reparse point;
//   - parent is ABSENT → securely create the missing parent chain
//     (G17: component-by-component, symlink-refusing, bounded to the
//     user home) via api.SecureCreateClientConfigParentDirWithOperatorOpt,
//     then proceed. A refusal (symlink/non-dir component, path
//     outside home, strict-mode broadened home anchor) maps to 500 /
//     INIT_FAILED with the actionable underlying message;
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

// clientInitializer is the narrow interface that the
// /api/init-client-config handler consumes. realClientInitializer
// is the production adapter; tests inject their own.
type clientInitializer interface {
	Init(client string) (*InitClientConfigResult, error)
}

type realClientInitializer struct{}

// Init looks up the adapter for `client`, verifies the immediate
// parent directory exists, and dispatches to `adapter.InitEmpty()`
// which returns an honest "created" flag (true iff this call wrote
// the stub bytes; false iff a regular file was already present —
// covers second-click and publish-race-lost cases without the
// pre-stat race window of the previous implementation).
//
// v0.4.5 PR #208 codex r1 F1 closure: the prior implementation
// short-circuited in strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1)
// with a STRICT_MODE_REFUSED error. That refusal was load-bearing
// when the Init pipeline used a non-hardened temp+hardlink path,
// but the Lane C #1 closure (commit 4be7b9d / `SecureCreateClientConfigIfMissing`)
// now routes adapter `InitEmpty()` through a fully hardened
// handle-relative pipeline that enforces the SAME parent-DACL gate
// as `SecureWriteClientConfig` — see internal/api/secure_create_client_config.go
// and its Windows/POSIX impl. In strict mode the hardened pipeline
// returns `ErrSecureWriteParentInsecure` on broadened parents, which
// the `secureCreateClientConfigIfMissingWithOperatorOpt` wrapper in
// internal/api/client_write_init.go propagates to the GUI as
// INIT_FAILED with the strict-mode message. Strict-mode operators
// with owner-only parent DACLs now see the Initialize affordance
// work as expected, instead of an unconditional "use the CLI"
// fallback that made the matrix UX inconsistent with the actual
// write contract.
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
	lst, err := os.Lstat(parent)
	switch {
	case err == nil && lst.Mode()&os.ModeSymlink != 0:
		// Parent is a symlink — the hardened pipeline refuses to follow
		// it. Keep the 412 PARENT_MISSING contract (the GUI suppresses
		// the affordance for this state; a stale-scan click lands here).
		return nil, fmt.Errorf("%w: %s is a symlink (init must not follow symlinks)", errParentMissing, parent)
	case err == nil && !lst.IsDir():
		return nil, fmt.Errorf("%w: %s exists but is not a directory", errParentMissing, parent)
	case err == nil:
		// Parent is a real directory — proceed to InitEmpty directly
		// (the legacy missing-init-possible path, behavior-identical).
	case os.IsNotExist(err):
		// G17: the parent directory is absent. Securely create the
		// missing parent chain (component-by-component, symlink-refusing,
		// home-bounded) so a not-yet-installed client can be pre-
		// configured. The secure create refuses a symlinked / non-dir
		// component or a path outside the user home; those refusals
		// surface here and map to INIT_FAILED (default branch) so the
		// operator sees the actionable message. Strict mode is honored
		// by the same wrapper as the file create.
		if mkErr := api.SecureCreateClientConfigParentDirWithOperatorOpt(path); mkErr != nil {
			return nil, fmt.Errorf("create parent for %s: %w", client, mkErr)
		}
	default:
		return nil, fmt.Errorf("stat parent %s: %w", parent, err)
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
		if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
			writeDecodeBodyError(w, err, "BAD_REQUEST")
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
				// v0.4.5 PR #208 codex r1 F1: strict-mode broadened-parent
				// rejections from SecureCreateClientConfigIfMissing surface
				// here (wrapped through secureCreateClientConfigIfMissingWithOperatorOpt's
				// strict-error path) as INIT_FAILED with the canonical
				// strict-mode message describing the parent-DACL gate.
				writeAPIError(w, err, http.StatusInternalServerError, "INIT_FAILED")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}))
}

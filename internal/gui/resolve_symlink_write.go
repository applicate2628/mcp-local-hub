// internal/gui/resolve_symlink_write.go
//
// POST /api/resolve-symlink-and-write drives the A3 PR-2 "Resolve symlink →
// write to real target" affordance the Servers matrix attaches to a
// `config-error-symlink` cell. Without the MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK
// env opt-in, mcphub refuses to follow a symlinked client config (confused-
// deputy protection, PR #209), so the cell is disabled. This endpoint is the
// explicit, per-config GUI ENABLE the operator asked for — no restart, no
// global env var.
//
// It mirrors init_client_config.go's structure: same-origin guard
// (requireSameOrigin), client-name allowlist via clients.AllClients() (a typo
// or adversarial name cannot fan into arbitrary filesystem writes), and a
// structured JSON result. It is a two-phase contract gated by `confirm`:
//
//   - confirm=false (RESOLVE): resolve the client's symlinked config, PIN the
//     full resolved real target path, and return {pinnedRealPath, contentHash}
//     for the confirm modal. NO write.
//   - confirm=true (WRITE): re-resolve server-side; refuse if the freshly-
//     resolved full target != the operator-confirmed pinnedRealPath (the
//     symlink was repointed between confirm and click — anti-TOCTOU, incl. a
//     same-parent repoint); read the target's
//     CURRENT raw bytes server-side (content is HOST-sourced, never browser-
//     supplied); refuse if the content hash drifted from the one the operator
//     saw (concurrent external edit); then perform a byte-exact round-trip
//     write through api.SecureWriteClientConfigWithConsent. PR-1's entry
//     re-resolves AGAIN and re-verifies the pin under the held handle, so even
//     this endpoint's own re-resolve cannot be trusted to redirect the write.
//
// Semantic (architect SEAM-C ruling S1): the write is a CONTENT-NEUTRAL
// proof-and-enable round-trip — the SAME bytes are written back, so no config
// is mutated, but the consent-bearing write SUCCEEDS where the default refusal
// blocked it, and a rescan re-classifies the cell from `config-error-symlink`
// to "ok". A desirable side effect: the round-trip re-stamps the target's DACL
// owner-only (hardening a dotfile target created outside mcphub).
//
// Strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) overrides the consent: the
// write facade refuses, and the affordance surfaces that refusal rather than a
// follow. The endpoint never bypasses strict.
package gui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// resolveSymlinkWriteRequest is the POST body. `Confirm` selects the phase.
// `PinnedRealPath` and `ContentHash` are carried back from the RESOLVE phase
// into the WRITE phase by the client (no server-side session state — the pin
// travels in the body and is re-verified server-side).
type resolveSymlinkWriteRequest struct {
	Client         string `json:"client"`
	Confirm        bool   `json:"confirm"`
	PinnedRealPath string `json:"pinned_real_path"`
	ContentHash    string `json:"content_hash"`
}

// ResolveSymlinkResult is the RESOLVE-phase (confirm=false) payload: what the
// confirm modal renders. `OriginalPath` is the symlink the operator pointed
// at; `ResolvedTarget` is the real file; `PinnedRealPath` is the FULL resolved
// target path the operator consents to (now equal to ResolvedTarget — shown ==
// pinned; the value to echo back in the WRITE phase). `ContentHash` is the
// sha256 of the target's current bytes, echoed back so the WRITE phase can
// refuse on a concurrent external edit.
type ResolveSymlinkResult struct {
	Client         string `json:"client"`
	OriginalPath   string `json:"original_path"`
	ResolvedTarget string `json:"resolved_target"`
	PinnedRealPath string `json:"pinned_real_path"`
	ContentHash    string `json:"content_hash"`
	IsSymlink      bool   `json:"is_symlink"`
}

// WriteSymlinkResult is the WRITE-phase (confirm=true) success payload.
type WriteSymlinkResult struct {
	Client       string `json:"client"`
	OriginalPath string `json:"original_path"`
	WrittenPath  string `json:"written_path"`
	Written      bool   `json:"written"`
}

// errSymlinkNotApplicable is returned when the client's config path is NOT a
// follow-able symlink (regular file, absent, or unresolvable). Mapped to 412 /
// NOT_SYMLINK so the GUI refreshes its scan instead of offering a follow.
var errSymlinkNotApplicable = errors.New("client config is not a follow-able symlink")

// errSymlinkRepointed is returned when the freshly-resolved full target at
// WRITE time does not match the operator-confirmed PinnedRealPath (including a
// repoint to a sibling file in the same parent). Mapped to 409 /
// SYMLINK_REPOINTED.
var errSymlinkRepointed = errors.New("symlink resolved target changed since confirm")

// errConfigChanged is returned when the target's content hash drifted between
// the RESOLVE phase and the WRITE phase (a concurrent external edit). Mapped
// to 409 / CONFIG_CHANGED.
var errConfigChanged = errors.New("client config changed since confirm")

// symlinkResolveWriter is the narrow interface the handler consumes;
// realSymlinkResolveWriter is the production adapter, tests inject their own.
type symlinkResolveWriter interface {
	Resolve(client string) (*ResolveSymlinkResult, error)
	Write(client, pinnedRealPath, contentHash string) (*WriteSymlinkResult, error)
}

type realSymlinkResolveWriter struct{}

// hashBytes returns the lowercase-hex sha256 of b — the content-drift token
// shared between the RESOLVE and WRITE phases.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// lookupClientConfigPath validates the client against the adapter catalog and
// returns its config path. The allowlist is the same one init_client_config.go
// uses — an unknown name never reaches the filesystem.
func lookupClientConfigPath(client string) (string, error) {
	adapter, ok := clients.AllClients()[client]
	if !ok {
		return "", fmt.Errorf("%w: %q", errUnknownClient, client)
	}
	return adapter.ConfigPath(), nil
}

// Resolve (RESOLVE phase) resolves the client's symlinked config and pins the
// resolved real target's parent, returning the data the confirm modal renders.
// It does NOT write.
func (realSymlinkResolveWriter) Resolve(client string) (*ResolveSymlinkResult, error) {
	path, err := lookupClientConfigPath(client)
	if err != nil {
		return nil, err
	}
	resolvedTarget, pinnedPath, isSymlink := api.ResolveClientConfigSymlink(path)
	if !isSymlink {
		return nil, fmt.Errorf("%w: %s", errSymlinkNotApplicable, path)
	}
	// Read the resolved target's CURRENT bytes server-side to compute the
	// content-drift token shown to the operator. The browser never supplies
	// content. A read failure here means the symlink is dangling or the target
	// is unreadable — treat as not-applicable so the GUI rescans.
	body, rerr := os.ReadFile(resolvedTarget)
	if rerr != nil {
		return nil, fmt.Errorf("%w: resolved target %s unreadable: %v", errSymlinkNotApplicable, resolvedTarget, rerr)
	}
	return &ResolveSymlinkResult{
		Client:         client,
		OriginalPath:   path,
		ResolvedTarget: resolvedTarget,
		PinnedRealPath: pinnedPath,
		ContentHash:    hashBytes(body),
		IsSymlink:      true,
	}, nil
}

// Write (WRITE phase) re-resolves server-side, refuses on a pin or content
// drift, then performs a byte-exact round-trip write of the target's CURRENT
// content through the consent-bearing PR-1 entry. The consent's
// PinnedResolvedPath is the operator-confirmed pinnedRealPath; PR-1's entry
// re-resolves AGAIN under the held handle and refuses if reality diverged.
func (realSymlinkResolveWriter) Write(client, pinnedRealPath, contentHash string) (*WriteSymlinkResult, error) {
	path, err := lookupClientConfigPath(client)
	if err != nil {
		return nil, err
	}
	// Re-resolve at write time (defense in depth — the symlink could have been
	// repointed between the confirm modal and this POST). The pin is the FULL
	// resolved target path, so a same-parent repoint (link -> other.json in the
	// SAME dir) is caught here too, not just a parent change.
	resolvedTarget, livePinnedPath, isSymlink := api.ResolveClientConfigSymlink(path)
	if !isSymlink {
		return nil, fmt.Errorf("%w: %s", errSymlinkNotApplicable, path)
	}
	if filepath.Clean(pinnedRealPath) != livePinnedPath {
		return nil, fmt.Errorf("%w: confirmed %q, now resolves to %q", errSymlinkRepointed, pinnedRealPath, livePinnedPath)
	}
	// Read the target's CURRENT raw bytes (HOST-sourced) — do NOT parse and
	// re-marshal (that would silently reformat / drop comments / reorder keys).
	body, rerr := os.ReadFile(resolvedTarget)
	if rerr != nil {
		return nil, fmt.Errorf("%w: resolved target %s unreadable: %v", errSymlinkNotApplicable, resolvedTarget, rerr)
	}
	// Concurrent-edit guard: refuse if the content drifted from what the
	// operator saw in the modal.
	if contentHash != "" && hashBytes(body) != contentHash {
		return nil, fmt.Errorf("%w: content of %s changed since the confirm modal was shown; rescan and retry", errConfigChanged, resolvedTarget)
	}
	consent := api.ResolvedSymlinkConsent{
		Client:             client,
		OriginalPath:       path,
		PinnedResolvedPath: livePinnedPath,
	}
	if werr := api.SecureWriteClientConfigWithConsent(consent, body); werr != nil {
		return nil, fmt.Errorf("write %s via consent: %w", client, werr)
	}
	return &WriteSymlinkResult{
		Client:       client,
		OriginalPath: path,
		WrittenPath:  resolvedTarget,
		Written:      true,
	}, nil
}

func registerResolveSymlinkWriteRoutes(s *Server) {
	s.mux.HandleFunc("/api/resolve-symlink-and-write", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req resolveSymlinkWriteRequest
		if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
			writeDecodeBodyError(w, err, "BAD_REQUEST")
			return
		}
		if req.Client == "" {
			writeAPIError(w, errors.New("client field is required"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}

		if !req.Confirm {
			// RESOLVE phase.
			res, err := s.symlinkWriter.Resolve(req.Client)
			if err != nil {
				writeResolveSymlinkError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(res)
			return
		}

		// WRITE phase — pinnedRealPath is mandatory (it is the anti-TOCTOU
		// pin the operator confirmed).
		if req.PinnedRealPath == "" {
			writeAPIError(w, errors.New("pinned_real_path is required to confirm the write"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		// content_hash is mandatory too (defense-in-depth): it is the
		// concurrent-edit drift token the operator saw in the confirm modal.
		// The GUI always sends it; requiring it here stops a hand-crafted
		// request from OMITTING it to skip the drift guard in Write(). PR-1's
		// pin re-verify is the real security boundary, so this is belt-and-
		// suspenders, not the fence.
		if req.ContentHash == "" {
			writeAPIError(w, errors.New("content_hash is required to confirm the write"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		res, err := s.symlinkWriter.Write(req.Client, req.PinnedRealPath, req.ContentHash)
		if err != nil {
			writeResolveSymlinkError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}))
}

// writeResolveSymlinkError maps the typed errors to HTTP status + code. The
// strict-mode refusal and any other secure-write failure fall through to 500 /
// WRITE_REFUSED carrying the underlying (path-bearing but non-secret) message
// so the GUI can render an actionable diagnostic — identical posture to
// init_client_config.go's INIT_FAILED.
func writeResolveSymlinkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errUnknownClient):
		writeAPIError(w, err, http.StatusNotFound, "UNKNOWN_CLIENT")
	case errors.Is(err, errSymlinkNotApplicable):
		writeAPIError(w, err, http.StatusPreconditionFailed, "NOT_SYMLINK")
	case errors.Is(err, errSymlinkRepointed):
		writeAPIError(w, err, http.StatusConflict, "SYMLINK_REPOINTED")
	case errors.Is(err, errConfigChanged):
		writeAPIError(w, err, http.StatusConflict, "CONFIG_CHANGED")
	default:
		// Strict-mode refusal and other secure-write failures. The message
		// carries the canonical strict-mode hint when applicable.
		writeAPIError(w, err, http.StatusInternalServerError, "WRITE_REFUSED")
	}
}

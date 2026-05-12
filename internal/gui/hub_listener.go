// hub_listener.go — Phase 4 Task 4.4 (G4 unified hub MCP).
//
// Owns the hub-mcp listener lifecycle, separate from the gui-server
// listener. Called by Server.Start after the gui-server listener
// binds and after the gui_server.hub_endpoint_enabled gate evaluates
// to true. The hub listener is a SEPARATE socket at the operator-
// chosen port (persisted in hub-mcp.endpoint.json across restarts);
// the existing per-daemon URLs are unaffected by Phase 4.
//
// Bind ordering (spec §"Bind ordering"):
//
//   1. Validate <state-dir> sanity (handled by DaemonStateDir).
//   2. flock(hub-mcp.lock) — implicit in the EnsureHubTokens +
//      EnsureHubEndpoint helpers.
//   3. Load OR generate hub-mcp-tokens.json (EnsureHubTokens).
//   4. Load existing hub-mcp.endpoint.json (LoadHubEndpoint).
//   5. Validate participating manifests in strict mode
//      (ManifestValidateForHubBind).
//   6. listener := NewListenerWithSOExclusive("127.0.0.1:<port>").
//   7. Write hub-mcp.endpoint.json with the OS-assigned port if
//      step 4 returned port=0 (EnsureHubEndpoint).
//   8. funlock — implicit.
//   9. Serve.
//
// Step 7 is the load-bearing crash-safety hop: if it fails AFTER the
// listener exists, we Close the listener so no traffic is accepted
// without a published endpoint file.
//
// Bind-failure handling: a port-in-use error during step 6 triggers
// the credential-rotation warning per spec §"Pre-bind handling" —
// a same-user local process may have pre-bound the port to harvest
// the token bytes a future client would have sent.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Bind ordering".
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 4.4.

package gui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// HubListenerComponents bundles the resources the hub listener owns
// across its lifetime. Bound to Server in Server.Start so Shutdown
// can tear them down on context cancellation.
type HubListenerComponents struct {
	srv     *http.Server
	store   *api.HubSessionStore
	handler *api.HubMcpHandler
	reload  *api.InternalReloadHandler
	port    int
}

// readHubEndpointGateFromSettings reads gui_server.hub_endpoint_enabled
// from the gui-preferences.yaml file directly. The settings registry
// does not yet carry this key (Phase 5 adds it); we bypass the
// registry to keep Phase 4 free of cross-task settings work.
//
// Returns false when the file is absent, the key is absent, the value
// is not exactly "true", or the YAML is malformed. This is the
// fail-closed posture spec §"Settings + CLI surface" demands — a
// corrupt settings file MUST NOT silently flip the gate on.
func readHubEndpointGateFromSettings() bool {
	path := api.SettingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	raw := map[string]string{}
	if uerr := yaml.Unmarshal(data, &raw); uerr != nil {
		return false
	}
	return raw["gui_server.hub_endpoint_enabled"] == "true"
}

// startHubMcpListener implements the spec §"Bind ordering" steps 1-9.
// Called from Server.Start AFTER the gui-server listener is up. The
// `enabled` argument lets the caller short-circuit without inspecting
// the settings file (tests inject the gate directly).
//
// Returns a HubListenerComponents bundle on success. The returned
// srv is already serving in a background goroutine; the store has its
// own sweep goroutine. The caller MUST call ShutdownHubListener on
// ctx cancellation to drain both.
//
// On bind failure during step 6, surfaces the underlying net error +
// emits the credential-rotation warning via LogHubMcpEvent. The
// caller's gui-server keeps running (gate-OFF semantics for the rest
// of this process lifetime); the operator must investigate and run
// the rotation CLI before the next start.
//
// On step-7 write failure, closes the listener before returning the
// error so no traffic is accepted without a published endpoint file
// (spec §"Bind ordering" rollback note).
func startHubMcpListener(ctx context.Context, enabled bool, a *api.API) (*HubListenerComponents, error) {
	if !enabled {
		return nil, nil
	}

	// Step 3 — load / generate per-client tokens. EnsureHubTokens
	// holds hub-mcp.lock internally. We pass the full supported set
	// so any client added since the last hub start gets a token
	// pre-generated on this start. Phase 5's install reconciler later
	// publishes the tokens into the matching client configs.
	if _, err := api.EnsureHubTokens(clients.SupportedClientNames()); err != nil {
		return nil, fmt.Errorf("hub-mcp: ensure tokens: %w", err)
	}

	// Step 4 — load existing endpoint, capture port + instance_id.
	// Missing file is acceptable (first-start path); other errors
	// surface (DACL / parse / partial-write recovery).
	ep, err := api.LoadHubEndpoint()
	if err != nil && !os.IsNotExist(err) && !isMissingHubEndpoint(err) {
		return nil, fmt.Errorf("hub-mcp: load endpoint: %w", err)
	}

	// Step 5 — validate participating manifests in strict mode. If
	// no manifests are loaded the hub can still bind (operator may
	// be configuring the hub from scratch); a STRICT violation in
	// any one manifest refuses the bind entirely (spec §"Pre-gate").
	if verr := validateParticipatingManifestsForHubBind(a); verr != nil {
		return nil, fmt.Errorf("hub-mcp bind refused: %w", verr)
	}

	// Step 6 — bind. Pass port=0 to let the OS pick if the persisted
	// port was zero (first start OR `mcphub gui --reset-port`).
	bindAddr := fmt.Sprintf("127.0.0.1:%d", ep.Port)
	ln, lnErr := api.NewListenerWithSOExclusive(bindAddr)
	if lnErr != nil {
		_ = api.LogHubMcpEvent("error", "hub-bind-failed", map[string]any{
			"port": ep.Port,
			"err":  lnErr.Error(),
		})
		// Spec §"Pre-bind handling": port-in-use on the persisted port
		// is indistinguishable from a credential-harvest attack. The
		// operator-facing recovery is the rotation chain documented
		// in §"Bind ordering". One log line; the caller surfaces the
		// error to the rest of gui-server startup.
		_ = api.LogHubMcpEvent("warn", "credential-rotation-required", map[string]any{
			"reason": "pre-bind window — credentials may have leaked to pre-binding process",
		})
		return nil, fmt.Errorf("hub-mcp: bind %s: %w", bindAddr, lnErr)
	}

	// Step 7 — write endpoint.json with the OS-assigned port.
	port := ln.Addr().(*net.TCPAddr).Port
	if _, eerr := api.EnsureHubEndpoint(port, os.Getpid()); eerr != nil {
		// Step-7 failure with the listener already open: defer-close
		// so no traffic is accepted without a published endpoint
		// file. This is the spec §"Bind ordering" rollback rule.
		_ = ln.Close()
		return nil, fmt.Errorf("hub-mcp: write endpoint.json: %w", eerr)
	}

	// Re-load the endpoint record so the handler's SetEndpoint call
	// carries the post-write instance_id (which EnsureHubEndpoint may
	// have generated on first start).
	ep, err = api.LoadHubEndpoint()
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("hub-mcp: reload endpoint: %w", err)
	}

	// Step 9 — set up mux + serve. Session store is per-listener so
	// a future "stop hub mode" path can tear down sessions cleanly.
	store := api.NewHubSessionStore(api.SessionStoreOpts{})
	handler := api.NewHubMcpHandler(store)
	handler.SetEndpoint(ep)
	reload := api.NewInternalReloadHandler()

	mux := http.NewServeMux()
	// /clients/ catches everything under /clients/<id>/mcp; the
	// handler's path parser validates the trailing /mcp + the client
	// id (gate 2).
	mux.Handle("/clients/", handler)
	mux.Handle("/internal/reload-tokens", reload)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = srv.Serve(ln)
	}()

	_ = api.LogHubMcpEvent("info", "hub-listener-up", map[string]any{
		"port": port,
	})

	return &HubListenerComponents{
		srv:     srv,
		store:   store,
		handler: handler,
		reload:  reload,
		port:    port,
	}, nil
}

// ShutdownHubListener drains the hub listener under a 5s timeout +
// closes the session store's sweep goroutine + removes the control
// token file. Idempotent: calling with a nil components is a no-op.
//
// Step-7 write failure means components may be nil; the caller's
// gui-server shutdown still runs but the hub side has nothing to
// drain.
func ShutdownHubListener(parentCtx context.Context, c *HubListenerComponents) {
	if c == nil {
		return
	}
	shutCtx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()
	if c.srv != nil {
		_ = c.srv.Shutdown(shutCtx)
	}
	if c.store != nil {
		c.store.Close()
	}
	if c.reload != nil {
		_ = c.reload.Shutdown()
	}
}

// validateParticipatingManifestsForHubBind iterates every server
// manifest known to this API instance + checks strict-mode validity.
// Returns the first failing manifest's error (one violation is
// enough to refuse the bind per spec §"Pre-gate"). An empty manifest
// set is acceptable (operator may be running gui without manifests).
//
// Wraps api.API.ManifestValidateForHubBind so callers in this file
// share the same gate.
func validateParticipatingManifestsForHubBind(a *api.API) error {
	if a == nil {
		return nil
	}
	scan, err := a.Scan()
	if err != nil {
		return fmt.Errorf("scan manifests: %w", err)
	}
	for _, entry := range scan.Entries {
		if !entry.ManifestExists {
			continue
		}
		yaml, _, gerr := a.ManifestGetWithHash(entry.Name)
		if gerr != nil {
			// codex bot phase4 r1 P1 closure on PR #158: only the
			// scan-window race (manifest deleted between scan and read)
			// is benign. Permission errors, schema errors, or any
			// other read failure MUST fail the bind so the strict
			// pre-gate isn't silently bypassed. The race signature is
			// fs.ErrNotExist via errors.Is — anything else propagates.
			if errors.Is(gerr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("manifest %q: read: %w", entry.Name, gerr)
		}
		if verr := a.ManifestValidateForHubBind(yaml); verr != nil {
			return fmt.Errorf("manifest %q: %w", entry.Name, verr)
		}
	}
	return nil
}

// isMissingHubEndpoint is true if err describes a "hub-mcp.endpoint.json
// does not yet exist" case. Endpoint helpers return wrapped errors;
// the wrapper inspects the underlying cause without leaking the
// secure-write error chain (which carries paths we'd otherwise need
// to redact).
func isMissingHubEndpoint(err error) bool {
	// Best-effort: rely on os.IsNotExist via errors.Is for the most
	// common case. The endpoint helpers wrap os.PathError via
	// fmt.Errorf("%w", ...) so the Is chain matches.
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return false
}

// hubControlTokenPath returns the absolute path to
// <state-dir>/hub-mcp-control.token. Test helpers use this to verify
// the file exists post-construction and disappears post-Shutdown.
// Production callers should NOT read this file outside the rotation
// CLI's flock-protected pipeline.
func hubControlTokenPath() (string, error) {
	dir, err := api.DaemonStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub-mcp-control.token"), nil
}

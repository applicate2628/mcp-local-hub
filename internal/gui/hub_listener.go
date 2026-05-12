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
	"net/http"
	"os"
	"path"
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

	// codex bot phase4 r10 P1 closure on PR #158: short-circuit if
	// ctx is already canceled BEFORE we touch hub-mcp.lock. The pre-r10
	// code would still try to acquire the lock (blocking flock) even
	// though the caller (Server.Start) already gave up — the goroutine
	// could come back later, bind a listener, and store it in
	// hubMcpComp AFTER Server.Start returned. Fail fast instead.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("hub-mcp: %w", err)
	}

	// Steps 3-7 — lock-atomic bind transaction. codex deep-sec P1
	// closure on PR #158: the pre-r2 implementation called
	// EnsureHubTokens (locked), LoadHubEndpoint (no lock),
	// validateManifests (no lock), bind (no lock), EnsureHubEndpoint
	// (locked) — leaving four lock-free windows in between. Two
	// concurrent gui-server starts could both clear the same
	// persisted port=0 record, both bind distinct OS-assigned
	// ports, and race the endpoint persist. BindHubMcpListener
	// holds hub-mcp.lock across the entire sequence so concurrent
	// starts serialize. SO_EXCLUSIVEADDRUSE (Windows) + ordinary
	// loopback semantics (POSIX) make the second caller's bind fail
	// fast against the first caller's already-persisted port.
	//
	// codex bot phase4 r10 P1 closure on PR #158: ctx is threaded
	// into BindHubMcpListener so the lock acquisition itself is
	// bounded — under contention with a sibling CLI holding
	// hub-mcp.lock, ctx.Done() unwinds the bind transaction within
	// ~10 ms (acquireHubMcpLockContext retry cadence). Without this,
	// the goroutine would block on flock indefinitely past Start's
	// shutdown budget.
	res, berr := api.BindHubMcpListener(
		ctx,
		clients.SupportedClientNames(),
		func() error { return validateParticipatingManifestsForHubBind(a) },
	)
	if berr != nil {
		return nil, fmt.Errorf("hub-mcp: %w", berr)
	}
	ln := res.Listener
	ep := res.Endpoint
	port := ep.Port

	// Step 9 — set up mux + serve. Session store is per-listener so
	// a future "stop hub mode" path can tear down sessions cleanly.
	store := api.NewHubSessionStore(api.SessionStoreOpts{})
	handler := api.NewHubMcpHandler(store)
	handler.SetEndpoint(ep)
	// codex bot phase4 r5 P2 closure on PR #158: control-token
	// persistence failure surfaces as a returned error so the hub
	// listener refuses to come up with a silently-broken
	// /internal/reload-tokens. Rollback the bound listener so we
	// don't leave a half-initialized hub server running.
	reload, reloadErr := api.NewInternalReloadHandler(ctx)
	if reloadErr != nil {
		_ = ln.Close()
		store.Close()
		// codex bot phase4 r8 P2 closure on PR #158: roll back the
		// persisted endpoint port so status / consumers of
		// hub-mcp.endpoint.json don't discover a dead port + PID
		// pair until the next successful startup. ResetHubPort sets
		// Port=0 and preserves StartedAt + InstanceID (port-only
		// mutation per its own r2 P2 closure). If the rollback
		// itself fails (e.g. flock contention with a sibling CLI),
		// surface that as an additional log line — the operator
		// must then manually `mcphub gui --reset-port` to clear the
		// stale endpoint.
		//
		// codex bot phase4 r12 P1 closure on PR #158: use the
		// ctx-aware variant so a sibling holder of hub-mcp.lock can
		// not freeze the rollback past Server.Start's shutdown
		// budget. If ctx is already canceled (the typical r10 race
		// — gui-server gave up on hub init), the rollback fails
		// fast with context.Canceled; the dead port + PID pair stays
		// on disk and the operator runs `mcphub gui --reset-port`
		// manually. That's strictly better than the goroutine
		// blocking past Start's return and mutating state after.
		if rerr := api.ResetHubPortContext(ctx); rerr != nil {
			_ = api.LogHubMcpEvent("error", "hub-endpoint-rollback-failed", map[string]any{
				"err": rerr.Error(),
			})
		}
		return nil, fmt.Errorf("hub-mcp: %w", reloadErr)
	}

	mux := http.NewServeMux()
	// /clients/ catches everything under /clients/<id>/mcp; the
	// handler's path parser validates the trailing /mcp + the client
	// id (gate 2).
	mux.Handle("/clients/", handler)
	// codex bot phase4 r12 P2 closure on PR #158: also register the
	// bare /clients pattern (no trailing slash) so ServeMux does NOT
	// auto-301 /clients → /clients/ before the auth/path gates run.
	// The 301 would (a) emit a redirect response outside the
	// handler's 404-empty contract and (b) some redirect-following
	// clients may rewrite POST as GET on the redirect step,
	// silently dropping the JSON-RPC body. Returning 404 directly
	// matches the handler's gate-2 path-shape contract.
	//
	// codex bot phase4 r13 P2 closure on PR #158: use plain
	// WriteHeader(404) with no body. http.NotFound writes
	// "404 page not found\n" which breaks the handler's empty-body
	// 404 contract for all other gate-2 path-shape failures. Now
	// every gate-2 path-shape rejection returns the SAME shape:
	// empty body, status 404. Eliminates route-fingerprinting and
	// keeps callers from differentiating /clients vs /clients/foo.
	mux.HandleFunc("/clients", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.Handle("/internal/reload-tokens", reload)

	// codex bot phase4 r23 P2 closure on PR #158: pre-filter the
	// request path BEFORE http.ServeMux gets a chance to apply its
	// built-in URL normalization (cleanPath collapsing `//`, `/./`,
	// `/../`). ServeMux would otherwise issue a 301 redirect for
	// any path that differs from path.Clean(path) — emitting a
	// Location header for malformed POSTs that the handler contract
	// says should return a quiet 404. Redirect-following POSTs may
	// also rewrite to GET on the second hop, silently dropping the
	// JSON-RPC body. Reject path-traversal / collapsed-slash inputs
	// directly with empty-body 404, matching every other gate-2
	// path-shape failure surface.
	muxedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cleaned := path.Clean(r.URL.Path); cleaned != r.URL.Path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Handler:           muxedHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		// codex bot phase4 r2 P2 closure on PR #158: surface non-
		// shutdown serve errors so operators can diagnose a hub
		// listener that died after startup reported `hub-listener-up`.
		// http.ErrServerClosed is the normal shutdown signal (returned
		// by Serve after Shutdown / Close) and not load-bearing here.
		// Anything else (accept loop fatal, unexpected close, etc.)
		// is structured-logged via LogHubMcpEvent so it appears in
		// the same hub-mcp.log stream as bind / lifecycle events.
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			_ = api.LogHubMcpEvent("error", "hub-listener-down", map[string]any{
				"port": port,
				"err":  serveErr.Error(),
			})
		}
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
		// codex bot phase4 r6 P2 closure on PR #158: surface a
		// graceful-shutdown failure (typically context.DeadlineExceeded
		// when an in-flight /clients/.../mcp request did not finish
		// within the budget). We still proceed to tear down the
		// session store and reload handler — keeping them alive
		// indefinitely would leak goroutines and the control-token
		// file. But the operator now sees a structured log line
		// indicating graceful shutdown did not complete cleanly,
		// which can correlate with later "request timed out mid-
		// flight" diagnostics on the client side.
		if err := c.srv.Shutdown(shutCtx); err != nil {
			_ = api.LogHubMcpEvent("warn", "hub-shutdown-incomplete", map[string]any{
				"port": c.port,
				"err":  err.Error(),
			})
			// codex deep-sec phase4 r24 P2 closure on PR #158 (lane #3):
			// Shutdown returned an error (typically context deadline
			// exceeded) — graceful drain failed. The pre-r24 code
			// continued to close store/reload, leaving active hub
			// request goroutines alive (a long tools/call inside
			// the 60s daemon POST keeps the request handler alive
			// past the 5s graceful budget). Call srv.Close() to
			// force-close active connections so request goroutines
			// can unwind before the process exits.
			_ = c.srv.Close()
		}
	}
	if c.store != nil {
		c.store.Close()
	}
	if c.reload != nil {
		// codex bot phase4 r10 P1 closure on PR #158: pass shutCtx
		// (the 5s shutdown budget) so the reload-token flock
		// acquisition cannot block past that budget. A sibling
		// process holding hub-mcp.lock surfaces as
		// context.DeadlineExceeded here; the in-memory control token
		// is already cleared, so leaving the on-disk file behind is
		// harmless — the next hub start regenerates it under flock.
		if err := c.reload.Shutdown(shutCtx); err != nil {
			_ = api.LogHubMcpEvent("warn", "hub-control-token-cleanup-incomplete", map[string]any{
				"port": c.port,
				"err":  err.Error(),
			})
		}
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
		// codex deep-sec P2 closure on PR #158: read with ManifestGet
		// (embedded-first) so manifests baked into the binary AND
		// disk overrides are validated against the same strict gate.
		// ManifestGetWithHash was disk-only — an embedded-only
		// manifest hit os.ErrNotExist and got silently skipped,
		// bypassing the bind pre-gate for the shipped manifest set.
		yaml, gerr := a.ManifestGet(entry.Name)
		if gerr != nil {
			// codex bot phase4 r1 P1 closure on PR #158: only the
			// scan-window race (manifest deleted between scan and read)
			// is benign. Permission errors, schema errors, or any
			// other read failure MUST fail the bind so the strict
			// pre-gate isn't silently bypassed.
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

package mcproute

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

const (
	hubMcpReadHeaderTimeout = 10 * time.Second
	hubMcpReadTimeout       = 15 * time.Second
	hubMcpIdleTimeout       = 120 * time.Second
)

// HubListener owns one existing loopback aggregate transport, its session
// store, reload handler and watchers for the lifetime selected by its caller.
type HubListener struct {
	srv            *http.Server
	store          *api.HubSessionStore
	reload         *api.InternalReloadHandler
	endpoint       api.HubEndpoint
	listenerCancel context.CancelFunc
	alive          atomic.Bool
}

// HubListenerOptions carries the existing restart and health behavior of
// callers that already own aggregate activation.
type HubListenerOptions struct {
	PreservePortOnReloadHandlerFailure bool
	ReloadHandler                      func(context.Context) (*api.InternalReloadHandler, error)
	HealthProbeInterval                time.Duration
	OnUnresponsive                     func()
	OnRecovered                        func()
	OnServeFailure                     func(error)
}

// StartHubListener binds the existing aggregate endpoint and starts serving.
// The caller owns activation and must call Shutdown to settle the listener.
func StartHubListener(ctx context.Context, a *api.API, opts HubListenerOptions) (*HubListener, error) {
	if opts.ReloadHandler == nil {
		opts.ReloadHandler = api.NewInternalReloadHandler
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

	// Groups/namespaces Phase 3 (decision §"Phase 3 diagnostic finding",
	// Defect A): publish the ResolverSnapshot from the participating
	// manifests NOW that the gate-ON listener has bound. Before this
	// wiring NOTHING in production published a snapshot, so the live hub
	// LoadResolverSnapshot()'d nil at every session initialize → empty
	// IntendedParticipants → AggregateInitialize fanned out to nothing →
	// the aggregate exposed no tools (dormant). This is the choke point
	// the design names: build from the SAME manifest source the bind
	// pre-gate (validateParticipatingManifestsForHubBind) just validated.
	//
	// Gate-ON only by construction: this code is past the `enabled` short-
	// circuit, so a gate-OFF process never reaches it (no publish at
	// gate-OFF — the listener never runs, so a published snapshot would be
	// read by no one anyway).
	//
	// A publish failure is NON-fatal to the bind: the listener is already
	// up and a published-nil snapshot degrades to the same dormant
	// behavior that existed before this wiring (empty participants), not a
	// crash. Surface it as a structured warn so the dormant-aggregate
	// condition is observable rather than silent.
	if perr := PublishHubResolverSnapshot(ctx, a); perr != nil {
		_ = api.LogHubMcpEvent("warn", "resolver-snapshot-publish-failed", map[string]any{
			"port": port,
			"err":  perr.Error(),
		})
	}

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
	reload, reloadErr := opts.ReloadHandler(ctx)
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
		//
		// Auto-restart opts out of this rollback. Installed client
		// configs already hold /clients/ and /g/ URLs with the
		// persisted port; clearing it would make the retry choose a
		// fresh OS port and orphan those gated URLs. The restart
		// driver emits hub-listener-restart-failed and retries against
		// the same persisted port instead.
		//
		// Initial startup does not carry a pre-start dependency snapshot into
		// this later failure boundary. ResetHubPortContext re-probes while its
		// hub-mcp.lock is held and returns ErrHubPortResetBlocked when a client,
		// group, or unreadable source now depends on the persisted port.
		if !opts.PreservePortOnReloadHandlerFailure {
			if rerr := api.ResetHubPortContext(ctx); rerr != nil {
				if !errors.Is(rerr, api.ErrHubPortResetBlocked) {
					_ = api.LogHubMcpEvent("error", "hub-endpoint-rollback-failed", map[string]any{
						"err": rerr.Error(),
					})
				}
			}
		}
		return nil, fmt.Errorf("hub-mcp: %w", reloadErr)
	}

	mux := http.NewServeMux()
	// /clients/ catches everything under /clients/<id>/mcp; the
	// handler's path parser validates the trailing /mcp + the client
	// id (gate 2). Sourced from api.HubClientPrefix — the single owner
	// of this literal shared with the parser + URL-builder (deep-review
	// r2 P4-4: this mount previously re-hardcoded "/clients/").
	mux.Handle(api.HubClientPrefix, handler)
	// Groups/namespaces Phase 4b: the SAME handler serves /g/<group>/mcp
	// (the design's structurally-separate-prefix decision). The handler's
	// parseHubPathFromURL recognizes BOTH prefixes and maps a group to the
	// kind-namespaced "g:<group>" scope key; gate 2 rejects an unknown
	// group with the same empty-body 404 the unknown-client path uses. No
	// /clients/ behavior changes — the client branch is byte-identical.
	// Sourced from api.HubGroupPrefix (deep-review r2 P4-4).
	mux.Handle(api.HubGroupPrefix, handler)
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
	mux.HandleFunc(strings.TrimSuffix(api.HubClientPrefix, "/"), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	// Mirror the bare-prefix 404 guard for /g (same anti-301 +
	// empty-body-404 contract as /clients above).
	mux.HandleFunc(strings.TrimSuffix(api.HubGroupPrefix, "/"), func(w http.ResponseWriter, r *http.Request) {
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

	srv := newHubMcpHTTPServer(muxedHandler)

	listenerCtx, listenerCancel := context.WithCancel(ctx)
	listener := &HubListener{
		srv: srv, store: store, reload: reload, endpoint: ep, listenerCancel: listenerCancel,
	}
	listener.alive.Store(true)

	// Share the same supervisor status observations as existing API consumers.
	go api.NewDaemonRestartWatcher(a.DaemonStatusSnapshot, store.MarkPortStale, 0).Run(listenerCtx)
	healthWatcher := api.NewHubListenerHealthWatcher(port, opts.HealthProbeInterval)
	healthWatcher.SetOnUnresponsive(opts.OnUnresponsive)
	healthWatcher.SetOnRecovered(opts.OnRecovered)
	go healthWatcher.Run(listenerCtx)
	go listener.serve(ln, opts.OnServeFailure)
	_ = api.LogHubMcpEvent("info", "hub-listener-up", map[string]any{"port": port})
	return listener, nil
}

// Endpoint returns the endpoint accepted by the serialized bind transaction.
func (c *HubListener) Endpoint() api.HubEndpoint { return c.endpoint }

// Alive reports whether Serve is active.
func (c *HubListener) Alive() bool { return c.alive.Load() }

func (c *HubListener) serve(ln net.Listener, onFailure func(error)) {
	defer c.alive.Store(false)
	if err := c.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		c.alive.Store(false)
		if onFailure != nil {
			onFailure(err)
		}
		_ = api.LogHubMcpEvent("error", "hub-listener-down", map[string]any{
			"port": c.endpoint.Port,
			"err":  err.Error(),
		})
	}
}

func newHubMcpHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: hubMcpReadHeaderTimeout,
		ReadTimeout:       hubMcpReadTimeout,
		IdleTimeout:       hubMcpIdleTimeout,
	}
}

// Shutdown cancels the watchers and drains the HTTP server, then closes the
// session store and removes the reload control token.
func (c *HubListener) Shutdown(parentCtx context.Context) {
	if c == nil {
		return
	}
	if c.listenerCancel != nil {
		c.listenerCancel()
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
				"port": c.endpoint.Port,
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
				"port": c.endpoint.Port,
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

// PublishHubResolverSnapshot builds + publishes the hub's
// ResolverSnapshot from the participating manifests. This is the
// production publish choke point the groups/namespaces design names
// (decision §"Phase 3 diagnostic finding", Defect A): pre-this-wiring
// the snapshot publish path (PublishResolverSnapshot /
// BumpResolverOnManifestChange) had ZERO production callers, so the
// gate-ON hub aggregate read a nil snapshot and exposed no tools.
//
// Manifest source: the SAME set validateParticipatingManifestsForHubBind
// validates — a.Scan() entries with ManifestExists, loaded via
// a.ManifestGet (embed-first). Building from the validated set keeps the
// published topology consistent with what passed the bind pre-gate.
//
// Each manifest's client_bindings rows become Bindings[client] entries
// (keyed by the same client id parseClientPathFromURL yields), via the
// existing BumpResolverOnManifestChange atomic-swap publish. A manifest
// whose client_bindings is empty contributes nothing — additive by
// omission, identical to today when the file carries no bindings.
//
// A per-manifest parse failure is a hard error here (not a silent skip):
// the bind pre-gate already validated every manifest, so a parse failure
// at this point is a genuine inconsistency the operator should see, not a
// routine missing-server case. The caller treats the returned error as
// non-fatal to the bind (logs a warn) so a single bad manifest degrades
// to the dormant-aggregate behavior, never a failed gui startup.
func PublishHubResolverSnapshot(ctx context.Context, a *api.API) error {
	if a == nil {
		return nil
	}
	// R4-3 (bot R4): the manifest scan is now run UNDER hub-mcp.lock, inside
	// PublishGroupsSnapshotLocked, via this closure — so scan+publish is one
	// atomic critical section and concurrent manifest mutations serialize on
	// the flock, each publishing the LATEST on-disk manifest set (no stale
	// scan can clobber a newer publish). The closure reads client config, the
	// workspace registry, process snapshots, and embed-first manifests — NONE
	// of which acquire hub-mcp.lock — so running it under the held lock is
	// deadlock-free (verified: a.Scan / a.ManifestGet / config.ParseManifest
	// have no path to acquireHubMcpLock).
	scanManifests := func() ([]config.ServerManifest, error) {
		scan, err := a.Scan()
		if err != nil {
			return nil, fmt.Errorf("scan manifests: %w", err)
		}
		manifests := make([]config.ServerManifest, 0, len(scan.Entries))
		for _, entry := range scan.Entries {
			if !entry.ManifestExists {
				continue
			}
			// Embed-first read, matching validateParticipatingManifestsForHubBind.
			yamlStr, gerr := a.ManifestGet(entry.Name)
			if gerr != nil {
				// Scan-window race (manifest deleted between scan and read) is
				// benign — skip it. Any other read error is surfaced.
				if errors.Is(gerr, os.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("manifest %q: read: %w", entry.Name, gerr)
			}
			m, perr := config.ParseManifest(strings.NewReader(yamlStr))
			if perr != nil {
				return nil, fmt.Errorf("manifest %q: parse: %w", entry.Name, perr)
			}
			manifests = append(manifests, *m)
		}
		return manifests, nil
	}
	// Groups/namespaces Phase 4a (DATA layer): fold groups.yaml into the
	// SAME published snapshot under kind-namespaced "g:<group>" keys, and
	// ensure a per-group hub-token row (loopback-only — the §D auth seam).
	//
	// P3-2 (opus-arch) + R4-3/R4-4 (bot R4): the scan → groups read →
	// token-ensure → publish span is serialized under ONE held hub-mcp.lock
	// inside PublishGroupsSnapshotLocked so a concurrent GUI groups OR manifest
	// mutation can never make this read an intermediate config and publish a
	// torn / out-of-order topology. The flock is acquired ctx-bounded (R4-4) so
	// a stuck sibling holder cannot freeze GUI startup/shutdown past its budget.
	// No deadlock: this runs AFTER any RMW / bind lock was released, the scan
	// closure does not acquire hub-mcp.lock, and the helper only calls in-flock
	// ("…Locked") halves that do not re-acquire.
	//
	// A bad / unreadable groups.yaml must NEVER fault the client snapshot
	// publish (additive-by-omission, decision claim 5): the helper degrades a
	// load error to "no groups" with a structured warn, so the bare-<client>
	// bindings still publish exactly as before. A token-ensure failure is
	// surfaced (deferred — the snapshot publishes first) so the GUI mutation
	// tail reports restart_required rather than a false full success.
	return api.PublishGroupsSnapshotLocked(ctx, scanManifests)
}

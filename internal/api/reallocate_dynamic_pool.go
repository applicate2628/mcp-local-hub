package api

import (
	"fmt"
	"strconv"

	"mcp-local-hub/internal/config"
)

// ReallocateDynamicPoolPort performs the atomic, crash-consistent port move for
// ONE dynamic-pool proxy descriptor (serena / workspace-LSP) whose loopback pool
// port was stolen by a foreign process (the ephemeral-collision self-heal, L1
// §E). It is invoked OFF the supervisor event loop by the reallocation worker.
//
// Under the held registry (workspaces.yaml) flock across the WHOLE transaction:
//  1. resolve the pool PER DESCRIPTOR (serena effective pool vs the LSP manifest
//     port_pool) — NOT a global constant;
//  2. AllocatePort — which OS-probes each candidate, so the returned port is one
//     that is ACTUALLY bindable right now, exactly what a stolen-port heal needs;
//  3. write the registry row (new port) FIRST via reg.Save() (atomic temp+rename);
//  4. write the supervisor-intent descriptor as ONE atomic temp+rename updating
//     the Port FIELD + the `--port` ARGV + (serena) the RuntimeSpec External/
//     UpstreamPort TOGETHER, so the field↔argv fail-closed guard
//     (effectivePort) and the serena self-validate never observe a partial write.
//
// Returns the new port, or a wrapped ErrPortPoolExhausted when the pool is full
// (the caller quarantines with a distinct reason — design §D).
//
// Crash-consistency (design §E): a crash BEFORE step 3 changes nothing (the
// daemon retries its old port and self-heals again); a crash BETWEEN steps 3 and
// 4 leaves the registry reserving the new port while the descriptor (supervisor-
// intent) still names the old one. On relaunch the daemon is spawned from intent
// (argv --port = old), reads the registry row (= new) and observes a port
// mismatch. For a SERENA proxy the self-validate checks argv vs INTENT (both old,
// self-consistent) so it comes up and self-heals again. For an LSP workspace-proxy
// the startup check compares argv vs the REGISTRY row (old vs new); FIX-3 makes
// THAT mismatch self-healing — classifyLSPPortMismatch (daemon_workspace.go) sees
// intent (the spawn authority) agree with argv while only the registry disagrees
// and exits exitBindRefused (exit 3) instead of a plain exit 1, so the supervisor's
// ephemeral-collision self-heal re-drives here, AllocatePort SKIPS the already-
// reserved new port, and both stores are rewritten consistently (never a
// cross-daemon double-alloc, never a permanent LSP brick).
func ReallocateDynamicPoolPort(registryPath, intentPath string, d SupervisorDaemon) (newPort int, err error) {
	isSerena := IsSerenaProxyDescriptor(d)
	isLSP := IsWorkspaceLSPProxyDescriptor(d)
	if !isSerena && !isLSP {
		return 0, fmt.Errorf("reallocate: descriptor %q is not a dynamic-pool proxy (serena / workspace-LSP); a fixed-port daemon is never moved", d.TaskName)
	}

	reg := NewRegistry(registryPath)
	unlock, err := reg.Lock()
	if err != nil {
		return 0, fmt.Errorf("reallocate: lock registry: %w", err)
	}
	defer ReleaseAndJoin(&err, unlock)
	if err := reg.Load(); err != nil {
		return 0, fmt.Errorf("reallocate: load registry: %w", err)
	}

	// Find the registry row for this descriptor by its unique task name.
	// Canonicalize both sides (P3): a legacy / hand-written workspaces.yaml row may
	// carry the BARE task name (no leading backslash) while the descriptor the loop
	// resolved is canonical — an exact `==` would miss it, fail the heal, and burn a
	// reallocation-cap slot. canonicalIntentTaskKey is the single api-package owner
	// of this leading-backslash normalization (the cli canonicalizer is unreachable
	// from here).
	wantKey := canonicalIntentTaskKey(d.TaskName)
	rowIdx := -1
	for i := range reg.Workspaces {
		if canonicalIntentTaskKey(reg.Workspaces[i].TaskName) == wantKey {
			rowIdx = i
			break
		}
	}
	if rowIdx < 0 {
		return 0, fmt.Errorf("reallocate: no workspaces.yaml row for task %q; cannot move its port without a registry row", d.TaskName)
	}

	pool, err := reallocPoolForDescriptor(d, isSerena)
	if err != nil {
		return 0, err
	}

	// AllocatePort holds no lock itself (we hold the registry flock), OS-probes
	// each candidate, and skips both registry-taken and OS-excluded ports.
	newPort, err = AllocatePort(reg, pool)
	if err != nil {
		return 0, err // wraps ErrPortPoolExhausted with the allocator's rich diagnostic
	}

	// Capture the pre-move registry port so a step-4 failure can COMPENSATE by
	// reverting step 3 under the STILL-HELD registry flock (P1-2).
	oldPort := reg.Workspaces[rowIdx].Port

	// Step 3: registry row → new port FIRST (atomic temp+rename; Save is the
	// lock-held variant — we already hold reg.Lock()).
	reg.Workspaces[rowIdx].Port = newPort
	if err := reg.Save(); err != nil {
		return 0, fmt.Errorf("reallocate: save registry with new port %d: %w", newPort, err)
	}

	// Step 4: supervisor-intent descriptor — Port field + --port argv +
	// (serena) RuntimeSpec ports, all in ONE atomic temp+rename under the intent
	// flock (nested inside the held registry flock). reallocMutateIntentFn is the
	// production MutateSupervisorIntentIfChanged; it is seamed only so a test can
	// force this step to fail and exercise the compensation below.
	if err := reallocMutateIntentFn(intentPath, func(f *SupervisorIntentFile) (bool, error) {
		// Mutate the descriptor BY INDEX in f.Daemons, NOT through
		// FindSupervisorDaemonByTaskName — that helper returns a struct COPY, so a
		// `copy.Port = x` would be silently lost while a `copy.Args[i] = y` (shared
		// backing array) and `copy.RuntimeSpec.X` (shared pointer) WOULD persist,
		// producing exactly the field↔argv/spec disagreement the fail-closed guard
		// + serena self-validate reject. Mutating the slice element in place keeps
		// Port + argv + RuntimeSpec provably consistent.
		idx := -1
		for i := range f.Daemons {
			if canonicalIntentTaskKey(f.Daemons[i].TaskName) == wantKey {
				idx = i
				break
			}
		}
		if idx < 0 {
			return false, fmt.Errorf("reallocate: descriptor %q not present in supervisor-intent", d.TaskName)
		}
		// applyReallocatedPortToDescriptor is the SINGLE owner of the "move a
		// dynamic-pool descriptor to newPort" mutation (Port field + --port argv +
		// serena RuntimeSpec external/upstream), shared with the supervisor loop's
		// FIX-3b targeted cache patch so the two can never diverge on what a port
		// move touches.
		if !applyReallocatedPortToDescriptor(&f.Daemons[idx], newPort) {
			return false, fmt.Errorf("reallocate: descriptor %q carries no --port argv to rewrite", d.TaskName)
		}
		return true, nil
	}); err != nil {
		// P1-2 compensation: step 4 (intent) failed AFTER step 3 committed the
		// registry row to newPort. Left uncompensated, registry=newPort while the
		// descriptor argv/Port still name oldPort. SERENA survives this divergence (it
		// self-validates argv-vs-intent, still self-consistent). For an LSP proxy the
		// startup check compares the registry row (newPort) against --port (oldPort);
		// with FIX-3 classifyLSPPortMismatch now classifies THIS shape — argv==intent==
		// oldPort, only the registry row disagrees — as exitBindRefused (exit 3), so the
		// ephemeral-collision self-heal RE-DRIVES on the next episode rather than the
		// exit-1-forever brick the pre-FIX-3 code hit. Compensation is still correct:
		// revert the registry row to oldPort under the STILL-HELD registry flock so BOTH
		// stores stay consistent on oldPort — the daemon then relaunches and binds
		// oldPort DIRECTLY (no spurious self-heal round), matching design §E's
		// crash-between-3-and-4 recovery, which also leaves the daemon on a port it can
		// bind.
		reg.Workspaces[rowIdx].Port = oldPort
		if revertErr := reg.Save(); revertErr != nil {
			// The compensating revert ALSO failed: the stores remain divergent
			// (registry=newPort, intent=oldPort). Surface BOTH errors — never mask the
			// revert failure — so the Failed-outcome retry and the operator see the
			// unhealable state. The manual recovery is `mcphub register` (which rewrites
			// the registry row to reconcile the two stores), NOT `mcphub daemon recover`
			// (which only force-respawns and does NOT reconcile the divergent stores).
			return 0, fmt.Errorf("reallocate: intent write for new port %d failed (%v); compensating registry revert to old port %d ALSO failed (%v): workspaces.yaml now diverges from supervisor-intent for task %q", newPort, err, oldPort, revertErr, d.TaskName)
		}
		return 0, fmt.Errorf("reallocate: intent write for new port %d failed; reverted registry row to old port %d to keep both stores consistent: %w", newPort, oldPort, err)
	}

	return newPort, nil
}

// reallocMutateIntentFn is the step-4 supervisor-intent mutate, seamed so a test
// can inject a write failure and exercise the registry-revert compensation
// (P1-2). Production is MutateSupervisorIntentIfChanged verbatim.
var reallocMutateIntentFn = MutateSupervisorIntentIfChanged

// reallocPoolForDescriptor resolves the effective port pool a dynamic-pool proxy
// allocates from, mirroring the registration-time resolution exactly:
//   - serena → api.EffectiveSerenaPortPool(embed-first manifest); it tolerates a
//     nil/absent manifest via the built-in dynamic-pool default, so a manifest
//     read miss never blocks the heal.
//   - workspace-LSP → the manifest's top-level port_pool (the same *m.PortPool
//     the LSP register / auto-register path allocates from).
func reallocPoolForDescriptor(d SupervisorDaemon, isSerena bool) (config.PortPool, error) {
	server := d.Server
	if server == "" {
		server = DescriptorServerName(d)
	}
	m, mErr := loadManifestForServer("", server)
	if isSerena {
		pool, err := EffectiveSerenaPortPool(m) // nil-tolerant → built-in default
		if err != nil {
			return config.PortPool{}, fmt.Errorf("reallocate: resolve serena pool: %w", err)
		}
		return pool, nil
	}
	if mErr != nil || m == nil {
		return config.PortPool{}, fmt.Errorf("reallocate: load manifest for LSP server %q: %w", server, mErr)
	}
	if m.PortPool == nil {
		return config.PortPool{}, fmt.Errorf("reallocate: LSP server %q manifest declares no port_pool", server)
	}
	return *m.PortPool, nil
}

// WorkspaceLSPManifestPool returns the mcp-language-server manifest's top-level
// port_pool — the pool workspace-LSP proxies allocate from — for callers that
// need the effective LSP pool WITHOUT a descriptor (the setup ephemeral-range
// overlap check). ok=false when the manifest or its port_pool is unavailable.
// Single-owner: resolves through the same embed-first manifest loader the
// register / auto-register / reallocation paths use.
func WorkspaceLSPManifestPool() (config.PortPool, bool) {
	m, err := loadManifestForServer("", "mcp-language-server")
	if err != nil || m == nil || m.PortPool == nil {
		return config.PortPool{}, false
	}
	return *m.PortPool, true
}

// rewriteDescriptorPortArg rewrites the value token following the FIRST `--port`
// flag in a proxy descriptor's args, returning whether it found one. It scopes
// the scan to i>=2 (past "daemon <kind>") to mirror descriptorArgPort, so a stray
// leading token can never be mistaken for the port flag.
func rewriteDescriptorPortArg(args []string, newPort int) bool {
	for i := 2; i+1 < len(args); i++ {
		if args[i] == "--port" {
			args[i+1] = strconv.Itoa(newPort)
			return true
		}
	}
	return false
}

// applyReallocatedPortToDescriptor moves a dynamic-pool descriptor to newPort IN
// PLACE: the Port field, the `--port` argv token, and (for a serena proxy) the
// RuntimeSpec External/Upstream ports — keeping the build-time invariant
// UpstreamPort == ExternalPort + NativeHTTPInternalPortOffset so a later
// reconcile/drift check does not flag the row. It is the SINGLE owner of "what a
// port move touches on a descriptor", shared by the ReallocateDynamicPoolPort
// step-4 intent write and the supervisor loop's FIX-3b targeted cache patch, so the
// field↔argv fail-closed guard (effectivePort) and the serena self-validate can
// never observe a partial move. Returns false when the descriptor carries no
// `--port` argv to rewrite (the field↔argv consistency cannot be met); the caller
// then leaves the descriptor untouched. Mutates *d.Args in place — the caller owns
// (and, for a shared cache snapshot, must have copied) the backing array.
func applyReallocatedPortToDescriptor(d *SupervisorDaemon, newPort int) bool {
	if d == nil || newPort <= 0 {
		return false
	}
	if !rewriteDescriptorPortArg(d.Args, newPort) {
		return false
	}
	d.Port = newPort
	if d.RuntimeSpec != nil {
		d.RuntimeSpec.ExternalPort = newPort
		d.RuntimeSpec.UpstreamPort = newPort + config.NativeHTTPInternalPortOffset
	}
	return true
}

// CloneIntentWithReallocatedPort returns a copy of src with ONLY the descriptor for
// taskName moved to newPort (Port + --port argv + serena RuntimeSpec, via the shared
// applyReallocatedPortToDescriptor owner). It is the supervisor loop's FIX-3b
// targeted cache patch: when the reallocation worker could not carry a fresh
// whole-intent snapshot (disk read miss, or unorderable/parse-failing timestamps),
// the loop patches JUST this descriptor's port in the CURRENT cache intent so the
// respawn still resolves argv=newPort — instead of respawning the stale (old-port)
// cache descriptor and bouncing on exit-1/exit-3 for up to the ~60s IntentWatcher
// window.
//
// The clone is deep ENOUGH to be safe for the atomic cache swap: the Daemons slice
// is copied into a fresh backing array, and the TARGET descriptor's Args slice (and
// RuntimeSpec pointer) are deep-copied before the in-place rewrite, so the mutation
// never touches the currently-published snapshot that off-loop cache readers hold.
// Every other descriptor is shallow-shared read-only. Returns (nil,false) when src
// is nil, newPort<=0, the descriptor is absent, or it carries no --port argv (the
// caller then keeps the current cache and relies on the backstop + IntentWatcher).
func CloneIntentWithReallocatedPort(src *SupervisorIntentFile, taskName string, newPort int) (*SupervisorIntentFile, bool) {
	if src == nil || newPort <= 0 {
		return nil, false
	}
	wantKey := canonicalIntentTaskKey(taskName)
	idx := -1
	for i := range src.Daemons {
		if canonicalIntentTaskKey(src.Daemons[i].TaskName) == wantKey {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, false
	}
	cp := *src
	cp.Daemons = make([]SupervisorDaemon, len(src.Daemons))
	copy(cp.Daemons, src.Daemons)
	target := &cp.Daemons[idx]
	argsCopy := make([]string, len(target.Args))
	copy(argsCopy, target.Args)
	target.Args = argsCopy
	if target.RuntimeSpec != nil {
		rsCopy := *target.RuntimeSpec
		target.RuntimeSpec = &rsCopy
	}
	if !applyReallocatedPortToDescriptor(target, newPort) {
		return nil, false
	}
	return &cp, true
}

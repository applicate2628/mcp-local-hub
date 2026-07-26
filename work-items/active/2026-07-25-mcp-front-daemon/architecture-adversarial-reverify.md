# MCP Front Daemon Adversarial Architecture Re-verification

Review strategy: **ADVERSARIAL**. The implementation was assumed wrong until the
current source proved otherwise.

Review target:

- branch delta `origin/master..HEAD`;
- functional commit `3f72365d`;
- style-only follow-up `d6c0501f`;
- current RouteHandler, read-only state access, registry publication, startup
  built-in seeding, descriptor canonicalization, strict port resolution, and the
  tests changed by those commits.

Execution constraints honored:

- static review only;
- no build, test, process, listener, or live-state execution;
- no CodeGraph;
- no prior finding reports, research report, claim review, QA report, or QA logs
  read;
- no source or test file changed.

## Gate

**REVISE**

Three design-level blockers remain. The route process can write the shared state
tree from a read path, both read-only resolver implementations publish through
an unsynchronized mutable registry object, and startup seeding can hand the
controller a different generation from the one committed under the intent-file
lock.

| ID | Severity | Defect class | Fix class |
|---|---|---|---|
| A1 | Critical | Read-only state access writes shared diagnostics | `design-decision` |
| A2 | High | Resolver refresh lacks one immutable-generation owner | `design-decision` |
| A3 | High | Startup seeder does not return the committed intent generation | `design-decision` |

## Reviewed Surface

The following source and test surfaces were directly read.

| Area | Files and contracts |
|---|---|
| Repository status and entry points | `README.md`; `git diff origin/master..HEAD`; commit deltas `3f72365d` and `d6c0501f` |
| Route construction | `internal/cli/route.go`; `internal/cli/mcp_front_port.go`; `internal/api/mcp_front_port.go`; `internal/gui/route_adapter.go`; `internal/gui/route_readonly_audit.go` |
| Serena route | `internal/gui/serena_router.go`; `internal/gui/serena_idle_sweeper.go`; `internal/api/serena_routing/resolver.go` |
| LSP route | `internal/gui/lsp_router.go`; `internal/api/lsp_routing/resolver.go`; `internal/api/lsp_trusted_roots.go` |
| Registry read/write boundary | `internal/api/workspace_registry.go`; `internal/api/state_file_helper.go`; `internal/api/state_read_inode_anchor.go`; `internal/api/hub_mcp_state_read_inode_posix.go`; `internal/api/hub_mcp_state_read_inode_windows.go` |
|  | `internal/api/secure_write_client_config.go`; `internal/api/secure_write_posix.go`; `internal/api/secure_write_windows.go`; `internal/api/hub_mcp_log.go` |
| Registry writer call sites | `internal/api/membership.go`; `internal/api/lsp_auto_register.go`; `internal/api/serena_auto_register.go`; `internal/api/register.go`; `internal/api/register_supervisor.go` |
|  | `internal/api/reallocate_dynamic_pool.go`; `internal/api/prune_workspace.go`; `internal/daemon/lazy_proxy.go`; `internal/cli/daemon_workspace.go`; `internal/cli/migrate_serena.go`; `internal/cli/workspace_cmd.go` |
| Startup seeding and intent lifecycle | `internal/api/builtin_route_daemon.go`; `internal/api/daemon_intent.go`; `internal/api/supervisor_intent.go`; `internal/api/supervisor_intent_mutate.go`; `internal/cli/supervise.go`; `internal/cli/supervise_watcher.go` |
| Process diagnostic transport | `internal/process/start_with_job_windows.go`; corresponding process-start call sites reached from supervisor spawn wiring |
| Changed and adjacent tests | `internal/api/builtin_route_daemon_test.go`; `internal/api/lsp_trusted_roots_test.go`; `internal/api/lsp_routing/resolver_test.go`; `internal/api/serena_routing/resolver_test.go` |
|  | `internal/cli/builtin_route_daemon_test.go`; `internal/cli/mcp_front_port_test.go`; `internal/cli/route_i6_readonly_test.go`; `internal/cli/supervise_test.go` |
|  | `internal/gui/route_readonly_test.go`; `internal/gui/lsp_router_test.go`; `internal/gui/serena_router_test.go` |

## Required Inventory 1: Request-Reachable Reads and Diagnostics

The route server constructs independent Serena and Language Server Protocol
(LSP) registry handles and read-only resolvers
(`internal/cli/route.go:175-215`). The following callbacks remain reachable from
an HTTP request.

| Route | Request-reachable callback or read | State/diagnostic behavior |
|---|---|---|
| Serena | `WorkspaceResolver.ResolveByPath`, `RegisteredWorkspace`, and `ListWorkspaces` through `SerenaRouterDeps.Resolver` (`internal/gui/serena_router.go:35-78`) | Calls resolver `snapshot`, which may call `Registry.Load` on the request path (`internal/api/serena_routing/resolver.go:101-195`) |
|  | `SessionRouter` access | Process-local session state |
|  | `HTTPClient` forwarding | Network I/O only |
|  | `AuditFn` | Read-only wiring installs `routeReadOnlySink`; it writes process stderr (`internal/gui/route_readonly_audit.go:35-60`) |
|  | `AutoRegisterFn` and `WakeIdleFn` | Both are nil in read-only wiring (`internal/gui/serena_router.go:249-298`) |
|  | Activity persistence | Disabled by read-only router configuration before the persistence callback (`internal/gui/serena_idle_sweeper.go:103-137`) |
| LSP | `WorkspaceResolver.ResolveByPath`, `RegisteredWorkspace`, and registry snapshot queries through `LSPRouterDeps.Resolver` (`internal/gui/lsp_router.go:51-92`) | Calls resolver `snapshotRegistry`, which may call `Registry.Load` on the request path (`internal/api/lsp_routing/resolver.go:307-374`) |
|  | `TrustedRootCheckFn` | Read-only wiring still installs `api.LSPWorkspaceRootTrusted`, which calls `LoadLSPTrustedRoots` on an unregistered first touch (`internal/gui/lsp_router.go:129-181`; `internal/api/lsp_trusted_roots.go:176-203`) |
|  | `SessionRouter` access | Process-local session state |
|  | `HTTPClient` forwarding | Network I/O only |
|  | `AuditFn` | Read-only wiring installs `routeReadOnlySink`; detached notification diagnostics use the supplied audit callback (`internal/gui/lsp_router.go:811-917`) |
|  | `AutoRegisterFn` | Nil in read-only wiring (`internal/gui/lsp_router.go:129-181`) |

Construction-time reads are also relevant to the same contract:

- `buildRouteServer` immediately calls `reg.Load`
  (`internal/cli/route.go:175-183`);
- LSP manifest loading is a read-only settings operation
  (`internal/cli/route.go:194-200`);
- the LSP resolver's separate registry handle first loads lazily on a request
  (`internal/cli/route.go:201-215`).

The router-level audit callback inventory is therefore complete, but it is not
the complete diagnostic inventory. `Registry.Load` and
`LoadLSPTrustedRoots` enter a lower state-file reader that has its own hardcoded
shared-log diagnostic path. That missing lower callback boundary is A1.

## Required Inventory 2: Registry Publication Paths

`Registry.Save` serializes a complete YAML snapshot and sends it to the one
hardened writer (`internal/api/workspace_registry.go:158-188`).
`WriteStateFileBytesLockHeld` delegates to the secure atomic writer
(`internal/api/state_file_helper.go:107-116`). The platform implementations
publish by same-directory rename
(`internal/api/secure_write_posix.go:155-174`;
`internal/api/secure_write_windows.go:285-301`).

The production `Registry.Save` call-site search found these writer families.

| Owning operation | Publication call sites |
|---|---|
| Membership and lifecycle changes | `internal/api/membership.go:39-83`; `internal/api/workspace_registry.go:459-496` |
| LSP auto-register | `internal/api/lsp_auto_register.go:204-223`, `:306` |
| Serena auto-register | `internal/api/serena_auto_register.go:263-290` |
| Register/unregister orchestration | `internal/api/register.go:812-855`, `:1161`; `internal/api/register_supervisor.go:306-331` |
| Dynamic-port reallocation and pruning | `internal/api/reallocate_dynamic_pool.go:95-150`; `internal/api/prune_workspace.go:145-162` |
| Lazy proxy and daemon workspace commands | `internal/daemon/lazy_proxy.go:1707-1712`; `internal/cli/daemon_workspace.go:214-223` |
| Migration and workspace CLI | `internal/cli/migrate_serena.go:629,1580,1677,1727`; `internal/cli/workspace_cmd.go:289,479` |

Static search was performed from two directions:

1. every production call shaped as a registry `Save`;
2. direct low-level `WriteFile`, hardened state-write, rename, and open-for-write
   references involving `workspaces.yaml`.

No independent production writer to the live registry leaf was found in the
second search. This is a **static inference**, not a runtime proof. The
lock-free reader design therefore depends on all publishers continuing to use
the atomic rename owner above. Even with that publisher discipline, the
in-process reader cache is unsafe because it mutates a shared `Registry`
receiver outside the resolver mutex; see A2.

## Required Inventory 3: Startup Seeder Returns and Events

`ensureBuiltinRouteDaemonAtStartup` has the following complete return/event
surface.

| Path | Return | Event |
|---|---|---|
| Test-only seeding disabled (`internal/cli/supervise.go:2569-2572`) | Original pointer | None |
| Canonical executable path absent (`:2575-2586`) | Original pointer | `builtin-route-ensure-failed`, warn |
| Strict port read/parse/range failure (`:2604-2625`) | Current pointer | `builtin-route-ensure-failed`, warn, phase `resolve-port` |
| Locked mutation, collision, or persistence failure (`:2648-2671`) | Current pointer | `builtin-route-ensure-failed`, warn |
| Persisted change, then in-memory reapply failure (`:2681-2702`) | Earlier caller snapshot | `builtin-route-ensure-failed`, warn, phase `in-memory-reapply` |
| Persisted change and successful in-memory reapply (`:2674-2714`) | Earlier snapshot plus locally re-applied built-in row | `builtin-route-ensured`, info, action `added` or `replaced` |
| No persisted descriptor change (`:2674-2714`) | Earlier snapshot | `builtin-route-ensured`, info, action `unchanged` |

Additional observations:

- the strict accessor correctly propagates a settings failure rather than
  silently selecting the compiled default
  (`internal/api/mcp_front_port.go:39-52`;
  `internal/cli/mcp_front_port.go:36-46`);
- all `events.Emit` results in this seeder are intentionally ignored;
- `hadRow` tests exact task-name spelling, while the descriptor owner uses the
  canonical task key, so a non-canonical leading-slash variant can be reported
  as `added` although it was replaced (`internal/cli/supervise.go:2628-2634`;
  `internal/api/builtin_route_daemon.go:131-172`). This is an observable
  diagnostic misclassification, but it is not one of the three highest-risk
  blockers.

## Required Inventory 4: Descriptor Equality and Normalization

The descriptor owner is `EnsureBuiltinRouteDaemon`.

| Step | Current behavior |
|---|---|
| Build canonical descriptor | Sets task name, server, daemon, executable, arguments, port, and restart policy (`internal/api/builtin_route_daemon.go:64-73`) |
| Normalize identity | `canonicalIntentTaskKey` adds the required leading separator (`internal/api/daemon_intent.go:286-299`) |
| Reject reserved-name collision | A canonical task-name match with a foreign server or daemon identity returns an error (`internal/api/builtin_route_daemon.go:131-140`) |
| Collapse duplicates | All same-identity rows are collected and only one canonical row remains (`internal/api/builtin_route_daemon.go:143-172`) |
| Compare descriptor | Full struct equality via `reflect.DeepEqual`; any field drift replaces the row (`internal/api/builtin_route_daemon.go:154-158`) |

The changed descriptor tests cover foreign collision, identity drift,
full-struct replacement, and duplicate collapse
(`internal/api/builtin_route_daemon_test.go:101-250`). Within the documented
lexical task-key contract, canonicalization is coherent. No independent
descriptor-normalization blocker was found. The startup consumer nevertheless
fails to adopt the exact snapshot on which that canonicalization was committed;
see A3.

## A1 — Read-Only State Access Writes Shared Diagnostics

**Severity:** Critical  
**Architecture laws:** C1 single owner, D2 diagnostic port, D4 global/shared
resource ownership  
**Fix class:** `design-decision`

### Exact mechanism

The route-specific router audit sites were redirected to process stderr
(`internal/gui/route_readonly_audit.go:35-60`). The generic state reader was not.

`Registry.Load` calls `readStateFileInodeAnchored`
(`internal/api/workspace_registry.go:126-136`). The LSP trust check calls the
same reader through `LoadLSPTrustedRoots`
(`internal/api/lsp_trusted_roots.go:176-203`). In the default-relax security
lane, that reader reports broadened parent or file permissions by directly
calling `LogHubMcpEvent`:

- POSIX parent fallback:
  `internal/api/hub_mcp_state_read_inode_posix.go:55-107`;
- POSIX file fallback:
  `internal/api/hub_mcp_state_read_inode_posix.go:135-155`;
- Windows parent fallback:
  `internal/api/hub_mcp_state_read_inode_windows.go:120-143`;
- Windows file fallback:
  `internal/api/hub_mcp_state_read_inode_windows.go:189-216`.

`LogHubMcpEvent` is not a read-only diagnostic. It appends to the GUI-owned
`hub-mcp.log`, creates/locks `hub-mcp.log.lock`, may rotate the log, and syncs
the append (`internal/api/hub_mcp_log.go:48-56,74-116,233-309`).

This path is reachable:

- once at route construction through `reg.Load`
  (`internal/cli/route.go:175-183`);
- repeatedly through Serena resolver refresh
  (`internal/api/serena_routing/resolver.go:101-195`);
- through LSP resolver refresh
  (`internal/api/lsp_routing/resolver.go:307-374`);
- through an unregistered LSP first-touch trust check
  (`internal/gui/lsp_router.go:129-181`).

The changed tests confirm the mechanism rather than falsify it. Both whole-tree
read-only tests deliberately use hardened parents. The GUI test explicitly
states that an unhardened parent makes the state reader call
`LogHubMcpEvent` on every `Load`
(`internal/gui/route_readonly_test.go:144-154`). The production-construction
test similarly uses `HardenedTempDir`
(`internal/cli/route_i6_readonly_test.go:154-158`). The LSP test invokes the
router's `AuditFn` directly; it never drives a trust-root or registry state read
(`internal/gui/lsp_router_test.go:1483-1523`).

### Consequence

The documented read-only process can create, append, lock, rotate, and sync
shared GUI state. The violation is deterministic whenever permission
hardening falls into the supported default-relax lane. It also gives two
processes write ownership of one log family.

### ADVISORY HOW

**Owning seam:** the inode-anchored state-read API, not individual routers.

**Candidate direction:** make state-read diagnostics an injected port or an
explicit read option. Route-daemon callers must supply a process-local sink;
GUI and normal API callers may retain `LogHubMcpEvent`. Apply the port to both
registry and trusted-root reads.

**Alternative:** make route state reads silent in the relax lane. That keeps
the no-write invariant but loses the operator-visible permission warning.

**Tradeoff:** adding a diagnostic port changes the shared read API, but it
preserves observability and restores one owner for shared-log publication.

**Minimal falsifier, not run:** create an owner-correct registry and trusted-root
file beneath intentionally broadened parents, snapshot the complete state tree,
construct the real `buildRouteServer`, then issue:

1. a registered Serena request that forces resolver refresh; and
2. an unregistered LSP first-touch request that forces the trust check.

Assert zero create/append/lock/rename below the state directory, specifically
including `hub-mcp.log`, `.log.1`, and `.lock`, while the injected process-local
diagnostic sink receives the permission warning.

## A2 — Resolver Refresh Lacks One Immutable-Generation Owner

**Severity:** High  
**Architecture laws:** C1 single owner, D4 shared-state lifecycle, D5
per-datum ownership  
**Fix class:** `design-decision`

### Exact mechanism

Both read-only resolvers call a mutating `Registry.Load` outside their resolver
mutex:

- Serena: `snapshot` calls `refresh`, and the read-only branch calls
  `r.reg.Load` before taking the cache publication lock
  (`internal/api/serena_routing/resolver.go:101-195`);
- LSP: `snapshotRegistry` has the same structure
  (`internal/api/lsp_routing/resolver.go:307-374`).

`Registry.Load` assigns receiver fields in separate statements
(`internal/api/workspace_registry.go:128-155`). Concurrent HTTP requests can
therefore enter two refreshes on the same resolver, mutate the same
`*Registry`, read entries from it, and install caches out of order. The mutex
protects `cached`, `loaded`, and `lastMtime`; it does not protect `r.reg`.

The invalidation token is also only `mtime`
(`internal/api/serena_routing/resolver.go:129-151`;
`internal/api/lsp_routing/resolver.go:325-347`). A complete atomic publication
with the same observed modification time is invisible once the cache is loaded.
The Serena test makes this behavior explicit: after a real registry update it
restores the previous modification time and expects one hundred requests to
continue serving the stale snapshot
(`internal/api/serena_routing/resolver_test.go:460-510`).

The new non-blocking tests use one resolver call while another process holds the
registry lock
(`internal/api/lsp_routing/resolver_test.go:305-359`;
`internal/api/serena_routing/resolver_test.go:582-631`). They do not exercise
two concurrent refreshes on one resolver or a complete same-mtime publication.

### Consequence

The lock-free cross-process read goal was obtained by dropping the file lock,
but no in-process immutable snapshot owner replaced it. Under concurrent
requests, cache publication is data-racy and can regress to an older
generation. Independently, a valid atomically-renamed generation can remain
unobserved indefinitely when its timestamp token aliases the cached one.

### ADVISORY HOW

**Owning seam:** one pure registry-snapshot loader shared by the Serena and LSP
resolver adapters.

**Candidate direction:** parse each reload into a fresh local registry or an
immutable `RegistrySnapshot`; never mutate the resolver's shared `*Registry`.
Serialize refresh/singleflight at the snapshot owner and publish the complete
generation under one lock. Use a publication token that distinguishes complete
generations, such as file identity plus size and timestamp, or a content
generation/hash when exact observation is required.

**Alternative:** hold each resolver mutex across `Load` and publication. This
removes the in-process race but duplicates the loading logic and still leaves
same-mtime generations invisible.

**Tradeoff:** hashing adds read cost; file identity/size/timestamp is cheaper
but can still alias. A writer-owned generation in the serialized document is
strongest but changes the persisted contract.

**Minimal falsifier, not run:** inject a pausable snapshot-loader seam. Arrange
two overlapping reloads so request A parses generation A and pauses, request B
parses and publishes generation B, then request A resumes. Assert:

- no mutable registry receiver is shared;
- the final cache cannot regress from B to A;
- every returned cache is one complete generation;
- a subsequent atomic same-mtime publication is eventually observed.

A race-enabled execution is a useful broader guard, but it is not a substitute
for the deterministic generation-order test.

## A3 — Startup Seeder Does Not Return the Committed Intent Generation

**Severity:** High  
**Architecture laws:** C1 state owner, D1 typed failure, D4 persisted-state
lifecycle  
**Fix class:** `design-decision`

### Exact mechanism

Startup loads `intent` once, then calls the seeder
(`internal/cli/supervise.go:904-944`). The seeder correctly performs the disk
mutation against a fresh, flock-held snapshot:

- `MutateSupervisorIntentIfChanged` reads, clones, mutates, and writes the
  current disk file under one lock
  (`internal/api/supervisor_intent_mutate.go:12-49`);
- the seeder invokes it at `internal/cli/supervise.go:2636-2655`.

The helper returns only an error. It does not return the committed snapshot.
After success, the seeder applies only `EnsureBuiltinRouteDaemon` to the older
caller snapshot (`internal/cli/supervise.go:2681-2703`).

If another legitimate writer commits a row or stop after the initial load but
before the seeder acquires the lock, the disk transaction preserves that
concurrent change, while the returned in-memory pointer omits it. Startup then:

- seeds `ctrl.intentCache` from the stale pointer
  (`internal/cli/supervise.go:1145-1184`);
- starts an intent watcher whose first action is to baseline current disk
  metadata without firing a callback
  (`internal/cli/supervise_watcher.go:133-174`);
- runs initial reconcile from the stale pointer
  (`internal/cli/supervise.go:1393-1443`);
- publishes `reconcile-ready`
  (`internal/cli/supervise.go:1446-1458`).

Because the watcher can baseline the already-merged disk file, it need not ever
deliver the generation missing from the controller cache.

The failure return contract also has a deterministic sibling violation. The
function promises that a resolution failure returns the original possibly-nil
intent unchanged (`internal/cli/supervise.go:2561-2568`), but it allocates an
empty intent before strict port resolution
(`internal/cli/supervise.go:2588-2605`). A nil input therefore becomes non-nil
on strict-port or later persistence failure (`:2625`, `:2671`), enabling the
otherwise-gated initial reconcile branch (`:1393`) despite the documented
contract.

The strict-port and persistence tests use pre-existing non-nil intent values
(`internal/cli/builtin_route_daemon_test.go:79-200`), so they do not falsify the
nil-return behavior. No changed test injects a second writer between startup
load and the seeder's locked read.

### Consequence

Disk state and controller state can diverge at the exact point intended to make
the disk snapshot authoritative. A newly added daemon, stop, or descriptor
update can be absent from initial reconcile while the process reports itself
ready. On a fresh host, a strict resolution failure also changes nil control
state into fabricated empty intent state.

### ADVISORY HOW

**Owning seam:** the locked supervisor-intent mutation API.

**Candidate direction:** return the exact committed fresh snapshot, plus a
typed result describing `changed` and the built-in action. Replace the startup
local pointer wholesale with that returned generation before controller-cache
seed, watcher baseline, and reconcile. Preserve the original pointer on every
pre-commit failure, including nil.

**Alternative:** re-read the intent file after successful persistence and
before starting the watcher. That is simpler, but creates a second race window
between the write and re-read and loses the exact transaction generation.

**Tradeoff:** returning the committed snapshot broadens the mutation helper
contract, but removes the need for post-commit reconstruction and supplies one
authoritative generation to all startup consumers.

**Minimal falsifier, not run:** add an injection seam after startup's initial
read and before the seeder mutation. Have a second writer add a daemon and a
stop, then let seeding commit. Assert the seeder return, controller cache, and
initial reconcile all contain:

- the concurrent daemon;
- the concurrent stop;
- exactly one canonical built-in route row.

Also call the seeder with nil intent under forced strict-port failure and forced
persistence failure. Assert the returned pointer remains nil, no intent file is
created or changed, exactly one failure event is emitted, and no ensured event
is emitted.

## Anti-Layering Audit

| Defect class | Verdict | Observation |
|---|---|---|
| Read-only diagnostics | **PILED** | Router audit sites use `routeReadOnlySink`, while the lower state reader independently hardcodes `LogHubMcpEvent`. The same no-shared-write invariant has two diagnostic owners with contradictory effects. |
| Resolver publication | **PILED** | Serena and LSP duplicate the same stat/load/cache algorithm and both expose the same mutable-receiver race. A shared immutable snapshot owner is absent. |
| Startup committed generation | **CLEAN-SINGLE-OWNER** | The defect is localized at the locked mutation/seeder handoff. Fix it there; do not add compensating reloads independently to controller setup, watcher startup, and reconcile. |
| Failure idiom | **REVISE** | Strict port failure is typed and propagated to the seeder, but the seeder changes nil control state before that failure and returns an untyped reconstructed snapshot after success. |

Because two defect classes are **PILED**, this batch cannot pass architecture
review even if local tests remain green.

## Blast Radius and Residual Uncertainty

- A1 affects construction, Serena refresh, LSP refresh, and LSP trusted-root
  first touch on both POSIX and Windows.
- A2 affects every concurrent Serena and LSP request after a registry
  publication.
- A3 affects supervisor startup, initial reconcile, cached desired state,
  intent-watcher baseline, stop enforcement, and readiness reporting.
- The registry-writer inventory and callback graph are static source
  inferences. Dynamic dispatch was not executed under the review constraints.
- No claim is made that supervisor stderr capture is correct on every platform.
  The route sink's write-to-stderr behavior is source-proven; delivery to the
  operator is outside this gate because no process was run.
- No build, unit test, race test, integration test, or live filesystem
  falsifier was executed. These findings are source-level blockers, not failed
  runtime-test reports.

## Required Routing

All three blockers are `design-decision` findings and should route to
`$architect` before implementation:

1. establish the diagnostic-port contract for all state reads used by the
   read-only route;
2. establish one immutable registry-generation publication owner shared by the
   Serena and LSP adapters;
3. establish the locked intent-mutation result contract that returns the exact
   committed generation.

After those decisions are implemented, re-run architecture review and execute
the named deterministic falsifiers. The present gate remains **REVISE**.

## Terms and Abbreviations

- **LSP:** Language Server Protocol.
- **MCP:** Model Context Protocol.
- **POSIX:** Portable Operating System Interface family used here for Unix-like
  platform behavior.
- **Registry generation:** one complete, internally consistent publication of
  `workspaces.yaml`.
- **PILED:** the same invariant is implemented or repaired at multiple
  contradictory or duplicated ownership points.
- **REVISE:** architecture gate failure requiring correction and re-review.

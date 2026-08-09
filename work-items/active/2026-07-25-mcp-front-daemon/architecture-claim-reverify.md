# Architecture claim re-verification — MCP front daemon

Reviewed target: `origin/master..d6c0501f5866644849423d12583ad4e8f4b0c696`,
including `3f72365d534251051190e84e84d7ab6bc0f01cea`.

Review strategy: CLAIM-VERIFY plus the mandatory multi-fix anti-layering
audit.

## Outcome

**Gate: REVISE.**

Four of the seven original findings are `CLOSED`, two are `PARTIALLY
CLOSED`, and one is `NOT CLOSED`.

The two blocking defect classes are:

1. a request served by `RouteHandler()` still reaches a hardcoded
   `LogHubMcpEvent` call in the shared inode-anchored state reader and writes
   `hub-mcp.log` plus `hub-mcp.log.lock` on a broadened parent; and
2. strict-port and persistence failures on a genuinely fresh nil input return
   a newly allocated non-nil empty intent instead of the original nil intent.

The first defect also makes the route-diagnostic fix class `PILED`: selected
router emit sites use an injected sink, while a lower-layer emit site in the
same request class still bypasses it.

## Review basis and reviewed surfaces

This review treated commit messages as untrusted claims. It used current
source, the current `origin/master..HEAD` diff, the accepted review inputs, and
preserved raw QA output. It did not run tests, builds, daemons, or fleet
processes.

Repository status and review contract:

- `README.md`; `CLAUDE.md`; `CONTRIBUTING.md`;
- `work-items/active/2026-07-25-mcp-front-daemon/status.md`;
- `work-items/active/2026-07-25-mcp-front-daemon/brief.md`;
- `work-items/active/2026-07-25-mcp-front-daemon/plan.md`;
- `work-items/active/2026-07-25-mcp-front-daemon/research-reverify.md`;
- `work-items/active/2026-07-25-mcp-front-daemon/qa-reverify.md`;
- `work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-front-daemon.md`;
- `work-items/bugs/2026-07-26-route-daemon-state-read-unhardened-parent-fallback-writes-hub-mcp-log.md`.

Request and read-only construction:

- `internal/gui/route_adapter.go`;
- `internal/cli/route.go`;
- `internal/gui/serena_router.go`;
- `internal/gui/lsp_router.go`;
- `internal/gui/route_readonly_audit.go`;
- `internal/gui/serena_idle_sweeper.go`;
- `internal/api/serena_routing/resolver.go`;
- `internal/api/lsp_routing/resolver.go`;
- `internal/api/lsp_trusted_roots.go`;
- `internal/api/hub_mcp_state_read_inode_windows.go`;
- `internal/api/hub_mcp_state_read_inode_posix.go`;
- `internal/api/hub_mcp_log.go`;
- `internal/api/state_paths_prod.go`;
- `internal/api/state_paths_windows.go`;
- `internal/api/state_paths_unix.go`.

Registry publication and writer inventory:

- `internal/api/workspace_registry.go`;
- `internal/api/state_file_helper.go`;
- `internal/api/secure_write_client_config.go`;
- `internal/api/secure_write_posix.go`;
- `internal/api/secure_write_windows.go`;
- `internal/api/membership.go`;
- `internal/api/prune_workspace.go`;
- `internal/api/register.go`;
- `internal/api/register_supervisor.go`;
- `internal/api/serena_auto_register.go`;
- `internal/api/lsp_auto_register.go`;
- `internal/api/reallocate_dynamic_pool.go`;
- `internal/daemon/lazy_proxy.go`;
- `internal/cli/workspace_cmd.go`;
- `internal/cli/daemon_workspace.go`;
- `internal/cli/migrate_serena.go`.

Seeder, descriptor, port, and downstream caller:

- `internal/cli/supervise.go`;
- `internal/cli/supervise_reconcile.go`;
- `internal/api/supervisor_intent_mutate.go`;
- `internal/api/builtin_route_daemon.go`;
- `internal/api/supervisor_intent.go`;
- `internal/cli/mcp_front_port.go`;
- `internal/api/mcp_front_port.go`.

Permanent guards read:

- `internal/gui/route_readonly_test.go`;
- `internal/gui/lsp_router_test.go`;
- `internal/api/serena_routing/resolver_test.go`;
- `internal/api/lsp_routing/resolver_test.go`;
- `internal/api/lsp_trusted_roots_test.go`;
- `internal/api/builtin_route_daemon_test.go`;
- `internal/api/mcp_front_port_test.go`;
- `internal/cli/builtin_route_daemon_test.go`;
- `internal/cli/route_i6_readonly_test.go`.

Preserved runtime evidence read:

- `.scratch/qa-accept-inc1/reverify/final-01-gui-exact.log`;
- `.scratch/qa-accept-inc1/reverify/final-02-api-exact.log`;
- `.scratch/qa-accept-inc1/reverify/final-03-serena-routing-exact.log`;
- `.scratch/qa-accept-inc1/reverify/final-04-lsp-routing-exact.log`;
- `.scratch/qa-accept-inc1/reverify/final-05-cli-exact.log`;
- `.scratch/qa-accept-inc1/reverify/final-06-go-build-all.log`;
- `.scratch/qa-accept-inc1/reverify/final-07-go-vet-all.log`;
- `.scratch/qa-accept-inc1/reverify/final-08-diff-check.log`;
- `.scratch/qa-accept-inc1/reverify/final-09-detached-clean-status.log`;
- `.scratch/qa-accept-inc1/reverify/main-worktree-reconciliation.log`;
- all `mutation-01` through `mutation-11` logs named by
  `qa-reverify.md:55-68`;
- `.scratch/qa-accept-inc1/reverify/scratch-adjacent-broadened-parent-request.log`;
- `.scratch/qa-accept-inc1/reverify/scratch-baseline-fresh-nil-seeder.log`;
- `.scratch/qa-accept-inc1/reverify/scratch-baseline-fresh-nil-port.log`;
- `.scratch/qa-accept-inc1/reverify/scratch-baseline-descriptor-convergence.log`.

## Seven-finding disposition

The canonical S4 verdict vocabulary is `verified`, `failed`, and
`not-verifiable (with reason)`.

| # | Original finding | S4 verdict | Closure |
|---:|---|---|---|
| 1 | P1-1 — every `RouteHandler()` request path performs zero shared writes | `failed` | **NOT CLOSED** |
|  |  | Direct Serena and Language Server Protocol (LSP) router diagnostics now use `routeReadOnlySink` (`internal/gui/serena_router.go:280-298`, `internal/gui/lsp_router.go:155-182`, `internal/gui/lsp_router.go:854-917`). |  |
|  |  | Registry/trusted-root reads still hardcode `LogHubMcpEvent` on a broadened parent (`internal/api/hub_mcp_state_read_inode_windows.go:116-143`, `internal/api/hub_mcp_state_read_inode_posix.go:66-107`). |  |
|  |  | A real Serena request returned HTTP 502 and created `hub-mcp.log` plus its lock: `.scratch/qa-accept-inc1/reverify/scratch-adjacent-broadened-parent-request.log:1-15`. |  |
| 2 | P1-2 — persist before adopt, preserve original input on every failure, and emit success only after durable persist | `failed` | **PARTIALLY CLOSED** |
|  |  | Persist now precedes in-memory reapply and success emission (`internal/cli/supervise.go:2636-2714`); the existing-row failure guard passed (`.scratch/qa-accept-inc1/reverify/final-05-cli-exact.log:4-15`). |  |
|  |  | Nil input is allocated before strict resolution and persistence (`internal/cli/supervise.go:2588-2605`) and returned on both failures (`internal/cli/supervise.go:2625,2656-2671`). Runtime: `.scratch/qa-accept-inc1/reverify/scratch-baseline-fresh-nil-seeder.log:5-14`. |  |
| 3 | P2-3 — read-only resolver/trusted-root reads create no lock or directory, and unlocked registry reload is safe because every writer atomically publishes a complete file | `failed` | **PARTIALLY CLOSED** |
|  |  | The Serena and LSP read-only resolvers skip `Registry.Lock` (`internal/api/serena_routing/resolver.go:125-188`, `internal/api/lsp_routing/resolver.go:321-368`), and the trusted-root accessor resolves through `DaemonStateDirReadOnly` (`internal/api/lsp_trusted_roots.go:90-121,206-220`). M4-M6 held. |  |
|  |  | Every located production workspace-registry writer converges on atomic `Registry.Save`; the C1 object-axis assessment below is `verified`. |  |
|  |  | The trusted-root and registry loads nevertheless share the hardcoded event-writing reader, and P12 proves the resulting request-time write. Thus the broad no-side-effect guarantee fails even though lock omission, no-directory path resolution, and atomic publication are each verified. |  |
| 4 | P2-4 — reserved-row collision is loud and canonical equality covers the complete descriptor without normalization churn | `verified` | **CLOSED** |
|  |  | Foreign `Server`/`Daemon` identities are rejected before replacement; canonical rows are compared with `reflect.DeepEqual` (`internal/api/builtin_route_daemon.go:122-173`). |  |
|  |  | All `SupervisorDaemon` and nested `DaemonRuntimeSpec` fields participate (`internal/api/supervisor_intent.go:66-110,125-151`). M8/M9 held, and nil/empty serialize/reload convergence passed: `.scratch/qa-accept-inc1/reverify/scratch-baseline-descriptor-convergence.log:1-20`, `.scratch/qa-accept-inc1/reverify/mutation-09-partial-descriptor-compare.log:1-23`. |  |
| 5 | P2-5 — startup seeding uses strict port resolution and fails closed on both an existing row and a fresh host | `verified` | **CLOSED** |
|  |  | `ResolveMCPFrontPort` returns read/parse/range failures (`internal/api/mcp_front_port.go:39-52`); the seeder stops before persistence (`internal/cli/supervise.go:2592-2625`). |  |
|  |  | M10 held for an existing custom-port row and a fresh nil host: `.scratch/qa-accept-inc1/reverify/mutation-10-lenient-port-fallback.log:1-18`. The baseline fresh-host result had no route row/file, one failure event, and no success event: `.scratch/qa-accept-inc1/reverify/scratch-baseline-fresh-nil-port.log:1-13`. |  |
|  |  | The non-nil return-shape defect is assigned to finding 2; it does not make the strict-port decision itself lenient. |  |
| 6 | P2-6 — the corrected controls are non-vacuous under controlled mutation | `verified` | **CLOSED** |
|  |  | M1-M11 all classified `HELD`; none survived or was skipped (`work-items/active/2026-07-25-mcp-front-daemon/qa-reverify.md:50-70,183-199`). P12 independently proved the still-open broadened-parent defect. |  |
|  |  | This closes the evidence-quality finding, not the two product defects. The fixes for findings 1 and 2 still require permanent versions of the scratch falsifiers before their own closure. |  |
| 7 | P3-7 — no trailing whitespace or blank line at end of the current diff | `verified` | **CLOSED** |
|  |  | Current `internal/gui/route_readonly_test.go` ends at line 223. Fresh `git diff --check origin/master..HEAD` returned 0; preserved clean evidence is `.scratch/qa-accept-inc1/reverify/final-08-diff-check.log:1-7`. M11 returned 2 and named line 224 under the committed scratch mutation: `.scratch/qa-accept-inc1/reverify/mutation-11-trailing-blank-line.log:1-11`. |  |

## Claim 1 — request-reachable write-class inventory

`RouteHandler()` mounts only `/serena/mcp` and `/lsp/`
(`internal/gui/route_adapter.go:20-28`). The production route builder supplies
the two read-only resolvers, read-only router constructors, and
`ReadOnlyRouterMode: true` (`internal/cli/route.go:167-217`).

| Request participant | Current owner/control | Disposition |
|---|---|---|
| Serena auto-registration | `SetSerenaRouterReadOnly` leaves `AutoRegisterFn` nil; the handler stops at the nil guard (`internal/gui/serena_router.go:249-298,713-745,1384-1388`). | No registry/intent write |
| Serena idle wake | The same constructor leaves `WakeIdleFn` nil; wake executes only when nonnil (`internal/gui/serena_router.go:249-298,846-883`). | No supervisor-intent write |
| Serena activity persistence | `ReadOnlyRouterMode` returns before debounce or registry I/O (`internal/gui/serena_idle_sweeper.go:103-136`). | No registry write |
| Serena direct diagnostics | `AuditFn: routeReadOnlySink`; the sink writes one redacted record to process stderr (`internal/gui/serena_router.go:280-298`, `internal/gui/route_readonly_audit.go:35-60`). | No shared file write; M1 held |
| LSP auto-registration | `SetLSPRouterReadOnly` leaves `AutoRegisterFn` nil; registered rows are returned directly and unregistered rows stop at the nil guard (`internal/gui/lsp_router.go:129-182,515-594`). | No registry/intent write |
| LSP direct/detached diagnostics | The read-only constructor injects `routeReadOnlySink`, and all detached return paths call the supplied `auditFn` (`internal/gui/lsp_router.go:155-182,854-917`). | No shared file write; M2/M3 held |
| Serena registry refresh | `ResolveByPath` refreshes with unlocked `Registry.Load` (`internal/api/serena_routing/resolver.go:107-188,242-256`). `Registry.Load` enters the inode reader (`internal/api/workspace_registry.go:126-155`). | **Writes `hub-mcp.log`/lock on broadened parent** |
| Registered LSP registry refresh | `ResolveByPath` reaches `snapshotRegistry` and the unlocked refresh (`internal/api/lsp_routing/resolver.go:123-152,307-368`). | **Same lower-layer write** |
| Unregistered LSP first touch | The resolver refreshes, then the trust gate calls `LSPWorkspaceRootTrusted` before the nil auto-register refusal (`internal/gui/lsp_router.go:515-594`, `internal/api/lsp_trusted_roots.go:329-334`). | **Registry and/or trusted-root read can reach the same lower-layer write** |
| Trusted-root path resolution | `DefaultLSPTrustedRootsPathReadOnly` uses `DaemonStateDirReadOnly` and does not create the state directory (`internal/api/lsp_trusted_roots.go:90-121,206-220`). | Direct path-resolution side effect closed |
| Shared inode reader | The Windows and POSIX implementations hardcode `LogHubMcpEvent` before the requested leaf is opened (`internal/api/hub_mcp_state_read_inode_windows.go:116-159`, `internal/api/hub_mcp_state_read_inode_posix.go:66-118`). | **Blocking writer** |
| Hub event persistence | `LogHubMcpEvent` enters append; append creates/resolves the state directory, locks, rotates, appends, and syncs (`internal/api/hub_mcp_log.go:98-116,233-310`). | `hub-mcp.log`, `.lock`, possible `.1`, and directory effects |

The adjacent bug is therefore **inside the architecture review contract**. Its
location outside the implementer's prior narrow surface does not redefine the
non-negotiable read-only guarantee
(`work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-front-daemon.md:69-79`).
It reopens finding 1. The first review missed it because the new no-write
request guards deliberately hardened their parents and therefore excluded the
lower-level relax branch (`internal/gui/route_readonly_test.go:144-154`);
P12 removed that blind spot.

The same call is also reachable before serving requests because
`buildRouteServer` performs an initial `Registry.Load`
(`internal/cli/route.go:175-183`). The runtime-proved request path is already
sufficient to fail the claim; the startup path is additional residual
exposure.

## Claim 3 — C1 object-axis and atomic registry writer assessment

### C1 object-axis

| Axis | Assessment |
|---|---|
| Policy object | Publication of the complete `workspaces.yaml` workspace registry |
| Policy owner who would change it | `Registry.Save` serializes the complete registry and delegates the live-file write (`internal/api/workspace_registry.go:158-189`) |
| Triggering change | Any registry mutation that must become visible across processes |
| Consumers that must co-vary | Membership/prune, register/unregister, supervisor register, Serena/LSP auto-register and rollback, dynamic-pool reallocation, lazy-proxy lifecycle/activity, CLI workspace/daemon/migration operations |
| One place maintainers inspect | `internal/api/workspace_registry.go:158-189` |
| Propagation path | `Registry.Save` → `WriteStateFileBytesLockHeld` (`internal/api/state_file_helper.go:107-115`) → hardened secure writer (`internal/api/state_file_helper.go:167-185`) → `SecureWriteClientConfig` (`internal/api/secure_write_client_config.go:68-89`) |
| Publication primitive | Same-directory handle-relative atomic rename: POSIX `Renameat` (`internal/api/secure_write_posix.go:162-174`); Windows `ntRenameRelative` (`internal/api/secure_write_windows.go:285-301`) |
| Falsifying probe | A production call site that writes/truncates/renames the registry path without `Registry.Save`, or a `Registry.Save` implementation that publishes before the complete payload is durable |
| Verdict | **CLEAN-SINGLE-OWNER; S4 `verified` for the atomic-publication premise** |

### Current production writer inventory

Current-source searches for direct `Save` calls and for registry-path write
primitives found no production writer that bypasses `Registry.Save`.

| Writer family | Current save sites |
|---|---|
| Registry lifecycle/activity methods | `internal/api/workspace_registry.go:423-496` |
| Membership and pruning | `internal/api/membership.go:39-83`; `internal/api/prune_workspace.go:145-163` |
| Register/unregister and rollback | `internal/api/register.go:801-854,1156-1164`; `internal/api/register_supervisor.go:292-332` |
| Serena/LSP auto-registration and rollback | `internal/api/serena_auto_register.go:245-292`; `internal/api/lsp_auto_register.go:204-266,295-306` |
| Dynamic-pool reallocation and compensation | `internal/api/reallocate_dynamic_pool.go:43-165` |
| Lazy-proxy lifecycle/materialization | direct save at `internal/daemon/lazy_proxy.go:1643-1710`; lifecycle/activity wrappers throughout `internal/daemon/lazy_proxy.go:423-423,1351-1405,1576-1647,1832-2089` |
| CLI workspace operations | `internal/cli/workspace_cmd.go:243-289,466-482` |
| CLI workspace daemon startup | `internal/cli/daemon_workspace.go:137-223` |
| Serena migration and compensation | `internal/cli/migrate_serena.go:617-635,1560-1583,1663-1684,1704-1730` |

Writer coordination remains caller-owned through `Registry.Lock`, while
reader tear-safety is publication-owned through atomic rename
(`internal/api/workspace_registry.go:158-204`). The QA phase did not run a
concurrent writer stress test (`work-items/active/2026-07-25-mcp-front-daemon/qa-reverify.md:171-181`);
no such runtime falsifier was part of the admitted scope. The current source
inventory is sufficient to verify the owner topology, but a future direct
writer would invalidate the unlocked-reader premise immediately.

## Claim 2 — all seeder return and event paths

`runSupervise` assigns the returned pointer and schedules initial reconcile
only when the result is nonnil (`internal/cli/supervise.go:933-944,1393-1443`).
That makes nilness a caller-visible control-flow value, not a cosmetic shape.

| Return path | Returned intent | Event and durable behavior | Verdict |
|---|---|---|---|
| Test-disable seam | Original input | No event or persistence (`internal/cli/supervise.go:2569-2572`) | Correct |
| Empty canonical executable | Original input | Failure event, no persistence (`internal/cli/supervise.go:2575-2586`) | Correct |
| Strict-port failure, existing input | Original existing object | Failure event; no persist; no success (`internal/cli/supervise.go:2592-2625`) | Correct; permanent guard passed |
| Strict-port failure, nil input | Newly allocated non-nil empty object | Failure event; no row/file/success | **Defect**; runtime `.scratch/qa-accept-inc1/reverify/scratch-baseline-fresh-nil-seeder.log:5-12` |
| Persist failure, existing input | Original existing object remains unmodified | Failure event after persistence error; no success (`internal/cli/supervise.go:2636-2671`) | Correct; M7 mutation held |
| Persist failure, nil input | Newly allocated non-nil empty object | Failure event; no row/file/success | **Defect**; same raw log |
| Persist success, `changed=false` | Current caller object | Success event, no reapply (`internal/cli/supervise.go:2647-2655,2674-2714`) | `not-verifiable (controlled stale-caller/concurrent-disk guard absent)` |
| Persist success, `changed=true`, reapply fails | Caller object returned without successful reapply | Disk is already durable; failure event; no success (`internal/cli/supervise.go:2674-2702`) | Ordering correct; convergence intentionally fails loud |
| Persist success, `changed=true`, reapply succeeds | Canonical row applied in memory | Success only after durable persist and reapply (`internal/cli/supervise.go:2674-2714`) | Correct; success/reread guard passed |

The current code therefore closes the original persist-before-adopt defect for
existing inputs and prevents premature success. It does not preserve an
original nil input. The `changed=false` stale-caller case also remains
unfalsified: a concurrent writer can make the fresh on-disk mutation report
unchanged while the caller object is stale. That uncertainty is not needed to
fail the claim because the two nil-failure paths are already runtime-proved.

## Claim 4 — collision identity and descriptor convergence

`EnsureBuiltinRouteDaemon` owns both decisions:

1. the canonical task key may be updated only when the existing row also has
   the reserved `(Server, Daemon) == ("route", "front")` identity
   (`internal/api/builtin_route_daemon.go:128-147`); and
2. an owned row is canonical only when its complete `SupervisorDaemon` value
   is `reflect.DeepEqual` to the built descriptor
   (`internal/api/builtin_route_daemon.go:149-173`).

The complete value includes `TaskName`, `Server`, `Daemon`, `Command`, `Args`,
`Env`, `Workspace`, `Port`, `ManifestHash`,
`StartupBindDeadlineSeconds`, and `RuntimeSpec`
(`internal/api/supervisor_intent.go:66-110`). The nested runtime spec includes
all fields at `internal/api/supervisor_intent.go:125-151`; there are no
unexported or time fields in either value.

M8 proved the identity guard is load-bearing
(`.scratch/qa-accept-inc1/reverify/mutation-08-foreign-identity-guard-removed.log:1-14`).
M9 proved the complete comparison catches empty `Env` and allocated empty
`RuntimeSpec`, and the clean table proved nil/empty normalization converges
after serialize/reload. This finding is fully closed.

## Claim 5 — strict-port first-run behavior

| Caller state | Strict resolution failure | Architecture disposition |
|---|---|---|
| Existing reserved row | The row remains byte-for-byte at its configured port; no intent file is newly written; a `builtin-route-ensure-failed` warning is emitted (`internal/cli/builtin_route_daemon_test.go:134-200`). | Correct fail-closed behavior |
| Fresh nil host | No route row or intent file is created; one failure event and no success event are emitted (`.scratch/qa-accept-inc1/reverify/scratch-baseline-fresh-nil-port.log:1-13`). | Correct fail-closed port/durability behavior |

Leaving no row on a fresh strict-resolution failure is **acceptable
fail-closed behavior, not silent degradation**. The caller deliberately keeps
the supervisor alive, and the warning carries the resolution error, intent
path, task name, and `phase:"resolve-port"`
(`internal/cli/supervise.go:2604-2625`). Seeding a compiled default instead
would be worse: M10 proves it would write a row and success event on a port the
operator did not configure.

The separate return-shape defect remains blocking under finding 2 because the
function returns non-nil after claiming to preserve the original possibly-nil
intent (`internal/cli/supervise.go:2561-2568,2588-2625`). It does not turn the
no-row outcome into a silent or lenient port decision.

## Claim 6 — falsification matrix

| ID | Controlled regression | Result | Raw evidence |
|---|---|---|---|
| M1 | Serena read-only sink replaced with shared log | `HELD` | `.scratch/qa-accept-inc1/reverify/mutation-01-serena-shared-audit.log:1-16` |
| M2 | Detached LSP helper ignores supplied audit sink | `HELD` | `.scratch/qa-accept-inc1/reverify/mutation-02-lsp-hardcoded-audit.log:1-14` |
| M3 | LSP read-only constructor omits sink | `HELD` | `.scratch/qa-accept-inc1/reverify/mutation-03-lsp-readonly-wiring-removed.log:1-14` |
| M4 | Serena read-only resolver reacquires exclusive lock | `HELD` | `.scratch/qa-accept-inc1/reverify/mutation-04-serena-readonly-relocks.log:1-14` |
| M5 | LSP read-only resolver reacquires exclusive lock | `HELD` | `.scratch/qa-accept-inc1/reverify/mutation-05-lsp-readonly-relocks.log:1-14` |
| M6 | Trusted-root read uses directory-creating path | `HELD` | `.scratch/qa-accept-inc1/reverify/mutation-06-trusted-roots-creates-state.log:1-14` |
| M7 | Caller intent is adopted before durable persistence | `HELD` | `.scratch/qa-accept-inc1/reverify/mutation-07-seeder-adopts-before-persist.log:1-23` |
| M8 | Reserved identity guard removed | `HELD` | `.scratch/qa-accept-inc1/reverify/mutation-08-foreign-identity-guard-removed.log:1-14` |
| M9 | Full equality weakened to Command/Args/Port | `HELD` | `.scratch/qa-accept-inc1/reverify/mutation-09-partial-descriptor-compare.log:1-23` |
| M10 | Strict startup uses lenient fallback | `HELD` | `.scratch/qa-accept-inc1/reverify/mutation-10-lenient-port-fallback.log:1-18` |
| M11 | Trailing whitespace-only EOF line reintroduced | `HELD` | `.scratch/qa-accept-inc1/reverify/mutation-11-trailing-blank-line.log:1-11` |
| P12 | Broadened-parent real route request | `PRODUCT DEFECT PROVED` | `.scratch/qa-accept-inc1/reverify/scratch-adjacent-broadened-parent-request.log:1-15` |

The restored detached tree then produced 14 exact test passes, zero failures,
zero skips, successful build/vet, a clean range diff check, and clean
`d6c0501f` status
(`work-items/active/2026-07-25-mcp-front-daemon/qa-reverify.md:183-199`).
This is sufficient to close the test-falsifiability finding. It cannot close
the product behavior that P12 and the fresh-nil probes disproved.

## Blocking findings, ordered by severity

### B1 — request-time shared event-log write bypasses the route diagnostic seam

- Severity: **P2 architecture blocker; low-volume operational side effect**.
- Original findings affected: 1 and 3.
- Defect class: cross-layer observability side effect; route read-only policy
  has no single injected owner.
- Layering laws: A6/D2 single injected diagnostic port and C1 single-owner
  policy; multi-fix verdict `PILED`.
- Defect: the route-specific constructors redirect selected router-owned
  diagnostics, but the shared registry/trusted-root read primitive still calls
  `LogHubMcpEvent` directly. A request on a broadened parent therefore mutates
  the GUI-owned log, lock, rotation file, and possibly state directory.
- Evidence:
  `internal/api/hub_mcp_state_read_inode_windows.go:116-143`;
  `internal/api/hub_mcp_state_read_inode_posix.go:66-107`;
  `internal/api/hub_mcp_log.go:98-116,233-310`;
  `.scratch/qa-accept-inc1/reverify/scratch-adjacent-broadened-parent-request.log:1-15`.
- Fix direction: make the state-read diagnostic destination explicit at a
  stable `internal/api` boundary and inject the route process's non-shared
  sink through every registry/trusted-root read reached by route construction
  and requests. Acceptance is behavioral: every route request class causes
  zero registry, intent, trusted-root, event-log, lock, rotation, or directory
  mutations.
- Fix-class: **design-decision**. The fix crosses the route composition and
  shared state-reader owners, affects sibling read callers on two operating
  systems, and changes the diagnostic dependency boundary.
- Route: `$architect` for the diagnostic-port boundary, then the implementer,
  QA, and this architecture re-review.

ADVISORY HOW, non-binding:

- Owner/seam: the shared state-read API in `internal/api`, with route-specific
  selection injected from `buildRouteServer`; do not use a mutable
  package-global override.
- Candidate: add a read-options/diagnostic-port parameter at the registry and
  trusted-root read boundary, defaulting existing GUI/CLI callers to the
  current hub event sink and supplying the route stderr sink explicitly.
- Alternative: expose an explicit no-shared-audit read variant and keep
  observability at the route composition boundary. This is narrower but risks
  proliferating paired APIs if more callers need custom diagnostics.
- Alternative: factor one envelope encoder plus destination interface so
  `routeReadOnlySink` and `LogHubMcpEvent` do not separately own
  timestamp/level/event/redaction assembly
  (`internal/gui/route_readonly_audit.go:35-60`,
  `internal/api/hub_mcp_log.go:98-116`).
- Falsifying guard: permanent broadened-parent before/after state-tree tests
  for Serena registered requests, LSP registered requests, and LSP
  unregistered trusted-root first touch. Each must observe the diagnostic only
  through the injected non-shared destination and zero shared-file delta.

### B2 — fresh nil seeder failures change caller-visible state

- Severity: **P2 durability/control-flow contract blocker**.
- Original finding affected: 2.
- Defect class: persist/adopt caller convergence across failure and concurrent
  disk-state paths.
- Layering law: one correct durability owner; multi-fix verdict
  `CLEAN-SINGLE-OWNER` structurally but semantically incomplete.
- Defect: the function allocates `intent` before strict resolution and
  persistence, then returns that allocation on failure despite its explicit
  promise to return the original possibly-nil input. `runSupervise` treats
  nilness as the initial-reconcile scheduling gate.
- Evidence:
  `internal/cli/supervise.go:933-944,1393-1443,2561-2671`;
  `.scratch/qa-accept-inc1/reverify/scratch-baseline-fresh-nil-seeder.log:1-18`.
- Fix direction: preserve the original caller pointer until durable
  persistence succeeds; after success, establish an in-memory object that
  converges to the persisted canonical row before emitting success. Define
  and test the `changed=false` stale-caller case rather than assuming disk and
  caller equivalence.
- Fix-class: **design-decision**. Although the immediate nil allocation is
  local, the complete correction owns persistence, concurrent-disk
  convergence, caller control flow, and success/failure events; more than one
  viable update/reload strategy exists.
- Route: `$architect` for the convergence contract, then the implementer, QA,
  and this architecture re-review.

ADVISORY HOW, non-binding:

- Owner/seam: `ensureBuiltinRouteDaemonAtStartup`.
- Candidate: retain `originalIntent`; perform strict resolution and disk
  mutation first; on any failure return `originalIntent`; on success allocate
  or clone only then and apply the canonical persisted state before success.
- Alternative: have the persisted mutation return/reload the committed
  snapshot and replace the caller view from that source. This handles
  `changed=false` concurrency more directly but widens the mutation API.
- Alternative: return a typed `{Intent, Outcome, Error}` result and let
  `runSupervise` make the degrade/reconcile decision explicitly. This is
  clearest but changes the local composition contract.
- Falsifying guard: permanent table cases for nil and nonnil inputs across
  strict failure, persist failure, unchanged success, changed success, and
  reapply failure. Assert returned identity/shape, row content, intent-file
  content, failure/success event exclusivity, and whether initial reconcile is
  scheduled.

## Multi-fix anti-layering audit

| Defect class | Verdict | Owner assessment |
|---|---|---|
| Route request shared-write diagnostics | **PILED** | Selected router emit sites use `AuditFn`, but the shared reader still hardcodes `LogHubMcpEvent`; the route-wide no-shared-write policy is not owned at one complete seam. Consolidation is B1. |
| Seeder persist/adopt/event ordering | `CLEAN-SINGLE-OWNER` | All product logic is in `ensureBuiltinRouteDaemonAtStartup`; no neighboring guard duplicates it. The nil and stale-caller paths are incomplete logic inside that owner, not a second patch layer. |
| Registry unlocked reload and atomic publication | `CLEAN-SINGLE-OWNER` | Each resolver owns its cache/lock choice; every located writer converges on `Registry.Save` and one secure publication pipeline. |
| Trusted-root read path resolution | `CLEAN-SINGLE-OWNER` | Read path selection is owned by `DefaultLSPTrustedRootsPathReadOnly`; writers retain the directory-creating path. The later event write is B1, not a duplicate path resolver. |
| Reserved identity and descriptor equality | `CLEAN-SINGLE-OWNER` | `EnsureBuiltinRouteDaemon` owns identity, whole-value equality, duplicate collapse, and the returned change bit in one pass. |
| Strict versus lenient port semantics | `CLEAN-SINGLE-OWNER` | `ResolveMCPFrontPort` owns validation; the lenient accessor wraps it only for read-only/best-effort consumers, while startup injects the strict function. |
| Regression-control matrix and whitespace | `CLEAN-SINGLE-OWNER` | Each mutation targets one production control; no mutation survived. The EOF claim is owned by the range diff check. |

Any `PILED` class blocks `PASS`; B1 therefore independently requires
`REVISE` even before considering the runtime contract failure.

### D1 failure-idiom check

**D1 verdict: CLEAN.**

The API leaves return typed errors:

- `ResolveMCPFrontPort` returns validation/read errors
  (`internal/api/mcp_front_port.go:39-52`);
- `EnsureBuiltinRouteDaemon` returns collision errors
  (`internal/api/builtin_route_daemon.go:122-173`); and
- `MutateSupervisorIntentIfChanged` propagates read/mutate/write errors
  (`internal/api/supervisor_intent_mutate.go:12-49`).

The CLI composition-root helper deliberately maps those errors to one
`builtin-route-ensure-failed` event and a degrade/continue decision; successful
completion uses `builtin-route-ensured`
(`internal/cli/supervise.go:2561-2714`). No leaf terminates the process, and
there are not two failure idioms for the same seeder failure class. B2 is a
wrong returned value, not an idiom split.

## Blast radius and residual risk

- B1 is cross-cutting in implementation ownership but narrow in behavior: the
  front daemon must select a non-shared diagnostic destination without
  changing existing GUI/CLI default audit behavior.
- The Windows request path is runtime-proved. The POSIX sibling contains the
  same hardcoded event call statically
  (`internal/api/hub_mcp_state_read_inode_posix.go:83-107`) but was not run in
  this Windows QA pass.
- Registry writer completeness and atomic publication were source-verified,
  not concurrent-stress-verified. A future direct writer would invalidate the
  unlocked-reader safety argument.
- The seeder `changed=false` stale-caller/concurrent-disk path has no runtime
  falsifier. It must be decided with B2 rather than silently assumed safe.
- Fresh-host strict failure can leave the front daemon absent until settings
  repair and a later startup. That is visible through the failure event and is
  the accepted fail-closed alternative to silently seeding the wrong port.

## Required fixes before PASS

1. Close B1 with one route-aware diagnostic seam that covers every
   registry/trusted-root read reached during route construction and requests.
   Promote P12 into permanent Serena and LSP broadened-parent zero-write
   guards.
2. Close B2 by preserving the original intent on every strict/persist failure
   and defining caller convergence for unchanged/changed success. Add the
   permanent return/event/durability matrix.
3. Re-run M1-M11, the broadened-parent request matrix, the fresh nil seeder
   matrix, the exact clean-HEAD guards, `go build ./...`, `go vet ./...`, and
   `git diff --check origin/master..HEAD`.
4. Return both design-decision findings through a fix-design review before
   implementation; neither may be downgraded to `inline-sufficient`.

## Final gate

**REVISE.**

`PASS` is unavailable because findings 1-3 are not all closed and the
route-diagnostic defect class is `PILED`. There is no external blocker, so
`BLOCKED` is not appropriate.

## Terms and Abbreviations

- **C1** — single-owner invariant.
- **D1** — failure is returned by reusable leaves; only the composition root
  decides terminate, degrade, or recover.
- **LSP** — Language Server Protocol.
- **MCP** — Model Context Protocol.
- **P12** — the scratch-only broadened-parent real route-request probe.
- **S4** — the per-claim evidence verdict vocabulary: `verified`, `failed`, or
  `not-verifiable (with reason)`.
- **CLOSED / PARTIALLY CLOSED / NOT CLOSED** — disposition of an original
  review finding after re-verification.
- **PASS / REVISE / BLOCKED** — final architecture gate outcomes.

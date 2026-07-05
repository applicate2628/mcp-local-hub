# Design — Unify the supervisor's daemon port-resolution owner

- **role:** `$architect`
- **date:** 2026-07-05
- **source of truth:** `work-items/active/2026-07-05-unify-port-resolution-owner/status.md` (accepted problem statement + target direction; user chose architect review 2026-07-05)
- **decision record:** `work-items/decisions/2026-07-05-daemon-port-resolution-single-owner.md` (`status: accepted` — promoted 2026-07-05 on the `$architecture-reviewer`'s verified §4a-regression finding; §4 revised proposed-4a → accepted-4b, see §4 revision banner + the ADR `## Correction`)
- **supersedes-later (not now):** PR #504 serena-guard (`internal/api/intent_port_backfill.go:207` `if server == "serena" { continue }`) — see Non-goals.
- **no implementation code in this package.**

---

## 1. Executive framing (the defect, in one paragraph)

"What port does daemon X use" has **two owners**: the persisted `SupervisorDaemon.Port` field (written by the install fan-out, the serena/LSP builders, and F5) and the **manifest** (read lazily by the status path). For a legacy `Port=0` descriptor the persisted field is a *stale/empty cache* and the manifest is the real answer — but only the *display* path (`supervisorStatusDaemons`) falls back to it. Every **port-protection decision** (liveness sweep, P1b first-bind deadline, P2a squatter reap, `mcphub daemon recover`, the startup running-scan) reads the raw field, so `Port=0` **structurally disables** them (`internal/cli/supervise_liveness.go:591` `if d.Port <= 0 { return true }` early-healthy; `internal/cli/daemon_recover.go:163` `if desc.Port <= 0` skip; `internal/cli/supervise.go:2353` `if port := intentPorts[...]; port > 0` bypass). F5 (`internal/api/intent_port_backfill.go` `BackfillIntentDaemonPorts`) is a **write-convergence** that warms the cache at startup so the decisions activate — a second owner answering the same question, now accreting special-cases (`d.Port>0 || d.RuntimeSpec!=nil` migrated-serena skip; `server=="serena"` legacy-unified skip). The design collapses the two owners into **one lazy resolver** consumed by every decision path.

---

## 2. Reader / writer inventory (verified this session; `file:line` cited)

Field declaration: `internal/api/supervisor_intent.go:60` (`Port`), `:77` (`StartupBindDeadlineSeconds`).

### 2a. `SupervisorDaemon.Port` — WRITERS (author the persisted field)

| Site | Class | Note |
|---|---|---|
| `internal/api/install_parsed_manifest.go:2452` (`Port: d.Port`, in `supervisorDaemonsFromPlan`, 2445-2456) | **install-write** | Global/legacy daemons; stamps manifest port at install. **Authoritative writer.** |
| `internal/api/supervisor_intent_build.go:281` (`Port: ws.Port`) | **serena-build** | serena-proxy dynamic-pool rows (`RuntimeSpec!=nil`); port lives only here. **Authoritative writer.** |
| `internal/api/register_supervisor.go:50` (`Port: entry.Port`) | **install-write (LSP)** | LSP workspace-proxy rows; port from registry. **Authoritative writer.** |
| `internal/api/intent_port_backfill.go:214` (`d.Port = port`) | **F5-write** | The redundant write-convergence. **Deletion candidate.** |
| `internal/cli/supervise_status.go:197` (`Port: port`) | display (synthetic) | Builds a throw-away descriptor with the *resolved* port for the liveness probe — not a persist. |
| `internal/cli/supervise.go:2364-2365` (`Port: port`) | liveness-decision (synthetic) | Startup-scan throw-away descriptor; `port` comes from the raw intent map (see readers). |

### 2b. `SupervisorDaemon.Port` — READERS (classified)

| Site | Class |
|---|---|
| `internal/cli/supervise_liveness.go:591` (`if d.Port <= 0` early-healthy), `:597` (`probe.PortOwnerPID(d.Port)`), `:648` (`probe.PortLive(d.Port)`) | **liveness-decision** (the core gap) |
| `internal/cli/supervise_liveness.go:376` (`livenessProbe.PortOwnerPID(d.Port)` on mismatch) | **squatter** (re-probe owner) |
| `internal/cli/supervise_liveness.go:707` (`out[...] = d.Port`, `supervisorIntentPortMapForStateDir`) → consumed at `internal/cli/supervise.go:2353` (`if port := intentPorts[...]; port > 0`) | **liveness-decision** (startup running-scan bypass gate) |
| `internal/cli/daemon_recover.go:163` (`if desc.Port <= 0` skip+warn), `:174,:176,:185,:201,:216,:241` (owner probe, messages, `waitRecoverPortFree`) | **recover** |
| `internal/cli/supervise_squatter.go:392,:621` (event body `port`) | **squatter** (audit) |
| `internal/cli/supervise_reconcile_ipc.go:814` (`if oldDescriptor.Port != newDescriptor.Port`) | **reconcile** (drift compare — cache-vs-cache; stays raw, see §3) |
| `internal/api/install.go:2087,:2091,:2108` (port-recon `row.Port` compares incl. `+NativeHTTPInternalPortOffset`) | **install-write** (port reconciliation) |
| `internal/api/stop_force_supervisor.go:155,:158,:166,:167,:179,:182` (`if d.Port == 0` → PID-only kill) | force-stop (**adjacent Port=0 gap** — §12) |
| `internal/api/register_supervisor.go:560` (`if port := descriptor.Port; port != 0` kill-by-port before serena removal) | reconcile/removal (runtime_spec serena always `Port>0`, no legacy-0 case) |
| `internal/cli/daemon_serena.go:255,:256` (`flagPort != spec.ExternalPort \|\| flagPort != d.Port`) | **spawn contract** (launcher asserts descriptor↔spec agree) |
| `internal/cli/install_migration_wiring_windows.go:87-88`, `internal/cli/migrate_serena_restart_windows.go:89-90` (gather expected ports, skip 0) | migrate (best-effort) |
| `internal/api/install_parsed_manifest.go:812,:848,:2412`, `internal/cli/supervise.go:3673`, `internal/gui/daemon_env.go:265`, `internal/cli/supervise_status.go:180` | **display** / dry-run / IPC |

Totals from the reference sweep: 325 unique refs to `SupervisorDaemon.Port`; **51 in non-test production `.go`**, 274 in `_test.go`.

### 2c. `SupervisorDaemon.StartupBindDeadlineSeconds` — writers + deadline-decision readers (24 refs)

| Site | Class |
|---|---|
| `internal/api/install_parsed_manifest.go:2455` (`= d.StartupBindDeadlineSeconds`) | **install-write** (from manifest) |
| `internal/api/supervisor_intent_build.go:289` (`= 120`) | **serena-build** |
| `internal/api/register_supervisor.go:58` (`= 120`) | **install-write (LSP)** |
| `internal/api/intent_port_backfill.go:230` (`d.StartupBindDeadlineSeconds = deadlineSecs`), `:238` (audit read) | **F5-write** |
| `internal/cli/supervise_liveness.go:516-517` (`if d.StartupBindDeadlineSeconds > 0 { return … }`) | **deadline-decision** (the owner, `supervisorStartupBindDeadline`) |
| `internal/cli/supervise_liveness.go:520` (`if isSerenaProxyDescriptor(d)` → 120s) | **serena special-case** (argv-keyed; the bug) |
| `internal/cli/supervise_status.go:199` (passes field into synthetic descriptor) | display |

Manifest side: `internal/config/manifest.go:266` declares `startup_bind_deadline_seconds` (validated 0..600 at `:1318`). The two near-duplicate manifest resolvers: `internal/api/migrate.go:376` `ResolveManifestDaemonPort` (port-only) and `internal/api/intent_port_backfill.go:50` `resolveManifestPortAndDeadline` (port+deadline). **These collapse into the owner (§3).**

### 2d. Verified serena deadline gap (the PR #504 root)

`servers/serena/manifest.yaml:60-64` — the `unified` daemon declares `port: 9121` but **no** `startup_bind_deadline_seconds`. So `resolveManifestPortAndDeadline("serena","unified")` returns `(9121, 0, true)`. A legacy-unified serena descriptor's args are `daemon --server serena --daemon unified` (not `daemon serena-proxy`), so `isSerenaProxyDescriptor` (`internal/cli/supervise.go:2949-2951`, checks `Args[1]=="serena-proxy"`) is **false** → `supervisorStartupBindDeadline` returns the **60s default** → cold uvx+SolidLSP start blows the deadline → daemon-bind-timeout restart cycle. F5 stamping port 9121 *turned on* the port check that exposed the wrong deadline; PR #504's `server=="serena"` skip is a symptom mask.

---

## 3. The owner — single lazy port/deadline resolver

### 3.1 Where it lives + why

A new file `internal/api/supervisor_port_owner.go`. It **must** be in `internal/api` because (a) it reads the embedded manifest via `loadManifestForServer` (api-internal) and (b) its consumers span **both** `internal/cli` (liveness, squatter, recover, status) **and** `internal/api` (force-stop, register). It **must be a pure, stateless function over `(SupervisorDaemon, embedded-manifest-FS)`** — NOT an in-memory supervisor cache — precisely because `mcphub daemon recover` runs **outside** the supervisor process and must resolve the identical answer. The embedded manifest is compiled into every `mcphub` binary, so the pure resolver is reachable from any process with zero shared runtime state.

### 3.2 Surface (signatures, no bodies)

```
// internal/api/supervisor_port_owner.go  (owner)

// EffectiveDaemonPort resolves the port a manifest-backed daemon actually
// uses: the descriptor's cached Port when >0, else the manifest-declared port
// for the descriptor's resolved (server,daemon) identity. ok=false ⇒ neither
// cache nor manifest yields port>0 (a portless-by-design timer row, or a
// renamed/removed manifest) — the caller keeps "no port protection", but the
// state is now a VISIBLE resolve-miss, not a structural bypass.
func EffectiveDaemonPort(d SupervisorDaemon) (port int, ok bool)

// EffectiveStartupBindDeadlineSeconds resolves the P1b first-bind deadline:
// explicit descriptor field(>0) > manifest-declared deadline(>0) >
// server-identity default. The server-identity default is
// SerenaStartupBindDeadlineSeconds (120) when the resolved server identity
// (descriptorServerDaemon) == SerenaServerName, else
// DefaultStartupBindDeadlineSeconds (60) — so BOTH serena shapes (legacy
// `unified` and the workspace-hash serena-proxy pool rows) resolve 120s with
// one rule keyed on server, not on daemon-name or argv shape (§4). Independent
// of the port short-circuit (a descriptor may carry Port but a zero deadline —
// exactly the post-F5-port-only state).
func EffectiveStartupBindDeadlineSeconds(d SupervisorDaemon) int

// Single owner of the deadline values (referenced only by the resolver above):
const (
	DefaultStartupBindDeadlineSeconds = 60
	SerenaStartupBindDeadlineSeconds  = 120
)

// NewDaemonPortResolver returns a per-PASS memoized resolver (one manifest
// parse per server per pass) for the hot loops (liveness sweep, status
// refresh). Generalizes the private newManifestPortResolver.
func NewDaemonPortResolver() *DaemonPortResolver
func (r *DaemonPortResolver) Resolve(d SupervisorDaemon) (port, deadlineSecs int, portOK bool)
```

Moved **into** the owner (single home) and deleted from `intent_port_backfill.go`: `descriptorServerDaemon` (identity from fields-or-args) and `resolveManifestPortAndDeadline` (the one manifest read returning port+deadline). `migrate.go:376 ResolveManifestDaemonPort` becomes a thin re-export of the owner (or is deleted once its only production caller — `supervise_status.go:43` — moves to the resolver).

### 3.3 `descriptor.Port` — persisted cache, NOT derived-only (decision)

**`SupervisorDaemon.Port` stays a persisted field, authored ONLY by the authoritative writers (install fan-out, serena/LSP builders). It is NOT made derived-only.** Rationale (each a hard blocker to derived-only):

1. **Spawn contract.** The serena-proxy launcher asserts `flagPort != d.Port` is an error (`internal/cli/daemon_serena.go:255-256`); the child binds on this port. It is an input to spawning, not just a view.
2. **No manifest source for runtime_spec rows.** serena dynamic-pool + LSP workspace-proxy ports live *only* in the descriptor/spec — the manifest is a per-workspace-less template. For these, `d.Port` **is** the source of truth (and always `>0`), so the owner returns it directly and never consults the manifest.
3. **Cache-vs-cache consumers.** Reconcile drift-detection (`supervise_reconcile_ipc.go:814`) compares two *descriptors'* cached fields; both sides must read the raw field or lazy-resolving one side fabricates phantom drift. Install port-reconciliation, migrate expected-ports, and display likewise read the raw value.

**What changes is ownership of the *decision*, not the field:** every port-**protection decision** stops treating `d.Port` as the source of truth and instead calls the owner (`EffectiveDaemonPort`), which returns `d.Port when >0, else manifest`. `d.Port` becomes a *spawn-time cache* of an answer the manifest owns for legacy rows; the manifest is the authoritative fallback. This is the "persisted cache + lazy fallback to the authoritative source" pattern — and it makes F5 (a cache-warmer) optional rather than load-bearing.

### 3.4 How each decision consumer changes (resolve at the boundary, pass the resolved value down)

- **Liveness sweep** (`sweepSupervisorLivenessOnce`): instantiate one `NewDaemonPortResolver()` per sweep (mirrors the existing per-sweep port-owner snapshot); at the per-daemon loop head (after `d, ok := byTask[taskName]`, `supervise_liveness.go:287`) resolve `(port, deadline, ok)` and set the **local** `d.Port` = resolved port (+ use resolved deadline). Every downstream reader in the sweep (`supervisorDaemonEntryLiveWithProbe` at `:591/:597/:648`, the squatter re-probe at `:376`, the event bodies) then sees the resolved port with **no** further change. The `if d.Port <= 0 { return true }` guard (`:592`) is retained but is now reached only for a **genuine** resolve-miss (`!ok`, e.g. a maintenance timer) — the intended semantics.
- **P1b deadline** (`supervisorStartupBindDeadline`, `:516-524`): delegate to `api.EffectiveStartupBindDeadlineSeconds(d)` and convert to `time.Duration`. **Delete the `isSerenaProxyDescriptor` arm (`:520-522`)** — see §4.
- **Startup running-scan** (`loadSupervisorCurrentRunning`, `supervise.go:2353`): `supervisorIntentPortMapForStateDir` (`:701-711`) currently maps raw `d.Port`; change it to map the resolved effective port so the `if port > 0` gate no longer bypasses legacy rows.
- **Squatter classifier** (`classifyPortSquatter` → `commandLineMatchesTaskArgv` → `isGlobalDaemonDescriptor`, `supervise_squatter.go:217-222`): resolve `(server,daemon)` via the owner's `descriptorServerDaemon(d)` instead of reading `d.Server`/`d.Daemon` raw, so a **blank-identity** legacy row classifies correctly (this replaces F5's identity-heal — §4). The port it probes is already resolved by the sweep.
- **Recover CLI** (`recoverReapPortSquatter`, `daemon_recover.go:160-174`): resolve the effective port at entry via `api.EffectiveDaemonPort(desc)`; proceed with the resolved port. Keep a warn **only** when the resolve *also* misses (`!ok`) — replacing the current unconditional `desc.Port <= 0` skip.
- **Status** (`supervisorStatusDaemons`): replace the private `newManifestPortResolver` + the inline `if port == 0` block (`supervise_status.go:141,181-186`) with the owner's `NewDaemonPortResolver`; feed the resolved deadline (not the raw field) into the synthetic descriptor at `:199`. Delete the "NOT made redundant by F5" comment (`:134-140`).

### 3.5 Cache / memoization strategy

The manifest lookup parses the whole server YAML per call (`loadManifestForServer → ParseManifest`). The owner exposes the **per-pass memoized** `DaemonPortResolver` (one parse per server per pass) — a direct generalization of today's `newManifestPortResolver` (`supervise_status.go:59-84`), which already proves the pattern and its once-per-server test seam. Hot loops (5s sweep, status refresh) instantiate one per pass; one-shot callers (recover, force-stop) call the pure `EffectiveDaemonPort` directly. The embedded manifest is immutable at runtime (no read/write race); the intent file's `d.Port` cache is still re-read per sweep exactly as today (`livenessSweepIntent`). Net added cost: one manifest parse per server per 5s sweep — negligible, and consistent with the correctness-over-micro-opt precedent already documented at `supervise_liveness.go:187-205`.

---

## 4. Deadline unification (item 3)

> **REVISION 2026-07-05 (architect, responding to an accepted `$architecture-reviewer` finding).** The prior §4 chose **4a** (declare `startup_bind_deadline_seconds: 120` on the serena manifest's `unified` daemon + delete the argv-keyed arm). That is a **verified regression** on the serena-**proxy** dynamic-pool population and is REPLACED here by the previously-rejected **4b** (resolver default keyed on **server identity**). The correction and its evidence are recorded in the ADR (`work-items/decisions/2026-07-05-daemon-port-resolution-single-owner.md`).

**Goal:** every serena descriptor whose resolved identity is server `serena` gets a **120s** first-bind deadline (unless it carries an explicit `StartupBindDeadlineSeconds>0`) **without** an argv-shape special-case — keyed on **server identity**, which covers **both** on-disk serena shapes with **one** rule.

### 4.1 Why 4a (manifest edit) is wrong — the serena-proxy pool-row population

A manifest edit keyed on the `unified` **daemon name** covers legacy-UNIFIED serena (its `Daemon` field is literally `unified`, so `findDaemon("serena","unified")` hits) but **misses the serena-proxy dynamic-pool rows**:

- A serena-proxy pool row's `Daemon` field is `daemonKey = ws.WorkspaceKey` — a per-workspace **hash**, NOT `"unified"` (`internal/api/supervisor_intent_build.go:247-250`, stamped `:276`). Its `Server` field IS `serena` (`:275`, `Server: m.Name`).
- `servers/serena/manifest.yaml` declares only the `unified` daemon (`:60-64`), so `resolveManifestPortAndDeadline("serena","<workspace-hash>")` **misses** → returns `(_, 0, false)` → the resolver falls to its default.
- Under 4a the default is a plain `60`, and the `isSerenaProxyDescriptor` arm that currently rescues these rows to 120s (`supervise_liveness.go:520`) is **deleted**. So a serena-proxy pool row with explicit deadline `0` gets **60s**.
- The field contract already documents `0 = ... 120s for serena-proxy descriptors` as a supported on-disk state (`supervisor_intent.go:73`). **Real reachable population:** serena-proxy fan-out shipped #234 (2026-05-27); the explicit `StartupBindDeadlineSeconds:120` stamp (`supervisor_intent_build.go:289`) AND the argv arm both shipped #488 (2026-07-02). Hosts that migrated serena to the dynamic pool in that ~5-week window carry serena-proxy rows with explicit deadline `0`. Reconcile drift-compare excludes the deadline field and a plain `mcphub supervise` restart does not rebuild serena rows, so those `0` values **persist**; deleting F5 does not re-stamp them.
- Result under 4a: those rows get 60s; serena's `uvx`+SolidLSP cold start (~46s+) exceeds it → `daemon-bind-timeout` restart cycle — the **exact** failure §4 exists to prevent, merely relocated from legacy-unified onto the dynamic-pool rows.

### 4.2 Chosen (4b — resolver default keyed on server identity; ONE rule, ONE owner of the value)

1. `EffectiveStartupBindDeadlineSeconds(d)` resolves: **explicit descriptor field `>0` wins**; else the **manifest-declared deadline** if the resolved `(server,daemon)` identity has one `>0`; else the **server-identity default** — `SerenaStartupBindDeadlineSeconds (120)` when the **resolved** server identity (via `descriptorServerDaemon(d)`, not raw `d.Server`, so a blank-field legacy row recovered from `--server serena` args is covered too) equals `api.SerenaServerName`, otherwise `DefaultStartupBindDeadlineSeconds (60)`.
2. This ONE rule covers **all three** shapes with no per-shape branch: legacy-unified serena (resolved server `serena`, explicit 0 → **120**), serena-proxy pool rows (resolved server `serena`, `Daemon` = workspace-hash, explicit 0 → **120** via the same server-identity default — the population 4a dropped), and serena-proxy/LSP rows that carry explicit `120` (`supervisor_intent_build.go:289`, `register_supervisor.go:58` → explicit branch). It is the SAME identity predicate F5 already uses for its serena skip (`intent_port_backfill.go:222 if server == SerenaServerName`), so no new concept is introduced.
3. **Delete** the `isSerenaProxyDescriptor` arm from `supervisorStartupBindDeadline` (`supervise_liveness.go:520-522`); the cli function becomes a thin `time.Duration(api.EffectiveStartupBindDeadlineSeconds(d)) * time.Second`. `isSerenaProxyDescriptor` **stays** for its other, non-deadline callers (see §10 claim 7 for the full, corrected reference set).

**Resolving the original 4b objection (a "second owner of the 120 magic value").** The prior §4 rejected 4b because it "re-homes the 120 into code as a second owner alongside the manifest constant." That objection dissolves once the value has **exactly one home**: the `120` (and the `60`) become **named constants in the owner package** — `api.SerenaStartupBindDeadlineSeconds = 120` and `api.DefaultStartupBindDeadlineSeconds = 60` in `internal/api/supervisor_port_owner.go`, referenced solely by `EffectiveStartupBindDeadlineSeconds`. There is no competing manifest source of the number under 4b (see §4.3). The existing cli constant `supervisorSerenaStartupBindDeadline` (`supervise_liveness.go:35`) has **no other consumer** once the arm is deleted (verified: its only reference is `:521`), so it is removed; the cli `supervisorDefaultStartupBindDeadline` (still used by tests) is re-pointed to derive from `api.DefaultStartupBindDeadlineSeconds` (or the tests reference the api constant directly). Net: the value lives once, in the owner. Package-direction note — the owner is the correct single home precisely because the resolver is in `internal/api` and `internal/api` **cannot** import `internal/cli`; homing the constant in cli (where it is today) would make the resolver unable to reference it.

### 4.3 Decision: DROP the 4a manifest edit (do not keep it as belt-and-suspenders)

4b covers the `unified` shape by itself (resolved server `serena` → 120 default), so the 4a manifest edit is **redundant** under 4b. Keeping it would re-introduce a **second source of the 120** (the manifest's `unified.startup_bind_deadline_seconds` alongside the owner constant) — the precise "one owner per cross-cutting invariant" violation this work removes, and it would mask a resolver-default regression behind a manifest value for the one shape that happens to match. **The 4a manifest edit is dropped.** `servers/serena/manifest.yaml` is REMOVED from the change surface (§6). The resolver — with its single owner-package constants — is the **sole authority** for the serena deadline. (The general `explicit field > manifest deadline > server-identity default` precedence still stands so that any FUTURE manifest that *does* declare a per-daemon deadline is honored; serena simply does not declare one, and does not need to.)

---

## 5. F5 deletion analysis (item 4)

F5 (`BackfillIntentDaemonPorts`) does three things: **(a)** port backfill, **(b)** identity-heal (`Server`/`Daemon` from args), **(c)** deadline stamp. Mapping each to the owner:

| F5 job | Covered by owner? | Residual requirement to delete |
|---|---|---|
| (a) port | Yes — `EffectiveDaemonPort` in liveness/squatter/recover/startup-scan | those 4 consumers migrated (§3.4) |
| (c) deadline | Yes — `EffectiveStartupBindDeadlineSeconds` | §4 adopted |
| (b) identity-heal | Yes — `descriptorServerDaemon` in the squatter argv gate + status already recovers from task name | squatter classifier resolves identity via owner (§3.4) |

**Verdict: F5 (`BackfillIntentDaemonPorts`) can be DELETED in full** — it is a cache-warmer, not structurally necessary, because **no** consumer is unable to reach the pure owner (all are in-process Go with embedded-manifest access, including the out-of-process `recover` CLI). Deletion is conditional on the three migrations above landing first. Deleting F5 removes: `internal/api/intent_port_backfill.go`, `internal/api/intent_port_backfill_test.go`, the call site + 4 event emits at `internal/cli/supervise.go:777-829`, **and** both accreted special-cases (`d.Port>0 || d.RuntimeSpec!=nil`, `server=="serena"`).

**What still legitimately needs the persisted `Port`** (and therefore keeps the field + its authoritative writers): the spawn contract (`daemon_serena.go`), runtime_spec rows (no manifest port), reconcile drift-compare, force-stop kill-by-port, migrate expected-ports, and display. None of these is F5 — they read the field the **authoritative writers** stamp, which is unaffected by deleting F5.

**Sequencing safety valve (if the reviewer wants a smaller first PR):** land §4 (the owner's server-identity deadline resolver + delete the `isSerenaProxyDescriptor` deadline arm) first, then delete **only** F5's `server=="serena"` arm — because once the deadline resolves correctly for **both** serena shapes regardless of the stamped port, F5 stamping serena's 9121 is harmless and the skip is dead weight. This removes the flagged special-case immediately while F5's full deletion follows behind the §3.4 migrations. This is a valid intermediate, not the end-state.

---

## 6. Change-Surface Contract

```
{
  intended change surface:
    - NEW  internal/api/supervisor_port_owner.go  (the single resolver;
           hosts EffectiveDaemonPort, EffectiveStartupBindDeadlineSeconds,
           NewDaemonPortResolver, descriptorServerDaemon [moved],
           resolveManifestPortAndDeadline [moved])
    - EDIT internal/cli/supervise_liveness.go       (sweep resolves effective
           port per daemon; supervisorStartupBindDeadline delegates + drops
           the isSerenaProxyDescriptor arm; supervisorIntentPortMapForStateDir
           maps resolved port)
    - EDIT internal/cli/supervise_status.go         (use owner resolver; drop
           private memo + "NOT redundant by F5" comment)
    - EDIT internal/cli/supervise_squatter.go       (identity via owner in the
           argv gate)
    - EDIT internal/cli/daemon_recover.go           (resolve effective port at
           entry; drop the Port<=0 skip)
    - EDIT internal/cli/supervise.go                (delete F5 call site + events;
           startup-scan reads resolved port map)
    - EDIT internal/cli/supervise_liveness.go       (deadline resolver delegates
           to api.EffectiveStartupBindDeadlineSeconds; drop the now-unused
           supervisorSerenaStartupBindDeadline const; the 120/60 values live
           ONLY in the owner package — §4.2)
    - EDIT internal/api/migrate.go                  (ResolveManifestDaemonPort →
           thin re-export of owner, or delete)
    - (DROPPED) servers/serena/manifest.yaml — the 4a manifest edit is NOT made;
           the owner's server-identity default is the sole authority (§4.3).
    - DELETE internal/api/intent_port_backfill.go (+ _test.go)   [F5, Tier 2]

  approved extension seam(s):
    - api.EffectiveDaemonPort / EffectiveStartupBindDeadlineSeconds /
      NewDaemonPortResolver  — the ONE port/deadline resolution surface.
      A new port-decision consumer calls the owner; it does NOT read d.Port
      raw for a protection decision, and it does NOT add a manifest lookup
      of its own.

  protected / must-not-touch surfaces:
    - SupervisorDaemon.Port + StartupBindDeadlineSeconds JSON schema/shape
      (byte-symmetric on-disk; additive discipline; no migration).
    - The authoritative WRITERS: supervisorDaemonsFromPlan
      (install_parsed_manifest.go), BuildSupervisorDaemonsForSerena
      (supervisor_intent_build.go), the LSP register builder
      (register_supervisor.go). Port/deadline WRITE ownership is unchanged.
    - The spawn contract: daemon_serena.go descriptor↔spec port assertion.
    - Reconcile drift-compare (supervise_reconcile_ipc.go:814) — stays RAW
      cache-vs-cache; must NOT lazy-resolve.
    - isSerenaProxyDescriptor's non-deadline callers (reconcile/respawn/
      controller/squatter-shape).
    - PR #504 serena-guard is superseded LATER by F5 deletion, not edited now.

  declared blast radius:
    - Behavioral change is confined to PORT-PROTECTION DECISIONS for
      Port=0 legacy rows (they gain protection) + the serena first-bind
      deadline: EVERY server-identity-serena descriptor with no explicit
      deadline now resolves 120s — legacy-unified rows AND the serena-proxy
      dynamic-pool rows carrying explicit deadline 0 (the ~5-week #234..#488
      migration population, §4.1). No on-disk schema change; no change to
      spawn, display, install-write, or drift-detection behavior for
      Port>0 rows. runtime_spec rows unchanged (always Port>0). No change to
      any manifest file (§4.3).
}
```

---

## 7. ADR-style tradeoffs (item 5)

**Chosen approach: lazy-resolve at each decision boundary + persisted cache + delete F5.**

**Alternative A — persist-once-at-startup (keep F5, add owner as defense-in-depth).** Keep F5's write-convergence so the on-disk descriptor becomes self-consistent for *all* consumers, and add the owner so decisions don't *depend* on F5 having run. Pro: belt-and-suspenders; covers F5-not-yet-run windows (contended flock, pre-restart host). Con: keeps the exact two-owner duplication the task exists to remove; F5's special-cases persist; "who answers the port question" stays ambiguous. **Rejected** — it treats the symptom (cache cold) not the cause (two owners).

**Alternative B — make `Port` derived-only (delete the field as a decision input everywhere, resolve on every read).** Pro: maximal single-owner purity. Con: breaks the spawn contract and runtime_spec rows (no manifest source), forces lazy-resolve into ~40 non-decision read sites (display, drift, migrate, force-stop), and phantom-drifts the reconcile compare. **Rejected** — blast radius is disproportionate and some readers have no derivable source.

**Alternative C — chosen.** Resolve lazily **only at the port-decision boundaries** (a bounded, enumerable set: §3.4), keep `Port` as a persisted spawn-cache written only by the authoritative writers, delete F5. Pro: one owner for the *decision*, smallest durable surface, no schema change, no spawn-path risk. Con: the persisted `Port=0` is never rewritten (a legacy row's on-disk field stays 0) — accepted, because the *display* already tolerates it and the *decision* now resolves it; the field is honestly a cache, not a lie.

**Blast radius (files) — named in §6.** Behavioral surface: only Port=0-legacy protection activation + serena deadline correctness.

**Migration / back-compat.** No on-disk format change. Existing descriptors with `Port=0` need **no** migration — resolved lazily. `Port>0` descriptors (fresh installs, post-F5 hosts) short-circuit to the cached value unchanged. Deleting F5 means startup no longer rewrites the intent file — a host that was F5-backfilled keeps its ports; a fresh legacy host simply resolves lazily instead. Byte-symmetric rollback preserved (removing a startup write-pass changes no schema).

**Failure modes.**
- *Manifest missing/renamed at resolve time:* owner returns `ok=false` → the row keeps "no port protection" (identical to today's F5 `UnresolvedPortZero`), but the miss is now visible at resolve time (an event, §8) instead of a structural silent bypass. No regression vs F5, which also left these untouched.
- *runtime_spec serena/LSP:* `d.Port` always `>0` → owner returns it; manifest never consulted. Correct (template has no per-workspace port).
- *Concurrent manifest change:* manifest is embedded (compiled-in) for the port lookup → immutable at runtime, no race. The intent `d.Port` cache is re-read per sweep as today.
- *Deadline for a Port>0 / deadline=0 descriptor:* the deadline resolver does NOT short-circuit on `d.Port`; it resolves independently (explicit field > manifest daemon deadline > server-identity default), so a port-stamped-but-deadline-zero row (the post-F5-port-only state) still resolves 120s when its resolved server identity is `serena`. This is the separation that makes §4 correct, and it is exactly why the serena-proxy pool rows (port>0, deadline=0, `Daemon`=workspace-hash → manifest miss) resolve 120s under 4b but would have fallen to 60s under 4a (§4.1).

**Security-by-design.** No new kill/authority surface. The squatter argv gate keeps its exact-token discipline (D-A); only the *identity source* changes from raw fields to `descriptorServerDaemon` (which recovers identity from the daemon's own `--server/--daemon` args — the same tokens the spawn uses, so they cannot be attacker-forged relative to the descriptor). Port comes from operator-authored owner-only-DACL intent + embedded manifest — both trusted inputs. No change to `TerminatePIDWithIdentity`, rate limits, or fail-closed posture.

---

## 8. Observability expectations

- Liveness resolve-miss (`!ok` for a row that *is* a manifest daemon, i.e. a renamed/removed manifest): emit a `debug`/`warn` `daemon-port-unresolved`-style event from the sweep (carry task, server, daemon) — the successor to F5's `intent-port-unresolved` (`supervise.go:822-828`), so a genuinely unprotected daemon stays visible. Keep it `debug` for recurring benign misses (timer rows are filtered upstream by `ok=false` on non-daemon shapes) per the existing "don't cry wolf" rationale (`supervise.go:815-821`).
- Deleting F5 removes its `intent-port-backfilled` / `intent-port-unresolved` / `intent-port-backfill-*` events — acceptable, they were startup-write telemetry; the resolve-miss event above replaces the only operator-actionable one.
- Recover: when the effective-port resolve misses, keep an explicit operator warning (successor to `daemon_recover.go:171-172`).

## 9. Test strategy

- **Owner unit tests** (`internal/api`): `EffectiveDaemonPort` — (Port>0 short-circuit; Port=0→manifest; runtime_spec→Port; renamed manifest→ok=false; blank fields recovered from args; timer row→ok=false). `EffectiveStartupBindDeadlineSeconds` — (explicit field wins; manifest daemon-deadline wins over default; **serena legacy-unified (server `serena`, daemon `unified`, no explicit field) → 120 via the server-identity default [§4.2]**; **serena-proxy pool row (server `serena`, `Daemon`=workspace-hash so the manifest daemon lookup MISSES, explicit field 0) → 120 via the SAME server-identity default — the 4a-regression guard, §4.1**; non-serena global with no explicit/manifest deadline → 60; blank-field legacy row whose `--server serena` args resolve → 120). `DaemonPortResolver` once-per-server memo assertion (port the existing `supervise_status_manifest_memo_test.go` seam to the owner). Move the surviving assertions from `intent_port_backfill_test.go` here before deleting it.
- **Liveness** (`internal/cli`): a Port=0 legacy descriptor whose manifest declares a port is now `port_unbound`/`port_owner_*`-classified (not early-healthy); the startup running-scan re-checks it. Retarget `supervise_liveness_bind_deadline_test.go` and `supervisor_startup_bind_deadline_test.go` to the owner; assert both legacy-unified serena (args `daemon --server serena --daemon unified`, no explicit field) AND a serena-proxy pool descriptor (args `daemon serena-proxy --server serena …`, `Daemon`=workspace-hash, explicit field 0) resolve 120s through `supervisorStartupBindDeadline` after the `isSerenaProxyDescriptor` arm is deleted.
- **Squatter**: a blank-identity global legacy row (`Server=""`, args carry `--server/--daemon`) classifies `own_task` on a matching squatter (identity recovered via owner) — the case F5's PR #504 blank-field heal covered.
- **Recover**: a Port=0 descriptor whose manifest resolves a port no longer prints the skip-warning and proceeds to the squatter check.
- **Regression guards**: reconcile drift-compare still compares raw fields (no lazy resolve); serena-proxy + LSP proxy rows carrying explicit 120 keep it (explicit branch); serena-proxy pool rows carrying explicit 0 resolve 120 via server-identity (the 4a regression this revision fixes — §4.1); a non-serena global daemon with no explicit/manifest deadline stays 60 (no serena over-reach); Port>0 rows are byte-identical through resolve; no manifest file is modified (grep the diff for `servers/serena/manifest.yaml` returns empty — §4.3).
- Repo-standard gates (CLAUDE.md Step 1): `go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...` + the `test_state_path_env` tagged run over `./internal/api/ ./internal/cli/`.

---

## 10. Claims section (1:1 input to `$architecture-reviewer`)

1. `{ guarantee: "What port does daemon X use for a protection DECISION" has exactly ONE resolver (api.EffectiveDaemonPort); no decision path reads SupervisorDaemon.Port raw as source-of-truth; single-owner: internal/api/supervisor_port_owner.go; enforcement-probe: grep of internal/cli/supervise_liveness.go + supervise_squatter.go + daemon_recover.go + supervise.go startup-scan shows every port-decision goes through the resolver, and the two pre-existing manifest resolvers (migrate.go:376, intent_port_backfill.go:50) collapse to one }`
2. `{ guarantee: A Port=0 legacy descriptor whose manifest declares a port>0 no longer disables the liveness bind-check, P1b deadline, squatter reap, or recover — protections resolve lazily; single-owner: EffectiveDaemonPort; enforcement-probe: new liveness test asserts a manifest-backed Port=0 row is port-classified (not early-healthy at supervise_liveness.go:591) }`
3. `{ guarantee: BOTH serena on-disk shapes with no explicit deadline — legacy-unified (Daemon "unified") AND the serena-proxy dynamic-pool rows (Daemon = workspace-hash, explicit deadline 0) — resolve a 120s first-bind deadline via ONE server-identity rule, WITHOUT an argv-shape special-case and WITHOUT a manifest edit; a non-serena global daemon with no explicit/manifest deadline stays 60; single-owner: api.EffectiveStartupBindDeadlineSeconds keyed on resolved server == api.SerenaServerName, value from the sole constant api.SerenaStartupBindDeadlineSeconds (=120); enforcement-probe: owner test asserts 120s for both serena shapes and 60s for a non-serena global; grep shows the isSerenaProxyDescriptor arm removed from supervisorStartupBindDeadline (supervise_liveness.go:520) AND no diff to servers/serena/manifest.yaml (§4.3) }`
4. `{ guarantee: F5 (BackfillIntentDaemonPorts) and BOTH its special-cases (Port>0||RuntimeSpec, server=="serena") are deleted; the persisted-port purpose is fully absorbed by the owner; single-owner: the owner + authoritative writers; enforcement-probe: internal/api/intent_port_backfill.go(+_test) absent; grep for BackfillIntentDaemonPorts returns only history/docs; no startup write-pass rewrites supervisor-intent.json }`
5. `{ guarantee: SupervisorDaemon.Port stays a persisted cache written only by install fan-out + serena/LSP builders; the spawn contract, runtime_spec rows, and reconcile drift-compare are unchanged; single-owner: supervisorDaemonsFromPlan / BuildSupervisorDaemonsForSerena / LSP register builder; enforcement-probe: grep shows no new writer of Port; daemon_serena.go:255 assertion untouched; supervise_reconcile_ipc.go:814 still compares raw fields }`
6. `{ guarantee: No on-disk schema change and no migration — existing Port=0 and Port>0 descriptors round-trip byte-identically; single-owner: SupervisorDaemon JSON tags (supervisor_intent.go:52-97); enforcement-probe: no struct-tag/field change in the diff; round-trip test on a pre-change intent fixture }`
7. `{ guarantee: isSerenaProxyDescriptor's non-deadline callers are untouched — the deadline arm at supervise_liveness.go:520 is the ONLY reference removed; single-owner: internal/cli/supervise.go:2949 (definition); enforcement-probe: grep for isSerenaProxyDescriptor over internal/cli shows exactly these production references surviving — supervise.go:3388 (serena-proxy intent-path env-channel injection), supervise_reconcile.go:190 (reconcile spawn-gate), supervise_respawn.go:196 (respawn), supervisor_controller.go:2795 (controller), supervise_squatter.go:203 (squatter argv shape), supervise_squatter.go:233 (isGlobalDaemonDescriptor exclusion) — plus the definition at supervise.go:2949 and the test refs in supervise_overlay_marker_spawn_test.go; only supervise_liveness.go:520-522 is gone }`

---

## 11. Non-goals (item 6)

- **PR #504's serena-guard stays as-is now.** This design *supersedes* it (F5 deletion, §5) but the sequencing is a separate PR behind its own review gate; do not edit PR #504's guard in this pass.
- **No force-stop migration in this scope** — `stop_force_supervisor.go`'s Port=0 gap is a real adjacent finding (§12), not admitted here; the orchestrator decides whether to fold it in.
- **No change to spawn, install-write, display, drift-detection, or IPC behavior for Port>0 rows.**
- **No new IPC verb, no new persisted field, no kill-authority change.**
- **`isSerenaProxyDescriptor` is not deleted** — only its deadline call site.

---

## 12. Adjacent findings

- **Force-stop shares the Port=0 gap.** `internal/api/stop_force_supervisor.go:155-182` falls back to PID-only kill when `d.Port == 0`, so a legacy row's kill-by-port safety net is off — the same structural gap this design closes for the liveness path. It is a *different verb* (force-stop, not liveness), so it is out of the admitted scope. Recommend filing `work-items/bugs/2026-07-05-force-stop-port-zero-gap.md` (`context: adjacent-finding`, `status: open`); the fix is a one-line `api.EffectiveDaemonPort` resolve at entry once the owner exists. Not blocking.

---

## 13. Gate

Traceable to accepted facts (status.md + verified `file:line`); alternatives, seams, dependency direction, blast radius, failure modes, observability, security, and test strategy explicit; the cross-cutting decision is filed as `work-items/decisions/2026-07-05-daemon-port-resolution-single-owner.md`, now `status: accepted` (promoted on the `$architecture-reviewer`'s verified §4a-regression finding, which §4 now incorporates via the §4b server-identity rule); no implementation code included.

**PASS** — ready for `$planner` to phase (recommend: Phase 1 = owner + §4 server-identity deadline resolver + delete the `isSerenaProxyDescriptor` deadline arm + delete F5's `server=="serena"` arm; Phase 2 = migrate liveness/squatter/recover/startup-scan to the owner; Phase 3 = delete F5 fully). The decision has been promoted `proposed → accepted` per the reviewer finding this revision closes; a subsequent `$architecture-reviewer` gate on the implementing PR maps the §10 claims 1:1 to findings.

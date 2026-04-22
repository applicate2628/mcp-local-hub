# PR #1 Codex Review Walkthrough — 27 rounds, 47 findings

**Context:** [PR #1](https://github.com/applicate2628/mcp-local-hub/pull/1) — Phase 3 Workspace-Scoped `mcp-language-server` Daemons with Lazy Materialization (18 M1–M5 tasks). Branch: `phase-3-workspace-scoped`. Codex reviewed 26 times; an additional manual review round (#27 = `819d0c6`) followed. Final tally: **47 findings — 21 P1, 25 P2, 1 P3.**

**Purpose:** Reference library for lazy-materialization / workspace-scoped daemon patterns. Findings cluster around scheduler rollback safety, Windows quirks, flock coordination, symlink canonicalization, JSON-RPC protocol correctness, and materialization lifecycle. Useful when building similar sidecar-per-workspace systems.

---

## Distribution

| Category | Findings | Key rounds |
|---|---:|---|
| Scheduler rollback & idempotent re-register | 10 | R1, R3, R5, R6, R9, R19, R20, R21, R22, R23 |
| Flock / registry coordination | 4 | R5, R6, R15, R16, R17 |
| Path canonicalization (symlinks, cleanup) | 4 | R9, R11, R18, R19, R25 |
| Legacy migration (one-off migrate-legacy path) | 5 | R5, R8, R9, R10, R11, R12 |
| JSON-RPC protocol correctness | 4 | R1, R2, R3, R6, R14 |
| Materialize / backend lifecycle | 5 | R3, R13, R14, R22, R23 |
| Port allocation | 2 | R13, R15 |
| Status enrichment | 5 | R1, R14, R22, R25, R26 |
| Weekly refresh upgrade-safety | 4 | R20, R23, R24 |
| Task naming / uniqueness (Windows) | 3 | R1, R9, R17 |
| Install health probe | 1 | R26 |

---

## Top files under review

| File | Findings |
|---|---:|
| `internal/api/register.go` | 13 |
| `internal/cli/daemon_workspace.go` | 5 |
| `internal/api/install.go` | 5 |
| `internal/api/workspace_path.go` | 4 |
| `internal/daemon/lazy_proxy.go` | 3 |
| `internal/api/legacy_migrate.go` | 3 |
| `internal/daemon/inflight.go` | 2 |
| `internal/daemon/backend_lifecycle.go` | 2 |
| `internal/api/weekly_refresh.go` | 2 |
| `internal/api/scheduler_mgmt.go` | 2 |

register.go at 13 findings reflects the complexity of atomic multi-resource orchestration: scheduler tasks + client entries + registry + weekly refresh + per-language rollback stack, all coordinated under a flock.

---

## I. Scheduler rollback & idempotent re-register (10 findings)

**Background:** `mcphub register <workspace> [lang...]` must be atomic — if any step fails (port allocation, task creation, client config write), previous side effects must unwind. `mcphub register` is also **idempotent**: re-registering an existing workspace should be a no-op when nothing changes, or a surgical update when something does.

### R6-round #6 — P1 — Kill proxy process in register rollback

**Problem:** `sch.Delete(taskName)` on Windows removes the task **definition** but does NOT terminate the running child process. If register fails after `sch.Run` succeeded but before client-config writes completed, the rollback stack deleted the task but left the live proxy holding the port. Next re-register attempt: `net.Listen` fails with "address already in use" even though no scheduler task exists.

**Fix:** rollback closure kills by port BEFORE `sch.Delete`:

```go
rollback = append(rollback, func() {
    if capturedPort > 0 {
        _ = killByPortFn(capturedPort, 5*time.Second)
    }
    _ = sch.Delete(capturedTaskName)
    // ...
})
```

### R6-round #8 — P1 — Avoid killing *existing* proxy on rollback

**Problem (follow-up):** R6's kill closure was added at task-creation time. But on idempotent re-register (task already existed + port held by a healthy proxy), if downstream failed, rollback would kill the still-running HEALTHY proxy.

**Fix:** kill-by-port only when `priorXML` was EMPTY (first-time register), OR after priorXML is restored via `ImportXML` + `Run`, in which case the restarted proxy is ours again.

### R6-round #10 — P1 — Preserve existing task when replacement creation fails

**Problem:** pattern was `sch.Delete(taskName)` → `sch.Create(newSpec)`. If Create failed (spec validation, scheduler service down), the user was left with NO task. Prior to this, install was "idempotent but brittle — mid-sequence failure left nothing."

**Fix:** snapshot via `sch.ExportXML` BEFORE Delete; rollback restores via `sch.ImportXML(capturedTaskName, capturedPriorXML)` if Create fails.

### R6-round #15 — P1 — Restore prior registry entry during re-register rollback

**Problem:** rollback removed the newly Put'd registry entry, but on re-register a PRIOR entry existed. After rollback the registry had NO entry for the (workspace, language), but the scheduler task was restored from `priorXML` → task pointed at a missing registry row → `workspace-proxy` subcommand exits with `"not registered"` → persistent outage.

**Fix:** rollback closure detects `capturedHad == true` and restores `capturedPrior` instead of Remove:

```go
if capturedHad {
    reg.Put(capturedPrior)
    _ = reg.Save()
    return
}
reg.Remove(capturedRegKey, capturedRegLang)
```

### R6-round #20 — P1 — Fail fast when task snapshot export fails on re-register

**Problem:** ExportXML can fail with various errors (permissions, XML corruption, scheduler service unreachable). Only `ErrTaskNotFound` is safe to ignore. Previously, any export error was treated as "no prior task exists" → rollback had no XML to restore → recoverable error became persistent outage.

**Fix:**

```go
var priorXML []byte
if xml, err := sch.ExportXML(taskName); err == nil {
    priorXML = xml
} else if !errors.Is(err, scheduler.ErrTaskNotFound) {
    return fmt.Errorf("export prior task %s: %w", taskName, err)
}
```

### R6-round #25 — P2 — Stop existing proxy before replacing a registered task

**Problem:** Delete-then-Create replace leaves the OLD running proxy alive with a now-dangling task definition. The new proxy (from Create+Run) binds the same port — bind fails.

**Fix:** kill by port BEFORE Delete:

```go
if len(priorXML) > 0 && port > 0 {
    _ = killByPortFn(port, 5*time.Second)
}
_ = sch.Delete(taskName)
```

### R6-round #29 — P2 — Preserve weekly_refresh on idempotent re-register

**Problem:** `mcphub register` had a CLI-level default `weekly_refresh=true`. Re-registering a workspace that was originally registered with `--no-weekly-refresh` would silently re-enable weekly refresh.

**Fix:** on re-register (`had == true`), preserve the prior `WeeklyRefresh` value instead of the new CLI default.

### R6-round #38 — P1 — Route lazy-proxy tasks through --all port resolution

**Problem:** `mcphub --all` flag paths in `install` / `restart` didn't discover workspace-proxy tasks. Their port came from the registry (not the manifest), so `api.Status().Port` was empty → health checks skipped silently.

**Fix:** `enrichStatusWithRegistry` populates port for lazy-proxy rows from registry lookup; scheduler-upgrade path rewires task args to match new canonical mcphub path.

> **Insight:** Rollback safety of a multi-resource orchestrator is harder than it looks. The register flow has 4 resources in play (scheduler task + registry row + N client entries + optional weekly-refresh task) and each has its own restore semantics. Getting it right requires:
>
> 1. **Snapshot BEFORE destroy.** `ExportXML` first, then `Delete`, then `Create`. Never the reverse.
> 2. **Distinguish "first-time" vs "re-register".** Rollback closures must branch on `capturedHad`: absent prior → remove, existed prior → restore.
> 3. **LIFO closure stack.** Each successful step appends its own rollback; errors trigger reverse unwind. No ad-hoc `defer` tricks.
> 4. **Kill-by-port, not delete-only, for external state.** Windows scheduler doesn't own the child process lifecycle; you do.
> 5. **"Fail fast" for snapshot failures.** A partial snapshot is worse than no operation — convert the op to a hard error instead.

---

## II. Flock / registry coordination (4 findings)

**Background:** Workspace registry lives at `%LOCALAPPDATA%\mcp-local-hub\workspaces.yaml` and is guarded by an adjacent `.lock` flock (`gofrs/flock`). Multiple concurrent `mcphub register` / `mcphub unregister` / `workspace-proxy` startups must coordinate.

### R6-round #1 — P1 — Ignore lifecycle writes for unregistered entries

**Problem:** `workspace-proxy` subcommand updated `DaemonStatus.Lifecycle` in registry on state transitions. But if the entry was concurrently removed by `unregister`, the proxy's lifecycle write would ADD a ghost entry back. "Ghost-resurrection" bug.

**Fix:** lifecycle update is a no-op when the entry doesn't exist:

```go
// internal/api/workspace_registry.go
func (r *Registry) PutLifecycle(key, lang string, lifecycle string, lastErr string) {
    existing, ok := r.Get(key, lang)
    if !ok {
        return  // ghost-resurrection guard
    }
    // ... update existing, Put back
}
```

### R6-round #12 — P1 — Persist registry entry before running workspace-proxy task

**Problem:** Register flow: Put entry in-memory → Create scheduler task → Run task → Save registry to disk. Proxy started (from Run) reads registry from disk → sees NO entry → exits "not registered" → readiness probe times out → rollback.

**Fix:** Save registry to disk BEFORE `sch.Run`. The spec comment at [register.go:412](internal/api/register.go#L412) states:

```
// Start the proxy. Registry is already persisted, so daemon startup
// finds the entry. Logon-triggered tasks only fire at the next logon,
// so this sch.Run prevents the port from advertising dead until reboot.
```

### R6-round #30 — P1 — Serialize startup check with registry to prevent unregister race

**Problem:** `workspace-proxy` startup order was: `reg.Lock()` → `reg.Load()` → `reg.Get(key, lang)` → `unlock` → `proxy.Bind(port)`. Concurrent `unregister` could slip between unlock and Bind: delete entry + remove scheduler task, then our proxy binds the port and registers as a "rogue" listener with no registry backing.

**Fix:** hold the flock THROUGH `Bind`:

```go
unlock, err := reg.Lock()
if err != nil { return err }
// ...load, get, validate entry...
// Do NOT releaseUnlock() yet — hold through Bind.
if err := proxy.Bind(); err != nil { unlock(); return err }
releaseUnlock()  // now safe: port is listening, kill-by-port will find us
```

### R6-round #32 — P1 — Release registry lock before calling proxy.Bind (part 2)

**Problem (follow-up):** the above "hold through Bind" design hit a Windows flock-reentrancy issue. `Bind` internally called `PutLifecycle(LifecycleConfigured)` which tried to acquire the SAME flock → `LockFileEx` is not reentrant on Windows → deadlock.

**Fix:** split: the caller (daemon_workspace.go) holds the flock and calls `WriteConfiguredState(...)` directly (bypassing PutLifecycle's own lock acquire). Bind then takes no lock at all — it just net.Listen + populate struct fields. Refined in 3 rounds (R16 and R17 renamed/reshaped the internal API).

> **Insight:** Flock coordination across Go + OS boundaries is tricky. Windows `LockFileEx` is non-reentrant (even within the same process — contrary to POSIX `flock`). Any code path that acquires a flock + calls a helper that tries to acquire the same flock → deadlocks on Windows but works on Linux. Always document the lock-ownership contract at each function's godoc, and prefer a "lock-aware" overload (e.g. `PutLifecycleLocked(reg, ...)` that assumes the caller holds the lock) rather than recursive acquire.
>
> A Codex reviewer at round 16 then immediately flagged a subsequent deadlock at round 17 — that's the code-review equivalent of "regression test for architectural fix." Each ownership decision needs a dedicated test.

---

## III. Path canonicalization — symlinks (4 findings)

**Background:** workspace path → `sha256[:8]` key. The key must be stable across the lifetime of the registration: registering, operating, and unregistering even after the workspace directory is deleted. Canonicalization via `filepath.EvalSymlinks` is the obvious approach — but fails when the path doesn't exist.

### R6-round #17 — P2 — Resolve symlinked workspace paths before hashing

**Problem:** `CanonicalWorkspacePath("/home/u/project")` returned the lexical path. If `project` was a symlink to `/mnt/drive/project`, two registrations for the same underlying directory would get different keys → same workspace registered twice under different names.

**Fix:** `filepath.EvalSymlinks` before `sha256[:8]`.

### R6-round #23 — P2 — Keep cleanup key stable when symlink target is gone

**Problem:** On `unregister`, we need the key to match the original register. If a symlink in the workspace path now points to a deleted target, `filepath.EvalSymlinks` returns an error → we can't compute the cleanup key → `unregister` fails with "symlink target not found".

**Fix:** `CanonicalWorkspacePathForCleanup` — a forgiving variant that falls back to lexical path if `EvalSymlinks` fails on any ancestor, preserving the same key the original register produced.

### R6-round #35 — P2 — Resolve broken parent symlinks in cleanup

**Problem:** cleanup variant was too lax — it fell back to lexical path on ANY failure, including legitimate "path is genuinely different." Unregister would then fail to match existing registry rows.

**Fix:** resolve each ancestor component independently; only fall back for components that don't exist. A symlink pointing at a missing target for a non-leaf ancestor still resolves via the preserved logical name.

### R6-round #44 — P1 — Keep resolving ancestor symlinks for deleted workspace paths

**Problem (iteration 3):** when the ENTIRE workspace directory tree was deleted after register (user rm -rf'd their project), cleanup canonicalization stopped at the deepest surviving symlink. If the symlink CHAIN was broken past a certain depth, we couldn't resolve far enough to match the original registration's canonicalized key.

**Fix:** walk each path segment with `filepath.EvalSymlinks`; if THAT segment fails, treat it as "target missing" and preserve the incoming lexical segment; continue to parent segments.

```go
// internal/api/workspace_path.go (simplified)
func CanonicalWorkspacePathForCleanup(raw string) (string, error) {
    // Try full EvalSymlinks first (same as strict variant).
    if canonical, err := filepath.EvalSymlinks(raw); err == nil {
        return canonical, nil
    }
    // Fall back: canonicalize ancestor-by-ancestor.
    segs := strings.Split(filepath.Clean(raw), string(filepath.Separator))
    // ... walk segments resolving each
}
```

> **Insight:** Canonicalization for "operations that may outlive the referenced entity" (e.g., cleanup of a registration whose workspace no longer exists) requires forgiving-but-consistent resolution. Strict `EvalSymlinks` is wrong for cleanup — it fails on missing paths. Loose lexical cleaning is wrong too — it skips legitimate symlinks and computes a different key.
>
> Proper pattern: segment-by-segment canonicalization, preserving original segments when EvalSymlinks fails for that specific segment. Document the contract: "returns the best-effort canonical path; for segments that don't exist, returns the input segment unchanged."

---

## IV. Legacy migration (5 findings)

**Background:** `mcphub migrate-legacy` detects existing stdio entries for `mcp-language-server` in client configs and converts them to registered workspace-scoped daemons. One-off tool, complex detection logic.

### R6-round #11 — P2 — Route migrate-legacy progress logs to stderr

**Problem:** `mcphub migrate-legacy --json` printed progress ("✓ detected serena in codex-cli...") and the final JSON report to the SAME stdout. Scripts piping `| jq` got junk before the JSON.

**Fix:** progress logs go to stderr; stdout only emits the JSON report on `--json`.

### R6-round #16 — P1 — Require explicit `enabled = false` for Codex legacy rows

**Problem:** Codex CLI config stores MCP servers with an `enabled` bool. Legacy detection treated "field absent" as "enabled: true" — standard TOML convention. But that's also how Codex represents a DISABLED server (it omits the field + adds a user-edited comment). So migrate-legacy tried to migrate entries the user had intentionally disabled.

**Fix:** require explicit `enabled = false` to skip. Absent `enabled` field → treat as enabled. The TOML convention still holds; but the user workflow for disabling was ambiguous — require the explicit literal.

### R6-round #19 — P1 — Avoid deleting newly migrated client entries

**Problem:** migrate-legacy iterated: (a) read legacy entry → (b) call `api.Register` which wrote a NEW entry → (c) delete the legacy entry. But Register's new entry and the legacy entry had the SAME name (`mcp-language-server-<language>`). Step (c) deleted the just-written row.

**Fix:** distinguish by content (URL/transport), not name. The legacy entry is stdio-shaped; the new entry is http-shaped. Delete only if the current entry still matches the LEGACY shape.

### R6-round #22 — P1 — Handle EOF as declined confirmation

**Problem:** migrate-legacy prompts `Migrate N workspaces? [y/N]`. `fmt.Scanln` in a pipe-less environment returns `io.EOF`. Code treated EOF as "no input yet, retry" → infinite loop.

**Fix:** EOF → declined (same as "N" / empty line).

### R6-round #21 — P2 — Scope in-place replacement check to current workspace

**Problem:** when a legacy entry was migrated for workspace A, a later check for workspace B's entry with the same name saw A's new http-shaped entry and treated B as "already migrated." Result: B's legacy stdio entry silently survived.

**Fix:** scope the "already migrated" check to the CURRENT workspace's canonical path. Two workspaces writing the same server name get separate migration tracking.

---

## V. JSON-RPC protocol correctness (4 findings)

**Background:** lazy_proxy multiplexes synthetic handshake (initialize + tools/list) with proxied tool calls to the real backend. JSON-RPC id correlation is load-bearing — clients match response id to their outgoing request id.

### R6-round #4 — P2 — Preserve JSON-RPC ids in synthetic ping replies

**Problem:** synthetic `ping` handler hard-coded `"id": 1` in the response. Any client that sent multiple pings concurrently got replies all labeled id=1 → client's id→request map matched the wrong callback.

**Fix:** echo the incoming `req.ID` on the synthetic response.

### R6-round #9 — P2 — Handle unrecognized notifications without request forwarding

**Problem:** lazy proxy saw a method like `notifications/progress` (MCP supports arbitrary notification methods). Dispatch logic hit `default: forward to backend`. For a pre-materialization call, this triggered materialize for a notification that the client expected to be fire-and-forget.

**Fix:** whitelist of synthetic-respondable notifications (`notifications/initialized`, `notifications/cancelled`); others are DROPPED structurally — don't materialize a backend just to swallow a notification.

### R6-round #14 — P1 — Preserve original JSON-RPC id in forwarded responses

**Problem:** proxying a `tools/call` to the materialized backend: the backend has its OWN id numbering (StdioHost rewrites ids). The `host.go` id-mux mapped response back by internal counter, but when extracting to write back to the client, the ORIGINAL client id was lost in one path. Client saw response with wrong id.

**Fix:** double-mapping: `clientID → internalID → backendID` on way in; reverse on way out. Both rewrites happen in stdio_host; proxy just passes through.

### R6-round #28 — P1 — Ignore client-cancel errors before tearing down backend

**Problem:** when an MCP client cancelled a request mid-flight (closed HTTP connection), proxy's Serve goroutine got `context.Canceled`. Teardown path called `Lifecycle.Stop()` → killed the backend subprocess → NEXT tools/call had to re-materialize.

**Fix:** distinguish client-cancel from genuine backend failure. `isClientCancelErr()` helper checks for `ctx.Canceled`, `http.ErrAbortHandler`, EOF on reader → treat as benign, DO NOT teardown.

> **Insight:** JSON-RPC over HTTP with id rewriting through a proxy is a classic place for off-by-one ownership bugs. Every hop (client → proxy → stdio host → backend) potentially rewrites ids for internal correlation. Every hop must reverse the rewrite on the way out. The invariant: **the id the client sent equals the id the client sees in the response.** Tests must cover concurrent requests with overlapping ids, not just serial single-request flows.

---

## VI. Materialize / backend lifecycle (5 findings)

### R6-round #7 — P1 — Classify missing wrapped LSP binaries as missing

**Problem:** `mcp-language-server --lsp pyright-langserver` — `mcp-language-server` exists but `pyright-langserver` is not on PATH. `Materialize` spawned `mcp-language-server` subprocess; it died on its own when it couldn't find pyright. The proxy classified this as `LifecycleFailed` (generic) instead of `LifecycleMissing` (specific user-actionable).

**Fix:** preflight check via `exec.LookPath(cfg.LSPCommand)` BEFORE spawning the wrapper; return `errMissingBinary` which the outer layer maps to `LifecycleMissing`.

### R6-round #24 — P1 — Isolate materialization context from request cancellation

**Problem:** backend spawn could take 5–15s (downloading language server via uvx, indexing). During that window, the client HTTP request's `r.Context()` could cancel. Materialize was using that ctx → killed mid-spawn → next tools/call restarted from scratch (retry throttle or not).

**Fix:** detach the materialize ctx from the request ctx:

```go
// internal/daemon/lazy_proxy.go
detachedCtx := context.WithoutCancel(r.Context())
detachedCtx, cancel := context.WithDeadline(detachedCtx, time.Now().Add(materializeTimeout))
defer cancel()
endpoint, err := p.gate.Do(key, func(ctx context.Context) (MCPEndpoint, error) {
    return p.cfg.Lifecycle.Materialize(ctx)
}, detachedCtx)
```

`context.WithoutCancel` (Go 1.21+) keeps values but drops cancellation; `WithDeadline` adds back a bounded deadline independent of request.

### R6-round #40 — P2 — Preserve request deadlines when detaching materialize context

**Problem (follow-up):** R24's detach dropped the REQUEST deadline too. If the client specified `Connection: close + Deadline: 30s`, the proxy's materialize would run for materializeTimeout (e.g. 60s) even though the client had already timed out at 30s.

**Fix:** use the **shorter** of (request deadline, materialize timeout):

```go
if deadline, ok := r.Context().Deadline(); ok {
    if deadline.Before(time.Now().Add(materializeTimeout)) {
        detachedCtx, cancel = context.WithDeadline(detachedCtx, deadline)
    }
}
```

### R6-round #31 — P2 — Ensure lifecycle teardown on listener error return path

**Problem:** `daemon_workspace` RunE had a path where `net.Listen` errored AFTER Lifecycle construction. Early-return path left the Lifecycle in a "constructed but no cleanup" state — not critical because no subprocess was spawned yet, but would become a leak if Lifecycle's constructor started acquiring resources (it does: a log-file writer).

**Fix:** defer `_ = lc.Stop()` immediately after construction. Idempotent Stop() (already had for post-materialize cleanup) is the load-bearing primitive.

### R6-round #41 — P2 — Stop proxy lifecycle before returning on Serve failure

**Problem:** `proxy.Serve()` returned an error (not `ErrServerClosed`). The RunE returned immediately without calling `proxy.Stop()` → materialized backend leaked as an orphan process until scheduler restart.

**Fix:** on any non-clean Serve error, invoke `proxy.Stop(context.WithTimeout(..., 5*time.Second))` before returning.

> **Insight:** Lifecycle management across nested async boundaries needs explicit ownership tables. For lazy_proxy:
>
> | State | Who can cancel | Load-bearing cleanup |
> |---|---|---|
> | Materializing | proxy timeout, NOT request ctx | `Lifecycle.Stop()` at end of attempt |
> | Serving | ctx (from CLI signal handler) | `proxy.Stop(5s)` before return |
> | Request in-flight | request ctx | just return error, backend stays alive |
>
> Each row has a different cleanup path. Mixing them up causes subtle leaks or premature teardowns. `context.WithoutCancel` (Go 1.21+) is the primitive for "keep values, drop cancellation."

---

## VII. Port allocation (2 findings)

### R6-round #26 — P1 — Check OS port availability before allocating proxy ports

**Problem:** `AllocatePort` scanned the registry for occupied ports in range [9200, 9299], returned the lowest free one. But nothing checked whether the OS actually had that port free — another process (unrelated) could be bound to it. Register proceeded, scheduler task created, proxy tried to bind → `address already in use`. Rollback fires 10s later.

**Fix:** after picking a candidate port from the registry scan, verify with a probe `net.Listen("tcp", "127.0.0.1:<port>")` + immediate `Close()`. Skip to next candidate on bind-fail.

### R6-round #34 — P2 — Validate scheduler port against registry before proxy bind

**Problem:** scheduler task args were stored at Create time with a specific port. If the registry was later edited by hand to change the port (dev workflow or corruption), the scheduler task args would drift from the registry's `entry.Port`. Proxy bound the scheduler-task port; registry expected different; client configs pointed at the registry port → clients hit the wrong listener.

**Fix:** workspace-proxy subcommand validates that `--port <flag>` matches `entry.Port` at startup; refuses to start with a clear "port mismatch, run `mcphub register` to reconcile" error.

---

## VIII. Status enrichment (5 findings)

### R6-round #5 — P2 — Filter workspace rows by task name, not registry fields

**Problem:** `Status()` iterated scheduler tasks. To show the 5-state lifecycle for workspace-scoped rows, enrichment tried to match by manifest field `entry.Workspace`/`.Language`. But registry-load could fail (partial permissions, schema migration mid-upgrade), leaving those fields empty. Enrichment then couldn't identify the row as workspace-scoped — it rendered as a "global task with a weird name."

**Fix:** use task-name structural match (`IsLazyProxyTaskName` regex: `^mcp-local-hub-lsp-<8hex>-<lang>$`) as the discriminator. Registry fields are overlays that enhance the display; the TASK NAME alone determines whether the row is a lazy proxy.

### R6-round #27 — P2 — Avoid state rewrite when lazy-proxy port cannot be resolved

**Problem:** enrichment tried to resolve port by registry lookup. If registry load failed, port was 0. Some downstream consumers (health probe) treated port=0 as "skip probe" (correct). Others tried to connect to 127.0.0.1:0 → error → marked the row as "unhealthy." False-red UX.

**Fix:** when port is 0 + registry load failed, enrichment preserves the row's raw Lifecycle state (don't overwrite with a synthesized "failed" state).

### R6-round #33 — P2 — Mark lazy-proxy probes as synthetic by task type

**Problem:** health probe logic was "send a random MCP method and expect 200." For global daemons this was fine. For lazy proxies, certain methods (tools/call) would TRIGGER backend materialization as a side effect of the probe — not desired for health monitoring.

**Fix:** for lazy-proxy rows, probe only with synthetic-respondable methods (`initialize` or `tools/list`). Both return static embedded catalog without touching the backend.

### R6-round #46 — P2 — Propagate lazy-proxy task listing errors in stop/restart

**Problem:** `mcphub stop --all` and `mcphub restart --all` iterated `sch.List(prefix)`. The result was silently truncated on error — if scheduler service hiccuped, `--all` operated on only the first batch. Some daemons got stop/restart; others silently skipped.

**Fix:** return the List error all the way up; `--all` refuses to proceed on partial enumeration.

### R6-round #47 — P2 — Probe with a valid mcp-language-server tool invocation

**Problem:** `--force-materialize` health probe sent a synthetic initialize. Passed. But didn't verify the materialized backend was functional — "bind succeeded" and "backend answers MCP" are different checks.

**Fix:** probe sends a real `tools/call` with a simple valid argument (e.g., `go_workspace` for go backend). Completion without error = backend actually works.

---

## IX. Weekly refresh upgrade-safety (4 findings)

### R6-round #36 — P2 — Preserve existing weekly task when refresh task update fails

**Problem:** `EnsureWeeklyRefreshTask` did Delete-then-Create. If Create failed, existing weekly refresh was gone → no protection until next successful register.

**Fix:** ExportXML → Delete → Create → if Create fails, ImportXML restore. Same pattern as per-server task rollback.

### R6-round #39 — P2 — Keep workspace refresh task name upgrade-safe

**Problem:** old workspace-scoped refresh tasks had name `mcp-local-hub-weekly-refresh` (pre-Phase-3 convention). Phase 3 introduced a split: `mcp-local-hub-workspace-weekly-refresh` (workspace-shared) vs `mcp-local-hub-<server>-weekly-refresh` (per-server). On upgrade, the old task name was orphaned.

**Fix:** scheduler-upgrade detects old name + renames (Export old XML → Create new name with XML → Delete old).

### R6-round #42 — P1 — Upgrade workspace-proxy tasks during scheduler upgrade

**Problem:** `mcphub scheduler upgrade` rewires global daemon tasks to the current `canonicalMcphubPath()`. But lazy-proxy tasks (workspace-scoped) were skipped — their command was still pointing at the dev binary path.

**Fix:** enumerate all `mcp-local-hub-lsp-<8hex>-<lang>` tasks + rewire their command field.

### R6-round #43 — P2 — Rewire workspace weekly-refresh task on upgrade

**Problem (follow-up):** #42 missed the workspace-shared refresh task. Upgrade left it pointing at the dev path.

**Fix:** include `mcp-local-hub-workspace-weekly-refresh` in the upgrade-rewire enumeration.

---

## X. Task naming / Windows quirks (3 findings)

### R6-round #3 — P2 — Normalize task name before registry reload lookup

**Problem:** Windows Task Scheduler prefixes task names with `\` (folder separator). `sch.List` returns `\mcp-local-hub-lsp-<hex>-<lang>`. Code that matched task names against registry lookup (`strings.HasPrefix`) didn't strip the backslash → mismatch.

**Fix:** `strings.TrimPrefix(name, "\\")` on every task-name crossing.

### R6-round #18 — P3 — Ensure suffixed client entry names are globally unique

**Problem:** entry name resolution was `mcp-language-server-<lang>`. On cross-workspace collision (two workspaces register same language), a 4-hex suffix from workspace-key was appended. But the suffix algorithm could collide at high registration counts.

**Fix:** resolve collision by iterating: `-<4hex>`, then `-<4hex>-<nonce>`, checking existing entries after each iteration.

### R6-round #13 — P2 — Permit unregister for workspaces that no longer exist

**Problem:** Strict canonical path resolution (EvalSymlinks on deleted workspace) failed → user couldn't unregister a dangling workspace → scheduler task + client entries persisted forever.

**Fix:** `CanonicalWorkspacePathForCleanup` (see §III) as the unregister variant.

---

## XI. Manual review round #27 (`819d0c6`)

Three findings from manual reviewer (not Codex). Largely cross-cutting issues Codex had missed:

### P1 — Readiness probe: check BEFORE registering rollback

**Problem:** register flow: Put registry → Run scheduler task → `proxyReadinessFn(port, 10s)`. If readiness failed, rollback ran. But the rollback closure for registry-put had already been pushed. It was correct to run.

However: the **order** of rollback closures was "task delete" → "registry remove". The task-delete closure kills by port → proxy exits → its lifecycle teardown tries to update registry with `LifecycleFailed`. Registry had just been removed by the later closure. Ghost entry added.

**Fix:** reorder closures: registry-remove BEFORE task-delete. Also guards `PutLifecycle` against unregistered entries (the R1 fix to [workspace_registry.go](internal/api/workspace_registry.go) is the belt-and-suspenders backup).

### P2 — Weekly-refresh task creation must be BEFORE per-language tasks

**Problem:** register flow created per-language tasks first (inside `registerOneLanguage`), then `EnsureWeeklyRefreshTask` at the end. If the weekly task failed (permissions, scheduler issue), the per-language tasks already existed → user had daemons but no weekly refresh.

**Fix:** create/verify weekly refresh FIRST. Fail fast — if refresh can't be set up, don't touch anything else.

### P2 — `--force-materialize --health` must propagate tools/call errors

**Problem:** health probe for lazy-proxy rows sent `tools/call`. Implementation used SSE framing-tolerant decoder. But if the tools/call returned an API-level error in the JSON-RPC `error` field, the decoder returned nil error + ignored the payload. `api.Status` reported "healthy" for rows whose backend was actually broken.

**Fix:** probe reads JSON-RPC envelope, extracts `result` OR `error` explicitly, returns `error.message` string as Go error if present.

---

## XII. Meta-observations

### Trajectory across 27 rounds

- **R1–R8 (8 findings):** core correctness — JSON-RPC id preservation, protocol dispatch, lifecycle teardown. The "first-pass" issues every new subsystem surfaces.
- **R9–R15 (10 findings):** rollback & atomic multi-resource coordination. These are the hardest — register.go had 13 findings alone.
- **R16–R18 (4 findings):** flock reentrancy on Windows. Each fix exposed the next (deadlock → new deadlock).
- **R19–R26 (18 findings):** edge cases — symlinks, legacy migration quirks, upgrade-safety, status enrichment for partial-state rows.
- **R27 manual (3 findings):** cross-cutting architectural sequencing that Codex's per-file review didn't detect.

### Themes that required architectural (not line-level) fixes

1. **Multi-resource rollback.** Four primitives (scheduler task + registry + client entries + weekly refresh) coordinated via LIFO closure stack with per-step snapshot/restore. No syntactic pattern — you have to design it upfront.
2. **Windows flock reentrancy.** `gofrs/flock` does not protect against same-process re-acquisition on Windows. Any helper that acquires the lock must document it + accept a "caller-holds-lock" overload.
3. **Symlink resolution in cleanup paths.** Strict canonicalization fails on deleted paths; loose lexical canonicalization fails on legitimate symlinks. Segment-by-segment fallback is the correct pattern, documented in `CanonicalWorkspacePathForCleanup`.
4. **Materialize context lifetime.** Backend spawn can outlive the triggering request. `context.WithoutCancel` (Go 1.21+) is the load-bearing primitive; pair with `WithDeadline` to bound separately from the request.
5. **Task-name as structural identifier.** Scheduler task names encode structural information (`mcp-local-hub-lsp-<hex>-<lang>` → lazy proxy). Relying on THAT pattern survives partial registry failures; relying on registry fields doesn't.

### Convergence quality

Unlike PR #5 which had zero merged regressions across rollbacks, PR #1 had THREE chained fixes:
- R7→R8 (kill-proxy vs. don't-kill-existing-proxy)
- R16→R17 (hold flock through Bind → deadlock, release before → race)
- R24→R40 (detach ctx → also drop request deadline → combine)

Each chain is a Codex review doing its job: a fix for issue N surfaces edge case N+1 that was previously masked. PR #5's single-attempt convergence was luck/pattern-reuse; PR #1's multi-attempt chains are the realistic baseline for novel architectural work.

### Lessons for future workspace-scoped / lazy-daemon systems

1. **Always snapshot before destroy.** ExportXML (Windows Task Scheduler) or equivalent `cp` (everywhere else) — then Delete, then Create. Rollback restores from snapshot.
2. **Persist state atomically BEFORE side effects that depend on it.** Registry written to disk BEFORE `sch.Run` fires the proxy subprocess.
3. **Distinguish "first-time" vs. "re-register" in every rollback closure.** `capturedHad bool` at closure capture time is the idiom.
4. **Windows flock is non-reentrant.** Document lock ownership contracts; provide `*Locked` overloads.
5. **Detach long-running ops from request contexts.** `context.WithoutCancel` + explicit deadline, always.
6. **Task name is structural metadata; make it match a predicate (regex).** Registry fields can fail to load; task names can't.
7. **Per-row errors in aggregates need a dedicated error list.** `MigrateReport.Failed[]` + aggregator; nil top-level err doesn't mean "everything worked."
8. **Cleanup must be forgiving but consistent.** Segment-by-segment symlink resolution for deleted paths.

---

## Appendix: PR #4 — Post-merge security hotfix (3 rounds, ~4 findings)

**Context:** [PR #4](https://github.com/applicate2628/mcp-local-hub/pull/4) — combined #2 (hyperfine RCE gate) + #3 (serena supply-chain pin). Much smaller: 1 Codex finding (clean-passed round 2) + 2 manual review rounds.

### Codex round 1 — P2 — Surface hyperfine gate in discovery metadata

**Problem:** `MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE` gated `AddTool(hyperfine)`, so `tools/call hyperfine` returned method-not-found when closed. But `resource://tools` + `list_tools` still advertised it unconditionally. Clients following "check availability, then call" contract regressed from "advertised ⇒ callable" to "maybe callable."

**Fix:** `availableToolsMap()` strips hyperfine from discovery metadata when gate closed.

### Manual review round 2 — P2 — Docs drift on hyperfine gate

**Problem:** README/INSTALL/workflow.go advertised perftools as "4 tools" with unconditional `perftools.hyperfine(...)` examples, but the gate now hid it by default. Fresh install: 3 tools, not 4. Docs misled users.

**Fix:** README + INSTALL + workflow.go all updated to clearly flag opt-in + Windows env-var recipe.

### Manual review round 2 — P2 — Reinstall leaks obsolete weekly-refresh task

**Problem:** serena manifest flipped `weekly_refresh: true → false` in this PR. Installer now doesn't CREATE the weekly refresh task, but existing installs kept running the old one on reinstall (installer only touches planned tasks, not prune-unplanned).

**Fix:** `pruneObsoleteServerTasks` added to `executeInstallTo`. On full install, enumerate existing `mcp-local-hub-<server>-*` tasks; delete any not in current plan. 7 new unit tests cover the matrix.

### Manual review round 3 — P2 — `git diff --check` CRLF trailing whitespace

**Problem:** INSTALL.md was the only CRLF markdown file in repo (README.md and others were LF). No `.gitattributes`. `git diff --check` flagged every added line as trailing whitespace.

**Fix:** one-time CRLF→LF normalization, matching [serena manifest normalization](servers/serena/manifest.yaml) done in the same PR.

### Manual review round 3 — P3 — `resource://tools` wording

**Problem:** INSTALL.md:358 described the catalog as "four tools are installed" — wrong after gate closed hyperfine.

**Fix:** rephrased to "tools advertised by this daemon: three always-on analyzers + hyperfine only when the opt-in gate is open."

---

**Final verdict:**
- PR #1: merged via fast-forward after R27. 59 commits to master.
- PR #4: merged via fast-forward after R4 (1 Codex + 3 manual). 4 commits to master.
- PR #5: merged via fast-forward after R24. 48 commits to master.

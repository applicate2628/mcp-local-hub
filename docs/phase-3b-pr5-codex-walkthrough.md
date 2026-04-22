# PR #5 Codex Review Walkthrough — 24 rounds, ~47 findings

**Context:** [PR #5](https://github.com/applicate2628/mcp-local-hub/pull/5) — Phase 3B GUI MVP. Branch: `phase-3b-gui-mvp`. Codex reviewed the PR 24 times (each triggered via `@codex review` comment), emitting ~47 findings classified P1 (blocking) or P2 (important). This document groups them by category for post-mortem learning.

**Purpose:** Reference library of anti-patterns surfaced during a production-grade GUI review, with specific fix patterns applied. Useful when building similar local-server + web UI projects.

---

## Round-by-round summary

| Round | Commit reviewed | Findings | Key themes |
|------:|---|---:|---|
| R1 | `5435f26` | 3 | per-client scope, dirty-tracking, SSE leak |
| R2 | `db1c84d` | 2 | per-server client collapse, script async race |
| R3 | `14ccaaa` | 3 | lock-before-bind, log cursor, log rotation |
| R4 | `398c517` | 3 | pidport re-read, multi-daemon logs, XSS server.name |
| R5 | `61821bb` | 2 | poller composite key, dashboard composite key |
| R6 | `dd2ad55` | 2 | Servers aggregate, workspace-proxy filter |
| R7 | `7864e8c` | 2 | empty logs selection, Release unlock order |
| R8 | `beb6cd9` | 2 | migrate Failed rows, Release ownership |
| R9 | `33a797b` | 2 | Release TOCTOU, XSS in failed text |
| R10 | `8c54c0f` | 3 | status envelope crash × 2, strict workspace matcher |
| R11 | `d86702f` | 2 | Servers envelope, path-traversal daemon |
| R12 | `37ff5e6` | 2 | stream method gate, placeholder prime |
| R13 | `c212600` | 1 | validNameRe dots |
| R14 | `0181c2c` | 1 | placeholder tick loop |
| R15 | `25d8f98` | 1 | CSRF |
| R16 | `779367e` | 1 | URL parse loopback |
| R17 | `4a9fe0e` | 1 | no reverse-migrate |
| R18 | `6b1e116` | 2 | stale --force tip, mixed-failed aggregate |
| R19 | `001ccce` | 1 | loose workspace regex |
| R20 | `491ddd6` | 2 | weekly-refresh in Logs + Dashboard |
| R21 | `52f27f6` | 2 | placeholder prefix match, handshake JSON decode |
| R22 | `cd0ff32` | 2 | undefined routing, partial log lines |
| R23 | `e952110` | 1 | aggregateStatus maintenance filter |
| R24 | `761d3a3` | 0 | **"Didn't find any major issues. 👍"** |

---

## I. Security (4 findings)

### R11 — Path traversal in `daemon` query parameter

**Bug class:** untrusted input composed into filesystem path.

```go
// Before
daemon := r.URL.Query().Get("daemon")
body, _ := s.logs.Logs(server, daemon, tail)  // → api.LogsGet
// internal path: logDir + "/" + server + "-" + daemon + ".log"
```

**Attack:** `GET /api/logs/serena?daemon=../../etc/passwd` → reads `<logDir>/serena-../../etc/passwd.log` → escapes log directory.

**Fix:**

```go
var validNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
func validName(s string) bool {
    return s != "" && validNameRe.MatchString(s) && !strings.Contains(s, "..")
}
// at handler entry: reject with 400 before calling Logs()
```

> **Insight:** Any request parameter reaching filesystem/command/URL composition must be validated with an allowlist at entry. Blacklists (`"does not contain .."`) break on URL-encoded forms, Unicode normalization, symlinks. Regex `[A-Za-z0-9._-]+` + explicit `..` check is defense-in-depth: regex blocks slashes/empties, `strings.Contains` blocks dots separated by allowed chars.

---

### R15 — CSRF on mutating endpoints

**Bug class:** cross-origin POST bypass via browser.

`127.0.0.1` binding **does not** protect against CSRF. Any page in the user's browser (`evil.com`) can:

```html
<script>
fetch("http://127.0.0.1:9100/api/migrate", {
  method: "POST",
  body: JSON.stringify({servers:["memory"]}),
  headers: {"Content-Type": "application/json"}
});
</script>
```

CORS blocks reading the response but does not block the request. Drive-by config rewrite while `mcphub gui` is running.

**Fix (middleware):**

```go
func (s *Server) requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        sfs := r.Header.Get("Sec-Fetch-Site")  // modern browser signal
        if sfs != "" {
            if sfs == "same-origin" || sfs == "none" { next(w, r); return }
            http.Error(w, "cross-origin rejected", 403); return
        }
        // Fallback: Origin allowlist. Empty Origin = curl/native (ok).
        origin := r.Header.Get("Origin")
        if origin == "" { next(w, r); return }
        // ...match against http://127.0.0.1:<port> / http://localhost:<port>
    }
}
```

> **Insight:** "Localhost-only binding = safe" is one of the most common mental-model mistakes. The browser happily sends cross-origin POSTs to loopback. Two layers of defense:
>
> 1. **`Sec-Fetch-Site`** header from modern browsers (Chrome 76+/Firefox 90+/Safari 16+). `same-origin` or `none` = permitted.
> 2. **`Origin` allowlist** fallback for older browsers. Empty `Origin` = curl/native (non-browser), safe to allow.
>
> GET endpoints do not need the guard — the browser cannot trick a tab into reading a cross-origin response. Only POST/PUT/DELETE/PATCH.

---

### R4, R9 — XSS via unsanitized interpolation

**Bug class:** user-controlled string → `innerHTML`.

```js
// servers.js before
row.innerHTML = `<td>${server.name}</td>...`;  // server.name comes from client config
```

**Attack:** a config entry named `<img src=x onerror=alert(1)>` → JS executes in the localhost GUI context → can hit mutating endpoints.

**Fix:**

```js
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    "&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"
  }[c]));
}
row.innerHTML = `<td>${escapeHtml(server.name)}</td>`;
```

> **Insight:** Defense-in-depth even on trusted localhost. `server.name` comes from client config (Claude/Codex), "trusted". But that trust can erode. Escape on boundary — paranoia that scales. `textContent` is the safer default for pure text cells; `innerHTML` + escape when nested elements are needed. Codex also flagged it in failed-migration report text (R9) — anywhere API error text could contain markup.

---

## II. Concurrency races (7 findings)

### R3 — Lock must come BEFORE bind, not after

**Bug:** pre-bind listener → close → acquire lock pattern. But `mcphub gui --port 9100` (fixed port) — if the incumbent already holds 9100, `net.Listen` fails with "address already in use" BEFORE reaching the handshake path.

**Fix:** acquire lock first, bind second. Lock busy → handshake → activate incumbent. The pidport initially records the requested port (may be 0); after `Server.Start` signals ready, rewrite it with the resolved port via `RewritePidportPort`.

---

### R4 — Handshake must re-read pidport on each retry

**Bug:** the R3 fix introduced `RewritePidportPort` — the incumbent writes pidport with `port=0` BEFORE bind, then rewrites with the resolved port after bind. A second instance launched in that window reads `port=0` ONCE before the retry loop and hammers `127.0.0.1:0` for the full timeout. Fails.

**Fix:** `TryActivateIncumbent` re-reads pidport **inside** the retry loop. `port == 0` → short-circuit + sleep + retry.

---

### R7 / R8 / R9 — Release pidport race (three iterations)

A saga:

**R7 (first fix):** unlock then remove.

```go
_ = l.fl.Unlock()
_ = os.Remove(l.pidport)
```

Between `Unlock` and `Remove`, a successor may acquire the lock and write its pidport → our `Remove` deletes **the successor's file**.

**R8 (second attempt):** ownership check.

```go
_ = l.fl.Unlock()
if pid, _, err := ReadPidport(l.pidport); err == nil && pid == os.Getpid() {
    _ = os.Remove(l.pidport)
}
```

Still races — between `ReadPidport` and `Remove` the successor can overwrite.

**R9 (final fix):** don't delete pidport at all.

```go
_ = l.fl.Unlock()
l.fl = nil
// pidport lingers — next acquirer overwrites via os.WriteFile
```

> **Insight:** Source of truth vs. metadata. The flock is the single source of truth for ownership. The pidport file is metadata for handshake convenience. Trying to manually synchronize both via cleanup always leaves a race window. Lesson: if primitive A owns an invariant, primitive B should not attempt cleanup — leave stale B and let the next acquirer overwrite via atomic `os.WriteFile` (atomic on POSIX/Windows through rename semantics). Stale pidport with no flock-holder is harmless: port probe returns connection-refused → "incumbent unreachable" is the correct outcome.
>
> Three attempts R7 → R8 → R9 illustrate classic Codex review value: each "fix" created a new race until the final redesign removed the need for cleanup.

---

### R15 — Broadcaster Publish-vs-Unsubscribe race

`Broadcaster.Publish` originally snapshotted subscribers under lock, released, then sent. If Subscribe's ctx-cancel goroutine fired between snapshot and send: `delete(subs, ch)` + `close(ch)` → send-on-closed-channel panic.

**Fix:** hold the mutex through the non-blocking fan-out.

```go
func (b *Broadcaster) Publish(ev Event) {
    b.mu.Lock()
    defer b.mu.Unlock()
    for c := range b.subs {
        select { case c <- ev: default: }  // non-blocking, cannot deadlock
    }
}
```

> **Insight:** Non-blocking send under mutex is a safe anti-pattern. "Holding a lock through I/O" is usually bad (deadlock risk), but `select { case c <- ev: default: }` completes in O(1) — never blocks. Here the mutex is load-bearing: it serializes `close(ch)` (which also runs under the same lock in Subscribe's ctx-done goroutine) with sends. Alternatives (RWMutex, copy-on-write subs, atomic pointer) are more complex and give no measurable benefit for local-loopback SSE.

---

## III. Multi-daemon cascades (6 findings)

Especially instructive group: **one architectural gap manifested in six places in a row.**

Background: `api.Status()` returns one row **per daemon**. Serena has `claude` + `codex` daemons. Every consumer must be aware.

### R4 — Logs adapter hardcoded `daemon="default"`

→ Serena logs empty (no `<server>-default.log` file).
**Fix:** widen `logsProvider` interface to `Logs(server, daemon, tail)`; route: `/api/logs/:server?daemon=X`.

### R5 — Poller cache key collision

```go
// Before
p.last[r.Server] = r  // claude → serena, codex overwrites → delta reported every tick
```

**Fix:** composite key `server/daemon`.

### R5 — Dashboard `state[r.server]` collision

Same bug on the UI side — only one card for Serena instead of two.
**Fix:** composite key + per-daemon card with "(daemon)" suffix.

### R6 — Servers matrix aggregation lost daemons

```js
Object.fromEntries((status||[]).map(s => [s.server, s]))  // last-wins collapse
```

**Fix:** group by server, compute aggregate state ("Running"/"Partial"/…).

### R18 — Aggregate mixed-failed → single state

The R6 fix had:

```js
if (allStopped) aggregate = states[0]  // picks ONE of ["Failed", "Stopped"]
```

**Fix:** if `unique > 1`, always "Partial".

### R19 — Workspace-scoped discriminator too broad

`isWorkspaceScoped` matcher `^mcp-local-hub-lsp-[0-9a-f]{8}-[^/]+$` would catch a hypothetical global server `lsp-<8hex>-foo` (task name `mcp-local-hub-lsp-<8hex>-foo-default`) as workspace-scoped → logs hidden.

**Fix:** add `DaemonStatus.IsWorkspaceScoped bool` server-side, populated from the canonical `IsLazyProxyTaskName()`. JS reads the flag.

> **Insight:** Server-side flags vs. client-side regex is a critical pattern. When JS tries to classify data (workspace vs. global, maintenance vs. daemon), it MUST mirror logic living in Go. While both implementations exist, they drift (JS tightens, Go doesn't know; Go tightens, JS regex staled). **Single source of truth:** Go parser owns the discriminator → exports a boolean field → JS reads the flag.
>
> We applied this pattern twice (R19: `IsWorkspaceScoped`, R20: `IsMaintenance`). After that all "classification" findings closed — structural fix instead of point patches.

---

### R20 — Maintenance rows pollute all screens

`/api/status` includes `mcp-local-hub-workspace-weekly-refresh` (shared scheduler maintenance task). Three separate bugs:

1. **Logs picker:** empty `server` → 404.
2. **Dashboard:** blank-name card with a Restart button → `/api/servers//restart` invalid.
3. **Servers matrix (R23):** maintenance state mixes in the aggregate → false "Partial".

**Fix:** `DaemonStatus.IsMaintenance` populated in `enrichStatusWithRegistry` from `parseTaskName` (normalizes all three weekly-refresh variants to `daemon == "weekly-refresh"`). All three consumers filter on the flag.

---

## IV. Error envelope handling (3 findings)

Spec: non-2xx `/api/*` response → `{error, code}` JSON envelope. Client code assumed an array.

### R10 — Dashboard bootstrap crash

```js
fetch("/api/status").then(r => r.json()).then(rows => {
  (rows || []).forEach(r => state[r.server] = r);  // throws if rows is {error,code}
});
```

**Fix (guard pattern):**

```js
const r = await fetch("/api/status");
const data = await r.json().catch(() => null);
if (!r.ok || !Array.isArray(data)) {
  cardsEl.innerHTML = `<p class="error">Failed: ${escapeHtml(data?.error ?? r.statusText)}</p>`;
  return;
}
data.forEach(...)
```

### R10 + R11 — Same pattern in Logs + Servers

Applied identical guard to both.

### R10 — Object truthy trick

```js
(rows || []).forEach(...)
```

If `rows = {error:"..."}`, `rows || []` **returns `rows`** (object is truthy!), then `.forEach` throws `TypeError` because objects don't have `.forEach`.

> **Insight:** `value || []` idiom breaks on non-null object truthiness. Correct guard pattern:
>
> ```js
> if (!Array.isArray(value)) { /* error path */ }
> ```
>
> or at destructure level: `const {entries = []} = data ?? {}`.
>
> This is a class of "assumed shape" bugs — typical for a TypeScript-less codebase. JSDoc or a runtime schema validator (ajv/zod) catches this automatically.

---

## V. State management / dirty tracking (5 findings)

### R1 — Per-client scope collapse

UI checkboxes are per-cell (server × client), but Apply sent only `{servers}`. Backend rewrote ALL bindings for those servers — flipping one checkbox migrated all four clients.

**Fix (round 1):** added `clients` in the request body + `MigrateOpts.ClientsInclude`.

### R2 — Per-server client scope leak

The R1 fix unioned clients across all servers → `(A,claude) + (B,gemini)` sent `servers:[A,B], clients:[claude,gemini]` → backend also rewrote `(A,gemini)` and `(B,claude)`.

**Fix (round 2):** loop per server-group client, multiple POSTs, each with `{servers:[oneServer], clients:[...only its dirty clients]}`.

### R1 — Dirty-tracking flip-flop

```js
// on change:
if (checked !== defaultChecked) toMigrate.add(server);
else toMigrate.delete(server);  // <-- bug
```

Flip A (dirty), flip B back (user thinks "undo B") → for B: `checked === defaultChecked` → `delete(server)` → server falls out of dirty set despite A's pending change.

**Fix:** per-cell dirty map `Map<server, Set<client>>`. Server stays dirty iff any cell is dirty.

### R17 — Unchecking via-hub silently no-ops

`api.MigrateFrom` is one-way (stdio → hub). Unchecking a via-hub cell + Apply returns 204 but does NOTHING → cell is checked again after refresh. Misleading.

**Fix (MVP):** disable via-hub cells in the UI with a tooltip directing users to `mcphub rollback`. Full reverse-migrate → Phase 3B-II.

### R22 — Undefined routing defaults to enabled checkbox

Clients absent from `/api/scan`'s `client_presence` map → `server.routing[client]` undefined → `renderCell` falls through → enabled checkbox → Apply silently skips (adapter.Exists() = false).

**Fix:** `const routing = server.routing[client] ?? "not-installed"`. Existing disable logic handles it.

> **Insight:** UI state ≠ server state. Three findings (R1, R2, R22) are manifestations of one class of bug: **UI computes a diff from "default state" and sends the diff, but backend semantics differ**. Mitigations:
>
> 1. **Explicit intent serialization:** send `"set_hub=true"` per cell, not implicit `toMigrate.add(server)`.
> 2. **Return server state after apply:** `/api/migrate` returns updated `ScanResult` → UI re-renders from truth, not from optimistic model.
> 3. **Reverse operations:** every `DoFoo` action should have a `UndoFoo`. A checkbox UI makes no sense without a reverse.
>
> MVP went pragmatic (#3 via disable), but the proper architecture is option 2 (return-state-after-apply).

---

## VI. Log streaming (7 findings)

### R3 — `lastLen = 0` replays the whole log

Subscribing to `/api/logs/:server/stream` emitted the entire existing log as new `log-line` events (the snapshot was already rendered via `/api/logs/:server` static load).

**Fix:** prime `lastLen = len(initial body)` before the tick loop.

### R3 — Log rotation freezes emission

Gate `len(body) > lastLen` breaks when the log rotates (truncated → smaller). Emission freezes forever.

**Fix:** `if len(body) < lastLen: lastLen = len(body); continue`.

### R12 — Stream handler missed method check

`/stream` dispatched BEFORE the method check → `POST /api/logs/X/stream` became a long-lived SSE response.

**Fix:** method gate at the top of `streamLogs`.

### R12 — lastLen primed from placeholder

`api.LogsGet` returns `"(no log output yet — …)"` when the file does not exist. Primer seeded `lastLen` to placeholder length (~30 chars). When the real log appeared, the first 30 bytes were skipped.

**Fix:** exact-match placeholder check; skip prime when matched.

### R14 — Same bug in the tick loop

The R12 fix was only in prime, not in the per-tick loop. If the user enables Follow **before** the daemon creates a log file, every tick emits the placeholder as a `log-line`.

**Fix:** move the placeholder check inside the tick loop.

### R21 — Prefix match → false positives

`isLogPlaceholder = strings.HasPrefix(body, "(no log output yet")`. Real log content starting with that phrase is silently dropped.

**Fix:** exact match against full placeholder format:

```go
// api.LogPlaceholderFor(server, daemon) returns the exact expected string
func isLogPlaceholder(body, server, daemon string) bool {
    return body == api.LogPlaceholderFor(server, daemon)
}
```

### R22 — Partial log lines split

Daemon writes `"Loading..."` (no `\n`) + `"Loading... done\n"` (continuation). Current code splits the suffix on `\n` → emits `"Loading..."` as a complete event + `"Loading... done"` as a second. UI shows corrupted boundaries.

**Fix:** carry buffer `pendingLine` across ticks:

```go
suffix := pendingLine + body[lastLen:]
pendingLine = ""
lines := strings.Split(suffix, "\n")
if last line != "" { pendingLine = last line; drop from lines }
```

> **Insight:** Tail-follow log streaming is deceptively hard. Naive versions (poll + diff + emit) hide five concerns:
>
> 1. **Snapshot vs. stream boundary:** don't emit the snapshot as live events.
> 2. **File rotation:** size decrease → don't freeze.
> 3. **Placeholder vs. real content:** sentinel values need exact match.
> 4. **Partial line assembly:** writes can split across ticks.
> 5. **Method enforcement:** streaming endpoints still need method gates.
>
> If you build a log-tail API — read how GNU `tail -f` works (inotify, follow file descriptor by inode). Our poll-based approach is acceptable trade-off for a local GUI, but a production log service should use a filesystem watcher.

---

## VII. Input validation / contracts (3 findings)

### R13 — `validNameRe` excluded dots

The R11 regex `^[A-Za-z0-9_-]+$` rejected `paper-search-mcp.io`-style names. `api.validManifestName` allows them.

**Fix:** align charset + explicit `..` block.

### R16 — URL substring match for loopback

```js
endpoint.includes("127.0.0.1")  // false positive: "https://127.0.0.1.evil.com/"
```

**Fix:**

```js
new URL(endpoint).hostname === "127.0.0.1"  // exact hostname match
```

### R18 — Stale `--force` suggestion

The error message recommended `--force` after I had hidden it and declared it a Phase 3B-II placeholder. Dead-end guidance.

**Fix:** rephrase the error to actionable recovery: "remove `<pidport>.lock` and retry".

> **Insight:** Error messages are documentation in failure mode. The user reads them in a stressed state (crash). Two anti-patterns caught:
>
> 1. **Silently wrong answer** (R16 substring match) — code does what was asked, but **not what was intended**.
> 2. **Dead-end guidance** (R18 `--force` tip) — user follows the instruction, and it doesn't work.
>
> Generalizable rule: **never write "use X to recover" without actively verifying X works**. Error message text should have its own smoke test.

---

## VIII. Resource / lifecycle (3 findings)

### R1 — Dashboard SSE leak

```js
const observer = new MutationObserver(() => {
  if (!document.body.contains(root)) es.close();
});
```

`#screen-root` is REUSED across screen swaps — only `innerHTML` is cleared, the element stays in the DOM tree. The observer never fires. Every Dashboard visit leaks a new `EventSource`.

**Fix (architectural):** router-level cleanup registry.

```js
window.mcphub = { screens: {}, _cleanups: [] };
window.mcphub.registerCleanup = fn => _cleanups.push(fn);

function render() {
  while (_cleanups.length) _cleanups.pop()();  // pre-render drain
  // ...
}
// in dashboard.js:
window.mcphub.registerCleanup(() => es.close());
```

### R2 — Async `<script>` race

Injected `<script>` tags (dynamic loader) load asynchronously. `DOMContentLoaded` fires BEFORE they execute → `window.mcphub.screens.*` is empty → "Unknown screen" on cold load.

**Fix:** static `<script>` tags in `index.html`, guaranteed synchronous in-order execution.

> **Insight:** Browser lifecycle hooks are tricky:
>
> - **`MutationObserver` on removal** does NOT fire if the element is only `innerHTML`-cleared (not removed from the DOM tree). Watch for actual attach/detach.
> - **`DOMContentLoaded` vs. async scripts** — async `<script src>` injection does not block `DOMContentLoaded`. Guarantee execution order with static tags in the right sequence.
> - **`defer` attribute** is another tool: loads in parallel but executes in order after parsing, before `DOMContentLoaded`. Works even for injected tags if the attribute is present.
>
> These guarantees are part of the HTML spec; understanding them saves debugging.

---

## IX. Summary — distribution by category

| Category | Count | Rounds |
|---|---:|---|
| Multi-daemon cascades | 6 | R4, R5×2, R6, R18, R19 |
| Log streaming | 7 | R3×2, R12×2, R14, R21, R22 |
| State management | 5 | R1×2, R2, R17, R22 |
| Concurrency races | 7 | R3, R4, R7, R8, R9, R11, R15 |
| Security | 4 | R4, R9, R11, R15 |
| Error envelope | 3 | R10×2, R11 |
| Input validation | 3 | R13, R16, R18 |
| Resource lifecycle | 3 | R1, R2, R12 |
| Maintenance rows | 3 | R20×2, R23 |
| Placeholder handling | 4 | R12, R14, R21, R22 |

---

## X. Meta-observations

**Review trajectory:** The first five rounds surfaced structural bugs (security, per-client scope, multi-daemon cache). Rounds 6–15 were cascading fixes (one architectural gap manifesting in 3–5 places). Rounds 15+ surfaced edge cases (placeholder matching, partial lines, undefined routing defaults). This is a **normal convergence curve** for a GUI MVP: early rounds catch broad-surface bugs, late rounds catch narrow edge cases.

**Key takeaways for future GUI code:**

1. **Server-side flags > client-side regex.** When JS classifies data originating from Go, mirror the classification via boolean JSON fields populated by canonical Go predicates. Eliminates drift.
2. **Composite keys for multi-instance entities.** Any entity with multiple daemons / workspaces / contexts must be keyed by the full tuple, not a single axis.
3. **Same-origin guard on mutating routes, even loopback.** 127.0.0.1 binding does not prevent cross-origin POSTs; `Sec-Fetch-Site` + `Origin` allowlist is the answer.
4. **Allowlist validation on any input → filesystem/command.** Regex `[safe-chars]+` + explicit blacklist of `..`. Both, not either.
5. **`escapeHtml` on boundary, even trusted input.** Defense-in-depth that costs nothing.
6. **Resource lifecycle hooks via explicit registry, not DOM observers.** `MutationObserver` on elements that are reused (only `innerHTML`-cleared) never fires.
7. **Log streaming has 5 orthogonal concerns.** Snapshot boundary, rotation, placeholder sentinel, partial lines, method enforcement — address each.
8. **Source of truth vs. metadata.** If primitive A owns an invariant, primitive B should not attempt cleanup. Stale B is usually OK if next-acquirer overwrites atomically.
9. **Error messages are documentation.** Never write "use X to recover" without verifying X works. Silent-wrong-answer is worse than obvious-failure.

**Convergence quality:** zero regressions merged across 24 rounds (each fix landed cleanly). One false-alarm resolution chain (R7 unlock-first → R8 ownership check → R9 don't-delete-at-all) — Codex found each intermediate state. Reflects TDD-first strategy across 22 plan tasks + per-fix regression tests.

**Final verdict:** R24 on commit `761d3a3` — "Didn't find any major issues. 👍"

# Phase 3B-II B1 — Servers matrix uncheck-to-demigrate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Design memo:** `docs/superpowers/specs/2026-04-24-phase-3b-ii-b1-servers-matrix-demigrate-design.md` (commit `48a2c21`, rev 13 after 12 Codex rounds / 24 findings). Read §4 (decisions D1–D10) and §5 (risks R1–R4) before implementing; the memo is the source of truth for every semantic choice and corner case.

**Goal:** Wire the existing `api.Demigrate` / `/api/demigrate` chain into the Servers matrix checkbox so users can uncheck a `via-hub` cell and click Apply to roll back that single (server, client) binding to direct-client config — plus three small adjacent doc/test cleanups bundled from the A2b follow-up list.

**Architecture:** Dirty state refactored from `Map<string, Set<string>>` to `Map<string, Map<string, Direction>>` so `applyChanges` can branch per direction. Apply runs in two phases (demigrate before migrate) with a per-client failure gate that prevents polluted-backup retries. Partial-failure handling uses 3-outcome tracking (`succeeded | failed | gated`): only `succeeded` entries are pruned from dirty; `failed` and `gated` entries are retained for retry. Every Apply completion unconditionally reloads `/api/scan` + `/api/status` so the Checkbox `initialChecked` baseline stays honest.

**Tech Stack:** Preact 10 + TypeScript 5 (Vite 8), Vitest 4 unit tests, Playwright + headless Chromium E2E, Go 1.26 (GUI handler tests only — backend `api.Demigrate` is already merged).

---

## File Structure

```
internal/api/
├── api.go                         (MODIFY — package + type comments: lifecycle + CLI-parity corrections)

internal/gui/
├── manifest_test.go               (MODIFY — append 3 /api/manifest/edit handler coverage tests)

internal/gui/frontend/src/screens/
├── Servers.tsx                    (MODIFY — DirtyMap shape change, toggleCell direction capture, applyChanges 2-phase + gate + outcome tracking + always-reload, enable via-hub checkbox, new tooltip copy)

internal/gui/e2e/tests/
├── servers.spec.ts                (MODIFY — append 5 scenarios)

internal/gui/assets/                (REGEN — go generate ./internal/gui/...)

work-items/bugs/
├── a2b-combined-pr-followups.md   (MODIFY — move item #2 to FIXED/verified)
├── b1-backup-file-race.md         (CREATE — durable tracking for residual R1)

CLAUDE.md                          (MODIFY — E2E coverage section: 43 committed → 47 at HEAD → 52 after B1)

docs/superpowers/specs/             (already committed at 48a2c21)
```

---

## Type definitions (referenced across Task 4 and Task 5)

These go into `Servers.tsx` alongside the existing `DirtyMap` type. Defined here for cross-task reference; Task 4 introduces them.

```ts
// Direction a cell is flipping: "migrate" when user checks a previously-direct
// cell (queues POST /api/migrate), "demigrate" when user unchecks a via-hub
// cell (queues POST /api/demigrate). Captured at toggle time because the
// cell's initialChecked (scan state) is the only honest source of direction.
type Direction = "migrate" | "demigrate";

// Per-cell dirty tracking with direction preserved. Outer key: server name.
// Inner map: client → Direction. Both layers are pruned on toggle-back so
// `dirty.size === 0` remains a correct "nothing pending" predicate without
// a deep-empty scan.
type DirtyMap = Map<string, Map<string, Direction>>;

// Per-entry outcome from one applyChanges run. Drives the success-prune /
// retain-failed-or-gated semantic in §4 D6 of the design memo:
//   - "succeeded"  : POST fired, got 2xx → prune from dirty
//   - "failed"     : POST fired, got non-2xx → retain in dirty (user retries)
//   - "gated"      : POST never fired because §4 D4's per-client gate
//                    removed the client after a phase-1 demigrate on the
//                    same client failed → retain in dirty (user retries;
//                    entry will fire once the blocking demigrate succeeds)
type Outcome = "succeeded" | "failed" | "gated";
type OutcomeMap = Map<string, Map<string, Outcome>>;
```

---

## Task 1: Doc-only prep — close a2b-combined-pr-followups item #2 + correct internal/api/api.go comments

**Files:**
- Modify: `work-items/bugs/a2b-combined-pr-followups.md`
- Modify: `internal/api/api.go:1-26`

This is a doc-only commit. No behavior change. Two files edited together because both make the same claim (`api.NewAPI()` per-process lifecycle + CLI≡UI parity "by construction") — the followup memo closure is incomplete without also fixing the source comment that contradicts the code.

### Step 1 — Read the current state of `work-items/bugs/a2b-combined-pr-followups.md`

- [ ] **Read the memo** and find section `## 2.` (the `api.NewAPI()` per-request adapters entry that is still in the OPEN list).

Run:

```bash
grep -n "## 2\." work-items/bugs/a2b-combined-pr-followups.md
```

Expected: one line with the section header around line 41.

### Step 2 — Rewrite item #2 as FIXED/verified

- [ ] **Edit `work-items/bugs/a2b-combined-pr-followups.md`** — replace the existing `## 2. ...` section body entirely with the FIXED-section form. The existing body describes the concern; replace it with the verified resolution.

Find and replace:

```markdown
## 2. `api.NewAPI()` called per-request in `realManifest*` adapters
```

…with the new heading + verified body below (preserving the section ordering; if FIXED items live in a different group in this file, move section #2 into that group). Note: the block uses a **4-backtick outer fence** because the body nests a 3-backtick Go code block — a 3-backtick outer fence would break rendering and prevent clean copy-paste.

````markdown
## 2. ~~`api.NewAPI()` called per-request in `realManifest*` adapters~~ — FIXED (verified, B1)

**Originally flagged:** Task 4 code-quality review (confidence 85), concern that `newEventBus()` might spawn a goroutine making per-request `api.NewAPI()` a leak.

**Resolution:** verified against [internal/api/events.go](../../internal/api/events.go):

```go
type EventBus struct{}
func newEventBus() *EventBus { return &EventBus{} }
```

`EventBus` is an empty struct. `newEventBus()` has no goroutine, no channel, no background worker. `api.NewAPI()` still allocates `&State{Daemons: make(map[string]DaemonStatus)}` and struct headers, so it is not *literally* zero-cost — but it spawns no background resources that would leak across requests. Per-request construction is safe.

**Doc fix bundled alongside B1** (`internal/api/api.go` comments revised to match — the old "A single instance is created per process" claim and the "CLI ≡ UI parity by construction" claim both contradicted current state; updated to describe per-request GUI adapter construction and softened the CLI-parity language).

**Shared-`*api.API` refactor deferred** until `EventBus` is actually populated (the source comment in `events.go` says "Populated in Task 22"). When that work lands, the same task restates the per-process-instance contract and threads a shared handle through all adapters.
````

### Step 3 — Read the current state of `internal/api/api.go`

- [ ] **Read the source comment** at `internal/api/api.go:1-26`:

```bash
sed -n '1,26p' internal/api/api.go
```

Expected: the package comment promising CLI ≡ UI parity "by construction" plus the type comment saying "A single instance is created per process".

### Step 4 — Replace the package + type comments with accurate text

- [ ] **Edit `internal/api/api.go`** — replace lines 1–26 (the package doc + `API` type doc + `NewAPI` constructor) with:

```go
// Package api is the single source of truth for operations exposed through
// the mcp-local-hub CLI and GUI frontends. Every command the user runs (via
// cobra) or every HTTP endpoint the GUI calls dispatches into one function
// here; they never reach directly into internal/clients, internal/scheduler,
// internal/config, or internal/secrets.
//
// This keeps CLI and GUI from skipping layers: capabilities live in api so
// both frontends can reach them without bypassing validation, backup, or
// audit logic. NOTE: not every api function has a CLI command today —
// api.Demigrate is wired into the GUI (/api/demigrate) but has no mcphub
// subcommand; adding one is a separate follow-up.
package api

// API is the orchestration handle held by cli and gui. Methods are safe for
// concurrent use unless noted otherwise.
//
// Lifecycle: the CLI layer creates one API per process via NewAPI. The GUI
// layer currently constructs a fresh API inside every real adapter (see
// internal/gui/server.go's realManifestCreator / realManifestGetter /
// realManifestEditor / realManifestValidator / realDemigrater). This is
// safe today because newEventBus (internal/api/events.go) returns an empty
// struct — no goroutine, no background resource. When EventBus is populated
// (Task 22 per events.go's source comment), the GUI adapters should be
// refactored to share one API handle via the Server struct.
type API struct {
	state *State
	bus   *EventBus
}

// NewAPI constructs a fresh API with an initialized state and event bus.
// Cheap: allocates the State struct + daemon map + EventBus struct header,
// no background resources. See the API type doc for the per-process vs
// per-request lifecycle caveat.
func NewAPI() *API {
	return &API{
		state: &State{Daemons: make(map[string]DaemonStatus)},
		bus:   newEventBus(),
	}
}
```

### Step 5 — Verify Go build still passes

- [ ] **Run build + vet** — comment-only change, but sanity-check:

```bash
go build ./... && go vet ./internal/api/
```

Expected: no output on either command.

### Step 6 — Commit

- [ ] **Commit Task 1:**

```bash
git add work-items/bugs/a2b-combined-pr-followups.md internal/api/api.go
git commit -m "docs(api): close a2b-combined-pr-followups item #2 + correct api.go lifecycle+parity comments (B1 prep)

item #2 (api.NewAPI per-request adapters potential goroutine leak)
moved to FIXED/verified section. Verified internal/api/events.go
EventBus is an empty struct with no goroutine / channel / worker;
per-request construction is safe. Shared-*api.API refactor deferred
until EventBus is populated (events.go source comment: 'Populated in
Task 22').

internal/api/api.go package + type comments revised to match code.
Previous text promised 'A single instance is created per process'
(violated by GUI per-request adapters) and 'CLI ≡ UI parity by
construction' (violated by api.Demigrate having no mcphub CLI
command). New text describes the per-request GUI pattern with a
forward-reference to the Task 22 refactor window, softens the parity
language to 'capabilities live in api so both frontends can reach
them' without promising existing CLI command coverage, and notes
the Demigrate CLI gap explicitly.

No behavior change; doc-only."
```

---

## Task 2: Create `work-items/bugs/b1-backup-file-race.md`

**Files:**
- Create: `work-items/bugs/b1-backup-file-race.md`

Doc-only commit to make the design memo's §5 R1 residual-race acknowledgement durable. No behavior change.

### Step 1 — Create the bug file

- [ ] **Write `work-items/bugs/b1-backup-file-race.md`:**

```markdown
# B1 residual race: backup + live-client-config I/O in internal/clients lacks serialization

**Status:** open (accepted residual for B1 — tracked for future fix)
**Found during:** B1 design-memo rev 2 / R1 P2 review of 2026-04-24
**Phase:** post-B1

## Summary

`internal/clients/clients.go` backup-write (`writeBackup`), backup-read
(`latestBackup`), restore, and live client-config read-modify-write paths
all use unguarded `os.ReadFile` / `os.WriteFile` with no `sync.Mutex`,
no `flock`, and no process-level lock.

Concurrent mutations on the same client config can therefore produce:

- **Silent lost updates** — writer B clobbers writer A's pending rotation
  before A's read-modify-write completes.
- **Interleaved backup files** — one writer's JSON overwrites another's
  in the same timestamp window.
- **Demigrate reading a polluted backup** — see B1 design memo §5 R1 for
  the specific multi-tab / CLI+GUI race pattern.

`latestBackup` does NOT validate JSON — it picks a path based on
lexicographic sort of timestamp filenames. So failures are not always
visible parse errors; they can be silent lost updates that surface much
later as stale state in a client config or a successful-but-wrong
restore.

## Exposure at B1 ship time

- **Single GUI tab**: the frontend's sequential per-change apply loop
  keeps requests serialized from the browser side. Within one Apply
  invocation from one tab, no two requests to the same client overlap.
- **Single GUI process**: the `<pidport>.lock` single-instance mutex
  prevents a second `mcphub gui` process from running concurrently.
- **Multiple GUI tabs**: two tabs of the same GUI process CAN issue
  interleaved `POST /api/migrate` and `POST /api/demigrate` to the
  same Go process. The HTTP mux does not serialize same-client
  requests. **This exists pre-B1** for two tabs clicking the Migration
  screen's Demigrate button simultaneously; B1 widens the exposure
  (matrix Apply on one tab + Migration Demigrate on another) but
  introduces no NEW primitive.
- **CLI + GUI interleaving**: `mcphub migrate ...` CLI running while
  the user clicks Apply on the GUI matrix — same risk class.

## Fix candidates (for a future plan)

1. **In-process mutex keyed by client name** (`map[string]*sync.Mutex`
   wrapping backup + live config I/O in `internal/clients`). Covers
   multi-tab case. Does NOT cover CLI-plus-GUI unless the CLI also goes
   through the same in-process state, which it does not in a separate
   process.
2. **OS-level advisory file-lock** on each client config file (`flock`
   on Linux, `LockFileEx` on Windows). Covers CLI+GUI interleave.
   Requires careful platform handling (Windows Git Bash vs PowerShell
   vs cmd quirks).
3. **Both** — in-process mutex as fast-path within one process, file-lock
   as coarse inter-process guard.

## Tests to add when fix lands

- Unit: two goroutines calling `writeBackup` + `latestBackup` for the
  same client in tight loop; assert no lost updates.
- Integration: spawn a second `mcphub` subprocess that calls `mcphub
  migrate` while the GUI tab issues `POST /api/migrate` + `POST
  /api/demigrate` — assert final backup state matches one of the two
  serializations (never a torn JSON blob).

## Related

- Design memo: `docs/superpowers/specs/2026-04-24-phase-3b-ii-b1-servers-matrix-demigrate-design.md`
  §5 R1 (5 Codex revisions, 1 P1 order-of-operations + 1 P1 gate + 1 P1
  retry-prune + 1 P1 always-reload all in this race's orbit)
- A2b backlog follow-ups: `work-items/bugs/a2b-combined-pr-followups.md`
```

### Step 2 — Verify the file renders cleanly

- [ ] **Run:**

```bash
cat work-items/bugs/b1-backup-file-race.md | head -10
```

Expected: markdown file, no Windows line-ending weirdness, first line is the H1 header.

### Step 3 — Commit

- [ ] **Commit Task 2:**

```bash
git add work-items/bugs/b1-backup-file-race.md
git commit -m "docs(b1): track residual backup-file-race risk as durable bug entry

B1 design memo §5 R1 acknowledges a multi-tab / CLI+GUI interleaving
race in internal/clients backup + live-config I/O (unguarded os I/O,
no sync primitive). B1 ships without a fix because the proper fix
(in-process mutex keyed by client + OS advisory file-lock) is a
meaningful engineering effort with its own test matrix.

Filing the concern as a work-items/bugs entry so it survives beyond
the B1 PR, includes the actual failure modes (silent lost updates,
not just surfaced parse errors), and names the fix candidates with
their trade-offs.

No behavior change; doc-only."
```

---

## Task 3: Three `/api/manifest/edit` handler coverage tests

**Files:**
- Modify: `internal/gui/manifest_test.go` (append tests)

Closes a2b-combined-pr-followups item #3. No production-code change.

### Step 1 — Read the existing test patterns

- [ ] **Find the existing `/api/manifest/edit` handler tests** so the new ones match their style and shared fakes:

```bash
grep -n "^func TestManifestEditHandler_" internal/gui/manifest_test.go
```

Expected output: existing tests like `TestManifestEditHandler_ForwardsNameYAMLAndHash`, `TestManifestEditHandler_HashMismatch_Returns409`, `TestManifestEditHandler_OtherError_Returns500`. Note the existing helpers (`newManifestTestServerFull`, `postJSON`, `fakeManifestCreator`, `fakeManifestValidator`, `fakeManifestGetter`, `fakeManifestEditor`).

### Step 2 — Write the three failing tests

- [ ] **Append to `internal/gui/manifest_test.go`** (end of file, after the existing tests):

```go
// Codex R7 (a2b-combined-pr-followups item #3): three coverage gaps on
// /api/manifest/edit that parallel the already-covered get/create handlers.

func TestManifestEditHandler_EmptyName_400(t *testing.T) {
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, &fakeManifestEditor{})
	rec := postJSON(t, s, "/api/manifest/edit",
		`{"name":"","yaml":"name: demo","expected_hash":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "BAD_REQUEST") {
		t.Errorf("body=%q missing BAD_REQUEST code", rec.Body.String())
	}
}

func TestManifestEditHandler_MalformedJSON_400(t *testing.T) {
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, &fakeManifestEditor{})
	rec := postJSON(t, s, "/api/manifest/edit", `{not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "BAD_REQUEST") {
		t.Errorf("body=%q missing BAD_REQUEST code", rec.Body.String())
	}
}

func TestManifestEditHandler_RejectsNonPOST_405(t *testing.T) {
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, &fakeManifestEditor{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/edit", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow header = %q, want POST", got)
	}
}
```

### Step 3 — Run the new tests

- [ ] **Run in isolation** to confirm they all pass:

```bash
go test ./internal/gui/ -run 'TestManifestEditHandler_EmptyName_400|TestManifestEditHandler_MalformedJSON_400|TestManifestEditHandler_RejectsNonPOST_405' -count=1 -v
```

Expected: 3 PASS lines.

### Step 4 — Run the full `internal/gui` package

- [ ] **Confirm no regression** in neighboring handler tests:

```bash
go test ./internal/gui/ -count=1
```

Expected: `ok  mcp-local-hub/internal/gui  <time>`.

### Step 5 — Commit

- [ ] **Commit Task 3:**

```bash
git add internal/gui/manifest_test.go
git commit -m "test(gui): /api/manifest/edit coverage gaps — empty-name 400, malformed-JSON 400, non-POST 405

Closes a2b-combined-pr-followups item #3. These three cases mirror
the existing /api/manifest/get and /api/manifest/create coverage and
were flagged as missing during A2b PR #16 code-quality review.

No production-code change; purely adds tests against the existing
fake infrastructure (fakeManifestCreator, fakeManifestValidator,
fakeManifestGetter, fakeManifestEditor, newManifestTestServerFull,
postJSON)."
```

---

## Task 4: Refactor `Servers.tsx` DirtyMap shape + toggleCell direction capture

**Files:**
- Modify: `internal/gui/frontend/src/screens/Servers.tsx` — type declarations + `toggleCell` body

This is the load-bearing state-shape change. Isolated commit so diff is reviewable on its own; `applyChanges` rewiring (Task 5) depends on the new shape.

After this task: `dirty` is `Map<string, Map<string, Direction>>`. `applyChanges` is UNCHANGED (still migrate-only). Behavior stays A2a-compatible: `dirty.size === 0` still means "nothing pending"; toggle-back still prunes entries. The direction field is captured but not yet consumed.

### Step 1 — Edit the `DirtyMap` type declaration

- [ ] **Edit `internal/gui/frontend/src/screens/Servers.tsx:9-15`** — replace the current `DirtyMap` type + comment with:

```ts
// Per-cell dirty tracking with direction preserved. Outer key: server name.
// Inner map: client → Direction.
//
// Direction is captured at toggle time because the cell's initialChecked
// (scan state, authoritative) is the only honest source of truth for
// "which endpoint should Apply call for this cell" — by the time
// applyChanges runs, routing may have reloaded. Storing Direction in the
// dirty map keeps endpoint selection stable across reloads.
//
// Prune invariant (see B1 memo §4 D4): on toggle-back (user re-flips a
// dirty cell to its initial state), delete the client entry AND delete
// the server entry if the inner map becomes empty. With the invariant
// enforced at every update, `dirty.size === 0` remains a correct
// "nothing pending" predicate without a deep-empty scan.
type Direction = "migrate" | "demigrate";
type DirtyMap = Map<string, Map<string, Direction>>;
```

### Step 2 — Edit `toggleCell` to capture direction + enforce the prune invariant

- [ ] **Edit `internal/gui/frontend/src/screens/Servers.tsx` `toggleCell`** (lines 57–76) — replace the function body entirely with:

```ts
  function toggleCell(server: string, client: string, nextChecked: boolean, initialChecked: boolean) {
    setDirty((prev) => {
      const next = new Map(prev);
      if (nextChecked !== initialChecked) {
        // Dirty: capture direction from initialChecked (authoritative scan
        // state). A cell that started `via-hub` (initialChecked=true) and
        // is now unchecked flips to "demigrate"; a direct cell (false) that
        // just got checked flips to "migrate".
        const direction: Direction = initialChecked ? "demigrate" : "migrate";
        let clients = next.get(server);
        if (!clients) {
          clients = new Map();
          next.set(server, clients);
        }
        clients.set(client, direction);
      } else {
        // Toggle-back: enforce the prune invariant (see DirtyMap doc).
        const clients = next.get(server);
        if (clients) {
          clients.delete(client);
          if (clients.size === 0) next.delete(server);
        }
      }
      return next;
    });
  }
```

### Step 3 — Patch `applyChanges` so it compiles against the new DirtyMap shape

The existing `applyChanges` iterates `dirty.entries()` where each value was `Set<string>`. Now each value is `Map<string, Direction>`. Task 5 rewrites `applyChanges` entirely; for THIS task, do the minimal edit to keep it compiling and migrate-only:

- [ ] **Edit `internal/gui/frontend/src/screens/Servers.tsx` `applyChanges`** — replace the top of the function (lines 86–89) with:

```ts
    // Minimal shim: ignore direction for now (Task 5 introduces per-direction
    // branching). Collects every dirty cell into the existing migrate-only
    // POST loop. Task 4 is purely a state-shape refactor; behavior is
    // unchanged in this commit.
    const changes = Array.from(dirty.entries())
      .filter(([, clients]) => clients.size > 0)
      .map(([server, clients]) => ({ server, clients: Array.from(clients.keys()) }));
```

(Everything from `if (changes.length === 0) return;` onward stays exactly as today.)

### Step 4 — Typecheck

- [ ] **Run the frontend typecheck:**

```bash
cd internal/gui/frontend && npm run typecheck
```

Expected: exit 0, no output beyond the normal `tsc` header.

### Step 5 — Run the Vitest suite

- [ ] **Confirm no unit-test regression** — no tests directly use `DirtyMap`, but the suite runs every `*.test.ts`:

```bash
cd internal/gui/frontend && npm run test --run
```

Expected: 107 tests pass (same count as HEAD).

### Step 6 — Regenerate embedded assets

- [ ] **From `internal/gui/frontend/` (where the previous typecheck+Vitest steps ran), hop back to repo root and regenerate:**

```bash
cd ../../.. && go generate ./internal/gui/...
```

Expected: vite build output showing `internal/gui/assets/app.js` regenerated. (If you lost your working dir — just `cd <repo-root> && go generate ./internal/gui/...` from anywhere.)

### Step 7 — Verify existing A2a servers E2E still passes

- [ ] **Run the existing 3 servers scenarios:**

```bash
cd internal/gui/e2e && npm test -- tests/servers.spec.ts
```

Expected: 3 passed. (This task must not change A2a behavior — Apply still posts only `/api/migrate`; only the internal state shape is different.)

### Step 8 — Commit

- [ ] **Commit Task 4:**

```bash
git add internal/gui/frontend/src/screens/Servers.tsx internal/gui/assets/
git commit -m "refactor(gui/frontend): Servers DirtyMap shape — Map<server, Map<client, Direction>>

Prep commit for B1 matrix wiring. Changes the DirtyMap shape from
Map<string, Set<string>> to Map<string, Map<string, Direction>> so
applyChanges in the NEXT commit can branch per (server, direction)
batch to pick /api/migrate vs /api/demigrate.

Direction is captured at toggle time from initialChecked (authoritative
scan state, before any routing reload). toggleCell enforces the prune
invariant on toggle-back so dirty.size === 0 remains a correct
predicate without a deep-empty scan.

applyChanges is shimmed with a keys-only flattening so behavior stays
migrate-only in this commit — the direction field is captured but not
yet consumed. A2a create-flow E2E (3 servers scenarios) passes
unchanged; Vitest 107/107; typecheck clean. No user-visible change.

Next commit (Task 5) rewires applyChanges to a 2-phase demigrate-
before-migrate loop with per-client gate + 3-outcome tracking +
success-pruning + always-reload."
```

---

## Task 5: Servers matrix wiring — enable via-hub checkbox + direction-branching applyChanges

**Files:**
- Modify: `internal/gui/frontend/src/screens/Servers.tsx` — `applyChanges` body, `CellView` disabled+tooltip

This is the user-visible B1 change. Rewrites `applyChanges` to the §4 D4+D6 semantics (2-phase demigrate-before-migrate with per-client gate, 3-outcome tracking, success-pruning, always-reload). Enables the `via-hub` checkbox and updates tooltip copy.

### Step 0 — Remove the dirty-wipe from the reload effect (Codex plan-R1 P1 fix)

The existing reload effect at `Servers.tsx:47` calls `setDirty(new Map())` at the end of every successful `/api/scan` + `/api/status` fetch. That was correct for A2a migrate-only behavior (Apply's own success path already cleared dirty; the reload's redundant clear was idempotent). But Task 5's Apply ALWAYS bumps `reloadToken` — including on partial failure, where `applyChanges` has just carefully pruned successful entries and RETAINED failed + gated entries. If the reload effect then wipes everything, retained entries disappear before the user can retry, breaking §4 D6 R7/R10 and failing scenario 3 / scenario 5.

- [ ] **Edit `internal/gui/frontend/src/screens/Servers.tsx:47`** — inside the reload `useEffect`'s success branch, delete the line:

```ts
        setDirty(new Map());
```

After deletion, the reload effect no longer touches `dirty` on its own. Dirty mutation becomes owned by exactly two call sites: (1) `toggleCell` adds/removes on user toggle, (2) `applyChanges` prunes succeeded-outcome entries at the end of each Apply. On full success, all entries prune to empty; on partial failure, failed + gated entries are retained for retry.

### Step 1 — Add the `OutcomeMap` type above `ServersScreen`

- [ ] **Edit `internal/gui/frontend/src/screens/Servers.tsx`** — immediately after the `DirtyMap` type declaration (around line 15), add:

```ts
// Per-entry outcome from one applyChanges run. Drives the success-prune /
// retain-failed-or-gated semantic in B1 memo §4 D6:
//   - "succeeded"  : POST fired, got 2xx → prune from dirty
//   - "failed"     : POST fired, got non-2xx → retain (user retries)
//   - "gated"      : POST never fired because phase-1 demigrate on the
//                    same client failed; the §4 D4 per-client gate
//                    removed this client from the phase-2 migrate batch.
//                    Retain (user retries; entry will fire once the
//                    blocking demigrate succeeds).
type Outcome = "succeeded" | "failed" | "gated";
type OutcomeMap = Map<string, Map<string, Outcome>>;
```

### Step 2 — Rewrite `applyChanges` body

- [ ] **Edit `internal/gui/frontend/src/screens/Servers.tsx` `applyChanges`** — replace the entire function (from `async function applyChanges() {` through its closing brace) with:

```ts
  async function applyChanges() {
    if (dirty.size === 0) return;
    setApplying(true);
    setApplyMsg(`Applying…`);

    // Per-cell POST granularity (memo §4 D2). Each (server, client, direction)
    // cell fires its OWN /api/migrate or /api/demigrate POST with a single-
    // element clients array. Batching multiple clients into one POST would
    // be collapsed by the handlers into a single 500 on any row failure,
    // corrupting per-cell outcome tracking — a batch containing one failed
    // row and one succeeded row would mark BOTH failed, leaving the actually-
    // successful row dirty and replaying it on retry (which reads the now-
    // polluted backup and hits the R5 sentinel bug). Per-cell POSTs keep
    // outcome 1:1 with cell state. [Codex plan-R4 P1 on this plan.]
    type Cell = { server: string; client: string };
    const demigrateCells: Cell[] = [];
    const migrateCells: Cell[] = [];
    for (const [server, clientMap] of dirty.entries()) {
      for (const [client, direction] of clientMap.entries()) {
        if (direction === "demigrate") demigrateCells.push({ server, client });
        else migrateCells.push({ server, client });
      }
    }

    // Per-entry outcomes — seed every entry as "gated" (will upgrade to
    // "succeeded" or "failed" once its POST fires; gated only remains for
    // cells skipped by the phase-2 per-client gate).
    const outcomes: OutcomeMap = new Map();
    for (const [server, clientMap] of dirty.entries()) {
      const row: Map<string, Outcome> = new Map();
      for (const [client] of clientMap.entries()) row.set(client, "gated");
      outcomes.set(server, row);
    }

    const failed: string[] = [];
    // Clients whose phase-1 demigrate failed. Phase 2 skips every migrate
    // cell targeting such a client (per-client gate, §4 D4). Gated cells
    // stay "gated" in outcomes and retain in dirty for retry.
    const failedDemigrateClients = new Set<string>();

    // PHASE 1 — demigrate (one POST per cell).
    for (const cell of demigrateCells) {
      try {
        const resp = await fetch("/api/demigrate", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ servers: [cell.server], clients: [cell.client] }),
        });
        if (resp.ok || resp.status === 204) {
          outcomes.get(cell.server)!.set(cell.client, "succeeded");
        } else {
          const body = (await resp.json().catch(() => ({}))) as { error?: string };
          failed.push(`${cell.server}/demigrate/${cell.client}: ${body.error ?? resp.status}`);
          outcomes.get(cell.server)!.set(cell.client, "failed");
          failedDemigrateClients.add(cell.client);
        }
      } catch (e) {
        failed.push(`${cell.server}/demigrate/${cell.client}: ${(e as Error).message ?? "unknown"}`);
        outcomes.get(cell.server)!.set(cell.client, "failed");
        failedDemigrateClients.add(cell.client);
      }
    }

    // PHASE 2 — migrate (one POST per cell, with per-client gate).
    for (const cell of migrateCells) {
      if (failedDemigrateClients.has(cell.client)) {
        // Gated: a phase-1 demigrate on this client failed. Do NOT fire
        // the migrate — it would write a polluted post-migrate backup
        // that the user's retry of the failed demigrate would then
        // misread. Outcome stays "gated"; entry retains in dirty.
        continue;
      }
      try {
        const resp = await fetch("/api/migrate", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ servers: [cell.server], clients: [cell.client] }),
        });
        if (resp.ok || resp.status === 204) {
          outcomes.get(cell.server)!.set(cell.client, "succeeded");
        } else {
          const body = (await resp.json().catch(() => ({}))) as { error?: string };
          failed.push(`${cell.server}/migrate/${cell.client}: ${body.error ?? resp.status}`);
          outcomes.get(cell.server)!.set(cell.client, "failed");
        }
      } catch (e) {
        failed.push(`${cell.server}/migrate/${cell.client}: ${(e as Error).message ?? "unknown"}`);
        outcomes.get(cell.server)!.set(cell.client, "failed");
      }
    }

    // Prune "succeeded" outcomes from dirty; retain "failed" and "gated".
    // §4 D6 rationale: successful entries would silently replay on retry
    // and re-read the now-polluted latest backup (R5/R6/R7). Gated entries
    // represent unfulfilled user intent that must retry (R10).
    setDirty((prev) => {
      const next = new Map(prev);
      for (const [server, outcomeRow] of outcomes.entries()) {
        const clientMap = next.get(server);
        if (!clientMap) continue;
        for (const [client, outcome] of outcomeRow.entries()) {
          if (outcome === "succeeded") clientMap.delete(client);
        }
        if (clientMap.size === 0) next.delete(server);
      }
      return next;
    });

    // Always reload, regardless of failure count. §4 D6 rationale: the
    // Checkbox useEffect syncs local `checked` from `initialChecked`
    // derived from server.routing; without a reload, successful demigrate
    // cells stay with stale "via-hub" initialChecked and the next toggle
    // fires the wrong direction. Reloading unconditionally keeps every
    // cell's baseline honest. Failed cells retain their local-flipped
    // state via a no-op useEffect sync (their initialChecked is unchanged
    // because backend rejected the POST).
    setReloadToken((x) => x + 1);

    if (failed.length === 0) {
      setApplyMsg("Applied. Refreshing…");
    } else {
      setApplyMsg(`Failed: ${failed.join("; ")}`);
    }
    setApplying(false);
  }
```

### Step 3 — Enable the `via-hub` checkbox + update tooltip

- [ ] **Edit `internal/gui/frontend/src/screens/Servers.tsx` `CellView`** — replace lines 225–239 (the comment + `disabled` predicate + `title` assignment) with:

```ts
  // Disable when cell is meaningless:
  //  - "unsupported"   : this client cannot route this server via the hub
  //  - "not-installed" : this client is not installed on this machine
  // "via-hub" is now INTERACTIVE (B1): uncheck + Apply posts
  // /api/demigrate for this (server, client) pair. See B1 memo §4 D5.
  const disabled = routing === "unsupported" || routing === "not-installed";
  let title: string | undefined;
  if (routing === "via-hub") {
    title = `Currently routed through the hub. Uncheck and Apply to roll this binding back to the original ${client} config.`;
  } else if (routing === "not-installed") {
    title = `${client} is not installed on this machine.`;
  } else if (routing === "unsupported") {
    title = `${client} cannot route this server through the hub (e.g., per-session servers).`;
  }
```

### Step 4 — Typecheck

- [ ] **Run the frontend typecheck:**

```bash
cd internal/gui/frontend && npm run typecheck
```

Expected: exit 0.

### Step 5 — Vitest

- [ ] **Run Vitest** — no new unit tests added (E2E covers the wiring), but confirm existing suite unchanged:

```bash
cd internal/gui/frontend && npm run test --run
```

Expected: 107 passed.

### Step 6 — Regenerate embedded assets

- [ ] **From repo root:**

```bash
cd ../../.. && go generate ./internal/gui/...
```

### Step 7 — Run Go test suite as sanity

- [ ] **Full Go test** — B1 changes are frontend-only, Go suite should pass unchanged:

```bash
go test ./... -count=1
```

Expected: every package `ok` (no FAIL lines). Known-flake `internal/perftools/TestClangTidyTool_RejectsOversizedOutput` is SKIPPED on Windows per the A2b fix.

### Step 8 — Commit

- [ ] **Commit Task 5:**

```bash
git add internal/gui/frontend/src/screens/Servers.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): Servers matrix uncheck-to-demigrate wiring (B1)

Enables reverse-migration via the Servers matrix checkbox. Uncheck a
via-hub cell, click Apply, and the cell's (server, client) binding
rolls back to the original direct-client config via POST /api/demigrate.

Wiring per B1 design memo §4 decisions:

- via-hub checkbox is now INTERACTIVE (D5): removed from the disabled
  predicate. Tooltip copy replaced (the old text pointed at a
  non-existent 'mcphub rollback --client' flag).
- applyChanges is now a 2-phase loop (D4 order invariant): every
  demigrate POST fires before any migrate POST, because api.MigrateFrom
  writes a fresh latest backup and a demigrate running after migrate on
  the same client would read a polluted backup and fall back to the
  sentinel (falsely refusing on 'server added after sentinel').
- Per-client gate (D4): if a demigrate POST fails, every migrate POST
  in phase 2 strips that client from its clients array (or skips
  entirely if the filtered list becomes empty). Prevents poisoning
  a client's backup state for a later retry of the failed demigrate.
- 3-outcome tracking (D6, R10): each entry ends as 'succeeded',
  'failed', or 'gated'. Prune 'succeeded' from dirty (retry would
  silently read the polluted backup); retain 'failed' and 'gated'
  (user retries, gated entries fire once the blocking demigrate
  succeeds).
- Unconditional reload after Apply (D6, R8): previously the code
  reloaded only on failed.length === 0. Always reloading keeps the
  Checkbox initialChecked baseline honest so future toggles fire the
  correct direction.
- Error surface (D3): failed.push entries are '{server}/{direction}/
  {client}: {error}' — per-cell, 1:1 with cell state (plan-R4 P1:
  switched from batched-client POSTs to per-cell POSTs because the
  handler collapses partial failures into a single 500 and that would
  mark truly-succeeded rows as failed, defeating success-pruning).

No backend change (api.Demigrate, /api/demigrate handler, and the
Migration-screen per-row Demigrate button were already merged as
part of the A1/A2a pre-work). Only Servers.tsx + regenerated assets.

Residual multi-tab / CLI+GUI race on internal/clients backup I/O is
tracked in work-items/bugs/b1-backup-file-race.md — acknowledged,
not mitigated in B1."
```

---

## Task 6: Playwright E2E — 5 new scenarios in `servers.spec.ts`

**Files:**
- Modify: `internal/gui/e2e/tests/servers.spec.ts`

Five scenarios per B1 design memo §6.3. Order below matches the memo's numbering.

**Routing-classifier contract (see `internal/gui/frontend/src/lib/routing.ts:29-40` for canonical source):**

| `transport` in `client_presence` | `endpoint` | Classifies as |
|---|---|---|
| `"absent"` or missing | (any) | `not-installed` (checkbox disabled) |
| `"http"` | loopback URL (`127.0.0.1` / `localhost` / `[::1]`) | `via-hub` (checkbox checked + interactive after B1) |
| `"relay"` | (any) | `via-hub` |
| `"http"` | non-loopback URL | `direct` (checkbox unchecked + interactive) |
| `"stdio"` or anything else | (any) | `direct` |

**The E2E stubs below use `transport: "stdio"` for direct cells** because it's the canonical direct shape (stdio = client runs the MCP server as a subprocess, no HTTP involved). Do NOT use `transport: "http"` with a loopback endpoint for a "direct" fixture — that classifies as `via-hub` and the scenario will queue the wrong direction. [Codex plan-R1 P1 caught this in an earlier draft of this plan.]

### Step 1 — Read existing `servers.spec.ts` to learn fixture patterns

- [ ] **Open the file and note the imports, `seedScanFixture` helper if present, and the `hub` fixture shape:**

```bash
head -40 internal/gui/e2e/tests/servers.spec.ts
```

Expected: a `test.describe("Servers"...)` with 3 existing scenarios. Note whether the file uses a `seedScanFixture(hub, ...)` helper or inline `page.route()` stubs for `/api/scan` / `/api/status`. The new scenarios follow whichever pattern is already established. If the file uses inline `page.route()` stubs, scenarios below copy that; if it uses a shared helper, adapt the body to call the helper.

### Step 2 — Append scenario 1: load-demigrate happy path

- [ ] **Append to `servers.spec.ts`** (before the final closing `});` of the `test.describe` block, if wrapped, OR at the end of the file if scenarios are top-level):

```ts
  // B1 scenario 1: load path. Uncheck a via-hub cell, Apply, assert
  // /api/demigrate fires with {servers, clients} narrowed to that cell,
  // AND the cell reflects "direct" after the post-Apply reload (per
  // §6.3 scenario 1 in the design memo — the post-reload state
  // assertion is what proves success-pruning + always-reload actually
  // compose to the expected UI outcome).
  test("uncheck via-hub + Apply posts /api/demigrate narrowed to that cell + post-reload reflects direct", async ({ page, hub }) => {
    // Stateful /api/scan: returns via-hub on first call (initial mount),
    // returns direct ("stdio") after the demigrate flips the backend.
    // A non-hub transport is what routing.ts:29-40 classifies as "direct".
    let demigrateCompleted = false;
    const viaHubBody = {
      entries: [
        {
          name: "demo",
          client_presence: {
            "claude-code": { transport: "relay", endpoint: "" },
            "codex-cli":   { transport: "absent", endpoint: "" },
          },
        },
      ],
    };
    const directBody = {
      entries: [
        {
          name: "demo",
          client_presence: {
            "claude-code": { transport: "stdio", endpoint: "" },
            "codex-cli":   { transport: "absent", endpoint: "" },
          },
        },
      ],
    };
    // Count /api/scan calls to prove the post-Apply reload actually ran
    // (§4 D6: always-reload). Without this counter, scenario 1's
    // post-reload assertions could pass without any reload happening at
    // all — the local checkbox state + Apply-disabled + via-hub-enabled
    // would all hold after success-prune alone.
    let scanCallCount = 0;
    await page.route("**/api/scan", async (r) => {
      scanCallCount++;
      const body = demigrateCompleted ? directBody : viaHubBody;
      await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
    });
    await page.route("**/api/status", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));

    // Intercept /api/demigrate to capture the body + flip the scan state.
    let demigrateBody: string | null = null;
    await page.route("**/api/demigrate", async (r) => {
      demigrateBody = r.request().postData();
      demigrateCompleted = true;
      await r.fulfill({ status: 204, body: "" });
    });

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('table.servers-matrix');
    const initialScanCount = scanCallCount; // snapshot after mount reload

    // Uncheck the via-hub cell.
    const claudeCell = page.locator('table.servers-matrix tr').filter({ hasText: "demo" }).locator('input[type="checkbox"]').nth(0);
    await expect(claudeCell).toBeChecked(); // sanity: starts as via-hub
    // Title must match the new B1 tooltip copy, not the obsolete
    // "mcphub rollback --client" text.
    await expect(claudeCell).toHaveAttribute("title", /Uncheck and Apply/);
    await claudeCell.uncheck();
    await page.locator('#servers-toolbar button', { hasText: "Apply" }).click();

    // Assert the POST body shape.
    await expect.poll(() => demigrateBody).not.toBeNull();
    expect(JSON.parse(demigrateBody!)).toEqual({ servers: ["demo"], clients: ["claude-code"] });

    // The post-Apply /api/scan reload MUST have run — §4 D6 always-reload.
    // Without this counter check, the assertions below could all pass even
    // if no reload fired.
    await expect.poll(() => scanCallCount).toBeGreaterThan(initialScanCount);

    // Post-reload state: the claude-code cell now reflects direct (title
    // changes to the "unsupported/not-installed/direct" branch rather than
    // the via-hub branch). Asserting title disappears (or changes) proves
    // the reload updated the scan state — a pure local-flip without reload
    // would still show the via-hub title attribute.
    await expect(claudeCell).not.toHaveAttribute("title", /Uncheck and Apply/);
    await expect(claudeCell).not.toBeChecked();
    await expect(claudeCell).toBeEnabled();
    // Apply button is disabled again (dirty.size === 0 after success-prune).
    await expect(page.locator('#servers-toolbar button', { hasText: "Apply" })).toBeDisabled();
  });
```

### Step 3 — Append scenario 2: mixed-Apply demigrate-first ordering

- [ ] **Append:**

```ts
  // B1 scenario 2: mixed Apply must POST /api/demigrate BEFORE /api/migrate
  // across the whole Apply (§4 D4 order invariant). Otherwise the migrate
  // writes a fresh backup that the demigrate would then read as polluted.
  test("mixed Apply dispatches demigrate before migrate", async ({ page, hub }) => {
    const scanBody = {
      entries: [
        {
          name: "a",
          client_presence: {
            "claude-code": { transport: "stdio", endpoint: "" }, // direct stdio → queued migrate
          },
        },
        {
          name: "b",
          client_presence: {
            "claude-code": { transport: "relay", endpoint: "" }, // via-hub → queued demigrate
          },
        },
      ],
    };
    await page.route("**/api/scan", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(scanBody) }));
    await page.route("**/api/status", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));

    const log: { url: string; at: number }[] = [];
    await page.route("**/api/demigrate", async (r) => { log.push({ url: "demigrate", at: Date.now() }); await r.fulfill({ status: 204, body: "" }); });
    await page.route("**/api/migrate",   async (r) => { log.push({ url: "migrate",   at: Date.now() }); await r.fulfill({ status: 204, body: "" }); });

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('table.servers-matrix');

    // Check the direct cell (a,claude-code) and uncheck the via-hub cell (b,claude-code).
    await page.locator('table.servers-matrix tr').filter({ hasText: "a" }).locator('input[type="checkbox"]').first().check();
    await page.locator('table.servers-matrix tr').filter({ hasText: "b" }).locator('input[type="checkbox"]').first().uncheck();
    await page.locator('#servers-toolbar button', { hasText: "Apply" }).click();

    await expect.poll(() => log.length).toBeGreaterThanOrEqual(2);
    expect(log[0].url).toBe("demigrate");
    expect(log[1].url).toBe("migrate");
  });
```

### Step 4 — Append scenario 3: demigrate failure always reloads + retains failed entry

- [ ] **Append:**

```ts
  // B1 scenario 3: a demigrate failure must still trigger a reload (§4 D6
  // revised) and retain the failed entry in dirty for retry.
  test("demigrate failure always-reloads and retains failed entry in dirty", async ({ page, hub }) => {
    const scanBody = {
      entries: [
        {
          name: "demo",
          client_presence: {
            "claude-code": { transport: "relay", endpoint: "" },
          },
        },
      ],
    };

    // Single /api/scan route that both fulfills AND increments the counter.
    // Installing a second page.route for the same URL would make only one of
    // them respond (Playwright route precedence is stack-based and
    // implementation-defined); keep it one route.
    let scanCallCount = 0;
    await page.route("**/api/scan", async (r) => {
      scanCallCount++;
      await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(scanBody) });
    });
    await page.route("**/api/status", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));
    await page.route("**/api/demigrate", (r) => r.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "disk full" }) }));

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('table.servers-matrix');
    const initialScanCount = scanCallCount;

    await page.locator('table.servers-matrix input[type="checkbox"]').first().uncheck();
    await page.locator('#servers-toolbar button', { hasText: "Apply" }).click();

    await expect(page.locator('#servers-toolbar .error')).toContainText("Failed:");
    await expect(page.locator('#servers-toolbar .error')).toContainText("demo/demigrate/claude-code");
    await expect(page.locator('#servers-toolbar .error')).toContainText("disk full");

    // Reload MUST have run (scan called again since Apply click).
    await expect.poll(() => scanCallCount).toBeGreaterThan(initialScanCount);

    // Apply button stays enabled because dirty retained the failed entry.
    await expect(page.locator('#servers-toolbar button', { hasText: "Apply" })).toBeEnabled();
  });
```

### Step 5 — Append scenario 4: tooltip copy reflects uncheck-to-demigrate semantic

- [ ] **Append:**

```ts
  // B1 scenario 4: tooltip copy on via-hub cells no longer points at the
  // obsolete `mcphub rollback --client` text.
  test("via-hub cell tooltip describes the Uncheck-and-Apply semantic", async ({ page, hub }) => {
    const scanBody = {
      entries: [
        {
          name: "demo",
          client_presence: { "claude-code": { transport: "relay", endpoint: "" } },
        },
      ],
    };
    await page.route("**/api/scan", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(scanBody) }));
    await page.route("**/api/status", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('table.servers-matrix');

    const checkbox = page.locator('table.servers-matrix input[type="checkbox"]').first();
    await expect(checkbox).toHaveAttribute("title", /Uncheck and Apply to roll this binding back/);
    // Negative assertion: the old copy must not survive.
    const title = await checkbox.getAttribute("title");
    expect(title).not.toContain("mcphub rollback --client");
  });
```

### Step 6 — Append scenario 5: per-client gate + 3-outcome pruning + second-Apply retry

- [ ] **Append:**

```ts
  // B1 scenario 5 (the big one): per-client gate prevents migrate from
  // writing a polluted backup that a retry-demigrate would then misread.
  // Dirty retains failed + gated entries across the first Apply; the
  // second Apply fires exactly one demigrate + one migrate in correct
  // order and does NOT re-fire the truly-successful migrate.
  test("per-client gate: dirty retains failed+gated, second Apply fires 2 POSTs in order", async ({ page, hub }) => {
    const scanBody = {
      entries: [
        {
          name: "A",
          client_presence: {
            "claude-code": { transport: "relay", endpoint: "" }, // via-hub → queued demigrate (WILL FAIL)
          },
        },
        {
          name: "B",
          client_presence: {
            "claude-code": { transport: "stdio", endpoint: "" }, // direct stdio → queued migrate (GATED)
            "codex-cli":   { transport: "stdio", endpoint: "" }, // direct stdio → queued migrate (SUCCESS)
          },
        },
      ],
    };
    await page.route("**/api/scan", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(scanBody) }));
    await page.route("**/api/status", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));

    const log: { url: string; body: string | null; at: number }[] = [];
    let demigrateShouldFail = true;

    await page.route("**/api/demigrate", async (r) => {
      const body = r.request().postData();
      log.push({ url: "demigrate", body, at: Date.now() });
      if (demigrateShouldFail) {
        await r.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "disk full" }) });
      } else {
        await r.fulfill({ status: 204, body: "" });
      }
    });
    await page.route("**/api/migrate", async (r) => {
      const body = r.request().postData();
      log.push({ url: "migrate", body, at: Date.now() });
      await r.fulfill({ status: 204, body: "" });
    });

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('table.servers-matrix');

    // Uncheck (A, claude-code) — queued demigrate.
    await page.locator('table.servers-matrix tr').filter({ hasText: "A" }).locator('input[type="checkbox"]').nth(0).uncheck();
    // Check (B, claude-code) — queued migrate (will be GATED).
    await page.locator('table.servers-matrix tr').filter({ hasText: "B" }).locator('input[type="checkbox"]').nth(0).check();
    // Check (B, codex-cli) — queued migrate (will SUCCEED).
    await page.locator('table.servers-matrix tr').filter({ hasText: "B" }).locator('input[type="checkbox"]').nth(1).check();

    // FIRST Apply.
    await page.locator('#servers-toolbar button', { hasText: "Apply" }).click();

    // Wait for Apply to fully settle: "Failed:" banner appears AND Apply
    // button is re-enabled (setApplying(false) has run). Then assert exact
    // POST count + per-cell shape. Phase 1 per-cell: ONE /api/demigrate
    // for (A, claude-code). Phase 2 per-cell with gate: migrate for
    // (B, claude-code) SKIPPED (gated — failedDemigrateClients has
    // claude-code), migrate for (B, codex-cli) FIRES as its own POST.
    // Total: 2 POSTs, each with a single-element clients array.
    await expect(page.locator('#servers-toolbar .error')).toContainText("Failed:");
    await expect(page.locator('#servers-toolbar button', { hasText: "Apply" })).toBeEnabled();
    expect(log).toHaveLength(2);
    expect(log[0].url).toBe("demigrate");
    expect(JSON.parse(log[0].body!)).toEqual({ servers: ["A"], clients: ["claude-code"] });
    expect(log[1].url).toBe("migrate");
    expect(JSON.parse(log[1].body!)).toEqual({ servers: ["B"], clients: ["codex-cli"] });

    // Un-stub demigrate so the retry succeeds.
    demigrateShouldFail = false;
    log.length = 0;

    // SECOND Apply (no re-toggling; dirty still has the retained entries —
    // the failed demigrate(A) and the gated migrate(B, claude-code)).
    await page.locator('#servers-toolbar button', { hasText: "Apply" }).click();

    // Wait for the success banner text (indicates setApplying(false) ran
    // AND success-pruning emptied dirty). Then assert Apply button is
    // DISABLED — because both retained entries ran successfully on this
    // retry and were pruned, dirty.size is now 0. (Asserting "enabled"
    // here would either race or time out on a correct implementation —
    // Codex plan-R3 P1.)
    await expect(page.locator('#servers-toolbar span')).toContainText("Applied.");
    await expect(page.locator('#servers-toolbar button', { hasText: "Apply" })).toBeDisabled();

    // Exact-count assertion: one demigrate(A, claude-code) that now succeeds,
    // one migrate(B, claude-code) that fires because the blocking demigrate
    // succeeded. (B, codex-cli) does NOT re-fire — pruned on first Apply
    // as truly-successful.
    expect(log).toHaveLength(2);
    expect(log[0].url).toBe("demigrate");
    expect(log[1].url).toBe("migrate");
    expect(JSON.parse(log[1].body!)).toEqual({ servers: ["B"], clients: ["claude-code"] });
  });
```

### Step 7 — Grep for obsolete assertions

- [ ] **Search the whole E2E tree for any test that asserted the old `mcphub rollback --client` tooltip text** so we don't leave a scenario expecting the deleted copy:

```bash
grep -rn "mcphub rollback --client" internal/gui/e2e/tests/ | grep -v "not.toContain"
```

Expected: **zero matches** after excluding scenario 4's deliberate negative assertion (which itself contains the literal inside `expect(title).not.toContain("mcphub rollback --client")`). If any match surfaces from the grep, delete or update that assertion. (The `grep -v "not.toContain"` filter is intentional — scenario 4 asserts the literal is ABSENT from the live tooltip, so its line legitimately contains the literal.)

### Step 8 — Run the 5 new scenarios in isolation

- [ ] **Confirm each passes standalone** (fast iteration if something's wrong):

```bash
cd internal/gui/e2e && npm test -- tests/servers.spec.ts
```

Expected: 8 tests passed (3 pre-existing + 5 new).

### Step 9 — Run the full E2E suite

- [ ] **No regression in neighboring suites** (add-server, edit-server, migration, shell, dashboard, logs):

```bash
npm test
```

Expected: 52 passed (47 pre-B1 + 5 new).

### Step 10 — Run the full suite a SECOND time

- [ ] **Stability check** — global-setup cleanup is idempotent (proven during A2b); confirm the new tests also run clean on a warm `e2e/bin/servers/` directory:

```bash
npm test
```

Expected: 52 passed again.

### Step 11 — Commit

- [ ] **Commit Task 6:**

```bash
git add internal/gui/e2e/tests/servers.spec.ts
git commit -m "test(gui/e2e): B1 servers matrix — 5 new Playwright scenarios

Covers every semantic introduced by B1's Servers.tsx rewrite per B1
memo §6.3:

1. uncheck-via-hub-then-Apply posts /api/demigrate narrowed to the
   single (server, client) pair the user unchecked.
2. Mixed Apply dispatches /api/demigrate before /api/migrate across
   the whole Apply (§4 D4 order invariant).
3. Demigrate 500 failure still triggers the post-Apply /api/scan
   reload (§4 D6 revised: always-reload), and dirty retains the
   failed entry so Apply stays enabled for retry.
4. Via-hub cell tooltip describes the Uncheck-and-Apply semantic
   and no longer references the obsolete 'mcphub rollback --client'
   CLI flag (which never existed).
5. Per-client gate + 3-outcome pruning + second-Apply retry — the
   load-bearing state-machine test: (A, claude-code) demigrate
   fails, (B, claude-code) migrate is GATED (no POST), (B, codex-cli)
   migrate SUCCEEDS. First Apply posts exactly 2 requests (demigrate
   then filtered migrate). Second Apply without re-toggling posts
   exactly 2 requests (demigrate then the previously-gated migrate)
   — (B, codex-cli) does NOT re-fire because success-pruning
   removed it from dirty.

E2E count 47 → 52. Ran the suite twice back-to-back; both runs
green (global-setup cleanup idempotent)."
```

---

## Task 7: CLAUDE.md E2E coverage catch-up

**Files:**
- Modify: `CLAUDE.md` (E2E coverage section)

Doc-only commit. Catches up the stale "43 smoke tests total" line (committed at A2b Task 19 before R1/R2 Codex fixes added +4 E2E scenarios to A2b) and adds B1's +5 to land at 52.

### Step 1 — Read current CLAUDE.md coverage section

- [ ] **Find the section:**

```bash
grep -n "smoke tests total\|### What's covered\|### What's NOT covered" CLAUDE.md
```

Expected: ~3 lines showing section boundaries and the current "43 smoke tests total" number.

### Step 2 — Update the coverage section

- [ ] **Edit `CLAUDE.md`** — find the `### What's covered` section and replace it with:

```markdown
### What's covered

- Shell: sidebar, five nav links, hash routing, active-link highlight.
- Servers: matrix columns (Server + 4 clients + Port + State), empty-body state on clean tmpHome, Apply disabled with no dirty cells, uncheck-via-hub + Apply posts /api/demigrate narrowed to cell, mixed Apply dispatches demigrate-before-migrate ordering, demigrate failure always-reloads and retains failed entry for retry, via-hub tooltip describes Uncheck-and-Apply semantic (no more 'mcphub rollback --client' stale text), per-client gate + 3-outcome pruning (failed + gated retained, succeeded pruned) with second-Apply retry firing exactly the previously-gated migrate.
- Migration: h1, empty-state copy, group sections hidden on empty home, hashchange swap from Servers, full POST /api/dismiss → on-disk JSON → GET /api/dismissed round-trip, /api/scan-unfiltered regression guard (seed + dismiss + re-scan).
- Add server: empty-state + debounced YAML preview, live name-regex inline error, single-daemon flat bindings, cascade rename/delete with confirm, Save writes manifest, Save&Install port-conflict failure path with Retry Install banner, Paste YAML import, sidebar-intercept unsaved-changes guard, Advanced kind-toggle (workspace/global reveals/hides languages+port_pool), Advanced always-visible fields survive kind toggles.
- Edit server: #/edit-server?name= load from disk, name+kind locked, Save → Reinstall banner, Force Save with external-edit hash-mismatch preserving `_preservedRaw` top-level fields, nested-unknown read-only mode, load failure banner, sidebar-intercept when dirty, 4+-daemon matrix view, workspace-scoped Advanced (languages + port_pool), internal-ID cascade daemon rename, hashchange cancel/accept dirty-guard, Paste YAML → Save race (version-counter invariant).
- Dashboard: empty-cards state on fresh home, `/api/events` SSE connection opens on mount.
- Logs: picker + controls render, notice text on no-daemons state, controls disabled when no eligible entries.

52 smoke tests total (3 shell + 8 servers + 6 migration + 13 add-server + 17 edit-server + 2 dashboard + 3 logs), ~45s wall-time on a warm machine.
```

(If the wall-time estimate differs materially from what Task 6's runs actually produced, use the measured value.)

### Step 3 — Commit

- [ ] **Commit Task 7:**

```bash
git add CLAUDE.md
git commit -m "docs: CLAUDE.md E2E coverage catch-up (43 → 47 A2b R1/R2 → 52 B1)

The committed CLAUDE.md coverage section said '43 smoke tests total'
— written at A2b Task 19 before the R1 (+3 E2E) and R2 (+1 E2E)
Codex-fix commits that brought A2b's merged count to 47. B1 adds +5
(one per §6.3 scenario). Rewrote the Servers bullet to describe the
B1 matrix semantics explicitly (uncheck-to-demigrate, demigrate-
first ordering, always-reload on failure, per-client gate + 3-outcome
pruning, retry contract). Final total 52; per-file breakdown updated
(3 + 8 + 6 + 13 + 17 + 2 + 3)."
```

---

## Task 8: Final full-suite smoke + pre-PR verification

**Files:** none modified.

Final verification before opening the PR.

### Step 1 — Full pipeline

- [ ] **Run the full validation pipeline from repo root:**

```bash
go build ./...
go test ./... -count=1
cd internal/gui/frontend && npm run typecheck && npm run test --run
cd ../e2e && npm test
cd ../../..
```

Expected:
- `go build ./...` — exit 0.
- `go test ./...` — every package `ok`, 3 new `/api/manifest/edit` handler tests green.
- `npm run typecheck` — exit 0.
- `npm run test --run` — 107 Vitest tests passed.
- `npm test` — 52 Playwright scenarios passed.

### Step 2 — Re-run E2E to confirm idempotent cleanup

- [ ] **Back-to-back:**

```bash
cd internal/gui/e2e && npm test
```

Expected: 52 passed (second run in a row).

### Step 3 — Inspect branch log

- [ ] **Confirm the 7 commit breakdown:**

```bash
git log --oneline master..HEAD
```

Expected: 7 commits (Task 1 doc-only api.go/memo, Task 2 bug entry, Task 3 3 Go tests, Task 4 DirtyMap refactor, Task 5 matrix wiring, Task 6 E2E, Task 7 CLAUDE.md) plus whatever design-memo commits predate the branch divergence from master (the memo revisions from rev 1 through rev 13). Total likely ~20 commits on branch (13 memo + 7 impl) — that's fine; the plan is delivered.

### Step 4 — Clean tree

- [ ] **Confirm nothing uncommitted:**

```bash
git status
```

Expected: `nothing to commit, working tree clean`.

---

## Self-review (author ran this)

**Spec coverage** (B1 design memo §7 handoff):

- §7 step 1 (doc-only prep: followup #2 + api.go comments) → Task 1 ✓
- §7 step 2 (file b1-backup-file-race.md) → Task 2 ✓
- §7 step 3 (3 `/api/manifest/edit` tests) → Task 3 ✓
- §7 step 4 (DirtyMap shape refactor) → Task 4 ✓
- §7 step 5 (matrix wiring) → Task 5 ✓
- §7 step 6 (5 E2E scenarios) → Task 6 ✓
- §7 step 7 (CLAUDE.md 43 → 47 → 52) → Task 7 ✓

**Placeholder scan:** no TBDs, no "handle edge cases", no "similar to" — every code step has its full code. ✓

**Type consistency:** `Direction`, `DirtyMap`, `OutcomeMap`, `Outcome` are all declared once (Task 4 for the first two; Task 5 for the last two) and referenced consistently. The test fixtures in Task 6 use the same `{servers, clients}` body shape for both `/api/migrate` and `/api/demigrate`. Tooltip copy text matches between Task 5 (the source change) and Task 6 scenario 4 (the assertion). ✓

---

Plan complete and saved to `docs/superpowers/plans/2026-04-25-phase-3b-ii-b1-servers-matrix-demigrate.md`. Two execution options:

**1. Subagent-Driven (recommended)** — fresh subagent per task, review between tasks, fast iteration in this session.

**2. Inline Execution** — executing-plans in this session, batch execution with checkpoints.

**Which approach?**

# Phase 3B-II A1 — Migration Screen Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the GUI Migration screen (`#/migration`) — a scan-driven, 4-group view (Via hub / Can migrate / Unknown / Per-session) with `Migrate selected`, per-row `Demigrate`, backend-persisted `Dismiss`, and a DISABLED `Create manifest` button gated until the A2 (Add/Edit manifest) screen ships.

**Architecture:** Frontend-heavy feature (Preact) with three small backend additions: (1) `/api/demigrate` wrapping the already-shipped `api.Demigrate`; (2) POST `/api/dismiss` persisting "hide this unknown entry" state in a per-machine JSON file (`%LOCALAPPDATA%\mcp-local-hub\gui-dismissed.json` on Windows, `$XDG_STATE_HOME/mcp-local-hub/gui-dismissed.json` on POSIX — same state-directory convention as `internal/api/logs.go` and `internal/api/workspace_registry.go`); (3) GET `/api/dismissed` returning the current dismissed-names list. The Migration screen fetches `/api/scan` AND `/api/dismissed` in parallel and filters the unknown group client-side via a pure helper (`lib/migration-grouping.ts`). Dismiss is therefore per-machine, per-user, cross-browser, and survives cache-clear / reinstall. `/api/scan` is deliberately left unchanged — Servers and every other GUI consumer continues to see the unfiltered truth. SSE auto-refresh uses the existing `useEventSource` hook. Playwright covers empty-state, group rendering, disabled Create-manifest, and a real POST → GET → read-back-from-disk assertion on the dismissed JSON.

**Why the dismissed list is NOT filtered server-side in /api/scan:** `/api/scan` is the shared GUI data source for both Servers (which builds its matrix via `collectServers(scan)` at `internal/gui/frontend/src/lib/routing.ts:47` — consumes every entry without status inspection) and Migration. Applying a Migration-specific dismissal filter inside the shared handler would hide dismissed unknowns from the Servers matrix too — a silent cross-screen regression. Keeping dismissal as an explicit opt-in (Migration fetches `/api/dismissed` alongside `/api/scan` and filters client-side) makes the scope unambiguous. A4 Settings later builds a management UI on top of the same endpoints.

**Tech Stack:** Vite 8 + TypeScript 5 + Preact 10 (frontend), Vitest 4 (unit), Playwright + Chromium (E2E), Go 1.26 (two new HTTP handlers + one storage helper).

---

## Codex-CLI design decisions absorbed before writing this plan

1. **A1 standalone, not A1+A2 combined.** Codex CLI reviewed three orderings and recommended A1 as a minimum deliverable slice to keep review blast-radius bounded. A2 (~2000 LOC accordion form + live YAML preview + validation) ships as a separate PR. `Create manifest` is visibly gated in this plan, not absent and not hacked-around.
2. **`Dismiss` is backend-persisted (not localStorage).** Codex CLI originally recommended localStorage for scope discipline, but user review flagged that localStorage has a user-visible UX flaw — browser cache clears / reinstalls / new profiles reset the list, forcing users to re-dismiss the same entries repeatedly. Dismiss semantically means "hide this server forever", not "hide-for-session". A small backend API + JSON file persists the user intent across browser state changes without cross-device sync (explicitly out-of-scope per spec §2.2). A4 Settings will later build UI on top of the same file.
3. **Minimal Dismiss surface — POST + GET only, no un-dismiss endpoint.** POST `/api/dismiss {server}` writes one name; GET `/api/dismissed` returns `{"unknown": [...]}`. No un-dismiss endpoint ships in A1 — that UX belongs to A4 Settings' management screen. Migration fetches `/api/scan` and `/api/dismissed` in parallel and filters client-side (backend filtering in `/api/scan` was rejected in R2 of this plan review because `/api/scan` is shared with Servers, which `collectServers`-maps every entry — a shared-handler filter would cause cross-screen regression).
4. **`/api/demigrate` endpoint exists.** The backend helper `api.Demigrate` shipped in PR #3 (commit `d76ac93`), but no GUI HTTP handler wraps it yet. Task 1 adds that handler mirroring the `migrate.go` shape.

## Codex CLI plan-review R1 fixes absorbed

Round 1 flagged two P1 blockers + four P2 important + one P3 nit. All addressed in this revision:

1. **[P1]** Handler tests originally set `Origin: http://127.0.0.1` + `req.Host = "127.0.0.1"`, but the existing `requireSameOrigin` middleware keys on `Sec-Fetch-Site`. R1 fix: all normal-path Go tests now set `Sec-Fetch-Site: same-origin` and the cross-origin negative test sets `Sec-Fetch-Site: cross-site`, matching the established `internal/gui/csrf_test.go` pattern.
2. **[P1]** No `api.DataDir()` helper exists. R1 fix: dismiss.go resolves its storage path inline using `os.Getenv("LOCALAPPDATA")` with an `os.UserConfigDir()` fallback — same convention as `internal/api/logs.go:93`, `settings.go:14`, `workspace_registry.go:71`. Test seam is the existing `t.Setenv("LOCALAPPDATA", t.TempDir())` pattern from `internal/api/install_test.go:287`.
3. **[P2]** §5.2 calls for green/yellow/purple/gray group semantics but the plan never updated CSS. R1 fix: Task 5 (scaffolding) now appends theme-aware CSS rules for `.group-via-hub` / `.group-can-migrate` / `.group-unknown` / `.group-per-session` to `internal/gui/frontend/src/styles/style.css`.
4. **[P2]** `/api/scan` filter changes contract for every GUI consumer. R1 fix (WRONG — see R2 superseding fix below): Architecture paragraph initially claimed Servers was unaffected. R2 verified this is false — `collectServers(scan)` at `internal/gui/frontend/src/lib/routing.ts:47` maps EVERY scan entry without status inspection, so a shared-handler filter would hide dismissed unknowns from the Servers matrix too.
5. **[P2]** Task 2 was missing schema-version fail-closed + concurrent-race tests. R1 fix: Task 2 now includes `TestDismissUnknown_FailsClosedOnUnknownVersion` and `TestDismissUnknown_ConcurrentCallsArePreserved` (goroutine race test).
6. **[P2]** Playwright dismiss test only asserted `/api/scan` was well-formed, not that persistence worked. R1 fix: Task 10 now reads `<hub.home>/mcp-local-hub/gui-dismissed.json` with `readFileSync` and asserts the POSTed name is present in the on-disk array.
7. **[P3]** Placeholders (`DataDir() { /* ... */ }`, "likely already exported", `writeJSON` references, wrong task-number comments). R1 fix: DataDir() removed entirely; scan.go edits use `json.NewEncoder(w).Encode(result)` matching the real file; Migration.tsx comments point to the correct task numbers.

## Codex CLI plan-review R2 fixes absorbed

Round 2 (after R1 fixes landed) found two more P2s. Both addressed in this revision:

1. **[P2 superseding R1 finding 4]** `/api/scan` dismissal filter was not actually safe. `collectServers(scan)` consumes every entry — dismissed unknowns would disappear from the Servers matrix too. R2 fix: removed the `/api/scan` filter entirely. Added GET `/api/dismissed` endpoint; Migration screen fetches `/api/scan` + `/api/dismissed` in parallel and filters the unknown group client-side. `/api/scan` is now untouched by Dismiss. Task 3 rewritten: no scan.go modification, no `dismissedLister` interface, no `TestScanHandler_FiltersDismissedUnknownEntries` test. Task 4 grouping helper takes `dismissedUnknown: Set<string>` parameter. Task 5 Migration fetches both endpoints in parallel.

2. **[P2]** Dismissed-file POSIX fallback used `os.UserConfigDir()` (returns `~/.config` on Linux) — misaligned with the architecture claim of `$XDG_STATE_HOME`. R2 fix: `dismissedFilePath()` now follows the same state-directory convention as `internal/api/workspace_registry.go:71` and `internal/api/logs.go:93` — `%LOCALAPPDATA%` on Windows, `$XDG_STATE_HOME` then `~/.local/state/mcp-local-hub/` on POSIX. Architecture paragraph updated to match.

## Codex CLI plan-review R3 fixes absorbed

Round 3 (after R2 fixes landed) found 1 P1 + 3 P2s where the R2 edits left stale references. All addressed in this revision:

1. **[P1]** Task 6's `MigrationScreen` replacement body had regressed — it fetched only `/api/scan`, dropped `DismissedResponse` / `dismissedUnknown`, and called `groupMigrationEntries(scan)` with one argument even though Task 4 defines the helper as `(scan, dismissedUnknown: Set<string>)`. R3 fix: Task 6 body now preserves the parallel `Promise.all([/api/scan, /api/dismissed])` fetch, the `dismissedUnknown` state, and the two-argument `groupMigrationEntries(scan, dismissedUnknown)` call.
2. **[P2]** File-structure block still listed `scan.go` / `scan_test.go` as modified, Task 2 preamble said "scan-filter integration ships in Task 3", and two commit-message snippets still described backend scan filtering. R3 fix: file-structure block trimmed to remove scan-file entries; all "scan-filter" prose rewritten to reflect the actual client-side filter in `groupMigrationEntries`; commit copy updated in Task 2 and Task 7.
3. **[P2]** Task 10 `/api/scan`-unmodified assertion only checked that the response had an `entries` field, which would not detect a hypothetical server-side filter on an empty home. R3 fix: split into a dedicated test "`/api/scan` remains unfiltered by dismissals (Servers-matrix invariant)" that seeds a real unknown stdio entry in `<hub.home>/.claude.json`, pre-asserts `/api/scan` shows it, POSTs `/api/dismiss` for its name, then asserts `/api/scan` still shows it. Total E2E count bumped 5 → 6.
4. **[P2]** Task 2 preamble and commit copy still named `os.UserConfigDir` / `<UserConfigDir>` as the POSIX fallback, contradicting the R2 state-dir convention the code itself follows. R3 fix: preamble + commit message rewritten to name `$XDG_STATE_HOME` and `~/.local/state/mcp-local-hub/` explicitly, matching `dismissedFilePath()` body and `workspace_registry.go:71`.

---

## File structure

```
internal/api/
├── dismiss.go                           (NEW — ListDismissedUnknown, DismissUnknown storage helpers)
└── dismiss_test.go                      (NEW — file persistence + atomic write + schema-version + race)

internal/gui/
├── demigrate.go                         (NEW — HTTP handler wrapping api.Demigrate)
├── demigrate_test.go                    (NEW)
├── dismiss.go                           (NEW — POST /api/dismiss + GET /api/dismissed handlers)
├── dismiss_test.go                      (NEW)
├── server.go                            (MODIFY — +demigrater +dismisser interfaces, wire handlers)
└── frontend/
    ├── src/
    │   ├── app.tsx                      (MODIFY — +Migration in SCREENS, sidebar link)
    │   ├── api.ts                       (MODIFY — +postDismiss helper)
    │   ├── lib/
    │   │   ├── migration-grouping.ts    (NEW — pure grouping helper)
    │   │   └── migration-grouping.test.ts (NEW — Vitest)
    │   └── screens/
    │       └── Migration.tsx            (NEW — the screen itself)

internal/gui/e2e/tests/
├── shell.spec.ts                        (MODIFY — 4 nav links + migration hashchange)
└── migration.spec.ts                    (NEW — Playwright)

CLAUDE.md                                (MODIFY — mention migration route in "What's covered" section)
```

**Not touched:**
- `api.Scan` / `api.MigrateFrom` / `api.Demigrate` / `api.ExtractManifestFromClient` — every helper A1 calls is already shipped. Both `api.Scan` and the GUI `/api/scan` handler are left untouched; dismissal is a Migration-only client-side filter applied by `groupMigrationEntries` against the separately-fetched `/api/dismissed` list.
- Any other adapter / scheduler / daemon code.

---

## Verification prerequisites (run before starting Task 1)

Confirm the branch is healthy:

```bash
cd D:/dev/mcp-local-hub
git checkout master && git pull --ff-only
go build ./... && go test ./... -count=1
```

Expected: clean build, all 12 Go packages PASS. Then create the feature branch:

```bash
git checkout -b feat/phase-3b-ii-a1-migration-screen
```

---

## Task 1: Backend `/api/demigrate` endpoint

**Files:**
- Create: `internal/gui/demigrate.go`
- Create: `internal/gui/demigrate_test.go`
- Modify: `internal/gui/server.go` (add `demigrater` interface + `realDemigrater` + wire `registerDemigrateRoutes`)

### Step 1: Write failing handler test

Create `internal/gui/demigrate_test.go`:

```go
package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeDemigrater struct {
	gotServers []string
	gotClients []string
	err        error
}

func (f *fakeDemigrater) Demigrate(servers, clients []string) error {
	f.gotServers = append([]string{}, servers...)
	f.gotClients = append([]string{}, clients...)
	return f.err
}

// Same-origin middleware keys on Sec-Fetch-Site (see internal/gui/csrf.go
// and the existing internal/gui/csrf_test.go pattern). Positive tests
// set same-origin; the explicit cross-origin negative sets cross-site.
func TestDemigrateHandler_RejectsNonPOST(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.demigrater = &fakeDemigrater{}
	req := httptest.NewRequest(http.MethodGet, "/api/demigrate", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDemigrateHandler_ForwardsServersAndClients(t *testing.T) {
	fake := &fakeDemigrater{}
	s := NewServer(Config{Port: 0})
	s.demigrater = fake
	body := bytes.NewReader([]byte(`{"servers":["memory"],"clients":["claude-code"]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/demigrate", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", w.Code, w.Body.String())
	}
	if len(fake.gotServers) != 1 || fake.gotServers[0] != "memory" {
		t.Errorf("gotServers=%v, want [memory]", fake.gotServers)
	}
	if len(fake.gotClients) != 1 || fake.gotClients[0] != "claude-code" {
		t.Errorf("gotClients=%v, want [claude-code]", fake.gotClients)
	}
}

func TestDemigrateHandler_SurfacesDemigrateError(t *testing.T) {
	fake := &fakeDemigrater{err: errStub("boom")}
	s := NewServer(Config{Port: 0})
	s.demigrater = fake
	body := bytes.NewReader([]byte(`{"servers":["memory"]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/demigrate", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	var body2 struct{ Error, Code string }
	_ = json.Unmarshal(w.Body.Bytes(), &body2)
	if !strings.Contains(body2.Error, "boom") {
		t.Errorf("error=%q, want contains boom", body2.Error)
	}
	if body2.Code != "DEMIGRATE_FAILED" {
		t.Errorf("code=%q, want DEMIGRATE_FAILED", body2.Code)
	}
}

func TestDemigrateHandler_RejectsCrossOrigin(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.demigrater = &fakeDemigrater{}
	body := bytes.NewReader([]byte(`{"servers":["memory"]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/demigrate", body)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
```

### Step 2: Run — confirm red

```bash
go test ./internal/gui/ -run TestDemigrateHandler -count=1
```

Expected: compile fails with `s.demigrater undefined` and `registerDemigrateRoutes` missing.

### Step 3: Add the handler + wire it into Server

Append to `internal/gui/server.go` immediately after the `migrator` block:

```go
// demigrater is the narrow interface the /api/demigrate handler needs.
// Semantics mirror migrator: per-row failures inside the DemigrateReport
// are aggregated into a single error so partial failures cannot silently
// 204 and mislead the GUI into thinking the rollback succeeded.
// realDemigrater is the production adapter; tests inject their own.
type demigrater interface {
	Demigrate(servers, clients []string) error
}

type realDemigrater struct{}

// Demigrate delegates to api.Demigrate. ScanOpts left zero (embed-first
// manifest path, like realMigrator). clients is forwarded into
// DemigrateOpts.ClientsInclude; empty slice preserves the "all bindings
// configured in the manifest" shape.
//
// Per-row failures are aggregated into a single error for the same
// reason realMigrator aggregates: api.Demigrate returns nil error when
// only per-row writes fail, and dropping that slice would let the GUI
// clear its pending state after partial success.
func (realDemigrater) Demigrate(servers, clients []string) error {
	report, err := api.NewAPI().Demigrate(api.DemigrateOpts{
		Servers:        servers,
		ClientsInclude: clients,
	})
	if err != nil {
		return err
	}
	if report != nil && len(report.Failed) > 0 {
		msgs := make([]string, 0, len(report.Failed))
		for _, f := range report.Failed {
			msgs = append(msgs, f.Server+"/"+f.Client+": "+f.Err)
		}
		return fmt.Errorf("%d demigrate row(s) failed: %s", len(report.Failed), strings.Join(msgs, "; "))
	}
	return nil
}
```

Add `demigrater demigrater` field to the `Server` struct (alongside `migrator migrator`):

```go
	migrator         migrator
	demigrater       demigrater
	restart          restarter
```

Set it in `NewServer` right after `s.migrator = realMigrator{}`:

```go
	s.migrator = realMigrator{}
	s.demigrater = realDemigrater{}
	s.restart = realRestarter{}
```

Register the route in `NewServer` right after `registerMigrateRoutes(s)`:

```go
	registerMigrateRoutes(s)
	registerDemigrateRoutes(s)
	registerServerRoutes(s)
```

Create `internal/gui/demigrate.go`:

```go
// internal/gui/demigrate.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// demigrateRequest is the /api/demigrate POST body.
//
// Servers lists server names whose migrated entries should be rolled back to
// their pre-migrate stdio shape. Clients is optional: when non-empty it
// narrows the rollback to the listed client adapters (matches
// api.DemigrateOpts.ClientsInclude semantics). Empty Clients rolls back
// every (server, client) binding the manifest lists.
type demigrateRequest struct {
	Servers []string `json:"servers"`
	Clients []string `json:"clients,omitempty"`
}

func registerDemigrateRoutes(s *Server) {
	s.mux.HandleFunc("/api/demigrate", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req demigrateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if err := s.demigrater.Demigrate(req.Servers, req.Clients); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "DEMIGRATE_FAILED")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
```

### Step 4: Run — confirm green + full suite

```bash
go test ./internal/gui/ -run TestDemigrateHandler -count=1
go test ./... -count=1
```

Expected: 4 new `TestDemigrateHandler_*` PASS. All other packages still PASS.

### Step 5: Commit

```bash
git add internal/gui/demigrate.go internal/gui/demigrate_test.go internal/gui/server.go
git commit -m "feat(gui): /api/demigrate endpoint wrapping api.Demigrate (A1 dep)

Mirrors migrate.go: POST-only, same-origin gated, request body
{servers[], clients[]}. realDemigrater aggregates DemigrateReport.Failed
rows into one error so partial failures cannot silently 204.
Injectable demigrater interface for tests (four handler tests cover
method rejection, forwarding, error surfacing, cross-origin reject)."
```

---

## Task 2: Backend `api.DismissUnknown` + persistence

**Files:**
- Create: `internal/api/dismiss.go`
- Create: `internal/api/dismiss_test.go`

Goal of this task: the data plane only. The GUI HTTP handlers (POST /api/dismiss + GET /api/dismissed) ship in Task 3 as a separate, easy-to-review piece.

Storage-path convention matches `internal/api/workspace_registry.go:71` (state-dir convention): `%LOCALAPPDATA%\mcp-local-hub\gui-dismissed.json` on Windows, `$XDG_STATE_HOME/mcp-local-hub/gui-dismissed.json` on POSIX, `~/.local/state/mcp-local-hub/gui-dismissed.json` fallback. Dismissed data is transient state, not user config data — state-dir is the right place. No new helper in `paths.go` is needed — the inline resolution mirrors existing sibling files. The E2E fixture already sets `LOCALAPPDATA: home` (`internal/gui/e2e/fixtures/hub.ts:46`), so the test seam is simply `t.Setenv("LOCALAPPDATA", t.TempDir())` — matches `internal/api/install_test.go:287`.

### Step 1: Write failing storage tests

Create `internal/api/dismiss_test.go`:

```go
package api

import (
	"os"
	"path/filepath"
	"testing"
)

// withTmpDataDir redirects the LOCALAPPDATA path the dismiss helpers
// resolve against (matching internal/api/logs.go:93,
// settings.go:14, workspace_registry.go:71) to a per-test tempdir.
// Same seam install_test.go uses at line 287. Tests run in parallel,
// so each t.TempDir() yields a fresh directory.
//
// Returns the mcp-local-hub subdirectory path the helpers will actually
// write into, so tests can check for leaked temp files or the dismissed
// JSON content directly.
func withTmpDataDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	return filepath.Join(tmp, "mcp-local-hub")
}

func TestDismissUnknown_EmptyFileReturnsEmptySet(t *testing.T) {
	_ = withTmpDataDir(t)
	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatalf("ListDismissedUnknown on fresh dir: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected empty set, got %v", names)
	}
}

func TestDismissUnknown_RoundTripsSingleEntry(t *testing.T) {
	_ = withTmpDataDir(t)
	if err := DismissUnknown("fetch"); err != nil {
		t.Fatalf("DismissUnknown: %v", err)
	}
	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := names["fetch"]; !ok {
		t.Errorf("fetch missing from dismissed set: %v", names)
	}
}

func TestDismissUnknown_DedupesRepeatedCalls(t *testing.T) {
	_ = withTmpDataDir(t)
	_ = DismissUnknown("fetch")
	_ = DismissUnknown("fetch")
	_ = DismissUnknown("fetch")
	names, _ := ListDismissedUnknown()
	if len(names) != 1 {
		t.Errorf("expected 1 entry after 3 dismiss calls, got %d: %v", len(names), names)
	}
}

func TestDismissUnknown_PersistsAcrossReads(t *testing.T) {
	dir := withTmpDataDir(t)
	_ = DismissUnknown("fetch")
	_ = DismissUnknown("ripgrep-mcp")
	// Now read from a fresh invocation — simulate a new process by
	// constructing the path manually and re-reading via the public API.
	_ = dir // only confirming the env-redirect worked
	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := names["fetch"]; !ok {
		t.Error("fetch missing")
	}
	if _, ok := names["ripgrep-mcp"]; !ok {
		t.Error("ripgrep-mcp missing")
	}
}

func TestDismissUnknown_GracefulOnCorruptFile(t *testing.T) {
	dir := withTmpDataDir(t)
	// Write a malformed JSON file where the dismissed list is expected.
	_ = os.MkdirAll(dir, 0o755)
	corruptPath := filepath.Join(dir, "gui-dismissed.json")
	if err := os.WriteFile(corruptPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := ListDismissedUnknown()
	// Design choice: corrupt file does NOT block the GUI. Return empty
	// set + nil error so the screen renders; the corruption will be
	// overwritten the next time the user calls DismissUnknown.
	if err != nil {
		t.Fatalf("corrupt file should not surface as error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("corrupt file should read as empty, got %v", names)
	}
}

func TestDismissUnknown_RejectsEmptyName(t *testing.T) {
	_ = withTmpDataDir(t)
	if err := DismissUnknown(""); err == nil {
		t.Error("expected error on empty name")
	}
}

func TestDismissUnknown_WritesAtomically(t *testing.T) {
	// Regression guard: if DismissUnknown writes via truncate + rewrite
	// without rename, a crash mid-write leaves the file empty/partial
	// and the next ListDismissedUnknown drops every prior dismissal.
	// This test verifies that a write-then-read sequence returns the
	// expected name AND that no temp file is left lingering.
	dir := withTmpDataDir(t)
	if err := DismissUnknown("stable-name"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "gui-dismissed.json" {
			continue
		}
		if filepath.Ext(name) == ".tmp" {
			t.Errorf("leaked temp file after atomic write: %s", name)
		}
	}
}

func TestDismissUnknown_FailsClosedOnUnknownVersion(t *testing.T) {
	// A4 Settings (future PR) may introduce a v2 schema with extra
	// fields. Readers that encounter v2 on a v1 build must not
	// silently return an empty/partial list as if it were v1 — the
	// failure mode is to return an empty set + nil err so the next
	// DismissUnknown call overwrites with v1 shape. Matches the
	// corrupt-JSON rationale at ListDismissedUnknown's docstring.
	dir := withTmpDataDir(t)
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "gui-dismissed.json")
	future := []byte(`{"version":2,"unknown":["should-be-ignored"],"extra":{"ts":"2026"}}`)
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatalf("unknown version should not surface as error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("unknown version should read as empty, got %v", names)
	}
}

func TestDismissUnknown_ConcurrentCallsArePreserved(t *testing.T) {
	// Two concurrent DismissUnknown calls could otherwise read the old
	// list, each add their own name, and race the rename — losing one.
	// The dismissMu mutex (internal to dismiss.go) serializes write
	// windows. This test fires N goroutines concurrently and asserts
	// every name made it into the persisted file.
	_ = withTmpDataDir(t)
	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		name := fmt.Sprintf("server-%02d", i)
		go func() {
			defer wg.Done()
			if err := DismissUnknown(name); err != nil {
				t.Errorf("DismissUnknown(%q): %v", name, err)
			}
		}()
	}
	wg.Wait()
	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != N {
		t.Fatalf("expected %d entries after concurrent dismisses, got %d: %v", N, len(names), names)
	}
	for i := 0; i < N; i++ {
		if _, ok := names[fmt.Sprintf("server-%02d", i)]; !ok {
			t.Errorf("server-%02d missing from dismissed set", i)
		}
	}
}
```

Add `"fmt"` and `"sync"` to the test file's import block (they are used only by the new tests).

### Step 2: Run — confirm red

```bash
go test ./internal/api/ -run TestDismissUnknown -count=1
```

Expected: `undefined: ListDismissedUnknown` and `undefined: DismissUnknown`.

### Step 3: Implement `internal/api/dismiss.go`

```go
// internal/api/dismiss.go
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// dismissedFileName is the per-machine persistence file for the
// Migration screen's "Dismiss" action on unknown stdio entries. Lives
// alongside other per-machine state (logs, settings, workspaces) so
// GUI and CLI agree on the same directory root.
const dismissedFileName = "gui-dismissed.json"

// dismissMu serializes concurrent DismissUnknown calls. Two racing
// POSTs could otherwise read the old file, add their name, and race
// the rename — losing one of the two dismissals.
var dismissMu sync.Mutex

// dismissedPayload is the on-disk JSON shape.
//
// Versioned via the top-level "version" field so a future A4 Settings
// schema change (e.g. "dismissed with timestamp", "per-client
// dismissal") can migrate without silently losing entries. Current
// version is 1; readers that encounter an unknown version fail
// closed (empty set returned) so the user can re-dismiss rather than
// seeing silently-half-applied state.
type dismissedPayload struct {
	Version int      `json:"version"`
	Unknown []string `json:"unknown"`
}

// ListDismissedUnknown returns the set of server names the user has
// dismissed in the Migration screen. Caller receives a map for O(1)
// membership checks (convert to slice with a helper if needed).
//
// Returns an empty set + nil error in all soft-failure cases:
// missing file, corrupt JSON, wrong schema version. Hard errors
// (permission denied, unreadable dir) surface so the GUI handler
// can log them without blocking the screen render.
func ListDismissedUnknown() (map[string]struct{}, error) {
	path, err := dismissedFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]struct{}{}, nil
	}
	var payload dismissedPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		// Corrupt file: return empty rather than blocking. Next
		// DismissUnknown write will overwrite the corrupt content.
		return map[string]struct{}{}, nil
	}
	if payload.Version != 1 {
		// Unknown schema version: same fail-closed rationale as
		// corrupt JSON — prefer an empty set over a silently
		// partially-honored list.
		return map[string]struct{}{}, nil
	}
	out := make(map[string]struct{}, len(payload.Unknown))
	for _, name := range payload.Unknown {
		if name == "" {
			continue
		}
		out[name] = struct{}{}
	}
	return out, nil
}

// DismissUnknown marks a server name as dismissed, persisting
// atomically (temp file + rename) so a crash mid-write can never
// leave a truncated on-disk list. Idempotent: dismissing the same
// name twice is a no-op.
//
// Returns an error on empty name (caller bug), filesystem failure
// (disk full, permission denied), or atomicity violation (rename
// failed). Dismissing a name that is currently in the hub-HTTP /
// can-migrate / per-session state is NOT rejected — the stored list
// only affects rendering in the "Unknown" group, so dismissing a
// name that later transitions to a different status is harmless:
// the name sits in the file unused until the transition flips back.
func DismissUnknown(name string) error {
	if name == "" {
		return errors.New("DismissUnknown: name must not be empty")
	}
	dismissMu.Lock()
	defer dismissMu.Unlock()

	existing, err := ListDismissedUnknown()
	if err != nil {
		return err
	}
	if _, already := existing[name]; already {
		return nil
	}
	existing[name] = struct{}{}

	sorted := make([]string, 0, len(existing))
	for n := range existing {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted) // Stable on-disk order for readable diffs / grep.

	payload := dismissedPayload{Version: 1, Unknown: sorted}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal dismissed payload: %w", err)
	}

	path, err := dismissedFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	// Atomic write: write to sibling temp file, then rename over the
	// target. Rename is atomic on both NTFS and ext4 for same-directory
	// renames; the sibling lives in the same dir as the target so the
	// cross-device-link gotcha does not apply.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // Best-effort cleanup on rename failure.
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// dismissedFilePath resolves the per-machine storage path using the
// same state-directory convention as internal/api/workspace_registry.go:71
// and internal/api/logs.go:93:
//   - Windows: %LOCALAPPDATA%\mcp-local-hub\gui-dismissed.json
//   - POSIX:   $XDG_STATE_HOME/mcp-local-hub/gui-dismissed.json
//              (~/.local/state/mcp-local-hub/gui-dismissed.json fallback)
//
// Dismissed data is transient state (user toggled "don't show me this
// again"), not user config data — state-dir is the right place.
// LOCALAPPDATA is checked first on Windows; POSIX prefers XDG_STATE_HOME
// then falls back to ~/.local/state. Tests override via
// t.Setenv("LOCALAPPDATA", t.TempDir()) (install_test.go:287 pattern).
func dismissedFilePath() (string, error) {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "mcp-local-hub", dismissedFileName), nil
		}
	}
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		// Honored on non-Windows too so the e2e hub fixture (which
		// sets LOCALAPPDATA: home to redirect every state path —
		// see internal/gui/e2e/fixtures/hub.ts:46) keeps working on
		// a Linux CI runner without extra plumbing.
		return filepath.Join(v, "mcp-local-hub", dismissedFileName), nil
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", dismissedFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", "mcp-local-hub", dismissedFileName), nil
}
```

Add `"runtime"` to the import block of `internal/api/dismiss.go`.

### Step 4: Run — confirm green + commit

```bash
go test ./internal/api/ -run TestDismissUnknown -count=1
```

Expected: 9/9 PASS (original 7 + FailsClosedOnUnknownVersion + ConcurrentCallsArePreserved).

```bash
git add internal/api/dismiss.go internal/api/dismiss_test.go
git commit -m "feat(api): DismissUnknown backend persistence for Migration screen

Stores dismissed server names in %LOCALAPPDATA%\\mcp-local-hub\\
gui-dismissed.json (Windows) or \$XDG_STATE_HOME/mcp-local-hub/
gui-dismissed.json (POSIX, fallback ~/.local/state/mcp-local-hub/),
matching the workspace_registry.go state-dir convention so no new
paths helper is needed.
v1 schema, atomic write (tmp + rename), package-level mutex
against concurrent POST races. Corrupt JSON, missing file, and
unknown schema version all fail closed to empty set so the GUI
never blocks on storage issues. Nine tests cover empty file,
round-trip, dedup, persistence, corrupt JSON, empty-name reject,
atomic-write (no leaked .tmp), schema-version fail-closed, and
concurrent-race preservation."
```

---

## Task 3: GUI `/api/dismiss` POST + `/api/dismissed` GET handlers

**Files:**
- Create: `internal/gui/dismiss.go` (both POST + GET handlers)
- Create: `internal/gui/dismiss_test.go`
- Modify: `internal/gui/server.go` (+`dismisser` interface + route registration)

`/api/scan` is intentionally NOT modified — R2 plan review confirmed that `collectServers(scan)` at `internal/gui/frontend/src/lib/routing.ts:47` maps every entry without status inspection, so a shared-handler filter would hide dismissed unknowns from the Servers matrix too. Migration instead reads the dismissed list via GET `/api/dismissed` and filters client-side (Task 4 helper + Task 5 wiring).

### Step 1: Write failing handler tests

Create `internal/gui/dismiss_test.go`:

```go
package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeDismisser struct {
	got []string
	err error
}

func (f *fakeDismisser) DismissUnknown(name string) error {
	f.got = append(f.got, name)
	return f.err
}

// Same-origin middleware keys on Sec-Fetch-Site; see
// internal/gui/csrf_test.go for the pattern.
func TestDismissHandler_RejectsNonPOST(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.dismisser = &fakeDismisser{}
	req := httptest.NewRequest(http.MethodGet, "/api/dismiss", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestDismissHandler_ForwardsServerName(t *testing.T) {
	fake := &fakeDismisser{}
	s := NewServer(Config{Port: 0})
	s.dismisser = fake
	body := bytes.NewReader([]byte(`{"server":"fetch"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/dismiss", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", w.Code, w.Body.String())
	}
	if len(fake.got) != 1 || fake.got[0] != "fetch" {
		t.Errorf("got=%v, want [fetch]", fake.got)
	}
}

func TestDismissHandler_RejectsEmptyServer(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.dismisser = &fakeDismisser{}
	body := bytes.NewReader([]byte(`{"server":""}`))
	req := httptest.NewRequest(http.MethodPost, "/api/dismiss", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	var body2 struct{ Code string }
	_ = json.Unmarshal(w.Body.Bytes(), &body2)
	if body2.Code != "BAD_REQUEST" {
		t.Errorf("code=%q, want BAD_REQUEST", body2.Code)
	}
}

func TestDismissHandler_SurfacesBackendError(t *testing.T) {
	fake := &fakeDismisser{err: errStub("disk full")}
	s := NewServer(Config{Port: 0})
	s.dismisser = fake
	body := bytes.NewReader([]byte(`{"server":"fetch"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/dismiss", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestDismissHandler_RejectsCrossOrigin(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.dismisser = &fakeDismisser{}
	body := bytes.NewReader([]byte(`{"server":"fetch"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/dismiss", body)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}
```

Also add the GET handler tests to the same `internal/gui/dismiss_test.go`. The same `fakeDismisser` type (already above) gains a `ListDismissedUnknown` method so tests can assert forwarded values without hitting the real file:

```go
// Extend the fakeDismisser defined above with the read side.
func (f *fakeDismisser) ListDismissedUnknown() (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, n := range f.got {
		out[n] = struct{}{}
	}
	return out, f.err
}

func TestDismissedHandler_RejectsNonGET(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.dismisser = &fakeDismisser{}
	req := httptest.NewRequest(http.MethodPost, "/api/dismissed", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestDismissedHandler_ReturnsUnknownList(t *testing.T) {
	fake := &fakeDismisser{got: []string{"fetch", "ripgrep-mcp"}}
	s := NewServer(Config{Port: 0})
	s.dismisser = fake
	req := httptest.NewRequest(http.MethodGet, "/api/dismissed", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Unknown []string `json:"unknown"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// Map iteration order is non-deterministic; assert as set.
	got := map[string]bool{}
	for _, n := range body.Unknown {
		got[n] = true
	}
	if !got["fetch"] || !got["ripgrep-mcp"] {
		t.Errorf("got %v, want {fetch, ripgrep-mcp}", body.Unknown)
	}
}

func TestDismissedHandler_EmptyListReturnsEmptyArray(t *testing.T) {
	// Empty-but-present array avoids frontend `undefined` checks. Must
	// not be `null` — the frontend code in Task 5 iterates this field.
	s := NewServer(Config{Port: 0})
	s.dismisser = &fakeDismisser{}
	req := httptest.NewRequest(http.MethodGet, "/api/dismissed", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	// Canonicalize via string-match so an encoder that emits "null"
	// for a nil slice fails this assertion.
	if !strings.Contains(w.Body.String(), `"unknown":[]`) {
		t.Errorf("want empty array, got body=%s", w.Body.String())
	}
}
```

Add `"strings"` to the test file's import block (used only by the empty-array test).

### Step 2: Run — confirm red

```bash
go test ./internal/gui/ -run TestDismiss -count=1
```

Expected: `s.dismisser undefined` and `registerDismissRoutes` missing.

### Step 3: Extend `Server` with one `dismisser` interface

In `internal/gui/server.go`, add the interface + real adapter right after the `demigrater` block:

```go
// dismisser is the narrow interface both /api/dismiss (POST) and
// /api/dismissed (GET) need. One interface for both directions keeps
// the injection shape small; the POST handler uses DismissUnknown,
// the GET handler uses ListDismissedUnknown.
// realDismisser forwards to api.DismissUnknown / api.ListDismissedUnknown
// (persistent JSON file).
type dismisser interface {
	DismissUnknown(name string) error
	ListDismissedUnknown() (map[string]struct{}, error)
}

type realDismisser struct{}

func (realDismisser) DismissUnknown(name string) error {
	return api.DismissUnknown(name)
}

func (realDismisser) ListDismissedUnknown() (map[string]struct{}, error) {
	return api.ListDismissedUnknown()
}
```

Extend the `Server` struct with the single field:

```go
	migrator   migrator
	demigrater demigrater
	dismisser  dismisser
	restart    restarter
```

Set it in `NewServer` right after `s.demigrater = realDemigrater{}`:

```go
	s.demigrater = realDemigrater{}
	s.dismisser = realDismisser{}
	s.restart = realRestarter{}
```

Register the new routes right after `registerDemigrateRoutes(s)`:

```go
	registerDemigrateRoutes(s)
	registerDismissRoutes(s)
	registerServerRoutes(s)
```

### Step 4: Implement `internal/gui/dismiss.go`

Create `internal/gui/dismiss.go`:

```go
// internal/gui/dismiss.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// dismissRequest is the /api/dismiss POST body. Matches the Migration
// screen's Unknown-group row: one server name per click.
type dismissRequest struct {
	Server string `json:"server"`
}

// dismissedResponse is the /api/dismissed GET body shape. The single
// "unknown" key is deliberately future-proof: A4 Settings may later
// add per-entry metadata (timestamp, per-client granularity) as
// additional keys alongside it, and the frontend can ignore fields
// it doesn't understand.
type dismissedResponse struct {
	Unknown []string `json:"unknown"`
}

func registerDismissRoutes(s *Server) {
	s.mux.HandleFunc("/api/dismiss", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req dismissRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if strings.TrimSpace(req.Server) == "" {
			writeAPIError(w, fmt.Errorf("server must not be empty"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if err := s.dismisser.DismissUnknown(req.Server); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "DISMISS_FAILED")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	s.mux.HandleFunc("/api/dismissed", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		set, err := s.dismisser.ListDismissedUnknown()
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "DISMISSED_LIST_FAILED")
			return
		}
		// Always emit a non-nil slice so the frontend sees `[]`
		// instead of `null` (see TestDismissedHandler_EmptyListReturnsEmptyArray).
		names := make([]string, 0, len(set))
		for n := range set {
			names = append(names, n)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dismissedResponse{Unknown: names})
	}))
}
```

`/api/scan` is not modified in this task. The existing scan handler and its tests stay as-is.

### Step 5: Green + full suite + commit

```bash
go test ./internal/gui/ -count=1
go test ./... -count=1
```

Expected: 5 new `TestDismissHandler_*` PASS + 3 new `TestDismissedHandler_*` PASS (8 new total); all previous packages still PASS.

```bash
git add internal/gui/dismiss.go internal/gui/dismiss_test.go internal/gui/server.go
git commit -m "feat(gui): /api/dismiss POST + /api/dismissed GET

POST /api/dismiss {server} persists via api.DismissUnknown (Task 2).
GET /api/dismissed returns {\"unknown\": [...]} for the Migration
screen to filter client-side (keeps /api/scan shared with Servers
untouched). Single dismisser interface owns both directions. Empty
list is emitted as [] not null so the frontend sees a stable array
shape. Eight handler tests cover method rejection, forwarding,
empty-body reject, backend-error surfacing, cross-origin reject,
and the empty-array canonicalization."
```

---

## Task 4: `lib/migration-grouping.ts` pure helper + Vitest

**Files:**
- Create: `internal/gui/frontend/src/lib/migration-grouping.ts`
- Create: `internal/gui/frontend/src/lib/migration-grouping.test.ts`

### Step 1: Write failing Vitest

Create `internal/gui/frontend/src/lib/migration-grouping.test.ts`:

```ts
import { describe, it, expect } from "vitest";
import { groupMigrationEntries } from "./migration-grouping";
import type { ScanResult } from "../types";

describe("groupMigrationEntries", () => {
  it("splits entries by backend-provided status into 4 groups", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "a", status: "via-hub", client_presence: {"claude-code": {transport: "http", endpoint: "http://localhost:9200/mcp"}}},
        {name: "b", status: "can-migrate", client_presence: {"claude-code": {transport: "stdio", endpoint: "npx"}}},
        {name: "c", status: "unknown", client_presence: {"codex-cli": {transport: "stdio", endpoint: "uvx"}}},
        {name: "d", status: "per-session", client_presence: {}},
      ],
    };
    const g = groupMigrationEntries(scan, new Set());
    expect(g.viaHub.map(e => e.name)).toEqual(["a"]);
    expect(g.canMigrate.map(e => e.name)).toEqual(["b"]);
    expect(g.unknown.map(e => e.name)).toEqual(["c"]);
    expect(g.perSession.map(e => e.name)).toEqual(["d"]);
  });

  it("drops entries with status='not-installed' (no client has them)", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "ghost", status: "not-installed", client_presence: {}},
        {name: "real", status: "via-hub", client_presence: {"claude-code": {transport: "http", endpoint: "http://localhost:9200/mcp"}}},
      ],
    };
    const g = groupMigrationEntries(scan, new Set());
    expect(g.viaHub.map(e => e.name)).toEqual(["real"]);
    expect(g.canMigrate.length + g.unknown.length + g.perSession.length).toBe(0);
  });

  it("drops entries without a status (defensive: malformed backend response)", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "no-status", client_presence: {}},
        {name: "real", status: "via-hub", client_presence: {}},
      ],
    };
    const g = groupMigrationEntries(scan, new Set());
    expect(g.viaHub.map(e => e.name)).toEqual(["real"]);
  });

  it("sorts each group alphabetically by name for stable UI order", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "z", status: "can-migrate", client_presence: {}},
        {name: "a", status: "can-migrate", client_presence: {}},
        {name: "m", status: "can-migrate", client_presence: {}},
      ],
    };
    const g = groupMigrationEntries(scan, new Set());
    expect(g.canMigrate.map(e => e.name)).toEqual(["a", "m", "z"]);
  });

  it("filters dismissedUnknown from the unknown group ONLY, not other groups", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "fetch", status: "unknown", client_presence: {"claude-code": {transport: "stdio"}}},
        {name: "also-dismissed", status: "can-migrate", client_presence: {}},
        {name: "kept", status: "unknown", client_presence: {}},
      ],
    };
    const dismissed = new Set<string>(["fetch", "also-dismissed"]);
    const g = groupMigrationEntries(scan, dismissed);
    // Dismissed unknown is hidden.
    expect(g.unknown.map(e => e.name)).toEqual(["kept"]);
    // A name in the dismissed set but classified as can-migrate must
    // NOT be filtered — dismissal is unknown-group only.
    expect(g.canMigrate.map(e => e.name)).toEqual(["also-dismissed"]);
  });

  it("handles a scan with null/undefined entries (fresh hub, no configs)", () => {
    const g1 = groupMigrationEntries({at: "x", entries: null}, new Set());
    expect(g1.viaHub).toEqual([]);
    expect(g1.canMigrate).toEqual([]);
    expect(g1.unknown).toEqual([]);
    expect(g1.perSession).toEqual([]);

    const g2 = groupMigrationEntries({at: "x", entries: []}, new Set());
    expect(g2.viaHub).toEqual([]);
    expect(g2.canMigrate).toEqual([]);
    expect(g2.unknown).toEqual([]);
    expect(g2.perSession).toEqual([]);
  });
});
```

### Step 2: Run — confirm red

```bash
cd internal/gui/frontend && npm run test -- src/lib/migration-grouping.test.ts
```

Expected: `Cannot find module './migration-grouping'`.

### Step 3: Implement `migration-grouping.ts`

Create `internal/gui/frontend/src/lib/migration-grouping.ts`:

```ts
import type { ScanResult, ScanEntry } from "../types";

// MigrationGroups is the 4-bucket shape the Migration screen renders.
// Names mirror the backend classifier (internal/api/scan.go:classify):
//   - viaHub: entries already routed through the hub (HTTP url pointing
//     at localhost). Readonly display; Demigrate roll-back action only.
//   - canMigrate: stdio entries whose server name matches a manifest
//     in servers/. Pre-checked with Migrate-selected batch action.
//   - unknown: stdio entries with no matching manifest. "Create
//     manifest" button (DISABLED until A2 ships) and "Dismiss".
//   - perSession: entries classified as not-shareable by nature
//     (currently: internal/api/scan.go:perSessionServers). Readonly info.
// An entry classified as "not-installed" (no client has it — scan saw
// the name via a manifest but no config references it) is dropped
// entirely — it has nothing to migrate/demigrate/dismiss.
//
// Dismissed entries are provided by a separate `/api/dismissed` GET
// (see Task 3). The grouping helper filters them out of the Unknown
// group ONLY — never from via-hub / can-migrate / per-session, even
// if the same name appears in dismissedUnknown. This keeps dismissal
// scoped to the Migration screen while /api/scan stays shared with
// Servers and other consumers.
export interface MigrationGroups {
  viaHub: ScanEntry[];
  canMigrate: ScanEntry[];
  unknown: ScanEntry[];
  perSession: ScanEntry[];
}

function byName(a: ScanEntry, b: ScanEntry): number {
  return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
}

export function groupMigrationEntries(
  scan: ScanResult,
  dismissedUnknown: Set<string>,
): MigrationGroups {
  const groups: MigrationGroups = {
    viaHub: [],
    canMigrate: [],
    unknown: [],
    perSession: [],
  };
  const entries = scan.entries ?? [];
  for (const entry of entries) {
    switch (entry.status) {
      case "via-hub":
        groups.viaHub.push(entry);
        break;
      case "can-migrate":
        groups.canMigrate.push(entry);
        break;
      case "unknown":
        if (dismissedUnknown.has(entry.name)) continue;
        groups.unknown.push(entry);
        break;
      case "per-session":
        groups.perSession.push(entry);
        break;
      default:
        // "not-installed" and malformed/missing status: drop. These
        // have nothing actionable in Migration.
        break;
    }
  }
  groups.viaHub.sort(byName);
  groups.canMigrate.sort(byName);
  groups.unknown.sort(byName);
  groups.perSession.sort(byName);
  return groups;
}
```

### Step 4: Run — confirm green

```bash
cd internal/gui/frontend && npm run test -- src/lib/migration-grouping.test.ts
npm run typecheck
```

Expected: 6/6 PASS; typecheck clean.

### Step 5: Commit

```bash
git add internal/gui/frontend/src/lib/migration-grouping.ts internal/gui/frontend/src/lib/migration-grouping.test.ts
git commit -m "feat(gui/frontend): groupMigrationEntries pure helper

Splits scan entries into the four Migration screen buckets (via-hub,
can-migrate, unknown, per-session) driven by backend-provided status.
Takes a dismissedUnknown Set<string> parameter that filters ONLY the
unknown group — other groups are never filtered even if their name
appears in the set (dismissal is unknown-group only). Sorts each
group alphabetically for stable UI order."
```

---

## Task 5: `postDismiss` API client helper + Migration scaffolding + route + sidebar

**Files:**
- Modify: `internal/gui/frontend/src/api.ts` (add `postDismiss`)
- Create: `internal/gui/frontend/src/screens/Migration.tsx`
- Modify: `internal/gui/frontend/src/app.tsx` (SCREENS + sidebar)
- Modify: `internal/gui/e2e/tests/shell.spec.ts` (4 nav links + migration hashchange)

### Step 1: Extend `api.ts` with POST helper

Replace the content of `internal/gui/frontend/src/api.ts` with:

```ts
// fetchOrThrow is the shared API wrapper mirroring the legacy fetchOrThrow
// from servers.js. Backend handlers surface errors via the {error, code}
// JSON envelope (writeAPIError in the Go side) — not the success shape the
// UI expects. Without the response-shape guard, callers that iterate the
// parsed body would treat the truthy envelope object as iterable and throw
// inside render logic, leaving the screen blank. Require resp.ok AND the
// declared top-level shape before trusting the payload.
export async function fetchOrThrow<T>(
  path: string,
  expect: "array" | "object",
  init?: RequestInit,
): Promise<T> {
  const resp = await fetch(path, init);
  let data: unknown = null;
  try {
    data = await resp.json();
  } catch {
    // Non-JSON body left as null; handled below.
  }
  if (!resp.ok) {
    const msg = (data as { error?: string } | null)?.error ?? resp.statusText ?? "unknown";
    throw new Error(`${path}: ${msg}`);
  }
  if (expect === "array" && !Array.isArray(data)) {
    throw new Error(`${path}: expected array, got ${typeof data}`);
  }
  if (
    expect === "object" &&
    (data === null || typeof data !== "object" || Array.isArray(data))
  ) {
    throw new Error(
      `${path}: expected object, got ${Array.isArray(data) ? "array" : typeof data}`,
    );
  }
  return data as T;
}

// postDismiss sends the Migration screen's Unknown-group Dismiss action
// to the hub. Backend persistence lives in Task 2; GET /api/dismissed
// in Task 3. This
// is a thin wrapper so the screen code does not repeat fetch plumbing.
// Throws on non-204 responses with a descriptive message including the
// backend-provided error field when present.
export async function postDismiss(server: string): Promise<void> {
  const resp = await fetch("/api/dismiss", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ server }),
  });
  if (resp.status === 204) return;
  let body: { error?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  throw new Error(`/api/dismiss: ${body?.error ?? resp.statusText}`);
}
```

### Step 2: Create `Migration.tsx` skeleton

Create `internal/gui/frontend/src/screens/Migration.tsx`:

```tsx
import { useEffect, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
import { groupMigrationEntries, type MigrationGroups } from "../lib/migration-grouping";
import type { ScanResult } from "../types";

// DismissedResponse mirrors the /api/dismissed handler shape from
// internal/gui/dismiss.go. Declared inline here rather than in
// types.ts because no other screen needs it today; promote to
// types.ts if A4 Settings reuses it.
interface DismissedResponse {
  unknown: string[];
}

// MigrationScreen is the §5.2 Migration view: scan-driven grouping of
// MCP server entries across all four supported clients, with per-group
// actions (Demigrate in Task 6; Migrate selected + Dismiss + gated
// Create-manifest in Task 7; Per-session readonly in Task 8). This
// scaffolding ships h1, parallel /api/scan + /api/dismissed fetches,
// groupMigrationEntries wiring with the dismissed-unknowns filter,
// empty-state copy, and the per-group scaffolding component so the
// route + router are testable end-to-end before the action handlers land.
export function MigrationScreen() {
  const [scan, setScan] = useState<ScanResult | null>(null);
  const [dismissedUnknown, setDismissedUnknown] = useState<Set<string>>(new Set());
  const [error, setError] = useState<string | null>(null);
  const [scanReloadToken, setScanReloadToken] = useState(0);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        // Parallel fetch — both endpoints are idempotent and
        // independent; round-trips run concurrently.
        const [s, d] = await Promise.all([
          fetchOrThrow<ScanResult>("/api/scan", "object"),
          fetchOrThrow<DismissedResponse>("/api/dismissed", "object"),
        ]);
        if (!cancelled) {
          setScan(s);
          setDismissedUnknown(new Set(d.unknown ?? []));
          setError(null);
        }
      } catch (err) {
        if (!cancelled) setError((err as Error).message);
      }
    })();
    return () => { cancelled = true; };
  }, [scanReloadToken]);

  const groups: MigrationGroups = scan
    ? groupMigrationEntries(scan, dismissedUnknown)
    : { viaHub: [], canMigrate: [], unknown: [], perSession: [] };

  if (error) {
    return (
      <section class="screen migration">
        <h1>Migration</h1>
        <p class="error">{error}</p>
      </section>
    );
  }
  if (scan == null) {
    return (
      <section class="screen migration">
        <h1>Migration</h1>
        <p>Loading…</p>
      </section>
    );
  }

  const totalRows =
    groups.viaHub.length +
    groups.canMigrate.length +
    groups.unknown.length +
    groups.perSession.length;

  return (
    <section class="screen migration">
      <h1>Migration</h1>
      {totalRows === 0 ? (
        <p class="empty-state">
          No MCP servers found across any client config. Install or configure
          an MCP server in Claude Code, Codex CLI, Gemini CLI, or Antigravity
          to see it here.
        </p>
      ) : (
        <>
          <GroupSection
            title="Via hub"
            tone="via-hub"
            entries={groups.viaHub}
            emptyLabel="No hub-routed entries yet."
          />
          <GroupSection
            title="Can migrate"
            tone="can-migrate"
            entries={groups.canMigrate}
            emptyLabel="No stdio entries with matching manifests."
          />
          <GroupSection
            title="Unknown"
            tone="unknown"
            entries={groups.unknown}
            emptyLabel="No unknown stdio entries."
          />
          <GroupSection
            title="Per-session"
            tone="per-session"
            entries={groups.perSession}
            emptyLabel="No per-session entries."
          />
        </>
      )}
      <button
        type="button"
        class="rescan"
        onClick={() => setScanReloadToken((n) => n + 1)}
      >
        Rescan
      </button>
    </section>
  );
}

// GroupSection is a minimal per-group row-list renderer shared by the
// scaffolding. Task 6 replaces the Via-hub call with ViaHubGroup
// (Demigrate per row); Task 7 replaces Can-migrate / Unknown with
// CanMigrateGroup (pre-checked + Migrate-selected) and UnknownGroup
// (disabled Create-manifest + Dismiss); Task 8 replaces Per-session
// with PerSessionGroup and removes this generic renderer.
function GroupSection(props: {
  title: string;
  tone: "via-hub" | "can-migrate" | "unknown" | "per-session";
  entries: Array<{ name: string }>;
  emptyLabel: string;
}) {
  return (
    <section class={`group group-${props.tone}`} data-group={props.tone}>
      <h2>{props.title}</h2>
      {props.entries.length === 0 ? (
        <p class="empty">{props.emptyLabel}</p>
      ) : (
        <ul class="group-rows">
          {props.entries.map((e) => (
            <li key={e.name} data-server={e.name}>
              <span class="server-name">{e.name}</span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
```

Also append the group-tone CSS rules to `internal/gui/frontend/src/styles/style.css` (spec §5.2 requires green / yellow / purple / gray group semantics — without these the four sections render indistinguishably). Use left-border tone rather than full backgrounds so contrast stays WCAG-AA under both light and dark themes (matches the existing `--success` / `--danger` token usage for daemon state):

```css
/* Migration screen group tones (spec §5.2) */
.screen.migration .group { margin: 1.25rem 0; padding: 0.75rem 1rem;
    border-left: 4px solid var(--border, #d0d7de); border-radius: 4px; }
.screen.migration .group > h2 { margin: 0 0 0.5rem; font-size: 1.05rem; }
.screen.migration .group > .empty { color: var(--text-muted, #656d76);
    font-style: italic; }
.screen.migration .group-via-hub     { border-left-color: var(--success, #1a7f37); }
.screen.migration .group-can-migrate { border-left-color: var(--warning, #bf8700); }
.screen.migration .group-unknown     { border-left-color: var(--accent, #8250df); }
.screen.migration .group-per-session { border-left-color: var(--text-muted, #656d76); }
.screen.migration .group-rows { list-style: none; padding: 0; margin: 0; }
.screen.migration .group-rows li {
    display: flex; align-items: center; gap: 0.5rem;
    padding: 0.25rem 0; border-bottom: 1px solid var(--border-subtle, #e4e7eb);
}
.screen.migration .group-rows li:last-child { border-bottom: none; }
.screen.migration .server-name { flex: 1 1 auto; font-family: var(--font-mono); }
.screen.migration .action-error { color: var(--danger, #cf222e); }
.screen.migration button[disabled] { opacity: 0.5; cursor: not-allowed; }
```

If the repo's CSS already defines `--warning` and `--accent` tokens, those are preferred; the inline fallback colors keep the rule working on themes that do not set them.

### Step 3: Wire route + sidebar link in `app.tsx` and update shell E2E

Replace `internal/gui/frontend/src/app.tsx`:

```tsx
import type { JSX } from "preact";
import { useRouter } from "./hooks/useRouter";
import { DashboardScreen } from "./screens/Dashboard";
import { LogsScreen } from "./screens/Logs";
import { MigrationScreen } from "./screens/Migration";
import { ServersScreen } from "./screens/Servers";

const SCREENS: Record<string, () => JSX.Element> = {
  servers: () => <ServersScreen />,
  migration: () => <MigrationScreen />,
  dashboard: () => <DashboardScreen />,
  logs: () => <LogsScreen />,
};

export function App() {
  const screen = useRouter("servers");
  const Render = SCREENS[screen];
  return (
    <>
      <aside class="sidebar">
        <div class="brand">mcp-local-hub</div>
        <nav>
          <a href="#/servers" class={screen === "servers" ? "active" : ""}>
            Servers
          </a>
          <a href="#/migration" class={screen === "migration" ? "active" : ""}>
            Migration
          </a>
          <a href="#/dashboard" class={screen === "dashboard" ? "active" : ""}>
            Dashboard
          </a>
          <a href="#/logs" class={screen === "logs" ? "active" : ""}>
            Logs
          </a>
        </nav>
      </aside>
      <main id="screen-root">
        {Render ? <Render /> : <p>Unknown screen: {screen}</p>}
      </main>
    </>
  );
}
```

Replace `internal/gui/e2e/tests/shell.spec.ts`:

```ts
import { test, expect } from "../fixtures/hub";

test.describe("shell", () => {
  test("renders sidebar with brand + four nav links", async ({ page, hub }) => {
    await page.goto(`${hub.url}/`);
    await expect(page.locator(".sidebar .brand")).toHaveText("mcp-local-hub");
    const links = page.locator(".sidebar nav a");
    await expect(links).toHaveCount(4);
    await expect(links.nth(0)).toHaveText("Servers");
    await expect(links.nth(1)).toHaveText("Migration");
    await expect(links.nth(2)).toHaveText("Dashboard");
    await expect(links.nth(3)).toHaveText("Logs");
  });

  test("default route is Servers and nav highlights on click", async ({ page, hub }) => {
    await page.goto(`${hub.url}/`);
    const serversLink = page.locator(".sidebar nav a", { hasText: "Servers" });
    await expect(serversLink).toHaveClass(/active/);
    await page.locator(".sidebar nav a", { hasText: "Migration" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Migration" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Migration");
    await page.locator(".sidebar nav a", { hasText: "Dashboard" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Dashboard" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Dashboard");
    await page.locator(".sidebar nav a", { hasText: "Logs" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Logs" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Logs");
  });

  test("hashchange triggers screen swap (browser back/forward)", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/dashboard`);
    await expect(page.locator("h1")).toHaveText("Dashboard");
    await page.goto(`${hub.url}/#/migration`);
    await expect(page.locator("h1")).toHaveText("Migration");
    await page.goto(`${hub.url}/#/logs`);
    await expect(page.locator("h1")).toHaveText("Logs");
    await page.goBack();
    await expect(page.locator("h1")).toHaveText("Migration");
  });
});
```

### Step 4: Rebuild + typecheck + Vitest + Go smoke + E2E

```bash
go generate ./internal/gui/...
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../../.. && go test ./internal/gui/ -count=1
cd internal/gui/e2e && npm test -- tests/shell.spec.ts
```

Expected: typecheck clean, Vitest all pass, Go GUI smoke pass, shell E2E pass (3/3).

### Step 5: Commit

```bash
git add internal/gui/frontend/src/api.ts \
        internal/gui/frontend/src/screens/Migration.tsx \
        internal/gui/frontend/src/app.tsx \
        internal/gui/e2e/tests/shell.spec.ts \
        internal/gui/assets/
git commit -m "feat(gui/frontend): Migration scaffolding + route + sidebar + postDismiss helper

Adds #/migration route positioned between Servers and Dashboard, a
scaffolded MigrationScreen that fetches /api/scan and renders the
four group sections, and a postDismiss API client. Shell E2E updated
for the fourth nav link and the new hashchange path. Action handlers
(Demigrate, Migrate selected, Dismiss) ship in Tasks 6-8."
```

---

## Task 6: Via-hub group — per-row Demigrate button

**Files:**
- Modify: `internal/gui/frontend/src/screens/Migration.tsx`

### Step 1: Add Demigrate state + handler + ViaHubGroup component

Replace the `MigrationScreen` + `GroupSection` pair in `Migration.tsx` with the following. Keep the top-of-file imports (`useEffect`, `useState`, `fetchOrThrow`, `groupMigrationEntries`, etc.) and ADD the `ScanEntry` import plus the action handler inline.

Update the imports at the top:

```tsx
import { useEffect, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
import { groupMigrationEntries, type MigrationGroups } from "../lib/migration-grouping";
import type { ScanEntry, ScanResult } from "../types";
```

Replace the function body of `MigrationScreen` (everything from `export function MigrationScreen() {` to its closing `}`). The parallel scan + dismissed-list fetch introduced in Task 5 Step 2 MUST be preserved here — dropping it would silently lose the R2 dismissal wiring and call `groupMigrationEntries(scan)` with the wrong arity:

```tsx
export function MigrationScreen() {
  const [scan, setScan] = useState<ScanResult | null>(null);
  const [dismissedUnknown, setDismissedUnknown] = useState<Set<string>>(new Set());
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [actionBusy, setActionBusy] = useState<string | null>(null); // server name being demigrated
  const [scanReloadToken, setScanReloadToken] = useState(0);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [s, d] = await Promise.all([
          fetchOrThrow<ScanResult>("/api/scan", "object"),
          fetchOrThrow<DismissedResponse>("/api/dismissed", "object"),
        ]);
        if (!cancelled) {
          setScan(s);
          setDismissedUnknown(new Set(d.unknown ?? []));
          setError(null);
        }
      } catch (err) {
        if (!cancelled) setError((err as Error).message);
      }
    })();
    return () => { cancelled = true; };
  }, [scanReloadToken]);

  async function runDemigrate(serverName: string) {
    setActionBusy(serverName);
    setActionError(null);
    try {
      const resp = await fetch("/api/demigrate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ servers: [serverName] }),
      });
      if (!resp.ok && resp.status !== 204) {
        const body = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(body?.error ?? `HTTP ${resp.status}`);
      }
      setScanReloadToken((n) => n + 1);
    } catch (err) {
      setActionError(`Demigrate ${serverName}: ${(err as Error).message}`);
    } finally {
      setActionBusy(null);
    }
  }

  const groups: MigrationGroups = scan
    ? groupMigrationEntries(scan, dismissedUnknown)
    : { viaHub: [], canMigrate: [], unknown: [], perSession: [] };

  if (error) {
    return (
      <section class="screen migration">
        <h1>Migration</h1>
        <p class="error">{error}</p>
      </section>
    );
  }
  if (scan == null) {
    return (
      <section class="screen migration">
        <h1>Migration</h1>
        <p>Loading…</p>
      </section>
    );
  }

  const totalRows =
    groups.viaHub.length +
    groups.canMigrate.length +
    groups.unknown.length +
    groups.perSession.length;

  return (
    <section class="screen migration">
      <h1>Migration</h1>
      {actionError && <p class="error action-error">{actionError}</p>}
      {totalRows === 0 ? (
        <p class="empty-state">
          No MCP servers found across any client config. Install or configure
          an MCP server in Claude Code, Codex CLI, Gemini CLI, or Antigravity
          to see it here.
        </p>
      ) : (
        <>
          <ViaHubGroup
            entries={groups.viaHub}
            actionBusy={actionBusy}
            onDemigrate={runDemigrate}
          />
          <GroupSection
            title="Can migrate"
            tone="can-migrate"
            entries={groups.canMigrate}
            emptyLabel="No stdio entries with matching manifests."
          />
          <GroupSection
            title="Unknown"
            tone="unknown"
            entries={groups.unknown}
            emptyLabel="No unknown stdio entries."
          />
          <GroupSection
            title="Per-session"
            tone="per-session"
            entries={groups.perSession}
            emptyLabel="No per-session entries."
          />
        </>
      )}
      <button
        type="button"
        class="rescan"
        onClick={() => setScanReloadToken((n) => n + 1)}
      >
        Rescan
      </button>
    </section>
  );
}

function ViaHubGroup(props: {
  entries: ScanEntry[];
  actionBusy: string | null;
  onDemigrate: (server: string) => void;
}) {
  if (props.entries.length === 0) {
    return (
      <section class="group group-via-hub" data-group="via-hub">
        <h2>Via hub</h2>
        <p class="empty">No hub-routed entries yet.</p>
      </section>
    );
  }
  return (
    <section class="group group-via-hub" data-group="via-hub">
      <h2>Via hub</h2>
      <ul class="group-rows">
        {props.entries.map((e) => (
          <li key={e.name} data-server={e.name}>
            <span class="server-name">{e.name}</span>
            <button
              type="button"
              class="demigrate"
              data-action="demigrate"
              disabled={props.actionBusy != null}
              onClick={() => props.onDemigrate(e.name)}
            >
              {props.actionBusy === e.name ? "Demigrating…" : "Demigrate"}
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}
```

Keep the existing `GroupSection` component at the bottom of the file — it's still used by Can-migrate / Unknown / Per-session until Tasks 7/8 replace them.

### Step 2: Rebuild + typecheck + Go smoke

```bash
go generate ./internal/gui/...
cd internal/gui/frontend && npm run typecheck
cd ../../.. && go test ./internal/gui/ -count=1
```

### Step 3: Commit

```bash
git add internal/gui/frontend/src/screens/Migration.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): Via-hub group with per-row Demigrate action

Each hub-routed row gets a Demigrate button that POSTs /api/demigrate
{servers:[name]} and triggers a fresh parallel /api/scan +
/api/dismissed refetch on success via scanReloadToken. Failures
surface in a top-level action-error banner without losing the rest
of the render. actionBusy disables all Demigrate buttons while one
is in flight to avoid racing concurrent rollbacks. Preserves the
parallel fetch + dismissedUnknown state + two-argument
groupMigrationEntries(scan, dismissedUnknown) wiring from Task 5."
```

---

## Task 7: Can-migrate group — checkboxes + Migrate selected + Unknown group (Dismiss + disabled Create manifest)

This task bundles the two remaining action-bearing groups (Can-migrate and Unknown) because they share state wiring (`selected`, `migrateBusy`, the Dismiss POST path) and individually each is tiny; splitting them inflates review overhead without improving testability.

**Files:**
- Modify: `internal/gui/frontend/src/screens/Migration.tsx`

### Step 1: Extend the screen

Add the `postDismiss` import (from `../api`) alongside `fetchOrThrow`:

```tsx
import { fetchOrThrow, postDismiss } from "../api";
```

Inside `MigrationScreen`, after the existing `scanReloadToken` state, add:

```tsx
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [migrateBusy, setMigrateBusy] = useState<boolean>(false);
```

Inside the existing `useEffect` that fetches scan, replace the `setScan(s); setError(null);` pair with:

```tsx
          setScan(s);
          setError(null);
          const canMigrateNames = (s.entries ?? [])
            .filter((e) => e.status === "can-migrate")
            .map((e) => e.name);
          setSelected(new Set(canMigrateNames));
```

Add the two new handlers inside `MigrationScreen`, below `runDemigrate`:

```tsx
  async function runMigrateSelected() {
    if (selected.size === 0) return;
    setMigrateBusy(true);
    setActionError(null);
    try {
      const resp = await fetch("/api/migrate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ servers: [...selected] }),
      });
      if (!resp.ok && resp.status !== 204) {
        const body = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(body?.error ?? `HTTP ${resp.status}`);
      }
      setScanReloadToken((n) => n + 1);
    } catch (err) {
      setActionError(`Migrate selected: ${(err as Error).message}`);
    } finally {
      setMigrateBusy(false);
    }
  }

  function toggleSelected(name: string, next: boolean) {
    setSelected((prev) => {
      const s = new Set(prev);
      if (next) s.add(name);
      else s.delete(name);
      return s;
    });
  }

  async function runDismiss(entry: ScanEntry) {
    setActionError(null);
    try {
      await postDismiss(entry.name);
      setScanReloadToken((n) => n + 1);
    } catch (err) {
      setActionError(`Dismiss ${entry.name}: ${(err as Error).message}`);
    }
  }
```

Replace the two `<GroupSection title="Can migrate" ... />` and `<GroupSection title="Unknown" ... />` calls with:

```tsx
          <CanMigrateGroup
            entries={groups.canMigrate}
            selected={selected}
            onToggle={toggleSelected}
            onMigrateSelected={runMigrateSelected}
            migrateBusy={migrateBusy}
          />
          <UnknownGroup
            entries={groups.unknown}
            onDismiss={runDismiss}
          />
```

Add the two new renderer components below `ViaHubGroup`:

```tsx
function CanMigrateGroup(props: {
  entries: ScanEntry[];
  selected: Set<string>;
  onToggle: (name: string, next: boolean) => void;
  onMigrateSelected: () => void;
  migrateBusy: boolean;
}) {
  if (props.entries.length === 0) {
    return (
      <section class="group group-can-migrate" data-group="can-migrate">
        <h2>Can migrate</h2>
        <p class="empty">No stdio entries with matching manifests.</p>
      </section>
    );
  }
  const selectedInGroup = props.entries.filter((e) => props.selected.has(e.name)).length;
  return (
    <section class="group group-can-migrate" data-group="can-migrate">
      <h2>Can migrate</h2>
      <ul class="group-rows">
        {props.entries.map((e) => (
          <li key={e.name} data-server={e.name}>
            <label>
              <input
                type="checkbox"
                data-action="select"
                checked={props.selected.has(e.name)}
                onChange={(ev) =>
                  props.onToggle(e.name, (ev.currentTarget as HTMLInputElement).checked)
                }
              />
              <span class="server-name">{e.name}</span>
            </label>
          </li>
        ))}
      </ul>
      <button
        type="button"
        class="migrate-selected"
        data-action="migrate-selected"
        disabled={selectedInGroup === 0 || props.migrateBusy}
        onClick={props.onMigrateSelected}
      >
        {props.migrateBusy ? "Migrating…" : `Migrate selected (${selectedInGroup})`}
      </button>
    </section>
  );
}

function UnknownGroup(props: {
  entries: ScanEntry[];
  onDismiss: (entry: ScanEntry) => void;
}) {
  if (props.entries.length === 0) {
    return (
      <section class="group group-unknown" data-group="unknown">
        <h2>Unknown</h2>
        <p class="empty">No unknown stdio entries.</p>
      </section>
    );
  }
  return (
    <section class="group group-unknown" data-group="unknown">
      <h2>Unknown</h2>
      <ul class="group-rows">
        {props.entries.map((e) => (
          <li key={e.name} data-server={e.name}>
            <span class="server-name">{e.name}</span>
            <button
              type="button"
              class="create-manifest"
              data-action="create-manifest"
              disabled
              title="Available after A2 (Add/Edit manifest) ships"
            >
              Create manifest
            </button>
            <button
              type="button"
              class="dismiss"
              data-action="dismiss"
              onClick={() => props.onDismiss(e)}
            >
              Dismiss
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}
```

### Step 2: Rebuild + typecheck + Vitest

```bash
go generate ./internal/gui/...
cd internal/gui/frontend && npm run typecheck && npm run test
```

### Step 3: Self-review checklist

- `checked={...}` (controlled), not `defaultChecked` — selection survives re-renders.
- `selectedInGroup` counts only rows currently in the group, so `selected` never lies about ghosts.
- `Migrate selected` is disabled when `selectedInGroup === 0` — empty POST is guarded.
- Dismiss ALWAYS goes through `postDismiss` (backend-persisted); no localStorage leftovers.
- Dismiss triggers both a scan refetch AND a /api/dismissed refetch (the state is in `scanReloadToken` — the same useEffect fetches both); `groupMigrationEntries` filters the unknown group client-side against the refreshed dismissed set.
- `Create manifest` is DOM-`disabled`, not just styled — keyboard-enter is blocked.
- `title` attribute provides the tooltip ("Available after A2…") that screen readers can announce.

### Step 4: Go smoke

```bash
cd ../../.. && go test ./internal/gui/ -count=1
```

### Step 5: Commit

```bash
git add internal/gui/frontend/src/screens/Migration.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): Can-migrate batch action + Unknown Dismiss (backend) + gated Create-manifest

Can-migrate rows have pre-checked selection + 'Migrate selected (N)'
button; selection re-seeds after each scan refetch to avoid stale
ghosts. Unknown rows have DOM-disabled 'Create manifest' with an
A2-reference tooltip + 'Dismiss' that POSTs /api/dismiss and
triggers a fresh /api/scan + /api/dismissed (client-side filter in
groupMigrationEntries hides the row from Unknown on the next
render)."
```

---

## Task 8: Per-session group readonly + delete dead GroupSection

**Files:**
- Modify: `internal/gui/frontend/src/screens/Migration.tsx`

### Step 1: Replace the Per-session GroupSection call

Replace the `<GroupSection title="Per-session" ... />` call in `MigrationScreen` with:

```tsx
          <PerSessionGroup entries={groups.perSession} />
```

Add the renderer below `UnknownGroup`:

```tsx
function PerSessionGroup(props: { entries: ScanEntry[] }) {
  if (props.entries.length === 0) {
    return (
      <section class="group group-per-session" data-group="per-session">
        <h2>Per-session</h2>
        <p class="empty">No per-session entries.</p>
      </section>
    );
  }
  return (
    <section class="group group-per-session" data-group="per-session">
      <h2>Per-session</h2>
      <p class="info">
        These entries are shareable per-session only (e.g. running IDE
        integrations). They cannot be migrated into the hub and do not
        support Demigrate.
      </p>
      <ul class="group-rows">
        {props.entries.map((e) => (
          <li key={e.name} data-server={e.name}>
            <span class="server-name">{e.name}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}
```

After replacing, the generic `GroupSection` component at the bottom of the file is no longer referenced — DELETE it. All four groups now have dedicated renderers.

### Step 2: Rebuild + typecheck + Vitest + Go smoke

```bash
go generate ./internal/gui/...
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../../.. && go test ./internal/gui/ -count=1
```

Expected: all green; typecheck confirms no references to `GroupSection` remain.

### Step 3: Commit

```bash
git add internal/gui/frontend/src/screens/Migration.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): Per-session group readonly + drop dead GroupSection

Per-session rows get a dedicated renderer that prefixes the list
with an explanatory note (why no actions). The generic GroupSection
had no remaining callers after Tasks 6-7 — removed to keep
Migration.tsx focused."
```

---

## Task 9: SSE refresh on scan-related events

**Files:**
- Modify: `internal/gui/frontend/src/screens/Migration.tsx`

### Step 1: Verify which SSE event names exist

Identify the event names the hub broadcaster actually publishes:

```bash
grep -n "Publish\|sse.Event" internal/gui/*.go | head
```

If specific `scan-changed` / `migrate-applied` / `demigrate-applied` / `dismiss-applied` names are not present, the event bus currently publishes only `daemon-state` (the Dashboard's existing subscription). Subscribe to the narrowest common superset actually emitted.

### Step 2: Wire `useEventSource`

Add the import at the top of `Migration.tsx`:

```tsx
import { useEventSource } from "../hooks/useEventSource";
```

Inside `MigrationScreen`, after the existing scan-fetch `useEffect`, add:

```tsx
  // SSE refresh: any out-of-band change (another GUI tab migrated, CLI
  // ran on this machine, user hand-edited .claude.json) should refresh
  // the view. Migrate/Demigrate/Dismiss local actions already bump
  // scanReloadToken on success; SSE covers the rest. Event names here
  // are whatever the hub broadcaster (internal/gui/events.go) actually
  // emits — keep the subscription narrow so unknown events do not cause
  // pointless rescans.
  useEventSource("/api/events", {
    "daemon-state": () => setScanReloadToken((n) => n + 1),
  });
```

If the broadcaster DOES publish scan/migrate/demigrate/dismiss events by the time this task runs, expand the handler map with one entry per event name, each calling the same `setScanReloadToken((n) => n + 1)` refresh.

### Step 3: Rebuild + typecheck + Vitest + Go smoke

```bash
go generate ./internal/gui/...
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../../.. && go test ./internal/gui/ -count=1
```

### Step 4: Commit

```bash
git add internal/gui/frontend/src/screens/Migration.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): Migration subscribes to scan-refresh SSE events

Out-of-band scan mutations (another GUI tab, CLI migrate, manual
edits) refresh the view automatically. Uses the existing
useEventSource hook so cleanup on unmount is guaranteed."
```

---

## Task 10: Playwright E2E — empty-state + route + Dismiss round-trip

**Files:**
- Create: `internal/gui/e2e/tests/migration.spec.ts`

### Step 1: Write E2E tests

Create `internal/gui/e2e/tests/migration.spec.ts`:

```ts
import { test, expect } from "../fixtures/hub";
import { readFileSync, existsSync } from "node:fs";
import { join } from "node:path";

test.describe("Migration screen", () => {
  test("renders h1 + empty-state copy on fresh tmp home", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/migration`);
    await expect(page.locator("h1")).toHaveText("Migration");
    await expect(page.locator(".empty-state")).toContainText("No MCP servers found");
  });

  test("Rescan button is present and clickable on empty home", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/migration`);
    const rescan = page.locator("button.rescan", { hasText: "Rescan" });
    await expect(rescan).toBeVisible();
    await rescan.click();
    await expect(page.locator(".empty-state")).toBeVisible();
  });

  test("group sections are not rendered when total row count is zero", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/migration`);
    await expect(page.locator('[data-group]')).toHaveCount(0);
  });

  test("hashchange from Servers to Migration swaps h1", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/servers`);
    await expect(page.locator("h1")).toHaveText("Servers");
    await page.locator(".sidebar nav a", { hasText: "Migration" }).click();
    await expect(page.locator("h1")).toHaveText("Migration");
  });

  test("POST /api/dismiss → GET /api/dismissed → on-disk JSON all agree", async ({
    page,
    hub,
  }) => {
    // The hub fixture at internal/gui/e2e/fixtures/hub.ts:46 sets
    // LOCALAPPDATA=<home>, so api.dismiss.go's dismissedFilePath
    // resolves to <home>/mcp-local-hub/gui-dismissed.json. Three
    // assertions together prove the full round-trip on a real
    // spawned binary:
    //   (a) POST /api/dismiss returns 204
    //   (b) The JSON file on disk includes the name with version=1
    //   (c) GET /api/dismissed returns the same name in its list
    const resp = await page.request.post(`${hub.url}/api/dismiss`, {
      data: { server: "synthetic-dismissed-e2e" },
      headers: { "Content-Type": "application/json" },
    });
    expect(resp.status()).toBe(204);

    const dismissedPath = join(hub.home, "mcp-local-hub", "gui-dismissed.json");
    expect(existsSync(dismissedPath)).toBe(true);
    const raw = readFileSync(dismissedPath, "utf-8");
    const parsed = JSON.parse(raw) as { version: number; unknown: string[] };
    expect(parsed.version).toBe(1);
    expect(parsed.unknown).toContain("synthetic-dismissed-e2e");

    // GET /api/dismissed should return what we just wrote. This is
    // the endpoint Migration screen consumes in Task 5.
    const list = await page.request.get(`${hub.url}/api/dismissed`);
    expect(list.status()).toBe(200);
    const listBody = (await list.json()) as { unknown: string[] };
    expect(Array.isArray(listBody.unknown)).toBe(true);
    expect(listBody.unknown).toContain("synthetic-dismissed-e2e");
  });

  test("/api/scan remains unfiltered by dismissals (Servers-matrix invariant)", async ({
    page,
    hub,
  }) => {
    // Regression guard: Servers matrix (via collectServers) consumes
    // every /api/scan entry without status inspection, so dismissing
    // an unknown entry MUST NOT hide it from /api/scan. We prove this
    // by seeding a real unknown stdio entry in ~/.claude.json that
    // the hub's scanner will classify as unknown (no matching
    // manifest), POSTing /api/dismiss for its exact name, and then
    // asserting /api/scan still includes that name. Without this
    // assertion a future regression (someone re-adding a server-side
    // filter to /api/scan) would silently pass every other test.
    const claudePath = join(hub.home, ".claude.json");
    writeFileSync(
      claudePath,
      JSON.stringify({
        mcpServers: {
          "e2e-unknown-guard": {
            type: "stdio",
            command: "npx",
            args: ["-y", "e2e-unknown-guard"],
          },
        },
      }),
      "utf-8",
    );

    // Pre-check: /api/scan should now show the seeded unknown entry.
    const preScan = await page.request.get(`${hub.url}/api/scan`);
    expect(preScan.status()).toBe(200);
    const preBody = (await preScan.json()) as {
      entries: Array<{ name: string; status?: string }> | null;
    };
    const preNames = (preBody.entries ?? []).map((e) => e.name);
    expect(preNames).toContain("e2e-unknown-guard");

    // Dismiss that name.
    const dismiss = await page.request.post(`${hub.url}/api/dismiss`, {
      data: { server: "e2e-unknown-guard" },
      headers: { "Content-Type": "application/json" },
    });
    expect(dismiss.status()).toBe(204);

    // /api/scan must STILL contain the name. Filtering moved
    // client-side in R2; /api/scan is shared with Servers and must
    // stay unfiltered.
    const postScan = await page.request.get(`${hub.url}/api/scan`);
    expect(postScan.status()).toBe(200);
    const postBody = (await postScan.json()) as {
      entries: Array<{ name: string; status?: string }> | null;
    };
    const postNames = (postBody.entries ?? []).map((e) => e.name);
    expect(postNames).toContain("e2e-unknown-guard");
  });
});
```

Add `writeFileSync` to the `node:fs` import at the top of the file:

```ts
import { readFileSync, existsSync, writeFileSync } from "node:fs";
```

### Step 2: Run E2E

```bash
cd internal/gui/e2e && npm test -- tests/migration.spec.ts
```

Expected: 6/6 PASS.

### Step 3: Full E2E suite re-run

```bash
cd internal/gui/e2e && npm test
```

Expected: 17 tests total (3 shell + 3 servers + 2 dashboard + 3 logs + 6 migration). All PASS.

### Step 4: Commit

```bash
git add internal/gui/e2e/tests/migration.spec.ts
git commit -m "test(gui/e2e): Migration empty-state + rescan + dismiss round-trip

Six Playwright tests cover the shipped-without-client-configs path,
a full POST /api/dismiss → on-disk JSON → GET /api/dismissed
round-trip that exercises the backend persistence end-to-end on a
real spawned binary, AND a dedicated /api/scan-unfiltered
regression guard that seeds a real unknown stdio entry in
<hub.home>/.claude.json, dismisses it, and asserts /api/scan still
returns the entry (documented invariant: /api/scan stays shared
with Servers; dismissal is a Migration-only client-side filter)."
```

---

## Task 11: Full-suite smoke + CLAUDE.md update

**Files:**
- Modify: `CLAUDE.md`

### Step 1: Full Go + frontend + E2E suite

```bash
cd D:/dev/mcp-local-hub
go build ./...
go test ./... -count=1
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../e2e && npm test
```

Expected:
- `go build ./...` clean
- All 12 Go packages PASS
- Vitest suite all PASS
- Playwright 17/17 PASS (3 shell + 3 servers + 6 migration + 2 dashboard + 3 logs)

Pre-existing flakes (`internal/daemon/TestHostStopUnblocksPendingHandlers`, `internal/api/TestInstallAllInstallsEverything` port collision) are not regressions — re-run once to confirm.

### Step 2: Update CLAUDE.md

In `CLAUDE.md`, find the `### What's covered` section under `## GUI E2E tests (Phase 3B-II onward)` and replace:

```
### What's covered

- Shell: sidebar, three nav links, hash routing, active-link highlight.
- Servers: matrix columns (Server + 4 clients + Port + State), empty-body state on clean tmpHome, Apply disabled with no dirty cells.
- Dashboard: empty-cards state on fresh home, `/api/events` SSE connection opens on mount.
- Logs: picker + controls render, notice text on no-daemons state, controls disabled when no eligible entries.

11 smoke tests total (3 shell + 3 servers + 2 dashboard + 3 logs), ~8s
wall-time on a warm machine.
```

with:

```
### What's covered

- Shell: sidebar, four nav links, hash routing, active-link highlight.
- Servers: matrix columns (Server + 4 clients + Port + State), empty-body state on clean tmpHome, Apply disabled with no dirty cells.
- Migration: h1, empty-state copy, group sections hidden on empty home, hashchange swap from Servers, full POST /api/dismiss → on-disk JSON → GET /api/dismissed round-trip, /api/scan-unfiltered regression guard (seed + dismiss + re-scan).
- Dashboard: empty-cards state on fresh home, `/api/events` SSE connection opens on mount.
- Logs: picker + controls render, notice text on no-daemons state, controls disabled when no eligible entries.

17 smoke tests total (3 shell + 3 servers + 6 migration + 2 dashboard + 3 logs), ~10s
wall-time on a warm machine.
```

### Step 3: Final commit

```bash
git add CLAUDE.md
git commit -m "docs: CLAUDE.md reflects Migration screen E2E coverage

Four nav links, six new Migration tests (incl. /api/scan-unfiltered
regression guard), updated total (11→17) and wall-time (~8s→~10s)."
```

### Step 4: Verify PR-ready state

```bash
git log master..HEAD --oneline
```

Expected: 11 commits — one per task. `git status` clean.

### Step 5: Hand off — DO NOT commit further. Proceed to Codex CLI branch review → PR open → GitHub Codex review cycle → merge, per overnight plan rules.

---

## Dependency order summary

Task 1 (Demigrate HTTP handler) → Task 2 (DismissUnknown storage) → Task 3 (POST /api/dismiss + GET /api/dismissed handlers; depends on Task 2's storage) → Task 4 (grouping helper; pure, independent of Tasks 1-3 code-wise but sequenced so frontend work starts after backend is ready) → Task 5 (scaffolding, postDismiss helper, parallel scan + dismissed fetch, route, sidebar, CSS) → Task 6 (Via-hub + Demigrate) → Task 7 (Can-migrate batch + Unknown Dismiss + gated Create-manifest; depends on Task 5's postDismiss + Task 3's `/api/dismiss`) → Task 8 (Per-session readonly) → Task 9 (SSE refresh) → Task 10 (Playwright) → Task 11 (full-suite smoke + docs).

- Task 1 must come first among handlers so Task 6's Demigrate button has an endpoint to POST against.
- Task 2 must precede Task 3 (the HTTP handlers depend on the storage helpers).
- Task 4's pure grouping helper can technically be done any time; placed here so frontend flows start after all backend surface is green.
- Task 5 establishes the route, scaffolding, and POST helper that Tasks 6-9 extend.
- Tasks 6-8 extend the same file (`Migration.tsx`) in distinct sections; order is fixed for reviewability.
- Task 10 depends on all previous screen tasks so the DOM shape is stable.
- Task 11 is verification + docs only; no new code.

**Estimated scope:** ~1000-1200 LOC added (Go handlers + storage ~250, frontend source ~550, Vitest ~100, Playwright ~100, tests+docs the rest). 11 commits. Budget ~5-7 hours of subagent-driven execution given the strict review discipline established by PR #3 (10 review rounds total).

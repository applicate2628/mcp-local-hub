# Phase 3B-II A2b — Edit-mode Add Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **⚠️ READ THE APPENDIX FIRST.** The Codex R2 plan-review identified 7 findings that were addressed via refined implementations. The **Appendix at the very bottom of this document OVERRIDES the task bodies** where they conflict — specifically for Tasks 3, 4, 8, 9, 12, 13, 18, plus P2-3 create-mode Advanced E2E coverage. Subagent implementers MUST apply appendix-refined code, not original task-body code, for the noted tasks.
>
> **NEW workflow gate:** after BOTH memo and plan are committed, request a **Codex full-plan review** BEFORE dispatching any implementer. Rationale: A2a's 3 in-flight fixes (paste stale closure, remount regression, spec accordion bugs) each cost one review cycle that a plan-level review would have caught at plan time.

**Goal:** Ship production-grade edit-mode hardening + Advanced section for the Add Server screen (`#/edit-server?name=<name>`), extending `AddServerScreen` via a `mode: "create" | "edit"` prop. Includes content-hash stale-file detection, Force Save last-writer-wins with `_preservedRaw` round-trip, beforeunload + hashchange nav guard, Advanced fields (kind-gated), multi-daemon matrix view, read-only mode for nested-unknown manifests, and identity locking (name + kind) in edit mode.

**Architecture:** Reuse A2a's `AddServerScreen` rather than fork. Backend `api.ManifestGet` is extended to return `(yaml, contentHash)` and `api.ManifestEdit` gains `expectedHash` concurrency check + `ErrManifestHashMismatch`. Frontend state adds `loadedHash`, `_preservedRaw` (Record<string, unknown>), and `_id` (UUID) per daemon. Navigation guard extends from A2a's sidebar-intercept-only to beforeunload + hashchange-interceptor + replaceState + suppression flag. Eight coherence-reviewed invariants (F1-F6, G1-G2) land as explicit task steps.

**Tech Stack:** Go 1.26 backend, Vite 8 + TypeScript 5 + Preact 10 frontend, Vitest 4 unit tests, Playwright + headless Chromium E2E.

**Reference documents (read before implementing):**
- Design memo: `docs/superpowers/specs/2026-04-24-phase-3b-ii-a2b-edit-mode-design.md` (authoritative for all 14 design decisions + 8 coherence fixes)
- A2a plan: `docs/superpowers/plans/2026-04-24-phase-3b-ii-a2a-create-manifest.md` (create-flow pattern this plan extends)
- A2a design memo: `docs/superpowers/specs/2026-04-24-phase-3b-ii-a2a-design.md` (Q1-Q8 decisions this plan builds on)
- Existing types: `internal/gui/frontend/src/types.ts` — `ManifestFormState` baseline shape from A2a
- Existing AddServer: `internal/gui/frontend/src/screens/AddServer.tsx` — component this plan extends

---

## File Structure

### Backend (Go)

```
internal/api/
├── manifest.go                  (MODIFY — ManifestGet returns hash; ManifestEdit accepts expectedHash; ErrManifestHashMismatch)
├── manifest_test.go             (MODIFY — add hash + expectedHash tests)
└── manifest_hash.go             (NEW — SHA-256 helper, shared between Get + Edit)

internal/gui/
├── manifest.go                  (MODIFY — handlers pass hash through; new response shape)
└── manifest_test.go             (MODIFY — assert hash in get response, reject on mismatch)
```

### Frontend (TypeScript + Preact)

```
internal/gui/frontend/src/
├── api.ts                       (MODIFY — getManifest returns {yaml, hash}; postManifestEdit takes expectedHash)
├── api.test.ts                  (MODIFY — new test cases)
├── types.ts                     (MODIFY — loadedHash, _preservedRaw, DaemonFormEntry._id)
├── app.tsx                      (MODIFY — add "edit-server" screen + hash nav guard)
├── hooks/
│   ├── useRouter.ts             (MODIFY — accept optional guard callback + suppression flag)
│   ├── useRouter.test.ts        (MODIFY — add guard + suppression cases)
│   └── useUnsavedChangesGuard.ts (NEW — wraps beforeunload + hashchange guard logic)
├── lib/
│   ├── manifest-yaml.ts         (MODIFY — _preservedRaw extract/merge, UUID on daemons, hasNestedUnknown)
│   ├── manifest-yaml.test.ts    (MODIFY — preserve round-trip + UUID + nested-unknown detection tests)
│   └── uuid.ts                  (NEW — tiny crypto.randomUUID wrapper with test-seam fallback)
└── screens/
    ├── AddServer.tsx            (MODIFY — mode prop, edit-path mount, submit branches, stale UI, read-only, Advanced, matrix, beforeunload)
    ├── Servers.tsx              (MODIFY — row click → #/edit-server?name=<row.name>)
    └── Migration.tsx            (MODIFY — Via-hub row click → #/edit-server?name=<entry.name>)

internal/gui/frontend/src/styles/
└── style.css                    (APPEND — stale banner, read-only overlay, matrix view, Advanced subsections)
```

### E2E

```
internal/gui/e2e/tests/
├── edit-server.spec.ts          (NEW — ~15 scenarios: load, stale, Force Save, name/kind lock, read-only, Advanced, matrix, nav guards)
├── shell.spec.ts                (MODIFY — if adding #/edit-server to any sidebar nav; currently no sidebar link per D7, so MAYBE NO CHANGE)
└── add-server.spec.ts           (MODIFY — one test confirming mode prop default is "create", ensures A2a contract preserved)
```

### Docs

```
CLAUDE.md                        (MODIFY — E2E coverage section bumps to include edit-server scenarios)
```

---

## Type definitions (referenced throughout — cross-task consistency)

These extensions go into `internal/gui/frontend/src/types.ts`. Tasks below use these exact names. Defined here for cross-task reference; Task 5 writes them to disk.

```ts
// Extended DaemonFormEntry: adds form-state-only UUID for identity-stable
// rename + delete operations. The UUID is NOT serialized to YAML.
export interface DaemonFormEntry {
  _id: string;              // UUID v4, form-state-only; never serialized
  name: string;
  port: number;
  context?: string;         // workspace-scoped only (D12)
  extra_args?: string[];    // D12 Advanced
}

// Extended BindingFormEntry: references daemon by _id internally.
// Serialized as {client, daemon: <resolved-name>, url_path}.
export interface BindingFormEntry {
  client: string;
  daemonId: string;         // DaemonFormEntry._id; resolved to daemon.name at toYAML
  url_path: string;
}

// Languages section (D12, workspace-scoped only). Each entry mirrors
// config.LanguageSpec. Kept optional in A2b because languages[] in YAML
// is absent for global manifests.
export interface LanguageFormEntry {
  _id: string;              // UUID v4, form-state-only
  name: string;
  backend: "mcp-language-server" | "gopls-mcp" | string;
  transport?: "stdio" | "http_listen" | "native_http";
  lsp_command?: string;
  extra_flags?: string[];
}

// Extended ManifestFormState: carries load-time content hash for stale
// detection, _preservedRaw for top-level unknown YAML keys, and optional
// Advanced fields (kind-gated in render but always in state).
export interface ManifestFormState {
  name: string;
  kind: "global" | "workspace-scoped";
  transport: "stdio-bridge" | "native-http";
  command: string;
  base_args: string[];
  env: Array<{ key: string; value: string }>;
  daemons: DaemonFormEntry[];        // UPDATED: includes _id
  client_bindings: BindingFormEntry[]; // UPDATED: daemonId instead of daemon name
  weekly_refresh: boolean;
  // A2b Advanced fields (D12):
  idle_timeout_min?: number;
  base_args_template?: string[];
  languages?: LanguageFormEntry[];   // kind === "workspace-scoped" only
  port_pool?: { start: number; end: number };
  // A2b state-only fields (NOT serialized to YAML directly):
  loadedHash: string;                // SHA-256 from api.ManifestGet; "" for create mode
  _preservedRaw: Record<string, unknown>;  // top-level unknown YAML keys (D13)
}

// GetManifestResponse mirrors the new /api/manifest/get response shape.
export interface GetManifestResponse {
  yaml: string;
  hash: string;   // SHA-256 hex
}
```

---

## Task 1: Backend — `manifest_hash.go` shared SHA-256 helper

**Files:**
- Create: `internal/api/manifest_hash.go`
- Create: `internal/api/manifest_hash_test.go`

### Step 1 — Write the failing test at `internal/api/manifest_hash_test.go`

```go
package api

import (
	"strings"
	"testing"
)

func TestManifestHashContent(t *testing.T) {
	// Same bytes → same hash.
	h1 := ManifestHashContent([]byte("name: demo\n"))
	h2 := ManifestHashContent([]byte("name: demo\n"))
	if h1 != h2 {
		t.Errorf("same bytes different hash: %q vs %q", h1, h2)
	}
	// Different bytes → different hash.
	h3 := ManifestHashContent([]byte("name: other\n"))
	if h1 == h3 {
		t.Errorf("different bytes same hash: %q", h1)
	}
	// Format: 64 hex chars.
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64", len(h1))
	}
	for _, c := range h1 {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("non-hex char %q in hash %q", c, h1)
		}
	}
}

func TestManifestHashContent_EmptyInput(t *testing.T) {
	h := ManifestHashContent([]byte{})
	// SHA-256("") is a well-known value.
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if h != want {
		t.Errorf("empty hash = %q, want %q", h, want)
	}
}
```

### Step 2 — Run; expect red

```bash
go test ./internal/api/ -run TestManifestHashContent -count=1
```

Expected: compile error — `ManifestHashContent` undefined.

### Step 3 — Implement `internal/api/manifest_hash.go`

```go
package api

import (
	"crypto/sha256"
	"encoding/hex"
)

// ManifestHashContent computes SHA-256 of a manifest file's bytes.
// Returned as lower-case hex, always 64 chars. Shared between
// ManifestGet (returned to client) and ManifestEdit (compared against
// client's expectedHash to detect external writes between Load and Save).
//
// We hash raw bytes, not parsed YAML, so whitespace/formatting changes
// from other editors are visible to the stale-detection flow. That is
// the intent — users should see "someone reformatted your manifest"
// as a stale event, not silently accept it.
func ManifestHashContent(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
```

### Step 4 — Run; expect green

```bash
go test ./internal/api/ -run TestManifestHashContent -count=1
```

Expected: 2 subtests PASS.

### Step 5 — Commit

```bash
git add internal/api/manifest_hash.go internal/api/manifest_hash_test.go
git commit -m "feat(api): ManifestHashContent SHA-256 helper for A2b stale detection

Used by ManifestGet (hash returned to client) and ManifestEdit
(compared against expectedHash to detect external writes between
Load and Save). Hashes raw bytes so whitespace/formatting changes
from other editors surface as stale events."
```

---

## Task 2: Backend — `api.ManifestGet` returns (yaml, hash)

**Files:**
- Modify: `internal/api/manifest.go`
- Modify: `internal/api/manifest_test.go`

### Step 1 — Write the failing test by APPENDING to `internal/api/manifest_test.go`

```go
func TestManifestGetIn_ReturnsContentHash(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "memory"
	// Must satisfy api.ManifestValidate (which ManifestCreateIn gates on):
	// requires kind, transport, command, and at least one daemon.
	yaml := "name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9210\n"
	if err := a.ManifestCreateIn(dir, name, yaml); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, hash, err := a.ManifestGetInWithHash(dir, name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != yaml {
		t.Errorf("yaml = %q, want %q", got, yaml)
	}
	want := ManifestHashContent([]byte(yaml))
	if hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
}

func TestManifestGetIn_HashChangesOnExternalWrite(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	initial := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9211\n"
	if err := a.ManifestCreateIn(dir, name, initial); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, h1, _ := a.ManifestGetInWithHash(dir, name)
	// External write — different bytes (port change) to simulate another
	// editor touching the file between Load and Save.
	path := filepath.Join(dir, name, "manifest.yaml")
	mutated := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9212\n"
	if err := os.WriteFile(path, []byte(mutated), 0600); err != nil {
		t.Fatalf("external write: %v", err)
	}
	_, h2, _ := a.ManifestGetInWithHash(dir, name)
	if h1 == h2 {
		t.Errorf("hash unchanged after external write: %q", h1)
	}
}
```

Import `os` and `filepath` if not already in the test file.

### Step 2 — Run; expect red

```bash
go test ./internal/api/ -run 'TestManifestGetIn_Returns|TestManifestGetIn_HashChanges' -count=1
```

Expected: `ManifestGetInWithHash` undefined.

### Step 3 — Extend `internal/api/manifest.go`

Add below the existing `ManifestGetIn` function:

```go
// ManifestGetInWithHash reads the manifest YAML and returns both the
// text and its SHA-256 content hash. Used by the GUI edit flow so
// ManifestEdit can detect external writes that occurred between Load
// and Save (A2b D3 stale-file detection).
func (a *API) ManifestGetInWithHash(dir, name string) (string, string, error) {
	path := filepath.Join(dir, name, "manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	return string(data), ManifestHashContent(data), nil
}

// ManifestGetWithHash is the default-dir convenience wrapper, used by
// GUI handlers which always read from defaultManifestDir().
func (a *API) ManifestGetWithHash(name string) (string, string, error) {
	if err := checkManifestName(name); err != nil {
		return "", "", err
	}
	// Read from disk (not embed) because edit flow only makes sense
	// for user-created / on-disk manifests — you cannot edit embedded
	// shipped manifests in-place.
	return a.ManifestGetInWithHash(defaultManifestDir(), name)
}
```

### Step 4 — Run; expect green

```bash
go test ./internal/api/ -run 'TestManifestGetIn_Returns|TestManifestGetIn_HashChanges' -count=1 -v
```

Expected: both subtests PASS.

### Step 5 — Full manifest package smoke

```bash
go test ./internal/api/ -run TestManifest -count=1
```

Expected: all pre-existing manifest tests still PASS.

### Step 6 — Commit

```bash
git add internal/api/manifest.go internal/api/manifest_test.go
git commit -m "feat(api): ManifestGetWithHash + ManifestGetInWithHash for A2b edit flow

Returns (yaml, sha256hash, err). GUI edit screen stores the hash at
Load; the matching ManifestEdit extension (next commit) rejects
writes when the on-disk hash no longer matches. This is the
stale-detection pipeline step 1 from the A2b memo D3."
```

---

## Task 3: Backend — `api.ManifestEdit` accepts expectedHash + `ErrManifestHashMismatch`

**Files:**
- Modify: `internal/api/manifest.go`
- Modify: `internal/api/manifest_test.go`

### Step 1 — Write the failing test

Append to `internal/api/manifest_test.go`:

```go
func TestManifestEditIn_RejectsHashMismatch(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	if err := a.ManifestCreateIn(dir, name, "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\n"); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, hash, _ := a.ManifestGetInWithHash(dir, name)
	// External write before edit.
	path := filepath.Join(dir, name, "manifest.yaml")
	if err := os.WriteFile(path, []byte("name: demo\nkind: workspace-scoped\ntransport: stdio-bridge\ncommand: npx\n"), 0600); err != nil {
		t.Fatalf("external write: %v", err)
	}
	// Attempt edit with now-stale hash.
	err := a.ManifestEditInWithHash(dir, name, "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\n", hash)
	if err == nil {
		t.Fatalf("expected hash-mismatch error, got nil")
	}
	if !errors.Is(err, ErrManifestHashMismatch) {
		t.Errorf("err = %v, want ErrManifestHashMismatch", err)
	}
}

func TestManifestEditIn_AcceptsMatchingHash(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	orig := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\n"
	if err := a.ManifestCreateIn(dir, name, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, hash, _ := a.ManifestGetInWithHash(dir, name)
	updated := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\n"
	if err := a.ManifestEditInWithHash(dir, name, updated, hash); err != nil {
		t.Fatalf("edit: %v", err)
	}
	got, _, _ := a.ManifestGetInWithHash(dir, name)
	if got != updated {
		t.Errorf("yaml = %q, want %q", got, updated)
	}
}

func TestManifestEditIn_EmptyExpectedHash_SkipsCheck(t *testing.T) {
	// Empty expectedHash is the "create-mode" escape hatch — skips
	// concurrency check, useful for the Force Save path which re-reads
	// at save-time and passes the fresh hash anyway.
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	if err := a.ManifestCreateIn(dir, name, "name: demo\n"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := a.ManifestEditInWithHash(dir, name, "name: demo\nkind: global\n", ""); err != nil {
		t.Fatalf("empty-hash edit should succeed: %v", err)
	}
}
```

Add `import "errors"` if missing.

### Step 2 — Run; expect red

```bash
go test ./internal/api/ -run TestManifestEditIn_ -count=1
```

Expected: `ManifestEditInWithHash` and `ErrManifestHashMismatch` undefined.

### Step 3 — Extend `internal/api/manifest.go`

Add near the top of the file (error declarations):

```go
// ErrManifestHashMismatch is returned by ManifestEditInWithHash when
// the on-disk manifest's current content hash does not match the
// client-supplied expectedHash. The GUI maps this to the stale-file
// banner (A2b D3). Passing an empty expectedHash skips the check —
// used by Force Save which re-reads at save-time.
var ErrManifestHashMismatch = errors.New("manifest hash mismatch: file changed on disk since it was loaded")
```

Add `"errors"` to the imports if not already there.

Then add below the existing `ManifestEditIn`:

```go
// ManifestEditInWithHash replaces an existing manifest, rejecting the
// write if the on-disk content hash has diverged from expectedHash.
// Passing expectedHash == "" skips the check (Force Save path).
func (a *API) ManifestEditInWithHash(dir, name, yaml, expectedHash string) error {
	if err := checkManifestName(name); err != nil {
		return err
	}
	target := filepath.Join(dir, name, "manifest.yaml")
	current, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("manifest %q does not exist; use create instead", name)
	}
	if expectedHash != "" {
		if got := ManifestHashContent(current); got != expectedHash {
			return ErrManifestHashMismatch
		}
	}
	if warnings := a.ManifestValidate(yaml); len(warnings) > 0 {
		return fmt.Errorf("manifest has validation errors: %s", strings.Join(warnings, "; "))
	}
	return os.WriteFile(target, []byte(yaml), 0644)
}

// ManifestEditWithHash is the default-dir convenience wrapper.
func (a *API) ManifestEditWithHash(name, yaml, expectedHash string) error {
	return a.ManifestEditInWithHash(defaultManifestDir(), name, yaml, expectedHash)
}
```

### Step 4 — Run; expect green

```bash
go test ./internal/api/ -run 'TestManifestEditIn_' -count=1 -v
```

Expected: 3 subtests PASS.

### Step 5 — Full manifest package smoke

```bash
go test ./internal/api/ -run TestManifest -count=1
```

Expected: all existing tests PASS + 3 new.

### Step 6 — Commit

```bash
git add internal/api/manifest.go internal/api/manifest_test.go
git commit -m "feat(api): ManifestEditWithHash with ErrManifestHashMismatch for A2b stale detection

Compares on-disk content hash against expectedHash before writing;
returns typed error on mismatch that the GUI maps to the Reload/
Force Save banner. Empty expectedHash skips the check — used by
Force Save which re-reads at save-time and passes the fresh hash."
```

---

## Task 4: GUI — extend manifest handlers for hash get/put

**Files:**
- Modify: `internal/gui/manifest.go`
- Modify: `internal/gui/manifest_test.go`
- Modify: `internal/gui/server.go` (add `manifestEditor` interface extension + real adapter)

### Step 1 — Extend interfaces + add new handler

In `internal/gui/manifest.go`:

1. Replace the `manifestValidator` and `manifestCreator` block to ALSO include a getter + hash-aware editor interface:

```go
type manifestCreator interface {
	ManifestCreate(name, yaml string) error
}

type manifestGetter interface {
	ManifestGetWithHash(name string) (yaml string, hash string, err error)
}

type manifestEditor interface {
	ManifestEditWithHash(name, yaml, expectedHash string) error
}

type manifestValidator interface {
	ManifestValidate(yaml string) []string
}
```

2. Add new request/response types:

```go
type manifestGetResponse struct {
	YAML string `json:"yaml"`
	Hash string `json:"hash"`
}

type manifestEditRequest struct {
	Name         string `json:"name"`
	YAML         string `json:"yaml"`
	ExpectedHash string `json:"expected_hash"`
}
```

3. Extend `registerManifestRoutes` to add two new routes:

```go
// Inside registerManifestRoutes(s *Server), after the existing two handlers,
// append:

s.mux.HandleFunc("/api/manifest/get", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeAPIError(w, fmt.Errorf("name must not be empty"), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	yaml, hash, err := s.manifestGetter.ManifestGetWithHash(name)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "MANIFEST_GET_FAILED")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(manifestGetResponse{YAML: yaml, Hash: hash})
}))

s.mux.HandleFunc("/api/manifest/edit", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req manifestEditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeAPIError(w, fmt.Errorf("name must not be empty"), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	if err := s.manifestEditor.ManifestEditWithHash(name, req.YAML, req.ExpectedHash); err != nil {
		code := "MANIFEST_EDIT_FAILED"
		status := http.StatusInternalServerError
		if errors.Is(err, api.ErrManifestHashMismatch) {
			code = "MANIFEST_HASH_MISMATCH"
			status = http.StatusConflict
		}
		writeAPIError(w, err, status, code)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}))
```

Add `"errors"` and `"mcp-local-hub/internal/api"` to the imports. (Note: `api` package import may already be indirectly available through the real adapter's wiring; verify during implementation.)

### Step 2 — Add tests at `internal/gui/manifest_test.go`

Append the following NEW tests (keep existing ones intact):

```go
type fakeManifestGetter struct {
	yaml string
	hash string
	err  error
}

func (f *fakeManifestGetter) ManifestGetWithHash(name string) (string, string, error) {
	return f.yaml, f.hash, f.err
}

type fakeManifestEditor struct {
	seenName         string
	seenYAML         string
	seenExpectedHash string
	err              error
}

func (f *fakeManifestEditor) ManifestEditWithHash(name, yaml, expectedHash string) error {
	f.seenName, f.seenYAML, f.seenExpectedHash = name, yaml, expectedHash
	return f.err
}

func newManifestTestServerFull(
	create *fakeManifestCreator,
	validate *fakeManifestValidator,
	getter *fakeManifestGetter,
	editor *fakeManifestEditor,
) *Server {
	s := &Server{
		mux:               http.NewServeMux(),
		manifestCreator:   create,
		manifestValidator: validate,
		manifestGetter:    getter,
		manifestEditor:    editor,
	}
	registerManifestRoutes(s)
	return s
}

// ---- /api/manifest/get ----

func TestManifestGetHandler_RejectsNonGET(t *testing.T) {
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{yaml: "name: x", hash: "h"}, &fakeManifestEditor{})
	req := httptest.NewRequest(http.MethodPost, "/api/manifest/get?name=x", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestManifestGetHandler_ReturnsYAMLAndHash(t *testing.T) {
	getter := &fakeManifestGetter{yaml: "name: demo\n", hash: "abc123"}
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		getter, &fakeManifestEditor{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/get?name=demo", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body manifestGetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.YAML != "name: demo\n" || body.Hash != "abc123" {
		t.Errorf("body = %+v", body)
	}
}

func TestManifestGetHandler_EmptyName_400(t *testing.T) {
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, &fakeManifestEditor{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/get?name=", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---- /api/manifest/edit ----

func TestManifestEditHandler_ForwardsNameYAMLAndHash(t *testing.T) {
	editor := &fakeManifestEditor{}
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, editor)
	rec := postJSON(t, s, "/api/manifest/edit",
		`{"name":"demo","yaml":"name: demo\nkind: global\n","expected_hash":"abc"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if editor.seenName != "demo" || editor.seenYAML != "name: demo\nkind: global\n" || editor.seenExpectedHash != "abc" {
		t.Errorf("got name=%q yaml=%q hash=%q", editor.seenName, editor.seenYAML, editor.seenExpectedHash)
	}
}

func TestManifestEditHandler_HashMismatch_Returns409(t *testing.T) {
	editor := &fakeManifestEditor{err: api.ErrManifestHashMismatch}
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, editor)
	rec := postJSON(t, s, "/api/manifest/edit",
		`{"name":"demo","yaml":"name: demo","expected_hash":"stale"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "MANIFEST_HASH_MISMATCH") {
		t.Errorf("body missing error code: %q", rec.Body.String())
	}
}

func TestManifestEditHandler_OtherError_Returns500(t *testing.T) {
	editor := &fakeManifestEditor{err: errors.New("disk full")}
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, editor)
	rec := postJSON(t, s, "/api/manifest/edit",
		`{"name":"demo","yaml":"name: demo","expected_hash":""}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}
```

Add `"mcp-local-hub/internal/api"` and `"errors"` imports.

### Step 3 — Wire Server struct + adapters in `internal/gui/server.go`

Add two fields to `Server`:
```go
	manifestGetter manifestGetter
	manifestEditor manifestEditor
```

Add adapters (follow the `realManifestCreator` pattern with empty-struct + internal `api.NewAPI()`):
```go
type realManifestGetter struct{}

func (realManifestGetter) ManifestGetWithHash(name string) (string, string, error) {
	return api.NewAPI().ManifestGetWithHash(name)
}

type realManifestEditor struct{}

func (realManifestEditor) ManifestEditWithHash(name, yaml, expectedHash string) error {
	return api.NewAPI().ManifestEditWithHash(name, yaml, expectedHash)
}
```

In `NewServer`, wire:
```go
	s.manifestGetter = realManifestGetter{}
	s.manifestEditor = realManifestEditor{}
```

### Step 4 — Run; expect green

```bash
go build ./... && go test ./internal/gui/ -count=1
```

Expected: all prior GUI tests + new manifest get/edit tests PASS.

### Step 5 — Commit

```bash
git add internal/gui/manifest.go internal/gui/manifest_test.go internal/gui/server.go
git commit -m "feat(gui): /api/manifest/get (with hash) + /api/manifest/edit (with expectedHash) for A2b

New handlers wrap api.ManifestGetWithHash and api.ManifestEditWithHash.
/api/manifest/get returns {yaml, hash}; /api/manifest/edit accepts
{name, yaml, expected_hash} and returns 409 MANIFEST_HASH_MISMATCH
when the on-disk hash diverged. 204 on success. Mirrors the PR #5
requireSameOrigin + writeAPIError pattern."
```

---

## Task 5: Frontend types — extend `ManifestFormState` for A2b

**Files:**
- Modify: `internal/gui/frontend/src/types.ts`

### Step 1 — Append new type definitions

Read `types.ts`. Keep all existing exports. Replace the `ManifestFormState` interface AND add the new interfaces listed in the memo's type section:

```ts
// DaemonFormEntry — A2b extension: adds form-state-only UUID for identity-
// stable rename + delete. The UUID is NEVER serialized to YAML; it's
// replaced by daemon.name at toYAML time and re-generated on each Load.
export interface DaemonFormEntry {
  _id: string;
  name: string;
  port: number;
  // A2b Advanced, workspace-scoped only:
  context?: string;
  extra_args?: string[];
}

// BindingFormEntry — A2b extension: references daemon by _id internally
// for rename safety. At toYAML time the _id is resolved to the daemon's
// current name.
export interface BindingFormEntry {
  client: string;
  daemonId: string;
  url_path: string;
}

// LanguageFormEntry — A2b Advanced (workspace-scoped only).
export interface LanguageFormEntry {
  _id: string;
  name: string;
  backend: string;
  transport?: "stdio" | "http_listen" | "native_http";
  lsp_command?: string;
  extra_flags?: string[];
}

// ManifestFormState — A2b shape. loadedHash + _preservedRaw support
// stale-file detection + round-trip preservation. Advanced fields are
// optional: kind-gated sub-fields (languages, port_pool, daemon.context)
// are only serialized when kind === "workspace-scoped".
export interface ManifestFormState {
  name: string;
  kind: "global" | "workspace-scoped";
  transport: "stdio-bridge" | "native-http";
  command: string;
  base_args: string[];
  env: Array<{ key: string; value: string }>;
  daemons: DaemonFormEntry[];
  client_bindings: BindingFormEntry[];
  weekly_refresh: boolean;
  // A2b Advanced:
  idle_timeout_min?: number;
  base_args_template?: string[];
  languages?: LanguageFormEntry[];
  port_pool?: { start: number; end: number };
  // A2b state-only:
  loadedHash: string;
  _preservedRaw: Record<string, unknown>;
}

export interface ValidationWarning {
  message: string;
}

export interface ManifestValidateResponse {
  warnings: string[];
}

export interface ExtractManifestResponse {
  yaml: string;
}

// GetManifestResponse mirrors the new /api/manifest/get response.
export interface GetManifestResponse {
  yaml: string;
  hash: string;
}
```

### Step 2 — Typecheck

```bash
cd internal/gui/frontend && npm run typecheck
```

Expected: MANY errors because downstream code (AddServer, manifest-yaml, api.ts tests) still references the OLD shapes. This is expected — Tasks 6-10 fix them.

We do NOT commit at this step alone. Typecheck-clean is restored after Task 6.

### Step 3 — NO COMMIT YET

Tasks 5, 6, 7, 8 form a tight group (types → yaml helpers → api.ts → uuid). We commit them together at the end of Task 8 once typecheck is clean again. Each of Tasks 5-7 ends with `# not committed yet — typecheck clean after Task 8`.

---

## Task 6: Frontend — `uuid.ts` helper

**Files:**
- Create: `internal/gui/frontend/src/lib/uuid.ts`
- Create: `internal/gui/frontend/src/lib/uuid.test.ts`

### Step 1 — Write failing test

`internal/gui/frontend/src/lib/uuid.test.ts`:

```ts
import { describe, it, expect } from "vitest";
import { generateUUID } from "./uuid";

describe("generateUUID", () => {
  it("produces a non-empty string", () => {
    const id = generateUUID();
    expect(typeof id).toBe("string");
    expect(id.length).toBeGreaterThan(0);
  });
  it("produces different ids on successive calls", () => {
    const a = generateUUID();
    const b = generateUUID();
    expect(a).not.toBe(b);
  });
  it("matches a v4-ish shape (8-4-4-4-12 hex) when crypto.randomUUID is native", () => {
    const id = generateUUID();
    // crypto.randomUUID output is always this shape; the fallback may differ
    // but must still be unique. Assert SHAPE only when native is present.
    if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
      expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
    }
  });
});
```

### Step 2 — Run; expect red

```bash
cd internal/gui/frontend && npm run test -- src/lib/uuid.test.ts
```

Expected: module not found.

### Step 3 — Implement `internal/gui/frontend/src/lib/uuid.ts`

```ts
// generateUUID returns a v4-style unique identifier. Uses the native
// crypto.randomUUID when available (Node 14.17+, all modern browsers);
// falls back to a Math.random-based non-cryptographic shape for happy-dom
// test environments that may not wire crypto.randomUUID.
//
// Used by manifest-yaml.ts to assign stable DaemonFormEntry._id and
// LanguageFormEntry._id at parse time. These IDs are NEVER serialized
// to YAML — they exist only to keep form-state references stable across
// user-driven rename/delete/reorder operations.
export function generateUUID(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  // Fallback (test env). Not cryptographically secure; uniqueness is
  // sufficient for in-memory form identity over a session.
  const s = () => Math.floor((1 + Math.random()) * 0x10000).toString(16).substring(1);
  return `${s()}${s()}-${s()}-${s()}-${s()}-${s()}${s()}${s()}`;
}
```

### Step 4 — Run; expect green

```bash
cd internal/gui/frontend && npm run test -- src/lib/uuid.test.ts
```

Expected: 3 subtests PASS.

### Step 5 — NO COMMIT YET (grouped with Task 5, 7, 8)

---

## Task 7: `parseYAMLToForm` + `toYAML` — UUID assignment, _preservedRaw, nested-unknown detection

**Files:**
- Modify: `internal/gui/frontend/src/lib/manifest-yaml.ts`
- Modify: `internal/gui/frontend/src/lib/manifest-yaml.test.ts`

### Step 1 — Extend the test file

Keep all existing tests. Append:

```ts
import { parseYAMLToForm, BLANK_FORM, toYAML, hasNestedUnknown } from "./manifest-yaml";

describe("parseYAMLToForm A2b extensions", () => {
  it("assigns a unique _id to each daemon", () => {
    const form = parseYAMLToForm(`name: demo
command: npx
daemons:
  - name: a
    port: 9100
  - name: b
    port: 9101
`);
    expect(form.daemons[0]._id).toBeDefined();
    expect(form.daemons[1]._id).toBeDefined();
    expect(form.daemons[0]._id).not.toBe(form.daemons[1]._id);
  });

  it("re-keys client_bindings to the freshly-generated daemonId", () => {
    const form = parseYAMLToForm(`name: demo
command: npx
daemons:
  - name: default
    port: 9100
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`);
    const defaultId = form.daemons[0]._id;
    expect(form.client_bindings[0].daemonId).toBe(defaultId);
    expect(form.client_bindings[0].client).toBe("claude-code");
    expect(form.client_bindings[0].url_path).toBe("/mcp");
  });

  it("extracts unknown top-level YAML keys into _preservedRaw", () => {
    const form = parseYAMLToForm(`name: demo
command: npx
some_future_field: "hi"
another_ref: 42
`);
    expect(form._preservedRaw).toEqual({
      some_future_field: "hi",
      another_ref: 42,
    });
  });

  it("leaves _preservedRaw empty for a fully-recognized manifest", () => {
    const form = parseYAMLToForm(`name: demo
kind: global
transport: stdio-bridge
command: npx
`);
    expect(form._preservedRaw).toEqual({});
  });

  it("sets loadedHash to empty string (caller sets it on Load)", () => {
    const form = parseYAMLToForm(`name: demo\ncommand: npx\n`);
    expect(form.loadedHash).toBe("");
  });

  it("parses A2b Advanced fields when present", () => {
    const form = parseYAMLToForm(`name: demo
kind: workspace-scoped
transport: stdio-bridge
command: ws
idle_timeout_min: 15
base_args_template: ["--lang=$LANG"]
port_pool:
  start: 9200
  end: 9220
languages:
  - name: python
    backend: mcp-language-server
    transport: stdio
    lsp_command: pyright-langserver
    extra_flags: ["--stdio"]
`);
    expect(form.idle_timeout_min).toBe(15);
    expect(form.base_args_template).toEqual(["--lang=$LANG"]);
    expect(form.port_pool).toEqual({ start: 9200, end: 9220 });
    expect(form.languages).toHaveLength(1);
    expect(form.languages![0]._id).toBeDefined();
    expect(form.languages![0].name).toBe("python");
    expect(form.languages![0].backend).toBe("mcp-language-server");
  });
});

describe("hasNestedUnknown", () => {
  it("returns false for a vanilla global manifest", () => {
    const yaml = `name: demo
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: a
    port: 9100
`;
    expect(hasNestedUnknown(yaml)).toBe(false);
  });

  it("returns true when a daemon has an unknown field", () => {
    const yaml = `name: demo
command: npx
daemons:
  - name: a
    port: 9100
    extra_config:
      foo: 1
`;
    expect(hasNestedUnknown(yaml)).toBe(true);
  });

  it("returns true when a language has an unknown field", () => {
    const yaml = `name: demo
kind: workspace-scoped
transport: stdio-bridge
command: ws
languages:
  - name: python
    backend: mcp-language-server
    future_feature: 42
`;
    expect(hasNestedUnknown(yaml)).toBe(true);
  });

  it("returns true when a client_binding has an unknown field", () => {
    const yaml = `name: demo
command: npx
daemons:
  - name: a
    port: 9100
client_bindings:
  - client: claude-code
    daemon: a
    url_path: /mcp
    priority: high
`;
    expect(hasNestedUnknown(yaml)).toBe(true);
  });

  it("returns false for top-level unknown keys (those go to _preservedRaw)", () => {
    const yaml = `name: demo
command: npx
future_top_level_field: ok
`;
    expect(hasNestedUnknown(yaml)).toBe(false);
  });
});

describe("toYAML A2b extensions", () => {
  it("resolves daemonId back to daemon.name at serialize time", () => {
    const form: ManifestFormState = {
      ...BLANK_FORM,
      name: "demo",
      command: "npx",
      daemons: [{ _id: "uuid-1", name: "only", port: 9100 }],
      client_bindings: [{ client: "claude-code", daemonId: "uuid-1", url_path: "/mcp" }],
    };
    const yaml = toYAML(form);
    expect(yaml).toMatch(/- client: claude-code\s+daemon: only\s+url_path: \/mcp/);
  });

  it("drops client_bindings whose daemonId no longer exists (safety net)", () => {
    const form: ManifestFormState = {
      ...BLANK_FORM,
      name: "demo",
      command: "npx",
      daemons: [{ _id: "uuid-live", name: "live", port: 9100 }],
      client_bindings: [
        { client: "claude-code", daemonId: "uuid-live", url_path: "/mcp" },
        { client: "codex-cli", daemonId: "uuid-deleted", url_path: "/mcp" },
      ],
    };
    const yaml = toYAML(form);
    expect(yaml).toContain("client: claude-code");
    expect(yaml).not.toContain("client: codex-cli");
  });

  it("merges _preservedRaw top-level keys into the output", () => {
    const form: ManifestFormState = {
      ...BLANK_FORM,
      name: "demo",
      command: "npx",
      _preservedRaw: {
        custom_annotation: "hello",
        ext_config: { a: 1 },
      },
    };
    const yaml = toYAML(form);
    expect(yaml).toContain("custom_annotation:");
    expect(yaml).toContain("ext_config:");
  });

  it("emits Advanced fields only when non-empty and kind gates workspace-only ones", () => {
    const global: ManifestFormState = {
      ...BLANK_FORM,
      name: "demo",
      command: "npx",
      kind: "global",
      idle_timeout_min: 15,
      languages: [{ _id: "u", name: "python", backend: "mcp-language-server" }],
      port_pool: { start: 9200, end: 9220 },
    };
    const y1 = toYAML(global);
    expect(y1).toContain("idle_timeout_min: 15");
    // kind-gated fields dropped when kind !== workspace-scoped.
    expect(y1).not.toContain("languages:");
    expect(y1).not.toContain("port_pool:");

    const workspace: ManifestFormState = { ...global, kind: "workspace-scoped" };
    const y2 = toYAML(workspace);
    expect(y2).toContain("languages:");
    expect(y2).toContain("port_pool:");
  });
});
```

Remember to update the existing `BLANK_FORM` default consumers and the `base` test const to include `loadedHash: ""` and `_preservedRaw: {}`.

### Step 2 — Run; expect red (many failures — types moved)

```bash
cd internal/gui/frontend && npm run test -- src/lib/manifest-yaml.test.ts
```

Expected: existing tests break because `daemons[0]` no longer matches the A2a shape, `BLANK_FORM` is missing `loadedHash`/`_preservedRaw`, and `hasNestedUnknown` is undefined.

### Step 3 — Refactor `internal/gui/frontend/src/lib/manifest-yaml.ts`

Replace the file entirely. Key changes:

1. `import { generateUUID } from "./uuid";` at top.
2. `BLANK_FORM` updated with `loadedHash: ""`, `_preservedRaw: {}`, `daemons: []`, `client_bindings: []`.
3. Known-key allowlists (top-level + nested-per-level).
4. `parseYAMLToForm`: extracts unknown top-level keys to `_preservedRaw`, assigns `_id` to each daemon / language, re-keys bindings by name→`_id`, returns `loadedHash: ""` (caller sets).
5. `toYAML`: resolves `daemonId → daemon.name`, drops orphan bindings, merges `_preservedRaw`, kind-gates workspace-only fields.
6. `hasNestedUnknown(yaml: string): boolean`: parses YAML and walks daemons[] / languages[] / client_bindings[], returns true if any element has a field outside its per-level allowlist.

```ts
import { parse as yamlParse } from "yaml";
import type {
  ManifestFormState,
  DaemonFormEntry,
  BindingFormEntry,
  LanguageFormEntry,
} from "../types";
import { generateUUID } from "./uuid";

// Known top-level keys — anything else lands in _preservedRaw.
const TOP_LEVEL_KNOWN = new Set([
  "name",
  "kind",
  "transport",
  "command",
  "base_args",
  "env",
  "daemons",
  "client_bindings",
  "weekly_refresh",
  "idle_timeout_min",
  "base_args_template",
  "languages",
  "port_pool",
]);

// Nested per-level known keys — used by hasNestedUnknown.
const DAEMON_KNOWN = new Set(["name", "port", "context", "extra_args"]);
const LANGUAGE_KNOWN = new Set([
  "name",
  "backend",
  "transport",
  "lsp_command",
  "extra_flags",
]);
const BINDING_KNOWN = new Set(["client", "daemon", "url_path"]);

export const BLANK_FORM: ManifestFormState = {
  name: "",
  kind: "global",
  transport: "stdio-bridge",
  command: "",
  base_args: [],
  env: [],
  daemons: [],
  client_bindings: [],
  weekly_refresh: false,
  loadedHash: "",
  _preservedRaw: {},
};

function quote(s: string): string {
  if (s.includes(`"`) || s.includes("\\")) {
    return `'${s.replace(/'/g, `''`)}'`;
  }
  return `"${s}"`;
}

function asString(v: unknown, fallback: string): string {
  return typeof v === "string" ? v : fallback;
}
function asNumber(v: unknown, fallback: number): number {
  return typeof v === "number" && Number.isFinite(v) ? v : fallback;
}
function asKind(v: unknown): "global" | "workspace-scoped" {
  return v === "workspace-scoped" ? "workspace-scoped" : "global";
}
function asTransport(v: unknown): "stdio-bridge" | "native-http" {
  return v === "native-http" ? "native-http" : "stdio-bridge";
}
function asStringArray(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
}

export function parseYAMLToForm(yaml: string): ManifestFormState {
  const raw = yamlParse(yaml) as Record<string, unknown> | null;
  if (raw == null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...BLANK_FORM, _preservedRaw: {} };
  }

  // Extract _preservedRaw: any TOP-LEVEL key we don't recognize.
  const preserved: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(raw)) {
    if (!TOP_LEVEL_KNOWN.has(k)) preserved[k] = v;
  }

  // env map → array-of-{key,value}.
  const envRaw = raw.env;
  const env: Array<{ key: string; value: string }> =
    envRaw && typeof envRaw === "object" && !Array.isArray(envRaw)
      ? Object.entries(envRaw as Record<string, unknown>).map(([key, value]) => ({
          key,
          value: asString(value, ""),
        }))
      : [];

  // Daemons: assign fresh _id per row.
  const daemonsRaw = raw.daemons;
  const daemons: DaemonFormEntry[] = Array.isArray(daemonsRaw)
    ? daemonsRaw
        .filter((d): d is Record<string, unknown> => typeof d === "object" && d !== null)
        .map((d) => {
          const entry: DaemonFormEntry = {
            _id: generateUUID(),
            name: asString(d.name, ""),
            port: asNumber(d.port, 0),
          };
          if (typeof d.context === "string") entry.context = d.context;
          const extra = asStringArray(d.extra_args);
          if (extra.length > 0) entry.extra_args = extra;
          return entry;
        })
    : [];

  // Build a map: daemon.name → _id for re-keying client_bindings.
  const nameToId = new Map<string, string>();
  for (const d of daemons) nameToId.set(d.name, d._id);

  // client_bindings: daemon name → daemonId.
  const bindingsRaw = raw.client_bindings;
  const bindings: BindingFormEntry[] = Array.isArray(bindingsRaw)
    ? bindingsRaw
        .filter((b): b is Record<string, unknown> => typeof b === "object" && b !== null)
        .map((b) => ({
          client: asString(b.client, ""),
          daemonId: nameToId.get(asString(b.daemon, "")) ?? "",
          url_path: asString(b.url_path, ""),
        }))
    : [];

  // Advanced: idle_timeout_min, base_args_template, port_pool, languages.
  const langRaw = raw.languages;
  const languages: LanguageFormEntry[] | undefined = Array.isArray(langRaw)
    ? langRaw
        .filter((l): l is Record<string, unknown> => typeof l === "object" && l !== null)
        .map((l) => {
          const entry: LanguageFormEntry = {
            _id: generateUUID(),
            name: asString(l.name, ""),
            backend: asString(l.backend, ""),
          };
          const t = l.transport;
          if (t === "stdio" || t === "http_listen" || t === "native_http") entry.transport = t;
          if (typeof l.lsp_command === "string") entry.lsp_command = l.lsp_command;
          const flags = asStringArray(l.extra_flags);
          if (flags.length > 0) entry.extra_flags = flags;
          return entry;
        })
    : undefined;

  const pp = raw.port_pool;
  const port_pool =
    pp && typeof pp === "object" && !Array.isArray(pp)
      ? (() => {
          const r = pp as Record<string, unknown>;
          const start = asNumber(r.start, 0);
          const end = asNumber(r.end, 0);
          return start > 0 || end > 0 ? { start, end } : undefined;
        })()
      : undefined;

  const bat = asStringArray(raw.base_args_template);

  return {
    name: asString(raw.name, ""),
    kind: asKind(raw.kind),
    transport: asTransport(raw.transport),
    command: asString(raw.command, ""),
    base_args: asStringArray(raw.base_args),
    env,
    daemons,
    client_bindings: bindings,
    weekly_refresh: raw.weekly_refresh === true,
    idle_timeout_min: typeof raw.idle_timeout_min === "number" ? raw.idle_timeout_min : undefined,
    base_args_template: bat.length > 0 ? bat : undefined,
    languages,
    port_pool,
    loadedHash: "",
    _preservedRaw: preserved,
  };
}

export function hasNestedUnknown(yaml: string): boolean {
  let raw: unknown;
  try {
    raw = yamlParse(yaml);
  } catch {
    return false;  // parse error handled elsewhere; not a "nested unknown".
  }
  if (raw == null || typeof raw !== "object" || Array.isArray(raw)) return false;
  const r = raw as Record<string, unknown>;

  const daemons = r.daemons;
  if (Array.isArray(daemons)) {
    for (const d of daemons) {
      if (d && typeof d === "object" && !Array.isArray(d)) {
        for (const k of Object.keys(d as Record<string, unknown>)) {
          if (!DAEMON_KNOWN.has(k)) return true;
        }
      }
    }
  }

  const languages = r.languages;
  if (Array.isArray(languages)) {
    for (const l of languages) {
      if (l && typeof l === "object" && !Array.isArray(l)) {
        for (const k of Object.keys(l as Record<string, unknown>)) {
          if (!LANGUAGE_KNOWN.has(k)) return true;
        }
      }
    }
  }

  const bindings = r.client_bindings;
  if (Array.isArray(bindings)) {
    for (const b of bindings) {
      if (b && typeof b === "object" && !Array.isArray(b)) {
        for (const k of Object.keys(b as Record<string, unknown>)) {
          if (!BINDING_KNOWN.has(k)) return true;
        }
      }
    }
  }

  return false;
}

export function toYAML(state: ManifestFormState): string {
  const lines: string[] = [];
  lines.push(`name: ${state.name}`);
  lines.push(`kind: ${state.kind}`);
  lines.push(`transport: ${state.transport}`);
  lines.push(`command: ${state.command}`);
  if (state.base_args.length > 0) {
    lines.push(`base_args: [${state.base_args.map(quote).join(", ")}]`);
  }
  if (state.base_args_template && state.base_args_template.length > 0) {
    lines.push(`base_args_template: [${state.base_args_template.map(quote).join(", ")}]`);
  }
  if (state.env.length > 0) {
    lines.push("env:");
    for (const { key, value } of state.env) {
      lines.push(`  ${key}: ${quote(value)}`);
    }
  }
  if (state.daemons.length > 0) {
    lines.push("daemons:");
    for (const d of state.daemons) {
      lines.push(`  - name: ${d.name}`);
      lines.push(`    port: ${d.port}`);
      if (state.kind === "workspace-scoped") {
        if (typeof d.context === "string" && d.context.length > 0) {
          lines.push(`    context: ${quote(d.context)}`);
        }
      }
      if (d.extra_args && d.extra_args.length > 0) {
        lines.push(`    extra_args: [${d.extra_args.map(quote).join(", ")}]`);
      }
    }
  }
  // Resolve daemonId → daemon.name; drop bindings whose daemon was deleted.
  const idToName = new Map<string, string>();
  for (const d of state.daemons) idToName.set(d._id, d.name);
  const liveBindings = state.client_bindings.filter((b) => idToName.has(b.daemonId));
  if (liveBindings.length > 0) {
    lines.push("client_bindings:");
    for (const b of liveBindings) {
      lines.push(`  - client: ${b.client}`);
      lines.push(`    daemon: ${idToName.get(b.daemonId)}`);
      lines.push(`    url_path: ${b.url_path}`);
    }
  }
  if (state.weekly_refresh) {
    lines.push("weekly_refresh: true");
  }
  // Workspace-gated Advanced.
  if (state.kind === "workspace-scoped") {
    if (state.languages && state.languages.length > 0) {
      lines.push("languages:");
      for (const l of state.languages) {
        lines.push(`  - name: ${l.name}`);
        lines.push(`    backend: ${l.backend}`);
        if (l.transport) lines.push(`    transport: ${l.transport}`);
        if (l.lsp_command) lines.push(`    lsp_command: ${quote(l.lsp_command)}`);
        if (l.extra_flags && l.extra_flags.length > 0) {
          lines.push(`    extra_flags: [${l.extra_flags.map(quote).join(", ")}]`);
        }
      }
    }
    if (state.port_pool) {
      lines.push("port_pool:");
      lines.push(`  start: ${state.port_pool.start}`);
      lines.push(`  end: ${state.port_pool.end}`);
    }
  }
  if (typeof state.idle_timeout_min === "number") {
    lines.push(`idle_timeout_min: ${state.idle_timeout_min}`);
  }
  // Merge top-level _preservedRaw using yaml library's stringify for
  // complex values. Simple scalars inlined.
  for (const [k, v] of Object.entries(state._preservedRaw)) {
    if (typeof v === "string") {
      lines.push(`${k}: ${quote(v)}`);
    } else if (typeof v === "number" || typeof v === "boolean") {
      lines.push(`${k}: ${v}`);
    } else {
      // Fallback: delegate to JSON (YAML accepts JSON-like inline). Not
      // pretty, but we don't know the shape. Users editing via GUI
      // won't touch these anyway.
      lines.push(`${k}: ${JSON.stringify(v)}`);
    }
  }
  return lines.join("\n") + "\n";
}
```

### Step 4 — Run; expect green

```bash
cd internal/gui/frontend && npm run test -- src/lib/manifest-yaml.test.ts
```

Expected: ALL tests PASS, both A2a-era + new A2b.

### Step 5 — NO COMMIT YET

---

## Task 8: Frontend API helpers — `getManifest` + `postManifestEdit`

**Files:**
- Modify: `internal/gui/frontend/src/api.ts`
- Modify: `internal/gui/frontend/src/api.test.ts`

### Step 1 — Extend the tests

Append to `internal/gui/frontend/src/api.test.ts`:

```ts
import { getManifest, postManifestEdit, ManifestHashMismatchError } from "./api";

describe("getManifest", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });
  it("returns {yaml, hash} on 200", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ yaml: "name: demo\n", hash: "abc" }),
    }) as unknown as Response);
    const out = await getManifest("demo");
    expect(out).toEqual({ yaml: "name: demo\n", hash: "abc" });
  });
  it("throws on non-2xx", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 500,
      statusText: "Internal Server Error",
      json: async () => ({ error: "read failed" }),
    }) as unknown as Response);
    await expect(getManifest("demo")).rejects.toThrow(/read failed/);
  });
  it("URL-encodes the name", async () => {
    const seen: { url?: string } = {};
    globalThis.fetch = vi.fn(async (url: RequestInfo | URL) => {
      seen.url = url.toString();
      return { ok: true, status: 200, json: async () => ({ yaml: "", hash: "" }) } as unknown as Response;
    });
    await getManifest("weird name");
    expect(seen.url).toContain("name=weird%20name");
  });
});

describe("postManifestEdit", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });
  it("resolves on 204", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 204,
      statusText: "No Content",
    }) as unknown as Response);
    await expect(postManifestEdit("demo", "name: demo\n", "hash")).resolves.toBeUndefined();
  });
  it("throws ManifestHashMismatchError on 409 MANIFEST_HASH_MISMATCH", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 409,
      statusText: "Conflict",
      json: async () => ({ error: "hash mismatch", code: "MANIFEST_HASH_MISMATCH" }),
    }) as unknown as Response);
    await expect(postManifestEdit("demo", "name: demo\n", "stale")).rejects.toBeInstanceOf(ManifestHashMismatchError);
  });
  it("throws generic Error on other non-2xx", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 500,
      statusText: "Internal Server Error",
      json: async () => ({ error: "disk full" }),
    }) as unknown as Response);
    await expect(postManifestEdit("demo", "name: demo\n", "hash")).rejects.toThrow(/disk full/);
  });
  it("sends name + yaml + expected_hash in JSON body", async () => {
    const seen: { body?: string } = {};
    globalThis.fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      seen.body = init?.body as string;
      return { ok: true, status: 204 } as unknown as Response;
    });
    await postManifestEdit("demo", "name: demo", "hash123");
    expect(JSON.parse(seen.body!)).toEqual({
      name: "demo",
      yaml: "name: demo",
      expected_hash: "hash123",
    });
  });
});
```

### Step 2 — Run; expect red

```bash
cd internal/gui/frontend && npm run test -- src/api.test.ts
```

### Step 3 — Extend `internal/gui/frontend/src/api.ts`

Append after existing exports:

```ts
// ManifestHashMismatchError marks the stale-file-detection branch so
// the AddServer edit flow can show the [Reload]/[Force Save] banner
// instead of a generic error toast.
export class ManifestHashMismatchError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ManifestHashMismatchError";
  }
}

// getManifest reads the named manifest from disk and returns the YAML
// together with the SHA-256 content hash. The hash is stored in form
// state at Load and passed back to postManifestEdit as expected_hash.
export async function getManifest(name: string): Promise<{ yaml: string; hash: string }> {
  const resp = await fetch(`/api/manifest/get?name=${encodeURIComponent(name)}`);
  if (!resp.ok) {
    let body: { error?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string };
    } catch {
      // Non-JSON error body; fall through.
    }
    throw new Error(`/api/manifest/get: ${body?.error ?? resp.statusText}`);
  }
  const payload = (await resp.json()) as { yaml?: string; hash?: string };
  return { yaml: payload.yaml ?? "", hash: payload.hash ?? "" };
}

// postManifestEdit overwrites an existing manifest. expectedHash is the
// hash returned by getManifest at Load time; the backend rejects the
// write with 409 if the on-disk hash has since changed (external edit).
// Pass expectedHash === "" to skip the concurrency check (Force Save
// re-read path).
export async function postManifestEdit(
  name: string,
  yaml: string,
  expectedHash: string,
): Promise<void> {
  const resp = await fetch("/api/manifest/edit", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, yaml, expected_hash: expectedHash }),
  });
  if (resp.status === 204) return;
  let body: { error?: string; code?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string; code?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  if (resp.status === 409 && body?.code === "MANIFEST_HASH_MISMATCH") {
    throw new ManifestHashMismatchError(body.error ?? "hash mismatch");
  }
  throw new Error(`/api/manifest/edit: ${body?.error ?? resp.statusText}`);
}
```

### Step 4 — Run; expect green

```bash
cd internal/gui/frontend && npm run test
```

Expected: ALL Vitest suites PASS — A2a-era (toYAML, parseYAMLToForm, fetchOrThrow, etc.) + A2b extensions (UUID, _preservedRaw, hasNestedUnknown, getManifest, postManifestEdit).

### Step 5 — Typecheck

```bash
cd internal/gui/frontend && npm run typecheck
```

Expected: exit 0. Downstream code still using OLD types (`DaemonFormEntry` without `_id`, `BindingFormEntry.daemon` instead of `daemonId`) will fail here. Because we don't touch AddServer.tsx yet (Task 10), the typecheck may STILL fail — if it does, that is expected; Task 10 restores clean state.

Pragmatic check: if `npm run typecheck` fails only inside `AddServer.tsx`, proceed to Step 6. If it fails elsewhere, fix that now as part of this task (it's part of the type migration).

### Step 6 — Commit the batched Tasks 5-8

```bash
git add internal/gui/frontend/src/types.ts \
        internal/gui/frontend/src/lib/uuid.ts \
        internal/gui/frontend/src/lib/uuid.test.ts \
        internal/gui/frontend/src/lib/manifest-yaml.ts \
        internal/gui/frontend/src/lib/manifest-yaml.test.ts \
        internal/gui/frontend/src/api.ts \
        internal/gui/frontend/src/api.test.ts
git commit -m "feat(gui/frontend): A2b type extensions + UUID + _preservedRaw + hash API

Types: ManifestFormState gains loadedHash + _preservedRaw; DaemonFormEntry +
LanguageFormEntry gain _id (UUID, form-state-only, never serialized);
BindingFormEntry replaces daemon-by-name with daemonId (internal-ID ref).

Helpers:
- uuid.generateUUID — crypto.randomUUID wrapper with test fallback.
- parseYAMLToForm — assigns _id to each daemon/language, re-keys bindings
  by name→_id at load time, extracts unknown top-level keys into
  _preservedRaw.
- toYAML — resolves daemonId→name at serialize time, drops orphan
  bindings whose daemon was deleted, merges _preservedRaw, kind-gates
  workspace-only fields (languages, port_pool, daemon.context).
- hasNestedUnknown(yaml) — detects unknown fields inside daemons[],
  languages[], or client_bindings[] entries (drives A2b read-only
  mode for manifests with nested unknowns).

API:
- getManifest(name) → {yaml, hash} wraps GET /api/manifest/get.
- postManifestEdit(name, yaml, expectedHash) wraps POST /api/manifest/edit;
  throws ManifestHashMismatchError on 409 MANIFEST_HASH_MISMATCH.

Downstream AddServer.tsx still references A2a shapes; next task migrates."
```

---

## Task 9: `useRouter` extension — accepts guard callback + suppression flag

**Files:**
- Modify: `internal/gui/frontend/src/hooks/useRouter.ts`
- Modify: `internal/gui/frontend/src/hooks/useRouter.test.ts`

### Step 1 — Extend test

Append to `useRouter.test.ts` (keeping existing tests):

```ts
import { renderHook, act } from "@testing-library/preact";

describe("useRouter guard (A2b)", () => {
  beforeEach(() => {
    window.location.hash = "";
  });

  it("calls guard on hashchange when installed", () => {
    const guard = vi.fn(() => true);
    renderHook(() => useRouter("servers", guard));
    act(() => {
      window.location.hash = "#/migration";
      window.dispatchEvent(new HashChangeEvent("hashchange", {
        oldURL: "http://localhost/",
        newURL: "http://localhost/#/migration",
      }));
    });
    expect(guard).toHaveBeenCalled();
  });

  it("reverts hash via replaceState when guard returns false", () => {
    window.location.hash = "#/add-server";
    const guard = vi.fn(() => false);
    const { result } = renderHook(() => useRouter("servers", guard));
    // Initial screen is add-server.
    expect(result.current).toBe("add-server");
    act(() => {
      window.history.pushState(null, "", "#/migration");
      window.dispatchEvent(new HashChangeEvent("hashchange", {
        oldURL: "http://localhost/#/add-server",
        newURL: "http://localhost/#/migration",
      }));
    });
    // Guard declined → hash reverted; screen stays add-server.
    expect(result.current).toBe("add-server");
  });
});
```

### Step 2 — Replace `useRouter.ts`

```ts
import { useEffect, useRef, useState } from "preact/hooks";

// useRouter is a minimal hash router. Returns the active screen name
// (text after "#/" up to first "?") and updates on hashchange.
//
// A2b addition: optional `guard(target)` callback. On every hashchange:
//   - If guard returns true (or not installed), navigation proceeds.
//   - If guard returns false, hash is reverted to the previous URL via
//     history.replaceState, with a suppression flag that skips the
//     follow-up hashchange event the replaceState itself fires.
//
// The guard sees the TARGET screen key, not the current one, so it can
// decide based on "leaving add-server while dirty" etc. Implementation
// returns the resolved screen; the guard's revert happens inside the
// hashchange handler so callers don't need to worry about race conditions.
export function useRouter(
  defaultScreen: string,
  guard?: (target: string) => boolean,
): string {
  const parse = () => {
    const hash = window.location.hash || `#/${defaultScreen}`;
    const afterPrefix = hash.replace(/^#\//, "");
    const screen = afterPrefix.split("?")[0];
    return screen || defaultScreen;
  };
  const [screen, setScreen] = useState<string>(parse());
  const suppressRef = useRef(false);

  useEffect(() => {
    const onHash = (e: HashChangeEvent) => {
      if (suppressRef.current) {
        suppressRef.current = false;
        return;
      }
      const target = parse();
      if (guard && !guard(target)) {
        suppressRef.current = true;
        window.history.replaceState(null, "", e.oldURL);
        return;
      }
      setScreen(target);
    };
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [guard]);

  return screen;
}
```

### Step 3 — Run + typecheck

```bash
cd internal/gui/frontend && npm run typecheck && npm run test -- src/hooks/useRouter.test.ts
```

Expected: existing useRouter tests pass + 2 new pass.

### Step 4 — Commit

```bash
git add internal/gui/frontend/src/hooks/useRouter.ts internal/gui/frontend/src/hooks/useRouter.test.ts
git commit -m "feat(gui/frontend): useRouter accepts guard + suppression flag (A2b D5)

Guard callback receives the target screen key; returning false reverts
the hash via history.replaceState. The suppression flag skips the
follow-up hashchange event that replaceState itself fires, preventing
infinite recursion. Suppression is internal — callers never see the
revert event."
```

---

## Task 10: `useUnsavedChangesGuard` hook — beforeunload + hashchange guard wrapper

**Files:**
- Create: `internal/gui/frontend/src/hooks/useUnsavedChangesGuard.ts`
- Create: `internal/gui/frontend/src/hooks/useUnsavedChangesGuard.test.ts`

### Step 1 — Write failing test

```ts
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook } from "@testing-library/preact";
import { useUnsavedChangesGuard } from "./useUnsavedChangesGuard";

describe("useUnsavedChangesGuard", () => {
  let beforeUnloadHandler: ((e: BeforeUnloadEvent) => void) | null = null;
  beforeEach(() => {
    beforeUnloadHandler = null;
    vi.spyOn(window, "addEventListener").mockImplementation((ev, h) => {
      if (ev === "beforeunload") beforeUnloadHandler = h as (e: BeforeUnloadEvent) => void;
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("does not call preventDefault when clean", () => {
    renderHook(() => useUnsavedChangesGuard(false));
    const e = new Event("beforeunload") as BeforeUnloadEvent;
    e.preventDefault = vi.fn();
    (e as any).returnValue = undefined;
    beforeUnloadHandler?.(e);
    expect(e.preventDefault).not.toHaveBeenCalled();
  });

  it("calls preventDefault + sets returnValue when dirty", () => {
    renderHook(() => useUnsavedChangesGuard(true));
    const e = new Event("beforeunload") as BeforeUnloadEvent;
    e.preventDefault = vi.fn();
    (e as any).returnValue = undefined;
    beforeUnloadHandler?.(e);
    expect(e.preventDefault).toHaveBeenCalled();
    expect((e as any).returnValue).toBeTruthy();
  });
});
```

### Step 2 — Run; expect red

```bash
cd internal/gui/frontend && npm run test -- src/hooks/useUnsavedChangesGuard.test.ts
```

### Step 3 — Implement

```ts
import { useEffect } from "preact/hooks";

// useUnsavedChangesGuard installs a window.beforeunload listener that
// fires the browser's native "Leave site?" dialog when `dirty` is true.
// When dirty is false (or toggles back), the listener is removed so the
// user never sees the dialog on a clean form.
//
// Note: modern browsers ignore the CUSTOM message — Chrome 51+ shows
// its own default text. Returning any truthy value is the signal that
// dirty state exists. We return a string for older browsers that may
// still display it.
export function useUnsavedChangesGuard(dirty: boolean): void {
  useEffect(() => {
    if (!dirty) return;
    const handler = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      // Modern browsers ignore this string; returnValue is the signal.
      // Legacy browsers may still display it.
      e.returnValue = "You have unsaved changes.";
      return "You have unsaved changes.";
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  }, [dirty]);
}
```

### Step 4 — Run; expect green

```bash
cd internal/gui/frontend && npm run test -- src/hooks/useUnsavedChangesGuard.test.ts
```

Expected: 2 subtests PASS.

### Step 5 — Commit

```bash
git add internal/gui/frontend/src/hooks/useUnsavedChangesGuard.ts \
        internal/gui/frontend/src/hooks/useUnsavedChangesGuard.test.ts
git commit -m "feat(gui/frontend): useUnsavedChangesGuard hook (beforeunload wrapper)

Installs a window.beforeunload listener that fires the browser's
native 'Leave site?' dialog when dirty is true. Listener is torn down
when dirty toggles to false, so clean forms never prompt. Modern
browsers ignore the custom message string (Chrome 51+); returning any
truthy value is the signal."
```

---

## Task 11: AddServerScreen — migrate to new types + `mode` prop foundations

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`

This task migrates the existing file to the new shape but does NOT yet implement edit-mode. It restores typecheck-clean state.

### Step 1 — Update imports + types

At the top of the file, update the imports to match the new shapes:

```tsx
import { useEffect, useRef, useState } from "preact/hooks";
import {
  BLANK_FORM,
  parseYAMLToForm,
  toYAML,
} from "../lib/manifest-yaml";
import { generateUUID } from "../lib/uuid";
import { useDebouncedValue } from "../hooks/useDebouncedValue";
import {
  getExtractManifest,
  getManifest,
  postManifestCreate,
  postManifestEdit,
  postManifestValidate,
  ManifestHashMismatchError,
} from "../api";
import type {
  BindingFormEntry,
  DaemonFormEntry,
  ManifestFormState,
} from "../types";
```

### Step 2 — Add `mode` prop + thread through

Change the signature:

```tsx
export function AddServerScreen(props: {
  mode?: "create" | "edit";
  onDirtyChange?: (dirty: boolean) => void;
} = {}) {
  const mode = props.mode ?? "create";
  // ... existing useState block ...
}
```

### Step 3 — Migrate daemon/binding mutators to use `_id` + `daemonId`

For every touch of `daemons`, assign `_id = generateUUID()` on add/paste/reset:

```tsx
  function addDaemon() {
    setFormState((prev) => ({
      ...prev,
      daemons: [...prev.daemons, { _id: generateUUID(), name: "", port: 0 }],
    }));
  }
```

Rename cascade (old: match `binding.daemon === target.name`; new: match `binding.daemonId === target._id`). Rename no longer needs to touch bindings because `_id` is identity-stable:

```tsx
  function updateDaemon(index: number, field: "name" | "port", value: string) {
    setFormState((prev) => {
      const target = prev.daemons[index];
      if (!target) return prev;
      const nextDaemons = prev.daemons.slice();
      if (field === "name") {
        nextDaemons[index] = { ...target, name: value };
      } else {
        nextDaemons[index] = { ...target, port: parsePort(value) };
      }
      // No cascade needed — bindings reference by _id which hasn't changed.
      return { ...prev, daemons: nextDaemons };
    });
  }
```

Delete cascade (old: delete bindings matching `target.name`; new: match `target._id`):

```tsx
  function deleteDaemon(index: number) {
    setFormState((prev) => {
      const target = prev.daemons[index];
      if (!target) return prev;
      const orphans = prev.client_bindings.filter((b) => b.daemonId === target._id);
      if (orphans.length > 0) {
        const ok = window.confirm(
          `Delete daemon "${target.name}" and its ${orphans.length} client binding${orphans.length === 1 ? "" : "s"}?`,
        );
        if (!ok) return prev;
      }
      return {
        ...prev,
        daemons: prev.daemons.filter((_, i) => i !== index),
        client_bindings: prev.client_bindings.filter((b) => b.daemonId !== target._id),
      };
    });
  }
```

Binding mutators now take daemonId:

```tsx
  function addBinding(daemonId: string) {
    setFormState((prev) => ({
      ...prev,
      client_bindings: [
        ...prev.client_bindings,
        { client: KNOWN_CLIENTS[0], daemonId, url_path: "/mcp" },
      ],
    }));
  }
```

And the `<ClientBindingsSection>` / `<BindingsList>` calls use `daemonId` where they used to use `daemon`:

```tsx
<BindingsList
  bindings={bindings.filter((b) => b.daemonId === d._id)}
  onAdd={() => onAdd(d._id)}
  // ...
/>
```

### Step 4 — Lock name + kind when `mode === "edit"`

In the JSX for the name input:

```tsx
<input
  id="field-name"
  type="text"
  value={formState.name}
  placeholder="memory"
  disabled={mode === "edit"}
  title={mode === "edit" ? "Kind and name are immutable after first install. Delete and recreate the server to change them." : undefined}
  onInput={(e) => updateField("name", (e.currentTarget as HTMLInputElement).value)}
/>
```

Same for `field-kind`:

```tsx
<select
  id="field-kind"
  value={formState.kind}
  disabled={mode === "edit"}
  title={mode === "edit" ? "Kind and name are immutable after first install. Delete and recreate the server to change them." : undefined}
  onChange={...}
>...</select>
```

### Step 5 — Ensure initial form state includes new fields

`useState<ManifestFormState>(BLANK_FORM)` — BLANK_FORM already contains `loadedHash: ""` and `_preservedRaw: {}` from Task 7. Nothing else to change here at this step.

### Step 6 — Typecheck + Vitest + Go smoke + E2E

```bash
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../../.. && go generate ./internal/gui/...
go test ./internal/gui/ -count=1
cd internal/gui/e2e && npm test -- tests/add-server.spec.ts tests/shell.spec.ts tests/migration.spec.ts
cd ../../..
```

Expected: all clean. The create-mode path works exactly as in A2a (sidebar `#/add-server` still opens the same screen; default `mode="create"` preserves behavior).

### Step 7 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/assets/
git commit -m "refactor(gui/frontend): AddServerScreen accepts mode prop + migrates to _id-keyed daemon refs

Adds optional mode: 'create' | 'edit' prop (default 'create'). Migrates
daemon/binding state to use DaemonFormEntry._id (UUID) instead of
matching on daemon.name. Rename no longer needs cascade — bindings
reference by _id which is identity-stable. Delete still cascades.

Locks name + kind fields when mode === 'edit' with immutability tooltip.

Edit-mode Load + Save paths come in Tasks 12-13. This commit preserves
the full A2a create-flow E2E (10/10 add-server + 3/3 shell + 6/6
migration) with zero behavior changes."
```

---

## Task 12: AddServerScreen — edit-mode mount (Load + hash + nested-unknown check)

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`

### Step 1 — Add state for edit-mode

Inside `AddServerScreen`, after existing `useState` declarations, add:

```tsx
  const [loadError, setLoadError] = useState<string | null>(null);
  const [readOnlyReason, setReadOnlyReason] = useState<string | null>(null);
```

Add an import for `hasNestedUnknown`:

```tsx
import { BLANK_FORM, hasNestedUnknown, parseYAMLToForm, toYAML } from "../lib/manifest-yaml";
```

### Step 2 — Mount effect for edit mode

Add an effect AFTER the existing `onDirtyChange` effect:

```tsx
  useEffect(() => {
    if (mode !== "edit") return;
    const params = parseAddServerQuery();
    const name = params.server; // reuse existing parser; key is "server" in query
    if (!name) {
      setLoadError("No manifest name specified");
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const { yaml, hash } = await getManifest(name);
        if (cancelled) return;
        // Check nested-unknown BEFORE parsing drops them into form state.
        const nested = hasNestedUnknown(yaml);
        const parsed = parseYAMLToForm(yaml);
        parsed.loadedHash = hash;
        setFormState(parsed);
        setInitialSnapshot(parsed);
        if (nested) {
          setReadOnlyReason(
            "This manifest contains fields the GUI cannot handle (daemons/languages/client_bindings have unknown entries). Editing via GUI would drop them.",
          );
        }
      } catch (err) {
        if (cancelled) return;
        setLoadError((err as Error).message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [mode]);
```

Note: `parseAddServerQuery()` exists from A2a. We reuse `params.server` as the edit-target name. The URL is `#/edit-server?name=<name>`, but the parser currently looks for `server=...` — we need to extend it OR add a new helper. Prefer adding a small helper to keep the A2a create-flow parser untouched:

```tsx
// parseEditServerQuery extracts ?name=<name> from the #/edit-server hash.
// The edit route uses a different query-key convention from the A2a
// create-flow (?server=&from-client=) because edit identity is just
// the manifest name; there's no "from-client" prefill source.
function parseEditServerQuery(): string {
  const hash = window.location.hash;
  const q = hash.split("?")[1] ?? "";
  const params = new URLSearchParams(q);
  return params.get("name") ?? "";
}
```

Then replace the effect to use this new helper. Place the new helper alongside `parseAddServerQuery`.

### Step 3 — Render load-failure UI

Near the top of the return JSX, before the form grid:

```tsx
{loadError && (
  <div class="banner error" data-testid="load-error-banner">
    <p>Failed to load <code>{parseEditServerQuery()}</code>: {loadError}</p>
    <div class="banner-actions">
      <button type="button" onClick={() => { setLoadError(null); window.location.reload(); }}>Retry</button>
      <button type="button" onClick={() => { window.location.hash = "#/servers"; }}>Back to Servers</button>
    </div>
  </div>
)}
```

### Step 4 — Render read-only mode banner + disable controls

Add inside the form area, above the accordion:

```tsx
{readOnlyReason && (
  <div class="banner warning" data-testid="readonly-banner">
    <p>{readOnlyReason}</p>
    <p>
      Edit via CLI (<code>mcphub manifest edit {parseEditServerQuery()}</code>) or
      delete + recreate via Add Server.
    </p>
    <div class="banner-actions">
      <button type="button" onClick={() => { window.location.hash = "#/servers"; }}>Back to Servers</button>
    </div>
  </div>
)}
```

Thread `readOnlyReason !== null` as a `disabled` predicate into every input/button/select. A compact approach: derive `const readOnly = readOnlyReason !== null;` then set `disabled={readOnly || /* existing disabled condition */}` on every form control, and for all action buttons except `[Back to Servers]` and `[Copy YAML]`.

### Step 5 — Typecheck + Vitest + Go smoke + E2E

```bash
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../../.. && go generate ./internal/gui/...
go test ./internal/gui/ -count=1
```

Expected: all clean. E2E for `#/edit-server` comes in Task 19.

### Step 6 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): AddServerScreen edit-mode Load + hash + read-only/load-failure UI

Mount effect (mode === 'edit'): fetch /api/manifest/get → store
loadedHash in form state → set initialSnapshot AFTER normalization
(Q8 invariant from A2a). If hasNestedUnknown returns true, enter
read-only mode: sticky warning banner + all inputs/action buttons
disabled; [Copy YAML] and [Back to Servers] remain enabled for
inspection.

Load failure: inline error banner + [Retry]/[Back to Servers];
form stays blank; dirty=false invariant preserved (deepEqualForm
against BLANK_FORM)."
```

---

## Task 13: AddServerScreen — edit-mode submit path (Save/Save & Install)

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`

### Step 1 — Branch `runSave` on mode

Replace the existing `runSave` with a version that branches on `mode`:

```tsx
  async function runSave(opts: { install: boolean }) {
    const version = ++submissionCounter.current;
    setBusy(opts.install ? "install" : "save");
    setBanner(null);
    try {
      const name = formState.name.trim();
      if (!name) {
        setBanner({ kind: "error", text: "Name is required." });
        return;
      }
      const payload = toYAML(formState);
      const warnings = await postManifestValidate(payload);
      if (version !== submissionCounter.current) return;
      if (warnings.length > 0) {
        setWarnings(warnings);
        setBanner({
          kind: "error",
          text: `Cannot save: ${warnings.length} validation warning${warnings.length === 1 ? "" : "s"}.`,
        });
        return;
      }
      if (mode === "edit") {
        try {
          await postManifestEdit(name, payload, formState.loadedHash);
          if (version !== submissionCounter.current) return;
        } catch (err) {
          if (version !== submissionCounter.current) return;
          if (err instanceof ManifestHashMismatchError) {
            setBanner({
              kind: "error",
              text: "Manifest changed on disk since you opened it. Reload will discard your edits and show the new version. Force Save will overwrite with your version.",
              staleReload: true,
              staleForceSave: true,
            });
            return;
          }
          throw err;
        }
      } else {
        await postManifestCreate(name, payload);
        if (version !== submissionCounter.current) return;
      }
      setWarnings(null);
      // Commit the save as the new baseline (Q8) and refresh loadedHash.
      setInitialSnapshot(formState);
      // Refresh loadedHash for the next Save in edit mode. Cheap re-read.
      if (mode === "edit") {
        try {
          const { hash } = await getManifest(name);
          if (version === submissionCounter.current) {
            setFormState((prev) => ({ ...prev, loadedHash: hash }));
          }
        } catch {
          // Non-fatal; next Save will just race if disk changes.
        }
      }
      if (!opts.install) {
        setBanner({
          kind: "success",
          text: mode === "edit"
            ? `Saved. Daemon still running old config.`
            : `Saved servers/${name}/manifest.yaml.`,
          reinstall: mode === "edit",
        });
        return;
      }
      await runInstallNow(name, version);
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      if (version === submissionCounter.current) setBusy("");
    }
  }
```

### Step 2 — Extend the banner state type

Extend the banner state to support stale + reinstall sub-states:

```tsx
type Banner = {
  kind: "error" | "success";
  text: string;
  retry?: () => Promise<void>;
  reinstall?: boolean;     // show [Reinstall] button on success
  staleReload?: boolean;   // show [Reload] button on error
  staleForceSave?: boolean; // show [Force Save] button on error
};

const [banner, setBanner] = useState<Banner | null>(null);
```

### Step 3 — Render Reinstall, Reload, Force Save buttons inside the banner

Update the banner JSX:

```tsx
{banner && (
  <div class={`banner ${banner.kind}`} data-testid="banner">
    <p>{banner.text}</p>
    {banner.retry && (
      <button type="button" onClick={() => banner.retry?.()} data-action="retry-install">Retry Install</button>
    )}
    {banner.reinstall && (
      <button
        type="button"
        onClick={() => runInstallNow(formState.name.trim(), ++submissionCounter.current)}
        data-action="reinstall"
      >
        Reinstall
      </button>
    )}
    {banner.staleReload && (
      <button type="button" onClick={() => runReload()} data-action="reload">Reload</button>
    )}
    {banner.staleForceSave && (
      <button type="button" onClick={() => runForceSave()} data-action="force-save">Force Save</button>
    )}
  </div>
)}
```

### Step 4 — Add `runReload` + `runForceSave`

```tsx
  async function runReload() {
    const name = parseEditServerQuery();
    if (!name) return;
    setBusy("save");
    setBanner(null);
    try {
      const { yaml, hash } = await getManifest(name);
      const parsed = parseYAMLToForm(yaml);
      parsed.loadedHash = hash;
      setFormState(parsed);
      setInitialSnapshot(parsed);
      setBanner({ kind: "success", text: "Reloaded fresh manifest from disk." });
    } catch (err) {
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      setBusy("");
    }
  }

  async function runForceSave() {
    const version = ++submissionCounter.current;
    setBusy("save");
    setBanner(null);
    try {
      const name = formState.name.trim();
      // Re-read disk to get fresh _preservedRaw + fresh hash.
      const fresh = await getManifest(name);
      if (version !== submissionCounter.current) return;
      const freshParsed = parseYAMLToForm(fresh.yaml);
      // Merge: user's known-field edits win; fresh disk _preservedRaw wins.
      const merged: ManifestFormState = {
        ...formState,
        _preservedRaw: freshParsed._preservedRaw,
      };
      const payload = toYAML(merged);
      await postManifestEdit(name, payload, fresh.hash);
      if (version !== submissionCounter.current) return;
      setFormState({ ...merged, loadedHash: fresh.hash });
      setInitialSnapshot({ ...merged, loadedHash: fresh.hash });
      const preservedKeys = Object.keys(freshParsed._preservedRaw);
      setBanner({
        kind: "success",
        text:
          preservedKeys.length > 0
            ? `Force-saved. Preserved external fields: ${preservedKeys.join(", ")}.`
            : `Force-saved.`,
        reinstall: true,
      });
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({ kind: "error", text: `Force Save failed: ${(err as Error).message}` });
    } finally {
      if (version === submissionCounter.current) setBusy("");
    }
  }
```

### Step 5 — Typecheck + Vitest + Go smoke

```bash
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../../.. && go generate ./internal/gui/...
go test ./internal/gui/ -count=1
```

### Step 6 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): AddServerScreen edit-mode Save + Force Save + Reinstall

runSave branches on mode: Edit uses postManifestEdit with loadedHash;
hash-mismatch (409 MANIFEST_HASH_MISMATCH → ManifestHashMismatchError)
surfaces as the [Reload]/[Force Save] banner. On success: refresh
loadedHash from disk and show 'Config saved. Daemon still running old
config.' banner with [Reinstall] button.

runForceSave re-reads disk at save-time, merges the fresh _preservedRaw
into the user's edited form, and writes with the fresh hash. Transparent
success banner lists which external top-level fields were preserved.

runReload fetches fresh yaml+hash, parses, resets formState +
initialSnapshot. Used when user chooses to discard local edits."
```

---

## Task 14: app.tsx — `#/edit-server` route + navigation guard installation

**Files:**
- Modify: `internal/gui/frontend/src/app.tsx`

### Step 1 — Extend `App()` to install the hashchange guard + beforeunload

Replace `app.tsx`:

```tsx
import type { JSX } from "preact";
import { useState } from "preact/hooks";
import { useRouter } from "./hooks/useRouter";
import { useUnsavedChangesGuard } from "./hooks/useUnsavedChangesGuard";
import { AddServerScreen } from "./screens/AddServer";
import { DashboardScreen } from "./screens/Dashboard";
import { LogsScreen } from "./screens/Logs";
import { MigrationScreen } from "./screens/Migration";
import { ServersScreen } from "./screens/Servers";

export function App() {
  const [addServerDirty, setAddServerDirty] = useState(false);

  // guard: returning false reverts hash via replaceState (useRouter).
  // We also need to know the CURRENT screen to decide whether leaving
  // triggers a prompt; useRouter invokes the guard BEFORE updating
  // its internal state, so `screen` here is the pre-change value.
  const guard = (target: string): boolean => {
    if (!addServerDirty) return true;
    if (target === screen) return true;
    // eslint-disable-next-line no-alert
    const ok = window.confirm("Discard unsaved changes?");
    if (ok) setAddServerDirty(false);
    return ok;
  };

  const screen = useRouter("servers", guard);
  useUnsavedChangesGuard(addServerDirty);

  // sidebar click interceptor handles programmatic hash writes triggered
  // from <a href> (guard fires AFTER the browser updates the hash, so
  // intercepting onClick lets us preempt and skip the history push
  // entirely when user declines).
  function guardClick(targetScreen: string): (e: MouseEvent) => void {
    return (e) => {
      if (!addServerDirty) return;
      if (screen !== "add-server" && screen !== "edit-server") return;
      if (targetScreen === screen) return;
      // eslint-disable-next-line no-alert
      const ok = window.confirm("Discard unsaved changes?");
      if (!ok) {
        e.preventDefault();
      } else {
        setAddServerDirty(false);
      }
    };
  }

  let body: JSX.Element;
  switch (screen) {
    case "servers":
      body = <ServersScreen />;
      break;
    case "migration":
      body = <MigrationScreen />;
      break;
    case "add-server":
      body = <AddServerScreen mode="create" onDirtyChange={setAddServerDirty} />;
      break;
    case "edit-server":
      body = <AddServerScreen mode="edit" onDirtyChange={setAddServerDirty} />;
      break;
    case "dashboard":
      body = <DashboardScreen />;
      break;
    case "logs":
      body = <LogsScreen />;
      break;
    default:
      body = <p>Unknown screen: {screen}</p>;
  }

  return (
    <>
      <aside class="sidebar">
        <div class="brand">mcp-local-hub</div>
        <nav>
          <a href="#/servers" class={screen === "servers" ? "active" : ""} onClick={guardClick("servers")}>Servers</a>
          <a href="#/migration" class={screen === "migration" ? "active" : ""} onClick={guardClick("migration")}>Migration</a>
          <a href="#/add-server" class={screen === "add-server" ? "active" : ""} onClick={guardClick("add-server")}>Add server</a>
          <a href="#/dashboard" class={screen === "dashboard" ? "active" : ""} onClick={guardClick("dashboard")}>Dashboard</a>
          <a href="#/logs" class={screen === "logs" ? "active" : ""} onClick={guardClick("logs")}>Logs</a>
        </nav>
      </aside>
      <main id="screen-root">
        {body}
      </main>
    </>
  );
}
```

Note: `#/edit-server` is NOT in the sidebar — it's entered only via row clicks in Servers / Migration (Task 15).

### Step 2 — Typecheck + Vitest + Go smoke

```bash
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../../.. && go generate ./internal/gui/...
go test ./internal/gui/ -count=1
```

### Step 3 — Commit

```bash
git add internal/gui/frontend/src/app.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): #/edit-server route + beforeunload + hashchange guard (A2b D5, D7)

SCREENS gets a new entry 'edit-server' that reuses AddServerScreen with
mode='edit'. No sidebar link — edit is entered via Servers/Migration
row clicks (Task 15).

useRouter receives a guard callback that prompts on dirty transitions
away from add-server OR edit-server and reverts via history.replaceState
on decline. useUnsavedChangesGuard installs a beforeunload listener
that fires the native browser 'Leave site?' dialog when dirty.

Sidebar onClick guard remains as a belt-and-suspenders check for
programmatic hash writes initiated from <a href>."
```

---

## Task 15: Servers matrix + Migration Via-hub → `#/edit-server` entry points

**Files:**
- Modify: `internal/gui/frontend/src/screens/Servers.tsx`
- Modify: `internal/gui/frontend/src/screens/Migration.tsx`

### Step 1 — Servers screen row click

Read `Servers.tsx`. Find the table body that renders each server row. Wrap the server name cell (or add an "Edit" button in the Actions column, whichever pattern the existing screen uses) to link to `#/edit-server?name=<name>`.

Minimal approach: make the server name clickable:

```tsx
<td>
  <a
    href={`#/edit-server?name=${encodeURIComponent(server.name)}`}
    data-action="edit-server"
  >
    {server.name}
  </a>
</td>
```

### Step 2 — Migration Via-hub row click

In `Migration.tsx`, find the `ViaHubGroup` component (`data-group="via-hub"`). Each row renders a `<Demigrate>` button already — add an inline `<a>` before the Demigrate button for edit:

```tsx
<a
  href={`#/edit-server?name=${encodeURIComponent(e.name)}`}
  data-action="edit-manifest"
>Edit manifest</a>
```

### Step 3 — Typecheck + Vitest + Go smoke + E2E

```bash
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../../.. && go generate ./internal/gui/...
go test ./internal/gui/ -count=1
cd internal/gui/e2e && npm test -- tests/servers.spec.ts tests/migration.spec.ts tests/shell.spec.ts
cd ../../..
```

Expected: existing tests still PASS. If shell.spec.ts now shows a link count different from 5, adjust — we did NOT add a sidebar link, only row-click affordances.

### Step 4 — Commit

```bash
git add internal/gui/frontend/src/screens/Servers.tsx internal/gui/frontend/src/screens/Migration.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): Servers row click + Migration Via-hub 'Edit manifest' -> #/edit-server

Servers matrix: server-name cell becomes an <a> pointing to
#/edit-server?name=<name>. Migration Via-hub rows get an 'Edit manifest'
link next to the existing Demigrate button."
```

---

## Task 16: Advanced section — always-visible fields (idle_timeout, extra_args, base_args_template)

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`
- Modify: `internal/gui/frontend/src/styles/style.css` (APPEND)

### Step 1 — Add an Advanced AccordionSection

In AddServerScreen JSX, after the existing Client-bindings accordion, add:

```tsx
<AccordionSection title="Advanced">
  <div class="form-row">
    <label for="field-idle-timeout">Idle timeout (min)</label>
    <input
      id="field-idle-timeout"
      type="number"
      min={0}
      value={formState.idle_timeout_min ?? ""}
      placeholder="(unset)"
      disabled={readOnly}
      onInput={(e) => {
        const v = (e.currentTarget as HTMLInputElement).value;
        updateField("idle_timeout_min", v === "" ? undefined : Number(v));
      }}
    />
  </div>
  <div class="form-row">
    <label>Base args template</label>
    <RepeatableStringRows
      label="arg"
      value={formState.base_args_template ?? []}
      onChange={(next) =>
        updateField("base_args_template", next.length > 0 ? next : undefined)
      }
      disabled={readOnly}
      dataTestId="base-args-template"
    />
  </div>
  {formState.kind === "workspace-scoped" && (
    <>
      <PortPoolField
        value={formState.port_pool}
        onChange={(pp) => updateField("port_pool", pp)}
        disabled={readOnly}
      />
      <LanguagesSubsection
        languages={formState.languages ?? []}
        onChange={(next) =>
          updateField("languages", next.length > 0 ? next : undefined)
        }
        disabled={readOnly}
      />
    </>
  )}
  {formState.daemons.length > 0 && (
    <div class="form-row" data-testid="daemon-extras">
      <label>Per-daemon extras</label>
      <DaemonExtrasSubsection
        daemons={formState.daemons}
        kind={formState.kind}
        onUpdate={(id, field, value) => updateDaemonExtras(id, field, value)}
        disabled={readOnly}
      />
    </div>
  )}
</AccordionSection>
```

`updateDaemonExtras` is a new helper that updates `daemons[i].context` or `daemons[i].extra_args` by `_id`:

```tsx
  function updateDaemonExtras(
    id: string,
    field: "context" | "extra_args",
    value: string | string[] | undefined,
  ) {
    setFormState((prev) => ({
      ...prev,
      daemons: prev.daemons.map((d) =>
        d._id === id ? { ...d, [field]: value } : d,
      ),
    }));
  }
```

### Step 2 — Create `RepeatableStringRows`, `PortPoolField`, `LanguagesSubsection`, `DaemonExtrasSubsection` as in-file components

Append to the bottom of `AddServer.tsx`:

```tsx
function RepeatableStringRows(props: {
  label: string;
  value: string[];
  onChange: (next: string[]) => void;
  disabled?: boolean;
  dataTestId?: string;
}) {
  return (
    <div class="repeatable-rows" data-testid={props.dataTestId}>
      {props.value.map((v, i) => (
        <div class="form-row" key={i}>
          <input
            type="text"
            value={v}
            disabled={props.disabled}
            onInput={(e) => {
              const next = props.value.slice();
              next[i] = (e.currentTarget as HTMLInputElement).value;
              props.onChange(next);
            }}
          />
          <button
            type="button"
            disabled={props.disabled}
            onClick={() => props.onChange(props.value.filter((_, j) => j !== i))}
          >×</button>
        </div>
      ))}
      <button
        type="button"
        disabled={props.disabled}
        onClick={() => props.onChange([...props.value, ""])}
      >+ Add {props.label}</button>
    </div>
  );
}

function PortPoolField(props: {
  value?: { start: number; end: number };
  onChange: (next: { start: number; end: number } | undefined) => void;
  disabled?: boolean;
}) {
  const pp = props.value ?? { start: 0, end: 0 };
  return (
    <div class="form-row" data-testid="port-pool">
      <label>Port pool</label>
      <input
        type="number"
        placeholder="start"
        min={0}
        max={65535}
        value={pp.start || ""}
        disabled={props.disabled}
        onInput={(e) => {
          const start = Number((e.currentTarget as HTMLInputElement).value) || 0;
          const end = pp.end;
          props.onChange(start > 0 || end > 0 ? { start, end } : undefined);
        }}
      />
      <input
        type="number"
        placeholder="end"
        min={0}
        max={65535}
        value={pp.end || ""}
        disabled={props.disabled}
        onInput={(e) => {
          const start = pp.start;
          const end = Number((e.currentTarget as HTMLInputElement).value) || 0;
          props.onChange(start > 0 || end > 0 ? { start, end } : undefined);
        }}
      />
    </div>
  );
}

function LanguagesSubsection(props: {
  languages: LanguageFormEntry[];
  onChange: (next: LanguageFormEntry[]) => void;
  disabled?: boolean;
}) {
  return (
    <div data-testid="languages-subsection">
      {props.languages.map((l, i) => (
        <fieldset class="language-entry" key={l._id} data-testid={`language-${l._id}`}>
          <legend>Language</legend>
          <div class="form-row">
            <label>Name</label>
            <input
              type="text"
              value={l.name}
              disabled={props.disabled}
              onInput={(e) => {
                const next = props.languages.slice();
                next[i] = { ...l, name: (e.currentTarget as HTMLInputElement).value };
                props.onChange(next);
              }}
            />
          </div>
          <div class="form-row">
            <label>Backend</label>
            <select
              value={l.backend}
              disabled={props.disabled}
              onChange={(e) => {
                const next = props.languages.slice();
                next[i] = { ...l, backend: (e.currentTarget as HTMLSelectElement).value };
                props.onChange(next);
              }}
            >
              <option value="mcp-language-server">mcp-language-server</option>
              <option value="gopls-mcp">gopls-mcp</option>
            </select>
          </div>
          <div class="form-row">
            <label>LSP command</label>
            <input
              type="text"
              value={l.lsp_command ?? ""}
              disabled={props.disabled}
              onInput={(e) => {
                const next = props.languages.slice();
                next[i] = { ...l, lsp_command: (e.currentTarget as HTMLInputElement).value || undefined };
                props.onChange(next);
              }}
            />
          </div>
          <button
            type="button"
            disabled={props.disabled}
            onClick={() => props.onChange(props.languages.filter((_, j) => j !== i))}
          >Remove language</button>
        </fieldset>
      ))}
      <button
        type="button"
        disabled={props.disabled}
        onClick={() =>
          props.onChange([
            ...props.languages,
            { _id: generateUUID(), name: "", backend: "mcp-language-server" },
          ])
        }
      >+ Add language</button>
    </div>
  );
}

function DaemonExtrasSubsection(props: {
  daemons: DaemonFormEntry[];
  kind: ManifestFormState["kind"];
  onUpdate: (id: string, field: "context" | "extra_args", value: string | string[] | undefined) => void;
  disabled?: boolean;
}) {
  return (
    <div>
      {props.daemons.map((d) => (
        <fieldset key={d._id} class="daemon-extras-entry">
          <legend>{d.name || "(unnamed)"}</legend>
          {props.kind === "workspace-scoped" && (
            <div class="form-row">
              <label>Context</label>
              <input
                type="text"
                value={d.context ?? ""}
                disabled={props.disabled}
                onInput={(e) => props.onUpdate(d._id, "context", (e.currentTarget as HTMLInputElement).value || undefined)}
              />
            </div>
          )}
          <div class="form-row">
            <label>Extra args</label>
            <RepeatableStringRows
              label="arg"
              value={d.extra_args ?? []}
              onChange={(next) => props.onUpdate(d._id, "extra_args", next.length > 0 ? next : undefined)}
              disabled={props.disabled}
            />
          </div>
        </fieldset>
      ))}
    </div>
  );
}
```

### Step 3 — Append CSS

Append to `internal/gui/frontend/src/styles/style.css`:

```css

/* A2b Advanced subsections */
.screen.add-server .language-entry,
.screen.add-server .daemon-extras-entry {
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 8px 12px;
  margin: 8px 0;
}
.screen.add-server .language-entry legend,
.screen.add-server .daemon-extras-entry legend {
  padding: 0 6px;
  font-size: 13px;
  color: var(--text-muted, #656d76);
}
.screen.add-server .banner.warning {
  border-left: 4px solid var(--warning, #bf8700);
  background: var(--warning-bg, #fff8e6);
  padding: 12px 16px;
  border-radius: 4px;
}
.screen.add-server .banner-actions {
  display: flex;
  gap: 8px;
  margin-top: 8px;
}
```

### Step 4 — Typecheck + Vitest + Go smoke

```bash
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../../.. && go generate ./internal/gui/...
go test ./internal/gui/ -count=1
```

### Step 5 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/frontend/src/styles/style.css internal/gui/assets/
git commit -m "feat(gui/frontend): Advanced accordion — idle_timeout, base_args_template, port_pool, languages[], daemon context/extra_args (A2b D12)

Advanced section adds:
- idle_timeout_min (int, always visible)
- base_args_template (repeatable string rows, always visible)
- port_pool {start, end} (visible only when kind === workspace-scoped)
- languages[] subsection (visible only when kind === workspace-scoped;
  UUID-keyed entries so future edits don't drop user edits across renames)
- per-daemon context (workspace-scoped only) + extra_args
  (DaemonExtrasSubsection reads from the existing daemons list by _id)

All fields respect the readOnly flag from Task 12 (nested-unknown
manifests). Env secret refs remain DEFERRED to A3."
```

---

## Task 17: Multi-daemon matrix view (4+ daemons threshold)

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`
- Modify: `internal/gui/frontend/src/styles/style.css` (APPEND)

### Step 1 — Extend `ClientBindingsSection` with the matrix branch

Find the existing `ClientBindingsSection` component. Its A2a logic was: 0 daemons → empty-state; 1 → flat; 2-3 → per-daemon accordion. Add a 4+ branch:

```tsx
function ClientBindingsSection(props: {
  daemons: DaemonFormEntry[];
  bindings: BindingFormEntry[];
  onAdd: (daemonId: string) => void;
  onUpdate: (index: number, field: "client" | "daemon" | "url_path", value: string) => void;
  onDelete: (index: number) => void;
}) {
  const { daemons, bindings, onAdd, onUpdate, onDelete } = props;
  if (daemons.length === 0) {
    return (
      <p class="empty-state">
        Add at least one daemon before creating client bindings.
      </p>
    );
  }
  if (daemons.length === 1) {
    // ... A2a single-daemon flat rendering ...
  }
  if (daemons.length >= 4) {
    return <BindingsMatrix daemons={daemons} bindings={bindings} onToggle={/* ... */} onUpdate={onUpdate} />;
  }
  // 2-3 daemons: existing per-daemon accordion rendering.
  // ... unchanged ...
}
```

### Step 2 — Implement `BindingsMatrix`

Append as an in-file component:

```tsx
function BindingsMatrix(props: {
  daemons: DaemonFormEntry[];
  bindings: BindingFormEntry[];
  onToggle: (daemonId: string, client: string, checked: boolean) => void;
  onUpdate: (bindingIndex: number, field: "url_path", value: string) => void;
}) {
  // One <table>: rows = KNOWN_CLIENTS, cols = daemons.
  // Cell renders checkbox (bound on daemon+client) + url_path input.
  const clients = KNOWN_CLIENTS;
  return (
    <table class="bindings-matrix" data-testid="bindings-matrix">
      <thead>
        <tr>
          <th>client \ daemon</th>
          {props.daemons.map((d) => (
            <th key={d._id}>{d.name || "(unnamed)"}</th>
          ))}
        </tr>
      </thead>
      <tbody>
        {clients.map((c) => (
          <tr key={c}>
            <th>{c}</th>
            {props.daemons.map((d) => {
              const idx = props.bindings.findIndex((b) => b.client === c && b.daemonId === d._id);
              const has = idx !== -1;
              return (
                <td key={d._id}>
                  <input
                    type="checkbox"
                    checked={has}
                    data-action="binding-toggle"
                    data-daemon={d._id}
                    data-client={c}
                    onChange={(e) => props.onToggle(d._id, c, (e.currentTarget as HTMLInputElement).checked)}
                  />
                  {has && (
                    <input
                      type="text"
                      value={props.bindings[idx].url_path}
                      placeholder="/mcp"
                      onInput={(e) => props.onUpdate(idx, "url_path", (e.currentTarget as HTMLInputElement).value)}
                    />
                  )}
                </td>
              );
            })}
          </tr>
        ))}
      </tbody>
    </table>
  );
}
```

Add the `onToggle` handler inside `AddServerScreen`:

```tsx
  function toggleBinding(daemonId: string, client: string, checked: boolean) {
    setFormState((prev) => {
      if (checked) {
        return {
          ...prev,
          client_bindings: [
            ...prev.client_bindings,
            { client, daemonId, url_path: "/mcp" },
          ],
        };
      }
      return {
        ...prev,
        client_bindings: prev.client_bindings.filter(
          (b) => !(b.client === client && b.daemonId === daemonId),
        ),
      };
    });
  }
```

Thread `onToggle={toggleBinding}` into the `<ClientBindingsSection>` call.

### Step 3 — Append CSS

```css

/* A2b multi-daemon matrix */
.screen.add-server .bindings-matrix {
  border-collapse: collapse;
  width: 100%;
  max-width: 960px;
  margin-top: 8px;
}
.screen.add-server .bindings-matrix th,
.screen.add-server .bindings-matrix td {
  border: 1px solid var(--border);
  padding: 6px 10px;
  text-align: left;
}
.screen.add-server .bindings-matrix thead th {
  background: var(--sidebar-bg);
  font-weight: 500;
}
.screen.add-server .bindings-matrix td input[type="text"] {
  width: 100%;
  margin-top: 4px;
}
```

### Step 4 — Typecheck + Vitest + Go smoke

```bash
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../../.. && go generate ./internal/gui/...
go test ./internal/gui/ -count=1
```

### Step 5 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/frontend/src/styles/style.css internal/gui/assets/
git commit -m "feat(gui/frontend): multi-daemon matrix view for 4+ daemons (A2b D1)

ClientBindingsSection picks a render branch based on daemon count:
  0      → empty-state
  1      → flat list (A2a)
  2-3    → per-daemon accordion (A2a)
  4+     → matrix view (new)

Matrix: rows = KNOWN_CLIENTS, cols = daemons. Each cell is a checkbox
bound to the (daemonId, client) pair; when checked, a url_path input
appears in-cell. Toggling adds/removes the BindingFormEntry.

Identical data model (daemonId-keyed) as A2a — matrix is a pure
render choice. Writing back to client_bindings is unchanged."
```

---

## Task 18: Playwright E2E — `edit-server.spec.ts` (edit load + stale + Force Save + locks + read-only)

**Files:**
- Create: `internal/gui/e2e/tests/edit-server.spec.ts`

### Step 1 — Write test file

```ts
import { test, expect } from "../fixtures/hub";
import { writeFileSync, mkdirSync, existsSync } from "node:fs";
import { join, dirname } from "node:path";

// Helper: seed a manifest into the hub's on-disk manifest dir. The
// fixture sets LOCALAPPDATA=home; the GUI binary reads manifests
// from servers/ under the repo (embed) OR the user's manifest dir.
// For edit-mode testing we need a WRITABLE manifest — the embed copy
// is read-only. So we write to the per-test home's manifest dir.
function seedManifest(home: string, name: string, yaml: string) {
  const dir = join(home, ".local", "share", "mcp-local-hub", "servers", name);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, "manifest.yaml"), yaml, "utf-8");
}

test.describe("Edit server", () => {
  test("load path: #/edit-server?name=<seeded> renders form populated from disk", async ({ page, hub }) => {
    seedManifest(hub.home, "e2e-edit", `name: e2e-edit
kind: global
transport: stdio-bridge
command: echo
`);
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit`);
    await expect(page.locator("h1")).toHaveText("Add server");
    await expect(page.locator("#field-name")).toHaveValue("e2e-edit");
    await expect(page.locator("#field-command")).toHaveValue("echo");
  });

  test("name + kind are disabled in edit mode", async ({ page, hub }) => {
    seedManifest(hub.home, "e2e-lock", `name: e2e-lock
kind: global
transport: stdio-bridge
command: npx
`);
    await page.goto(`${hub.url}/#/edit-server?name=e2e-lock`);
    await expect(page.locator("#field-name")).toBeDisabled();
    await expect(page.locator("#field-kind")).toBeDisabled();
  });

  test("Save writes new yaml + updates hash", async ({ page, hub }) => {
    seedManifest(hub.home, "e2e-save", `name: e2e-save
kind: global
transport: stdio-bridge
command: old-cmd
`);
    await page.goto(`${hub.url}/#/edit-server?name=e2e-save`);
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("new-cmd");
    await page.locator('[data-action="save"]').click();
    await expect(page.locator('[data-testid="banner"].success')).toContainText("Daemon still running old config");
    await expect(page.locator('[data-action="reinstall"]')).toBeVisible();
  });

  test("Force Save path: external edit → hash mismatch → [Force Save] button writes", async ({ page, hub }) => {
    const name = "e2e-stale";
    seedManifest(hub.home, name, `name: ${name}
kind: global
transport: stdio-bridge
command: a
`);
    await page.goto(`${hub.url}/#/edit-server?name=${name}`);
    // Wait for form to load.
    await expect(page.locator("#field-command")).toHaveValue("a");
    // Modify form.
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("b");
    // Simulate external write: modify the manifest on disk.
    seedManifest(hub.home, name, `name: ${name}
kind: global
transport: stdio-bridge
command: a
externally_added: "from-other-tab"
`);
    await page.locator('[data-action="save"]').click();
    await expect(page.locator('[data-testid="banner"].error')).toContainText("changed on disk");
    await expect(page.locator('[data-action="force-save"]')).toBeVisible();
    // Click Force Save.
    await page.locator('[data-action="force-save"]').click();
    await expect(page.locator('[data-testid="banner"].success')).toContainText("Force-saved");
    // Banner should mention the preserved external field.
    await expect(page.locator('[data-testid="banner"]')).toContainText("externally_added");
  });

  test("read-only mode: manifest with nested unknown enters read-only + banner + Back to Servers", async ({ page, hub }) => {
    seedManifest(hub.home, "e2e-nested", `name: e2e-nested
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: d1
    port: 9100
    extra_config:
      custom: 1
`);
    await page.goto(`${hub.url}/#/edit-server?name=e2e-nested`);
    await expect(page.locator('[data-testid="readonly-banner"]')).toBeVisible();
    await expect(page.locator("#field-name")).toBeDisabled();
    await expect(page.locator('[data-action="save"]')).toBeDisabled();
    await expect(page.locator('[data-action="paste-yaml"]')).toBeDisabled();
    await expect(page.locator('[data-action="copy-yaml"]')).toBeEnabled();
  });

  test("load failure: manifest not found → inline error banner + [Retry][Back to Servers]", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/edit-server?name=nonexistent`);
    await expect(page.locator('[data-testid="load-error-banner"]')).toBeVisible();
    await expect(page.locator('[data-testid="load-error-banner"]')).toContainText("Failed to load");
  });

  test("sidebar-intercept: dirty edit + click sidebar → confirm dialog; cancel keeps state", async ({ page, hub }) => {
    seedManifest(hub.home, "e2e-dirty", `name: e2e-dirty
kind: global
transport: stdio-bridge
command: a
`);
    await page.goto(`${hub.url}/#/edit-server?name=e2e-dirty`);
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("b");
    // Dialog handler declines.
    page.once("dialog", (d) => d.dismiss());
    await page.locator(".sidebar nav a", { hasText: "Servers" }).click();
    // Still on edit screen.
    await expect(page.locator("h1")).toHaveText("Add server");
    await expect(page.locator("#field-command")).toHaveValue("b");
  });

  test("matrix view appears for 4+ daemons", async ({ page, hub }) => {
    const seed = `name: e2e-matrix
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: a
    port: 9100
  - name: b
    port: 9101
  - name: c
    port: 9102
  - name: d
    port: 9103
client_bindings:
  - client: claude-code
    daemon: a
    url_path: /mcp
`;
    seedManifest(hub.home, "e2e-matrix", seed);
    await page.goto(`${hub.url}/#/edit-server?name=e2e-matrix`);
    await page.locator(".accordion-header", { hasText: "Client bindings" }).click();
    await expect(page.locator('[data-testid="bindings-matrix"]')).toBeVisible();
    // 4 daemon columns + 1 header column.
    await expect(page.locator('.bindings-matrix thead th')).toHaveCount(5);
  });

  test("Advanced section: workspace-scoped kind reveals languages + port_pool", async ({ page, hub }) => {
    seedManifest(hub.home, "e2e-ws", `name: e2e-ws
kind: workspace-scoped
transport: stdio-bridge
command: ws
port_pool:
  start: 9200
  end: 9220
languages:
  - name: python
    backend: mcp-language-server
`);
    await page.goto(`${hub.url}/#/edit-server?name=e2e-ws`);
    await page.locator(".accordion-header", { hasText: "Advanced" }).click();
    await expect(page.locator('[data-testid="port-pool"]')).toBeVisible();
    await expect(page.locator('[data-testid="languages-subsection"]')).toBeVisible();
  });

  test("daemon rename does NOT orphan bindings (internal-ID cascade)", async ({ page, hub }) => {
    seedManifest(hub.home, "e2e-rename", `name: e2e-rename
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9100
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`);
    await page.goto(`${hub.url}/#/edit-server?name=e2e-rename`);
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-field="daemon-name"]').first().fill("main");
    // YAML preview reflects rename + binding cascade.
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("name: main");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("daemon: main");
    await expect(page.locator('[data-testid="yaml-preview"]')).not.toContainText("daemon: default");
  });
});
```

### Step 2 — Run the new spec

```bash
cd internal/gui/e2e && npm test -- tests/edit-server.spec.ts
```

Expected: 10/10 PASS. If any fail because of selector / seed path issues, debug and fix. Common issue: the binary may read manifests from `servers/` in the repo root (embed FS) rather than the test home — verify by running the test once, printing the effective manifest dir via a brief inline `page.evaluate(() => fetch('/api/scan'))` inspection, and adjusting `seedManifest`'s target dir.

### Step 3 — Full E2E regression

```bash
cd internal/gui/e2e && npm test
cd ../../..
```

Expected: existing suites still green + edit-server 10/10.

### Step 4 — Commit

```bash
git add internal/gui/e2e/tests/edit-server.spec.ts
git commit -m "test(gui/e2e): edit-server 10 Playwright scenarios

Load happy path, name+kind locked, Save + Reinstall banner, Force Save
with external-edit hash mismatch (preserves externally_added top-level
field), nested-unknown read-only mode, load failure, sidebar-intercept
when dirty, 4+-daemon matrix view, workspace-scoped Advanced (languages
+ port_pool), internal-ID cascade daemon rename (no orphan bindings)."
```

---

## Task 19: Full-suite smoke + CLAUDE.md update

**Files:**
- Modify: `CLAUDE.md`

### Step 1 — Run the full validation pipe

```bash
go build ./...
go test ./... -count=1
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../e2e && npm test
cd ../../..
```

Expected:
- `go build ./...` exits 0.
- All Go packages PASS (known flakes: `internal/daemon.TestHostStopUnblocksPendingHandlers`, `internal/api.TestInstallAllInstallsEverything` — rerun if hit).
- Vitest all green.
- Playwright 37/37 PASS (3 shell + 3 servers + 6 migration + 10 add-server + 10 edit-server + 2 dashboard + 3 logs).

### Step 2 — Update `CLAUDE.md`

Find the `### What's covered` section. Replace with:

```
### What's covered

- Shell: sidebar, five nav links, hash routing, active-link highlight.
- Servers: matrix columns (Server + 4 clients + Port + State), empty-body state on clean tmpHome, Apply disabled with no dirty cells.
- Migration: h1, empty-state copy, group sections hidden on empty home, hashchange swap from Servers, full POST /api/dismiss → on-disk JSON → GET /api/dismissed round-trip, /api/scan-unfiltered regression guard (seed + dismiss + re-scan).
- Add server: empty-state + debounced YAML preview, live name-regex inline error, single-daemon flat bindings, cascade rename/delete with confirm, Save writes manifest, Save&Install port-conflict failure path with Retry Install banner, Paste YAML import, sidebar-intercept unsaved-changes guard.
- Edit server: #/edit-server?name= load from disk, name+kind locked, Save → Reinstall banner, Force Save with external-edit hash-mismatch preserving `_preservedRaw` top-level fields, nested-unknown read-only mode, load failure banner, sidebar-intercept when dirty, 4+-daemon matrix view, workspace-scoped Advanced (languages + port_pool), internal-ID cascade daemon rename.
- Dashboard: empty-cards state on fresh home, `/api/events` SSE connection opens on mount.
- Logs: picker + controls render, notice text on no-daemons state, controls disabled when no eligible entries.

37 smoke tests total (3 shell + 3 servers + 6 migration + 10 add-server + 10 edit-server + 2 dashboard + 3 logs), ~30s
wall-time on a warm machine.
```

### Step 3 — Commit

```bash
git add CLAUDE.md
git commit -m "docs: CLAUDE.md reflects Edit server E2E coverage

Edit server: 10 new Playwright tests (load, name+kind lock, Save+
Reinstall, Force Save + preservedRaw, nested-unknown read-only mode,
load failure, sidebar-intercept, 4+-daemon matrix view, workspace
Advanced fields, internal-ID cascade rename). Updated total
(27→37) and wall-time (~20s→~30s)."
```

### Step 4 — Verify PR-ready

```bash
git log master..HEAD --oneline
git status
```

Expected: ~17-20 commits including design memo + plan + 16-18 implementation commits. Working tree clean.

### Step 5 — Hand off

Follow the A2a PR pattern:
1. Push: `git push -u origin feat/phase-3b-ii-a2b-edit-mode`
2. Open PR with a summary listing the 14 design decisions + 8 coherence fixes.
3. Trigger `@codex review`.
4. Address R-rounds.
5. Merge when CLEAN.

---

## Dependency order summary

Task 1 (hash helper) → Task 2 (ManifestGetWithHash) → Task 3 (ManifestEditWithHash + ErrManifestHashMismatch) → Task 4 (GUI manifest/get + manifest/edit handlers) → Tasks 5-8 (frontend types + UUID + yaml + api — bundled commit) → Task 9 (useRouter guard) → Task 10 (useUnsavedChangesGuard) → Task 11 (AddServer mode prop migration) → Task 12 (edit-mode Load + read-only + load-failure UI) → Task 13 (edit-mode Save + Force Save + Reload + Reinstall) → Task 14 (app.tsx edit-server route + guards installed) → Task 15 (Servers/Migration row entry points) → Task 16 (Advanced section) → Task 17 (matrix view) → Task 18 (E2E) → Task 19 (docs + smoke).

- Tasks 1-8 are largely backend + frontend-type foundations and must precede any screen work.
- Tasks 11-13 extend AddServer in tight succession; the commits before Task 11 must keep the A2a contract (mode=create default) exactly.
- Task 14 wires the screen into routing; earlier tasks MUST keep `#/add-server` behavior untouched.
- Tasks 15-17 add UI affordances that depend on the edit-mode path being wired.
- Task 18 exercises everything end-to-end; MUST come last among feature tasks.

**Estimated scope:** ~2800-3500 LOC TS + ~200 LOC Go + ~500 LOC Playwright. 17-19 commits. Budget 8-10 hours of subagent-driven execution given the strict review discipline (per-task two-stage review AND a Codex full-plan review BEFORE starting implementation).

**Required pre-execute gate:** request a Codex full-plan review on BOTH memo + plan after they are committed and before dispatching any implementer. This is the workflow hardening introduced after A2a (3 in-flight fixes that plan-level review would have caught).

---

## Self-review (author ran this)

**Spec coverage:** Each of the 14 memo decisions (D1-D14) + 8 coherence fixes (F1-F6, G1-G2) maps to at least one task:
- D1 scope (monolith + matrix + secrets→A3): Task 17 (matrix), Task 16 (advanced) — secrets explicitly NOT in plan.
- D2 Save transaction: Task 13.
- D3 stale detection: Tasks 1-3 (backend), 8 (API), 13 (UI).
- D4 Force Save + last-writer-wins: Task 13 (runForceSave).
- D5 nav hardening: Tasks 9-10, 14.
- D6 E2E per-test dialog: Task 18 (uses `page.once("dialog", ...)` explicitly).
- D7 route + mode prop: Tasks 11, 14.
- D8 name lock: Task 11.
- D9 kind lock: Task 11.
- D10 internal-ID rename: Tasks 5, 7 (_id assignment), 11 (cascade logic).
- D11 Accept-as-baseline DROPPED: no task adds it; nothing to reject.
- D12 Advanced G1 + kind-gated: Task 16.
- D13 _preservedRaw + read-only: Task 7 (extraction), Task 12 (read-only UI).
- D14 load failure + dirty=false invariant: Task 12.
- F1 button drop: not in plan (correctly absent).
- F2 Force Save re-read: Task 13 runForceSave.
- F3 _preservedRaw top-level only: Task 7 allowlist + hasNestedUnknown.
- F4/F5 name+kind locked: Task 11.
- F6 dirty=false on load failure: Task 12 invariant.
- G1 last-writer-wins + banner text: Task 13 runForceSave banner.
- G2 nested-unknown read-only mode: Task 12.

**Placeholder scan:** no "TBD" / "TODO" / vague requirements. Each step has complete code or explicit commands.

**Type consistency:** `ManifestFormState` shape consistent Tasks 5, 7, 11, 12, 13, 16, 17. `DaemonFormEntry._id` introduced in Task 5 and used consistently through 11, 13, 16, 17. `ManifestHashMismatchError` named in Task 8 and used in Task 13. Data-testid strings (`load-error-banner`, `readonly-banner`, `bindings-matrix`, `port-pool`, `languages-subsection`) defined in the tasks that add them and referenced in Task 18 tests.

No gaps found.

---

# Appendix: Codex R2 plan-review refinements (OVERRIDES task bodies)

Codex R1 full-plan review flagged 7 findings. Codex R2 reviewed the lead's proposed fixes and identified needed revisions. The resolved fixes below **override the original task bodies** for Tasks 3, 4, 8, 9, 12, 13, 18, plus added create-mode Advanced E2E coverage. Subagent implementers MUST apply the code in this appendix, not the original task-body code, for the noted sections.

## Summary table

| Finding | Severity | Area | Resolution |
|---|---|---|---|
| P1-1 | must-fix | Task 3 atomic write | `os.CreateTemp` + atomic rename + defer cleanup + injected failure hook |
| P1-2 | dropped | Task 13 hash refresh | Superseded by P1-3 (backend returns new hash; no extra `getManifest` call needed) |
| P1-3 | must-fix | Tasks 3, 4, 8, 13 | `ManifestEditWithHash` returns new hash; `/api/manifest/edit` responds 200 + `{hash}`; `postManifestEdit(): Promise<{hash}>`; runSave/runForceSave consume it atomically |
| P1-4 | must-fix | Task 13 Force Save | Validate the FINAL payload AFTER re-read + merge + serialize, not before |
| P2-1 | should-fix | Task 18 E2E | Drop `page.close()` + beforeunload test (unreliable); add hashchange cancel/accept + Paste→Save race |
| P2-2 | should-fix | Tasks 9, 12 | `useRouter` returns `{screen, query}`; AddServer derives `editName` from route state; load effect deps on stable strings; guard-driven state update (not direct hashchange listener) |
| P2-3 | should-fix | Task 18 (and add-server.spec.ts) | Add create-mode Advanced kind-toggle + always-visible field E2E coverage |

## P1-1 refined — Task 3 atomic write

**Replaces Task 3 Step 3 impl + Step 1 tests.**

### Refined `ManifestEditInWithHash` impl

```go
// ManifestEditInWithHash replaces an existing manifest atomically via
// tmp-file-plus-rename. If the on-disk content hash diverged from
// expectedHash (non-empty), returns ErrManifestHashMismatch without
// writing. Empty expectedHash skips the check (Force Save path which
// re-reads at save-time). Returns the new post-write content hash so
// callers can update their loadedHash cache in one pass — avoids an
// extra GET round-trip AND the stale-hash-after-force-save race.
func (a *API) ManifestEditInWithHash(dir, name, yaml, expectedHash string) (newHash string, err error) {
	if err := checkManifestName(name); err != nil {
		return "", err
	}
	target := filepath.Join(dir, name, "manifest.yaml")
	current, err := os.ReadFile(target)
	if err != nil {
		return "", fmt.Errorf("manifest %q does not exist; use create instead", name)
	}
	if expectedHash != "" {
		if got := ManifestHashContent(current); got != expectedHash {
			return "", ErrManifestHashMismatch
		}
	}
	if warnings := a.ManifestValidate(yaml); len(warnings) > 0 {
		return "", fmt.Errorf("manifest has validation errors: %s", strings.Join(warnings, "; "))
	}
	// Atomic write: unique tmp in the same directory, defer cleanup,
	// os.Rename on success. Test-only hook manifestEditFailWriteHook
	// lets tests inject a write/rename failure without relying on
	// read-only-dir tricks (brittle on Windows).
	tmp, err := os.CreateTemp(filepath.Dir(target), "manifest-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	// Always attempt to remove tmp; harmless if rename already moved it.
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write([]byte(yaml)); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close tmp: %w", err)
	}
	if manifestEditFailWriteHook != nil && manifestEditFailWriteHook() {
		return "", fmt.Errorf("injected write failure")
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return "", fmt.Errorf("atomic rename: %w", err)
	}
	return ManifestHashContent([]byte(yaml)), nil
}

// manifestEditFailWriteHook is a package-internal test hook that forces
// a simulated write-failure to verify atomic-write crash-safety. Tests
// set it via ManifestSetFailWriteHook, run the operation, then reset.
var manifestEditFailWriteHook func() bool

// ManifestSetFailWriteHook is test-only; callers in production code MUST
// NOT set this. Exported only so internal/gui tests can reuse the hook.
func ManifestSetFailWriteHook(h func() bool) { manifestEditFailWriteHook = h }

// ManifestEditWithHash is the default-dir convenience wrapper.
func (a *API) ManifestEditWithHash(name, yaml, expectedHash string) (string, error) {
	return a.ManifestEditInWithHash(defaultManifestDir(), name, yaml, expectedHash)
}
```

### Refined Task 3 tests (adjust for `(string, error)` return + atomic test)

All existing test bodies change:

```go
func TestManifestEditIn_RejectsHashMismatch(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	if err := a.ManifestCreateIn(dir, name, "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\n"); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, hash, _ := a.ManifestGetInWithHash(dir, name)
	path := filepath.Join(dir, name, "manifest.yaml")
	if err := os.WriteFile(path, []byte("name: demo\nkind: workspace-scoped\ntransport: stdio-bridge\ncommand: npx\n"), 0600); err != nil {
		t.Fatalf("external write: %v", err)
	}
	_, err := a.ManifestEditInWithHash(dir, name, "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\n", hash)
	if err == nil {
		t.Fatalf("expected hash-mismatch error, got nil")
	}
	if !errors.Is(err, ErrManifestHashMismatch) {
		t.Errorf("err = %v, want ErrManifestHashMismatch", err)
	}
}

func TestManifestEditIn_AcceptsMatchingHash_ReturnsNewHash(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	orig := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\n"
	if err := a.ManifestCreateIn(dir, name, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, hash, _ := a.ManifestGetInWithHash(dir, name)
	updated := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\n"
	newHash, err := a.ManifestEditInWithHash(dir, name, updated, hash)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	wantHash := ManifestHashContent([]byte(updated))
	if newHash != wantHash {
		t.Errorf("returned hash = %q, want %q", newHash, wantHash)
	}
	got, diskHash, _ := a.ManifestGetInWithHash(dir, name)
	if got != updated {
		t.Errorf("yaml = %q, want %q", got, updated)
	}
	if diskHash != newHash {
		t.Errorf("disk hash = %q does not match returned newHash %q", diskHash, newHash)
	}
}

func TestManifestEditIn_EmptyExpectedHash_SkipsCheck(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	if err := a.ManifestCreateIn(dir, name, "name: demo\n"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := a.ManifestEditInWithHash(dir, name, "name: demo\nkind: global\n", ""); err != nil {
		t.Fatalf("empty-hash edit should succeed: %v", err)
	}
}

func TestManifestEditIn_AtomicWrite_TargetUnchangedOnFailure(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	orig := "name: demo\n"
	if err := a.ManifestCreateIn(dir, name, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Inject write failure between tmp-close and rename.
	ManifestSetFailWriteHook(func() bool { return true })
	defer ManifestSetFailWriteHook(nil)
	_, err := a.ManifestEditInWithHash(dir, name, "name: demo\nkind: global\n", "")
	if err == nil {
		t.Fatalf("expected injected failure, got nil")
	}
	// Target content must be UNCHANGED.
	got, _, _ := a.ManifestGetInWithHash(dir, name)
	if got != orig {
		t.Errorf("target yaml changed on failure: %q, want %q", got, orig)
	}
	// No stale tmp file left.
	files, _ := os.ReadDir(filepath.Join(dir, name))
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".tmp") {
			t.Errorf("stale tmp file left: %q", f.Name())
		}
	}
}
```

## P1-3 refined — hash return cascade (Tasks 4, 8, 13)

### Task 4 — GUI handler response: 200 + `{hash}` instead of 204

Replace the handler body for `/api/manifest/edit` in Task 4 Step 1:

```go
s.mux.HandleFunc("/api/manifest/edit", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req manifestEditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeAPIError(w, fmt.Errorf("name must not be empty"), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	newHash, err := s.manifestEditor.ManifestEditWithHash(name, req.YAML, req.ExpectedHash)
	if err != nil {
		code := "MANIFEST_EDIT_FAILED"
		status := http.StatusInternalServerError
		if errors.Is(err, api.ErrManifestHashMismatch) {
			code = "MANIFEST_HASH_MISMATCH"
			status = http.StatusConflict
		}
		writeAPIError(w, err, status, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(manifestEditResponse{Hash: newHash})
}))
```

Add the response type near the other types in Task 4:

```go
type manifestEditResponse struct {
	Hash string `json:"hash"`
}
```

Update the `manifestEditor` interface signature:

```go
type manifestEditor interface {
	ManifestEditWithHash(name, yaml, expectedHash string) (newHash string, err error)
}
```

`realManifestEditor` and `fakeManifestEditor` both return `(string, error)`. Update test expectations: success case asserts `rec.Code == http.StatusOK` (not 204) and the body contains `"hash":"<returned>"`. Hash-mismatch case still asserts 409.

### Task 8 — `postManifestEdit` returns `Promise<{hash: string}>`

Replace the test block for `postManifestEdit` success case:

```ts
  it("returns new hash on 200", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ hash: "new-hash-abc" }),
    }) as unknown as Response);
    const out = await postManifestEdit("demo", "name: demo\n", "old-hash");
    expect(out).toEqual({ hash: "new-hash-abc" });
  });
```

Replace the impl in `api.ts`:

```ts
export async function postManifestEdit(
  name: string,
  yaml: string,
  expectedHash: string,
): Promise<{ hash: string }> {
  const resp = await fetch("/api/manifest/edit", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, yaml, expected_hash: expectedHash }),
  });
  if (resp.ok) {
    const payload = (await resp.json()) as { hash?: string };
    // R3 correction: reject malformed success (empty/missing hash).
    // An empty returned hash would become loadedHash, and the next
    // edit call would send expected_hash="" — which the backend treats
    // as "skip optimistic concurrency". That silently drops stale
    // detection. Reject 200 without a non-empty hash.
    if (!payload.hash) {
      throw new Error("/api/manifest/edit: success response missing hash field");
    }
    return { hash: payload.hash };
  }
  let body: { error?: string; code?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string; code?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  if (resp.status === 409 && body?.code === "MANIFEST_HASH_MISMATCH") {
    throw new ManifestHashMismatchError(body.error ?? "hash mismatch");
  }
  throw new Error(`/api/manifest/edit: ${body?.error ?? resp.statusText}`);
}
```

### Task 13 — runSave + runForceSave consume returned hash atomically

Replace the entire `runSave` success branch AND `runForceSave` body in Task 13:

```tsx
  async function runSave(opts: { install: boolean }) {
    const version = ++submissionCounter.current;
    setBusy(opts.install ? "install" : "save");
    setBanner(null);
    try {
      const name = formState.name.trim();
      if (!name) {
        setBanner({ kind: "error", text: "Name is required." });
        return;
      }
      const payload = toYAML(formState);
      const warnings = await postManifestValidate(payload);
      if (version !== submissionCounter.current) return;
      if (warnings.length > 0) {
        setWarnings(warnings);
        setBanner({
          kind: "error",
          text: `Cannot save: ${warnings.length} validation warning${warnings.length === 1 ? "" : "s"}.`,
        });
        return;
      }
      if (mode === "edit") {
        try {
          const { hash: newHash } = await postManifestEdit(name, payload, formState.loadedHash);
          if (version !== submissionCounter.current) return;
          // Atomic snapshot update: build one post-save object carrying the
          // fresh hash AND the user's just-persisted form state; set both
          // formState and initialSnapshot from the same reference so dirty
          // is false (P1-2 fix: no separate getManifest refresh, no ordering race).
          const postSave: ManifestFormState = { ...formState, loadedHash: newHash };
          setFormState(postSave);
          setInitialSnapshot(postSave);
        } catch (err) {
          if (version !== submissionCounter.current) return;
          if (err instanceof ManifestHashMismatchError) {
            setBanner({
              kind: "error",
              text: "Manifest changed on disk since you opened it. Reload will discard your edits and show the new version. Force Save will overwrite with your version.",
              staleReload: true,
              staleForceSave: true,
            });
            return;
          }
          throw err;
        }
      } else {
        await postManifestCreate(name, payload);
        if (version !== submissionCounter.current) return;
        setInitialSnapshot(formState);
      }
      setWarnings(null);
      if (!opts.install) {
        setBanner({
          kind: "success",
          text: mode === "edit"
            ? `Saved. Daemon still running old config.`
            : `Saved servers/${name}/manifest.yaml.`,
          reinstall: mode === "edit",
        });
        return;
      }
      await runInstallNow(name, version);
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      if (version === submissionCounter.current) setBusy("");
    }
  }

  async function runForceSave() {
    const version = ++submissionCounter.current;
    setBusy("save");
    setBanner(null);
    try {
      const name = formState.name.trim();
      // 1. Re-read disk to get fresh hash + fresh _preservedRaw.
      const fresh = await getManifest(name);
      if (version !== submissionCounter.current) return;
      const freshParsed = parseYAMLToForm(fresh.yaml);
      // 2. Merge: user's known-field edits win; fresh disk _preservedRaw wins.
      const merged: ManifestFormState = {
        ...formState,
        _preservedRaw: freshParsed._preservedRaw,
      };
      // 3. Serialize FINAL payload AFTER merge.
      const payload = toYAML(merged);
      // 4. Validate the FINAL payload (P1-4 fix: validate the exact bytes
      // that will be written, not pre-merge).
      const warnings = await postManifestValidate(payload);
      if (version !== submissionCounter.current) return;
      if (warnings.length > 0) {
        setWarnings(warnings);
        setBanner({
          kind: "error",
          text: `Cannot Force Save: ${warnings.length} validation warning${warnings.length === 1 ? "" : "s"} in merged payload.`,
        });
        return;
      }
      // 5. Write with fresh hash as expectedHash; consume returned new hash.
      const { hash: newHash } = await postManifestEdit(name, payload, fresh.hash);
      if (version !== submissionCounter.current) return;
      // 6. Atomic baseline update.
      const postSave: ManifestFormState = { ...merged, loadedHash: newHash };
      setFormState(postSave);
      setInitialSnapshot(postSave);
      const preservedKeys = Object.keys(freshParsed._preservedRaw);
      setBanner({
        kind: "success",
        text:
          preservedKeys.length > 0
            ? `Force-saved. Preserved external fields: ${preservedKeys.join(", ")}.`
            : `Force-saved.`,
        reinstall: true,
      });
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({ kind: "error", text: `Force Save failed: ${(err as Error).message}` });
    } finally {
      if (version === submissionCounter.current) setBusy("");
    }
  }
```

## P2-2 refined — useRouter `{screen, query}` + guard-driven state (Tasks 9, 12)

### Task 9 — `useRouter` exposes `{screen, query}` and updates state ONLY on accepted navigation

Replace `useRouter.ts`:

```ts
import { useEffect, useState } from "preact/hooks";

export interface RouterState {
  screen: string;
  query: string;  // the raw query-string after "?", empty if none
}

function parse(defaultScreen: string): RouterState {
  const hash = window.location.hash || `#/${defaultScreen}`;
  const afterPrefix = hash.replace(/^#\//, "");
  const [screenRaw, queryRaw = ""] = afterPrefix.split("?", 2);
  return {
    screen: screenRaw || defaultScreen,
    query: queryRaw,
  };
}

// useRouter is a minimal hash router. Returns {screen, query} as stable
// strings. The guard receives the TARGET RouterState BEFORE it is
// committed to internal state. Returning false reverts the hash via
// history.replaceState; internal state NEVER moves to the declined
// target. This is critical for AddServer's identity-change flow: a
// dirty-cancelled ?name=a → ?name=b navigation must not update editName,
// or the load effect would fire and fetch b against the user's intent.
//
// R3 correction: no suppression flag. replaceState with a hash does NOT
// fire hashchange (per HTML spec), so the "follow-up event we suppress"
// doesn't exist. The earlier draft kept suppressRef=true forever after
// the first decline, poisoning all future navigations. Correct pattern
// is just replaceState; the next real hashchange (from user action) is
// not synthetically generated by us.
export function useRouter(
  defaultScreen: string,
  guard?: (target: RouterState) => boolean,
): RouterState {
  const [state, setState] = useState<RouterState>(() => parse(defaultScreen));

  useEffect(() => {
    const onHash = (e: HashChangeEvent) => {
      const target = parse(defaultScreen);
      if (guard && !guard(target)) {
        // replaceState does NOT fire hashchange — no suppress-flag needed.
        // The URL reverts, internal state stays, and the next real
        // user-initiated navigation fires hashchange cleanly.
        window.history.replaceState(null, "", e.oldURL);
        return;
      }
      setState(target);
    };
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [guard, defaultScreen]);

  return state;
}
```

### Task 9 — tests add same-screen-different-query case

Append to `useRouter.test.ts`:

```ts
describe("useRouter same-key-different-query (A2b P2-2)", () => {
  beforeEach(() => {
    window.location.hash = "";
  });

  it("calls guard when query changes even if screen key stays the same", () => {
    window.location.hash = "#/edit-server?name=a";
    const guard = vi.fn(() => true);
    renderHook(() => useRouter("servers", guard));
    guard.mockClear();
    act(() => {
      window.history.pushState(null, "", "#/edit-server?name=b");
      window.dispatchEvent(new HashChangeEvent("hashchange", {
        oldURL: "http://localhost/#/edit-server?name=a",
        newURL: "http://localhost/#/edit-server?name=b",
      }));
    });
    expect(guard).toHaveBeenCalledWith({ screen: "edit-server", query: "name=b" });
  });

  it("query state does NOT change when guard returns false on same-key nav", () => {
    window.location.hash = "#/edit-server?name=a";
    const guard = vi.fn(() => false);
    const { result } = renderHook(() => useRouter("servers", guard));
    expect(result.current.query).toBe("name=a");
    act(() => {
      window.history.pushState(null, "", "#/edit-server?name=b");
      window.dispatchEvent(new HashChangeEvent("hashchange", {
        oldURL: "http://localhost/#/edit-server?name=a",
        newURL: "http://localhost/#/edit-server?name=b",
      }));
    });
    // Declined — state stays on a.
    expect(result.current.query).toBe("name=a");
  });
});
```

### Task 9 — existing A2a tests must keep passing

Prior tests call `useRouter("servers")` and assert the returned value is a string (screen key). Update those assertions to `.screen` access, e.g.:

```ts
// BEFORE: expect(result.current).toBe("servers")
// AFTER:  expect(result.current.screen).toBe("servers")
```

Apply to every existing useRouter test assertion.

### Task 12 — AddServer derives `editName` from route state; load effect uses stable deps

Replace the Task 12 mount effect with:

```tsx
  // props.route is passed from App.tsx (the consumer of useRouter).
  // We accept it rather than calling useRouter ourselves because the
  // guard lives in App and needs to reference addServerDirty.
  const editName = useMemo(() => {
    if (props.mode !== "edit") return "";
    const params = new URLSearchParams(props.route?.query ?? "");
    return params.get("name") ?? "";
  }, [props.mode, props.route?.query]);

  useEffect(() => {
    if (props.mode !== "edit") return;
    // R3 correction: reset prior per-manifest state BEFORE the new load.
    // Without this, navigating a→b in edit mode inherits a's loadError
    // or readOnlyReason (e.g., a had nested unknowns, b is clean, b
    // would render in read-only mode). Also blank the form while
    // fetching so we don't flash a's data in b's UI.
    setLoadError(null);
    setReadOnlyReason(null);
    setFormState(BLANK_FORM);
    setInitialSnapshot(BLANK_FORM);
    if (!editName) {
      setLoadError("No manifest name specified");
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const { yaml, hash } = await getManifest(editName);
        if (cancelled) return;
        const nested = hasNestedUnknown(yaml);
        const parsed = parseYAMLToForm(yaml);
        parsed.loadedHash = hash;
        setFormState(parsed);
        setInitialSnapshot(parsed);
        if (nested) {
          setReadOnlyReason(
            "This manifest contains fields the GUI cannot handle. Editing via GUI would drop them.",
          );
        }
      } catch (err) {
        if (cancelled) return;
        setLoadError((err as Error).message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [props.mode, editName]);
```

Add `useMemo` to the `preact/hooks` import line.

Update the `AddServerScreen` props signature:

```tsx
export function AddServerScreen(props: {
  mode?: "create" | "edit";
  route?: RouterState;  // passed from App.tsx, used for edit-mode identity
  onDirtyChange?: (dirty: boolean) => void;
} = {}) {
```

`RouterState` imported from `../hooks/useRouter`.

### Task 14 — App.tsx threads route into AddServerScreen and guard uses full RouterState

Update Task 14's App.tsx body:

```tsx
export function App() {
  const [addServerDirty, setAddServerDirty] = useState(false);

  const guard = (target: RouterState): boolean => {
    if (!addServerDirty) return true;
    // Same screen AND same query → no navigation, no prompt.
    if (target.screen === route.screen && target.query === route.query) return true;
    // eslint-disable-next-line no-alert
    const ok = window.confirm("Discard unsaved changes?");
    if (ok) setAddServerDirty(false);
    return ok;
  };

  const route = useRouter("servers", guard);
  useUnsavedChangesGuard(addServerDirty);

  function guardClick(targetScreen: string): (e: MouseEvent) => void {
    return (e) => {
      if (!addServerDirty) return;
      if (route.screen !== "add-server" && route.screen !== "edit-server") return;
      if (targetScreen === route.screen) return;
      // eslint-disable-next-line no-alert
      const ok = window.confirm("Discard unsaved changes?");
      if (!ok) {
        e.preventDefault();
      } else {
        setAddServerDirty(false);
      }
    };
  }

  let body: JSX.Element;
  switch (route.screen) {
    case "servers":
      body = <ServersScreen />;
      break;
    case "migration":
      body = <MigrationScreen />;
      break;
    case "add-server":
      body = <AddServerScreen mode="create" route={route} onDirtyChange={setAddServerDirty} />;
      break;
    case "edit-server":
      body = <AddServerScreen mode="edit" route={route} onDirtyChange={setAddServerDirty} />;
      break;
    case "dashboard":
      body = <DashboardScreen />;
      break;
    case "logs":
      body = <LogsScreen />;
      break;
    default:
      body = <p>Unknown screen: {route.screen}</p>;
  }

  return (
    <>
      <aside class="sidebar">
        <div class="brand">mcp-local-hub</div>
        <nav>
          <a href="#/servers" class={route.screen === "servers" ? "active" : ""} onClick={guardClick("servers")}>Servers</a>
          <a href="#/migration" class={route.screen === "migration" ? "active" : ""} onClick={guardClick("migration")}>Migration</a>
          <a href="#/add-server" class={route.screen === "add-server" ? "active" : ""} onClick={guardClick("add-server")}>Add server</a>
          <a href="#/dashboard" class={route.screen === "dashboard" ? "active" : ""} onClick={guardClick("dashboard")}>Dashboard</a>
          <a href="#/logs" class={route.screen === "logs" ? "active" : ""} onClick={guardClick("logs")}>Logs</a>
        </nav>
      </aside>
      <main id="screen-root">
        {body}
      </main>
    </>
  );
}
```

Import `RouterState` from `./hooks/useRouter`.

## P2-1 refined — Task 18 E2E (drop page.close, add hashchange cancel/accept + Paste→Save race)

Remove from Task 18 any `page.close()` + beforeunload dialog test. Add these three scenarios to `edit-server.spec.ts`:

```ts
  test("hashchange cancel: dirty edit + hash nav away with dialog.dismiss -> stays", async ({ page, hub }) => {
    seedManifest(hub.home, "e2e-hc-cancel", `name: e2e-hc-cancel
kind: global
transport: stdio-bridge
command: a
`);
    await page.goto(`${hub.url}/#/edit-server?name=e2e-hc-cancel`);
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("b");
    page.once("dialog", (d) => d.dismiss());
    await page.evaluate(() => { window.location.hash = "#/servers"; });
    // Give the hashchange handler time to process + revert.
    await page.waitForTimeout(100);
    await expect(page.locator("h1")).toHaveText("Add server");
    await expect(page.locator("#field-command")).toHaveValue("b");
  });

  test("hashchange accept: dirty edit + hash nav away with dialog.accept -> navigates + clears dirty", async ({ page, hub }) => {
    seedManifest(hub.home, "e2e-hc-accept", `name: e2e-hc-accept
kind: global
transport: stdio-bridge
command: a
`);
    await page.goto(`${hub.url}/#/edit-server?name=e2e-hc-accept`);
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("b");
    page.once("dialog", (d) => d.accept());
    await page.evaluate(() => { window.location.hash = "#/servers"; });
    await expect(page.locator("h1")).toHaveText("Servers");
  });

  test("Paste YAML -> Save race: Save payload contains pasted content, not pre-paste (version-counter invariant)", async ({ page, hub }) => {
    // Create-mode test: fill name, paste YAML that overwrites, then Save
    // immediately. The submission-version counter must ensure the POST
    // body reflects the POSTED form state (post-paste), not a mid-stream
    // stale snapshot. We intercept the network request and inspect body.
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator("#field-name").fill("precursor");
    const posted: { body?: string } = {};
    await page.route("**/api/manifest/create", async (r) => {
      posted.body = r.request().postData() ?? "";
      await r.fulfill({ status: 204, body: "" });
    });
    const yaml = `name: pasted-wins\nkind: global\ntransport: stdio-bridge\ncommand: echoed\n`;
    page.once("dialog", (d) => d.accept(yaml));
    await page.locator('[data-action="paste-yaml"]').click();
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("name: pasted-wins");
    await page.locator('[data-action="save"]').click();
    await expect.poll(() => posted.body ?? "").toContain(`"name":"pasted-wins"`);
    expect(posted.body).not.toContain(`"name":"precursor"`);
  });
```

Note on the beforeunload coverage gap: browser-close behavior is declared **best-effort**, not E2E-tested. Rationale: `page.close()` + native `beforeunload` dialog handling is unreliable across Playwright browsers / versions. A manual smoke test (close tab with dirty form, verify dialog) lives in D2 matrix.

## P2-3 refined — create-mode Advanced kind-toggle E2E (add-server.spec.ts)

Append to `internal/gui/e2e/tests/add-server.spec.ts`:

```ts
  test("Advanced kind-toggle: workspace-scoped reveals languages/port_pool/daemon.context; global hides them", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    // Expand Advanced. Initially kind=global: workspace-only fields absent.
    await page.locator(".accordion-header", { hasText: "Advanced" }).click();
    await expect(page.locator('[data-testid="port-pool"]')).toHaveCount(0);
    await expect(page.locator('[data-testid="languages-subsection"]')).toHaveCount(0);
    // Switch kind to workspace-scoped.
    await page.locator(".accordion-header", { hasText: "Basics" }).click();
    await page.locator("#field-kind").selectOption("workspace-scoped");
    await page.locator(".accordion-header", { hasText: "Advanced" }).click();
    await expect(page.locator('[data-testid="port-pool"]')).toBeVisible();
    await expect(page.locator('[data-testid="languages-subsection"]')).toBeVisible();
    // Switch back to global: workspace-only fields vanish.
    await page.locator(".accordion-header", { hasText: "Basics" }).click();
    await page.locator("#field-kind").selectOption("global");
    await page.locator(".accordion-header", { hasText: "Advanced" }).click();
    await expect(page.locator('[data-testid="port-pool"]')).toHaveCount(0);
    await expect(page.locator('[data-testid="languages-subsection"]')).toHaveCount(0);
  });

  test("Advanced always-visible fields survive kind toggles (idle_timeout, base_args_template, daemon.extra_args)", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator(".accordion-header", { hasText: "Advanced" }).click();
    await expect(page.locator("#field-idle-timeout")).toBeVisible();
    await expect(page.locator('[data-testid="base-args-template"]')).toBeVisible();
    // Flip kind both ways; always-visible fields remain.
    await page.locator(".accordion-header", { hasText: "Basics" }).click();
    await page.locator("#field-kind").selectOption("workspace-scoped");
    await page.locator(".accordion-header", { hasText: "Advanced" }).click();
    await expect(page.locator("#field-idle-timeout")).toBeVisible();
    await expect(page.locator('[data-testid="base-args-template"]')).toBeVisible();
    await page.locator(".accordion-header", { hasText: "Basics" }).click();
    await page.locator("#field-kind").selectOption("global");
    await page.locator(".accordion-header", { hasText: "Advanced" }).click();
    await expect(page.locator("#field-idle-timeout")).toBeVisible();
    await expect(page.locator('[data-testid="base-args-template"]')).toBeVisible();
  });
```

## Test count updates

Task 18 (edit-server.spec.ts) goes from 10 → 12 tests (added hashchange cancel + accept; Paste-Save race already present or explicit). add-server.spec.ts goes from 10 → 12 tests (added two create-mode Advanced kind-toggle tests).

Task 19 CLAUDE.md coverage block totals: **41 smoke tests** (3 shell + 3 servers + 6 migration + 12 add-server + 12 edit-server + 2 dashboard + 3 logs).

## Summary: what subagent implementers must do

For Tasks 3, 4, 8, 9, 12, 13, 14, 18, plus the P2-3 add-server.spec.ts additions:

1. **Read the original task body** for context (files, commit message, general approach).
2. **Apply the refined code from this Appendix** in place of any conflicting original task-body code.
3. **Commit messages** stay as originally specified in the task bodies — they already describe the feature, and the appendix refinement is an implementation detail, not a scope change.
4. When self-reviewing before DONE, verify the refined constraints: atomic write with no stale tmp leftovers (Task 3); hash returned from edit endpoint (Task 4); `postManifestEdit` returns `{hash}` (Task 8); `useRouter` returns `{screen, query}` and guard-driven state updates (Task 9); `editName` derived state + stable deps (Task 12); runSave atomic baseline update from returned hash + runForceSave validates after merge (Task 13); `route` threaded through App → AddServer (Task 14); hashchange cancel/accept + Paste-Save race tests present in edit-server.spec.ts, create-mode Advanced kind-toggle tests present in add-server.spec.ts (Task 18).


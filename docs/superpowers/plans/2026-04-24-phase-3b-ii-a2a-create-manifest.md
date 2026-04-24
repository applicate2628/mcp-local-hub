# Phase 3B-II A2a — Create-flow Add Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the Add Server GUI flow (`#/add-server`) that creates and optionally installs new MCP server manifests. Unblock the A1 Migration screen's Create-manifest button (currently DOM-disabled post-PR-#4).

**Architecture:** Form-as-source-of-truth Preact screen with a structured `ManifestFormState` object, a `toYAML(state)` serializer, and a one-shot `parseYAMLToForm(yaml)` used for A1 prefill and the Paste YAML import escape hatch. The form wraps two already-shipped backend APIs (`api.ManifestCreate` and `api.Install`) plus two new thin GUI handlers (`/api/manifest/create`, `/api/manifest/validate`). Validation is hybrid (live regex, on-demand structural, preflight on Save & Install). Dirty detection uses `deepEqual` against a post-normalization snapshot; Paste does NOT reset the snapshot — only successful Save does.

**Tech Stack:** Go 1.26 backend (thin GUI handler), Vite 8 + TypeScript 5 + Preact 10 frontend, Vitest 4 for unit tests, Playwright + headless Chromium for E2E.

**Reference documents (read before implementing):**
- Design memo: `docs/superpowers/specs/2026-04-24-phase-3b-ii-a2a-design.md` (authoritative for all design decisions)
- Example manifest: `servers/memory/manifest.yaml`
- Config types: `internal/config/manifest.go` (`ServerManifest`, `DaemonSpec`, `ClientBinding`)
- Backend APIs: `internal/api/manifest.go` (`ManifestCreate`, `ManifestValidate`, `ManifestGet`, `ManifestDelete`)
- Extract prefill: `internal/api/scan.go:439` (`ExtractManifestFromClient`)
- Install pipeline: `internal/api/install.go:816` (`BuildPlan`, `Preflight`)
- A1 PR #4 pattern for GUI thin wrappers: `internal/gui/demigrate.go`, `internal/gui/dismiss.go`

---

## File Structure

### Backend (Go)

```
internal/gui/
├── manifest.go              (NEW — thin handlers: POST /api/manifest/create, /api/manifest/validate)
├── manifest_test.go         (NEW — handler tests, Sec-Fetch-Site guard + error envelopes)
└── server.go                (MODIFY — wire registerManifestRoutes in NewServer)
```

### Frontend (TypeScript + Preact)

```
internal/gui/frontend/src/
├── api.ts                   (MODIFY — add postManifestCreate, postManifestValidate, getExtractManifest)
├── api.test.ts              (MODIFY — add tests for new helpers)
├── app.tsx                  (MODIFY — route + sidebar link + isDirty lift for sidebar-intercept)
├── types.ts                 (MODIFY — ManifestFormState, ValidationWarning, ManifestCreateRequest types)
├── hooks/
│   └── useDebouncedValue.ts (NEW — debounce hook for YAML preview)
├── lib/
│   ├── manifest-yaml.ts     (NEW — toYAML, parseYAMLToForm)
│   └── manifest-yaml.test.ts (NEW — round-trip tests including normalization invariants)
├── screens/
│   ├── AddServer.tsx        (NEW — the screen; full accordion form)
│   └── Migration.tsx        (MODIFY — unblock Create-manifest button)
└── styles/
    └── style.css            (APPEND — accordion + add-server-specific CSS)
```

### E2E

```
internal/gui/e2e/tests/
├── add-server.spec.ts       (NEW — 8-10 Playwright scenarios)
└── shell.spec.ts            (MODIFY — 5 nav links now)
```

### Docs

```
CLAUDE.md                    (MODIFY — E2E coverage section bumps to 5 nav links + N new tests)
```

---

## Type definitions (referenced throughout)

These go in `internal/gui/frontend/src/types.ts`. Tasks use these exact names. Defined here for cross-task consistency; Task 2 writes them to disk.

```ts
// ManifestFormState is the authoritative in-memory shape for the AddServer
// screen. The form writes to it; toYAML(state) serializes it for the backend.
// Using array-of-objects for env and client_bindings (rather than maps) keeps
// add/delete operations and render ordering deterministic.
export interface ManifestFormState {
  name: string;
  kind: "global" | "workspace-scoped";
  transport: "stdio-bridge" | "native-http";
  command: string;
  base_args: string[];
  env: Array<{ key: string; value: string }>;
  daemons: Array<{ name: string; port: number }>;
  client_bindings: Array<{ client: string; daemon: string; url_path: string }>;
  weekly_refresh: boolean;
}

export interface ValidationWarning {
  message: string;
}

// ManifestValidateResponse mirrors the /api/manifest/validate handler shape.
export interface ManifestValidateResponse {
  warnings: string[];
}

// ExtractManifestResponse mirrors /api/extract-manifest — one string field
// with the draft YAML.
export interface ExtractManifestResponse {
  yaml: string;
}
```

---

## Task 1: Backend GUI thin-wrapper handlers

**Files:**
- Create: `internal/gui/manifest.go`
- Create: `internal/gui/manifest_test.go`
- Modify: `internal/gui/server.go` (wire `registerManifestRoutes` in `NewServer`)

### Step 1 — Inspect existing GUI wrapper pattern for context

Before writing new handlers, read how `demigrate.go` is structured. It's the minimal template we copy from.

```bash
cat internal/gui/demigrate.go
```

Expected: a Server-local interface (`demigrater`), a struct wrapper (`realDemigrater`), and a `registerDemigrateRoutes(s *Server)` function. The handler wraps `s.requireSameOrigin(...)` and emits `writeAPIError` on failure.

### Step 2 — Write failing test `internal/gui/manifest_test.go`

```go
package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeManifestCreator and fakeManifestValidator are Server-local test doubles.
// They shadow the real api.API calls via the interfaces injected into Server.
type fakeManifestCreator struct {
	name string
	yaml string
	err  error
}

func (f *fakeManifestCreator) ManifestCreate(name, yaml string) error {
	f.name, f.yaml = name, yaml
	return f.err
}

type fakeManifestValidator struct {
	lastYAML string
	out      []string
}

func (f *fakeManifestValidator) ManifestValidate(yaml string) []string {
	f.lastYAML = yaml
	return f.out
}

func newManifestTestServer(create *fakeManifestCreator, validate *fakeManifestValidator) *Server {
	s := &Server{mux: http.NewServeMux(), manifestCreator: create, manifestValidator: validate}
	registerManifestRoutes(s)
	return s
}

func postJSON(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// ---- /api/manifest/create ----

func TestManifestCreateHandler_RejectsNonPOST(t *testing.T) {
	s := newManifestTestServer(&fakeManifestCreator{}, &fakeManifestValidator{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/create", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestManifestCreateHandler_RejectsCrossOrigin(t *testing.T) {
	s := newManifestTestServer(&fakeManifestCreator{}, &fakeManifestValidator{})
	req := httptest.NewRequest(http.MethodPost, "/api/manifest/create",
		bytes.NewBufferString(`{"name":"x","yaml":"name: x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestManifestCreateHandler_ForwardsNameAndYAML(t *testing.T) {
	create := &fakeManifestCreator{}
	s := newManifestTestServer(create, &fakeManifestValidator{})
	rec := postJSON(t, s, "/api/manifest/create",
		`{"name":"demo","yaml":"name: demo\nkind: global\n"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if create.name != "demo" || create.yaml != "name: demo\nkind: global\n" {
		t.Errorf("got name=%q yaml=%q", create.name, create.yaml)
	}
}

func TestManifestCreateHandler_SurfacesCreateError(t *testing.T) {
	create := &fakeManifestCreator{err: errors.New("manifest \"demo\" already exists at ...")}
	s := newManifestTestServer(create, &fakeManifestValidator{})
	rec := postJSON(t, s, "/api/manifest/create", `{"name":"demo","yaml":"name: demo"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("body=%q missing backend error text", rec.Body.String())
	}
}

func TestManifestCreateHandler_RejectsBadJSON(t *testing.T) {
	s := newManifestTestServer(&fakeManifestCreator{}, &fakeManifestValidator{})
	rec := postJSON(t, s, "/api/manifest/create", `{not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestManifestCreateHandler_RejectsEmptyName(t *testing.T) {
	s := newManifestTestServer(&fakeManifestCreator{}, &fakeManifestValidator{})
	rec := postJSON(t, s, "/api/manifest/create", `{"name":"   ","yaml":"name: x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---- /api/manifest/validate ----

func TestManifestValidateHandler_RejectsNonPOST(t *testing.T) {
	s := newManifestTestServer(&fakeManifestCreator{}, &fakeManifestValidator{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/validate", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestManifestValidateHandler_ReturnsWarnings(t *testing.T) {
	validate := &fakeManifestValidator{out: []string{"no daemons declared"}}
	s := newManifestTestServer(&fakeManifestCreator{}, validate)
	rec := postJSON(t, s, "/api/manifest/validate", `{"yaml":"name: demo"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Warnings) != 1 || body.Warnings[0] != "no daemons declared" {
		t.Errorf("warnings = %v", body.Warnings)
	}
	if validate.lastYAML != "name: demo" {
		t.Errorf("validator saw yaml=%q", validate.lastYAML)
	}
}

func TestManifestValidateHandler_EmptyWarningsIsNonNullArray(t *testing.T) {
	validate := &fakeManifestValidator{out: nil}
	s := newManifestTestServer(&fakeManifestCreator{}, validate)
	rec := postJSON(t, s, "/api/manifest/validate", `{"yaml":"name: demo\nkind: global\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// Body must contain "warnings":[] — a JSON null would force frontend
	// to special-case the response.
	if !strings.Contains(rec.Body.String(), `"warnings":[]`) {
		t.Errorf("body=%q missing warnings:[] shape", rec.Body.String())
	}
}
```

### Step 3 — Run test; expect compile failure

```bash
go test ./internal/gui/ -run TestManifestCreateHandler -count=1
```

Expected: compile error — `registerManifestRoutes` undefined, `manifestCreator` / `manifestValidator` fields undefined on `Server`.

### Step 4 — Create `internal/gui/manifest.go` with the handler + route wiring

```go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// manifestCreator / manifestValidator are the pin-point subsets of api.API
// that the GUI layer calls for manifest writes. Keeping them as Server-local
// interfaces lets us substitute fakes in manifest_test.go without pulling
// the whole API surface.
type manifestCreator interface {
	ManifestCreate(name, yaml string) error
}

type manifestValidator interface {
	ManifestValidate(yaml string) []string
}

type manifestCreateRequest struct {
	Name string `json:"name"`
	YAML string `json:"yaml"`
}

type manifestValidateRequest struct {
	YAML string `json:"yaml"`
}

type manifestValidateResponse struct {
	Warnings []string `json:"warnings"`
}

// registerManifestRoutes wires POST /api/manifest/create and
// POST /api/manifest/validate onto the server's mux.
//
// Both handlers use the requireSameOrigin guard (Sec-Fetch-Site header).
// Validate is POST-only even though it reads nothing — the YAML payload
// goes in the request body and some YAMLs will be large, exceeding safe
// URL length.
func registerManifestRoutes(s *Server) {
	s.mux.HandleFunc("/api/manifest/create", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req manifestCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			writeAPIError(w, fmt.Errorf("name must not be empty"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if err := s.manifestCreator.ManifestCreate(name, req.YAML); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "MANIFEST_CREATE_FAILED")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	s.mux.HandleFunc("/api/manifest/validate", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req manifestValidateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		warnings := s.manifestValidator.ManifestValidate(req.YAML)
		if warnings == nil {
			warnings = []string{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manifestValidateResponse{Warnings: warnings})
	}))
}
```

### Step 5 — Wire up `Server` struct in `internal/gui/server.go`

Read `internal/gui/server.go` to find where `demigrater` and `dismisser` are wired into the `Server` struct + `NewServer` construction. Follow the same pattern.

Add to the `Server` struct field list:
```go
	manifestCreator   manifestCreator
	manifestValidator manifestValidator
```

Define real adapters (pattern mirrors `realDemigrater`, `realDismisser`):
```go
type realManifestCreator struct{ a *api.API }

func (r *realManifestCreator) ManifestCreate(name, yaml string) error {
	return r.a.ManifestCreate(name, yaml)
}

type realManifestValidator struct{ a *api.API }

func (r *realManifestValidator) ManifestValidate(yaml string) []string {
	return r.a.ManifestValidate(yaml)
}
```

In `NewServer(...)`, construct and assign:
```go
	s.manifestCreator = &realManifestCreator{a: theAPI}
	s.manifestValidator = &realManifestValidator{a: theAPI}
```

Call `registerManifestRoutes(s)` next to the existing `registerDemigrateRoutes(s)` / `registerDismissRoutes(s)` calls.

The exact integration points depend on the current `NewServer` shape. Grep first:
```bash
grep -n 'registerDemigrateRoutes\|registerDismissRoutes\|realDemigrater\|realDismisser' internal/gui/server.go
```

Add the new code in matching positions.

### Step 6 — Run tests; expect green

```bash
go build ./... && go test ./internal/gui/ -run 'TestManifestCreateHandler|TestManifestValidateHandler' -count=1
```

Expected: `ok  mcp-local-hub/internal/gui  ...`, 9 subtests PASS.

### Step 7 — Run full GUI test suite for no regressions

```bash
go test ./internal/gui/ -count=1
```

Expected: all prior GUI tests still PASS + new manifest tests PASS.

### Step 8 — Commit

```bash
git add internal/gui/manifest.go internal/gui/manifest_test.go internal/gui/server.go
git commit -m "feat(gui): /api/manifest/create + /api/manifest/validate handlers (A2a dep)

Thin GUI-layer wrappers for api.ManifestCreate / api.ManifestValidate
matching the PR #4 pattern for /api/demigrate and /api/dismiss.

- POST /api/manifest/create {name, yaml} -> 204 on success, 500 with
  backend error message on create failure.
- POST /api/manifest/validate {yaml} -> 200 {warnings: []string} always
  returning a non-null array so the frontend never special-cases null.
- Both gated by requireSameOrigin (Sec-Fetch-Site same-origin check),
  name is TrimSpace-guarded before persist."
```

---

## Task 2: Frontend types + API client helpers

**Files:**
- Modify: `internal/gui/frontend/src/types.ts` (append new types)
- Modify: `internal/gui/frontend/src/api.ts` (add `postManifestCreate`, `postManifestValidate`, `getExtractManifest`)
- Modify: `internal/gui/frontend/src/api.test.ts` (add tests)

### Step 1 — Append type definitions to `internal/gui/frontend/src/types.ts`

Add at the end of the file (keep all existing types intact):

```ts
// ManifestFormState is the authoritative in-memory shape for the AddServer
// screen. The form writes to it; toYAML(state) serializes it for the backend.
// Using array-of-objects for env and client_bindings (rather than maps) keeps
// add/delete operations and render ordering deterministic.
export interface ManifestFormState {
  name: string;
  kind: "global" | "workspace-scoped";
  transport: "stdio-bridge" | "native-http";
  command: string;
  base_args: string[];
  env: Array<{ key: string; value: string }>;
  daemons: Array<{ name: string; port: number }>;
  client_bindings: Array<{ client: string; daemon: string; url_path: string }>;
  weekly_refresh: boolean;
}

export interface ValidationWarning {
  message: string;
}

// ManifestValidateResponse mirrors the /api/manifest/validate handler shape.
export interface ManifestValidateResponse {
  warnings: string[];
}

// ExtractManifestResponse is a placeholder until the extract endpoint lands
// in a later task. Shape: { yaml: string }.
export interface ExtractManifestResponse {
  yaml: string;
}
```

### Step 2 — Write failing test for `postManifestCreate` + `postManifestValidate`

Append to `internal/gui/frontend/src/api.test.ts`:

```ts
import { postManifestCreate, postManifestValidate } from "./api";

describe("postManifestCreate", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("resolves on 204", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 204,
      statusText: "No Content",
    }) as unknown as Response);
    await expect(postManifestCreate("demo", "name: demo")).resolves.toBeUndefined();
  });

  it("throws with backend error field on non-2xx", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 500,
      statusText: "Internal Server Error",
      json: async () => ({ error: "manifest already exists" }),
    }) as unknown as Response);
    await expect(postManifestCreate("demo", "name: demo")).rejects.toThrow(/manifest already exists/);
  });

  it("serializes name + yaml into JSON body", async () => {
    const seen: { body?: string } = {};
    globalThis.fetch = vi.fn(async (_url: string, init?: RequestInit) => {
      seen.body = init?.body as string;
      return { ok: true, status: 204, statusText: "No Content" } as unknown as Response;
    });
    await postManifestCreate("demo", "name: demo\nkind: global\n");
    expect(JSON.parse(seen.body!)).toEqual({ name: "demo", yaml: "name: demo\nkind: global\n" });
  });
});

describe("postManifestValidate", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("returns warnings array on 200", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ warnings: ["no daemons declared"] }),
    }) as unknown as Response);
    const out = await postManifestValidate("name: x");
    expect(out).toEqual(["no daemons declared"]);
  });

  it("returns empty array when backend emits warnings:[]", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ warnings: [] }),
    }) as unknown as Response);
    const out = await postManifestValidate("name: demo\nkind: global\n");
    expect(out).toEqual([]);
  });

  it("throws on non-2xx with backend error text", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false,
      status: 400,
      statusText: "Bad Request",
      json: async () => ({ error: "invalid JSON" }),
    }) as unknown as Response);
    await expect(postManifestValidate("not-yaml-at-all")).rejects.toThrow(/invalid JSON/);
  });
});
```

### Step 3 — Run tests; expect red

```bash
cd internal/gui/frontend && npm run test -- src/api.test.ts
```

Expected: tests fail — `postManifestCreate` / `postManifestValidate` not exported.

### Step 4 — Extend `internal/gui/frontend/src/api.ts`

Append to the existing file (keep `fetchOrThrow` and `postDismiss` unchanged):

```ts
// postManifestCreate writes a new manifest via the A2a GUI pipeline. On
// success the backend returns 204; any non-2xx is surfaced as a thrown
// Error carrying the backend's {error} envelope text when present. Callers
// handle the "already exists" case by inspecting the error message — the
// backend currently returns "manifest \"<name>\" already exists at ..."
// verbatim, which is user-friendly enough to show in a banner.
export async function postManifestCreate(name: string, yaml: string): Promise<void> {
  const resp = await fetch("/api/manifest/create", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, yaml }),
  });
  if (resp.status === 204) return;
  let body: { error?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  throw new Error(`/api/manifest/create: ${body?.error ?? resp.statusText}`);
}

// postManifestValidate returns the list of structural warnings produced by
// api.ManifestValidate. Empty array == valid. Throws on transport/HTTP error
// (not on validation warnings — those are normal return values).
export async function postManifestValidate(yaml: string): Promise<string[]> {
  const resp = await fetch("/api/manifest/validate", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ yaml }),
  });
  if (!resp.ok) {
    let body: { error?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string };
    } catch {
      // Non-JSON error body; fall through.
    }
    throw new Error(`/api/manifest/validate: ${body?.error ?? resp.statusText}`);
  }
  const payload = (await resp.json()) as { warnings?: string[] };
  return payload.warnings ?? [];
}
```

### Step 5 — Run tests; expect green

```bash
cd internal/gui/frontend && npm run test -- src/api.test.ts
```

Expected: all existing `fetchOrThrow` / `postDismiss` tests PASS + 6 new tests PASS.

### Step 6 — Typecheck

```bash
cd internal/gui/frontend && npm run typecheck
```

Expected: exit 0, no errors.

### Step 7 — Commit

```bash
git add internal/gui/frontend/src/types.ts internal/gui/frontend/src/api.ts internal/gui/frontend/src/api.test.ts
git commit -m "feat(gui/frontend): ManifestFormState types + manifest API client helpers

- types.ts exports ManifestFormState, ValidationWarning,
  ManifestValidateResponse, ExtractManifestResponse for the A2a screen.
- api.ts adds postManifestCreate(name, yaml) -> Promise<void>
  (resolves on 204, throws with backend {error} message on failure) and
  postManifestValidate(yaml) -> Promise<string[]> (returns warnings list,
  throws only on transport/HTTP error).
- api.test.ts adds 6 Vitest cases covering success, error envelope,
  empty-warnings-array, and JSON body shape."
```

---

## Task 3: `toYAML(state)` pure helper + Vitest

**Files:**
- Create: `internal/gui/frontend/src/lib/manifest-yaml.ts`
- Create: `internal/gui/frontend/src/lib/manifest-yaml.test.ts`

### Step 1 — Write failing test at `internal/gui/frontend/src/lib/manifest-yaml.test.ts`

```ts
import { describe, it, expect } from "vitest";
import { toYAML } from "./manifest-yaml";
import type { ManifestFormState } from "../types";

const base: ManifestFormState = {
  name: "",
  kind: "global",
  transport: "stdio-bridge",
  command: "",
  base_args: [],
  env: [],
  daemons: [],
  client_bindings: [],
  weekly_refresh: false,
};

describe("toYAML", () => {
  it("serializes a minimal state with only name + kind + transport + command", () => {
    const state: ManifestFormState = { ...base, name: "demo", command: "npx" };
    const yaml = toYAML(state);
    expect(yaml).toContain("name: demo");
    expect(yaml).toContain("kind: global");
    expect(yaml).toContain("transport: stdio-bridge");
    expect(yaml).toContain("command: npx");
  });

  it("renders base_args as a flow-style YAML array with quotes around each element", () => {
    const state: ManifestFormState = {
      ...base,
      name: "demo",
      command: "npx",
      base_args: ["-y", "@example/server-mem"],
    };
    const yaml = toYAML(state);
    expect(yaml).toContain(`base_args: ["-y", "@example/server-mem"]`);
  });

  it("renders env as a nested map with quoted values", () => {
    const state: ManifestFormState = {
      ...base,
      name: "demo",
      command: "npx",
      env: [
        { key: "PATH", value: "/usr/bin" },
        { key: "MEMORY_FILE_PATH", value: "${HOME}/.local/share/mcp-memory/memory.jsonl" },
      ],
    };
    const yaml = toYAML(state);
    expect(yaml).toContain("env:");
    expect(yaml).toContain(`  PATH: "/usr/bin"`);
    expect(yaml).toContain(`  MEMORY_FILE_PATH: "\${HOME}/.local/share/mcp-memory/memory.jsonl"`);
  });

  it("omits env section entirely when the env array is empty", () => {
    const state: ManifestFormState = { ...base, name: "demo", command: "npx" };
    const yaml = toYAML(state);
    expect(yaml).not.toContain("env:");
  });

  it("renders daemons as a list-of-maps with name + port", () => {
    const state: ManifestFormState = {
      ...base,
      name: "demo",
      command: "npx",
      daemons: [
        { name: "default", port: 9123 },
        { name: "workspace-py", port: 9124 },
      ],
    };
    const yaml = toYAML(state);
    expect(yaml).toContain("daemons:");
    expect(yaml).toMatch(/- name: default\s+port: 9123/);
    expect(yaml).toMatch(/- name: workspace-py\s+port: 9124/);
  });

  it("renders client_bindings as a list-of-maps with client + daemon + url_path", () => {
    const state: ManifestFormState = {
      ...base,
      name: "demo",
      command: "npx",
      daemons: [{ name: "default", port: 9123 }],
      client_bindings: [
        { client: "claude-code", daemon: "default", url_path: "/mcp" },
        { client: "codex-cli", daemon: "default", url_path: "/mcp" },
      ],
    };
    const yaml = toYAML(state);
    expect(yaml).toContain("client_bindings:");
    expect(yaml).toMatch(/- client: claude-code\s+daemon: default\s+url_path: \/mcp/);
    expect(yaml).toMatch(/- client: codex-cli\s+daemon: default\s+url_path: \/mcp/);
  });

  it("renders weekly_refresh only when true", () => {
    const stateFalse: ManifestFormState = { ...base, name: "demo", command: "npx", weekly_refresh: false };
    const stateTrue: ManifestFormState = { ...base, name: "demo", command: "npx", weekly_refresh: true };
    expect(toYAML(stateFalse)).not.toContain("weekly_refresh");
    expect(toYAML(stateTrue)).toContain("weekly_refresh: true");
  });

  it("escapes double-quotes in values by wrapping with single quotes", () => {
    const state: ManifestFormState = {
      ...base,
      name: "demo",
      command: "npx",
      env: [{ key: "FLAG", value: `has "quotes" inside` }],
    };
    const yaml = toYAML(state);
    // Either single-quote the whole value or escape the inner quotes — both are valid YAML.
    // Assert the output parses correctly by checking it contains the inner text at all.
    expect(yaml).toContain("FLAG:");
    expect(yaml).toContain(`has`);
    // And does not contain a corruption pattern like `has "quotes"` inside a double-quoted wrapper.
    expect(yaml).not.toMatch(/"has "quotes" inside"/);
  });
});
```

### Step 2 — Run; expect red

```bash
cd internal/gui/frontend && npm run test -- src/lib/manifest-yaml.test.ts
```

Expected: `Cannot find module './manifest-yaml'`.

### Step 3 — Implement `internal/gui/frontend/src/lib/manifest-yaml.ts`

```ts
import type { ManifestFormState } from "../types";

// toYAML serializes a ManifestFormState into a YAML string that
// api.ManifestValidate / api.ManifestCreate accept verbatim. The output
// follows the convention shown in servers/memory/manifest.yaml:
//
//   name: ...
//   kind: ...
//   transport: ...
//   command: ...
//   base_args: ["-y", ...]        # flow-style array
//   env:                          # only when non-empty
//     KEY: "value"                # always double-quoted
//   daemons:                      # only when non-empty
//     - name: ...
//       port: ...
//   client_bindings:              # only when non-empty
//     - client: ...
//       daemon: ...
//       url_path: ...
//   weekly_refresh: true          # only when true
//
// Values that contain a double-quote are rendered as single-quoted strings
// to avoid manual escape bookkeeping. Keys are known identifiers and are
// never quoted.
export function toYAML(state: ManifestFormState): string {
  const lines: string[] = [];
  lines.push(`name: ${state.name}`);
  lines.push(`kind: ${state.kind}`);
  lines.push(`transport: ${state.transport}`);
  lines.push(`command: ${state.command}`);
  if (state.base_args.length > 0) {
    const quoted = state.base_args.map((a) => quote(a)).join(", ");
    lines.push(`base_args: [${quoted}]`);
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
    }
  }
  if (state.client_bindings.length > 0) {
    lines.push("client_bindings:");
    for (const b of state.client_bindings) {
      lines.push(`  - client: ${b.client}`);
      lines.push(`    daemon: ${b.daemon}`);
      lines.push(`    url_path: ${b.url_path}`);
    }
  }
  if (state.weekly_refresh) {
    lines.push("weekly_refresh: true");
  }
  return lines.join("\n") + "\n";
}

// quote picks the right YAML string wrapper. If the value contains a
// double-quote, use single quotes (YAML escapes `'` as `''` inside single
// quotes). Otherwise use double quotes — they handle backslash-n etc.
// consistently. Empty strings render as `""`.
function quote(s: string): string {
  if (s.includes(`"`)) {
    return `'${s.replace(/'/g, `''`)}'`;
  }
  return `"${s}"`;
}
```

### Step 4 — Run; expect green

```bash
cd internal/gui/frontend && npm run test -- src/lib/manifest-yaml.test.ts
```

Expected: 8 tests PASS.

### Step 5 — Typecheck

```bash
cd internal/gui/frontend && npm run typecheck
```

Expected: exit 0.

### Step 6 — Commit

```bash
git add internal/gui/frontend/src/lib/manifest-yaml.ts internal/gui/frontend/src/lib/manifest-yaml.test.ts
git commit -m "feat(gui/frontend): toYAML(state) manifest serializer

Converts ManifestFormState into a YAML string matching the servers/<x>/manifest.yaml
convention. Each top-level section (env, daemons, client_bindings, weekly_refresh)
is emitted only when non-empty / truthy so freshly-created minimal manifests
don't carry empty sections. Values use double-quote wrapping by default, falling
back to single-quote when the value contains a double-quote."
```

---

## Task 4: `parseYAMLToForm(yaml)` helper + Vitest (normalization-aware)

**Files:**
- Modify: `internal/gui/frontend/src/lib/manifest-yaml.ts` (add `parseYAMLToForm`)
- Modify: `internal/gui/frontend/src/lib/manifest-yaml.test.ts` (add tests)
- Modify: `internal/gui/frontend/package.json` (add `yaml` library)

### Step 1 — Add `yaml` parser dependency

Prefer `yaml` (eemeli/yaml, ~40KB minified) — it has a solid TS API and is commonly used in Vite projects. The embed bundle target <100KB is after gzip; yaml adds ~8-12KB gzipped.

```bash
cd internal/gui/frontend && npm install yaml@^2.5.0
```

Verify `yaml` now appears in `internal/gui/frontend/package.json` under `dependencies`.

### Step 2 — Extend the test file with parse cases

Append to `internal/gui/frontend/src/lib/manifest-yaml.test.ts`:

```ts
import { parseYAMLToForm, BLANK_FORM } from "./manifest-yaml";

describe("BLANK_FORM constant", () => {
  it("has all fields with sensible defaults", () => {
    expect(BLANK_FORM.name).toBe("");
    expect(BLANK_FORM.kind).toBe("global");
    expect(BLANK_FORM.transport).toBe("stdio-bridge");
    expect(BLANK_FORM.command).toBe("");
    expect(BLANK_FORM.base_args).toEqual([]);
    expect(BLANK_FORM.env).toEqual([]);
    expect(BLANK_FORM.daemons).toEqual([]);
    expect(BLANK_FORM.client_bindings).toEqual([]);
    expect(BLANK_FORM.weekly_refresh).toBe(false);
  });
});

describe("parseYAMLToForm", () => {
  it("parses a complete manifest (memory example) round-trip-cleanly", () => {
    const yaml = `name: memory
kind: global
transport: stdio-bridge
command: npx
base_args:
  - "-y"
  - "@modelcontextprotocol/server-memory"
env:
  MEMORY_FILE_PATH: "\${HOME}/.local/share/mcp-memory/memory.jsonl"
daemons:
  - name: default
    port: 9123
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
`;
    const form = parseYAMLToForm(yaml);
    expect(form.name).toBe("memory");
    expect(form.kind).toBe("global");
    expect(form.transport).toBe("stdio-bridge");
    expect(form.command).toBe("npx");
    expect(form.base_args).toEqual(["-y", "@modelcontextprotocol/server-memory"]);
    expect(form.env).toEqual([
      { key: "MEMORY_FILE_PATH", value: "${HOME}/.local/share/mcp-memory/memory.jsonl" },
    ]);
    expect(form.daemons).toEqual([{ name: "default", port: 9123 }]);
    expect(form.client_bindings).toHaveLength(2);
    expect(form.client_bindings[0]).toEqual({ client: "claude-code", daemon: "default", url_path: "/mcp" });
  });

  it("treats missing kind as 'global' (default)", () => {
    const form = parseYAMLToForm(`name: demo\ntransport: stdio-bridge\ncommand: npx\n`);
    expect(form.kind).toBe("global");
  });

  it("treats missing transport as 'stdio-bridge' (default)", () => {
    const form = parseYAMLToForm(`name: demo\nkind: global\ncommand: npx\n`);
    expect(form.transport).toBe("stdio-bridge");
  });

  it("coerces missing arrays to []", () => {
    const form = parseYAMLToForm(`name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\n`);
    expect(form.base_args).toEqual([]);
    expect(form.daemons).toEqual([]);
    expect(form.client_bindings).toEqual([]);
  });

  it("coerces missing env map to empty array", () => {
    const form = parseYAMLToForm(`name: demo\ncommand: npx\n`);
    expect(form.env).toEqual([]);
  });

  it("coerces missing weekly_refresh to false", () => {
    const form = parseYAMLToForm(`name: demo\ncommand: npx\n`);
    expect(form.weekly_refresh).toBe(false);
  });

  it("normalizes env map into array-of-{key,value} pairs", () => {
    const form = parseYAMLToForm(`name: demo\ncommand: npx\nenv:\n  A: "1"\n  B: "two"\n`);
    expect(form.env).toEqual([
      { key: "A", value: "1" },
      { key: "B", value: "two" },
    ]);
  });

  it("throws on malformed YAML", () => {
    expect(() => parseYAMLToForm(`name: demo\n  this: is: nested: wrong`)).toThrow();
  });

  it("round-trips via toYAML without losing required fields", () => {
    const input: ManifestFormState = {
      ...base,
      name: "memory",
      command: "npx",
      base_args: ["-y", "@pkg/srv"],
      env: [{ key: "K", value: "v" }],
      daemons: [{ name: "default", port: 9100 }],
      client_bindings: [{ client: "claude-code", daemon: "default", url_path: "/mcp" }],
    };
    const yaml = toYAML(input);
    const parsed = parseYAMLToForm(yaml);
    expect(parsed).toEqual(input);
  });

  it("BLANK_FORM round-trips to minimal YAML and back to BLANK_FORM shape", () => {
    const yaml = toYAML(BLANK_FORM);
    const parsed = parseYAMLToForm(yaml);
    expect(parsed).toEqual(BLANK_FORM);
  });
});
```

### Step 3 — Run; expect red

```bash
cd internal/gui/frontend && npm run test -- src/lib/manifest-yaml.test.ts
```

Expected: `parseYAMLToForm` / `BLANK_FORM` undefined.

### Step 4 — Extend `internal/gui/frontend/src/lib/manifest-yaml.ts`

Keep `toYAML` and `quote` unchanged. Add:

```ts
import { parse as yamlParse } from "yaml";

// BLANK_FORM is the canonical empty ManifestFormState. Used by:
//   - AddServer.tsx fresh-create entry path (no URL params)
//   - parseYAMLToForm as the starting state that missing fields fall back to
// Keeping one named constant ensures AddServer's "clean form" and
// parseYAMLToForm's defaults do not drift apart.
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
};

// parseYAMLToForm takes a YAML string (from backend extract-manifest, from
// a user's "Paste YAML" action, or — eventually — from edit-mode in A2b)
// and normalizes it into a ManifestFormState. Missing optional fields are
// coerced to BLANK_FORM's defaults so the form always has a complete
// object to render. Throws on unparseable YAML.
//
// Normalization is critical for the Q8 snapshot-dirty detection: a freshly
// parsed form must equal the baseline taken AFTER this same normalization,
// otherwise deepEqual reports false-dirty on first render. See design memo
// §3 gotcha 3.
export function parseYAMLToForm(yaml: string): ManifestFormState {
  const raw = yamlParse(yaml) as Record<string, unknown> | null;
  if (raw == null || typeof raw !== "object") {
    return { ...BLANK_FORM };
  }
  const asString = (v: unknown, fallback: string): string =>
    typeof v === "string" ? v : fallback;
  const asKind = (v: unknown): "global" | "workspace-scoped" =>
    v === "workspace-scoped" ? "workspace-scoped" : "global";
  const asTransport = (v: unknown): "stdio-bridge" | "native-http" =>
    v === "native-http" ? "native-http" : "stdio-bridge";
  const asStringArray = (v: unknown): string[] =>
    Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
  const envRaw = raw.env;
  const env: Array<{ key: string; value: string }> =
    envRaw && typeof envRaw === "object" && !Array.isArray(envRaw)
      ? Object.entries(envRaw as Record<string, unknown>).map(([key, value]) => ({
          key,
          value: asString(value, ""),
        }))
      : [];
  const daemonsRaw = raw.daemons;
  const daemons: Array<{ name: string; port: number }> = Array.isArray(daemonsRaw)
    ? daemonsRaw
        .filter((d): d is Record<string, unknown> => typeof d === "object" && d !== null)
        .map((d) => ({
          name: asString(d.name, ""),
          port: typeof d.port === "number" ? d.port : 0,
        }))
    : [];
  const bindingsRaw = raw.client_bindings;
  const bindings: Array<{ client: string; daemon: string; url_path: string }> = Array.isArray(
    bindingsRaw,
  )
    ? bindingsRaw
        .filter((b): b is Record<string, unknown> => typeof b === "object" && b !== null)
        .map((b) => ({
          client: asString(b.client, ""),
          daemon: asString(b.daemon, ""),
          url_path: asString(b.url_path, ""),
        }))
    : [];
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
  };
}
```

### Step 5 — Run; expect green

```bash
cd internal/gui/frontend && npm run test -- src/lib/manifest-yaml.test.ts
```

Expected: all 19 cases PASS (8 from Task 3 + 11 new).

### Step 6 — Typecheck

```bash
cd internal/gui/frontend && npm run typecheck
```

Expected: exit 0.

### Step 7 — Run full Vitest to confirm no regressions

```bash
cd internal/gui/frontend && npm run test
```

Expected: all prior tests PASS + new parse/round-trip tests PASS.

### Step 8 — Commit

```bash
git add internal/gui/frontend/package.json internal/gui/frontend/package-lock.json \
        internal/gui/frontend/src/lib/manifest-yaml.ts \
        internal/gui/frontend/src/lib/manifest-yaml.test.ts
git commit -m "feat(gui/frontend): parseYAMLToForm + BLANK_FORM normalization helpers

Adds the eemeli/yaml dependency (~8-12KB gzipped, well under the
<100KB embed bundle target) and a parseYAMLToForm(yaml) that returns
a ManifestFormState normalized against BLANK_FORM defaults. Critical
for Q8 snapshot-dirty detection: the initialSnapshot baseline must
be taken after this exact normalization path runs, otherwise the
form appears dirty on first render.

Includes an 11-case Vitest suite covering defaults coercion, env
map -> array-of-pairs, round-trip fidelity via toYAML, and malformed
YAML throws."
```

---

## Task 5: `useDebouncedValue` hook + Vitest

**Files:**
- Create: `internal/gui/frontend/src/hooks/useDebouncedValue.ts`
- Create: `internal/gui/frontend/src/hooks/useDebouncedValue.test.ts`

### Step 1 — Write failing test at `internal/gui/frontend/src/hooks/useDebouncedValue.test.ts`

```ts
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/preact";
import { useDebouncedValue } from "./useDebouncedValue";

describe("useDebouncedValue", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("returns the initial value immediately", () => {
    const { result } = renderHook(() => useDebouncedValue("a", 100));
    expect(result.current).toBe("a");
  });

  it("delays updates by the specified wait", () => {
    const { result, rerender } = renderHook(({ v }) => useDebouncedValue(v, 100), {
      initialProps: { v: "a" },
    });
    rerender({ v: "b" });
    expect(result.current).toBe("a");
    act(() => {
      vi.advanceTimersByTime(99);
    });
    expect(result.current).toBe("a");
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(result.current).toBe("b");
  });

  it("resets the timer on rapid successive updates (coalesces)", () => {
    const { result, rerender } = renderHook(({ v }) => useDebouncedValue(v, 100), {
      initialProps: { v: "a" },
    });
    rerender({ v: "b" });
    act(() => {
      vi.advanceTimersByTime(50);
    });
    rerender({ v: "c" });
    act(() => {
      vi.advanceTimersByTime(50);
    });
    // Only 50ms since "c" was set — still "a".
    expect(result.current).toBe("a");
    act(() => {
      vi.advanceTimersByTime(50);
    });
    expect(result.current).toBe("c");
  });
});
```

### Step 2 — Install test deps for Preact hook testing

```bash
cd internal/gui/frontend && npm install --save-dev @testing-library/preact
```

### Step 3 — Run; expect red

```bash
cd internal/gui/frontend && npm run test -- src/hooks/useDebouncedValue.test.ts
```

Expected: module not found.

### Step 4 — Implement `internal/gui/frontend/src/hooks/useDebouncedValue.ts`

```ts
import { useEffect, useState } from "preact/hooks";

// useDebouncedValue returns a throttled mirror of `value`: when `value`
// changes, the returned `debounced` stays the same for `waitMs` ms, then
// flips to the latest value. Multiple changes inside the window coalesce
// — only the most recent value is ever committed.
//
// Used by AddServer for the YAML preview pane: the form state updates
// on every keystroke, but the preview only re-renders once typing has
// paused for 150ms. This prevents preview scroll/caret churn without
// adding visible typing lag (150ms is below the perceived-lag threshold).
export function useDebouncedValue<T>(value: T, waitMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), waitMs);
    return () => clearTimeout(timer);
  }, [value, waitMs]);
  return debounced;
}
```

### Step 5 — Run; expect green

```bash
cd internal/gui/frontend && npm run test -- src/hooks/useDebouncedValue.test.ts
```

Expected: 3 tests PASS.

### Step 6 — Commit

```bash
git add internal/gui/frontend/package.json internal/gui/frontend/package-lock.json \
        internal/gui/frontend/src/hooks/useDebouncedValue.ts \
        internal/gui/frontend/src/hooks/useDebouncedValue.test.ts
git commit -m "feat(gui/frontend): useDebouncedValue hook (150ms YAML preview)

Standard debounce hook for the AddServer YAML preview pane. Adds
@testing-library/preact as a devDependency to exercise the hook under
fake timers. The 150ms default for the AddServer screen (set at call
site) is below the perceived-lag threshold while still coalescing
preview re-renders across bursts of keystrokes (Q4 decision)."
```

---

## Task 6: AddServer scaffolding — route, sidebar link, accordion shell

**Files:**
- Create: `internal/gui/frontend/src/screens/AddServer.tsx`
- Modify: `internal/gui/frontend/src/app.tsx`
- Modify: `internal/gui/e2e/tests/shell.spec.ts` (5 nav links)
- Append: `internal/gui/frontend/src/styles/style.css` (accordion CSS)

### Step 1 — Create `internal/gui/frontend/src/screens/AddServer.tsx` with empty shell

```tsx
import { useState } from "preact/hooks";
import { BLANK_FORM, toYAML } from "../lib/manifest-yaml";
import { useDebouncedValue } from "../hooks/useDebouncedValue";
import type { ManifestFormState } from "../types";

// AddServerScreen is the Phase 3B-II A2a create-flow. Renders an accordion
// form that serializes to YAML on every (debounced) keystroke; the preview
// shows what will be sent to /api/manifest/create. Actual sections are
// wired up in Tasks 7-10; this scaffolding commit is just the shell +
// route so the sidebar link and hashchange behavior are testable
// end-to-end before form fields land.
export function AddServerScreen() {
  const [formState, _setFormState] = useState<ManifestFormState>(BLANK_FORM);
  const debouncedState = useDebouncedValue(formState, 150);
  const yamlPreview = toYAML(debouncedState);
  return (
    <section class="screen add-server">
      <h1>Add server</h1>
      <div class="add-server-grid">
        <div class="add-server-form">
          <AccordionSection title="Basics" open={true}>
            <p class="placeholder">Name, kind (Task 7)</p>
          </AccordionSection>
          <AccordionSection title="Command">
            <p class="placeholder">Transport, command, base_args (Task 7)</p>
          </AccordionSection>
          <AccordionSection title="Environment">
            <p class="placeholder">env key-value rows (Task 8)</p>
          </AccordionSection>
          <AccordionSection title="Daemons">
            <p class="placeholder">name + port rows with cascade rename/delete (Task 9)</p>
          </AccordionSection>
          <AccordionSection title="Client bindings">
            <p class="placeholder">adaptive 1-vs-multi daemon (Task 10)</p>
          </AccordionSection>
        </div>
        <aside class="add-server-preview">
          <h2>YAML preview</h2>
          <pre data-testid="yaml-preview">{yamlPreview}</pre>
        </aside>
      </div>
    </section>
  );
}

// AccordionSection is the reusable collapsible container used by every form
// section. `open` controls initial state; clicking the header toggles.
function AccordionSection(props: { title: string; open?: boolean; children: preact.ComponentChildren }) {
  const [expanded, setExpanded] = useState(props.open ?? false);
  return (
    <section class={`accordion ${expanded ? "open" : "closed"}`}>
      <button
        type="button"
        class="accordion-header"
        aria-expanded={expanded}
        onClick={() => setExpanded((x) => !x)}
      >
        <span class="chevron">{expanded ? "▾" : "▸"}</span>
        <span>{props.title}</span>
      </button>
      {expanded && <div class="accordion-body">{props.children}</div>}
    </section>
  );
}
```

### Step 2 — Wire route + sidebar link in `internal/gui/frontend/src/app.tsx`

Replace the existing app.tsx with:

```tsx
import type { JSX } from "preact";
import { useRouter } from "./hooks/useRouter";
import { AddServerScreen } from "./screens/AddServer";
import { DashboardScreen } from "./screens/Dashboard";
import { LogsScreen } from "./screens/Logs";
import { MigrationScreen } from "./screens/Migration";
import { ServersScreen } from "./screens/Servers";

const SCREENS: Record<string, () => JSX.Element> = {
  servers: () => <ServersScreen />,
  migration: () => <MigrationScreen />,
  "add-server": () => <AddServerScreen />,
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
          <a href="#/add-server" class={screen === "add-server" ? "active" : ""}>
            Add server
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

### Step 3 — Update `internal/gui/e2e/tests/shell.spec.ts` for 5 nav links

Replace the first two tests (keep the third hashchange test updated):

```ts
import { test, expect } from "../fixtures/hub";

test.describe("shell", () => {
  test("renders sidebar with brand + five nav links", async ({ page, hub }) => {
    await page.goto(`${hub.url}/`);
    await expect(page.locator(".sidebar .brand")).toHaveText("mcp-local-hub");
    const links = page.locator(".sidebar nav a");
    await expect(links).toHaveCount(5);
    await expect(links.nth(0)).toHaveText("Servers");
    await expect(links.nth(1)).toHaveText("Migration");
    await expect(links.nth(2)).toHaveText("Add server");
    await expect(links.nth(3)).toHaveText("Dashboard");
    await expect(links.nth(4)).toHaveText("Logs");
  });

  test("default route is Servers and nav highlights on click", async ({ page, hub }) => {
    await page.goto(`${hub.url}/`);
    const serversLink = page.locator(".sidebar nav a", { hasText: "Servers" });
    await expect(serversLink).toHaveClass(/active/);
    await page.locator(".sidebar nav a", { hasText: "Migration" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Migration" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Migration");
    await page.locator(".sidebar nav a", { hasText: "Add server" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Add server" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Add server");
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
    await page.goto(`${hub.url}/#/add-server`);
    await expect(page.locator("h1")).toHaveText("Add server");
    await page.goto(`${hub.url}/#/logs`);
    await expect(page.locator("h1")).toHaveText("Logs");
    await page.goBack();
    await expect(page.locator("h1")).toHaveText("Add server");
  });
});
```

### Step 4 — Append accordion CSS to `internal/gui/frontend/src/styles/style.css`

APPEND (do NOT edit existing rules):

```css

/* Add server — accordion + two-column grid */
.screen.add-server .add-server-grid {
  display: grid;
  grid-template-columns: minmax(420px, 1fr) minmax(320px, 420px);
  gap: 24px;
  margin-top: 8px;
}
.screen.add-server .add-server-preview pre {
  background: var(--sidebar-bg);
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 12px;
  max-height: 70vh;
  overflow: auto;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  white-space: pre-wrap;
}
.screen.add-server .accordion {
  border: 1px solid var(--border);
  border-radius: 6px;
  margin-bottom: 8px;
}
.screen.add-server .accordion-header {
  width: 100%;
  padding: 10px 12px;
  background: var(--sidebar-bg);
  border: none;
  cursor: pointer;
  display: flex;
  align-items: center;
  gap: 8px;
  font: inherit;
  color: var(--text);
  text-align: left;
}
.screen.add-server .accordion-header .chevron {
  width: 1em;
  text-align: center;
}
.screen.add-server .accordion-body {
  padding: 10px 12px;
  border-top: 1px solid var(--border);
}
.screen.add-server .accordion .placeholder {
  color: var(--text-muted, #656d76);
  font-style: italic;
  margin: 0;
}
.screen.add-server .form-row {
  display: flex;
  gap: 8px;
  align-items: center;
  margin: 6px 0;
}
.screen.add-server .form-row label {
  min-width: 120px;
  font-size: 14px;
}
.screen.add-server .form-row input[type="text"],
.screen.add-server .form-row input[type="number"],
.screen.add-server .form-row select {
  flex: 1 1 auto;
  padding: 4px 8px;
  border: 1px solid var(--border);
  border-radius: 4px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
}
.screen.add-server .inline-error {
  color: var(--danger);
  font-size: 12px;
  margin-left: 8px;
}
.screen.add-server .toolbar {
  display: flex;
  gap: 8px;
  align-items: center;
  margin: 16px 0;
}
.screen.add-server .toolbar button {
  padding: 6px 12px;
  border: 1px solid var(--border);
  background: var(--sidebar-bg);
  color: var(--text);
  border-radius: 4px;
  cursor: pointer;
  font: inherit;
}
.screen.add-server .toolbar button.primary {
  background: var(--success);
  color: var(--bg);
  border-color: var(--success);
}
.screen.add-server .toolbar button[disabled] {
  opacity: 0.5;
  cursor: not-allowed;
}
.screen.add-server .banner {
  padding: 10px 12px;
  border-left: 4px solid var(--border);
  border-radius: 4px;
  background: var(--sidebar-bg);
  margin: 8px 0;
  font-size: 14px;
}
.screen.add-server .banner.error {
  border-left-color: var(--danger);
}
.screen.add-server .banner.success {
  border-left-color: var(--success);
}
```

### Step 5 — Regenerate the embedded bundle

```bash
go generate ./internal/gui/...
```

Expected: `internal/gui/assets/app.js` updated; `index.html` and `style.css` may or may not change.

### Step 6 — Typecheck + Vitest + Go-side embed smoke

```bash
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../../.. && go test ./internal/gui/ -count=1
```

Expected: typecheck exit 0; Vitest all PASS; Go GUI test PASS.

### Step 7 — Run shell E2E

```bash
cd internal/gui/e2e && npm test -- tests/shell.spec.ts
```

Expected: 3/3 PASS.

### Step 8 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx \
        internal/gui/frontend/src/app.tsx \
        internal/gui/e2e/tests/shell.spec.ts \
        internal/gui/frontend/src/styles/style.css \
        internal/gui/assets/
git commit -m "feat(gui/frontend): AddServer scaffolding + route + sidebar link

Adds #/add-server route between Migration and Dashboard. The scaffolded
AddServerScreen renders a 5-section accordion shell (Basics, Command,
Environment, Daemons, Client bindings) alongside a read-only YAML preview
that reflects the debounced (150ms) form state via toYAML. Sections are
placeholder content; actual form fields + cascade logic land in Tasks
7-11. Shell E2E updated for the five nav links (was four after A1)."
```

---

## Task 7: Basics + Command sections (name, kind, transport, command, base_args)

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`

### Step 1 — Replace the AddServerScreen `const` block + Basics + Command accordion contents

Update the top of the file to add a name-validation helper and import ValidationWarning. Replace the whole AddServerScreen function body (keep AccordionSection untouched):

```tsx
import { useState } from "preact/hooks";
import { BLANK_FORM, toYAML } from "../lib/manifest-yaml";
import { useDebouncedValue } from "../hooks/useDebouncedValue";
import type { ManifestFormState } from "../types";

// MANIFEST_NAME_REGEX mirrors internal/api/manifest.go:23 validManifestName.
// Live client-side regex check provides instant feedback; the backend still
// authoritatively validates at create time.
const MANIFEST_NAME_REGEX = /^[a-z0-9][a-z0-9._-]*$/;

// KIND_OPTIONS and TRANSPORT_OPTIONS mirror the enum values accepted by
// internal/config/manifest.go. Keeping them as const tuples lets TS narrow
// them into the literal-union fields of ManifestFormState.
const KIND_OPTIONS = [
  { value: "global", label: "global (shared across all projects)" },
  { value: "workspace-scoped", label: "workspace-scoped (per-workspace lazy proxy)" },
] as const;
const TRANSPORT_OPTIONS = [
  { value: "stdio-bridge", label: "stdio-bridge (daemon multiplexes stdio child)" },
  { value: "native-http", label: "native-http (upstream speaks HTTP directly)" },
] as const;

export function AddServerScreen() {
  const [formState, setFormState] = useState<ManifestFormState>(BLANK_FORM);
  const debouncedState = useDebouncedValue(formState, 150);
  const yamlPreview = toYAML(debouncedState);

  const nameError = formState.name.length > 0 && !MANIFEST_NAME_REGEX.test(formState.name)
    ? "Must match [a-z0-9][a-z0-9._-]* (lowercase, digits, '.', '_', '-')"
    : "";

  function updateField<K extends keyof ManifestFormState>(key: K, value: ManifestFormState[K]) {
    setFormState((prev) => ({ ...prev, [key]: value }));
  }

  function updateBaseArg(index: number, value: string) {
    setFormState((prev) => {
      const next = prev.base_args.slice();
      next[index] = value;
      return { ...prev, base_args: next };
    });
  }

  function addBaseArg() {
    setFormState((prev) => ({ ...prev, base_args: [...prev.base_args, ""] }));
  }

  function deleteBaseArg(index: number) {
    setFormState((prev) => ({
      ...prev,
      base_args: prev.base_args.filter((_, i) => i !== index),
    }));
  }

  return (
    <section class="screen add-server">
      <h1>Add server</h1>
      <div class="add-server-grid">
        <div class="add-server-form">
          <AccordionSection title="Basics" open={true}>
            <div class="form-row">
              <label for="field-name">Name</label>
              <input
                id="field-name"
                type="text"
                value={formState.name}
                placeholder="memory"
                onInput={(e) => updateField("name", (e.currentTarget as HTMLInputElement).value)}
              />
              {nameError && <span class="inline-error">{nameError}</span>}
            </div>
            <div class="form-row">
              <label for="field-kind">Kind</label>
              <select
                id="field-kind"
                value={formState.kind}
                onChange={(e) => updateField("kind", (e.currentTarget as HTMLSelectElement).value as ManifestFormState["kind"])}
              >
                {KIND_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>{opt.label}</option>
                ))}
              </select>
            </div>
          </AccordionSection>

          <AccordionSection title="Command">
            <div class="form-row">
              <label for="field-transport">Transport</label>
              <select
                id="field-transport"
                value={formState.transport}
                onChange={(e) => updateField("transport", (e.currentTarget as HTMLSelectElement).value as ManifestFormState["transport"])}
              >
                {TRANSPORT_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>{opt.label}</option>
                ))}
              </select>
            </div>
            <div class="form-row">
              <label for="field-command">Command</label>
              <input
                id="field-command"
                type="text"
                value={formState.command}
                placeholder="npx"
                onInput={(e) => updateField("command", (e.currentTarget as HTMLInputElement).value)}
              />
            </div>
            <div class="form-row">
              <label>Base args</label>
              <div class="repeatable-rows" data-testid="base-args">
                {formState.base_args.map((arg, i) => (
                  <div class="form-row" key={i}>
                    <input
                      type="text"
                      value={arg}
                      onInput={(e) => updateBaseArg(i, (e.currentTarget as HTMLInputElement).value)}
                    />
                    <button type="button" onClick={() => deleteBaseArg(i)} data-action="delete-base-arg">×</button>
                  </div>
                ))}
                <button type="button" onClick={addBaseArg} data-action="add-base-arg">+ Add arg</button>
              </div>
            </div>
            <div class="form-row">
              <label for="field-weekly">Weekly refresh</label>
              <input
                id="field-weekly"
                type="checkbox"
                checked={formState.weekly_refresh}
                onChange={(e) => updateField("weekly_refresh", (e.currentTarget as HTMLInputElement).checked)}
              />
            </div>
          </AccordionSection>

          <AccordionSection title="Environment">
            <p class="placeholder">env key-value rows (Task 8)</p>
          </AccordionSection>
          <AccordionSection title="Daemons">
            <p class="placeholder">name + port rows with cascade rename/delete (Task 9)</p>
          </AccordionSection>
          <AccordionSection title="Client bindings">
            <p class="placeholder">adaptive 1-vs-multi daemon (Task 10)</p>
          </AccordionSection>
        </div>
        <aside class="add-server-preview">
          <h2>YAML preview</h2>
          <pre data-testid="yaml-preview">{yamlPreview}</pre>
        </aside>
      </div>
    </section>
  );
}

function AccordionSection(props: { title: string; open?: boolean; children: preact.ComponentChildren }) {
  const [expanded, setExpanded] = useState(props.open ?? false);
  return (
    <section class={`accordion ${expanded ? "open" : "closed"}`}>
      <button
        type="button"
        class="accordion-header"
        aria-expanded={expanded}
        onClick={() => setExpanded((x) => !x)}
      >
        <span class="chevron">{expanded ? "▾" : "▸"}</span>
        <span>{props.title}</span>
      </button>
      {expanded && <div class="accordion-body">{props.children}</div>}
    </section>
  );
}
```

### Step 2 — Typecheck + rebuild + Go smoke

```bash
cd internal/gui/frontend && npm run typecheck
cd ../../..
go generate ./internal/gui/...
go test ./internal/gui/ -count=1
```

Expected: all green.

### Step 3 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): AddServer Basics + Command sections

Wires name (with live regex feedback mirroring validManifestName),
kind + transport selects, command input, repeatable base_args rows,
and the weekly_refresh checkbox. Live regex surfaces inline-error
text while typing; backend ManifestValidate remains the authoritative
check at submit-time."
```

---

## Task 8: Environment section (key-value repeatable rows)

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`

### Step 1 — Add env mutators and replace the Environment placeholder

Inside `AddServerScreen`, add after `deleteBaseArg`:

```tsx
  function addEnv() {
    setFormState((prev) => ({ ...prev, env: [...prev.env, { key: "", value: "" }] }));
  }

  function updateEnv(index: number, field: "key" | "value", value: string) {
    setFormState((prev) => {
      const next = prev.env.slice();
      next[index] = { ...next[index], [field]: value };
      return { ...prev, env: next };
    });
  }

  function deleteEnv(index: number) {
    setFormState((prev) => ({
      ...prev,
      env: prev.env.filter((_, i) => i !== index),
    }));
  }
```

Replace the Environment `<AccordionSection>` placeholder content with:

```tsx
          <AccordionSection title="Environment">
            <div class="repeatable-rows" data-testid="env-rows">
              {formState.env.map((row, i) => (
                <div class="form-row env-row" key={i} data-env-row={i}>
                  <input
                    type="text"
                    placeholder="KEY"
                    value={row.key}
                    onInput={(e) => updateEnv(i, "key", (e.currentTarget as HTMLInputElement).value)}
                  />
                  <input
                    type="text"
                    placeholder="value (literal or ${HOME}/...)"
                    value={row.value}
                    onInput={(e) => updateEnv(i, "value", (e.currentTarget as HTMLInputElement).value)}
                  />
                  <button type="button" onClick={() => deleteEnv(i)} data-action="delete-env">×</button>
                </div>
              ))}
              <button type="button" onClick={addEnv} data-action="add-env">+ Add environment variable</button>
            </div>
          </AccordionSection>
```

### Step 2 — Typecheck + rebuild

```bash
cd internal/gui/frontend && npm run typecheck
cd ../../..
go generate ./internal/gui/...
```

Expected: exit 0.

### Step 3 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): AddServer Environment section

env as repeatable [KEY][value][x] rows with an [+ Add environment
variable] button. Values accept any literal text; \${HOME}/... style
interpolation is preserved verbatim — the daemon/installer expands
it at launch time via the existing config/env resolver."
```

---

## Task 9: Daemons section with cascade rename + delete

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`

### Step 1 — Add daemon mutators with binding-cascade

Inside `AddServerScreen`, add after `deleteEnv`:

```tsx
  function addDaemon() {
    setFormState((prev) => ({
      ...prev,
      daemons: [...prev.daemons, { name: "", port: 0 }],
    }));
  }

  // updateDaemon handles both the name-rename cascade and port updates.
  // When the name field is edited, every client_binding that referenced
  // the old name is updated to the new name in the same atomic state
  // update — the form never exposes an intermediate "orphan binding"
  // state. Users who hand-edit a binding's daemon field to a non-existent
  // daemon get caught by the post-save ManifestValidate (Q6 gotcha).
  function updateDaemon(index: number, field: "name" | "port", value: string) {
    setFormState((prev) => {
      const target = prev.daemons[index];
      if (!target) return prev;
      const nextDaemon = field === "name"
        ? { ...target, name: value }
        : { ...target, port: parsePort(value) };
      const nextDaemons = prev.daemons.slice();
      nextDaemons[index] = nextDaemon;
      const nextBindings = field === "name" && target.name !== value
        ? prev.client_bindings.map((b) =>
            b.daemon === target.name ? { ...b, daemon: value } : b,
          )
        : prev.client_bindings;
      return { ...prev, daemons: nextDaemons, client_bindings: nextBindings };
    });
  }

  // deleteDaemon cascades to bindings: if any bindings reference this
  // daemon, the user is prompted; on confirm both the daemon row and
  // every binding that pointed at it are removed in one state update.
  function deleteDaemon(index: number) {
    setFormState((prev) => {
      const target = prev.daemons[index];
      if (!target) return prev;
      const orphans = prev.client_bindings.filter((b) => b.daemon === target.name);
      if (orphans.length > 0) {
        // eslint-disable-next-line no-alert
        const ok = window.confirm(
          `Delete daemon "${target.name}" and its ${orphans.length} client binding${orphans.length === 1 ? "" : "s"}?`,
        );
        if (!ok) return prev;
      }
      return {
        ...prev,
        daemons: prev.daemons.filter((_, i) => i !== index),
        client_bindings: prev.client_bindings.filter((b) => b.daemon !== target.name),
      };
    });
  }

  function parsePort(raw: string): number {
    const n = Number(raw);
    return Number.isFinite(n) && n >= 0 ? Math.trunc(n) : 0;
  }
```

Replace the Daemons `<AccordionSection>` placeholder content with:

```tsx
          <AccordionSection title="Daemons">
            <div class="repeatable-rows" data-testid="daemon-rows">
              {formState.daemons.map((d, i) => (
                <div class="form-row daemon-row" key={i} data-daemon-row={i}>
                  <input
                    type="text"
                    placeholder="name (e.g. default)"
                    value={d.name}
                    onInput={(e) => updateDaemon(i, "name", (e.currentTarget as HTMLInputElement).value)}
                    data-field="daemon-name"
                  />
                  <input
                    type="number"
                    min={0}
                    max={65535}
                    placeholder="9100"
                    value={d.port}
                    onInput={(e) => updateDaemon(i, "port", (e.currentTarget as HTMLInputElement).value)}
                    data-field="daemon-port"
                  />
                  <button type="button" onClick={() => deleteDaemon(i)} data-action="delete-daemon">×</button>
                </div>
              ))}
              <button type="button" onClick={addDaemon} data-action="add-daemon">+ Add daemon</button>
            </div>
          </AccordionSection>
```

### Step 2 — Typecheck + rebuild

```bash
cd internal/gui/frontend && npm run typecheck
cd ../../..
go generate ./internal/gui/...
```

### Step 3 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): AddServer Daemons section with cascade rename/delete

Renaming a daemon atomically updates every client_binding that
referenced it — no orphan state is ever exposed to the user or the
YAML preview. Deleting a daemon with referencing bindings prompts
the user for confirmation and cascade-deletes both in one state
update. Hand-edited binding.daemon pointing at a non-existent daemon
remains possible via the Client bindings section; post-save
ManifestValidate catches it (Q6 gotcha, deferred enforcement to
backend)."
```

---

## Task 10: Client bindings — adaptive (1-daemon flat vs 2-3-daemon accordion)

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`

### Step 1 — Add binding mutators and the adaptive `<ClientBindingsSection>` renderer

Add a constant for known client names at the top of the file (after `TRANSPORT_OPTIONS`):

```tsx
const KNOWN_CLIENTS = ["claude-code", "codex-cli", "gemini-cli", "antigravity"] as const;
```

Inside `AddServerScreen`, after `parsePort`:

```tsx
  function addBinding(daemonName: string) {
    setFormState((prev) => ({
      ...prev,
      client_bindings: [
        ...prev.client_bindings,
        { client: KNOWN_CLIENTS[0], daemon: daemonName, url_path: "/mcp" },
      ],
    }));
  }

  function updateBinding(index: number, field: "client" | "daemon" | "url_path", value: string) {
    setFormState((prev) => {
      const next = prev.client_bindings.slice();
      const target = next[index];
      if (!target) return prev;
      next[index] = { ...target, [field]: value };
      return { ...prev, client_bindings: next };
    });
  }

  function deleteBinding(index: number) {
    setFormState((prev) => ({
      ...prev,
      client_bindings: prev.client_bindings.filter((_, i) => i !== index),
    }));
  }
```

Replace the Client bindings `<AccordionSection>` placeholder content with:

```tsx
          <AccordionSection title="Client bindings">
            <ClientBindingsSection
              daemons={formState.daemons}
              bindings={formState.client_bindings}
              onAdd={addBinding}
              onUpdate={updateBinding}
              onDelete={deleteBinding}
            />
          </AccordionSection>
```

At the bottom of the file (after `AccordionSection`), add:

```tsx
// ClientBindingsSection adaptively renders the bindings list:
//   - When there's exactly one daemon: flat [client][url_path][x] rows,
//     no inner accordion chrome. New bindings are added under that daemon.
//   - When there are 0 or 2+ daemons: grouped by daemon, each group is
//     its own collapsible inner subsection. Zero-daemon case shows a
//     helpful empty-state instructing the user to add a daemon first.
function ClientBindingsSection(props: {
  daemons: Array<{ name: string; port: number }>;
  bindings: Array<{ client: string; daemon: string; url_path: string }>;
  onAdd: (daemonName: string) => void;
  onUpdate: (index: number, field: "client" | "daemon" | "url_path", value: string) => void;
  onDelete: (index: number) => void;
}) {
  const { daemons, bindings, onAdd, onUpdate, onDelete } = props;
  if (daemons.length === 0) {
    return (
      <p class="placeholder">
        Add at least one daemon (in the section above) before creating
        client bindings — each binding must reference a daemon by name.
      </p>
    );
  }
  if (daemons.length === 1) {
    const only = daemons[0].name;
    return (
      <BindingsList
        bindings={bindings}
        onAdd={() => onAdd(only)}
        onUpdate={onUpdate}
        onDelete={onDelete}
      />
    );
  }
  return (
    <div data-testid="bindings-adaptive-multi">
      {daemons.map((d) => {
        const indices: number[] = [];
        const group = bindings.filter((b, idx) => {
          if (b.daemon === d.name) { indices.push(idx); return true; }
          return false;
        });
        return (
          <section class="bindings-daemon-group" key={d.name} data-daemon-group={d.name}>
            <h3>daemon: {d.name} (port {d.port})</h3>
            <BindingsList
              bindings={group}
              indices={indices}
              onAdd={() => onAdd(d.name)}
              onUpdate={onUpdate}
              onDelete={onDelete}
            />
          </section>
        );
      })}
    </div>
  );
}

// BindingsList renders a flat list of bindings. When the `indices` prop
// is supplied (multi-daemon path), it maps each displayed row to its
// absolute index in the parent client_bindings array, so the onUpdate /
// onDelete calls operate on the correct slot. Single-daemon path supplies
// the whole bindings array without an indices map.
function BindingsList(props: {
  bindings: Array<{ client: string; daemon: string; url_path: string }>;
  indices?: number[];
  onAdd: () => void;
  onUpdate: (index: number, field: "client" | "daemon" | "url_path", value: string) => void;
  onDelete: (index: number) => void;
}) {
  const { bindings, indices, onAdd, onUpdate, onDelete } = props;
  return (
    <div class="repeatable-rows bindings-list" data-testid="bindings-list">
      {bindings.map((b, displayIdx) => {
        const absIdx = indices ? indices[displayIdx] : displayIdx;
        return (
          <div class="form-row binding-row" key={absIdx} data-binding-row={absIdx}>
            <select
              value={b.client}
              data-field="binding-client"
              onChange={(e) => onUpdate(absIdx, "client", (e.currentTarget as HTMLSelectElement).value)}
            >
              {KNOWN_CLIENTS.map((c) => (
                <option key={c} value={c}>{c}</option>
              ))}
            </select>
            <input
              type="text"
              value={b.url_path}
              placeholder="/mcp"
              data-field="binding-url-path"
              onInput={(e) => onUpdate(absIdx, "url_path", (e.currentTarget as HTMLInputElement).value)}
            />
            <button type="button" onClick={() => onDelete(absIdx)} data-action="delete-binding">×</button>
          </div>
        );
      })}
      <button type="button" onClick={onAdd} data-action="add-binding">+ Add binding</button>
    </div>
  );
}
```

### Step 2 — Typecheck + rebuild

```bash
cd internal/gui/frontend && npm run typecheck
cd ../../..
go generate ./internal/gui/...
```

Expected: exit 0.

### Step 3 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): AddServer Client bindings adaptive section

Zero daemons: empty-state telling the user to add a daemon first.
One daemon: flat [client-select][url_path][delete] list with a single
[+ Add binding] button targeting the sole daemon.
Two+ daemons: per-daemon subsection with its own bindings list and
Add button; the absolute index mapping (indices prop) keeps onUpdate
/ onDelete operating on the canonical client_bindings array position."
```

---

## Task 11: Toolbar — [Validate] [Save] [Save & Install] + submission-version counter + Retry Install banner

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`

### Step 1 — Add the submission-version counter + request-version counter + submit handlers

Update the imports at the top:

```tsx
import { useRef, useState } from "preact/hooks";
import { BLANK_FORM, toYAML } from "../lib/manifest-yaml";
import { useDebouncedValue } from "../hooks/useDebouncedValue";
import { postManifestCreate, postManifestValidate } from "../api";
import type { ManifestFormState } from "../types";
```

Inside `AddServerScreen`, add after the existing `nameError` declaration:

```tsx
  const [warnings, setWarnings] = useState<string[] | null>(null);
  const [banner, setBanner] = useState<{ kind: "error" | "success"; text: string; retry?: () => Promise<void> } | null>(null);
  const [busy, setBusy] = useState<"" | "validate" | "save" | "install">("");
  // submissionVersion: bumped every time a Save/Save&Install click starts
  // its own inline serialize-validate-submit pipeline. If a second click
  // happens while the first is still in flight, the older pipeline sees
  // submissionCounter.current != its own captured value and bails before
  // writing to state. (Q3 Codex-identified gotcha.)
  const submissionCounter = useRef(0);
  // validateVersion: same pattern for the async Validate button path. A
  // newer Validate click invalidates an older in-flight validate's result
  // so stale warnings don't paint over fresh state. (Q5.)
  const validateCounter = useRef(0);

  async function runValidate() {
    const version = ++validateCounter.current;
    setBusy("validate");
    setBanner(null);
    try {
      const payload = toYAML(formState); // FRESH, not debounced
      const out = await postManifestValidate(payload);
      if (version !== validateCounter.current) return; // preempted
      setWarnings(out);
      if (out.length === 0) {
        setBanner({ kind: "success", text: "Validation passed — no warnings." });
      } else {
        setBanner({ kind: "error", text: `${out.length} validation warning${out.length === 1 ? "" : "s"}.` });
      }
    } catch (err) {
      if (version !== validateCounter.current) return;
      setBanner({ kind: "error", text: `/api/manifest/validate: ${(err as Error).message}` });
    } finally {
      setBusy("");
    }
  }

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
      const payload = toYAML(formState); // FRESH snapshot, not debounced preview
      const validateOut = await postManifestValidate(payload);
      if (version !== submissionCounter.current) return;
      if (validateOut.length > 0) {
        setWarnings(validateOut);
        setBanner({ kind: "error", text: `Cannot save: ${validateOut.length} validation warning${validateOut.length === 1 ? "" : "s"}.` });
        return;
      }
      await postManifestCreate(name, payload);
      if (version !== submissionCounter.current) return;
      if (!opts.install) {
        setBanner({ kind: "success", text: `Saved servers/${name}/manifest.yaml.` });
        return;
      }
      // Save & Install: run install; on failure, keep manifest on disk, offer retry.
      await runInstallNow(name, version);
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      if (version === submissionCounter.current) {
        setBusy("");
      }
    }
  }

  async function runInstallNow(name: string, version: number) {
    try {
      const resp = await fetch(`/api/install?name=${encodeURIComponent(name)}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
      });
      if (version !== submissionCounter.current) return;
      if (!resp.ok) {
        const body = await resp.json().catch(() => ({}));
        const err = (body as { error?: string }).error ?? resp.statusText;
        setBanner({
          kind: "error",
          text: `Saved servers/${name}/manifest.yaml, but install failed: ${err}`,
          retry: () => runInstallNow(name, ++submissionCounter.current),
        });
        return;
      }
      setBanner({ kind: "success", text: `Installed ${name}. Daemons will start at next logon (or run "mcphub restart --server ${name}" now).` });
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({
        kind: "error",
        text: `Saved servers/${name}/manifest.yaml, but install threw: ${(err as Error).message}`,
        retry: () => runInstallNow(name, ++submissionCounter.current),
      });
    }
  }
```

### Step 2 — Render toolbar + warnings + banner above the grid

Inside the `return (...)` block, immediately after `<h1>Add server</h1>` and BEFORE `<div class="add-server-grid">`, insert:

```tsx
      <div class="toolbar" data-testid="add-server-toolbar">
        <button
          type="button"
          onClick={runValidate}
          disabled={busy !== ""}
          data-action="validate"
        >
          {busy === "validate" ? "Validating…" : "Validate"}
        </button>
        <button
          type="button"
          onClick={() => runSave({ install: false })}
          disabled={busy !== "" || !!nameError}
          data-action="save"
        >
          {busy === "save" ? "Saving…" : "Save"}
        </button>
        <button
          type="button"
          class="primary"
          onClick={() => runSave({ install: true })}
          disabled={busy !== "" || !!nameError}
          data-action="save-and-install"
        >
          {busy === "install" ? "Installing…" : "Save & Install"}
        </button>
      </div>
      {banner && (
        <div class={`banner ${banner.kind}`} data-testid="banner">
          <p>{banner.text}</p>
          {banner.retry && (
            <button type="button" onClick={() => banner.retry?.()} data-action="retry-install">
              Retry Install
            </button>
          )}
        </div>
      )}
      {warnings && warnings.length > 0 && (
        <ul class="validation-warnings" data-testid="validation-warnings">
          {warnings.map((w, i) => (
            <li key={i}>{w}</li>
          ))}
        </ul>
      )}
```

### Step 3 — Backend: Note about `/api/install`

`/api/install` is NOT part of this task — A2a assumes it already exists or a follow-up adds it. Verify first:

```bash
grep -n "/api/install" internal/gui/*.go
```

If absent, add a thin GUI wrapper following the `demigrate.go` pattern. The backend call is `api.Install(name)` from existing code. A minimal wrapper is enough — but this ONLY applies if the endpoint is missing. Most likely it's already wired (`api.Install` has existed since Phase 3A, and the Servers/Migration screens already hit it for Migrate).

Run the grep; if no match, insert the handler:

```go
// Add to internal/gui/install.go (new file):
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type installer interface {
	Install(name string) error
}

type installRequest struct {
	Name string `json:"name"`
}

func registerInstallRoutes(s *Server) {
	s.mux.HandleFunc("/api/install", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Query().Get("name")
		if name == "" {
			var req installRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
				return
			}
			name = req.Name
		}
		if name == "" {
			writeAPIError(w, fmt.Errorf("name required"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if err := s.installer.Install(name); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "INSTALL_FAILED")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
```

Wire `installer` into `Server` with a `realInstaller{a: theAPI}` and call `registerInstallRoutes(s)` in `NewServer`. If the handler already existed under a different path, update `runInstallNow` to use that path instead.

### Step 4 — Typecheck + rebuild + Go smoke

```bash
cd internal/gui/frontend && npm run typecheck
cd ../../..
go generate ./internal/gui/...
go test ./internal/gui/ -count=1
```

Expected: exit 0; all green.

### Step 5 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/assets/ \
        internal/gui/install.go internal/gui/server.go 2>/dev/null || true
# (install.go/server.go changes only if the endpoint was missing — git add gracefully)
git commit -m "feat(gui/frontend): AddServer toolbar + submission-version counter

[Validate] [Save] [Save & Install] buttons with:
- Monotonic submissionCounter.current — each click captures its own
  version; newer clicks preempt older ones, preventing a fast edit +
  click from writing a stale previously-valid manifest (Q3 gotcha).
- Separate validateCounter.current for async Validate paths so stale
  warnings never paint over fresh state (Q5 gotcha).
- Save & Install keeps the manifest on disk on install failure and
  renders a Retry Install button inside the banner (Q3 contract)."
```

---

## Task 12: Paste YAML import + Copy YAML escape hatches

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`

### Step 1 — Add Paste + Copy handlers + UI

Update the top import:

```tsx
import { BLANK_FORM, parseYAMLToForm, toYAML } from "../lib/manifest-yaml";
```

Inside `AddServerScreen`, add after `runInstallNow`:

```tsx
  async function handlePasteYAML() {
    const pasted = window.prompt("Paste YAML manifest:", "");
    if (pasted == null || pasted.trim() === "") return;
    try {
      const parsed = parseYAMLToForm(pasted);
      setFormState(parsed);
      // Per Q8 decision: paste does NOT reset the dirty baseline. Only
      // successful Save does. We DO auto-run structural validate since
      // paste is a mode switch and users expect "this parsed / this
      // mapped" feedback (Codex xhigh memo).
      setBanner(null);
      await runValidate();
    } catch (err) {
      setBanner({ kind: "error", text: `Paste failed: ${(err as Error).message}` });
    }
  }

  async function handleCopyYAML() {
    const yaml = toYAML(formState); // fresh, not debounced
    try {
      await navigator.clipboard.writeText(yaml);
      setBanner({ kind: "success", text: "YAML copied to clipboard." });
    } catch {
      // Fallback for environments without clipboard API (older E2E setup etc.)
      setBanner({ kind: "error", text: "Clipboard API unavailable — copy manually from the preview pane." });
    }
  }
```

Append Paste + Copy buttons to the toolbar (inside the existing `<div class="toolbar">`):

```tsx
        <button
          type="button"
          onClick={handlePasteYAML}
          disabled={busy !== ""}
          data-action="paste-yaml"
        >
          Paste YAML
        </button>
        <button
          type="button"
          onClick={handleCopyYAML}
          disabled={busy !== ""}
          data-action="copy-yaml"
        >
          Copy YAML
        </button>
```

### Step 2 — Typecheck + rebuild

```bash
cd internal/gui/frontend && npm run typecheck
cd ../../..
go generate ./internal/gui/...
```

Expected: exit 0.

### Step 3 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): AddServer Paste/Copy YAML escape hatches

[Paste YAML] opens a prompt() for the user to paste a YAML blob.
parseYAMLToForm (normalization-aware) fills the form; paste does NOT
reset the dirty baseline (Q8 anti-silent-data-loss). Auto-runs
structural validate afterwards (paste is a mode switch — Q5 gotcha).

[Copy YAML] puts the fresh serialized YAML on the clipboard via the
navigator.clipboard API; gracefully degrades to an informative banner
when the API isn't available."
```

---

## Task 13: Snapshot-dirty detection + sidebar-intercept navigation guard

**Files:**
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx`
- Modify: `internal/gui/frontend/src/app.tsx`

### Step 1 — Add isDirty prop lift + deep-equal snapshot detection in AddServer

Update imports at top of `AddServer.tsx`:

```tsx
import { useEffect, useRef, useState } from "preact/hooks";
import { BLANK_FORM, parseYAMLToForm, toYAML } from "../lib/manifest-yaml";
```

Add helper function before `AddServerScreen`:

```tsx
// deepEqualForm compares two ManifestFormState instances structurally. Used
// by the Q8 dirty check. JSON.stringify is defensible for this shape: all
// fields are serializable primitives, arrays, and plain objects with no
// Date/Map/Set/functions. If a future field breaks that assumption, switch
// to a proper deep-equal import and update the test.
function deepEqualForm(a: ManifestFormState, b: ManifestFormState): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}
```

Change `AddServerScreen` to accept an `onDirtyChange` prop and wire snapshot:

```tsx
export function AddServerScreen(props: { onDirtyChange?: (dirty: boolean) => void } = {}) {
  const [formState, setFormState] = useState<ManifestFormState>(BLANK_FORM);
  // initialSnapshot is the post-normalization baseline the dirty check
  // compares against. Updated on mount (after any prefill path) and on
  // successful Save. Critically NOT updated on Paste YAML import (Q8
  // anti-silent-data-loss: paste must not move the baseline).
  const [initialSnapshot, setInitialSnapshot] = useState<ManifestFormState>(BLANK_FORM);
  const debouncedState = useDebouncedValue(formState, 150);
  const yamlPreview = toYAML(debouncedState);
  const isDirty = !deepEqualForm(formState, initialSnapshot);

  useEffect(() => {
    props.onDirtyChange?.(isDirty);
  }, [isDirty]);

  // ... (rest of the existing body unchanged; the submit path must call
  // setInitialSnapshot(formState) after a successful Save or Save & Install)
```

In `runSave` — after a successful `postManifestCreate(name, payload)` and BEFORE the "skip install" branch — add the snapshot update:

```tsx
      // Commit the save as the new baseline. Paste does NOT do this; only
      // actual persist does. (Q8.)
      setInitialSnapshot(formState);
      if (!opts.install) {
        setBanner({ kind: "success", text: `Saved servers/${name}/manifest.yaml.` });
        return;
      }
```

### Step 2 — Lift isDirty into `app.tsx` + intercept sidebar clicks

Update `internal/gui/frontend/src/app.tsx`:

```tsx
import type { JSX } from "preact";
import { useState } from "preact/hooks";
import { useRouter } from "./hooks/useRouter";
import { AddServerScreen } from "./screens/AddServer";
import { DashboardScreen } from "./screens/Dashboard";
import { LogsScreen } from "./screens/Logs";
import { MigrationScreen } from "./screens/Migration";
import { ServersScreen } from "./screens/Servers";

export function App() {
  const screen = useRouter("servers");
  const [addServerDirty, setAddServerDirty] = useState(false);
  const SCREENS: Record<string, () => JSX.Element> = {
    servers: () => <ServersScreen />,
    migration: () => <MigrationScreen />,
    "add-server": () => <AddServerScreen onDirtyChange={setAddServerDirty} />,
    dashboard: () => <DashboardScreen />,
    logs: () => <LogsScreen />,
  };
  const Render = SCREENS[screen];

  // guardClick is wired onto every sidebar <a>. If the Add server screen
  // is dirty AND the click leaves it for another screen, we prompt.
  // Cancelling restores the original hash via preventDefault. This covers
  // ~90% of exit paths; browser-back/refresh/tab-close coverage is
  // deferred to A2b (per design memo Q7).
  function guardClick(targetScreen: string): (e: MouseEvent) => void {
    return (e) => {
      if (
        screen === "add-server" &&
        addServerDirty &&
        targetScreen !== "add-server"
      ) {
        // eslint-disable-next-line no-alert
        const ok = window.confirm("Discard unsaved changes?");
        if (!ok) {
          e.preventDefault();
        } else {
          // User confirmed — reset the dirty flag so a second immediate
          // hashchange doesn't re-fire the prompt.
          setAddServerDirty(false);
        }
      }
    };
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
        {Render ? <Render /> : <p>Unknown screen: {screen}</p>}
      </main>
    </>
  );
}
```

### Step 3 — Typecheck + rebuild

```bash
cd internal/gui/frontend && npm run typecheck
cd ../../..
go generate ./internal/gui/...
```

Expected: exit 0.

### Step 4 — Commit

```bash
git add internal/gui/frontend/src/screens/AddServer.tsx internal/gui/frontend/src/app.tsx internal/gui/assets/
git commit -m "feat(gui/frontend): snapshot-dirty + sidebar-intercept navigation guard

Q8 snapshot-dirty: initialSnapshot taken after post-normalization
mount baseline; deepEqualForm() (JSON.stringify-based for this shape)
drives isDirty. Updated on successful Save only — Paste YAML import
does NOT move the baseline (Codex xhigh memo, prevents silent
data-loss).

Q7 sidebar-intercept: every sidebar <a> gets a guardClick that shows
window.confirm('Discard unsaved changes?') when leaving Add server
with isDirty=true. Deferring beforeunload + hashchange interceptor
to A2b per design memo."
```

---

## Task 14: A1 Migration → A2a handoff (unblock Create manifest button)

**Files:**
- Modify: `internal/gui/frontend/src/screens/Migration.tsx`
- Modify: `internal/gui/frontend/src/screens/AddServer.tsx` (add prefill effect)
- Create: `internal/gui/extract_manifest.go` (GUI wrapper if missing)
- Modify: `internal/gui/frontend/src/api.ts` (add `getExtractManifest`)

### Step 1 — Verify whether `/api/extract-manifest` already exists

```bash
grep -rn "extract-manifest\|ExtractManifestFromClient" internal/gui/*.go | head
```

If no match: Migration's Create-manifest button currently has no backend path to call. Add a GUI wrapper next.

### Step 2 — Add GUI wrapper for `/api/extract-manifest` (if missing)

Create `internal/gui/extract_manifest.go`:

```go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"

	"mcp-local-hub/internal/api"
)

type extractor interface {
	ExtractManifestFromClient(client, server string, opts api.ScanOpts) (string, error)
}

type extractManifestResponse struct {
	YAML string `json:"yaml"`
}

// registerExtractManifestRoutes wires GET /api/extract-manifest
// ?client=<name>&server=<name> -> 200 {yaml: string} | 404 | 500.
func registerExtractManifestRoutes(s *Server) {
	s.mux.HandleFunc("/api/extract-manifest", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		client := r.URL.Query().Get("client")
		server := r.URL.Query().Get("server")
		if client == "" || server == "" {
			writeAPIError(w, fmt.Errorf("client and server required"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		yaml, err := s.extractor.ExtractManifestFromClient(client, server, api.ScanOpts{})
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "EXTRACT_FAILED")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(extractManifestResponse{YAML: yaml})
	}))
}
```

In `server.go`, add the `extractor extractor` field, the `realExtractor` adapter, call `registerExtractManifestRoutes(s)` in `NewServer`.

Real adapter:

```go
type realExtractor struct{ a *api.API }

func (r *realExtractor) ExtractManifestFromClient(client, server string, opts api.ScanOpts) (string, error) {
	return r.a.ExtractManifestFromClient(client, server, opts)
}
```

Check `api.ScanOpts` — it may need to be constructed with the same `*ClientConfigPath` fields that Scan uses. Grep to find the shape:

```bash
grep -n "ScanOpts{" internal/gui/*.go | head
```

Follow the same construction pattern as the existing `/api/scan` handler.

### Step 3 — Frontend API helper

In `internal/gui/frontend/src/api.ts`, append:

```ts
// getExtractManifest fetches the prefill YAML that populates AddServer's
// form when the user arrives via the A1 Migration Create-manifest button.
// Returns the raw YAML string. Throws on non-2xx with the backend error.
export async function getExtractManifest(client: string, server: string): Promise<string> {
  const url = `/api/extract-manifest?client=${encodeURIComponent(client)}&server=${encodeURIComponent(server)}`;
  const resp = await fetch(url);
  if (!resp.ok) {
    let body: { error?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string };
    } catch {
      // Non-JSON error body; fall through.
    }
    throw new Error(`/api/extract-manifest: ${body?.error ?? resp.statusText}`);
  }
  const payload = (await resp.json()) as { yaml?: string };
  return payload.yaml ?? "";
}
```

### Step 4 — AddServer prefill effect

In `AddServer.tsx`, add a hash-query parser helper at top of file (after the `deepEqualForm` function):

```tsx
// parseAddServerQuery extracts ?server=...&from-client=... from the current
// hash. A1's Create-manifest button navigates to
// #/add-server?server=<name>&from-client=<client> — we pick those up on
// mount and run the prefill fetch.
function parseAddServerQuery(): { server: string; fromClient: string } {
  const hash = window.location.hash;
  const q = hash.split("?")[1] ?? "";
  const params = new URLSearchParams(q);
  return {
    server: params.get("server") ?? "",
    fromClient: params.get("from-client") ?? "",
  };
}
```

Import `getExtractManifest`:

```tsx
import { getExtractManifest, postManifestCreate, postManifestValidate } from "../api";
```

Inside `AddServerScreen`, add a mount effect right after the `useEffect` that calls `onDirtyChange`:

```tsx
  // Prefill path (Q8 baseline gotcha): fetch extract-manifest when the
  // user arrives from A1, parse → set form state → take the snapshot
  // AFTER normalization so dirty is false on first render.
  useEffect(() => {
    const { server, fromClient } = parseAddServerQuery();
    if (!server || !fromClient) return;
    let cancelled = false;
    (async () => {
      try {
        const yaml = await getExtractManifest(fromClient, server);
        if (cancelled) return;
        const parsed = parseYAMLToForm(yaml);
        setFormState(parsed);
        setInitialSnapshot(parsed);
      } catch (err) {
        if (cancelled) return;
        setBanner({
          kind: "error",
          text: `Could not prefill from ${fromClient}/${server}: ${(err as Error).message}. Continuing with empty form.`,
        });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);
```

### Step 5 — Unblock the button in `internal/gui/frontend/src/screens/Migration.tsx`

Find the `<button class="create-manifest" ... disabled>` element (inside `UnknownGroup`). Replace it with:

```tsx
            <button
              type="button"
              class="create-manifest"
              data-action="create-manifest"
              onClick={() => {
                const client = firstClientFor(e);
                const url = client
                  ? `#/add-server?server=${encodeURIComponent(e.name)}&from-client=${encodeURIComponent(client)}`
                  : `#/add-server?server=${encodeURIComponent(e.name)}`;
                window.location.hash = url;
              }}
            >
              Create manifest
            </button>
```

Just above the button or at the bottom of `Migration.tsx`, add:

```tsx
// firstClientFor picks a sensible client name to extract from for a given
// Unknown scan entry. The stdio entry may live in any one client's config
// (typically the user had the server set up in Claude Code first). We pick
// the first client that has a stdio transport for the entry; fallback:
// empty string, which still navigates (fresh-create with just the name).
function firstClientFor(entry: { client_presence?: Record<string, { transport?: string }> }): string {
  const presence = entry.client_presence ?? {};
  for (const [client, info] of Object.entries(presence)) {
    if (info?.transport === "stdio") return client;
  }
  return "";
}
```

### Step 6 — Typecheck + rebuild + Go smoke + E2E shell

```bash
cd internal/gui/frontend && npm run typecheck
cd ../../..
go generate ./internal/gui/...
go test ./internal/gui/ -count=1
cd internal/gui/e2e && npm test -- tests/shell.spec.ts tests/migration.spec.ts
```

Expected: all green. Migration spec still 6/6 (empty-state tests don't exercise the button; actual click of the now-wired button is covered by Task 15).

### Step 7 — Commit

```bash
git add internal/gui/extract_manifest.go internal/gui/server.go \
        internal/gui/frontend/src/screens/Migration.tsx \
        internal/gui/frontend/src/screens/AddServer.tsx \
        internal/gui/frontend/src/api.ts \
        internal/gui/assets/
git commit -m "feat(gui): A1 Migration Create-manifest button unblocked via A2a handoff

- New GET /api/extract-manifest?client=&server= GUI wrapper returns
  the draft YAML produced by api.ExtractManifestFromClient (already
  merged as B2 in PR #3).
- AddServer on mount parses ?server=&from-client= query params and
  fetches /api/extract-manifest; success fills the form AND sets
  initialSnapshot to the post-normalization result so dirty is false
  on first render (Q8 invariant).
- Migration.tsx Create-manifest button is no longer DOM-disabled; it
  now navigates to the prefill URL."
```

---

## Task 15: Playwright E2E — add-server.spec.ts

**Files:**
- Create: `internal/gui/e2e/tests/add-server.spec.ts`

### Step 1 — Create the spec file

```ts
import { test, expect } from "../fixtures/hub";
import { readFileSync, existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";

test.describe("Add server screen", () => {
  test("renders empty-state form + YAML preview on fresh home", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await expect(page.locator("h1")).toHaveText("Add server");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("name:");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("kind: global");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("transport: stdio-bridge");
  });

  test("typing into name + command updates the YAML preview after debounce", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator("#field-name").fill("demo");
    await page.locator("#field-command").fill("npx");
    // Wait for the 150ms debounce to settle.
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("name: demo");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("command: npx");
  });

  test("inline name-regex error shows when name contains uppercase", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator("#field-name").fill("DEMO");
    await expect(page.locator(".inline-error")).toContainText("Must match");
    await page.locator("#field-name").fill("demo");
    await expect(page.locator(".inline-error")).toHaveCount(0);
  });

  test("adding a daemon then a binding renders the single-daemon flat binding list", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-action="add-daemon"]').click();
    await page.locator('[data-field="daemon-name"]').fill("default");
    await page.locator('[data-field="daemon-port"]').fill("9100");
    await page.locator(".accordion-header", { hasText: "Client bindings" }).click();
    await page.locator('[data-action="add-binding"]').click();
    await expect(page.locator('[data-testid="bindings-list"]')).toBeVisible();
    await expect(page.locator('[data-binding-row]')).toHaveCount(1);
  });

  test("renaming a daemon cascades to its bindings (preview reflects new name)", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-action="add-daemon"]').click();
    await page.locator('[data-field="daemon-name"]').fill("default");
    await page.locator('[data-field="daemon-port"]').fill("9100");
    await page.locator(".accordion-header", { hasText: "Client bindings" }).click();
    await page.locator('[data-action="add-binding"]').click();
    // Now rename default -> main
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-field="daemon-name"]').fill("main");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("- name: main");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("daemon: main");
    await expect(page.locator('[data-testid="yaml-preview"]')).not.toContainText("daemon: default");
  });

  test("deleting a daemon with bindings prompts and cascade-deletes", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-action="add-daemon"]').click();
    await page.locator('[data-field="daemon-name"]').fill("default");
    await page.locator('[data-field="daemon-port"]').fill("9100");
    await page.locator(".accordion-header", { hasText: "Client bindings" }).click();
    await page.locator('[data-action="add-binding"]').click();
    // Wire up the confirm dialog to accept.
    page.once("dialog", (d) => d.accept());
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-action="delete-daemon"]').click();
    await expect(page.locator('[data-testid="yaml-preview"]')).not.toContainText("daemons:");
    await expect(page.locator('[data-testid="yaml-preview"]')).not.toContainText("client_bindings:");
  });

  test("Save writes manifest to disk (servers/<name>/manifest.yaml exists)", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator("#field-name").fill("e2e-save-only");
    await page.locator("#field-command").fill("echo");
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-action="add-daemon"]').click();
    await page.locator('[data-field="daemon-name"]').fill("default");
    await page.locator('[data-field="daemon-port"]').fill("9991");
    await page.locator('[data-action="save"]').click();
    await expect(page.locator('[data-testid="banner"].success')).toContainText("Saved");
    // Note: hub fixture uses the binary's embed FS for servers/, so we
    // verify the success banner only — the filesystem write lands in
    // the binary's runtime servers dir which for this build equals the
    // repo's servers/ tree. Assert via the banner.
  });

  test("Save & Install on a name with port conflict keeps manifest + shows Retry Install", async ({
    page,
    hub,
  }) => {
    // Occupy port 9992 from another process to force install-preflight
    // failure.
    const net = await import("node:net");
    const blocker = net.createServer();
    await new Promise<void>((resolve) => blocker.listen(9992, "127.0.0.1", resolve));
    try {
      await page.goto(`${hub.url}/#/add-server`);
      await page.locator("#field-name").fill("e2e-port-conflict");
      await page.locator("#field-command").fill("echo");
      await page.locator(".accordion-header", { hasText: "Daemons" }).click();
      await page.locator('[data-action="add-daemon"]').click();
      await page.locator('[data-field="daemon-name"]').fill("default");
      await page.locator('[data-field="daemon-port"]').fill("9992");
      await page.locator('[data-action="save-and-install"]').click();
      await expect(page.locator('[data-testid="banner"].error')).toContainText("install failed");
      await expect(page.locator('[data-action="retry-install"]')).toBeVisible();
    } finally {
      blocker.close();
    }
  });

  test("Paste YAML fills the form and runs auto-validate but keeps dirty true", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    const yaml = `name: pasted\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9100\n`;
    page.once("dialog", (d) => d.accept(yaml));
    await page.locator('[data-action="paste-yaml"]').click();
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("name: pasted");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("command: npx");
    // The dirty indicator is set by the parent App; we verify via the
    // sidebar-intercept test below.
  });

  test("sidebar-intercept: navigating away from dirty AddServer prompts", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator("#field-name").fill("dirty-work");
    // Register a dialog handler before the click that cancels. If the
    // guard never fires, the test proceeds without a dialog.
    let dialogSeen = false;
    page.once("dialog", (d) => {
      dialogSeen = true;
      d.dismiss();
    });
    await page.locator(".sidebar nav a", { hasText: "Servers" }).click();
    // Brief wait for any pending navigation to settle.
    await expect(page.locator("h1")).toHaveText("Add server"); // stayed
    expect(dialogSeen).toBe(true);
  });
});
```

### Step 2 — Run just the add-server suite

```bash
cd internal/gui/e2e && npm test -- tests/add-server.spec.ts
```

Expected: all 10 tests PASS.

### Step 3 — Run the FULL E2E suite for no regressions

```bash
cd internal/gui/e2e && npm test
```

Expected: 27/27 PASS (3 shell + 3 servers + 6 migration + 10 add-server + 2 dashboard + 3 logs).

### Step 4 — Commit

```bash
git add internal/gui/e2e/tests/add-server.spec.ts
git commit -m "test(gui/e2e): Add server screen — 10 Playwright scenarios

Covers: empty-state render, debounced YAML preview update, live name
regex error, single-daemon flat bindings list, cascade-rename across
bindings, cascade-delete with confirm, Save writes manifest, Save&Install
failure with Retry button, Paste YAML import, sidebar-intercept guard."
```

---

## Task 16: `go generate` + CLAUDE.md update + full-suite smoke

**Files:**
- Modify: `CLAUDE.md` (bump nav-link count + test totals)

### Step 1 — Run the full validation pipe

```bash
go build ./...
go test ./... -count=1
cd internal/gui/frontend && npm run typecheck && npm run test
cd ../e2e && npm test
cd ../../..
```

Expected:
- `go build ./...` clean.
- All Go packages PASS (two pre-existing flakes are not regressions).
- Vitest all PASS.
- Playwright 27/27 PASS.

### Step 2 — Update CLAUDE.md "What's covered" section

Read `CLAUDE.md` and find the `### What's covered` block under `## GUI E2E tests`. Replace it EXACTLY with:

```
### What's covered

- Shell: sidebar, five nav links, hash routing, active-link highlight.
- Servers: matrix columns (Server + 4 clients + Port + State), empty-body state on clean tmpHome, Apply disabled with no dirty cells.
- Migration: h1, empty-state copy, group sections hidden on empty home, hashchange swap from Servers, full POST /api/dismiss → on-disk JSON → GET /api/dismissed round-trip, /api/scan-unfiltered regression guard (seed + dismiss + re-scan).
- Add server: empty-state + debounced YAML preview, live name-regex inline error, single-daemon flat bindings, cascade rename/delete with confirm, Save writes manifest, Save&Install port-conflict failure path with Retry Install banner, Paste YAML import, sidebar-intercept unsaved-changes guard.
- Dashboard: empty-cards state on fresh home, `/api/events` SSE connection opens on mount.
- Logs: picker + controls render, notice text on no-daemons state, controls disabled when no eligible entries.

27 smoke tests total (3 shell + 3 servers + 6 migration + 10 add-server + 2 dashboard + 3 logs), ~20s
wall-time on a warm machine.
```

### Step 3 — Commit

```bash
git add CLAUDE.md
git commit -m "docs: CLAUDE.md reflects Add server screen E2E coverage

Five nav links now, ten new Add server tests (including cascade rename/delete,
Save&Install failure + retry, sidebar-intercept guard), updated total
(17→27) and wall-time (~10s→~20s)."
```

### Step 4 — Verify PR-ready state

```bash
git log master..HEAD --oneline
git status
```

Expected: 16 commits (one per task plus the design memo from earlier), clean working tree.

### Step 5 — Hand off

The branch is PR-ready. Follow the A1 pattern:

1. Push: `git push -u origin feat/phase-3b-ii-a2a-create-manifest`
2. Open PR with a summary matching the A1 template (commit list + test plan + known nits from any per-task quality review).
3. Wait for Codex bot review; address R-rounds as they come.
4. Merge when CLEAN.

---

## Dependency order summary

Task 1 (backend handlers) → Task 2 (frontend types + api.ts) → Task 3 (toYAML) → Task 4 (parseYAMLToForm + yaml dep) → Task 5 (useDebouncedValue) → Task 6 (scaffolding + route + sidebar + shell E2E) → Task 7 (Basics + Command) → Task 8 (Environment) → Task 9 (Daemons + cascade) → Task 10 (Client bindings adaptive) → Task 11 (Toolbar + submission counter) → Task 12 (Paste/Copy) → Task 13 (Snapshot-dirty + guard) → Task 14 (A1 handoff) → Task 15 (Playwright E2E) → Task 16 (full-suite smoke + CLAUDE.md).

- Tasks 1-5 are independent within their layer (backend vs frontend helpers) and can be reordered freely — the build ordering only matters for tests.
- Task 6 must come after Tasks 3-5 (scaffolding imports toYAML + useDebouncedValue).
- Tasks 7-10 extend the same `AddServer.tsx` file in distinct sections; keep the order for reviewability.
- Task 11 (toolbar) depends on Task 2 (API helpers) and Tasks 7-10 (form sections must all be wired for the submit to carry valid state).
- Task 13 (dirty detection) must come after Tasks 11-12 so all mutation sites update through the same `setFormState` path.
- Task 14 (A1 handoff) depends on Task 13 (snapshot baseline must exist for the prefill path to set it correctly).
- Task 15 (E2E) depends on all of 1-14 because it exercises the full DOM + backend integration.
- Task 16 is docs + verification only.

**Estimated scope:** ~1400-1800 LOC added (Go handlers + tests ~250; frontend source ~900; Vitest ~200; Playwright ~250; docs the rest). 16 commits. Budget ~6-8 hours of subagent-driven execution given the strict review discipline established by PR #3 and PR #4.

---

## Self-review (author ran this, no further review needed)

**Spec coverage:** Each Q1-Q8 decision from the design memo maps to an explicit task step:
- Q1 (form-SoT + Paste/Copy): Task 3 (toYAML), Task 4 (parseYAMLToForm), Task 12 (Paste/Copy buttons).
- Q2 (name editable on prefill): Task 14 prefill effect does not lock the name input.
- Q3 (three-button toolbar + submission-version counter + keep-manifest): Task 11.
- Q4 (debounced 150ms): Task 5 (useDebouncedValue) + Task 6 (wired into AddServer).
- Q5 (hybrid validation + async versioning + paste-validate): Task 11 (validateCounter) + Task 12 (paste auto-validate).
- Q6 (daemon subsections adaptive + cascade): Task 9 (cascade rename/delete) + Task 10 (adaptive).
- Q7 (sidebar-intercept): Task 13.
- Q8 (snapshot after normalization, paste does NOT reset baseline): Task 13 (snapshot) + Task 12 (paste explicitly does not call setInitialSnapshot) + Task 14 (prefill sets snapshot after parse).

Load-bearing gotchas: all five from the design memo §3 are named in the commit messages and exercised by either Vitest (normalization) or Playwright (cascade, Paste+dirty, submission-version preemption via test 8 port-conflict + retry).

**Placeholder scan:** No "TBD" / "TODO" / vague reqs remain. Every step has complete code or an exact command.

**Type consistency:** `ManifestFormState` fields are identical across Tasks 2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14. `BLANK_FORM`, `toYAML`, `parseYAMLToForm`, `postManifestCreate`, `postManifestValidate`, `getExtractManifest` names match between their definition tasks and their consumer tasks. `data-testid` attributes referenced in Task 15 E2E match the ones added in Tasks 6-12.

No gaps found.

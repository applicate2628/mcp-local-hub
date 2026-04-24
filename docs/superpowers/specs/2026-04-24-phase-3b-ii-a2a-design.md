# Phase 3B-II A2a — Create-flow Add/Server Design

> **Scope:** Create-only Add Server form. Edit-mode hardening and the Advanced section are deferred to **A2b** (separate cycle).
>
> **Status:** Brainstorm complete. Design memo reviewed by Codex (gpt-5.5, reasoning xhigh). Ready for `writing-plans` handoff.

---

## 1. Context

A2a ships the GUI flow for creating a new MCP server manifest. The entry points are:

- **Sidebar link** `#/add-server` — fresh-create (empty form).
- **A1 Migration → Unknown group → Create manifest button** → `#/add-server?server=<name>&from-client=<client>` — prefill flow. This unblocks the button that was DOM-disabled in PR #4 with tooltip "Available after A2 (Add/Edit manifest) ships".

The screen renders an accordion form (Basics → Command → Environment → Daemons → Client bindings) with a read-only YAML preview pane. On submit, the form serializes to YAML and calls the already-shipped `api.ManifestCreate(name, yaml)` and (optionally) `api.Install(name)` endpoints.

Manifest shape (example from `servers/memory/manifest.yaml`):

```yaml
name: memory
kind: global
transport: stdio-bridge
command: npx
base_args: ["-y", "@modelcontextprotocol/server-memory"]
env:
  MEMORY_FILE_PATH: "${HOME}/.local/share/mcp-memory/memory.jsonl"
daemons:
  - name: default
    port: 9123
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
# ... (three more bindings for codex-cli, gemini-cli, antigravity)
weekly_refresh: false
```

Backend dependencies (all already merged):

- `api.ManifestCreate(name, yaml string) error` — fails if manifest dir exists.
- `api.ManifestValidate(yaml string) []string` — returns warnings; empty means valid.
- `api.ExtractManifestFromClient(client, server string, opts) (yaml string, error)` — prefill pipeline from A1.
- `api.Install(name)` via `BuildPlan` + `Preflight` — port probe + binary check + scheduled task registration.

Frontend constraints:

- Embedded bundle `internal/gui/assets/app.js` target <100 KB minified (shipped inside the Go binary).
- Vite 8 + TypeScript 5 + Preact 10. Vitest 4 unit. Playwright E2E.
- No YAML editor component shipping (Monaco/CodeMirror rejected for bundle cost — see Q1).

---

## 2. Design decisions (8 of 8 resolved)

All decisions validated by Codex (`gpt-5.5`, `reasoning_effort=xhigh`) against an earlier lead-formulated set. Reversals and additions from the Codex pass are called out per-row.

### Q1 — Source-of-truth model

**Decision: (a+) Form-as-source-of-truth + read-only YAML preview + Paste/Copy YAML escape hatches.**

- Frontend state is a structured `ManifestFormState` object. A `toYAML(state)` helper (~30-50 LOC) renders the preview on each change.
- YAML preview pane is `<pre>` — not editable.
- **"Paste YAML to import"** button: user pastes a YAML blob, we parse once via `parseYAMLToForm(yaml)`, replace form state. After paste, the form remains SoT. Does NOT require importing a YAML editor dependency.
- **"Copy YAML"** button: copies the preview to clipboard.
- Advanced "edit raw YAML in a modal" is an A2b nice-to-have; not shipping in A2a.

Rejected alternatives:

- **(b) YAML-as-SoT with Monaco/CodeMirror editor** — rejected. Shipping a 200-500 KB editor doubles the embedded bundle for a 5% user story. Power users already have `mcphub manifest validate` and direct `servers/<name>/manifest.yaml` edits via CLI.
- **(c) Dual-editable with shared AST** — rejected. Codex flagged drift/merge as a landmine.

### Q2 — Name field editable when prefilled

**Decision: name field is ALWAYS editable, including when prefilled from A1.**

When user arrives via `#/add-server?server=foo&from-client=claude-code`, the name field pre-populates to `foo` but remains unlocked. User can rename to anything that passes the regex `^[a-z0-9][a-z0-9._-]*$`.

Consequence: if user renames, the original stdio entry in the client's config still references the old name. Migration's Unknown group will not match the new manifest. This is an explicit user choice, not a bug. Backend `ManifestCreate` also catches "name already exists" independently.

### Q3 — Save/install transaction + submission-version counter

**Decision: Three-button toolbar + keep-manifest-on-failure + monotonic submission counter.**

Toolbar layout: `[Validate] [Save] [Save & Install]`.

- **Validate** — runs `api.ManifestValidate(yaml)`, shows warnings in a status banner.
- **Save** — runs validation, then `api.ManifestCreate(name, yaml)`. Does NOT install. Useful for "draft this and install later" flow (e.g., port conflict being resolved separately).
- **Save & Install** — runs Save, then `api.Install(name)`. If Install fails (port taken, binary missing), **the manifest stays on disk**. Banner shows "Saved, install failed: \<reason\>". A **Retry Install** button appears on the banner. User can also edit the form and re-click Save & Install — the second Save is a no-op (manifest already exists), then Install runs afresh.

**Why keep-manifest and not rollback**: install failures are typically runtime-recoverable (stop the process on the conflicting port, install `npx`, etc.). Rollback would punish the user for backend conditions they can fix. The manifest on disk is easily removable via `mcphub manifest delete <name>` if the user genuinely changed their mind.

**Submission-version counter (Codex xhigh load-bearing gotcha):**

All three buttons serialize the form inline at click time via a monotonic counter. They do NOT read from the debounced preview cache:

```ts
async function submit(action: "save" | "install") {
  const submissionVersion = ++submissionCounter.current;
  const payload = serialize(formState);  // fresh, synchronous
  const warnings = await api.validate(payload);
  if (submissionVersion !== submissionCounter.current) return;  // preempted by newer submit
  if (warnings.length > 0) { showErrors(warnings); return; }
  await api.create(name, payload);  // same exact payload
  if (action === "install") await api.install(name);
  // ... set baseline, handle failures
}
```

Without this, a fast edit followed by immediate click could save an older valid manifest while the preview still appears "close enough" to miss the stale state.

### Q4 — YAML preview cadence

**Decision: Debounced 150 ms re-render, NOT live-every-keystroke.**

- The cost that matters is not `toYAML` execution (~microseconds) but preview scroll/caret stability and cross-tree re-render churn on a ~40-field form.
- 150 ms is below the perceived-lag threshold; typing feels instant but the preview settles cleanly between keystrokes.
- Implemented via `useEffect` + `setTimeout` watching `formState`, or the `useDebouncedValue` hook pattern.

Initial lead recommendation was "live" based on `toYAML` being cheap; Codex xhigh correctly pointed out preview-UX concerns that dominate the decision.

### Q5 — Validation timing (hybrid) + submit-path uses fresh snapshot

**Decision: Hybrid multi-tier validation.**

| Tier | What | When | Where |
|---|---|---|---|
| **Regex / format** | `name` matches `^[a-z0-9][a-z0-9._-]*$`, `port` in 1-65535, required fields present | Live, on every keystroke | Client-side, synchronous |
| **Structural** | `api.ManifestValidate(yaml)` — parses YAML, checks required fields + enums + declared-vs-used daemon names | Explicit `[Validate]` button + auto-on-Save / Save&Install + auto-after Paste YAML import | Backend, HTTP |
| **Install preflight** | Port probe, binary existence, scheduled-task registration | Only on `[Save & Install]` | Backend, HTTP |

**Version every async validate request (stale-response handling):**

```ts
async function validateDebounced() {
  const reqId = ++validateCounter.current;
  const result = await api.validate(serialize(formState));
  if (reqId !== validateCounter.current) return;  // stale; newer request in flight
  setValidationWarnings(result);
}
```

Without versioning, slow network + fast typing = warnings from older state painted over newer state.

**Paste YAML import auto-runs structural validate** (paste is a mode switch — user expects immediate "this parsed / this failed" feedback).

**Submit-path uses fresh synchronous snapshot** (see Q3 submission-version counter) — debounced validate cache is for preview warnings only, never for submit.

### Q6 — Client-bindings UI shape

**Decision: Daemon subsections with adaptive layout + cascade rename/delete with loud validation.**

Layout rules:

| Daemon count | Layout |
|---|---|
| 1 | Flat binding list: `[client select][url_path input][delete]` rows + `[+ Add binding]` button. No accordion chrome. |
| 2-3 | Accordion subsection per daemon, each with the flat list above scoped to that daemon's bindings. Subsection header shows daemon name + port. |

The manifest's data ownership is `daemon → bindings`; the UI mirrors it. Matrix (rows=clients, cols=daemons) was rejected because `url_path` makes each cell heavy. Flat unified table was rejected because it allows selecting the wrong daemon or creating duplicate `(client, daemon)` rows.

**Daemon rename / delete — cascade-with-loud-validation (A2a):**

- On daemon rename: find all bindings with `binding.daemon === oldName`, update to `newName`. No user confirm needed.
- On daemon delete: if any bindings reference it, show `confirm("Delete daemon 'X' and its N client bindings?")`. If user agrees, cascade-delete both.
- Post-save: `api.ManifestValidate(yaml)` catches manually-introduced orphan bindings (user hand-edited `binding.daemon` to a non-existent daemon name) and blocks Save with a visible warning.

**Internal-ID approach deferred to A2b** where edit-mode rename is genuinely frequent. A2a create-flow sees rare renames (user usually settles on a name before committing), so cascade-with-validation suffices without the extra abstraction layer.

### Q7 — Unsaved-changes navigation guard

**Decision: Sidebar-intercept only for A2a. `beforeunload` and browser-back interceptor deferred to A2b.**

- `isDirty` from Q8 snapshot-dirty detection is surfaced to `app.tsx` level (prop-drilled or via lightweight Preact context).
- Sidebar nav link `onClick`:
  ```tsx
  <a href="#/servers" onClick={(e) => {
    if (appIsDirty) {
      if (!confirm("Discard unsaved changes?")) { e.preventDefault(); return; }
    }
  }}>Servers</a>
  ```
- Sidebar covers ~90% of exit paths. The remaining paths (tab close, refresh, browser back) are genuine edge cases in a scaffolded-form flow. A2b adds `window.beforeunload` + `hashchange` interceptor as polish.

Rejected `window.beforeunload` in A2a because it complicates Playwright E2E dialog handling and fires on legitimate refresh actions.

### Q8 — Snapshot-dirty detection, paste does NOT reset baseline

**Decision: `deepEqual(currentForm, initialSnapshot)`-based dirty flag. Baseline updates ONLY on mount and on successful Save.**

- `initialSnapshot` is set **after** `parseYAMLToForm` normalization runs (so defaults, empty-to-null conversions, and array/map ordering are already applied — otherwise the form would appear "dirty" on first render).
- `initialSnapshot` updates in exactly two places:
  1. On mount, after prefill (B2 extract for `#/add-server?...`) or empty defaults (fresh create) finishes.
  2. After a successful `Save` or `Save & Install` response (204 or equivalent).
- Paste YAML import does **NOT** reset the baseline (Codex xhigh reversal of the lead's initial suggestion).

**Silent-data-loss scenario that the reversal prevents:**

1. User opens fresh `#/add-server`, baseline = empty form, dirty = false.
2. User types name "serena", command "uvx", 4 bindings (2 minutes of work). dirty = true.
3. User remembers colleague has a ready YAML, pastes it. If Paste reset baseline, dirty would flip to false.
4. User navigates to Servers to check something. If dirty=false, navigation guard silently allows it.
5. User returns — **form is empty, all work lost** (including the paste).

With the correct logic, Paste updates `formState` + runs auto-validate, but `initialSnapshot` stays pointed at "last persisted or mount baseline". dirty stays true, guard fires.

Optional A2b feature: explicit "Accept as baseline" button that resets `initialSnapshot` to current form state — an opt-in commit, not an implicit side effect of paste.

---

## 3. Cross-cutting load-bearing gotchas

Summarized from Codex xhigh memos across all 8 questions:

1. **Submission-version counter (Q3, Q5)** — monotonic counter ensures Save / Save & Install serialize and validate the EXACT payload they submit, not a debounced preview cache. Without this, fast-edit + quick-click can save an older valid manifest.

2. **Async validate request versioning (Q5)** — same pattern as submission counter but for the `[Validate]` button and auto-paste-validate. Debounced async requests can return out-of-order; only the latest should paint the UI.

3. **Snapshot baseline after normalization (Q8)** — initial snapshot must be taken AFTER `parseYAMLToForm` applies all defaults, null-to-empty coercions, and array/map ordering normalizations. Otherwise first render shows false-dirty immediately.

4. **Paste does not commit (Q8)** — Paste YAML import updates form state and runs structural validate, but does NOT move the dirty baseline. Only successful Save moves the baseline.

5. **Daemon rename cascade + loud validation (Q6)** — on every rename/delete, cascade-update all referencing bindings in the form state. Post-Save `ManifestValidate` catches hand-edited orphans. Internal-ID abstraction is an A2b concern.

6. **Prefill error path (Q5)** — if `/api/extract-manifest?server=...&client=...` returns an error (server name not found, client-config missing), A2a renders the fresh-create form with an error banner "Could not prefill: \<reason\>. Continuing with empty form." instead of blocking the screen.

---

## 4. Scope boundaries

### In A2a

- Create-only Add Server screen at `#/add-server` and `#/add-server?server=&from-client=`.
- Accordion form: Basics (name, kind), Command (transport, command, base_args), Environment (env key-value rows), Daemons (name + port rows), Client bindings (adaptive per-daemon).
- Read-only YAML preview, debounced 150 ms.
- Paste YAML import + Copy YAML actions.
- `[Validate] [Save] [Save & Install]` toolbar with submission-version counter.
- Hybrid validation: live regex, on-demand structural, install-preflight on Save & Install.
- Snapshot-dirty detection with paste-safe baseline.
- Sidebar-intercept unsaved-changes guard.
- Playwright E2E: empty-state, validate roundtrip, Save-only success path, Save&Install keep-manifest-on-failure banner, sidebar navigation guard.
- A1 Migration's Create-manifest button unblocks: navigates to `#/add-server?server=...&from-client=...`, removes `disabled` + tooltip.

### Deferred to A2b

- Edit mode (`#/edit-server/<name>` or similar).
- Internal-ID-based daemon rename (cascade with stable IDs, avoiding name-reference fragility).
- Advanced section (uncommon manifest fields, manual raw-YAML modal edit).
- `window.beforeunload` and browser-back/forward interceptors.
- "Accept as baseline" explicit action after Paste.
- Multi-daemon matrix view if it becomes frequent in practice.

### Out of scope entirely (not Phase 3B-II)

- Secrets vault integration in Environment section (that's A3).
- JSON Schema inline auto-completion / field suggestions (per spec §2.2 non-goal).
- Cross-machine manifest sync.

---

## 5. Non-goals reiterated

- No YAML editor component. Power users edit `servers/<name>/manifest.yaml` directly and run `mcphub manifest validate` from CLI.
- No dual-editable form+YAML. The form is authoritative during the session; paste-import is a one-shot replace.
- No automatic "save draft to localStorage" recovery. If the user loses work via browser close (before A2b adds `beforeunload`), that's the expected web-app behavior.

---

## 6. Handoff contract to writing-plans

The plan should produce an implementation broken into small commits with TDD discipline. Suggested scope structure (plan may refine):

1. Backend wrapper — `/api/manifest/create` and `/api/manifest/validate` thin GUI handlers (~40 LOC each, pattern matches existing `demigrate.go` / `dismiss.go`).
2. Frontend types — `ManifestFormState`, `ValidationWarning`, helpers' signatures in `types.ts`.
3. `toYAML(state)` helper + Vitest.
4. `parseYAMLToForm(yaml)` helper + Vitest (normalization included).
5. `useDebouncedValue` or equivalent hook (if not already present).
6. `AddServer.tsx` scaffolding — accordion layout, state management skeleton, routing, sidebar link.
7. Basics / Command / Environment sections.
8. Daemons + Client-bindings subsections with cascade rename/delete.
9. Toolbar: Validate, Save, Save & Install with submission-version counter.
10. Paste/Copy YAML escape hatches.
11. Snapshot-dirty + sidebar-intercept guard.
12. A1 Migration → A2a handoff (unblock Create manifest button).
13. Playwright E2E suite.
14. `CLAUDE.md` coverage update + full-suite smoke.

Each step is a commit with TDD (red → green → commit), matching the A1 cadence. Estimated 12-15 commits, ~1200-1800 LOC TS + ~50 LOC Go backend wrappers + ~200 LOC Playwright.

---

## 7. Open questions for plan authoring

None critical. Items the plan author should decide during decomposition (not design-level questions):

- Exact JSX structure for adaptive daemon-subsections (1-daemon flat vs 2-3-daemons accordion) — one component with `if (daemons.length === 1)` branch, or two components selected by parent.
- Whether `parseYAMLToForm` lives in `lib/manifest-yaml.ts` (one file) or split into `toYAML.ts` + `parseYAMLToForm.ts` (two files). Plan decides per test isolation convenience.
- CSS tokens for the accordion chrome — reuse existing `--border`/`--sidebar-bg` or introduce `--accordion-*`. Plan can default to reuse.

These are implementation-level concerns, not admitted-scope decisions.

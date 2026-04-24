# Phase 3B-II A2b — Edit-mode Add Server Design

> **Scope:** Production-grade edit-mode hardening + Advanced section for the Add Server screen. Monolith PR (no split into A2b-a/A2b-b per user direction "don't defer, single production PR").
>
> **Status:** Brainstorm complete. 14 decisions resolved via lead + Codex (gpt-5.5, reasoning xhigh). Two full-quorum coherence review rounds run against the composed system caught 8 landmines (6 in R1, 2 in R2); all fixes merged into the decisions below. Ready for `writing-plans` handoff.

---

## 1. Context

A2a shipped the create-flow at `#/add-server` (PR #5, merged as `d165c79`). A2b makes that screen symmetric for edit: load an existing manifest, mutate through the same accordion UI, save back safely, with production-grade stale-detection + navigation hardening + Advanced field coverage.

Entry points:
- **Sidebar link** `#/add-server` — unchanged create-flow.
- **New:** `#/edit-server?name=<name>` — load + edit. Reached from Servers matrix row action or Migration Via-hub row action (A2b also wires Servers screen row click; that task is included in this plan).
- **A1 Migration → Create manifest** flow from A2a continues to open `#/add-server?server=&from-client=` — create-mode, no regression.

Backend dependencies (already shipped, extended by A2b):
- `api.ManifestGet(name string) (yaml string, err error)` — reads `servers/<name>/manifest.yaml`.
- `api.ManifestEdit(name, yaml string) error` — overwrites existing; fails if absent.
- `api.ManifestCreate(name, yaml string) error` — A2a path, unchanged.
- `api.ManifestValidate(yaml string) []string` — A2a path, unchanged.
- `api.Install(name string) error` — for the Reinstall banner action.

New backend surface A2b adds:
- `api.ManifestGet(name) → (yaml, contentHash, err)` — returns SHA-256 of the file at read time. Allows the GUI to detect external writes between Load and Save.
- `api.ManifestEdit(name, yaml, expectedHash) error` — rejects if the on-disk hash differs from `expectedHash`. Returns a typed `ErrManifestHashMismatch` the GUI maps to the stale-detection banner.

Frontend constraints (inherited from A2a): embedded bundle target <100KB, Vite 8 + TS 5 + Preact 10, Vitest 4, Playwright E2E.

---

## 2. Design decisions (14 total)

All decisions validated by Codex through two full-system coherence review rounds. Individual `(my pick / Codex pick)` noted where they converged or diverged; disagreements are explicitly resolved in favor of the safer option.

### D1 — Scope: monolith A2b, matrix view IN, secrets DEFERRED to A3

Codex initially recommended an A2b-a/A2b-b split for review-surface sanity. User overrode: "don't defer, single production PR." Monolith accepted with the explicit mitigation Codex then provided — **staged commits with separate review checkpoints inside the same PR**, so `@codex review` can be triggered at logical milestones instead of only at end-of-branch.

Multi-daemon matrix view is INCLUDED (reversal of lead's initial YAGNI-defer). Rationale: "common prod, not typical" per user direction — production users will have 4+ daemons eventually, and the matrix view keeps the form usable without a separate follow-up PR.

Env secret refs (`env: {FOO: "secret:KEY"}`) remain DEFERRED to A3. Without the Secrets screen there is no authoritative key namespace; any UX in A2b would invent a false source of truth or bake in migration churn when A3 ships.

### D2 — Save transaction: YAML-only + explicit [Reinstall] banner

Save overwrites `servers/<name>/manifest.yaml` via `api.ManifestEdit`. Save does NOT touch running daemons, scheduled tasks, or installed state. After a successful save the UI renders a banner:

> **Config saved.** Daemon is still running the old config. **[Reinstall]**

The `[Reinstall]` button calls `api.Install(name)` (the same endpoint A2a uses for Save & Install), which does port-probe + scheduled-task recreate + daemon restart.

This parallels A2a's `[Save]` vs `[Save & Install]` split and keeps the mental model clean: file mutation is a separate transaction from installed-state reconciliation.

### D3 — Stale-file detection: content hash with [Reload] / [Force Save] banner

On Load, `api.ManifestGet(name)` returns both YAML and a SHA-256 content hash. The hash is stored in form state as `loadedHash`. On Save, the request includes `loadedHash`; backend re-reads the file, computes current hash, and rejects the write if they differ (returning `ErrManifestHashMismatch`).

On mismatch, the GUI renders a banner:

> **Manifest changed on disk since you opened it.** Reload will discard your edits and show the new version. Force Save will overwrite with your version. **[Reload] [Force Save]**

### D4 — Force Save: re-read, re-merge `_preservedRaw`, last-writer-wins for known fields

(Codex R1 F2 + R2 G1.)

`[Force Save]` is NOT a naive overwrite. The pipeline:
1. Re-read the current disk contents via `api.ManifestGet(name)`.
2. Extract the current-disk `_preservedRaw` (unknown top-level keys).
3. Merge `_preservedRaw` from disk with the user's edited known fields (form state).
4. Write the merged result via `api.ManifestEdit(name, yaml, newHash)` — using the FRESHLY-READ hash as `expectedHash`.

Explicit semantic: **last-writer-wins for known fields. External additions to unknown top-level keys are preserved.** The Force Save banner text makes this concrete:

> Force Save will overwrite your manifest. **Preserving these external fields from the current disk copy:** `<list of top-level keys in _preservedRaw>`. Any nested changes from other sources may be lost.

A narrow TOCTOU window remains between step 4's hash-compute and the atomic rename. We accept this as documented semantic, not as a bug — closing it would require a filesystem lock (Windows-expensive, overkill for the "two GUI tabs in microsecond race" use case).

### D5 — Nav guard hardening: beforeunload + hashchange-interceptor + replaceState + suppression flag

(Codex R1 Q_C converged.)

A2a ships sidebar-intercept only (~90% exit coverage). A2b extends with:

- **`window.beforeunload` listener** — fires on tab close / refresh / Ctrl+W. Returns a truthy string from the handler, which triggers the browser's native "Leave site?" dialog. The returned string itself is ignored by modern browsers (Chrome 51+), but returning SOMETHING is the signal.
- **`hashchange` interceptor** — wraps the `useRouter` hook. On each `hashchange`:
  1. If form not dirty OR target === current → no-op, let the navigation proceed.
  2. Else show `window.confirm("Discard unsaved changes?")`.
  3. On user-accept → clear dirty state, let navigation proceed.
  4. On user-decline → `window.history.replaceState(null, "", event.oldURL)` to revert the hash WITHOUT re-firing hashchange.
- **Suppression flag** — the replaceState in step 4 itself fires another hashchange event. A `suppressNextHashchange: boolean` flag (ref) is set to `true` before replaceState, checked + cleared by the handler to skip the revert event.

### D6 — E2E strategy: per-test dialog handlers

(Codex R1 Q_C reversed lead's P1 pick.)

Playwright tests MUST register `page.once("dialog", ...)` handlers explicitly in each test that expects a dialog. NO global `test.beforeEach(page.on('dialog', d => d.accept()))`.

Rationale: global dialog auto-accept hides unexpected confirm/alert dialogs and makes spurious guard-fires look like successful navigation. Per-test handling forces regressions to surface as "Unexpected dialog" test failures.

Boilerplate cost: minor — only tests that actually trigger navigation on dirty forms need handlers; most tests don't.

### D7 — Route: `#/edit-server?name=<name>` + `AddServerScreen` mode prop reuse

Edit-mode adds a new screen key `"edit-server"`. The existing `AddServerScreen` component is extended with an optional `mode: "create" | "edit"` prop (default `"create"`, preserving A2a contract):

```tsx
case "add-server":
  body = <AddServerScreen mode="create" onDirtyChange={setAddServerDirty} />;
  break;
case "edit-server":
  body = <AddServerScreen mode="edit" onDirtyChange={setAddServerDirty} />;
  break;
```

Canonical identity comes from `manifest.name` in the loaded YAML, NOT from the `?name=` query string (Codex R1 gotcha: URL is dispatcher only).

No sidebar link for `#/edit-server` — entry points are Servers matrix row action + Migration Via-hub row action (existing surfaces, extended by A2b).

### D8 — Name field LOCKED in edit mode

(Codex R1 F4.)

`name` is identity. Renaming an installed manifest via GUI would require cascading URL updates, scheduled-task rename, client-config updates — massive blast radius for a rare operation. Edit mode locks the name field (`disabled` with tooltip "Kind and name are immutable after first install. Delete and recreate the server to change them.").

Create mode (via `#/add-server`) keeps name editable as before — A1 Migration → A2a prefill flow unaffected.

### D9 — Kind field LOCKED in edit mode

(Codex R1 F5.)

Same rationale as D8: `kind` determines install pipeline (global vs workspace-scoped lazy-proxy) and has installed-state consequences. Edit mode locks kind with the same tooltip. Users who need to change kind must delete and recreate via Add Server.

### D10 — Internal-ID daemon rename

(Codex R1 Q_B converged.)

Form state carries a stable UUID per daemon row, not serialized to YAML:

```ts
interface DaemonFormEntry {
  _id: string;  // UUID, form-state-only
  name: string;
  port: number;
  // ... workspace-scoped fields
}
```

`client_bindings` reference daemons by `_id` internally. On `toYAML`, the serializer resolves `_id → daemon.name`. On `parseYAMLToForm`, a fresh `_id` is assigned per daemon and bindings are re-linked by name (one-time resolution at load).

User-facing UX is identical to A2a cascade-with-validation. Internal-ID is pure robustness hardening — it prevents orphan bindings during rapid rename/delete sequences in edit mode.

### D11 — Accept-as-baseline button: DROPPED

(Codex R1 F1.)

The button, if implemented, would move `initialSnapshot` to the current form state after a Paste YAML import, resetting dirty to false. But this creates a **data-loss path**: pasted content is not persisted to disk, yet nav protection disappears. User closes tab → beforeunload silent → data lost.

Normal Save already covers the "accidental paste then keep" case. Dropping the button removes the data-loss surface without losing functionality.

### D12 — Advanced section: G1 (everything except secrets) + kind-gated sub-fields

(Codex R1 Q_G, R2 G2 composition fix.)

Advanced section includes these fields:

**Always visible:**
- `idle_timeout_min: int`
- `daemon.extra_args: string[]` — per-daemon, repeatable rows
- `base_args_template: string[]` — templated variant, tooltip "`$VAR` placeholders expanded at install time"

**Visible only when `kind === "workspace-scoped"`:**
- `languages: LanguageSpec[]` — repeatable subsections with `{name, backend, transport, lsp_command, extra_flags[]}`
- `port_pool: {start, end}` — range picker
- `daemon.context: string` — workspace context marker (per-daemon)

When `kind` is edited in CREATE mode: conditional fields appear/disappear. When `kind` is locked (edit mode), the form renders whichever branch matches the loaded manifest's kind.

### D13 — `_preservedRaw`: top-level unknowns only + read-only mode for nested-unknown manifests

(Codex R1 F3 + R2 G2.)

`_preservedRaw: Record<string, unknown>` holds top-level YAML keys the GUI doesn't recognize (forward-compat + A3-bound `env.secret:...` refs + user custom annotations). On `parseYAMLToForm`: unknown top-level keys go here. On `toYAML`: known-field output is merged with `_preservedRaw` before serialization.

**Nested-unknown fields (`daemons[0].extra_config`, `languages[0].weird_field`) are NOT preserved.** Supporting nested preservation requires identity-by-path semantics that get tangled with rename/delete/reorder operations.

Instead: on Load, if the parsed YAML contains ANY nested unknown, AddServer enters **read-only mode**:
- All inputs + action buttons (`[Validate][Save][Save & Install][Paste][Force Save][Reinstall]`) are DISABLED.
- Sticky banner at the top:
  > **This manifest contains fields the GUI can't handle: `<list>`.** Editing via GUI would drop them. Edit via CLI (`mcphub manifest edit <name>`) or delete + recreate via Add Server. **[Back to Servers]**
- `[Copy YAML]` remains enabled (inspection-only).
- `[Back to Servers]` is enabled and bypasses the dirty check (read-only mode has no dirty state — formState stays at load baseline).

User inspects without risk of accidental data loss; sophisticated manifests stay CLI-owned.

### D14 — Load failure UX + invariant

Load failure (`api.ManifestGet(name)` returns an error: file missing, bad YAML, permission denied, etc.):

- Inline error banner:
  > **Failed to load `<name>`:** `<error message>`
  > **[Retry] [Back to Servers]**
- Form renders but all inputs + action buttons disabled.
- `isDirty` invariant: load failure → `formState = BLANK_FORM`, `initialSnapshot = BLANK_FORM` → `deepEqualForm(a, b) === true` → `isDirty === false`.
- Navigation guards never fire on load-failure screen (dirty=false). `[Back to Servers]` works via normal sidebar logic.

### Save pipeline (Codex R1 explicit ordering)

Every Save / Save&Install / Force Save follows this exact sequence:

1. **Validate** — client-side (regex, required fields), then server-side via `api.ManifestValidate`. Abort on warnings.
2. **Recheck disk hash** — Save sends `expectedHash = loadedHash`; Force Save re-reads disk AT save-time and uses that as `expectedHash`.
3. **Resolve stale choice** — if hash mismatch on Save, show [Reload]/[Force Save] banner, abort the submit.
4. **Serialize + merge** — `toYAML(formState)` + splice in `_preservedRaw`.
5. **Atomic write** — `api.ManifestEdit` internally writes to `tmp` + atomic rename.
6. **Update hash + snapshot** — on success, `setLoadedHash(newHash)` + `setInitialSnapshot(formState)`.
7. **Show reinstall banner** — "Config saved. Daemon still running old config. [Reinstall]".

Submission-version counter from A2a remains: Steps 1-7 bail if `submissionCounter.current !== capturedVersion` after any await.

---

## 3. Cross-cutting invariants

Summarized from both coherence review rounds:

1. **`_preservedRaw` ownership** — only top-level unknown keys. Nested unknowns trigger read-only mode (D13). No rename/delete interactions with nested preservation.

2. **Identity immutability in edit mode** — name and kind locked (D8, D9). URL stays canonical; internal `manifest.name` is authoritative.

3. **Force Save semantic** — last-writer-wins for known fields; external top-level unknowns preserved from fresh disk read (D4). Narrow TOCTOU window explicit, not a bug.

4. **Hash-check lifecycle** — Load stores hash in form state; every Save path (including Save&Install / retry) sends it as `expectedHash`; success updates it to the new post-write hash (step 6 of pipeline).

5. **Suppression flag on hashchange revert** — prevents infinite loop on nav-guard cancel (D5).

6. **Per-test E2E dialog handling** — forces regressions to surface as "Unexpected dialog" test failures (D6).

7. **Submission-version counter** — carried from A2a; all awaits in Save pipeline check `version === submissionCounter.current`.

8. **Dirty-flag invariant on load failure** — always false (D14). Nav guards never spuriously fire on load-failure screens.

---

## 4. Scope boundaries

### In A2b

- `#/edit-server?name=<name>` route.
- `AddServerScreen` extended with `mode: "create" | "edit"` prop.
- Load: `api.ManifestGet` returning YAML + hash; `parseYAMLToForm` + hash stored in form state.
- Save: `api.ManifestEdit(name, yaml, expectedHash)` with stale-detection banner.
- Force Save with re-read-and-remerge (D4).
- Name + kind locked in edit mode.
- Internal-ID daemon rename (form-state-only UUIDs).
- Nav hardening: beforeunload + hashchange-interceptor + replaceState + suppression flag.
- Advanced section: G1 + kind-gated sub-fields (D12).
- `_preservedRaw` for top-level unknowns.
- Read-only mode for manifests with nested unknowns (D13).
- Load failure UX (D14).
- Multi-daemon matrix view when `daemons.length >= 4` (D1).
- Reinstall banner after successful Save.
- Servers matrix row click → `#/edit-server?name=<row.name>` (new entry point).
- Playwright E2E: edit load happy path, stale detection, Force Save semantics, name/kind locking, nested-unknown read-only mode, Advanced section (create + edit), matrix view rendering, Paste→Reload rapid-flow, nav guards (beforeunload + hashchange).

### Deferred (explicitly NOT in A2b)

- **A3 Secrets integration** — `env.secret:KEY` UX, secret-key namespace.
- **A2c** (future): edit-mode name rename with cascade (delete-old + create-new UX wrapper), bulk manifest operations.
- **`beforeunload` custom message** — ignored by modern browsers since Chrome 51. We return truthy; browser shows its default dialog.

### Out of scope entirely

- Cross-platform `beforeunload` variance (documented in D5 comment).
- JSON Schema inline autocomplete for Advanced fields (non-goal per spec §2.2).
- Conflict auto-resolution (three-way merge) — stale detection is explicit reload-or-overwrite only.

---

## 5. Handoff contract to writing-plans

The plan should produce a staged-commit implementation. Suggested structure (plan may refine):

1. Backend: extend `api.ManifestGet` to return `(yaml, contentHash)`; extend `api.ManifestEdit` to accept `expectedHash`; add `ErrManifestHashMismatch` typed error.
2. Frontend types: `ManifestFormState` gets `loadedHash: string`, `_preservedRaw: Record<string, unknown>`; `DaemonFormEntry` gets `_id: string`.
3. `parseYAMLToForm` + `toYAML` updates: UUID assignment, `_preservedRaw` extraction + merge, nested-unknown detection (helper `hasNestedUnknown(raw): boolean`).
4. `useRouter` extension: `hashchange-interceptor` hook wrapping the base router, accepts `guard: (target) => boolean | Promise<boolean>` callback.
5. `AddServerScreen` mode prop: branch on mount (load vs blank), branch on submit (Edit vs Create API), lock name + kind when mode === "edit".
6. Stale-detection UI: banner + [Reload]/[Force Save] buttons, Force Save pipeline.
7. Read-only mode: disable-all-inputs branch, sticky banner, [Back to Servers].
8. Advanced section — G1 + kind-gated subsections. Probably the largest single task.
9. Multi-daemon matrix view — extends `ClientBindingsSection`'s adaptive dispatch.
10. `beforeunload` wire-up (client-side).
11. Servers matrix row click + Migration Via-hub row click → `#/edit-server?name=...`.
12. Playwright E2E — ~12-15 new scenarios.
13. `CLAUDE.md` coverage update + full-suite smoke.

Each step becomes one commit with TDD (red → green → commit), matching A2a cadence. Estimated ~35-45 commits, ~2500-3500 LOC TS + ~150 LOC Go + ~500 LOC Playwright.

**Required pre-execute gate:** after memo and plan are both committed, request Codex full-plan review BEFORE dispatching any implementer. Round 1 and 2 coherence reviews on this memo caught 8 composition-level issues; the equivalent plan-level review is expected to catch implementation-ordering landmines that escape per-task subagents.

---

## 6. Open questions for plan authoring

These are implementation-level concerns the plan author decides, NOT admitted-scope design decisions:

- Exact JSX structure for kind-gated sections (one component with internal `if` branches, or two components dispatched by parent).
- Whether `_preservedRaw` extraction lives in `parseYAMLToForm` or a separate `extractPreserved` helper.
- CSS tokens for the stale-detection banner and read-only mode banner (reuse existing `--danger`/`--warning` or introduce `--stale`).
- Matrix view rendering: HTML `<table>` or CSS grid. Plan decides.
- Commit staging for review checkpoints within the PR — plan groups commits into logical milestones (e.g., "backend ready", "load path green", "save path green", "advanced section", "matrix view", "E2E").

These are implementation concerns, not design decisions.

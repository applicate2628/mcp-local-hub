# Phase 3B-II A3-b — env.secret picker in AddServer/EditServer (design memo)

**Status:** design memo (pre-plan, rev 14)
**Author:** Claude Code (brainstorm + Codex Q1–Q4 consensus, 2026-04-26)
**Predecessor:** PR #18 (Phase 3B-II A3-a — Secrets registry screen, merged at `f175604`)
**Backlog entry:** `docs/superpowers/plans/phase-3b-ii-backlog.md` §A row A3-b
**Sibling memo:** [`2026-04-25-phase-3b-ii-a3a-secrets-screen-design.md`](2026-04-25-phase-3b-ii-a3a-secrets-screen-design.md)

## 1. Summary

A3-b delivers the consumer UX for the secret registry that A3-a built: a `<SecretPicker>` combobox embedded in every env-row of `AddServerScreen` (used by both `#/add-server` and `#/edit-server` via the existing `mode` prop). The picker exposes a 🔑 affordance button that opens a dropdown of vault keys, also auto-opens when the user types the `secret:` prefix, and provides an inline "Create this secret" path that opens the existing `<AddSecretModal>` overlaid on the form. Broken or unverified secret references in already-saved manifests are surfaced via per-row inline markers and (when count > 1 or vault state is not `ok`) a compact summary line above the env section.

The form data model is **unchanged**: env values stay as raw strings (`secret:<key>`, `$VAR`, `file:<path>`, or literal). The picker only writes back `secret:<key>` strings. Other prefixes pass through.

This memo encodes:
- The Q1–Q4 brainstorming consensus as locked design decisions D1–D4.
- Six implementation-structure decisions D5–D10 not covered by Q1–Q4 (form data model, component decomposition, modal hosting, snapshot wiring, name-match algorithm, keyboard contract).
- Component prop surfaces, state machines, and the ref-state classification.
- Risks and the tests required to ship.

A3-b does **not** ship batch-add cycling (Q4 option E), inline name-match hint (Q2 option C/E), or pickers for `$VAR` / `file:` prefixes (Q1 option B). Each is recorded in §3.3 and is a separate future phase.

## 2. Context recon

### 2.1 The host form — `AddServerScreen`

[`internal/gui/frontend/src/screens/AddServer.tsx`](../../../internal/gui/frontend/src/screens/AddServer.tsx) is a single component used by both `#/add-server` and `#/edit-server` via a `mode: "create" | "edit"` prop. The env section lives at [AddServer.tsx:807-829](../../../internal/gui/frontend/src/screens/AddServer.tsx#L807-L829):

```tsx
<AccordionSection title="Environment">
  <div class="repeatable-rows" data-testid="env-rows">
    {formState.env.map((row, i) => (
      <div class="form-row env-row" key={i} data-env-row={i}>
        <input type="text" placeholder="KEY" value={row.key}
               onInput={(e) => updateEnv(i, "key", ...)} disabled={readOnly} />
        <input type="text" placeholder="value (literal or $HOME/...)" value={row.value}
               onInput={(e) => updateEnv(i, "value", ...)} disabled={readOnly} />
        <button type="button" onClick={() => deleteEnv(i)} ...>×</button>
      </div>
    ))}
    <button type="button" onClick={addEnv} ...>+ Add environment variable</button>
  </div>
</AccordionSection>
```

`formState.env` is `{ key: string, value: string }[]`. The value is a single string. `updateEnv(i, "value", v)` reassigns it. Serialization to YAML happens in `lib/manifest-yaml.ts:toYAML` and parses back via `parseYAMLToForm`. Neither parses prefixes — the value is round-tripped as-is.

The picker does **not** add a third sub-form-state field; it only swaps the value `<input>` for `<SecretPicker>`. The `value` and `onChange` continue to be the existing single-string contract.

### 2.2 Existing `AddSecretModal` — close-path consolidation + async `onSaved`

[`internal/gui/frontend/src/components/AddSecretModal.tsx`](../../../internal/gui/frontend/src/components/AddSecretModal.tsx) (shipped in A3-a) accepts `prefillName?: string` and locks the name field when prefilled ([line 70](../../../internal/gui/frontend/src/components/AddSecretModal.tsx#L70)). It surfaces backend errors inline ([line 84](../../../internal/gui/frontend/src/components/AddSecretModal.tsx#L84)) and uses `<dialog>` with `showModal()`, ESC guard via `onCancel`, and is fully keyboard-driven.

The current implementation calls `props.onClose()` explicitly from inside the submit handler ([line 53](../../../internal/gui/frontend/src/components/AddSecretModal.tsx#L53)) AND wires `onClose={() => props.onClose()}` on the native `<dialog>` element ([line 40](../../../internal/gui/frontend/src/components/AddSecretModal.tsx#L40)). The native `onClose` event also fires from the `useEffect`-driven `dialogRef.current.close()` when `props.open` becomes `false` ([line 28-30](../../../internal/gui/frontend/src/components/AddSecretModal.tsx#L28)). On success this currently fires `props.onClose()` twice (once from the submit handler, once from the native event after the parent flips `open` to `false`).

**A3-b corrections (Codex memo-R2 P2-A + P1-1):**

1. **Async `onSaved` contract.** Widen `onSaved: () => void` to `onSaved: () => void | Promise<void>` and `await` it in the submit handler. Backwards-compatible: A3-a's `Secrets.tsx` call site returns `void` synchronously and continues to work; A3-b's AddServer returns a Promise and the modal awaits it. The save flow becomes:

    ```ts
    try {
      await addSecret(name, value);
      await props.onSaved();          // parent does its post-save work (e.g., snapshot.refresh())
      dialogRef.current?.close();     // native close → onClose event → props.onClose() ONCE
    } catch (err) {
      setServerErr((err as Error).message);
    }
    ```

    The explicit `props.onClose()` call after `props.onSaved()` is **removed**. Closing via `dialogRef.current?.close()` triggers the native `onClose` event, which is the single path that calls `props.onClose()`.

2. **Single-close-path invariant.** Wherever the modal needs to dismiss itself (Save success, Cancel button, ESC), it routes through `dialogRef.current?.close()` only. The Cancel button handler changes from `onClick={() => props.onClose()}` to `onClick={() => dialogRef.current?.close()}`. The form submission path is described in §1 above. ESC is already routed through `<dialog>`'s native cancel/close events.

3. **Parent-side `onClose` is the only refresh trigger from the close lifecycle.** Because `props.onClose()` now fires exactly once per modal lifecycle (regardless of Save/Cancel/ESC/error dismiss), the parent can put its post-modal refresh logic there unconditionally. To avoid the redundant refresh in the success path (where `onSaved` already refreshed), the parent uses a ref to skip the on-close refresh once. Wiring is in §5.7.

Apart from these changes (≈6 lines in AddSecretModal), no other prop or behavior changes. The component remains usable from Secrets.tsx without modification.

### 2.3 Existing `useSecretsSnapshot` hook

[`internal/gui/frontend/src/lib/use-secrets-snapshot.ts`](../../../internal/gui/frontend/src/lib/use-secrets-snapshot.ts) (shipped in A3-a) wraps `getSecrets()` with stale-while-revalidate semantics:

- Returns `{ status, data, error, refresh }` with `status: "loading" | "ok" | "error"`.
- Stale-while-revalidate: subsequent refreshes retain `status: "ok"` and the previous `data` until the new fetch completes.
- Auto-refetches on `window.focus`.
- Uses a generation counter to drop stale fetch responses ([line 15](../../../internal/gui/frontend/src/lib/use-secrets-snapshot.ts#L15)).

A3-b consumes this hook **once per form** (one instance shared across all env rows), then passes `snapshot` down as a prop to each `<SecretPicker>`. Calling `refresh()` after a successful Add bubbles up to invalidate dropdowns in every row.

### 2.4 Backend — already complete

A3-a shipped:
- `GET /api/secrets` → `{ vault_state, secrets: [{name, state, used_by}], manifest_errors }` — listed in `internal/gui/secrets.go`.
- `POST /api/secrets` (Add) — `{name, value}`, returns 201 on success, 409 on duplicate.
- `POST /api/secrets/init` — idempotent vault init.
- All endpoints CSRF-protected via `requireSameOrigin` per A3-a memo §5.

A3-b adds **zero new backend endpoints**. All needs are met by `getSecrets()` + `addSecret()` from `lib/secrets-api.ts`.

### 2.5 Resolver and prefix coexistence

[`internal/secrets/resolver.go`](../../../internal/secrets/resolver.go) parses manifest env values:

| Prefix    | Source                | A3-b picker scope |
|-----------|-----------------------|-------------------|
| `secret:` | `vault.Get`           | **YES — picker reads/writes** |
| `file:`   | `local config map`    | NO — passes through |
| `$VAR`    | `os.LookupEnv`        | NO — passes through |
| (literal) | returned as-is        | NO — passes through |

The picker recognizes only the `secret:` prefix. Values starting with `file:`, `$`, or anything else are treated as literals from the picker's perspective — the dropdown stays closed unless the user clicks the 🔑 button explicitly. Writing back from the picker always produces a `secret:<key>` string.

The auto-open-on-typing-`secret:` heuristic (D1 in §4) is intentionally narrow: the prefix must be exactly `secret:` (no whitespace, no other prefixes). Typing `file:` does NOT open the picker.

### 2.6 Reusable patterns from A3-a Secrets screen

The A3-a Secrets screen at `Secrets.tsx:385-395` already uses a `↳ Add this secret` linklike button for `referenced_missing` rows. A3-b's picker dropdown "Create this secret" entry uses the **same vocabulary** (button labelled `+ Create "<key>" in vault…` for the broken case, `+ Create new secret…` for the generic case) and reuses CSS class `linklike` to keep visual consistency across the two surfaces.

## 3. Scope

### 3.1 In scope

**Frontend only — no backend changes.**

1. `SecretPicker` component (combobox + dropdown + auto-open + create flow).
2. `secret-ref.ts` — pure helper to classify a value string as `secret:<key>` | other-prefix | literal.
3. `name-match.ts` — pure helper to normalize an env KEY for matching against vault key names (Q2 sort).
4. AddServer.tsx integration:
   - Replace plain value `<input>` with `<SecretPicker>` per env row.
   - Single `useSecretsSnapshot()` instance at form level, passed down.
   - Single `<AddSecretModal>` instance at form level, opened by picker callback.
   - Conditional `<BrokenRefsSummary>` line above env section when count > 1 OR vault_state ≠ ok.
5. CSS for picker dropdown, broken/unverified value border, compact summary line, `matches KEY name` and `missing` / `unverified` badges.
6. Vitest unit tests: SecretPicker rendering and interactions; secret-ref parser; name-match.
7. Playwright E2E scenarios: see §7.2.
8. `go generate ./internal/gui/...` to regenerate `internal/gui/assets/{index.html,app.js,style.css}` — required because the embedded bundle is what ships.

### 3.2 Explicitly out of scope — deferred to future phases

- **Cycling AddSecretModal for bulk-add** (Q4 option E). Multi-missing batching is a separate workflow; the user can repeat the picker → Create flow per row in A3-b.
- **Inline name-match hint above value field** (Q2 option C/E). The Q2 decision was sort-by-match-only inside the dropdown; an additional always-visible inline hint is a future enhancement gated on dogfood feedback.
- **Picker for `$VAR` and `file:` prefixes** (Q1 option B). Treating env values as discriminated union (literal vs secret vs env vs file) is a "typed env value editor" phase that requires a `parseYAMLToForm`/`toYAML` refactor. A3-b does not touch it.
- **CLI parity for the picker.** The existing `mcphub manifest edit <name>` flow remains text-only. There is no plan to add an interactive picker to the CLI in A3-b.
- **Multi-tab vault edit conflict resolution.** The "Manage all secrets (new tab)" link can produce concurrent vault edits across tabs; A3-b relies on the existing `vaultMutex` (process-local) and the focus-revalidation in `useSecretsSnapshot`. Cross-process LWW is the open `work-items/bugs/a3a-vault-concurrent-edit-lww.md` work and is unchanged by this phase.

### 3.3 Why split this from A3-a

A3-a delivered the registry surface and proved the backend contract under real workloads (10+ Codex review rounds, 14 E2E scenarios, dogfooding by the user). A3-b layers consumer UX on top of that contract. The split allows A3-b to consume `GET /api/secrets` exactly as it stands, with no follow-up envelope changes; it also lets the AddServer form remain the single visual surface that adopts the picker (vs. building both registry + picker in one PR, which would have ~1.5x the surface and 2x the regression risk).

## 4. Decisions

### D1 — UX paradigm: combobox with affordance button (Q1)

**Chosen:** Combobox — value field stays a regular `<input type="text">` plus a 🔑 button on the right. Clicking the button opens a dropdown anchored under the value field. Typing exactly `secret:` at the start of the value also auto-opens the dropdown. ESC closes. Click-outside closes. Dropdown does NOT open on value-field focus alone.

The button has `aria-label="Pick secret"` and `title="Pick secret"`. Visible affordance is required — the dropdown is otherwise undiscoverable for users who haven't memorized the `secret:` syntax.

**Why not autocomplete-on-focus (Q1 option A):** opens on every focus event, including the common literal-value path (e.g., `DEBUG=1`). Suppression heuristics ("hide on Backspace", "hide if value doesn't match `secret:`") accumulate as UX debt and are user-hostile. Codex's framing: "auto-opening on every focus will punish the common literal-value path."

**Why not mode-toggle pills (Q1 option B):** would change the form data model — env values become discriminated unions instead of single strings, requiring a `parseYAMLToForm`/`toYAML` rewrite. That is the "typed env value editor" feature, not "secret picker". A3-b stays narrow.

### D2 — Smart suggestion based on env KEY: sort-by-match only (Q2)

**Chosen:** When the dropdown opens (via button or `secret:` typing), the vault key whose normalized name matches the normalized env KEY is sorted to the top with a `matches KEY name` badge. No auto-prefill. No always-visible hint. No surprise mutation.

Normalization rules (D9 below): lowercase the env KEY, replace `-` with `_`, then string-equals against vault key names (vault keys are case-sensitive but conventionally lowercase). No fuzzy matching, no Levenshtein.

If multiple vault keys normalize to the same string as the env KEY, sort by exact-equality first, then prefix-equality, then alphabetical. Stable.

**Why not auto-prefill (Q2 option D):** silently mutates user input; for a manifest editor where literal-vs-secret intent must remain user-owned, this is dangerous. Codex flagged it as REJECT.

**Why not inline hint (Q2 option C):** adds a second affordance outside the combobox model and creates UI clutter on every env row. Defer to a future phase if dogfood shows the picker is missed.

### D3 — Broken refs presentation: inline marker + dropdown surface + conditional summary (Q3)

**Chosen behavior per row** (this table summarizes inline-marker decisions only; the dropdown render order and section composition are defined authoritatively in §5.5 and referenced from §5.6 — Codex memo-R8 P3):

| condition | inline marker |
|---|---|
| `value` is not `secret:<k>` | none |
| `value = secret:<k>`, vault_state="ok", `k` in vault | none |
| `value = secret:<k>`, vault_state="ok", `k` NOT in vault | red border + ⚠ icon, tooltip "Secret '\<k\>' not found in current vault" |
| `value = secret:<k>`, vault_state ≠ "ok" | yellow border + ⚠ icon, tooltip "Cannot verify — vault \<vault_state\>" |
| any state, `snapshot.status === "error"` | yellow border + ⚠ icon, tooltip "Could not load vault state — Retry" |

For the dropdown shape (sections / Create-entry placement / ordering) per state, see the §5.5 RefState table — that is the single source of truth.

**Conditional summary line** above the env section:
- Renders only when `brokenCount > 1` OR `vault_state !== "ok"`.
- Layout: small horizontal pill with ⚠ icon + text. NOT a heavy banner with primary CTA — that would make the form feel like an error dashboard.
- For `brokenCount > 1`: text reads `"{count} secrets referenced but not in vault: {comma-separated names}. Daemons will fail to start."`
- For `vault_state="decrypt_failed"`: "Vault not readable (decrypt_failed). Cannot verify any `secret:` references. Fix vault on Secrets screen first."
- For `vault_state="missing"`: "Vault not initialized. Open Secrets screen to create one."
- For `vault_state="corrupt"`: "Vault file corrupted. Open Secrets screen to recover."

**Why not banner-only (Q3 option C/E):** banner is heavy UI and the primary CTA (open Secrets registry) re-introduces dirty-guard navigation issues; the picker is the right surface for inline remediation.

**Why not per-row Add button (Q3 option B):** adds UI weight to every broken row and splits the picker UX from remediation. Codex framing: "useful but clutters every broken row and splits picker UX from remediation."

### D4 — Create-secret UX: modal in-place + new-tab escape (Q4)

**Chosen:** Clicking "+ Create '\<k\>' in vault…" or "+ Create new secret…" in the dropdown opens the existing `<AddSecretModal>` as an overlay (`<dialog showModal()>`). The Create entry is itself **gated** by snapshot state per §5.6 (`canCreate = snapshot.status === "ok" && snapshot.data.vault_state === "ok"`); when `canCreate === false`, the entry renders disabled with a reason tooltip and clicking is a no-op. The modal:
- Pre-fills name with the broken-ref key when applicable (locks the name field via existing `prefillName` prop).
- Shows password-masked value input.
- On Save: `await addSecret(name, value)` → on success, calls `snapshot.refresh()` (NOT optimistic — wait for revalidation), then closes.
- On 409 conflict: shows inline error "Secret already exists. Refresh and use the existing entry, or pick a new name." Modal stays open. (After user dismisses, `snapshot.refresh()` is still triggered so the picker can use the now-known existing entry.)
- On other backend errors (vault not ok, validation, network): shows inline error, modal stays open, picker status unchanged.

**Modal footer:** small link `⤴ Manage all secrets (new tab)` that opens `#/secrets` via `window.open(url, "_blank")`. This avoids `<a target="_blank">` SPA hash-routing pitfalls. Form context is preserved completely.

**Why not full-nav (Q4 option B):** fights dirty state, risks losing unsaved manifest edits, makes a small picker repair feel like a context switch.

**Why not cycling modal (Q4 option E):** multi-missing batching is a separate workflow with partial-success and ordering concerns; A3-b is too narrow a phase to absorb it.

### D5 — Form data model: env value stays a single string

**Chosen:** `formState.env[i].value` continues to be `string`. The picker reads it and writes back to it. No new type discrimination at form-state level. Serialization (`toYAML`) and deserialization (`parseYAMLToForm`) are unchanged.

**Implication:** the picker's "is this currently a secret ref?" judgment is computed at render time by `parseSecretRef(value)`. Re-computing on every render is cheap (string-prefix check). No memoization required; if profiling later shows it matters, memoize at the row level.

### D6 — Component decomposition: one new component, one host, no per-row modal

```text
SecretPicker (new)
├── owns: dropdown open/closed state, focused item index
│           (filter is DERIVED from `value` prop, not a separate state field —
│            the value <input> is the single source of typed text; see §5.2)
├── consumes: snapshot (prop), envKey (prop), value (prop), onChange (prop), onRequestCreate (prop)
├── DOES NOT own: AddSecretModal (lifted to AddServer)
└── DOES NOT own: useSecretsSnapshot (lifted to AddServer)

AddServer (modified)
├── uses: useSecretsSnapshot once for the entire form
├── hosts: single AddSecretModal instance with state for prefill name + open/closed
├── computes: brokenRefs[] for summary line and per-row marker
└── renders: <SecretPicker> per env row, <BrokenRefsSummary> conditionally above env section
```

This keeps SecretPicker testable in isolation with mocked `snapshot` prop. Hosting AddSecretModal at AddServer level guarantees only one modal can be open at a time, which matches `<dialog showModal()>` browser semantics (calling `showModal()` on a second dialog while one is open throws).

### D7 — `useSecretsSnapshot` is a single instance per form

The snapshot is consumed once at AddServer level via `const snapshot = useSecretsSnapshot();` and passed down to every `<SecretPicker>` row. This guarantees:
- A single `GET /api/secrets` per form mount.
- A single revalidation when any picker triggers a Create flow (refresh propagates to all rows).
- Consistent state across all rows: if the vault transitions from `ok` → `decrypt_failed` mid-session (e.g., via `mcphub secrets edit` in another shell) and the focus-refetch picks it up, every row updates simultaneously rather than per-row staggered.

### D8 — Snapshot revalidation triggers

Beyond `useSecretsSnapshot`'s built-in `window.focus` listener, A3-b adds two more triggers:

1. **On dropdown open** — first render of the dropdown calls `snapshot.refresh()` if `snapshot.status === "ok"` and `snapshot.fetchedAt` is older than 30 seconds (i.e., `Date.now() - snapshot.fetchedAt > 30_000`). This catches stale state when the user takes a long time to open the picker after form load.
2. **On AddSecretModal save success** — the `onSaved` callback calls `snapshot.refresh()`. The callback awaits `refresh()` before closing the modal so the picker dropdown opens fresh on the next user action.

**D8a — fetchedAt tracking.** The existing snapshot hook returns `{ status, data, error, refresh }` without a timestamp. A3-b extends it to optionally return `fetchedAt: number | null` (epoch ms of last successful fetch). The change is additive; existing consumers (Secrets.tsx) ignore the new field. **Codex review must verify:** the existing tests in `use-secrets-snapshot.test.ts` keep passing without changes; the new field is `null` while loading and gets stamped to `Date.now()` on success.

### D9 — Name-match algorithm

```ts
// lib/name-match.ts

// normalizeForMatch lowercases and converts hyphens to underscores. Used on
// BOTH sides of comparison (env KEY and vault key) so case-mismatches don't
// hide an obvious match. Vault keys are not constrained to be lowercase by
// the backend (NAME_RE allows mixed case), so a vault key written as
// "OPENAI_API_KEY" must still match an env KEY of "OPENAI_API_KEY".
export function normalizeForMatch(s: string): string {
  return s.toLowerCase().replace(/-/g, "_");
}

// matchTier returns a comparable tier for sorting dropdown items.
// Lower tier = higher priority (sort ascending). Tier 0 also gets the
// "matches KEY name" badge; tier 1 sorts up but gets no badge.
export function matchTier(vaultKey: string, envKey: string): 0 | 1 | 2 {
  const v = normalizeForMatch(vaultKey);
  const e = normalizeForMatch(envKey);
  if (v === e) return 0;                                  // exact after normalization (badge)
  if (v.startsWith(e) || e.startsWith(v)) return 1;       // prefix overlap in either direction (no badge)
  return 2;                                               // unrelated, alphabetical fall-back
}
```

Sort the dropdown items by `(matchTier(vaultKey, envKey), vaultKey)` ascending. Show the `matches KEY name` badge only on tier 0. Tier 1 (prefix overlap in either direction) sorts up but gets no badge — "matches KEY name" would be misleading when only one string is a prefix of the other.

No Levenshtein, no Jaro-Winkler, no acronym matching. Conservatism is the point. The badge text is the user-facing claim; only show it when the claim is unambiguously true.

### D10 — Keyboard contract (editable-combobox model)

The picker uses the **editable-combobox** ARIA pattern (Codex memo-R2 P2-C): DOM focus stays on the value `<input>` at all times while the dropdown is open. Dropdown options are NOT focusable; instead `aria-activedescendant` on the input tracks the highlighted option's id. There is no separate "focus on dropdown item" mode.

```text
Focus on value <input> (dropdown closed):
  Type "secret:" exactly       → dropdown auto-opens; focus stays in input
  Type any other character     → focus stays in input; no dropdown change
  Tab                          → focus moves to next form element (🔑 button)
  ArrowDown                    → opens dropdown, highlights first item, aria-activedescendant set
  ArrowUp                      → opens dropdown, highlights last item, aria-activedescendant set
  Esc                          → no-op (nothing to close)
  Enter                        → no-op (no submit; the parent form has its own submit buttons)

Focus on value <input> (dropdown open):
  ArrowDown                    → highlight moves down; wraps from last to first
  ArrowUp                      → highlight moves up; wraps from first to last
  Enter                        → selects highlighted option, populates value with `secret:<key>`,
                                 closes dropdown, focus stays in input
  Esc                          → closes dropdown, focus stays in input
  Tab                          → closes dropdown, focus moves to next form element
  Type any character           → narrows dropdown items (substring on normalized vault key);
                                 highlight resets to first item

Focus on 🔑 button:
  Enter / Space                → toggles dropdown open/closed; if opening, moves DOM focus
                                 back to the value <input> so the editable-combobox model holds
  Tab                          → focus moves to next form element
```

ArrowUp/ArrowDown wrapping is consistent across all three rows (closed-input, open-input). The earlier rev claim that "ArrowUp at top closes" is dropped; wrap is the only behavior.

No focus trap. Clicking outside the picker closes the dropdown (standard combobox semantics). The host form's existing dirty-guard and other form behaviors are unaffected.

## 5. Component / API contracts

### 5.1 `SecretsSnapshot` — picker-side type alias

The existing hook in `use-secrets-snapshot.ts` returns `SnapshotState & { refresh: () => Promise<void> }`. A3-b adds a `fetchedAt: number | null` field (D8a) and exports a single named type for picker consumers:

```ts
// lib/use-secrets-snapshot.ts (revised export shape)

export type SnapshotState =
  | { status: "loading"; data: null;            error: null;             fetchedAt: null }
  | { status: "ok";      data: SecretsEnvelope; error: null;             fetchedAt: number }
  | { status: "error";   data: null;            error: APIError | Error; fetchedAt: number | null };

export type SecretsSnapshot = SnapshotState & { refresh: () => Promise<void> };

export function useSecretsSnapshot(): SecretsSnapshot { /* ... */ }
```

`fetchedAt` is `null` while initial loading and on errors that have not yet seen a successful fetch. Once a fetch succeeds, it's stamped to `Date.now()`. Subsequent failures retain the last-known `fetchedAt` (so callers can tell "we used to have data" from "we never got data") but **do not retain the prior `data`** — the existing hook contract sets `data: null` on transition to `error`, and A3-b preserves that contract intentionally. Translating to the picker surface (Codex memo-R2 P2-B):

- A transient refresh error after a successful load → `status: "error"`, `data: null`, `fetchedAt: <previous timestamp>`. classifyRefState (§5.5) treats this as `"unverified"` — we no longer have validated data, so we cannot honestly classify a `secret:foo` ref as `present` / `missing`.
- The `fetchedAt: number` on the error variant exists ONLY for time-based UX hints ("vault data may be stale — last loaded 2 minutes ago — Retry"), NOT for classification fallback.

The hook never throws from `refresh()` — failures resolve the promise with `status: "error"` set. Callers don't need try/catch.

`use-secrets-snapshot.test.ts` gains a unit test asserting the success-then-error transition: after a successful first load (status: "ok", fetchedAt: T1), force a network failure on the next refresh, observe `status: "error"`, `data: null`, `fetchedAt: T1`.

### 5.2 `SecretPicker` props

```ts
import type { SecretsSnapshot } from "../lib/use-secrets-snapshot";

export interface SecretPickerProps {
  // Required
  value: string;                        // raw env value (literal | secret:<k> | $VAR | file:<p>)
  onChange: (next: string) => void;     // commits the new value to formState.env[i]
  envKey: string;                       // the env KEY in this row (for sort-by-match)
  snapshot: SecretsSnapshot;            // shared snapshot, owned by AddServer
  onRequestCreate: (prefillName: string | null) => void; // opens form-level AddSecretModal
  // Optional
  disabled?: boolean;                   // mirrors the form's readOnly state
  ariaLabel?: string;                   // defaults to "Secret value picker"
}
```

The component renders:

- A wrapper `<div class="secret-picker-wrap">` containing:
  - The value `<input type="text">` (single source of truth for the raw value) with `role="combobox"`, `aria-expanded`, `aria-controls` pointing to the listbox id, `aria-activedescendant` referencing the currently highlighted option's stable id.
  - A 🔑 `<button class="secret-picker-toggle">` with `aria-label="Pick secret"` and `aria-haspopup="listbox"`.
  - Conditionally, a `<div role="listbox" class="secret-picker-dropdown" id="...">` when open. Each item is `<div role="option" id="..." aria-selected="...">`.
- Inline marker (red ⚠ or yellow ⚠) when ref state is `missing` or `unverified` — referenced via `aria-describedby` from the `<input>` to a hidden status text node ("Secret 'foo' not found in current vault").
- A `aria-live="polite"` status region for refresh errors ("Could not load vault state — Retry").

**Close mechanisms (Codex memo-R3 P2-2):** the dropdown closes through three explicit triggers, each with a concrete handler:

1. **Outside-click close.** A document-level `mousedown` listener (added on dropdown open, removed on close) checks if the click target is outside `.secret-picker-wrap`; if so, calls `setIsOpen(false)`. The 🔑 button is INSIDE the wrapper, so clicking it does not trigger this — the toggle's own click handler handles it.
2. **Tab-close on the value `<input>`.** An `onKeyDown` handler on the `<input>` detects `Tab` (with or without Shift): if dropdown is open, calls `setIsOpen(false)`; does NOT call `preventDefault`. Browser-default Tab behavior continues, so focus moves normally to the 🔑 button (forward) or the previous form element (Shift+Tab). A blur listener on the `<input>` is **not** sufficient for this case because focus moves to the adjacent toggle button inside the same wrapper, where the blur target is still inside the picker — `relatedTarget` would be the toggle button, not "outside".
3. **Esc-close on the value `<input>`.** Same `onKeyDown` handler detects `Escape` and calls `setIsOpen(false)`; calls `preventDefault` to avoid the dialog ESC behavior bubbling up if the picker is rendered inside a `<dialog>` (it isn't in A3-b, but defense in depth). Focus stays in the input.

The toggle button itself does not have a blur-close handler — clicking the toggle while the dropdown is already open closes it via the toggle's own click handler (`setIsOpen(prev => !prev)`).

The component does **NOT** maintain a separate filter query — the value `<input>` is itself the filter source. Typing further into the input narrows the dropdown by substring on `vaultKey` (case-insensitive after `normalizeForMatch`); the `secret:` prefix is stripped before filtering. This matches Codex memo-R1 P2 clarification of D6: SecretPicker owns "open/closed state, focused item index, and a derived filter expression computed from `value`" — there is no independent filter state. (E2E scenario 3 is updated accordingly: typing `secret:wolf` after auto-open narrows the dropdown to `wolfram_app_id`.)

The broken/unverified styling is applied by the picker via class on the `<input>` (`broken` or `unverified`). The picker does NOT render the border directly.

### 5.3 `BrokenRefsSummary` props

```ts
export interface BrokenRefsSummaryProps {
  vaultState: VaultState;               // "ok" | "missing" | "decrypt_failed" | "corrupt"
  brokenRefs: string[];                 // vault keys referenced but not present (only meaningful when vaultState === "ok")
}
```

Returns `null` when `vaultState === "ok" && brokenRefs.length <= 1`. Otherwise renders a compact summary line (CSS class `secret-broken-summary`) with copy per D3.

### 5.4 `parseSecretRef` helper

```ts
// lib/secret-ref.ts

export type RefKind = "secret" | "file" | "env" | "literal";

export interface ParsedRef {
  kind: RefKind;
  key: string | null;                   // for "secret"/"file": the key name; for "env": the var name; for "literal": null
}

export function parseSecretRef(value: string): ParsedRef {
  if (value.startsWith("secret:")) return { kind: "secret", key: value.slice(7) };
  if (value.startsWith("file:"))   return { kind: "file",   key: value.slice(5) };
  if (value.startsWith("$"))       return { kind: "env",    key: value.slice(1) };
  return { kind: "literal", key: null };
}

export function isSecretRef(value: string): boolean {
  return value.startsWith("secret:");
}

// hasSecretKey distinguishes "user is mid-typing the prefix" from "user
// has committed to a secret key". Empty key after the prefix means the
// picker should be open in browse mode but the row should NOT show a
// missing/broken marker yet. (Codex memo-R1 P2.)
export function hasSecretKey(value: string): boolean {
  return value.startsWith("secret:") && value.length > "secret:".length;
}
```

All three functions are pure and trivially testable.

### 5.5 Ref-state classifier

```ts
// inside SecretPicker, computed every render
type RefState =
  | "literal"     // no marker — value is not a secret ref or is just the editing prefix "secret:"
  | "present"     // no marker — vault has the key
  | "missing"     // red ⚠ — vault is ok but doesn't have the key
  | "unverified"  // yellow ⚠ — snapshot loading, snapshot error, or vault state not ok
  | "loading";    // no marker — initial snapshot fetch in flight (distinct from "unverified" so first paint doesn't flash yellow)

function classifyRefState(value: string, snapshot: SecretsSnapshot): RefState {
  // Editing prefix or non-secret literal: no marker.
  if (!isSecretRef(value) || !hasSecretKey(value)) return "literal";

  const key = value.slice(7);

  // Initial load (no successful fetch ever): suppress marker.
  if (snapshot.status === "loading" && snapshot.fetchedAt === null) return "loading";

  // Any error path → unverified. Per §5.1, the hook does NOT preserve
  // stale data on transition to "error" — `data` is always null in the
  // error variant. We cannot honestly classify present/missing without
  // current data. fetchedAt may be non-null (we used to have data) but
  // that is only used for "last loaded N minutes ago" copy, not for
  // classification fallback.
  if (snapshot.status === "error") return "unverified";

  // status === "ok": stale-while-revalidate keeps data populated even
  // during a background refresh. Trust it.
  if (snapshot.data.vault_state !== "ok") return "unverified";

  const found = snapshot.data.secrets.some(s => s.name === key && s.state === "present");
  return found ? "present" : "missing";
}
```

**Dropdown render order (single source of truth — D3 and §5.6 reference this table; Codex memo-R8 P3 + R9 P3 + R12 P3):**

**Snapshot-state precedence rule (Codex memo-R12 P3 + R13 P3):** snapshot state ALWAYS takes precedence over `RefState` for dropdown body composition. The exact resolution per snapshot-state combination:

| Snapshot state | Value shape | Dropdown body |
|---|---|---|
| `loading` (`fetchedAt === null`) | any | `[LD]` only |
| `error` (any `fetchedAt`) | any | `[RT]` only |
| `ok` + `vault_state !== "ok"` | secret ref with key (`hasSecretKey === true`) | `[CR]` (yellow `unverified` badge) → `[CN-disabled]` |
| `ok` + `vault_state !== "ok"` | literal OR `secret:` prefix only | `[CN-disabled]` only (no `[CR]` because there is no key to echo) |
| `ok` + `vault_state === "ok"` | any | RefState determines the dropdown shape per the per-state table below |

This makes literal + `decrypt_failed` unambiguous: dropdown body is just `[CN-disabled]` with the vault-not-ok tooltip; no Currently-referenced section. Vitest case 8 covers the secret-ref path; a new subcase 8b (added below) covers the literal path.

There are exactly four possible dropdown body slots. Each `RefState` selects an explicit sequence — never mix-and-match the slot definitions in the prelude with "last" wording. The per-state column "Render sequence" in the table below is the literal top-to-bottom order; the sequence is the answer (Codex memo-R9 P3).

Slot definitions (referenced from per-state sequences below):

```text
[CR]   "Currently referenced" section header + one row showing the value's existing key with the appropriate badge
[CRE]  "+ Create '<k>' in vault…" entry (contextual remediation; appears only with [CR])
[DIV]  divider line
[AV]   "Available secrets" section header + N rows (other vault keys, sorted by matchTier(vaultKey, envKey))
[CN]   "+ Create new secret…" entry (generic create, no specific key prefilled)
[RT]   "Retry loading vault" button + descriptive copy
[LD]   "Loading vault…" skeleton placeholder
```

| State | Tooltip on `<input>` | Render sequence (literal top-to-bottom) |
|---|---|---|
| `literal` | none | `[AV]` (only when vault non-empty) → `[CN]` |
| `present` | none | `[AV]` (matched key sorts via `matchTier` like any other; no "top" pinning) → `[CN]` |
| `missing` | "Secret '\<k\>' not found in current vault" | `[CR]` (red `missing` badge) → `[CRE]` → `[DIV]` → `[AV]` (other keys if any) |
| `unverified` (vault not ok) | "Cannot verify — vault \<vault_state\>" | `[CR]` (yellow `unverified` badge) → `[CN-disabled]` ("+ Create new (disabled — vault \<vault_state\>)") |
| `unverified` (snapshot error, any `fetchedAt`) | "Could not load vault state — Retry" announcement in `aria-live` region | `[RT]` only |
| `loading` (first paint, `fetchedAt === null`) | none (suppressed) | `[LD]` only |

**Currently-referenced section** (`[CR]`) appears ONLY for `missing` and `unverified` (vault not ok). For `present`, the existing referenced key is part of `[AV]` (sorted by `matchTier` normally). For `literal`, there is no referenced key.

**Create entry forms** in the table:

- `[CRE]` — "+ Create '\<k\>' in vault…" — specific to the missing key. Enabled when vault is in a state allowing creation (vault state `ok`) AND the key name is not in `RESERVED_SECRET_NAMES`. **Disabled form `[CRE-reserved-disabled]`** (Codex memo-R10 P3): when the missing key IS in `RESERVED_SECRET_NAMES` (currently `{"init"}`), this entry renders as disabled with tooltip "Cannot create — '\<k\>' is a reserved name (HTTP routing). Choose a different name." Click is a no-op. See R4 for the broader reserved-name policy and the disabled chip on legacy `init` entries that already exist in the vault.
- `[CN]` — "+ Create new secret…" — generic, appears at end of `[AV]` for `literal`/`present`. Always enabled (the modal's own client-side validation handles reserved names if the user types `init` into the modal — that path never pre-fills `init`).
- `[CN-disabled]` — same generic copy but disabled with tooltip stating reason. Used when `unverified` (vault not ok). Click is a no-op.

For `unverified` (snapshot error) and `loading`, NO Create entry of any form is rendered — see §5.6 for the gating-rule consequence.

**Per-state Create-entry resolution table** (Codex memo-R10 P3 explicit gating):

| State | Reserved-name applies? | Create entry rendered |
|---|---|---|
| `literal` / `present` | n/a (no specific key prefilled by `[CN]`) | `[CN]` enabled |
| `missing`, `k ∉ RESERVED_SECRET_NAMES` | no | `[CRE]` enabled |
| `missing`, `k ∈ RESERVED_SECRET_NAMES` | **yes** | `[CRE-reserved-disabled]` |
| `unverified` (vault not ok) | not applicable (no specific key targeted by `[CN-disabled]`) | `[CN-disabled]` |
| `unverified` (snapshot error) | n/a | NO Create entry — `[RT]` only |
| `loading` | n/a | NO Create entry — `[LD]` only |

### 5.6 Create-flow trigger gating (Codex memo-R1 P2)

A Create entry is **enabled** if and only if BOTH conditions hold (Codex memo-R11 P3 tightening — combines snapshot gate AND reserved-name gate):

```ts
const canCreate =
  snapshot.status === "ok" &&
  snapshot.data.vault_state === "ok";

// For contextual [CRE] entries that prefill a specific key, also check reserved names:
const canCreateContextual = (key: string) =>
  canCreate && !RESERVED_SECRET_NAMES.has(key);
```

The dropdown's body composition is authoritatively defined in §5.5's RefState table. §5.6 only adds the gating rule that picks the enabled-vs-disabled form §5.5 names per state:

- **`literal` / `present`** — §5.5 renders `[CN]` ONLY when the snapshot-state precedence rule resolves to "ok+vault-ok" (i.e., `canCreate === true`). If snapshot is loading/error/vault-not-ok, the snapshot-precedence rule (§5.5 prelude) overrides the RefState and renders `[LD]` / `[RT]` / `[CN-disabled]` accordingly. So when `[CN]` is actually rendered, `canCreate === true` is guaranteed and `[CN]` is enabled. The conjunction "`literal`/`present` ⇒ `canCreate === true`" only applies to the dropdown-rendering path that actually reaches `[CN]`; classifyRefState returning `"literal"` for a non-secret value while snapshot is loading does not contradict this — the snapshot precedence rule handles that case at the dropdown level.
- **`missing`** — §5.5 renders `[CRE]` for non-reserved keys (`canCreateContextual(k) === true`), or `[CRE-reserved-disabled]` when `RESERVED_SECRET_NAMES.has(k)`. The `canCreate` snapshot gate is always `true` for this state; the reserved-name gate is the binding constraint. Phrased differently: even though `canCreate === true`, an entry can still render disabled when the contextual reserved-name gate fails.
- **`unverified` (vault not ok)** — §5.5 renders `[CN-disabled]`. `canCreate === false` because `vault_state !== "ok"`. Reserved-name gate is moot here (`[CN-disabled]` is generic and has no specific key prefilled).
- **`unverified` (snapshot error)** — §5.5 renders no Create entry (`[RT]` only). Both gates fail (`canCreate === false`), but the gating has nothing to act on.
- **`loading`** — §5.5 renders no Create entry (`[LD]` only). `canCreate === false`. Same as above.

**Click semantics (covers ALL disabled forms — `[CN-disabled]` and `[CRE-reserved-disabled]`):** when the user clicks any enabled Create entry, `onRequestCreate(prefillName)` fires and D4 opens the modal. Clicking any disabled Create entry — whether `[CN-disabled]` (snapshot-gate failure) or `[CRE-reserved-disabled]` (reserved-name-gate failure with a `canCreate === true` snapshot) — is a no-op. The click handler checks the entry's enabled state computed from BOTH gates and returns early before `onRequestCreate` would fire. There is no path where the modal opens for a disabled entry, regardless of which gate disabled it.

The "Currently referenced" section, when shown for `unverified` (vault not ok) state, is rendered without consulting `snapshot.data.secrets[]` (which is non-authoritative when vault_state ≠ ok); it echoes the value's existing key as an `unverified`-badged item so the user can see what was previously selected and remove it. Selection of that item is allowed (it stays the same key); validation does not occur.

### 5.7 AddSecretModal hosting in AddServer (Codex memo-R2 P2-A: single-refresh-per-lifecycle)

```tsx
// inside AddServer component
const snapshot = useSecretsSnapshot();
const [createModalState, setCreateModalState] = useState<{ open: boolean; prefill: string | null }>({ open: false, prefill: null });

// savedFiredRef tracks whether onSaved (which already does a refresh) has
// fired during this modal lifecycle. If it has, the on-close refresh is
// skipped to prevent a stale-replacing-fresh race where a redundant
// refresh resolves AFTER the success refresh and degrades classification.
// Reset to false every time the modal opens.
const savedFiredRef = useRef(false);

function openCreateModal(prefill: string | null) {
  savedFiredRef.current = false;
  setCreateModalState({ open: true, prefill });
}

// rendered once at the bottom of the AddServer JSX, OUTSIDE the form sections:
<AddSecretModal
  open={createModalState.open}
  prefillName={createModalState.prefill ?? undefined}
  onSaved={async () => {
    savedFiredRef.current = true;
    // AddSecretModal awaits this before closing (§2.2 contract). The
    // dropdown will see the new key by the time the modal unmounts —
    // PROVIDED snapshot.refresh() resolves with status:"ok". Per §5.1
    // the hook never throws but can resolve into status:"error" (e.g.,
    // network drop on the GET /api/secrets after a successful POST).
    // In that case the picker transitions to status:"error" → unverified
    // until the user clicks Retry. The vault POST succeeded; only the
    // re-read failed.
    await snapshot.refresh();
  }}
  onClose={async () => {
    setCreateModalState({ open: false, prefill: null });
    // Skip refresh on the success path — onSaved already did it. On
    // Cancel / ESC / 409-conflict-dismiss paths, savedFiredRef is false
    // and we refresh so the dropdown reflects any externally-added entry
    // (e.g. the conflicting key from a 409).
    if (savedFiredRef.current) return;
    await snapshot.refresh();
  }}
/>
```

The modal is rendered exactly once per form. Each `<SecretPicker>` calls `props.onRequestCreate(...)` which the parent translates into `openCreateModal(...)`. This avoids racing `<dialog>.showModal()` calls.

The ref-based dedup ensures **exactly one `snapshot.refresh()` per modal lifecycle**:

| Path | onSaved fires | onClose fires | refreshes called |
|---|---|---|---|
| Save success | yes (sets ref=true) | yes (skips refresh) | **1** (from onSaved) |
| Cancel | no | yes (refreshes) | **1** (from onClose) |
| ESC | no | yes (refreshes) | **1** (from onClose) |
| 409 conflict, then Cancel | no | yes (refreshes) | **1** (from onClose) |
| Backend validation error, then Cancel | no | yes (refreshes) | **1** (from onClose) |
| Save fails inline → user fixes input → Save succeeds (Codex memo-R3 P3) | yes on the success attempt only (sets ref=true) | yes once at end (skips refresh) | **1** (from final onSaved); the failed first attempt does not call onSaved |
| Save succeeds but `snapshot.refresh()` resolves to `status:"error"` (Codex memo-R4 P3) | yes (sets ref=true), but the refresh resolved into error rather than ok | yes (skips refresh) | **1** (from onSaved, ended in error). Picker transitions to `status:"error"` / `unverified`; vault POST already succeeded; user can click Retry in the dropdown to recover. |

The race Codex flagged in memo-R2 P2-A — a redundant later refresh overwriting a fresh successful snapshot — cannot happen with this scheme because the success path skips the on-close refresh, and the close-only paths fire exactly one refresh.

`useSecretsSnapshot` itself uses a generation counter (`use-secrets-snapshot.ts:15`) that already drops stale fetch responses if a newer `refresh()` has started. That guarantees window-focus refetch concurrent with this flow doesn't degrade fresh state either.

## 6. Risks

### R1 — Snapshot staleness across the picker → modal → picker round-trip

If the user opens the picker, clicks Create, the POST `/api/secrets` succeeds, but the follow-up `GET /api/secrets` (triggered by `snapshot.refresh()`) fails (network drop), the dropdown must not continue showing the now-stale "missing" entry. Mitigation: the modal's `onSaved` awaits `refresh()`; per §5.1 `refresh()` never throws — it resolves with `status: "error"` set, `data: null`. The picker's `classifyRefState` (§5.5) returns `unverified` for any error state, so the row transitions to yellow `unverified` (NOT red `missing`), and the dropdown body switches to the Retry-only state per §5.6. The Save committed at the backend; the user can click Retry to re-fetch and confirm.

### R2 — Multi-tab vault edits

User opens AddServer, opens `Manage all secrets` in a new tab, deletes a secret in that tab, returns to the original tab. The `window.focus` listener in `useSecretsSnapshot` fires `refresh()`, which transitions any rows referencing that key to `missing`. This is correct behavior — the user's manifest now has a broken ref. The form does NOT auto-edit the value to drop the secret reference. Recovery: user picks a different secret or restores the deleted vault key.

### R3 — Form data model leakage

A future phase that wants typed env values (Q1 option B) will need to refactor `parseYAMLToForm` and `toYAML`. A3-b's stop-at-string contract means the picker does NOT introduce intermediate state that later refactors must accommodate. The escape hatch is `formState.env[i].value`, which remains the canonical truth.

### R4 — Reserved `init` name (Codex memo-R1 P2 corrected)

A3-a established `RESERVED_SECRET_NAMES = new Set(["init"])` to avoid HTTP route conflicts (`/api/secrets/init` vs `/api/secrets/<name>`). However, **a legacy `init` vault key can already exist** — the backend's reserved-name guard only blocks NEW writes via `SecretsSet`/`SecretsRotate` (per A3-a memo §4 D8 and `Secrets.tsx:12-18`); pre-existing vault entries created before the guard landed remain readable. `buildSecretRows` in `internal/api/secrets.go:337-338` includes such legacy keys in the snapshot.

A3-b's correct handling:

1. **Listing in dropdown:** if a legacy `init` key appears in `snapshot.data.secrets[]`, render it as a regular dropdown item (NOT filtered out). The user MAY want to reference it from a manifest (`secret:init` resolves at install time just fine — the backend resolution path doesn't consult the route-naming reservation).
2. **Visual treatment:** a legacy `init` row in the dropdown gets a small disabled-style chip "(legacy reserved name)" + a tooltip "This name is reserved for HTTP routing; new vault keys cannot use it. Existing entry may still be referenced." Selection still works (clicking populates `secret:init` in the value). The Secrets registry screen mirrors this treatment per A3-a precedent.
3. **`secret:init` ref classification:** `classifyRefState` checks `secrets.some(s => s.name === "init" && s.state === "present")` exactly like any other key — no special case. It classifies as `present` when the legacy entry exists.
4. **Create flow:** the dropdown's "+ Create" entry NEVER pre-fills name `"init"`. If `value === "secret:init"` and the key is missing (legacy was deleted but manifest still references it), the "+ Create '\<k\>' in vault…" entry shows as disabled with tooltip "Cannot create — `init` is a reserved name". A3-b adds a shared `RESERVED_SECRET_NAMES` constant in `lib/reserved-names.ts` (single export from one place) so both AddSecretModal's existing client-side validator (which Codex memo-R1 P2 noted is currently missing) and the picker can use it.
5. **AddSecretModal client-side validation gap (Codex memo-R1 P2):** the existing `AddSecretModal` does NOT currently reject reserved names client-side — it relies entirely on the backend's 400 response. A3-b adds a client-side check that mirrors the server-side guard, surfaced inline before POST. This is a small additive change in scope for A3-b and is listed in §8 commit 1 (helpers) since it co-locates with the new `lib/reserved-names.ts` extraction.

The "defense in depth filter out `init` from `secrets[]`" approach in rev 1 was incorrect — silent filtering would hide legacy keys that the user can legitimately reference. Surfacing them as visible-but-disabled-for-create is the right call.

### R5 — Auto-open trigger ambiguity

Typing `secret:` into the value field auto-opens the dropdown. But what if the user starts typing `secret:` to record a literal value that happens to start with that prefix? Mitigation: this is acceptable because (a) `secret:` is a reserved manifest prefix — typing it to mean a literal would be confusing in any other context; (b) ESC closes the dropdown without affecting the typed value; (c) the dropdown does not intercept further keystrokes — the user can keep typing into the value input even with the dropdown open.

### R6 — Dropdown dismissed on outside click vs intentional click on the modal

When AddSecretModal opens via the dropdown's "+ Create" entry, the dropdown closes (the click is outside the dropdown's bounds when the modal mounts). This is the correct sequence: the modal is now the focused surface; the dropdown's job is done. After the modal closes (Save or Cancel), focus returns to the value `<input>`. If the user wants to pick a different vault key, they re-open the dropdown via the 🔑 button.

### R7 — Edit mode + unsaved manifest + create + close

User opens `#/edit-server?name=memory`. Manifest has `value: secret:foo`, vault doesn't have `foo`. User opens picker, clicks "+ Create 'foo' in vault…", enters value, saves. Modal closes. Snapshot refreshes. Inline marker on this row disappears (now `present`). But the form is still dirty (manifest YAML on disk hasn't been re-saved — it didn't change because the value text is still `secret:foo`). The user's next save attempt writes the same `secret:foo` value to disk, but now the resolution succeeds at install time.

This is **the correct behavior** — A3-b explicitly does NOT mutate the manifest in this flow. The vault and the manifest are independent pieces of state; A3-b's modal only writes the vault.

### R8 — Embed bundle staleness

Like every frontend change, `internal/gui/assets/{index.html,app.js,style.css}` must be regenerated via `go generate ./internal/gui/...` before commit. Without this, the embedded bundle ships with stale code. Plan acceptance criterion includes a manual `go generate` step.

## 7. Testing

### 7.1 Vitest unit tests

**`secret-ref.test.ts`** (3 cases each function):
- `parseSecretRef("secret:foo")` → `{ kind: "secret", key: "foo" }`
- `parseSecretRef("file:bar")` → `{ kind: "file", key: "bar" }`
- `parseSecretRef("$HOME")` → `{ kind: "env", key: "HOME" }`
- `parseSecretRef("plain literal")` → `{ kind: "literal", key: null }`
- `parseSecretRef("")` → `{ kind: "literal", key: null }`
- `isSecretRef("secret:x")` → `true`
- `isSecretRef("file:x")` → `false`
- `isSecretRef("")` → `false`

**`name-match.test.ts`**:

- `normalizeForMatch("OPENAI_API_KEY")` → `"openai_api_key"`
- `normalizeForMatch("X-API-KEY")` → `"x_api_key"`
- `normalizeForMatch("foo")` → `"foo"`
- `matchTier("openai_api_key", "OPENAI_API_KEY")` → 0 (exact after normalization)
- `matchTier("openai_api_key_v2", "OPENAI_API_KEY")` → 1 (vault key extends env key)
- `matchTier("openai", "OPENAI_API_KEY")` → 1 (env key extends vault key)
- `hasSecretKey("secret:")` → `false` (editing prefix only)
- `hasSecretKey("secret:foo")` → `true`
- `hasSecretKey("secret:")` does NOT trigger missing/broken classification

**`SecretPicker.test.tsx`** (12 cases — case 8 + 8b):

1. Renders value `<input>` with current value, `role="combobox"`, `aria-expanded="false"`, `aria-controls` pointing to listbox id; 🔑 button has `aria-label="Pick secret"` and `aria-haspopup="listbox"`.
2. Clicking 🔑 opens dropdown (`aria-expanded="true"`); listbox has `role="listbox"`, items have `role="option"` with stable ids and `aria-selected` reflecting highlight; clicking outside closes it.
3. Typing `secret:` auto-opens dropdown; further typing filters items (case-insensitive substring on normalized vault key, with `secret:` stripped).
4. Dropdown sorts vault keys by `matchTier(vaultKey, envKey)` ascending; tier-0 item has `matches KEY name` badge; tier-1 sorts up but no badge.
5. Clicking a vault key item commits `secret:<key>` to `onChange` and closes dropdown; focus returns to value `<input>`.
6. Keyboard navigation (editable-combobox per D10): DOM focus stays on the value `<input>` throughout. ArrowDown opens dropdown highlighting first item (when closed) or moves highlight down with wrap (when open); ArrowUp opens highlighting last item / moves up with wrap; Enter selects highlighted; Esc closes; Tab closes + moves focus to 🔑 button. `aria-activedescendant` on `<input>` tracks highlighted option's id; options are NOT focusable.
7. With `value="secret:never_added"` and snapshot `status:"ok"` + `vault_state:"ok"` + key absent, value `<input>` has `broken` class and visible ⚠; `aria-describedby` points to a status node containing "Secret 'never_added' not found in current vault".
8. With `vault_state="decrypt_failed"` AND `value="secret:foo"` (secret ref with key), value `<input>` has `unverified` class; dropdown renders `[CR]` (yellow unverified badge) → `[CN-disabled]`; clicking `[CN-disabled]` is a no-op (no `onRequestCreate` call).
8b. **Literal + vault-not-ok** (Codex memo-R13 P3): with `vault_state="decrypt_failed"` AND `value="some literal"` (non-secret), value `<input>` has NO border class (literal — no marker per D3); dropdown renders `[CN-disabled]` only — NO `[CR]` because there is no secret key to echo. Verifies the snapshot-precedence resolution row "ok + vault-not-ok + literal" in §5.5.
9. With `value="secret:"` (just the editing prefix), classifyRefState returns `"literal"` — no marker, no broken classification.
10. With `snapshot.status="error"` (regardless of `fetchedAt` — null OR a previous timestamp from a successful load), dropdown body is replaced by a "Retry loading vault" button only; aria-live region announces the error. Two sub-cases verified: (a) `fetchedAt === null` (never loaded) — error message "Could not load vault state"; (b) `fetchedAt === <timestamp>` (post-success transient failure) — error message "Could not refresh vault state — last loaded {age}". Both sub-cases render the same dropdown body shape (Retry-only); the only difference is the descriptive copy (Codex memo-R4 P3 alignment between §5.6 and test wording).
11. **Reserved-name `[CRE-reserved-disabled]` rendering** (Codex memo-R10 P3): with `value="secret:init"`, `vault_state="ok"`, and `init` not in `snapshot.data.secrets[]` (i.e., classifyRefState returns `"missing"`), the dropdown's Create entry renders as DISABLED with copy "+ Create 'init' in vault…" and tooltip "Cannot create — 'init' is a reserved name (HTTP routing). Choose a different name." Clicking the entry is a no-op (no `onRequestCreate` call). Verifies §5.5 reserved-name gate.

### 7.2 Playwright E2E scenarios

Ten scenarios in a new `internal/gui/e2e/tests/secret-picker.spec.ts`. **Note (Codex memo-R1 P2):** `BLANK_FORM.env` starts as `[]` and the env section only renders the "+ Add environment variable" button initially. All AddServer scenarios first click that button to materialize a row before interacting with the picker.

1. **Empty form, vault empty** — go to `#/add-server`, click "+ Add environment variable" → enter `KEY=API_KEY`, focus value `<input>`, click 🔑 → dropdown opens with only "+ Create new secret…" item (no Currently-referenced section, no Available section).
2. **Empty form, vault seeded with two keys** — seed vault with `wolfram_app_id` and `unpaywall_email`. AddServer → add env row → KEY=`OPENAI_API_KEY`, click 🔑 → both keys appear, alphabetical (no exact match). Now seed an additional `openai_api_key` and re-open picker → that key sorts to top with `matches KEY name` badge (tier 0).
3. **Auto-open via `secret:` typing + filter narrowing** — AddServer → add env row → focus value `<input>`, type `secret:` → dropdown auto-opens. Continue typing `secret:wolf` → dropdown narrows to `wolfram_app_id` only (case-insensitive substring on normalized vault key, with `secret:` prefix stripped from filter). Press Enter → value becomes `secret:wolfram_app_id`, dropdown closes, focus returns to `<input>`.
4. **EditServer with broken ref, vault ok** — seed manifest `memory` with `env: { API_KEY: secret:never_added }`, vault has 1 unrelated key. Go to `#/edit-server?name=memory`. Row has red border + ⚠ icon; `aria-describedby` resolves to text "Secret 'never_added' not found in current vault". Open picker → "Currently referenced" section header → row with red `missing` badge → "+ Create 'never_added' in vault…" entry enabled.
5. **EditServer with multiple broken refs (count=3)** — manifest has 3 env vars referencing missing secrets. Compact summary line above env section reads `"3 secrets referenced but not in vault: never_added, also_missing, old_token. Daemons will fail to start."`. Per-row red borders.
6. **Create flow happy path** — start from scenario 4. Click "+ Create 'never_added' in vault…" → AddSecretModal opens with locked name "never_added". Enter value, click Save. Modal awaits `snapshot.refresh()` then closes. Row marker disappears (now `present`). Re-open picker — `never_added` now in "Available secrets" with no badge. Form's dirty flag is unchanged from before the modal interaction.
7. **Create 409 conflict** — start from scenario 4. Race-seed vault with `never_added` between page load and Save click (simulated by E2E fixture). Click "+ Create 'never_added' in vault…" → enter value → Save. Backend returns 409. Modal stays open with inline error "Secret already exists…". Click Cancel → modal `onClose` fires `snapshot.refresh()` → row's broken marker disappears (the pre-seed is now visible as `present`).
8. **Vault decrypt_failed scenario** — induce decrypt_failed (corrupt `.age-key` after first init). Open EditServer for manifest with `secret:foo`. Value `<input>` has yellow `unverified` border; tooltip "Cannot verify — vault decrypt_failed". Compact summary: "Vault not readable (decrypt_failed). Cannot verify any `secret:` references. Fix vault on Secrets screen first." Open picker → "+ Create new (disabled — vault decrypt_failed)" entry disabled with tooltip; click is a no-op (modal does NOT open).
9. **Editing prefix only** — AddServer → add env row → focus value, type `secret:` (just the prefix, no key). Dropdown auto-opens. Row does NOT show red border or ⚠ marker (`hasSecretKey()` returns false → classifyRefState returns `literal`). Continue typing `f` → dropdown narrows to keys starting with `f` (or substring match); no marker until user picks a key or types one not in vault.
10. **Save-success-but-refresh-fails** (Codex memo-R5 P3 + R6 P3) — start from scenario 4. Click "+ Create 'never_added'" → enter value → install a `route.fulfill` rule on the next `GET /api/secrets` to return 503. Click Save. AddSecretModal awaits `onSaved → snapshot.refresh()` which resolves to `status:"error"` (no throw per §5.1). Modal closes (per R6, closing the modal does NOT auto-reopen the picker dropdown). Row's value `<input>` transitions to `unverified` (yellow ⚠) class — NOT `broken` — because classifyRefState returns "unverified" for any error state. User reopens the picker (click 🔑) → dropdown body shows the Retry-only state (§5.6 row 4). Click Retry → next GET succeeds → snapshot transitions to `status:"ok"` → dropdown re-renders normally with `never_added` in "Available secrets"; row marker disappears (now `present`).

Total scenario count: 66 existing + 10 new = 76.

### 7.2.1 Lifecycle coverage matrix

Each `<AddSecretModal>` lifecycle path enumerated in §5.7 maps to specific test coverage (Codex memo-R5 P3):

| §5.7 lifecycle path | Vitest unit coverage | Playwright E2E coverage |
|---|---|---|
| Save success | `SecretPicker.test.tsx` case 5 (commit on click) + `use-secrets-snapshot.test.ts` (refresh stamps `fetchedAt`) | E2E scenario 6 |
| Cancel | `AddSecretModal` existing test (close on Cancel) | E2E scenario 7 fall-through (Cancel after error inline) |
| ESC | `AddSecretModal` existing test (ESC closes via native cancel event) | covered indirectly by scenario 7 |
| 409 conflict, then Cancel | `AddSecretModal` existing test (inline serverErr) + new ref-dedup test in AddServer | E2E scenario 7 |
| Backend validation error, then Cancel | same as 409 path (inline serverErr handling) | not separately tested — covered by 7 |
| Save fails inline → fixes input → Save succeeds | `AddSecretModal` test (re-submit after error clears serverErr); ref-dedup test asserts `savedFiredRef` only sets on success | not separately tested — strictly an internal-state test, equivalent to 7+6 in user-visible terms |
| Save success + refresh fails (Codex memo-R5 P3) | `use-secrets-snapshot.test.ts` (success-then-error transition) + `SecretPicker.test.tsx` case 10 (error renders Retry-only) | E2E scenario 10 (new) |

The "Save fails inline → fixes input → Save succeeds" path is **intentionally unit-tested only**. Its user-visible end state (a successfully-saved secret) is identical to scenario 6; the only thing the failed first attempt changes is the internal `savedFiredRef` accounting. A dedicated E2E would re-verify the happy path with a transient error in the middle, which adds CI time without finding new bugs the unit test wouldn't.

### 7.3 Manual smoke

Before merge:
- Run `npm test` in `internal/gui/frontend/` — Vitest passes.
- Run `npm run typecheck` — TypeScript clean.
- Run `npm test` in `internal/gui/e2e/` — all scenarios green (66 existing + 10 new = 76 total).
- Run `go test ./internal/gui/...` — embed smoke green.
- Manual flow per A3-a precedent: open AddServer, walk through scenarios 1–3 in a real browser; open EditServer with seeded broken-ref manifest, walk through 4 + 6.

### 7.4 Coverage tracker update

`CLAUDE.md` "What's covered" line gets a new bullet:

> - **Add/Edit server env picker:** 🔑 affordance button, auto-open on `secret:` typing with substring-narrowing filter, sort-by-match with `matchTier`-based badge for exact-after-normalization, broken-ref inline marker (red ⚠ for missing, yellow ⚠ for unverified), conditional compact summary line above env section when count > 1 or vault not ok, in-place AddSecretModal with snapshot revalidate-on-save and revalidate-on-close, "Manage all secrets (new tab)" escape hatch, full ARIA combobox semantics with keyboard navigation.

Total scenario count: 66 → 76.

## 8. Handoff to writing-plans

Once this memo is approved (after Codex iteration if needed), the writing-plans skill produces an implementation plan with the following commit shape (suggested 9 commits on a feature branch `feat/phase-3b-ii-a3b-env-secret-picker`):

1. **Helpers + reserved-names extraction (TDD)** — `lib/secret-ref.ts`, `lib/name-match.ts`, `lib/reserved-names.ts` (extracted from `Secrets.tsx` constant; both screens import from one place). Add client-side reserved-name validation to existing `AddSecretModal` (Codex memo-R1 P2). All Vitest. No UI rendering changes.
2. **`useSecretsSnapshot` D8a extension** — add `fetchedAt` field to all three union variants; export named `SecretsSnapshot` type. Existing `Secrets.tsx` unchanged (consumes only the legacy fields). New unit tests cover timestamp progression on success/error/refresh.
3. **`AddSecretModal` contract widening** — change `onSaved: () => void` to `onSaved: () => void | Promise<void>`; await it before close. Existing call site (`Secrets.tsx`) unaffected. Unit test covers async-saved completion ordering.
4. **`SecretPicker` component scaffold** — props, ARIA combobox semantics (role/aria-expanded/aria-controls/aria-activedescendant), dropdown markup with listbox+option roles, value `<input>` as filter source, no integration yet. Vitest cases 1–6 + 9.
5. **Ref-state classifier + inline marker + Create-flow gating** — `classifyRefState`, `hasSecretKey`, broken/unverified border CSS classes, snapshot-precedence rule (§5.5 prelude), two-gate disabled "+ Create" rendering (snapshot gate `canCreate === false` AND reserved-name gate `RESERVED_SECRET_NAMES.has(k)` per §5.6), `[CRE-reserved-disabled]` form, `aria-describedby` status nodes, aria-live region for refresh errors. Vitest cases 7, 8, 8b, 10, 11 (Codex memo-R12 P3 — case 11 verifies `[CRE-reserved-disabled]`; Codex memo-R13 P3 — case 8b verifies literal + vault-not-ok renders `[CN-disabled]` only, no `[CR]`).
6. **`BrokenRefsSummary` component** — conditional render + 4 vault-state copy variants per D3.
7. **AddServer integration** — replace plain `<input>` value with `<SecretPicker>`; lift `useSecretsSnapshot` + `<AddSecretModal>` to form level (single instance each); render `<BrokenRefsSummary>` above env section. AddSecretModal gets new wrapped `onClose` (refresh-on-close) and async `onSaved` (refresh-then-close). Manual dev-mode sanity.
8. **CSS** — picker dropdown chrome, broken/unverified borders, compact summary line, badges (`matches KEY name`, `missing`, `unverified`, `legacy reserved name`), aria-live status styling.
9. **E2E + asset regen + docs** — 10 Playwright scenarios; `go generate ./internal/gui/...`; CLAUDE.md "What's covered" update (count 66 → 76); backlog mark A3-b done.

Each commit is small, atomic, and CI-clean. Per project precedent, the PR gets multi-round Codex review before merge.

The plan must NOT push or open the PR without explicit user approval (project standing instruction).

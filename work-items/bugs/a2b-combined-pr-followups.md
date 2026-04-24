---
status: open
context: combined-PR review note
found-during: Phase 3B-II A2b Task 2/4 code-quality reviews
phase: Phase 3B-II A2b
---

# A2b findings to reconcile against user's separate security fixes at combined-PR time

The user is preparing security fixes in a separate GitHub branch/PR and wants a
combined PR with A2b. During A2b task-level code-quality reviews, a few
observations came up that are PRE-EXISTING A2a patterns but now repeat on new
A2b surfaces. Noting them here so they are not silently lost when we assemble
the combined PR.

None of these blocked the individual tasks because they are A2a-inherited
patterns, not A2b-introduced regressions. But the user asked for
production-grade work, so the combined PR should decide what to do with them
once the separate security fixes are in view.

## 1. ~~`writeAPIError` forwards raw `err.Error()` to the client on 500s~~ **FIXED in PR #16 (Codex R1)**

**Reviewer:** Task 4 code-quality (confidence 87) + Codex R1 #16 (P2)
**Surface:** `/api/manifest/get`, `/api/manifest/edit`, `/api/manifest/create` 500 paths
**Fixed in:** Combined PR #16 commit addressing Codex R1 findings

All three 500 paths now `log.Printf` the raw error server-side with name+op
context and return a sanitized `"internal error ..."` message to the client.
The 409 `MANIFEST_HASH_MISMATCH` path still passes through since its message
is generic.

Regression tests in `internal/gui/manifest_test.go`:
- `TestManifestGetHandler_500DoesNotLeakErrorDetails`
- `TestManifestEditHandler_500DoesNotLeakErrorDetails`
- `TestManifestEditHandler_HashMismatch_PassesThrough`
- `TestManifestCreateHandler_500DoesNotLeakErrorDetails`

All inject `*os.PathError`-style errors and assert the response body contains
neither the absolute path nor the raw error text, and does contain the
sanitized message.

## 2. `api.NewAPI()` called per-request in `realManifest*` adapters

**Reviewer:** Task 4 code-quality (confidence 85)
**Surface added:** `realManifestGetter.ManifestGetWithHash`,
`realManifestEditor.ManifestEditWithHash`
**Sibling existing:** `realManifestCreator.ManifestCreate` (A2a)

`api.NewAPI()` constructs a fresh `*API` including whatever EventBus wiring
lives in the package (the reviewer could not confirm from this commit whether
`newEventBus()` spawns a goroutine). The `api` package comment says "a single
instance is created per process" — the intent is process-wide, not request-
scoped.

If `newEventBus()` does spawn a long-lived goroutine that is never stopped,
each request on any of the four adapters is a goroutine leak.

Suggested remediation:
- Confirm whether `newEventBus()` is a goroutine.
- If yes, thread one shared `*api.API` onto `Server` and have all four
  adapters close over the same instance. Single diff touches all four.
- If no, document on `NewAPI` that per-request construction is cheap and
  intended to be safe, and leave the adapters as they are.

## 3. `/api/manifest/edit` handler test-coverage gaps

**Reviewer:** Task 4 code-quality (nits)

Three cases are not explicitly tested:
- 400 when `name` is empty on `/api/manifest/edit` (the get route has this
  test; the edit route does not).
- 400 on malformed JSON body (consistent omission with A2a create route, but
  the edit path is new surface).
- 405 on wrong method (GET against `/api/manifest/edit`; the get route has
  this test; the edit route does not).

These are nits because the happy path, the 409 hash-mismatch path, and the
500 other-error path are all tested. But if combined-PR review tightens test
expectations, this is the low-hanging fruit to fill in.

## 4. (Task 2 nits — already accepted as plan-verbatim tradeoffs)

- `&API{}` zero-value construction in tests (plan-verbatim; future-proofing
  would require shift to `NewAPI()`).
- `_, h1, _ :=` error-ignoring pattern in the second hash-change test
  (plan-verbatim).
- No missing-file test on `ManifestGetInWithHash` (not in plan scope).

Left as-is per plan-fidelity priority.

---

Review at combined-PR prep time: decide which of 1, 2, 3 to fold into the
combined PR vs defer, and which are genuinely covered by the user's separate
security work.

---

## FIXED: Codex R1 P1 findings (fixed in PR #16)

**Found during:** `@codex review` on PR #16 (combined A2b + 9 security PRs)
**Fixed in:** Same PR, commit addressing R1

### Finding 1 (P1) — `runReload` bypassed nested-unknown guard

The Reload path after a hash-mismatch banner fetched fresh YAML, parsed it,
and restored form state — but never called `hasNestedUnknown` again. If the
external write that caused the mismatch also introduced unsupported nested
fields, Reload left the form editable and a subsequent Save would silently
drop those fields.

Fix: `runReload` now mirrors the mount effect — calls `hasNestedUnknown`
on the fetched YAML, sets `readOnlyReason` when true, explicitly clears it
when false (so removing a problematic field via external edit + Reload
unlocks the form).

E2E regression: `Reload after external nested-unknown change enters read-only mode`
in `edit-server.spec.ts`.

### Finding 2 (P1) — Save in edit mode anchored to mutable identity/hash

`runSave` used `formState.name` and `formState.loadedHash`. Both are mutable:
`handlePasteYAML` overwrites formState including the name, and resets
`loadedHash` to `""`. So a mid-session Paste YAML in edit mode could make
the Save target a different manifest name with `expected_hash=""` — no
stale-write protection, wrong file overwritten.

Two-layer fix:
1. `runSave` and `runForceSave` in edit mode anchor to `editName` (URL-derived,
   immutable per session) for identity and `initialSnapshot.loadedHash`
   (set on Load/Save, never touched by Paste) as `expectedHash`. The merged
   payload is forced back to the target `name` before serialization so even
   if Paste slipped through, the written YAML matches the path.
2. `[Paste YAML]` button is disabled when `mode === "edit"` with an
   explanatory title. Users who need to replace a manifest wholesale should
   delete + recreate via Add Server.

E2E regression: `Paste YAML button is disabled in edit mode` in
`edit-server.spec.ts`.

---

## FIXED: BindingsMatrix readOnly (fixed pre-PR, same branch)

**Found during:** Final whole-branch review of `feat/phase-3b-ii-a2b-edit-mode`
**Fixed in:** Same branch, commit after `cc1ad6b`

`BindingsMatrix` (the 4+-daemon path inside `ClientBindingsSection`) did not
receive or propagate the `readOnly` flag. A manifest with 4+ daemons AND any
nested-unknown field would show a read-only banner and a disabled Save button,
but the matrix checkboxes and url_path inputs remained interactive. Users could
dirty the form without being able to save — violating the D13 invariant.

Fix: added `readOnly?: boolean` to `BindingsMatrix` props; applied
`disabled={props.readOnly}` to every `<input type="checkbox">` and
`<input type="text">` in each matrix cell; threaded `readOnly={readOnly}` into
the `BindingsMatrix` call site in `ClientBindingsSection`.

E2E regression added in `edit-server.spec.ts` (test 14): seeds a 4-daemon
manifest with `extra_config` nested-unknown, navigates to edit mode, expands
Client bindings, asserts every matrix checkbox and the first url_path input
are disabled. E2E count 43 → 44.

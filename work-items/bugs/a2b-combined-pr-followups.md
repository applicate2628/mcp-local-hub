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

## 1. `writeAPIError` forwards raw `err.Error()` to the client on 500s

**Reviewer:** Task 4 code-quality (confidence 87)
**Surface added:** `/api/manifest/get` and `/api/manifest/edit` 500 paths
**Sibling existing:** `/api/manifest/create` 500 path already has this
(internal/gui/scan.go writeAPIError behavior)

Backend errors on disk-bound calls can be `*os.PathError` — the error string
includes an absolute filesystem path. When forwarded through the JSON response,
this leaks the on-disk layout to the browser. For the edit flow, the absolute
path of the user's manifests/ directory becomes visible to any same-origin
caller that can trigger a 500 (e.g., by POSTing a name pointing at a non-
existent manifest or by exhausting FDs).

Suggested remediation (to be considered against the user's security fixes):
- Sentinel "internal error" message to the client; log the real error
  server-side with context (`name`, operation, pid).
- Applies to both new handlers and the pre-existing create handler (fix in
  one place if we choose to address).

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

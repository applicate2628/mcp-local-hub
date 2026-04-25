---
title: Rotate CTA and RotateResultBanner destroyed by loading flash on post-rotate refresh
severity: medium
found-by: qa-engineer
found-in-phase: Phase 3B-II A3-a Task 9
affected-surface: internal/gui/frontend/src/screens/Secrets.tsx + lib/use-secrets-snapshot.ts
context: feat/phase-3b-ii-a3a-secrets-screen
status: fixed
---

## Reproduction

E2E tests 7 and 8 in `internal/gui/e2e/tests/secrets.spec.ts` demonstrate this:

1. Navigate to `#/secrets` with vault containing K1 and 2 running daemons (alpha, beta).
2. Click Rotate on K1.
3. Fill a new value and click "Save without restart".
4. The PUT /api/secrets/K1 returns 200 (no-restart path).
5. `onSaved` is called, which calls `setBannerName("K1")`, `setRotateMode("no-restart")`, and `void props.refresh()`.

Expected: The `PersistentRotateCTA` banner appears with "Restart now" button.

Actual: The CTA appears momentarily (if at all) but is immediately destroyed when `props.refresh()` triggers `useSecretsSnapshot` to transition to `status: "loading"`, which causes `SecretsScreen` to render the loading branch (`<p>Loading…</p>`) instead of `<InitKeyedView .../>`. When `InitKeyedView` unmounts, all its local state (`bannerName`, `rotateMode`, `runningByServer`) resets to null/initial. When the refresh completes and `InitKeyedView` remounts, `rotateMode === null` so the CTA does not render.

Same defect affects `RotateResultBanner` (test 8: the 207 partial-failure banner disappears on the same refresh cycle).

## Expected vs actual

Expected: After rotate, the CTA/banner persists until the user interacts with it (Restart now or Dismiss), surviving the background refresh triggered by `onSaved`.

Actual: The CTA/banner is destroyed by the loading-flash from `useSecretsSnapshot.refresh()` and never reappears.

## Root cause

`useSecretsSnapshot.refresh()` transitions through `status: "loading"` before `status: "ok"`. `SecretsScreen` renders `<p>Loading…</p>` in the loading state instead of the current screen body, causing `InitKeyedView` to unmount and lose all local state.

## Fix options

Option 1 (preferred): Change `useSecretsSnapshot.refresh()` to NOT transition through `loading` when there is existing data — use a stale-while-revalidate pattern. Set `status: "ok"` with stale data while the refetch is in progress, then update to fresh data on completion.

Option 2: Move `bannerName`, `rotateMode`, `rotateResult`, and `runningByServer` state UP to `SecretsScreen` (the parent), so they survive the `InitKeyedView` unmount/remount cycle.

Option 3: Change `SecretsScreen` to keep rendering `InitKeyedView` during refresh (pass the last-known `snap.data` while loading), guarding only against the truly-empty initial state.

## Files involved

- `internal/gui/frontend/src/screens/Secrets.tsx:33-44` — early-return on `snap.status === "loading"` destroys `InitKeyedView`
- `internal/gui/frontend/src/lib/use-secrets-snapshot.ts:17-19` — transitions through loading on every refresh
- `internal/gui/e2e/tests/secrets.spec.ts:375,494` — tests 7 and 8 rewritten to assert the PUT request shape instead of CTA/banner visibility (pragmatic until fixed)

## Resolution

Fixed via Option 1 (stale-while-revalidate in `useSecretsSnapshot`).

In `refresh()`, the `setState` call now uses a functional updater: if the previous state is already `status: "ok"`, the state is kept unchanged (returning `prev`) so the snapshot stays mounted and `InitKeyedView` is not unmounted during the background refetch. The `status: "loading"` transition only fires on the initial fetch when there is no prior data.

E2E tests 7 and 8 in `internal/gui/e2e/tests/secrets.spec.ts` were restored to assert the actual UI behavior:

- Test 7: verifies the `[data-testid="rotate-cta"]` banner is visible after "Save without restart" with 2 running daemons, that "Restart now" triggers `POST /api/secrets/K1/restart`, and the CTA dismisses on success.
- Test 8: verifies the `[data-testid="rotate-banner-partial"]` banner is visible after a 207 response and that the "Retry failed restarts" button is present.

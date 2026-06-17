---
title: G3 capability-display Playwright E2E covers only empty/nav/refresh-URL paths
severity: low
found-by: codex deep-sec r10 qa lane on PR #144
found-on: 2026-05-08
project: mcp-local-hub
related-pr: #144 (G3 capability display, all unit-tested but minimal E2E)
status: closed
closed: 2026-06-17
---

# G3 needs populated-fixture E2E coverage

## Closure (2026-06-17)

Closed by adding `internal/gui/e2e/tests/populated-matrix.spec.ts` plus a
real on-disk seed fixture `internal/gui/e2e/fixtures/seeded-hub.ts`.

- **Populated matrix via the REAL backend (the primary gap):** the seed
  fixture writes a real `~/.cursor/mcp.json` into the per-test temp home
  BEFORE the binary starts, so the live Go `/api/scan` reads it off disk
  (full embed-bundle + backend roundtrip — exactly the wiring unit tests
  with mocked `fetchOrThrow` cannot see). Two rows are exercised: a
  manifested server (`memory`, bundled manifest) renders as an interactive
  main-matrix row with a cursor cell; a manifest-less server
  (`legacy-thirdparty`) surfaces in the read-only "Other MCP entries"
  expander. Verified the seed→scan wiring end-to-end against the
  production `api.Scan()` path (memory → manifested/can-migrate,
  legacy → non-manifested/unknown).
- **Capabilities cards + synthetic-source pill + partial-failures /
  redaction banner:** exercised via an injected `/api/health` response
  (page.route), covering the per-card render, the `synthetic-source-pill`,
  the `capabilities-partial-failures` banner, the failure-empty branch, and
  the backend-redacted error text surviving to the section-error list.
- **DEFERRED (architectural):** a REAL-backend (un-mocked) capabilities /
  redaction-banner e2e is not feasible under the current fixture.
  Capabilities/probes iterate live `DaemonStatus` rows and the e2e fixture
  pins `MCPHUB_E2E_SCHEDULER=none` (so `/api/status` is always `[]`); a
  seeded CLIENT config populates `/api/scan` but does NOT start a hub
  DAEMON, so no probe ever runs. Exercising the live redaction path needs a
  scheduler/supervisor test seam that injects a failing daemon — tracked
  alongside the CLAUDE.md "Real migrate/restart flows (needs populated
  client configs)" + scheduler-test-seam backlog items. The injected-response
  test above covers the banner + pill + redacted-text RENDER wiring, which is
  the portion reachable in the headless e2e environment.

## Current state

`internal/gui/e2e/tests/capabilities.spec.ts` ships 3 Playwright
tests:

1. Sidebar Capabilities link navigates to `#/capabilities` + h1.
2. Empty-state copy renders when no servers are installed.
3. Refresh button issues `/api/health?include=capabilities&refresh=true`.

The 31 unit tests in `Capabilities.test.tsx` cover every behavioural
contract via mocked `fetchOrThrow`, but those exercises don't touch
the real Go backend.

## Coverage gap

Six populated-fixture scenarios have unit-test coverage but NO E2E
exercise:

1. **Per-server cards rendering** with real probe + capability data.
2. **Partial-failures banner** when some daemons succeed and others
   fail (round-7 P2 fix).
3. **Failure-empty branch** when all probes fail (round-4 P2 fix).
4. **Synthetic-source pill** for lazy-proxy daemons (probe.source
   === "proxy-synthetic").
5. **Legend toggle** open/close interaction.
6. **Item-list expand/collapse** with multiple items in a section.

E2E exercises the full embed-bundle + Go-backend roundtrip; unit
tests stub `fetchOrThrow`. A populated E2E catches the wiring
issues that unit tests can't see (e.g. wire-type mismatches that
TypeScript missed, probe-row matching by server/daemon tuple).

## Risk

Low — unit coverage is thorough. CI E2E currently runs only on
empty-fixture mocks (`MCPHUB_E2E_SCHEDULER=none` returns []),
which limits the populated-fixture path entirely.

## Fix proposal

Add `internal/gui/e2e/tests/capabilities-populated.spec.ts` (or
extend the existing spec) with a fixture-mocking strategy:

- Use Playwright's `page.route('/api/health*', ...)` to inject
  a populated fixture covering 2-3 daemons with mixed states
  (one ok, one probe-failure, one synthetic).
- Assert per-card render + banner + pill + legend + section
  expand for each.

Effort: ~½d. Defer to a dedicated E2E expansion PR after G3 ships;
not a v0.3.0 blocker.

## Plan

- File against the broader Phase 3B-II E2E backlog work.
- Coordinate with future maintenance-section E2E (Cleanup-6 had
  a similar gap deferred).
- Consider extracting a fixture-builder utility in `e2e/fixtures/`
  so future E2E specs can compose populated `/api/health`
  responses without inline mocking.

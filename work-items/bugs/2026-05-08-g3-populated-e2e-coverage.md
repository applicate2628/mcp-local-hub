---
title: G3 capability-display Playwright E2E covers only empty/nav/refresh-URL paths
severity: low
found-by: codex deep-sec r10 qa lane on PR #144
found-on: 2026-05-08
project: mcp-local-hub
related-pr: #144 (G3 capability display, all unit-tested but minimal E2E)
---

# G3 needs populated-fixture E2E coverage

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

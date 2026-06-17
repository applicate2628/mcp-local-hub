---
title: CI workflow lacks bundle-freshness gate for internal/gui/assets/
severity: medium
found-by: codex deep-sec r10 reliability lane on PR #144 (G3 capability display)
found-on: 2026-05-08
project: mcp-local-hub
related-pr: #144 (filing as adjacent finding; not blocking G3 merge)
---

# CI bundle-freshness gate missing

## Reproduction

1. Edit any file under `internal/gui/frontend/src/` (TSX, CSS, types).
2. Forget to run `go generate ./internal/gui/...` before committing.
3. Push and observe CI on `.github/workflows/ci.yml`: it runs Go
   build/test only and PASSES even though the embedded
   `internal/gui/assets/app.js` is stale.
4. Operators downloading the binary get the old UI; new feature
   not visible despite the source code containing it.

The E2E `internal/gui/e2e/global-setup.ts` already enforces a
"working tree clean for `internal/gui/assets/` after `npm run
build`" check — but it runs only when E2E executes, NOT in the
default Go-test CI path.

## Risk

Medium. Symptom is a "feature merged but invisible to users"
mismatch. PR #144 (G3) and PR #143 (Cleanup-6) both required
`go generate` runs in their final phases — the discipline was
manual + per-phase plan-text reminders. A CI gate would mechanize
the rule.

## Fix proposal

Add to `.github/workflows/ci.yml` (or a new dedicated job):

```yaml
- name: Regenerate GUI bundle
  run: |
    cd internal/gui/frontend && npm ci && cd ../../..
    go generate ./internal/gui/...
- name: Verify bundle is fresh
  run: git diff --exit-code -- internal/gui/assets/
```

Place AFTER the `setup-go` and `setup-node` steps. Fails if the
regenerated assets differ from the committed ones. Operator's
local `go generate` becomes mandatory; CI catches the omission
deterministically.

## Constraints

User has standing directive that CI is intentionally manual-only
(workflow_dispatch). Adding a step to an EXISTING manual workflow
is consistent — the gate fires only when CI is manually triggered.
Does NOT add an auto-trigger on push.

## Plan

- Defer to v0.4.x or a dedicated CI hardening PR.
- User to confirm whether the gate should run on every manual CI
  invocation or only on a dedicated pre-tag check.
- Coordinate with the existing `internal/gui/e2e/global-setup.ts`
  pattern — DRY: extract the freshness check into a helper script
  invoked from both CI and E2E setup.

## Resolution (closed 2026-06-17)

Fixed-in: #315 (acf9f7f) — ci.yml lines 136-156 regenerate the embedded bundle + git diff --exit-code internal/gui/assets/

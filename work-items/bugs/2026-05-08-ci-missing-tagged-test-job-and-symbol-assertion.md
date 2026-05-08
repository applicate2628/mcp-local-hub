---
title: CI workflow missing -tags=test_state_path_env parallel test step and production-binary symbol assertion
severity: low
found-by: qa-engineer
found-in-phase: PR #134 final QA gate
affected-surface: .github/workflows/ci.yml
context: standalone
status: open
---

## Reproduction

```bash
grep -E "test_state_path_env|nm.*mcphub|daemonStateDirWithEnvFallback" .github/workflows/ci.yml
# Returns no matches.
```

## Expected vs actual

**Expected (plan §43 + §50, v13):** CI workflow runs `go test` with two tag profiles AND verifies the production binary excludes the env-fallback symbol:

1. `go test -count=1 -timeout 5m ./...` (default tags) — production-fail-closed path tested.
2. `go test -count=1 -timeout 5m -tags=test_state_path_env ./...` (env-fallback path tested).
3. Production-binary symbol assertion:
   ```yaml
   - name: Production binary symbol assertion
     run: |
       go build -o mcphub-prod ./cmd/mcphub
       if go tool nm mcphub-prod | grep -q 'daemonStateDirWithEnvFallback\|test_state_path_env'; then
         echo "FAIL: production binary contains test-only symbols"
         exit 1
       fi
   ```

**Actual:** `.github/workflows/ci.yml` lines 94-98 only run the default-tag invocation. No env-fallback test step. No symbol assertion.

## Risk

The plan calls this a defense-in-depth check against "developer forgets `-tags` discipline". The build-tag file structure split (`state_paths_prod.go` has `//go:build !test_state_path_env`, `state_paths_envfallback.go` has `//go:build test_state_path_env`) makes the symbol absence a compile-time guarantee for any normal `go build` invocation, so runtime risk of leaking env-fallback into production is low. The security boundary holds without the CI gate.

However, the explicit acceptance criterion from plan v13 §43 is unmet:

> Task 1.4 + CI job: BOTH default tags AND `-tags=test_state_path_env`; production `go build` MUST be without tag (compile-time exclusion verified)

## Files involved

- .github/workflows/ci.yml:94-98 (Test step) — needs to fan out into two parallel test invocations.
- .github/workflows/ci.yml:91-92 (Build step) — could be extended with the `nm` assertion right after.
- docs/superpowers/plans/2026-05-07-mcphub-watchdog.md:666-668 (plan §43) and :761-787 (plan §50) — source-of-truth requirements.

## Severity rationale

Low: build-tag separation already provides compile-time guarantee for symbol absence. The CI gate is additional defense-in-depth, missing but not blocking ship.

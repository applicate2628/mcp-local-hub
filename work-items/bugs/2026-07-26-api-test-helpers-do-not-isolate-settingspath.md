---
severity: medium
context: adjacent-finding (test-infra leak into the operator's live %LOCALAPPDATA%)
---

- **status:** open
- **found-by:** branch `fix/cursor-not-default-install` while closing the register client-scope findings.
- **surface:** `internal/api` test helpers vs `api.SettingsPath()`.

# `internal/api` state-isolation test helpers do not redirect `SettingsPath()`, so a test that touches settings writes the operator's REAL `gui-preferences.yaml`

## Symptom (reproduced this session)

A throwaway probe test in `internal/api` that called
`daemonIntentTestHelper(t)` + `preparePreflightBinaryChecks(t)` +
`installFakeScheduler(t, f)` — i.e. the standard isolation preamble used by the
`TestInstallParsedManifest*` family — and then called
`(*API).SetDefaultInstallClientNames([]string{"claude-code","cursor"})`
wrote

```text
clients.default_install: claude-code,cursor
```

into the developer's LIVE `%LOCALAPPDATA%\mcp-local-hub\gui-preferences.yaml`.

It was caught only because a pre-run SHA256 baseline of the live state dir had
been taken; the file was then restored byte-identical. Without that baseline the
change is silent, persistent, and would alter the behavior of the operator's
real `mcphub install` / GUI (it is a real operator preference, not scratch
state).

## Cause

The isolation helpers redirect the DAEMON state root, not the SETTINGS path.
They are two different resolvers:

- `daemonIntentTestHelper` (`internal/api/daemon_intent_test.go:31`) sets the
  package-level `daemonStateRootOverride`, which governs `DaemonStateDir()` —
  `supervisor-intent.json`, `supervisor-state.json`, etc.
- `SettingsPath()` (`internal/api/settings.go:22-34`) ignores that override
  entirely. It resolves from the ENVIRONMENT: `LOCALAPPDATA`, then
  `XDG_DATA_HOME`, then `os.UserHomeDir()`.

So any test that has isolated "the state dir" still resolves
`gui-preferences.yaml` to the developer's real profile. Every settings mutator
(`SetDefaultInstallClientNames`, `SettingsSet`, `SetLSPRouterDisabledClients`, …)
and every settings reader inherits the hazard.

The read side is the quieter half: a test that only READS settings silently
takes the operator's machine as input, so its result is host-dependent — it can
pass on the author's machine and fail in CI, or vice versa, with no visible
cause.

## Blast radius

Any `internal/api` test that reaches a settings read or write without its own
`t.Setenv("LOCALAPPDATA", …)`. ~50 test sites already do the redirect by hand
(`grep -c 'Setenv("LOCALAPPDATA"' internal/api/*_test.go`), which shows the
pattern is known — but it is opt-in per test rather than provided by the shared
isolation helper, so each new test re-litigates it and a miss is invisible.

This became materially more reachable on branch
`fix/cursor-not-default-install`: `effectiveClientBindings` now resolves the
operator's `clients.default_install` at call time, so the whole `register` path
reads `SettingsPath()` where it previously read a compile-time snapshot. That
branch mitigated its own surface by adding the redirect to `newRegisterHarness`,
but did NOT fix the general helper.

## Suggested fix

Fold the settings redirect into the shared isolation helper rather than leaving
it per-test — e.g. have `statePathsHelper` / `daemonIntentTestHelper` also
`t.Setenv("LOCALAPPDATA", root)` and `t.Setenv("XDG_DATA_HOME", root)` so
`SettingsPath()` lands inside the same hardened temp root the daemon state uses.
Tests that want a persisted preference then write it into that temp root
explicitly (as `TestRegisterBindings_HonorPersistedDefaultClientsOverride` and
`TestRegister_SupervisedHonorsPersistedDefaultClientsOverride` do).

Note `internal/api` has zero `t.Parallel()` call sites, so `t.Setenv` is
available in these helpers without restructuring.

A cheap regression guard: a `TestMain`-level or `t.Cleanup` assertion that the
real `SettingsPath()` was neither created nor modified during the package run.

## Not fixed here

Out of the approved change surface for the branch that found it (that branch
closes four register/install client-scope review findings). Filed per the
adjacent-findings protocol; the orchestrator decides priority.

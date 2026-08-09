---
status: open
date: 2026-07-26
slug: supervisor-liveness-probe-swallows-its-own-release
severity: high
affected-surface: supervisor singleton liveness probe
related-pr: PR #590
---

# Supervisor liveness probe swallows its own release

## Summary

The supervisor liveness probe treats successful `TryLock` acquisition as proof that no supervisor is running, then discards the probe handle's release result. An unlock failure can therefore be reported as the safe `not running` result while this process may retain the singleton leaf.

## Current source evidence

- `internal/api/supervisor_lock.go:245-260` implements the authoritative `TryLock` probe and returns `not running` after `_ = lk.Unlock()`.
- `internal/api/supervisor_lock.go:238-243` documents the fail-closed contract for acquisition errors, but no corresponding release-error channel exists.

## Expected vs actual

- Expected: an unconfirmed probe release produces the undeterminable error result so destructive callers continue to fail closed.
- Actual: the release result is discarded and the probe returns `(false, 0, nil)`.

## Scope

Keep this record open. This singleton probe is an independent owner and is outside PR #590's Registry migration.

## Terms and Abbreviations

- Liveness probe: the non-blocking singleton-lock check used to decide whether a supervisor is running.
- Fail closed: refuse an unsafe operation when ownership cannot be determined.

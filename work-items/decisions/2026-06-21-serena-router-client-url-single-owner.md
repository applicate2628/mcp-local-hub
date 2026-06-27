---
status: accepted
date: 2026-06-21
accepted: 2026-06-28
---

# Serena Router Client URL Single Owner

`SerenaRouterClientURL` is the single owner consumed by all serena client-URL writers and ownership checks: migrate write, install write, scan read/reconcile, and uninstall ownership. This change wires the last unwired writer, the install `BuildPlanWithOpts` write plane, so manifest-driven install planning cannot revert serena clients back to the legacy per-daemon URL.

Strategic follow-up (original wording): make the serena manifest router-native, deleting the legacy/dynamic split so the generic manifest flow no longer needs a serena-specific install gate.

## Strategic-clause disposition — REVISED to narrower (area-4, 2026-06-28)

The strategic follow-up was admitted under architect review a5920370 (accepted REVISE-to-narrower) and SHIPPED as the proportionate part; the deletion half of the original clause was REJECTED as a misread.

- SHIPPED — the catalog flip. `servers/serena/manifest.yaml` is now `kind: workspace-scoped` with a `daemon_template` (and no static `daemons[]`/`client_bindings`), so NEW installs are router-native from the start: a fresh host writes ZERO legacy per-daemon `9121` client entries and ZERO `runtime_spec` rows (the §7.1 gate stays inert until a workspace registers). The embed's `daemon_template` matches `EffectiveSerenaDaemonTemplate`'s built-in default exactly, so the in-memory dynamic-pool synthesizer is now an identity projection.
- KEPT — the migrate as an existing-host upgrade/recovery path. `mcphub migrate serena legacy-to-dynamic-pool` still owns the legacy→router cutover for already-installed hosts. Its no-op/cutover decision keys on the committed `supervisor-intent.json` (`serenaRuntimeIntentIsDynamicPool` — a nil-`runtime_spec` row ⇒ false ⇒ proceed), NOT on the catalog shape, so a legacy-intent host still gets the full reap+`runtime_spec` cutover even though the catalog now classifies as `already-migrated`. Deleting the split would strand every existing host.
- KEPT — the §7.1 spec-bearing-write gate. The "no serena-specific install gate" half of the original clause was a misread: the gate is GENERIC, keyed on `HasRuntimeSpecRow()` (any spec-bearing supervisor-intent write), not on serena-by-name. It is a spec-version split-brain guard against an older supervisor binary, independent of serena. The client-URL is ALREADY router-native (the dynamic-pool manifest carries no `client_bindings`; serena client routing is the `SerenaRouterClientURL` owner via the migrate reconcile), so no serena-specific install gate exists to delete.

This was a PROPORTIONATE change (catalog flip + classifier comment + tests), NOT the full split-deletion. The PROTECTED surfaces stayed 0-diff: the §7.1 gate (`install_parsed_manifest.go`), the interlock bypass token (`supervisor_lock.go`), the INTRODUCE cutover (`serena_auto_register.go`), the migrate driver LOGIC (`migrate_serena.go`), `HasRuntimeSpecRow` (`supervisor_intent.go`), the `SerenaRouterClientURL` owner (`serena_client_reconcile.go`), and the #400 router-native client write (`install.go`).

## Evidence

- Owner: `SerenaRouterClientURL(guiPort)` in `internal/api/serena_client_reconcile.go:63`; serena predicate `IsSerenaServer` in `internal/api/serena_client_reconcile.go:130`.
- Existing writer: migrate overrides serena client URLs via `SerenaRouterClientURL` in `internal/api/migrate.go:233` and sets relay-stdio `RelayURL` in `internal/api/migrate.go:263`.
- New writer: install plan consumes the same owner in `internal/api/install.go:1591` and skips the client write with a notice when `BuildPlanOpts.GUIPort` is zero in `internal/api/install.go:1592`.
- Scan/read owner: serena scan/reconcile treats router URLs as serena-owned via `IsSerenaRouterURL` in `internal/api/scan.go:1479`; router helpers live in `internal/api/serena_client_reconcile.go:73` and `internal/api/serena_client_reconcile.go:115`.
- Uninstall owner: uninstall ownership recognizes hub-owned serena router entries via `IsHubOwnedSerenaRouterEntry` in `internal/api/install.go:1287`; ownership helper lives in `internal/api/serena_client_reconcile.go:102`.
- Entry-point injection: CLI reads the live GUI pidport in `internal/cli/install.go:1198`; GUI routes pass their bound port in `internal/gui/install.go:156` and `internal/gui/install.go:212`.

## Related

- Bug: [serena-client-revert-on-manifest-sync](../bugs/2026-06-19-serena-client-revert-on-manifest-sync.md).

## Terms and Abbreviations

- CLI: Command-line interface.
- GUI: Graphical user interface.
- MCP: Model Context Protocol.
- URL: Uniform Resource Locator.

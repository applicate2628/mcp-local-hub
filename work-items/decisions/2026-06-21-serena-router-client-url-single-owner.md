---
status: proposed
date: 2026-06-21
---

# Serena Router Client URL Single Owner

`SerenaRouterClientURL` is the single owner consumed by all serena client-URL writers and ownership checks: migrate write, install write, scan read/reconcile, and uninstall ownership. This change wires the last unwired writer, the install `BuildPlanWithOpts` write plane, so manifest-driven install planning cannot revert serena clients back to the legacy per-daemon URL.

Strategic follow-up: make the serena manifest router-native, deleting the legacy/dynamic split so the generic manifest flow no longer needs a serena-specific install gate.

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

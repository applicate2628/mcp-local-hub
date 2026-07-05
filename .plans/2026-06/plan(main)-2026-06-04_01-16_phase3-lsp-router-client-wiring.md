# Phase 3 LSP Router Client Wiring Plan Snapshot

## Goal

Wire client configs to the per-language GUI LSP router during `mcphub setup`, migrate legacy per-project LSP entries to router URLs with backups, and provide rollback without touching deployment or live hub processes.

## Steps

- Completed: Inspect setup flow, client adapters, LSP manifest loading, GUI port settings, registry rows, and existing LSP router/register paths.
- Completed: Add failing hermetic tests for setup auto-wire, migration backup, rollback, and idempotency.
- Completed: Implement `internal/api` LSP router client reconciler using existing client backup/write APIs and preserving workspace registry rows.
- Completed: Wire `mcphub setup` to run the reconciler after bootstrap and add `--rollback-lsp-router` for config rollback/removal.
- Deferred: GUI LSP daemons Enable/Disable semantics; current UI is workspace-registration based and needs a separate frontend/API model change.
- Completed: Run narrow tests, `go build ./...`, and `go vet ./...`.

## Terms and Abbreviations

- LSP: Language Server Protocol.
- GUI: Graphical user interface.
- MCP: Model Context Protocol.

# Plan: Serena Router R2 Consumer Completion

Status: completed
Owner: main conversation
Date: 2026-06-19

1. Inspect existing serena router owners and consumer call sites with codegraph and language-server/tool discovery evidence.
2. Add red Go and TypeScript tests for relay-shape migration/uninstall/demigrate, live-port scan classification, and frontend per-cell routing.
3. Implement shared backend helpers in `internal/api/serena_client_reconcile.go`.
4. Update backend consumers: migrate relay URL, uninstall ownership, managed-entry backfill, scan live GUI port wiring.
5. Update frontend `routing.ts` to recognize serena router URLs per server row and pass server names through `collectServers`.
6. Run required verification and regenerate embedded GUI assets.

Outcome: PASS


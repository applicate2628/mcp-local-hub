# mcp-local-hub — Claude Code notes

This file documents developer workflows and conventions for this repo that
are load-bearing enough to be worth surfacing to the agent by default. Add
new sections as they become necessary.

## GUI frontend (Phase 3B-II onward)

The web UI lives under `internal/gui/frontend/` (Vite + TypeScript +
Preact). Built artifacts land in `internal/gui/assets/` and are
committed — `go build` does not require Node.

### Day-to-day frontend dev (hot reload)

```bash
# Terminal 1: Go backend with a fixed port so Vite proxy can target it.
go run ./cmd/mcphub gui --no-browser --no-tray --port 9125

# Terminal 2: Vite dev server on 5173 with /api → 9125 proxy.
cd internal/gui/frontend
npm install  # once
npm run dev
# Browse http://localhost:5173
```

### Build + smoke against embedded assets (what ships)

```bash
cd internal/gui/frontend
npm run build
cd ../../..
go run ./cmd/mcphub gui --no-browser --no-tray --port 9125
# Browse http://127.0.0.1:9125
```

### Regenerate the embedded bundle (CI + commits)

```bash
go generate ./internal/gui/...
```

This calls `npm run build` under `internal/gui/frontend/` and
overwrites `internal/gui/assets/{index.html,app.js,style.css}`. Always
rebuild before committing frontend changes so the embedded bundle
matches the source.

### Tests

- Frontend unit tests (pure helpers): `cd internal/gui/frontend && npm run test`
- Type-check: `cd internal/gui/frontend && npm run typecheck`
- Go-side embed smoke: `go test ./internal/gui/`

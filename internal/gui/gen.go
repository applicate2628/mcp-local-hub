// Package gui — build-time hook for the Vite frontend bundle.
//
// `go generate ./internal/gui/...` runs `npm run build` inside
// internal/gui/frontend/, which re-emits app.js, style.css, and
// index.html into internal/gui/assets/ where //go:embed assets/*
// (see assets.go) picks them up. The generated files are committed
// so `go build` keeps working without Node.js installed.
//
// Day-to-day frontend dev uses `npm run dev` inside
// internal/gui/frontend/ with its Vite proxy pointed at a running
// `go run ./cmd/mcphub gui --no-browser --no-tray --port 9125` —
// see CLAUDE.md for the exact workflow.

//go:generate npm --prefix frontend run build

package gui

// register_workspace is a probe fixture — NOT part of the shipped mcp-local-hub
// binary. It lives under an underscore-prefixed directory so `go build ./...`,
// `go vet ./...`, and `go test ./...` all skip it (documented go tool
// behavior: directories starting with "_" are ignored). Run it with
// `go run` from the repo root (it needs the module's internal/api package).
//
// It writes one serena workspace entry directly into a workspaces.yaml at the
// given path, bypassing the trust-gate + auto-register flow entirely (the
// same shortcut internal/gui/serena_router_test.go's
// TestSerenaRouter_RealResolverIntegration_RoutesPathArgToCorrectWorkspace
// uses via reg.PutSerena + reg.Save) — this is how the F3 end-to-end probe
// (see ../README.md) gets a REAL, already-registered workspace to prove a
// forwarded tool-call round-trips through both the GUI's port and the route
// daemon's port.
//
// IMPORTANT: run this against a REDIRECTED registry path only (a temp
// LOCALAPPDATA), never the operator's real ~/.local/bin fleet's registry.
package main

import (
	"flag"
	"fmt"
	"os"

	"mcp-local-hub/internal/api"
)

func main() {
	regPath := flag.String("registry", "", "path to workspaces.yaml (must be under a redirected/temp LOCALAPPDATA)")
	wsPath := flag.String("workspace-path", "", "workspace path to register (must contain a real .serena/project.yml marker)")
	port := flag.Int("port", 0, "upstream fake-daemon port")
	flag.Parse()

	if *regPath == "" || *wsPath == "" || *port == 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./work-items/active/2026-07-25-mcp-front-daemon/probe/_fixtures/register_workspace -registry <path> -workspace-path <path> -port <n>")
		os.Exit(2)
	}

	// CanonicalWorkspacePath matches how a real register/auto-register path
	// stores WorkspacePath — ResolveByPath's ancestor-walk compares against
	// this canonical form, so a non-canonicalized path here would silently
	// never match (the exact mistake this fixture's first draft made during
	// the F3 probe run: 503 "register workspace first" even though an entry
	// existed, because the raw un-canonicalized path did not match).
	canon, err := api.CanonicalWorkspacePath(*wsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "CanonicalWorkspacePath:", err)
		os.Exit(1)
	}

	reg := api.NewRegistry(*regPath)
	_ = reg.Load() // ok if the file doesn't exist yet

	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  api.WorkspaceKey(canon),
		WorkspacePath: canon,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          *port,
		TaskName:      `\mcp-local-hub-serena-probe`,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "PutSerena:", err)
		os.Exit(1)
	}
	if err := reg.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "Save:", err)
		os.Exit(1)
	}
	fmt.Println("registered", canon, "-> port", *port, "in", *regPath)
}

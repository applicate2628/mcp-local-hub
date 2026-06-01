package api

import (
	"context"
	"errors"
)

// Phase 5 — serena auto-register-on-miss.
//
// AutoRegisterSerenaWorkspace registers a serena workspace at runtime when a
// `/serena/mcp` tool call arrives for a project path that has a
// `.serena/project.yml` marker but no registered serena workspace yet. The GUI
// router (internal/gui/serena_router.go) calls this on the
// ErrWorkspaceNotFound branch, then forwards the original call to the
// freshly-spawned per-workspace daemon.
//
// CONTRACT (do not change without updating the router caller + the deps seam in
// internal/gui/serena_router.go's serenaRouterDeps.AutoRegisterFn):
//   - absPath: the tool-argument path the agent called serena from.
//   - returns the registered *WorkspaceEntry on success (router then forwards).
//   - returns ErrNotASerenaProject when absPath has no `.serena/project.yml`
//     ancestor marker (router → keep the existing 503 not-found; this is the
//     DoS bound — an attacker cannot register a path with no marker they own).
//   - returns ErrNoLanguages when the marker exists but lists no languages
//     (router → HTTP 422).
//   - returns any other error for install/spawn/readiness failure (router →
//     HTTP 503); the implementation MUST roll back the registry row so a failed
//     auto-register leaves no half-registered workspace.
//
// IMPLEMENTATION (Phase 5 Part B) composes, in order, under a per-wsKey
// concurrency guard so concurrent calls for the same path register exactly once:
//  1. ancestor-walk for `.serena/project.yml` (ErrNotASerenaProject if absent) +
//     read its `languages:` (ErrNoLanguages if empty);
//  2. registry write under flock with re-read idempotency (GetSerena → return
//     existing if a concurrent winner already registered; else AllocateSerenaPort
//     + PutSerena + Save), arming a remove-row rollback;
//  3. BuildInMemorySerenaDynamicPoolManifest + InstallParsedManifest with
//     Workspaces: reg.SerenaEntries() (the §7.1 gate now ALLOWS this live-add
//     because the prior intent already carries runtime_spec post-cutover);
//  4. DialSupervisorIPCReconcile(ctx, true) for an immediate spawn (not the 60s
//     IntentWatcher poll);
//  5. readiness probe on the allocated port's /mcp (verifyProxyReady pattern);
//  6. audit `workspace-auto-registered` {path, languages, port, ...}.
// Spawn is Windows-only (the supervisor start primitive is a no-op stub
// elsewhere); the decision/rollback logic is cross-platform.
func (a *API) AutoRegisterSerenaWorkspace(ctx context.Context, absPath string) (*WorkspaceEntry, error) {
	// Phase 5 Part B replaces this stub body with the composed flow above.
	return nil, errors.New("AutoRegisterSerenaWorkspace: not implemented (Phase 5 Part B)")
}

// ErrNotASerenaProject is returned by AutoRegisterSerenaWorkspace when the
// requested path has no `.serena/project.yml` ancestor marker, so it is not an
// auto-registrable serena project. The router maps this to the existing
// workspace-not-found response (NOT an auto-register) — it is the load-bearing
// DoS bound against registering arbitrary attacker-supplied paths.
var ErrNotASerenaProject = errors.New("serena auto-register: path is not a serena project (no .serena/project.yml marker)")

// ErrNoLanguages is returned by AutoRegisterSerenaWorkspace when the
// `.serena/project.yml` marker exists but declares no languages, so no serena
// descriptor can be synthesized. The router maps this to HTTP 422.
var ErrNoLanguages = errors.New("serena auto-register: .serena/project.yml declares no languages")

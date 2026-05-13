// client_write_init.go — wire the production secure-write pipeline
// into the client-adapter writer hook.
//
// `internal/clients/write.go` declares `WriteConfigFile` defaulted to a
// plain os.WriteFile wrapper so in-package adapter tests continue to
// work against `t.TempDir()` (which on Windows lives under %TEMP%'s
// Authenticated Users-readable DACL and would fail the
// SecureWriteClientConfig parent-dir gate).
//
// Production wires it to `secureWriteWithOperatorOpt` so every
// token-bearing rewrite — including the Phase 5 install reconciler's
// `mcphub-hub` aggregate entry — flows through the handle-relative,
// DACL-bound pipeline. The swap is a one-way override: package `api`
// is in the import graph of every production entry point (cmd/mcphub,
// internal/cli, internal/gui), so this init() always runs before any
// adapter call.
//
// Issue #161 P1 closure: the wrapper adds an operator-explicit
// fallback for corp-policy machines where the hardened gate would
// otherwise refuse ordinary install/migrate. The fallback never
// fires silently — operators must set the
// MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE env var to "1" / "true" first,
// and every fallback write logs a structured warn event via the
// hub-mcp event log.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"SecureWriteClientConfig sequence" + §"Bidirectional install
// reconciler".
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 5.1
// step 6 ("Route ALL adapter writes through SecureWriteClientConfig").

package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/clients"
)

func init() {
	clients.WriteConfigFile = secureWriteWithOperatorOpt
}

// AllowUnhardenedClientWriteEnv is the operator-explicit opt-in
// for the unhardened client-config write path. When set to "1" or
// "true" (case-insensitive) in the process environment,
// SecureWriteClientConfig failures that originate from the
// parent-dir DACL/mode gate fall back to a plain os.WriteFile with
// mode 0600. The fallback is logged via the hub-mcp event log so
// audit trails record the opt-in.
//
// The env var is intentionally verbose so operators can grep their
// shell profile / scheduler scripts for it before opting in.
const AllowUnhardenedClientWriteEnv = "MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE"

// secureWriteWithOperatorOpt is the cross-package writer that
// `clients.WriteConfigFile` resolves to in production. It first
// attempts the hardened secure-write; on
// ErrSecureWriteParentInsecure (Windows DACL or POSIX mode/owner
// rejection at the parent dir gate) it either surfaces a clearer
// error pointing operators at the opt-in env var, or — when the
// opt-in is set — falls back to a symlink-refusing os.WriteFile
// with mode 0600 and logs a warn event.
//
// Failure classes that propagate unchanged (the opt-in is scoped
// narrowly to the parent-dir gate):
//
//   - open temp, write, rename, post-rename verify
//   - pre-existing symlink/reparse-point at destination
//   - all non-gate hardened-write errors
//
// codex bot r1 P1 closure (PR #165): the original opt-in path used
// raw os.WriteFile, which silently follows symlinks. Even on the
// opt-in lane we MUST refuse to write through a pre-existing
// symlink/junction, otherwise an attacker on a shared host could
// pre-create a symlink at the destination and harvest the token-
// bearing content into a target of their choosing. The
// fallbackWriteRefusingSymlink helper Lstats first and rejects
// before opening.
func secureWriteWithOperatorOpt(path string, contents []byte) error {
	err := SecureWriteClientConfig(path, contents)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		return err
	}
	if !operatorAllowedUnhardenedClientWrite() {
		return fmt.Errorf("%w; this path can be unblocked by setting %s=1 (loses parent-dir DACL/mode protection — only set this if the host is under your sole control and corp-policy DACLs cannot be tightened)",
			err, AllowUnhardenedClientWriteEnv)
	}
	if logErr := LogHubMcpEvent("warn", "client-write-unhardened-fallback", map[string]any{
		"path":   path,
		"reason": "operator opt-in via " + AllowUnhardenedClientWriteEnv,
		"err":    err.Error(),
	}); logErr != nil {
		// Best-effort: never swallow the original failure path; the
		// fallback write still proceeds.
		_ = logErr
	}
	return fallbackWriteRefusingSymlink(path, contents)
}

// fallbackWriteRefusingSymlink writes contents to path with mode
// 0600 via a temp file + atomic rename. The opt-in lane's residual
// protections beyond the parent-dir gate:
//
//  1. Lstat the destination first (does NOT follow symlinks). If
//     a symlink is present at `path`, refuse outright. Otherwise
//     a hostile pre-existing symlink would redirect the write
//     after the rename.
//  2. Write to a fresh temp file in the same dir, Chmod it to
//     0600, then atomically rename over `path`. This avoids
//     inheriting permissions / explicit ACEs from a pre-existing
//     file at `path` — raw os.WriteFile would have kept the prior
//     mode bits intact on POSIX and the prior explicit ACEs on
//     Windows (the inheritance-from-parent ACEs remain regardless;
//     the operator accepted those by opting in).
//
// codex bot r1 P1 closure (PR #165): the original implementation
// used raw os.WriteFile, which silently followed symlinks.
// codex bot r2 P1 closure (PR #165): subsequent fix using
// os.WriteFile(path, ..., 0o600) preserved the pre-existing file's
// mode bits — temp+rename closes that channel.
//
// Residual gap vs the hardened path: a small TOCTOU window between
// Lstat and the temp+rename. Handle-relative ops would close it;
// the opt-in path documents the trade-off (operator must trust the
// host for symlink-swap races).
func fallbackWriteRefusingSymlink(path string, contents []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write to %s: destination is a symlink (the unhardened-client-write opt-in does NOT downgrade symlink refusal — remove or replace the symlink and retry)", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lstat %s before unhardened write: %w", path, err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mcphub-unhardened-write.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := tmp.Write(contents); err != nil {
		cleanup()
		return fmt.Errorf("write temp %s: %w", tmpPath, err)
	}
	// Defensive Chmod: CreateTemp may apply default umask on POSIX,
	// giving 0600 → 0600 already, but if a hostile umask widened
	// the bits the defensive call tightens them. No-op on Windows
	// (Chmod only toggles the read-only attribute there).
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp %s to 0600: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp to %s: %w", path, err)
	}
	return nil
}

// operatorAllowedUnhardenedClientWrite reports whether the operator
// has explicitly opted into the unhardened-write fallback via the
// AllowUnhardenedClientWriteEnv env var. Accepts "1" and "true"
// case-insensitively; everything else (including unset, "0",
// "false", "no", garbage) returns false.
func operatorAllowedUnhardenedClientWrite() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(AllowUnhardenedClientWriteEnv))) {
	case "1", "true":
		return true
	}
	return false
}

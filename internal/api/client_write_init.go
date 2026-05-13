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
// opt-in is set — falls back to plain os.WriteFile with mode 0600
// and logs a warn event.
//
// All other secure-write failures (open temp, write, rename,
// post-rename verify, symlink refusal, etc.) propagate unchanged —
// the opt-in is scoped narrowly to the parent-dir gate and does
// NOT downgrade other TOCTOU / symlink protections.
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
	return os.WriteFile(path, contents, 0o600)
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

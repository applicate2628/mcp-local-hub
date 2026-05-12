---
title: G4 Phase 1 optional cleanups deferred — MINOR-5/6/7 from code-quality reviewer pass
severity: low
found-by: code-quality reviewer pass on feat/g4-phase1-pre-gate
found-in-phase: G4 Phase 1 — Pre-gate + Write Hardening
affected-surface: internal/api/server_manifest.go, internal/api/secure_write_*.go, multiple Windows test files
context: feat/g4-phase1-pre-gate post-implementation review (reviewer returned PASS with 7 MINOR optional cleanups; 4 applied as feat/g4-phase1 cleanup commit; 3 deferred to follow-up)
status: open
label: phase-1-followup
---

## Background

The Phase 1 code-quality reviewer returned PASS with 7 MINOR optional
cleanups and the explicit comment "I would not return to REVISE — these
are taste-level polish, not blockers." Four cheap+worth-it cleanups were
applied as a single cleanup commit on `feat/g4-phase1-pre-gate`
(MINOR-1, MINOR-2, MINOR-3, MINOR-4 — dead interface decl, hex →
named constants, double-CloseHandle race tightening, POSIX sentinel
wrap with path/mode context). The remaining three (MINOR-5, MINOR-6,
MINOR-7) require structural work that does not belong inside a
"cleanup" commit on this branch.

This doc tracks the three deferred items so the recommendations are
not lost.

## MINOR-5 — Validation rule duplication across config + api layers

### What

The same validation rules (name regex, port range, kind whitelist,
language whitelist, etc.) are partly enforced in `internal/config/`
on read AND in `internal/api/server_manifest.go` `ValidateManifestStrict` /
`ValidateManifestCompat` on write. Two sources of truth for "what is
a valid manifest" risks divergence the next time a rule changes.

### Reviewer's recommendation

Pull the validation rule set into a single shared module (likely a
new `internal/manifest/validate.go` or `internal/config/rules.go`)
that both layers call. Config-side load and api-side write path agree
on one rule set; tests target the shared module directly.

### Why deferred

Cross-layer refactor. Touches `internal/config/` boundary in addition
to `internal/api/`. Outside the cleanup-commit scope, and a non-trivial
diff that wants its own review pass.

### Pointer

- Reviewer comment thread: feat/g4-phase1-pre-gate review pass on HEAD `4072020`.
- Affected files: `internal/api/server_manifest.go` (ValidateManifestStrict/Compat), `internal/config/` (whatever currently load-validates).

## MINOR-6 — 9-step EXPLICIT_ACCESS helper duplication across 3 test files

### What

The Windows-side test files synthesize hostile DACLs by constructing
9-step EXPLICIT_ACCESS structures (build SID → build trustee → build
access mask → call ACLFromEntries → call SetSecurityInfo → ... etc).
The same ~30-line block is open-coded in three test files.

### Reviewer's recommendation

Add a new test helper file like
`internal/api/hub_mcp_state_dacl_windows_testhelpers.go` (build-tagged
`//go:build windows`, in `package api_test` or `package api` testing
namespace) that exposes a `setExplicitAccessAllow(t, handle, sid,
mask)` helper. Tests call the helper once per ACE; the 9-step plumbing
lives in one place.

### Why deferred

Needs a new test-helper file with a build tag and helper API design
(which fields are caller-supplied, which are baked in). Tractable but
not a "cleanup commit" change — better as a focused test-refactor PR.

### Pointer

- Affected files (per reviewer): three of the Windows-tagged test files
  under `internal/api/`. Visible via `grep -l EXPLICIT_ACCESS
  internal/api/*_test.go`.

## MINOR-7 — `setRestrictiveDACL` failure leaves empty file briefly readable

### Bot affirmations

- Codex bot review on PR #155 HEAD `128e7c4` (round 9), P3 inline at
  `internal/api/secure_write_windows.go:146` — "Create temp file with
  final DACL in one syscall". Bot explicitly classified as P3 (lowest
  severity) and recommended the same `NtCreateFile` +
  `OBJECT_ATTRIBUTES.SecurityDescriptor` approach the
  architecture-reviewer flagged. Deferral rationale: still needs
  dedicated security-engineer review pass (per architecture-reviewer
  PASS verdict that classified this as "don't block on").

### What

In `secure_write_windows.go::secureWriteClientConfigImpl` step 5,
the temp file is created via `ntCreateRelative` with the inherited
default DACL, and `setRestrictiveDACL` is called immediately after.
If `setRestrictiveDACL` returns an error, the temp file exists on
disk with the parent's inherited DACL for the brief window between
`NtCreateFile` return and `setFileDeleteOnClose` cleanup. On a parent
dir broadened by GPO that has slipped past step 2's parent-dir-DACL
check (e.g., between two scans), an attacker could read the
zero-byte temp file. Window is sub-millisecond and the file is empty,
so risk is mostly theoretical, but the reviewer flagged it as a
"defense in depth" cleanup.

### Reviewer's recommendation

Plumb the restrictive DACL into the `NtCreateFile` call via
`OBJECT_ATTRIBUTES.SecurityDescriptor`. This atomizes "create with
restrictive DACL" into a single syscall. The current `SetSecurityInfo`
call after create becomes redundant and can be removed.

Cite: NtCreateFile docs + Project Zero issue 1369 (handle-acl
race on Windows file create).

### Why deferred

Requires building a `SECURITY_DESCRIPTOR` + `SECURITY_ATTRIBUTES`
structure in Go-side x/sys/windows types (currently we build the
DACL via `windows.ACLFromEntries`; the descriptor wrapper around
that ACL needs new wiring). Not trivial; deserves its own review
pass with security-engineer.

### Pointer

- Affected file: `internal/api/secure_write_windows.go`, function
  `setRestrictiveDACL` + the `ntCreateRelative` call at step 4.

## Triage suggestion

All three are low severity. MINOR-5 has the highest maintainability
value (rule divergence is a real category-bug risk). MINOR-7 has the
highest security value (race window, however brief). MINOR-6 is pure
test code hygiene — handle alongside any future Windows-DACL test
expansion.

Recommend: schedule as three independent small PRs on top of master
post-merge of `feat/g4-phase1-pre-gate`. None is blocking for Phase 2
work.

---
status: proposed
date: 2026-06-20
slug: draft-readiness-mirrors-write-gate-follow-up
---

# Extract draft-readiness write admission into one owner

## Context

`readinessDraft` in `internal/gui/readiness.go` currently re-derives the manifest
write gate for draft Add/Edit-server YAML: backend name validation, create/edit
existence checks, and strict manifest validation. This is a fifth spelling of an
admission decision adjacent to the accepted
`work-items/decisions/2026-06-19-admission-check-single-gate.md` decision.

The implementation works today because `ManifestValidateMode(Strict)` uses
`manifestValidationWarnings`, and `manifestValidationWarnings` is currently an
alias for `manifestBlockingWarnings`, the same blocking-warning owner used by
`ManifestCreateIn`, `ManifestEditIn`, and `ManifestEditInWithHash`.

## Proposed Decision

Extract a shared write-admission owner in a dedicated follow-up PR, for example
`CanCreateManifest` / `ManifestWriteAdmission`, and have both storage mutation
paths and draft readiness call it. This owner should cover the create and edit
cases without making the GUI readiness handler maintain a hand-listed copy of
the write gate.

This is related to, but narrower than, the accepted install-admission end state
in `2026-06-19-admission-check-single-gate.md`: this decision tracks the
save/write gate, not the install/spawn gate.

## Current Guard

PR #378 r15 does not perform the extraction. It adds a cheap anti-drift guard:
`TestReadinessHandler_DraftPOST_MirrorsManifestWriteGate` asserts that
`ManifestCreateIn` / `ManifestEditInWithHash` would-succeed results match
draft-readiness `Ready` for a small create/edit manifest corpus.

The temporary dependency is also pinned in comments at:

- `internal/gui/readiness.go` near the `readinessDraft` re-derivation.
- `internal/api/manifest.go` near `manifestValidationWarnings`.

## Out of Scope

Do not bolt the shared owner extraction onto PR #378. The extraction touches a
critical write path and should be reviewed as its own PR with focused tests.

## Terms and Abbreviations

- PR: pull request.
- GUI: graphical user interface.
- YAML: YAML Ain't Markup Language, the manifest file format.
- Admission: the decision that a create, edit, install, or spawn operation may
  proceed.

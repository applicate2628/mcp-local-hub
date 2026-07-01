---
status: closed
type: refactor
severity: P3
date: 2026-06-30
closed: 2026-07-01
origin: PR #468 round-4/5 churn analysis
---

# Single-owner for access-denied vault remediation text

## Resolution (2026-07-01)
Done in the same PR as the deep-review findings closeout. `classifyVault` now
threads the access-denied error out (`(state, keys, accessErr)`), and the
`/api/secrets` list response ships a single-owner `remediation` field
(`vaultAccessDeniedFix(accessErr)`, omitempty, populated only for
`access_denied`). The frontend `AccessDeniedView` renders `env.remediation`
verbatim (keeping only its intact-vault + confidentiality FRAMING paragraphs)
instead of re-authoring the icacls/chmod/repair-state-dacl guidance — collapsing
the Go↔frontend copies to one backend owner and ending the wording-drift defect
class. The generic `StateFileDACLRunbookPointer` (runtime daemon-launch path,
not vault-specific) is left as a distinct surface. Tests: Go
`TestSecretsListWithUsage_RemediationSingleOwner` + 2 frontend AccessDeniedView
cases (renders verbatim / falls back when omitted).

## Problem
The access-denied (DACL-refused vault) operator remediation text is duplicated
across THREE hand-maintained copies that drift independently:
1. Go runbook pointer — `internal/api/hub_mcp_state_dacl.go` (`StateFileDACLRunbookPointer`)
2. Go readiness/admission owner — `internal/api/secrets.go` (`vaultAccessDeniedFix()`)
3. Frontend — `internal/gui/frontend/src/screens/Secrets.tsx` (`AccessDeniedView`)

PR #468 rounds 4 + 5 were BOTH wording defects where a fix landed in one copy
but not the others (strict-mode-as-fix, repair-state-dacl-is-file-only-not-dir,
legacy-vault repair suggestion). Each bot round caught one copy lagging. This is
3-copy drift = wrong abstraction for shared operator guidance.

## Durable fix (recommended by the implementing agent)
Make the Go side the SINGLE OWNER of the remediation strings: a small
`RemediationGuidance` struct/constants with the variants (file-case vs dir-case,
canonical-vs-legacy vault location). Have `/api/secrets` ship the access-denied
remediation text/flags to the frontend so `AccessDeniedView` RENDERS
backend-owned guidance instead of re-authoring it. Collapses 3 copies → 1,
ends the wording-drift defect class.

## Scope note
Touches the `/api/secrets` response contract (new remediation field) — warrants
its own review. Not urgent: the current 3 copies are CONSISTENT as of #468
(whole-repo grep clean, all variants aligned); this is a maintainability fix to
stop FUTURE drift, not a live bug.

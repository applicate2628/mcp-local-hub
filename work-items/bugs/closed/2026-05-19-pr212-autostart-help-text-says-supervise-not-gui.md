---
title: autostart help text says "mcphub supervise" but shim now launches "mcphub gui"
severity: medium
found-by: qa-engineer
found-in-phase: PR #212 r6 QA review
affected-surface: internal/cli/autostart.go
context: feat/gui-supervisor-lifecycle
status: closed
closed-by: PR #212 r7 (commit 2833ee4) + PR #213 (architecture-cleanliness verification)
closed-at: 2026-05-19
---

# Closure note

Closed by PR #212 r7 commit 2833ee4. Three references in internal/cli/autostart.go to "mcphub supervise" replaced with "mcphub gui" via global Edit. Verified by build matrix exit 0 + manual `mcphub autostart enable --help` output check.

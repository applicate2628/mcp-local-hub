---
title: Stale pre-2026-06-18 workspaces.yaml (broad inherited DACL) is unreadable by the hardened registry read → serena -32001 until a Save() rewrites it
severity: low
found-by: backend-engineer
found-in-phase: harden serena workspace-registry write investigation (fix/harden-workspace-registry-write)
affected-surface: >
  internal/api/workspace_registry.go Registry.Load (readStateFileInodeAnchored
  file-DACL gate) consumed by `mcphub workspace list`, register's
  ListByWorkspace, and the serena router /serena/mcp; the on-disk
  %LOCALAPPDATA%\mcp-local-hub\workspaces.yaml file written by a
  pre-2026-06-18 binary on a broadened-DACL parent.
context: adjacent-finding
status: open
---

## Symptom (the report that triggered this investigation)

On a host whose `%LOCALAPPDATA%\mcp-local-hub` parent is broadened (codex
sandbox grants `Wave\CodexSandboxUsers` + an orphan SID `S-1-5-21-…-1857089675`
Modify), the on-disk `workspaces.yaml` carried that broad (Modify-class) DACL on
its OWN file. The hardened registry READ then refused it:

- `mcphub workspace list` → `read registry …\workspaces.yaml: {Access Denied}`
- `claude mcp get serena` → `! Connected · tools fetch failed`,
  `MCP error -32001: no serena workspace registered`, tools/list fails.

## Root cause (verified this session)

The registry WRITE (`Registry.Save` → `WriteStateFileBytesLockHeld` →
`SecureWriteClientConfig`) is ALREADY hardened on origin/master: it installs a
PROTECTED owner-only DACL (`D:P(A;;GA;;;<user>)(A;;GA;;;SY)(A;;GA;;;BA)`) on the
file HANDLE at create time (stripping inherited parent ACEs) and re-verifies the
DACL after the atomic rename. Verified empirically: the new regression test
`TestRegistrySave_BroadenedParent_PublishesOwnerOnlyAndReadable` drives the
production `Save()` through a parent dir carrying an inheritable Authenticated-
Users Modify ACE and the published file is owner-only + hardened-read-readable.

The write hardening landed 2026-06-18 (commits `007a86cf`, `e0b050ad`). So the
observed broad-DACL `workspaces.yaml` is a **stale file written by a
pre-2026-06-18 binary** (plain `os.WriteFile(0600)+os.Rename`, which inherited
the broadened parent DACL) that has not been re-written by `Save()` since the
hardening shipped. The READ-side file-DACL gate
(`readStateFileInodeAnchored` → `verifyWindowsDACLFromHandleWriteOrAdmin`)
refuses a WRITE/DAC/DELETE/Modify ACE granted to a non-allowlisted SID in
EVERY mode (including default-relax), because such a file is tampering-capable.

## No write-path self-heal — Save() also refuses first

`Registry.Save()` does NOT clear this condition. Before the hardened
owner-only write, `Save()` first reads the existing file to roll the `.bak`
backup via `readStateFileInodeAnchored(r.path)` and returns on any
non-missing error — i.e. it hits the SAME file-DACL refusal this work item
documents and aborts BEFORE reaching the hardened write. So register /
unregister / lifecycle / migrate paths that call `Save()` cannot rewrite the
file owner-only while the broad DACL stands; the write-hardening only keeps a
file owner-only once it is ALREADY readable. Both the cold READ
(`mcphub workspace list`, the serena router's startup `ListByWorkspace`, the
GUI weekly-membership read) AND the write's own backup-read fail loud with
`{Access Denied}` → the operator sees `-32001 no serena workspace registered`.
The only remediations are the manual icacls owner-only fix or the optional
owner-verified cold-read self-heal below — NOT a normal Save().

## Possible remediations (for the owner to prioritize)

1. Operator remediation already documented: the CLAUDE.md runbook "secret
   daemons exit 1 on a sandbox-broadened %LOCALAPPDATA%" — `icacls
   workspaces.yaml /inheritance:r` then re-grant owner-only. (No code change.)
2. OR a one-time self-heal on cold read: when `Registry.Load` hits the
   file-DACL WRITE/DAC refusal AND the file's OWNER is the current user, treat
   it as a recoverable stale-DACL condition — read the bytes inode-anchored,
   then immediately rewrite via the hardened `Save()` to tighten the DACL.
   (Review the trust implications: the gate exists precisely to refuse a file a
   non-allowlisted SID could have tampered with, so an owner-only-owner check
   is load-bearing before trusting the bytes.)
3. OR `mcphub setup` / install best-effort-tightens an existing broad-DACL
   `workspaces.yaml` (and siblings) on upgrade, the same way it tightens other
   migrated state files.

Closely related (same read-side gate, test angle):
`work-items/bugs/2026-06-29-e2e-membership-tests-dacl-reject-on-broadened-temp.md`.

## Scope note

This is NOT a defect in the current write path (proven owner-only). It is the
post-upgrade interaction between a stale pre-hardening file and the now-strict
read. Filed as adjacent-finding; the orchestrator decides priority. The
write/read symmetry is now pinned by the regression test added in this branch.

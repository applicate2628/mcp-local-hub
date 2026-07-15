# Closure — adopt-npx-orphans (absorb unmanaged direct-stdio MCP entries into the hub)

Closed: 2026-07-16

## Outcome — DELIVERED

Every deliverable this item owned shipped and deployed:

- **`mcphub adopt` (CLI + API)** — absorb an unmanaged direct-stdio client entry into a
  hub-managed manifest: secret routing, manifest create, per-client repoint to the hub URL,
  9300-9399 port allocator, signature-matched multi-client repoint, backup-collision-safe
  rollback, scoped symlink consent (#513/#516).
- **GUI "Adopt into hub" surface** (Discovery button gated on `adopt_supported`, preview modal,
  scoped symlink-consent as reviewed (client,path) data, fail-closed execute-error redaction,
  audit row) — #516, deployed 2026-07-08. Bot FULL PASS + fable independent security PASS.
- **Auto-reaper hardening** (A2 PR5 config-absence gate + snapshot fail-closed + reap_verdict
  presentation + identity-bind + specificity) — #520, plus both reaper-hardening bugs #521/#522
  (walk-uncertainty 3-state fail-closed + aggressive-token identity binding). Deployed + live-verified.
- **Anti-drift "unmanaged detected" GUI signal** — #523, deployed. Surfaces bypass servers so the
  operator can adopt them.
- **Phase-2 de-adopt / revert-to-native** (the separate item `2026-07-09-deadopt-hub-to-native`) —
  DELIVERED + CLOSED 2026-07-15 (#539-#550): `mcphub de-adopt <server>` (CLI + GUI affordance)
  atomically restores every adopt-owned client entry to its exact pre-adopt config + removes the
  hub manifest. Superseded the interim `mcphub uninstall --server X` + manual restore.

Blockers H2 (symlinked codex config write) and H5 (pipe-peer reaper-gate spike) were resolved
(scoped-consent write path #516; pipe-peer declared a dead-end for the reaper gate, adopt is the
primary fix — decision `2026-07-08-pipe-peer-unreliable-reaper-gate.md`).

## Post-delivery stabilization (2026-07-16 consilium qualification)

After the v0.7 adopt/de-adopt/reap/forget stream landed, the fable+Sol consilium ran a release
qualification over this surface: the two open gate-ON bugs were closed
(`hub-reconcile-gate-on-zero-binding-stale-aggregate` FIXED; `classify-dead-adopting-row-gate-on-blind`
data-loss verified already closed by #551, downgraded to a classifier-imprecision residual), and
the missing end-to-end `adopt → de-adopt` lifecycle round-trip test was added (PR #554).

## Residual (NOT this item — cross-referenced only)

The status header's "D P2a/P2b GUI" is a cross-reference to the **per-project-GUI** initiative's
approval-surface work (decision `2026-06-24-per-project-gui-design.md`: P2a = Model-B path-reparam
clients registry; P2b = the `~/.claude.json`-only approval reader, tracked as the deferred non-bug
adjacent-finding `backlog/2026-06-25-p2b-approval-surface-claude-json-only.md`). Those belong to the
per-project-GUI initiative, not to adopt-npx-orphans; nothing this item owns is left open.

## Archive location

`work-items/archive/2026-07/2026-07-05-adopt-npx-orphans/`

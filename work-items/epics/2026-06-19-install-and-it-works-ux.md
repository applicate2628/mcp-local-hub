---
status: active
---

# Epic: install-and-it-works — clean-install + hub-launch + per-project UX

## Goal

mcphub должен ставиться и **просто работать из коробки**. На чистой машине
каждый MCP-сервер либо работает сразу, либо даёт **явный actionable запрос**
(не криптовый "handshaking failed HTTP 502"). Видно и управляемо что
подключено — **и глобально, и per-project**, явно в GUI. Прямой запуск
`mcphub.exe` (Explorer double-click) поднимает gui+tray.

Agreed direction (operator, 2026-06-19):

- **Все 6 областей равноценны** — не одна-первая, делаем все.
- **Deps-политика: DETECT + явный guided prompt.** Когда серверу нужен
  отсутствующий launcher / runtime / ключ — НЕ ставим молча, НЕ авто-инсталлим;
  детектим, показываем точную команду/шаг + кнопку, user подтверждает.
  "Из коробки" = ноль гадания + явное согласие, а не молчаливый авто-act.
- **Per-project: поддержать ВСЕ три модели** проекта — workspace-папка
  (serena/LSP), client-config-проект (project-scope .mcp.json/.claude.json),
  и группа (/g/ namespace).
- **`.exe` напрямую → gui+tray** (уже: `cmd/mcphub/main.go` shouldAutoLaunchGUI;
  verify + harden onboarding).

## Children (areas — each becomes a work-item)

1. (active) **readiness-core** — per-server readiness-check (launcher + runtime
   + required secrets + port) → actionable diagnostics; wire into install
   preflight so a missing dep yields a guided "here's the exact fix" instead of
   a bare `%v` / downstream 502. The DETECT substrate for everything else.
2. **env-secrets-onboarding** — surface manifest-declared env/secret
   requirements at install ("этот сервер нужен secret X — задай сейчас");
   link to Secrets screen. Manifests ALREADY declare env (wolfram →
   `secret:wolfram_app_id`), so this is surfacing + guiding, not new schema.
3. **symlink-client-config** — detect a symlinked client config (codex
   config.toml → OneDrive), resolve the real target safely OR prompt "это
   symlink на X, писать?" — instead of the current cross-device / refuse-symlink
   cryptic failure.
4. **serena-out-of-box** — dynamic-pool as the DEFAULT serena install shape
   (no separate fragile `migrate serena legacy-to-dynamic-pool`); fix the
   client-config revert (bugs/2026-06-19-serena-client-revert); per-workspace
   "just works".
5. **trusted-folders-ux** — explicit workspace-trust prompt (VS Code style)
   for LSP/serena roots (`lsp-trusted-roots.json`), visible + persisted, instead
   of the opaque current gate with no add-UX.
6. **per-project-gui** — GUI surface to SEE which MCPs are active per project
   AND toggle them, across all three project models; keep the global Servers
   matrix. The biggest new feature.

## Sequencing (areas equal in importance; this is a build order, not a priority)

readiness-core first (it is the DETECT substrate the deps-policy needs and it
directly kills the cryptic-502 pain), then 2/3/4/5 (the specific hub-launch
fixes, each independently shippable), then 6 (per-project GUI — the largest,
builds on the workspace registry + groups + client-config scopes the earlier
areas touch).

## Non-goals (for now)

- Auto-installing system toolchains (gdb/lldb/clang) — that stays the MSYS
  installer backlog; readiness-core only DETECTS + guides for them.
- Remote/HTTP MCP onboarding (G6) — separate track.

## First slice (this PR)

readiness-core backbone: `CheckServerReadiness(manifest) → ReadinessReport`
(launcher-on-PATH, runtime presence for uvx/npx/node, required-secret
resolvable in vault, port free) with per-requirement {ok, reason, fix} and an
actionable string the install preflight emits in place of the bare error.

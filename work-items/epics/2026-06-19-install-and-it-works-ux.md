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

1. (DONE) **readiness-core** — per-server readiness-check (launcher + runtime
   + required secrets + port) → actionable diagnostics. #377 built the DETECT
   substrate (`AdmissionCheck`/`CheckServerReadiness`); the install-preflight
   surfacing was completed by area 2 (the `Preflight` Fix field was being
   thrown away). DONE 2026-06-21.
2. (DONE) **env-secrets-onboarding** — surface manifest-declared env/secret
   requirements at install. Done as the install-readiness SURFACING layer:
   CLI (#407 — typed `AdmissionError` carries the Fix; blockers hard-stop,
   optional-secret advisories proceed; `--check`) + GUI (#408 — Catalog
   pre-install readiness panel: blockers disable Install + show Fix, optional
   secrets set inline via the reused AddSecretModal + `?key=` deep-link). DONE 2026-06-21.
3. (MOSTLY DONE) **symlink-client-config** — both named deliverables MERGED:
   PR-1 (#409) closed a LIVE TOCTOU in the shipping
   `MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK` lane (resolve-to-string-then-re-walk →
   handle-pinned resolve-and-write), hardened by follow-ups #414 (full-path pin /
   consent-bypass close) + #415 (component-walk closes the intermediate-component
   TOCTOU) + #416 (bound symlink target reads). PR-2 (#410) shipped the guided
   consent UX — GUI resolve-symlink-and-write endpoint + Servers confirm
   affordance + CLI `[y/N]` default-N. Operator decisions applied: Supplement
   consent model + CLI default-N + explicit GUI enable. RESIDUE: the relax
   residual is an ACCEPT-DISCLOSE posture (documented, not pending code) — no open
   symlink-area work item remains.
4. (MOSTLY DONE) **serena-AND-lsp-out-of-box** — serena auto-introduces
   dynamic-pool on first `/serena/mcp` call (no manual migrate on the happy
   path); client-revert (#400), idle-stop + stale-session races (#386), and the
   env overlay (#403) all fixed. RESIDUE: the shipped serena manifest is still
   `unified-intermediate` on disk, so a crash/legacy path still needs manual
   `migrate serena legacy-to-dynamic-pool` — the router-native manifest is a
   PROPOSED decision (`decisions/2026-06-21-serena-router-client-url-single-owner.md`).
5. (NEXT — premise corrected 2026-06-21) **trusted-folders-ux** — the original
   "opaque gate with no add-UX" premise is STALE: a full trusted-roots GUI panel
   (`SectionTrustedRoots.tsx`), a `mcphub setup --trusted-root` flag, auto-bless
   on `mcphub register`, and `/api/lsp/trusted-roots` GET/POST/DELETE already
   exist. The REAL remaining gaps are narrower: (a) a first-touch interactive
   "do you trust this folder?" prompt, (b) a standalone `mcphub trust` verb,
   (c) extend trust-gating to serena roots (currently LSP-only).
6. (DONE) **per-project-gui** — GUI surface to SEE which MCPs are active per
   project AND toggle them, across all three project models; keep the global
   Servers matrix. The biggest new feature; Model B (project-local `.mcp.json`)
   was built from zero. Shipped P1→P3c: read-only Projects screen (#428),
   P2a + P2b approval-surface backend (#431 + #432), then the write phase —
   P3a writes-backend + `/api/projects` aggregate (#433), P3b frontend toggle +
   both claude scopes + deps-consent (#434), P3c group↔project binding (#435).
   Full PR chain: #428 + #431 + #432 + #433 + #434 + #435. Design accepted in
   `decisions/2026-06-25-per-project-gui-p3-design.md`. DONE 2026-06-27,
   live 8c359a1c.

## Sequencing (areas equal in importance; this is a build order, not a priority)

readiness-core first (it is the DETECT substrate the deps-policy needs and it
directly kills the cryptic-502 pain), then 2/3/4/5 (the specific hub-launch
fixes, each independently shippable), then 6 (per-project GUI — the largest,
builds on the workspace registry + groups + client-config scopes the earlier
areas touch).

> **Status (2026-06-27):** area 6 (per-project-gui) is DONE/shipped (PRs
> #428 + #431 + #432 + #433 + #434 + #435, live 8c359a1c).
> - **DONE:** 1 (readiness-core), 2 (env-secrets-onboarding), 6 (per-project-gui).
> - **MOSTLY DONE:** area 3 (symlink-client-config — both named PRs merged
>   #409/#410 + hardening #414/#415/#416; residual is the accept-disclose relax
>   posture, no open work item), area 4 (serena-AND-lsp-out-of-box — happy path
>   shipped; residual = shipped-manifest `unified-intermediate` on disk + a
>   PROPOSED router-native decision).
> - **NEXT (real open work):** area 5 (trusted-folders-ux — three narrowed gaps
>   a/b/c: first-touch trust prompt, `mcphub trust` verb, serena-root trust-gating).
>
> The epic STAYS `status: active`. Area 5 is the only child with genuine
> unstarted scope; areas 3 and 4 carry documented residual (a posture and a
> proposed-decision + manifest-state item) rather than fully closed. Close the
> epic only when 3's posture is final, 4's residual lands, AND area 5 ships.

## Non-goals (for now)

- Auto-installing system toolchains (gdb/lldb/clang) — that stays the MSYS
  installer backlog; readiness-core only DETECTS + guides for them.
- Remote/HTTP MCP onboarding (G6) — separate track.

## First slice (this PR)

readiness-core backbone: `CheckServerReadiness(manifest) → ReadinessReport`
(launcher-on-PATH, runtime presence for uvx/npx/node, required-secret
resolvable in vault, port free) with per-requirement {ok, reason, fix} and an
actionable string the install preflight emits in place of the bare error.

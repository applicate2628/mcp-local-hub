---
status: closed
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
3. (DONE — closeable; residual is a documented posture) **symlink-client-config** — both named deliverables MERGED:
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
4. (DONE — defect fixed; router-native rewrite DEFERRED) **serena-AND-lsp-out-of-box** — serena auto-introduces
   dynamic-pool on first `/serena/mcp` call (no manual migrate on the happy
   path); client-revert (#400), idle-stop + stale-session races (#386), and the
   env overlay (#403) all fixed. RESIDUE: the shipped serena manifest is still
   `unified-intermediate` on disk, so a crash/legacy path still needs manual
   `migrate serena legacy-to-dynamic-pool` — the router-native manifest is a
   PROPOSED decision (`decisions/2026-06-21-serena-router-client-url-single-owner.md`).
5. (DONE) **trusted-folders-ux** — the original "opaque gate with no add-UX"
   premise was corrected 2026-06-21 (a full trusted-roots GUI panel
   `SectionTrustedRoots.tsx`, a `mcphub setup --trusted-root` flag, auto-bless
   on `mcphub register`, and `/api/lsp/trusted-roots` GET/POST/DELETE already
   existed). The three narrowed remaining gaps all SHIPPED in #437: (a) a
   first-touch interactive "do you trust this folder?" prompt (NEEDS_TRUST),
   (b) a standalone `mcphub trust` verb, (c) trust-gating extended to serena
   roots (the serena trust-gate + bless co-design). DONE 2026-06-27, live
   7b7699e2.
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

> **Status (2026-06-28): all children DONE or deferred — epic CLOSED.** See
> `## Closure` below for the per-area outcome and the deferred follow-ups.
>
> - **DONE:** 1 (readiness-core), 2 (env-secrets-onboarding), 5
>   (trusted-folders-ux — #437), 6 (per-project-gui — #428/#431/#432/#433/#434/#435).
> - **CLOSEABLE (residual is a documented posture, not pending code):** area 3
>   (symlink-client-config — both named PRs #409/#410 + hardening #414/#415/#416
>   merged; the accept-disclose relax is a namespace-rights POSTURE mitigated by
>   `MCPHUB_REQUIRE_SINGLE_USER_HOME=1`; the TOCTOU bug is resolved).
> - **DEFECT FIXED, strategic rewrite DEFERRED:** area 4 (serena-AND-lsp-out-of-box
>   — happy path live-verified; the core client-revert defect is fixed by #400;
>   the router-native-manifest rewrite is a separate strategic follow-up, NOT an
>   epic blocker).

## Non-goals (for now)

- Auto-installing system toolchains (gdb/lldb/clang) — that stays the MSYS
  installer backlog; readiness-core only DETECTS + guides for them.
- Remote/HTTP MCP onboarding (G6) — separate track.

## First slice (this PR)

readiness-core backbone: `CheckServerReadiness(manifest) → ReadinessReport`
(launcher-on-PATH, runtime presence for uvx/npx/node, required-secret
resolvable in vault, port free) with per-requirement {ok, reason, fix} and an
actionable string the install preflight emits in place of the bare error.

## Closure

Historical closed date: 2026-06-28

All six areas plus the two per-project follow-ups (D1, D2) are DONE or
explicitly deferred; no child carries genuinely-unstarted in-epic scope. The
goal — mcphub installs and just works out of the box, with what is connected
visible/manageable globally and per-project in the GUI — is met on the happy
path, with the remaining items being a documented security posture (area 3) and
two strategic/permanent deferrals (area 4 router-native rewrite, D1).

Per-area outcome:

- **Area 1 — readiness-core: DONE** (#377 DETECT substrate; surfacing completed
  by area 2). 2026-06-21.
- **Area 2 — env-secrets-onboarding: DONE** (CLI #407 + GUI #408). 2026-06-21.
- **Area 3 — symlink-client-config: CLOSEABLE (posture-mitigated).** Both named
  PRs #409/#410 + hardening #414/#415/#416 merged. The TOCTOU bug
  (`bugs/2026-06-21-symlink-optin-toctou-string-rewalk.md`) is resolved
  (`status: resolved-by-this-PR`, 2026-06-21). The remaining accept-disclose
  relax is a documented namespace-rights POSTURE — not pending code — mitigated
  by `MCPHUB_REQUIRE_SINGLE_USER_HOME=1`. Decision
  `decisions/2026-06-21-symlink-client-config-scoped-consent.md` promoted
  proposed → accepted (code merged + verified).
- **Area 4 — serena-AND-lsp-out-of-box: DEFECT FIXED, rewrite DEFERRED.** Happy
  path live-verified (serena on 9125). The core client-revert defect is fixed by
  #400 (live-proven); the tracking bug
  `bugs/2026-06-19-serena-client-revert-on-manifest-sync.md` is mostly-resolved.
  Only a crash/legacy path still needs manual `migrate serena
  legacy-to-dynamic-pool`. The router-native-manifest REWRITE is a separate
  PROPOSED strategic decision
  (`decisions/2026-06-21-serena-router-client-url-single-owner.md`), DEFERRED as
  a follow-up — NOT an epic blocker.
- **Area 5 — trusted-folders-ux: DONE** (#437, live 7b7699e2): `mcphub trust`
  verb + serena trust-gate + bless co-design + NEEDS_TRUST first-touch prompt.
  2026-06-27.
- **Area 6 — per-project-gui: DONE** (#428/#431/#432/#433/#434/#435, live
  8c359a1c). 2026-06-27.
- **D1 — claude Local-scope toggle: PERMANENT/SEPARATE DEFERRAL** (READ-ONLY in
  v1; the catastrophic `~/.claude.json` corruption surface makes write-back a
  standalone effort). Not an epic close-condition — decision
  `decisions/2026-06-27-per-project-gui-p3b-uxdesign.md:32`.
- **D2 — cold object-member re-enable: DONE** (#439, live 5fbef07c): embed-only
  catalog-by-name prefill + shipped-server edit-shadow notice + clobber-guard,
  secret-safe by construction. Backlog
  `backlog/closed/2026-06-25-p3b-reenable-value-source.md` resolved by #439.

### Deferred follow-ups (tracked outside this epic)

- **Area 4 router-native manifest rewrite** — `decisions/2026-06-21-serena-router-client-url-single-owner.md` (PROPOSED strategic decision).
- **D1 claude Local-scope write-back** — permanent/separate deferral per `decisions/2026-06-27-per-project-gui-p3b-uxdesign.md`.
- **D2 stash-full-restore** — `backlog/2026-06-27-d2-stash-full-restore-deferred.md` (the deeper restore beyond #439's catalog-by-name prefill).
- **Embed-first install shadows same-named disk manifest** — `bugs/2026-06-28-embed-first-install-shadows-disk-manifest.md` (pre-existing adjacent finding; deeper override-precedence decision deferred).

Closed: 2026-08-08T22:58:13Z
Outcome: Pre-V1 terminal status `closed` is preserved during operator-authorized V1 physical migration.
Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `f30b1701be126db9120d244f60138bc61d8110206b6f86392b83cc7b0e958b82`; original terminal status `closed`; explicit operator-authorized V1 migration.
V1-Migration-Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `f30b1701be126db9120d244f60138bc61d8110206b6f86392b83cc7b0e958b82`; original terminal status `closed`; explicit operator-authorized V1 migration.

# PR #288 cumulative review — parking state

PARKED 19:21 UTC → resume 22:21 UTC (cron 073b98f1; user: "припаркуйся на 3 часа,
потом продолжай (ждем обновление лимитов)"). master bcfe284 (r17 заземлён);
полл r18 (b7fqi7b2e) добегает в фоне — бот к 19:21 ещё НЕ ревьюил bcfe284; на
resume: если вердикта нет — ретриггер. Раунды: r14=5 → r15=1 → r16=3 → r17=2.

(пред. парковка) PARKED 14:32 UTC → resume 17:02 UTC (исполнена).
State at park: master c14a8cd запушен (= r11 + internal sweep S/A/B/C + интеграция +
D1-D3 + r12 + r13). Полл r14 (bicqow8yk) крутится на c14a8cd — если вердикт придёт
во время парковки: ЗАПИСАТЬ, НЕ действовать до 17:02. Codex-only окно тоже до ~17:10
(fable точечно — исключение). После resume: вердикт r14 → PASS = закрыть #288 (НЕ
мержить) → REDEPLOY-бандл → финал-чистка; находки → codex r14-раунд по паттерну.

## (старая парковка — reboot 2026-06-11 ~12:00 UTC, исполнена)

Template: review (REVIEW-ONLY PR — закрыть, НЕ мержить)
Orchestrator: main conversation
State: PARKED-FOR-REBOOT, ждём bot r11 verdict

## Где мы

master = b83c709 (запушен). PR 288 раунды r1-r10 пофикшены и на master.
PR 289 СМЕРЖЕН (10146b3). 4 codex-security PR (284-287) смержены ранее.

Раунды #288: r5=3 наход. (51e4873) → r6=3 (bfc469d) → r7=2+тест-гигиена
(6fa9f55) → r8=2 (522ac09) → r9=1 PID-fallback (17834f1) → r10=4
install/wake (b83c709). Бот ретриггернут на b83c709 в ~11:57 UTC
(issuecomment-4680212747), полл был bn2esofsa (умрёт с ребутом — НЕ важно).

## Resume после ребута (точная последовательность)

**r11 ВЕРДИКТ УЖЕ ПОЛУЧЕН до ребута (12:01 UTC): 2 P1 на b83c709.**
Находки: .scratch/codex-prompts/pr288-r11-findings.txt
Готовый промпт: .scratch/codex-prompts/pr288-r11-fixes.md
(F1 = Preflight не признаёт supervisor-owned порт → reinstall падает;
F2 = legacy-task cleanup не убивает процесс → орфан держит порт → quarantine.
ОБА бьют в редеплой — фиксить ДО редеплоя.)

0. СРАЗУ: `git worktree add .claude/worktrees/codex-288r11 -b codex-288r11-fixes master`
   → диспатч codex по готовому промпту (паттерн п.2 ниже) → верифай → squash
   на master → push → ретриггер → полл r12. Шаг 1 ниже НЕ нужен (вердикт есть).

1. (исполнено до ребута) Полл r11 на HEAD=b83c70921a866af836c8b27c792f81148e39de25:

   ```bash
   H=b83c70921a866af836c8b27c792f81148e39de25
   gh api repos/applicate2628/mcp-local-hub/pulls/288/reviews --paginate | jq -r --arg sha "$H" '.[] | select(.user.login=="chatgpt-codex-connector[bot]") | select(.commit_id==$sha) | "\(.state) @ \(.submitted_at)"'
   gh api repos/applicate2628/mcp-local-hub/pulls/288/comments --paginate | jq --arg sha "$H" '[.[] | select(.user.login=="chatgpt-codex-connector[bot]") | select(.original_commit_id==$sha)] | length'
   ```

   ВНИМАНИЕ: если master сдвинулся (новый фикс-раунд) — фильтровать по НОВОМУ
   `git rev-parse HEAD` (KOSYAK: local HEAD после push, не gh headRefOid).
   PASS-правило CLAUDE.md: no-major-issues/👍/APPROVED на ТЕКУЩЕМ HEAD +
   0 inline на HEAD + inline не ПОСЛЕ summary.

2. Находки → codex-фикс-раунд (паттерн из этой сессии):
   worktree `git worktree add .claude/worktrees/codex-288rN -b codex-288rN-fixes master`,
   промпт в .scratch/codex-prompts/pr288-rN-fixes.md (см. r5-r10 как образцы:
   точные сайты+фиксы, verification-блок, state-safety блок с SHA),
   `cd worktree && codex exec - -c model_reasoning_effort=xhigh < prompt > out 2> err`
   (timeout 2400000, run_in_background), верифай САМ (build×3 GOOS + vet +
   narrow тесты + gofmt touched + state SHA), `git merge --squash` на master,
   commit, push, `git branch -f v0.6-core-review master && git push -f`,
   `gh pr comment 288 --body "@codex review"`, новый полл.

3. PASS → ЗАКРЫТЬ #288 (`gh pr close 288` — review-only, НЕ мержить!) →
   REDEPLOY-бандл → финал-чистка → закрыть этот work-item (closure.md +
   archive). Затем PUSH master уже сделан — редеплой остаётся.

## REDEPLOY-бандл (после закрытия #288)

1. `bash build.sh` (из d:/dev/mcp-local-hub, master)
2. stage SAME-DIR: `Copy-Item .\build\mcphub.exe C:\Users\dima_\.local\bin\mcphub-deploy.exe` (cross-volume MoveFileEx fail → стейдж рядом с target)
3. batch-stop ВСЕХ serena-proxy + workspace-proxy демонов (иначе port-verify timeout whack-a-mole)
4. `mcphub install --upgrade` (флага --reset-failure-windows НЕ существует)
5. Удалить 4 стейл-таски v0.4.x: `\mcp-local-hub-memory-default`, `\mcp-local-hub-fetch-default`, `\mcp-local-hub-serena-unified`, `\mcp-local-hub-watchdog` (ОСТАВИТЬ `\mcp-local-hub-supervisor` + `\mcp-local-hub-liveness` + `-workspace-weekly-refresh`). Это лечит "вечный рестарт" (логон-таски спавнят демонов ДО супервизора → port-fight 9123).
6. Kill орфаны старых тасков (memory был PID 20424 на 9123 — перепроверить netstat)
7. Full supervisor restart (kill старый supervisor+gui+tray; `schtasks /Run /TN \mcp-local-hub-supervisor`)
8. gate#0: `claude mcp list` (serena/LSP/memory/fetch √), `mcphub status` честный running+PID, fetch client-config 9121→9133. НЕ верить "Quarantined" от старого кода — проверять `claude mcp list`.

## Финал-чистка (после редеплоя)

- Ветки remote: v0.6-core-base, v0.6-core-review, codex-288r5..r10-fixes (локальные ветки тоже), codex/fix-vulnerability-* остаток если есть
- Worktrees: .claude/worktrees/codex-288r5, r6, r7, r8, r9, r10, codex-289, agent-* призраки (`git worktree prune` + remove)
- .scratch/state-backup-* (старые бэкапы; svc-intent эталон: 12851 bytes SHA256 6D25E865D0A35C83D2FE4907E5AFC20E9748563C96687D5022EE90B053D537D9 — ПОСЛЕ редеплоя intent изменится легитимно, бэкапы больше не эталон)
- Дефер드: cleanup.go:268 ToLower index-mismatch (кириллица в cmdline → panic) — проверить актуальность на новом master

## Дисциплина (короткая выжимка сессии)

- НЕ верить codex "all green" — re-run сам (r7: поймал env-зависимые AllocatePort)
- НЕ верить адверс-панели слепо — проверять каждый claim против кода (r5: 2 из 3 "P1" ложные)
- UTC-якорь: `date -u`, не локальное время (утренний полл фильтровал всё из-за якоря в будущем)
- Бот пишет PASS-summary иногда в issues-comments, находки иногда в review BODY (r8) — читать оба
- Стейл-PASS от дубль-ретриггера НЕ считается (проверять commit_id на HEAD)

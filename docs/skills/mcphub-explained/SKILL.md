---
name: mcphub-explained
description: "Explains mcphub architecture in plain Russian: how supervisor + daemon wrappers + MCP servers (serena, gopls-mcp, mcp-language-server) fit together; what hub-routing solves; key files; common operator-visible failures. Load when working on mcphub or onboarding a new contributor."
---

# mcphub на пальцах

Этот skill — учебник для оператора и быстрый брифинг для агента который только что попал в репозиторий `mcp-local-hub`. Содержит plain Russian объяснения архитектуры + glossary + file:line указатели на самые часто читаемые места кода.

Сохранение в `docs/skills/` (а не в `.claude/skills/`) — преднамеренно: учебник версионируется вместе с кодом, чтобы при изменении архитектуры обновлялись оба синхронно. Loadable через файловый Read, не через runtime skill resolver.

## Когда читать этот skill

- Только что попал в репозиторий, нужен высокоуровневый обзор за 10 минут
- Оператор спрашивает "что такое mcphub" / "как это работает" / "почему 200 процессов"
- Расследуется баг в supervisor lifecycle, респавне, или hub-routing — нужен ментальный мап перед погружением в код
- Просматриваешь план `docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md` и нужен глоссарий

## 1. Что такое MCP-сервер

**Аналогия**: AI-ассистент (claude / codex / cursor) умеет говорить, но не умеет читать файлы или запускать команды. MCP-сервер — это маленькая программа-переводчик. Помощник говорит "хочу увидеть файл X" — MCP-сервер идёт, читает файл, возвращает содержимое.

Каждый MCP-сервер — это **отдельный процесс**. Когда AI-помощник запускается, он спавнит нужные MCP-серверы и общается с ними через `stdin/stdout` или HTTP.

Известные MCP-серверы в этом репо: `memory`, `time`, `wolfram`, `serena` (умная работа с кодом), `gopls-mcp` (Go LSP), `mcp-language-server` (LSP-обёртка для других языков), `gdb`, `lldb`, `godbolt`, `paper-search-mcp`, `perftools`, `sequential-thinking`.

## 2. Зачем mcphub существует

**Проблема без mcphub**: 7 AI-ассистентов × ~14 MCP-серверов каждый = ~100 процессов. Каждый ест RAM/CPU/диск. На Windows: Task Manager показывает сотню `node.exe`, `python.exe`, `mcphub.exe`.

**С mcphub**: ассистенты вместо собственных копий MCP-серверов подключаются к ОДНОМУ экземпляру через mcphub. Hub держит одну `memory`, одну `serena`, одну `mcp-language-server` per язык — все 7 ассистентов с ними общаются.

Результат: **~18 процессов вместо ~100**. Меньше памяти, меньше CPU, меньше хаоса.

**Это и есть смысл mcphub**: сокращение копий MCP-серверов через shared-hub архитектуру. Запись от 2026-04-17: [memory/project_mcphub_process_tails_motivation.md](../../../../../C:/Users/dima_/.claude/projects/d--dev-mcp-local-hub/memory/project_mcphub_process_tails_motivation.md).

## 3. Архитектура — три слоя

### Слой 1: Supervisor (нянька)

- Один процесс `mcphub.exe supervise` — долгоживущий
- Знает какие MCP-серверы должны быть запущены — читает [`supervisor-intent.json`](#) и [`daemon-intent.json`](#)
- Спавнит их при старте
- При падении пытается перезапустить (через auto-respawn dispatcher — PR #230, см. §6)
- При выключении старается убить всех дочек через Windows Job Object с `KILL_ON_JOB_CLOSE`

Канонический код: [internal/cli/supervise.go:380-680](../../../internal/cli/supervise.go) — `runSupervise` функция.

### Слой 2: Daemon wrapper (обёртка)

- Для каждого MCP-сервера supervisor запускает обёртку `mcphub.exe daemon --server <name> --daemon <variant>`
- Обёртка делает то, что сам MCP-сервер не умеет: открывает HTTP-порт, переводит сообщения, держит соединения
- Для `serena`: обёртка слушает 9121 (claude) или 9122 (codex), а **внутри неё** uvx → python serena на внутреннем порту (`external + 10000`)
- Обёртка проксирует запросы. Похоже на reverse proxy.

Канонический код: [internal/daemon/http_host.go](../../../internal/daemon/http_host.go) (для native-http daemons), [internal/daemon/host.go](../../../internal/daemon/host.go) (для stdio bridge).

### Слой 3: GUI + router

- `mcphub.exe gui` — веб-интерфейс на `127.0.0.1:9125` (port настраиваемый)
- Содержит **router**: client → request → router → нужный daemon
- Tray icon показывает aggregate state (StateOK / StatePartial / StateDown)

Канонический код: [internal/gui/](../../../internal/gui/), tray под [internal/cli/gui.go](../../../internal/cli/gui.go).

### Картинка

```text
твой AI-ассистент (codex / claude / cursor)
       ↓
mcphub.exe gui (порт 9125)
       ↓ роутинг
mcphub.exe supervise  ←  state files in %LOCALAPPDATA%\mcp-local-hub
       ↓ держит дочек через Job Object
mcphub.exe daemon --server serena --daemon claude  (порт 9121)
       ↓ внутри спавнит
uvx → uv → python serena (порт 19121)
```

## 4. Serena — почему она особенная

`serena` — MCP-сервер для умной работы с кодом. Знает символы (find_symbol, rename_symbol), может искать ссылки. Написан на Python, запускается через `uvx`.

**Особенность 1**: `_active_project` — глобальное состояние процесса. В каждый момент работает с ОДНИМ проектом. Переключение через `activate_project` убивает LSP-дочек (clangd, tsserver) старого проекта и спавнит для нового. 200-400 мс + RAM-spike.

**Особенность 2**: контексты — `claude-code` и `codex`. В `claude-code` контексте `activate_project` НЕ экспонируется (single_project: true) и `search_for_pattern` тоже исключён. В `codex` контексте — оба доступны.

**Текущий legacy setup** (`servers/serena/manifest.yaml` на master): 2 daemon-а — `claude` на 9121 (context claude-code) + `codex` на 9122 (context codex). Это плохо для multi-project работы — все 6 claude-family агентов делят одну серену залоченную на один проект.

**Целевой дизайн** (план `docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md`): **N серен = N активных воркспейсов агентов** (1:1 биекция, dynamic-pool). Каждый воркспейс получает свой долгоживущий serena-daemon забутстрапленный на `--project <abs-path>`.

## 5. Что mcphub УЖЕ решает / ЕЩЁ нет

| Проблема | Решение | Статус |
|---|---|---|
| 100 процессов вместо 18 | hub-routing через `mcphub migrate` | ✓ работает; codex stdio→HTTP миграция применена 2026-05-20 |
| Падающий daemon не перезапускается | PR #229 daemon-exited emit + PR #230 auto-respawn dispatcher | ✓ merged 2026-05-20, validated в production (kill→respawn за 1.1s) |
| `_active_project` thrashing между проектами | dynamic-pool 1:1 biection | пишется в плане, 4 BLOCKERS открыты в v3 review |
| LSP-language MCP-серверы (clangd, typescript и т.д.) per-subagent | PR #222 workspace-scoped LSP-bridge на 9200-9299 | в работе на ветке `feat/v0.5.x-servers-matrix-revamp` |
| Per-subagent fan-out codex CLI | upstream codex feature request (out-of-scope для mcphub PR) | external-only |
| Аварийная очистка live-rooted процессов | Phase H в плане: `mcphub cleanup --aggressive` с typed-confirmation | дизайн готов, реализация в v4+ |

## 6. PR #229 + PR #230 (что сделано 2026-05-20)

**PR #229** — supervisor `daemon-exited` event emit:
- В goroutine `cmd.Wait()` (`internal/cli/supervise.go:~1600`) после exit пишется audit event `daemon-exited` с `pid`, `exit_code`, `wait_err`
- БЕЗ этого emit супервизор тихо переводил state в "idle" и оператор не знал ПОЧЕМУ daemon упал. 35 циклов silent crash подтверждены логом
- Закрывает diagnostic gap

**PR #230** — auto-respawn dispatcher:
- Channel-based goroutine `runRespawnDispatcher` ([internal/cli/supervise_respawn_dispatcher.go](../../../internal/cli/supervise_respawn_dispatcher.go))
- При `daemon-exited` non-clean: запись в sliding window (30 мин) → если < 10 в окне → backoff 1s/2s/4s/8s/16s/32s/60s cap → respawn
- При >= 10 fails в окне → `MarkQuarantined` + persist supervisor-state.json → respawn stops до cold restart
- Operator-stop respect: проверяет `daemon-intent.json` IsActiveStop → suppress respawn если оператор сам остановил
- Validated live 2026-05-20 16:12Z: kill серена → cycle exited→scheduled→fired→spawned за 1.1 секунды

## 7. Где смотреть код для типичных задач

| Задача | Файл |
|---|---|
| Supervisor lifecycle, IPC, state files | [`internal/cli/supervise.go`](../../../internal/cli/supervise.go) |
| Auto-respawn dispatcher (PR #230) | [`internal/cli/supervise_respawn_dispatcher.go`](../../../internal/cli/supervise_respawn_dispatcher.go) |
| Daemon runtime state tracker | [`internal/cli/supervisor_runtime_tracker.go`](../../../internal/cli/supervisor_runtime_tracker.go) |
| State machine (currently bypassed in production) | [`internal/api/supervisor_state_machine.go`](../../../internal/api/supervisor_state_machine.go) |
| HTTP daemon host (native-http like serena) | [`internal/daemon/http_host.go`](../../../internal/daemon/http_host.go) |
| Stdio daemon host (process bridge) | [`internal/daemon/host.go`](../../../internal/daemon/host.go) |
| LazyProxy / workspace-scoped LSP (PR #222) | [`internal/daemon/lazy_proxy.go`](../../../internal/daemon/lazy_proxy.go) |
| Workspace registry | [`internal/api/workspace_registry.go`](../../../internal/api/workspace_registry.go) |
| Manifest parsing + validation | [`internal/config/manifest.go`](../../../internal/config/manifest.go) |
| Process cleanup with safety guards | [`internal/api/cleanup.go`](../../../internal/api/cleanup.go) |
| GUI server + endpoints | [`internal/gui/`](../../../internal/gui/) |
| Tray + GUI lifecycle | [`internal/cli/gui.go`](../../../internal/cli/gui.go) |
| Serena manifest | [`servers/serena/manifest.yaml`](../../../servers/serena/manifest.yaml) |

## 8. Workflow PR — golden path

См. [`CLAUDE.md`](../../../CLAUDE.md) §"PR review + merge workflow (MANDATORY)" — это hard gate. Кратко:

1. `go build ./... && go vet ./... && go test ./...` локально (но `./internal/cli/...` + `./internal/daemon/...` чтобы не убить установленных daemon-ов)
2. `git push -u origin <branch>` + `gh pr create`
3. **Codex Cloud bot** делает review — wait for PASS на CURRENT HEAD commit (не stale на старом)
4. Все findings (P0/P1/P2/P3) фиксить ВСЕ перед merge. Не deferer на post-merge без явного разрешения оператора
5. Deep-review per CLAUDE.md Step 5 — параллельный codex `-c model_reasoning_effort=xhigh` deep-security
6. CI `workflow_dispatch`-only — НЕ автотриггерить
7. `gh pr merge <N> --squash --delete-branch` — НЕ `--admin`

## 9. Glossary

| Термин | Простыми словами |
|---|---|
| MCP | Model Context Protocol — стандарт общения AI с инструментами |
| MCP-сервер | Программа-переводчик между AI и инструментом |
| mcphub | "Телефонная станция" для MCP-серверов — одна копия вместо N |
| supervisor | Нянька внутри mcphub. Запускает, рестартит |
| daemon wrapper | mcphub.exe обёртка вокруг конкретного MCP-сервера |
| serena | MCP-сервер для умной работы с кодом |
| uvx | Python-пакет launcher (как pip install + run) |
| active project | Внутри serena — текущий проект на котором работает |
| `_active_project` thrashing | Когда N AI пытаются переключить проект — серена дёргается |
| dynamic-pool | Архитектура: 1 серена на каждый проект, без перетягивания |
| handshake-port | Альтернатива fixed port: kernel-assigned + IPC publish |
| state machine | Формальная модель состояний (idle/running/exiting и т.д.) |
| orphan | Процесс чей родитель умер, но он сам выжил |
| Job Object | Windows: убить родителя → kernel убьёт детей |
| supervisor-intent.json | Канонический список daemon-ов которые должны крутиться |
| daemon-intent.json | Per-task override: stopped/disabled/quarantined |
| supervisor-state.json | Runtime snapshot: state, current_pid, pid_generation per task |
| supervisor-events.log | JSONL audit — daemon-spawned/exited/respawn-scheduled и т.д. |
| auto-respawn dispatcher | PR #230 goroutine — реагирует на crashCh события |
| sliding window | 30-min окно для failure counter (quarantine threshold = 10) |
| PR #222 | Servers-matrix LSP+env-overlay revamp (in flight) |
| PR #229 | daemon-exited emit (merged 2026-05-20) |
| PR #230 | Auto-respawn dispatcher (merged 2026-05-20) |
| Phase H | Operational hygiene tooling в плане (cleanup --aggressive) |

## 10. Где спросить операатора

Когда что-то не понятно — лучше спросить чем гадать (`AGENTS.md` AXIOM "never guess"). Operator preferences:

- **Russian default**: отвечай на русском по умолчанию (memory: feedback_russian_default)
- **Никогда без tray**: GUI launch без `--no-tray` (memory: feedback_gui_always_tray)
- **Subagents always opus + max** (memory: feedback_subagent_opus_max_default)
- **Codex always xhigh** + file-based prompt only (memory: feedback_codex_xhigh_default, feedback_codex_file_prompt_only)
- **Build → install order**: commit first, then bash build.sh, then install (memory: feedback_build_then_install_order)

## 11. Term Recap для не-Russian читателя

- skill — учебник (literally "skill" / methodology document)
- учебник — textbook / primer
- демон / daemon — long-running process
- обёртка / wrapper — proxy process spawned around a third-party MCP server
- серены / serena — the serena MCP server (plural "серены" = "serena instances")
- багу / fix / починить — bug / fix / repair (used colloquially)
- merge / merge'нём / merge-ить — git merge, transliterated

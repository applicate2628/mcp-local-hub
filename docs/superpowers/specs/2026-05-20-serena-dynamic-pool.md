# Spec: Serena Dynamic Pool — 1:1 daemon-per-workspace биекция

Дата: 2026-05-20
Статус: draft v1 (под codex-консультацию для no-path-args case)
Связанные документы:
- [g4-unified-hub-mcp-design-v3.md](2026-05-12-g4-unified-hub-mcp-design-v3.md) — unified hub vision, в которую вписывается dynamic-pool
- [2026-05-19-servers-matrix-lsp-and-env-revamp-design.md](2026-05-19-servers-matrix-lsp-and-env-revamp-design.md) — текущий servers-matrix revamp, на котором dynamic-pool строится
- `servers/serena/manifest.yaml` — текущий 2-daemon legacy (claude+codex), который dynamic-pool заменит

## 1. Контекст и проблема

На машине оператора одновременно живут несколько проектов с пересекающимися MCP-агентами:

- `D:\dev\PaperPane` (cpp + typescript + markdown)
- `D:\dev\mcp-local-hub` (go + typescript + markdown)
- иногда фронтенд-проекты на TS

И ~6-7 MCP-клиентов: `claude-code`, `codex-cli`, `cursor`, `vscode`, `gemini-cli`, `qwen-cli`, `antigravity`.

Текущая schema (`servers/serena/manifest.yaml` v0.4.x):

```yaml
daemons:
  - {name: claude, context: claude-code, port: 9121, extra_args: [--context, claude-code]}
  - {name: codex,  context: codex,       port: 9122, extra_args: [--context, codex]}
```

— **2 serena-демона на ВСЕ проекты**. Поскольку serena держит `_active_project` в process-global state, **переключение между PaperPane и mcp-local-hub в любом из 6 claude-family агентов триггерит thrashing**: kill LSPs (clangd/tsserver/gopls) для проекта A → spawn для проекта B. На warm cache `.serena/cache/` warm-up ускоряется, но 200-400 ms latency + RAM-spike сохраняются.

В `claude-code` context (serena 1.26.0, pin commit `f0a3a279...`) ещё хуже: `single_project: true` означает что `activate_project` **не экспонируется как tool вообще** — клиент не может переключить проект, только рестарт daemon с другим `--project`.

## 2. Аксиома (фиксированное архитектурное решение)

**N серен = N активных воркспейсов агентов. 1:1 биекция. Глобальных серен нет.**

- Каждый зарегистрированный воркспейс получает свой долгоживущий serena-daemon
- Каждый daemon забутстраплен на `--project <abs-path>` с языками из `.serena/project.yml`
- mcphub становится router'ом: клиент шлёт tool-call на mcphub-endpoint (`localhost:9100/serena/mcp` или подобное) → middleware определяет workspace по path-аргументу → форвардит на нужный daemon
- Никакого `_active_project tug-of-war` потому что каждый процесс уже залочен на свой проект

**Trade-off**: RAM-бюджет растёт линейно с числом проектов (≈300-500 MB per warm serena). На «generous 3+ GB» машине оператора — приемлемо до ~6 воркспейсов в активной ротации.

## 3. Итоговая таблица — оптимальная архитектура

### Часть А: какой backend для какой задачи

| Use case | Кто обслуживает | Почему |
|---|---|---|
| Primary IDE work на проекте X (semantic edits, `rename_symbol`, memory files) | Отдельный `serena-<X>` daemon pre-activated на X: `--context codex --project X` | Нет thrashing, `_active_project` залочен на X bootstrap-time, безопасно для N агентов |
| Parallel glance на проект Y без переключения primary | `mcp-language-server` через mcphub LazyProxy (per-workspace LSP) | Не трогает serena's `_active_project`; basic `definition`/`hover`/`diagnostics`; singleflight collapse N агентов на 1 backend |
| Refactoring `rename_symbol` для Go | `gopls-mcp` (native Google's gopls MCP) | Лучше generic mcp-language-server для Go; уже в [manifest.yaml:35-40](../../../servers/mcp-language-server/manifest.yaml) |
| Refactoring для C++ / TypeScript | `mcp-language-server-<lang>` через mcphub | Generic LSP; работает но без serena's memory-files |
| Codex CLI multi-project workflow (codex сам `activate_project` switching) | `serena-<X>` daemon с `--context codex` (используется same per-workspace daemon, не отдельная глобальная серена) | После 1:1 биекции отдельная codex-серена не нужна; codex агент работает в конкретном workspace = в его per-workspace серене |
| Background analysis на не-primary проектах | Отдельный `serena-<Y>` daemon (та же 1:1 биекция, просто Y ≠ primary IDE workspace) | Изолированный от primary daemon-ов; per-workspace state |
| Не-LSP MCPs (`memory`, `time`, `wolfram`, etc.) | mcphub global routing | 1 process на N агентов, hub-multiplex (не меняется) |

### Часть Б: переход с текущего 2-daemon к dynamic-pool

**Текущее (legacy, до удаления):**

```yaml
daemons:
  - {name: claude, context: claude-code, port: 9121, extra_args: [--context, claude-code]}
  - {name: codex,  context: codex,       port: 9122, extra_args: [--context, codex]}
client_bindings:
  - {client: claude-code, daemon: claude, ...}
  # 6 claude-family агентов на claude daemon — все вынуждены работать над ОДНИМ проектом
```

**Целевое (dynamic-pool, после реализации):**

```yaml
name: serena
kind: workspace                 # ← новый kind (был: global)
transport: native-http
command: uvx
base_args: [...]
env: {PYTHONUNBUFFERED: "1"}

# Single template daemon descriptor; instance-per-workspace
# создаётся динамически при register/auto-detect:
daemon_template:
  context: codex                # единый context для всех agents
  port_pool: [9121, 9122, ..., 9199]   # mcphub выбирает свободный
  extra_args_template:
    - --context
    - codex
    - --project
    - "${workspace.path}"        # подставляется per-instance

# Клиентских bindings нет в манифесте — все клиенты ходят через mcphub
# router, который определяет нужный daemon-instance по path-аргументу
# (см. §4 routing middleware).
```

### Часть В: формула процессов

Для N агентов, W активных воркспейсов с overlapping языками, L уникальных языков:

| Категория | Сколько процессов | Почему |
|---|---|---|
| `serena-<workspace>` daemons | **W** (по 1 на активный workspace) | 1:1 биекция, нет thrashing, languages из `.serena/project.yml` |
| `mcp-language-server` через mcphub LazyProxy | Σ_w(L_w) (по 1 на пару `(workspace, language)`) | Per-(ws,lang); singleflight для N агентов |
| Global MCPs (`memory`, `time`, `wolfram`, etc.) | константа (~10) | Hub-multiplex, не меняется |
| **Итого для W=3, L=4, N=7 агентов** | **3 + ~6 + ~10 = ~19 процессов** | (vs 7×~19 = ~130 без mcphub) |

vs. legacy 2-daemon:
- legacy: **2 serena + ~6 LSP + ~10 global = ~18**
- dynamic-pool: **3 serena + ~6 LSP + ~10 global = ~19**

Один лишний процесс — но без thrashing и без `_active_project tug-of-war`. RAM выше на ~300-400 MB per extra workspace.

### Часть Г: операторские шаги при миграции на dynamic-pool

| Шаг | Команда | Эффект |
|---|---|---|
| 1 | Удалить stdio `mcp-language-server` entries из `.codex/config.toml` (вручную или `mcphub migrate-legacy --client codex-cli --yes`) | Codex перестанет форкать сирот при каждой сессии |
| 2 | Зарегистрировать workspaces: `mcphub workspace register "D:\dev\PaperPane"` + `mcphub workspace register "D:\dev\mcp-local-hub"` | Записывает в `workspaces.yaml`, mcphub при следующем supervise spawn'нет serena-instance per registered workspace |
| 3 | `mcphub install --upgrade` | Supervisor перечитывает intent, спавнит N серен (по 1 на workspace), legacy claude/codex daemons удаляются |
| 4 | Открыть GUI → Servers matrix → чекнуть нужные agents × serena cells → Apply | Переписывает client config'и на mcphub-router URL |
| 5 | Перезапустить агентов (close+reopen Cursor/Claude/codex) | Подтягивают новый mcphub-router URL |

### Часть Д: критические ограничения от codex (deep-source review)

1. **`didOpen`/`didClose` cross-session lifecycle через mcp-language-server lazy-proxy НЕ имеет per-client refcount.** Если агент A открывает file X через didOpen, потом агент B закрывает его через didClose — file перестаёт быть открытым для агента A. Hidden bug в multi-agent setup. Flag'нуто в [lazy_proxy.go](../../../internal/daemon/lazy_proxy.go).

2. **`claude-code` context НЕ имеет `activate_project` tool вообще.** Per `single_project: true` в claude-code preset (pinned commit `f0a3a279...`). Это значит: если оператор в IDE работает над `D:\dev\mcp-local-hub`, а его claude-daemon serena pre-activated на `D:\dev\PaperPane` (default-cwd-detection) — claude-агент не может переключиться. Нужен либо отдельный per-project daemon (что dynamic-pool и делает), либо рестарт serena с другим `--project`.

3. **mcp-language-server crash mid-request → нет auto-retry.** Inflight requests fail; next call re-materializes backend. OK для interactive use, плохо для batch.

4. **Multi-root LSP через `mcp-language-server --workspace D:\dev` (parent-root)** — возможно, но плохо: лишняя индексация, tsconfig/go.mod discovery drift, особенно плохо для Go без `go.work`.

5. **Для TS canonical native MCP server (как gopls-mcp для Go) не существует.** Оставляем `typescript-language-server` через `mcp-language-server` обёртку.

## 4. Routing middleware (3 mode)

mcphub-router принимает все client-side serena tool-calls на единый endpoint `localhost:9100/serena/mcp` и маршрутизирует на конкретный `serena-<workspace>` daemon.

### Mode 1: Path-aware (default для большинства tools)

Tool args содержат `relative_path` / `file_path` / `name_path` (e.g. `find_symbol`, `replace_symbol_body`, `find_referencing_symbols`, `search_for_pattern` с `file_pattern`):

```
1. Resolve path:
   - Absolute path → use as-is
   - Relative path → for each registered workspace, try `workspace.path + relative_path`;
                     first existing match wins
2. Walk parents до первого `.serena/project.yml` → workspace identified
3. Lookup mcphub workspace registry → daemon-port → forward request
```

### Mode 2: Sticky-session (для no-path tools — pending codex consult)

Tools БЕЗ path-аргумента (`list_memories`, `get_current_config`, `write_memory`, `delete_memory`, `read_memory`):

```
1. mcphub MCP-session ID → workspace map (per MCP client connection)
2. Workspace fixed at FIRST path-aware call в сессии
3. Subsequent no-path calls → same workspace daemon
4. Если no-path call BEFORE first path call: возврат к default workspace (TBD) ИЛИ batched error
```

**Open question** (под codex консультацию): что делать если первый tool-call в сессии — без path? Кандидаты:
- (A) Defer ответ до первого path-call
- (B) Default workspace из `workspaces.yaml` (есть поле `default: true`?)
- (C) Reject + require client to call path-tool first
- (D) Aggregate from all daemons (only for read-only queries: `list_memories` etc.)

### Mode 3: Auto-register on miss

Unknown path не маппится ни на один зарегистрированный workspace:

```
1. File-extension survey пути и его родителей
2. Если detected languages → создать `<path>/.serena/project.yml` со списком языков
3. Записать в `workspaces.yaml`
4. Spawn новый serena-instance на свободный port из pool
5. Wait until daemon healthy → forward request
6. Audit event: workspace-auto-registered
```

Failures (extension survey пустой, .serena dir creation failed, port pool exhausted) → fallback на default workspace ИЛИ ошибка клиенту с explicit reason.

## 5. workspaces.yaml schema

`%LOCALAPPDATA%\mcp-local-hub\workspaces.yaml`:

```yaml
version: 1
workspaces:
  - path: "D:\\dev\\PaperPane"
    languages: [cpp, typescript, markdown]  # snapshot of .serena/project.yml at register time
    default: false                           # exactly one workspace может быть default (для no-path fallback)
    registered_at: 2026-05-20T12:34:56Z
    registered_via: manual                   # manual | auto-detect | migration
    serena_port: 9121                        # mcphub-allocated, persisted
  - path: "D:\\dev\\mcp-local-hub"
    languages: [go, typescript, markdown]
    default: true
    registered_at: 2026-05-20T12:35:10Z
    registered_via: manual
    serena_port: 9122
```

Read via `internal/api.ReadWorkspacesFile()` (новый), validation:
- exactly один workspace с `default: true`
- `path` существует и есть `.serena/project.yml`
- `serena_port` уникален и в диапазоне port_pool
- `languages` non-empty

## 6. Lifecycle: spawn / health / restart / shutdown

### Spawn
1. supervisor читает `workspaces.yaml` + `servers/serena/manifest.yaml`
2. Для каждого workspace создаёт `SupervisorDaemon` descriptor с:
   - task_name: `\mcp-local-hub-serena-<hash(workspace_path)>`
   - args: `[daemon, --server, serena, --workspace, "<path>"]`
   - port: workspace.serena_port
3. spawn через standard supervisor pipeline (после супервизор-фикса из PR #229 — с emit `daemon-exited`)

### Health
- mcphub проверяет TCP health на `serena_port` каждые 30s
- При unreachable → `daemon-health-failed` event → respawn через restart-policy state machine (после P2 fix wiring state machine в production — отложен на отдельный PR)

### Restart
- На редактирование `.serena/project.yml` (languages change) → graceful restart инстанса (intent-watcher уже есть в supervise_watcher.go)
- На редактирование `workspaces.yaml` (add/remove workspace) → reconcile-diff → spawn/terminate соответствующих instances

### Shutdown
- supervisor exit → Job Object KILL_ON_JOB_CLOSE накрывает всю tree per workspace (требует P1 fix из PR #229 для closing Start-then-Assign race)

## 7. Handshake / dynamic-port: куда вписывается

Текущая модель **фиксированные порты** (9121, 9122 в legacy; теперь port_pool в dynamic-pool с persistent assignment) — клиенты пишут жёсткие URL в свои config'и.

**Альтернатива — handshake**: daemon биндится на port 0 → kernel выдаёт случайный → publish своего port через handshake (например, supervisor IPC → workspaces.yaml). Клиенты discoverят через mcphub router.

| Plane | Фикс-порт (current dynamic-pool) | Handshake (future) |
|---|---|---|
| Простота клиентов | URL `localhost:9100/serena/mcp` (router constant) | То же — клиент знает только router |
| Port-collision recovery | orphan на 9122 → spawn fail → retry on alternate из pool | port 0 → collision impossible by design |
| Архитектура | persistent port-assignment в `workspaces.yaml` | ephemeral port, supervisor отслеживает на per-spawn basis |
| Зависимость от router | Обязательна (без router — клиент не знает какой daemon-instance) | То же |

**Решение для v1**: остаёмся на фикс-порту из pool + router (как описано выше). Handshake — улучшение для v2, когда docked к G4 unified hub spec.

## 8. Memory budget

Per workspace (после warm-up с прогретыми LSP children):

| Component | RAM |
|---|---|
| serena Python core | ~80 MB |
| Per-language LSP child (clangd / tsserver / gopls) | ~150-250 MB |
| `.serena/cache/` mmap'ed indices | ~50 MB |
| **Total per workspace (3 langs)** | **~600-900 MB** |

| Setup | RAM |
|---|---|
| 1 workspace, 1 lang | ~250 MB |
| 1 workspace, 3 langs (PaperPane: cpp+ts+md) | ~700 MB |
| 3 workspaces, mixed | ~2-2.5 GB |
| 6 workspaces (realistic ceiling) | ~4-5 GB |

На «generous 3+ GB» машине оператора: до 4-5 workspaces в активной ротации без swapping.

## 9. Migration plan от 2-daemon к dynamic-pool

### Phase 1 (текущий PR scope): foundation
1. Реализовать `workspaces.yaml` schema + validator (`internal/api/workspaces.go`)
2. Реализовать `mcphub workspace {register,unregister,list}` CLI
3. Расширить manifest schema поддержкой `kind: workspace` + `daemon_template`
4. Supervisor: spawn N инстансов по workspaces.yaml (вместо fixed `daemons:` list)

### Phase 2: routing
5. Реализовать mcphub-router endpoint `/serena/mcp` (path-aware mode)
6. Реализовать sticky-session map (per MCP-session workspace binding)
7. Реализовать auto-register on miss (file-extension survey)

### Phase 3: cutover
8. Migration script: `mcphub migrate serena legacy-to-dynamic-pool` — переводит `claude` + `codex` daemons на per-workspace
9. Удаление legacy 2-daemon descriptors из `servers/serena/manifest.yaml`
10. Удаление per-client bindings в манифесте (всё через router)

### Phase 4: handshake (v2, future)
11. Daemon биндится на port 0, публикует через supervisor IPC
12. Router discoverит через IPC status, не через `workspaces.yaml.serena_port`
13. Docking к G4 unified hub spec — clients используют ONE constant mcphub URL для всего

## 10. Failure modes и graceful degradation

| Failure | Behavior |
|---|---|
| `workspaces.yaml` malformed | supervisor refuses to start, audit `workspaces-file-malformed`, manual `mcphub workspace validate` для diagnose |
| Workspace dir не существует | skip спавн instance, audit `workspace-path-missing`, mcphub-router возвращает HTTP 503 на запросы для этого workspace |
| `.serena/project.yml` отсутствует | auto-register fallback: file-extension survey → create stub `.serena/project.yml` ИЛИ skip с audit |
| Port pool exhausted | spawn fail, audit `serena-port-pool-exhausted`, recommend operator widen pool или unregister stale workspaces |
| Path-aware routing miss (никакой workspace не подходит) | auto-register (mode 3) ИЛИ fallback на default workspace ИЛИ HTTP 422 с explicit "no workspace matches path X" |
| No-path tool-call before first path call | TBD после codex consult (см. §4 Mode 2 open question) |
| Daemon instance crash | restart-policy state machine (после P2 fix из follow-up PR); до того — manual `mcphub restart --task <task_name>` |
| Cross-workspace symbol-call (e.g. `find_referencing_symbols` где symbol в A, refs в B) | **Out of scope для v1**. Возврат references ТОЛЬКО из workspace, в котором symbol определён. Multi-workspace refs — feature для v2 |

## 11. Open questions (под codex consultation)

1. **No-path-args sticky-session semantics (§4 Mode 2)**: какой default behavior для первого no-path call в сессии? A/B/C/D из §4?
2. **MCP-session ID stability across reconnects**: serena MCP-сервер не expose session ID в headers; mcphub router может использовать client TCP connection ID? Сохраняется ли при reconnect?
3. **workspaces.yaml hot-reload latency**: при `mcphub workspace register` нужен ли restart существующих daemons или только spawn нового? Какой минимальный disruption?
4. **Auto-register `.serena/project.yml` content generation**: какие defaults для `read_only: false`? `excluded_dirs: [.git, node_modules, target, dist]`? Какой `language_detector_threshold`?
5. **Port allocation persistence**: при удалении workspace из `workspaces.yaml`, освобождать ли port немедленно или с retention для backward-compat reconnects?

## 12. Терминология

- **serena daemon** — long-lived Python process (uvx → serena.cli) с активированным `--project <abs-path>` и языками из `.serena/project.yml`
- **serena instance** — то же что serena daemon в контексте dynamic-pool (по 1 на workspace)
- **workspace** — отдельный проект на диске с `.serena/project.yml` и опционально `.serena/cache/`
- **active workspace** — workspace, для которого есть запись в `workspaces.yaml` И существует серена-инстанс
- **dynamic-pool** — текущая spec'а: N серен = N активных workspaces (1:1 биекция)
- **legacy 2-daemon** — текущий live setup (`claude`/`codex` 2 серен на ВСЕ проекты), который dynamic-pool заменяет
- **mcphub-router** — внутренний routing layer mcphub, который принимает client-side MCP calls и форвардит на нужный backend daemon
- **path-aware routing** — Mode 1 из §4: маршрутизация по path-аргументу tool-call'а
- **sticky-session routing** — Mode 2 из §4: маршрутизация no-path tools по MCP-session ID
- **auto-register** — Mode 3 из §4: создание нового workspace + spawn нового serena-instance при miss
- **handshake-port** — альтернатива фикс-порту: daemon биндится на port 0 + publish через IPC (deferred к v2)

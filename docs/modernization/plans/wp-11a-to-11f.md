<a id="top"></a>
# Архитектурный план WP-11A—WP-11F

## Оглавление

1. [Общие ограничения](#constraints)
2. [WP-11A](#wp-11a)
3. [WP-11B](#wp-11b)
4. [WP-11C](#wp-11c)
5. [WP-11D](#wp-11d)
6. [WP-11E](#wp-11e)
7. [WP-11F](#wp-11f)
8. [Финальный gate](#final-gate)

<a id="constraints"></a>
## 1. Общие ограничения

- Исторический audit SHA и текущий baseline SHA — разные поля.
- Новый PR создаётся от актуального `master` после merge зависимостей.
- Characterization PR не исправляет обнаруженное поведение.
- `archcheck` использует только существующие kinds: `import`, `mutable_global`, `api_construction`, `production_constructor`, `production_test_hook`, `file_budget`, `history_comment`, `embedded_document`, `worker`, `generic_package`.
- `production_constructors` — список `{import_path, symbol, allowed_globs}`; отдельные kinds для каждого constructor не создаются.
- Hosted CI не добавляется.

<a id="wp-11a"></a>
## 2. WP-11A — guardrails и inventory

### PR-11A-01 / A1 — archcheck core

PR №602, reviewed HEAD `0235deb95af170598d6572e9af5f35ab5532bc9d`. План считается историческим; нормативный контракт определяется фактическими `Policy`, `Baseline`, `Workers`, `Verification` и CLI после merge.

### PR-11A-02 / A2 — repository ratchet

Создать:

```text
architecture/policy.yaml
architecture/owners.yaml
architecture/baseline.yaml
architecture/workers.yaml
architecture/README.md
```

Начальная policy использует фактическую схему A1:

```yaml
schema_version: 1
module: mcp-local-hub
source_roots: [cmd, internal, tools]
exclude_globs:
  - internal/gui/assets/**
  - internal/gui/frontend/**

import_rules:
  - from: [internal/api/**]
    deny: [internal/app/**, internal/cli/**, internal/gui/**]
  - from: [internal/supervisor/**]
    deny: [internal/app/**, internal/cli/**, internal/gui/**]

api_constructors:
  - import_path: mcp-local-hub/internal/api
    symbol: NewAPI
    allowed_globs:
      - internal/app/**
      - internal/api/**/*_test.go
      - internal/cli/**/*_test.go
      - internal/gui/**/*_test.go
      - internal/daemon/**/*_test.go
production_constructors: []

allowed_global_name_patterns: []
test_hook_name_patterns:
  - ^Set.*ForTest$
  - ^Restore.*ForTest$
  - .*ForTest$
history_comment_patterns:
  - PR #[0-9]+
  - round[- ]?[0-9]+
history_allowed_globs:
  - docs/adr/**
  - work-items/**
  - '**/*_test.go'
test_only_build_tags:
  - test_state_path_env
embedded_document_min_bytes: 4096
file_budgets:
  production_advisory_lines: 1000
  production_hard_lines: 1500
  test_review_lines: 2000
generic_package_names: [common, utils]
```

Пороги [D-013](../decisions.md#d-013) и каждый regex проверяются inventory. Широкий allowlist `^Err[A-Z]` не принимается без отчёта типов/инициализаторов; предпочтительны точные имена либо отдельное семантическое правило. Если delta-review выявит дополнительные custom test tags, они добавляются явным reviewable diff.

Baseline создаётся из фактического дерева:

```bash
base_sha="$(git rev-parse HEAD)"
mkdir -p .reports

go run ./tools/archcheck scan \
  --root . \
  --policy architecture/policy.yaml \
  --report-json .reports/architecture-inventory.json \
  --report-md .reports/architecture-inventory.md

go run ./tools/archcheck baseline \
  --root . \
  --policy architecture/policy.yaml \
  --owners architecture/owners.yaml \
  --generated-from "$base_sha" \
  --baseline architecture/baseline.yaml

go run ./tools/archcheck verify \
  --root . \
  --policy architecture/policy.yaml \
  --baseline architecture/baseline.yaml \
  --workers architecture/workers.yaml
```

`owners.yaml` использует specific-first rules. Последний catch-all допускается только как краткоживущий `triage-owner` и не считается завершённой классификацией.

A2 также добавляет минимальный deterministic JSON golden helper в `internal/testkit`, чтобы A3 и A4 действительно могли идти параллельно.

### PR-11A-03 / A3 — supervisor/install characterization

Фиксируются startup/shutdown/status/second-instance и install plan/dry-run/rollback order. PID, timestamps, absolute paths и ephemeral ports нормализуются; причинно значимый порядок не сортируется.

### PR-11A-04 / A4 — MCP/LazyProxy characterization

Фиксируются initialize/list/call/cancellation/partial failure и LazyProxy singleflight/warm/probation/teardown/document references.

### PR-11A-05 / A5 — critical workers

Команда учитывает фактический CLI:

```bash
go run ./tools/archcheck workers \
  --root . \
  --policy architecture/policy.yaml \
  --baseline architecture/baseline.yaml \
  --workers architecture/workers.yaml \
  --unclassified \
  --path internal/cli/supervise.go \
  --path internal/daemon/lazy_proxy.go
```

Каждая запись получает component/owner/start/cancel/join/bounded_by/contract_test/work_package. Для обновления baseline создаётся candidate-файл и reviewable diff; деструктивное автоматическое pruning baseline не используется.

### PR-11A-06 / A6 — полный worker inventory

- `workers --unclassified` возвращает пустой результат;
- worker entries удалены из baseline;
- stale records блокируют verify;
- repeated/shuffle/race проходят для critical packages.

<a id="wp-11b"></a>
## 3. WP-11B — composition root и DI

### PR-11B-01 / B1

Создать `internal/app`, production Runtime и `NewRootCmdWithDependencies`. Migration inventory использует `api_construction`, `production_constructor` и `production_test_hook`; package-specific constructor kinds не создаются.

Пример правил:

```yaml
production_constructors:
  - import_path: mcp-local-hub/internal/gui
    symbol: NewServer
    allowed_globs:
      - internal/app/**
  - import_path: mcp-local-hub/internal/cli
    symbol: NewRootCmd
    allowed_globs:
      - internal/app/**
```

### PR-11B-02 / B2

Read-only CLI получает shared `*api.API`; output/error contracts сохраняются.

### PR-11B-03 / B3

Mutating CLI получает shared dependencies; transaction authority не дублируется.

### PR-11B-04 / B4

GUI получает process-scoped `Services`; handlers не вызывают `api.NewAPI()`.

### PR-11B-05 / B5

Production main, GUI и route daemon используют один service graph; fallback constructors остаются только compatibility facade.

### PR-11B-06 / B6

Три `TestMain` используют общий process harness; filesystem/process side effects заменены instance-owned fakes.

### PR-11B-07 / B7

Supervisor package globals заменены typed instance dependencies внутри `internal/cli`; это prerequisite для `WP-11C` extraction.

### PR-11B-08 / B8

Удаляются legacy constructors и production test hooks. После B8 policy остаётся в фактической схеме списка `SymbolRule`, а не вымышленной map-схеме.

<a id="wp-11c"></a>
## 4. WP-11C — supervisor и childprocess

1. `PR-11C-01`: механически разделить `supervise.go` внутри package без изменения поведения.
2. `PR-11C-02`: перенести core state/effects/IPC в `internal/supervisor`, сохранив facade.
3. `PR-11C-03`: создать `internal/childprocess` с runner/environment/logging/containment/teardown.
4. `PR-11C-04`: мигрировать `StdioHost`.
5. `PR-11C-05`: мигрировать `HTTPHost`, удалить duplicate lifecycle owners и facade.

Выход: CLI — thin adapter; один process-tree owner; contract tests и platform lifecycle gates зелёные.

<a id="wp-11d"></a>
## 5. WP-11D — registry и install

1. `PR-11D-01`: общий generic `RegistrySnapshotView[T]`.
2. `PR-11D-02`: Serena resolver migration.
3. `PR-11D-03`: LSP resolver migration.
4. `PR-11D-04`: pure `InstallPlan` без side effects.
5. `PR-11D-05`: executor и transaction journal с rollback/resume.
6. `PR-11D-06`: удалить duplicate client/process/reload helpers.

Выход: reload concurrency реализована один раз; planner детерминирован и чист; transaction checkpoints наблюдаемы.

<a id="wp-11e"></a>
## 6. WP-11E — MCP и LazyProxy

1. `PR-11E-01`: protocol/session/routing boundaries.
2. `PR-11E-02`: fanout/dispatch/recovery boundaries.
3. `PR-11E-03`: typed LazyProxy state/event reducer + effect runner.
4. `PR-11E-04`: единая timeout/lifecycle policy.
5. `PR-11E-05`: вынести embedded Markdown, сократить historical comments до invariant + ADR.

Выход: state transitions исчерпывающе тестируются; protocol, routing и recovery имеют независимые owners.

<a id="wp-11f"></a>
## 7. WP-11F — removal и enforcement

1. `PR-11F-01`: удалить оставшиеся compatibility facades.
2. `PR-11F-02`: закрыть allowlist и включить hard import/global/file gates.
3. `PR-11F-03`: перегруппировать `codex_round*` и другие исторические tests по устойчивым контрактам.
4. `PR-11F-04`: независимый architecture review, dead-code sweep и финальная traceability reconciliation.

Выход: нет duplicate owners, stale baseline, unclassified workers или production test seams.

<a id="final-gate"></a>
## 8. Финальный gate

```bash
go run ./tools/archcheck verify \
  --root . \
  --policy architecture/policy.yaml \
  --baseline architecture/baseline.yaml \
  --workers architecture/workers.yaml \
  --report-json .reports/architecture-report.json \
  --report-md .reports/architecture-report.md

go build ./...
go vet ./...
go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api ./internal/cli
go test -shuffle=on -count=20 -timeout 10m ./internal/api ./internal/cli ./internal/gui ./internal/daemon
go test -race -count=1 -timeout 15m ./internal/api ./internal/cli ./internal/gui ./internal/daemon
```

[Вернуться к началу](#top)

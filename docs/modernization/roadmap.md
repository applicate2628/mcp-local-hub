<a id="top"></a>
# Roadmap программы модернизации

## Оглавление

1. [Масштаб программы](#scale)
2. [Граф зависимостей](#dependency-graph)
3. [Рабочие пакеты](#work-packages)
4. [Релизные волны](#release-waves)
5. [Архитектурный PR registry](#pr-registry)
6. [Правила детализации WP-00—WP-10](#jit-planning)
7. [Конечная граница](#final-boundary)

<a id="scale"></a>
## 1. Масштаб программы

На текущем уровне планирования определены:

- **17 рабочих пакетов:** `WP-00—WP-10` и `WP-11A—WP-11F`;
- **36 findings:** `R-01—R-36`;
- **236 нормативных требований**;
- **20 архитектурных приёмочных тестов:** `AT-001—AT-020`;
- **16 решений:** `D-001—D-016`;
- **272 управляемых идентификатора** требований, тестов и решений суммарно;
- **34 архитектурных PR** для `WP-11A—WP-11F`;
- **6 релизных волн**.

Точное число PR всей программы намеренно не фиксируется до delta-review соответствующего рабочего пакета. Пункты разных WP имеют разный масштаб; искусственное равенство «одна строка roadmap = один PR» запрещено.

<a id="dependency-graph"></a>
## 2. Граф зависимостей

<a id="figure-1"></a>
```mermaid
graph TD
    W0[WP-00] --> W1[WP-01]
    W0 --> A[WP-11A]
    W0 --> W9[WP-09 foundation]
    A --> B[WP-11B]
    A --> D[WP-11D]
    A --> E[WP-11E]
    B --> C[WP-11C]
    W1 --> C
    D --> W3[WP-03]
    D --> W7[WP-07]
    E --> W4[WP-04]
    E --> W5[WP-05]
    C --> W8[WP-08]
    W6[WP-06] --> W7
    W9 --> W10[WP-10]
    B --> F[WP-11F]
    C --> F
    D --> F
    E --> F
    W10 --> F
```

**Рисунок 1 — ориентированный ациклический граф основных зависимостей.**

`WP-11D` и `WP-11E` могут идти параллельно с частью `WP-11B`. `WP-11C` начинается после instance-owned supervisor dependencies (`PR-11B-07`), а не обязательно после полного B8. `WP-11F` является общей точкой удаления временных путей.

<a id="work-packages"></a>
## 3. Рабочие пакеты

| ID | Название | Зависит от | Критерий выхода |
|---|---|---|---|
| <a id="wp-00"></a>`WP-00` | Базовая воспроизводимость и тестовая изоляция | — | Одна локальная команда доказывает состояние репозитория; тесты не затрагивают реальные конфигурации. |
| <a id="wp-01"></a>`WP-01` | Единая входная безопасность и framing | `WP-00` | Все локальные входы используют одну политику admission; сообщения ограничены и типизированы. |
| <a id="wp-02"></a>`WP-02` | Correlation router и cancellation | `WP-00`, `WP-11A` | Внешние и внутренние идентификаторы сопоставляются однозначно; отмена достигает backend. |
| <a id="wp-03"></a>`WP-03` | Sharing policy и Serena project pool | `WP-02`, `WP-11D` | Проекты изолированы; маршрутизация и лимиты экземпляров формализованы. |
| <a id="wp-04"></a>`WP-04` | Разделение legacy и modern MCP | `WP-02`, `WP-11E` | Каждая рекламируемая версия имеет отдельный адаптер и conformance evidence. |
| <a id="wp-05"></a>`WP-05` | Точность capabilities, списков и caching | `WP-02`, `WP-04`, `WP-11E` | Capabilities и route map публикуются из одного согласованного snapshot. |
| <a id="wp-06"></a>`WP-06` | Единый audit writer | `WP-00` | Запись, синхронизация, unlock и health имеют одного владельца и типизированный результат. |
| <a id="wp-07"></a>`WP-07` | Crash-safe adoption/de-adoption | `WP-06`, `WP-11D` | Каждая транзакция завершается commit, rollback или recovery-required с однозначным resume. |
| <a id="wp-08"></a>`WP-08` | Platform lifecycle | `WP-00`, `WP-01`, `WP-11C` | Деревья процессов, deadlines и platform containment проверены на заявленных платформах. |
| <a id="wp-09"></a>`WP-09` | Release и supply chain | `WP-00` | Локальная release verification, immutable pins, SBOM, licenses и trusted publishing связаны с релизом. |
| <a id="wp-10"></a>`WP-10` | Документация и управление состоянием проекта | `WP-09` | Один реестр состояния; версии, capabilities и документация синхронизированы. |
| <a id="wp-11a"></a>`WP-11A` | Architecture guardrails и инвентаризация | `WP-00` | Новый архитектурный долг блокируется; поведение зафиксировано; workers классифицированы. |
| <a id="wp-11b"></a>`WP-11B` | Корень композиции и внедрение зависимостей | `WP-11A` | Production service graph создаётся только в internal/app; global test seams удалены. |
| <a id="wp-11c"></a>`WP-11C` | Supervisor и child-process extraction | `WP-11A`, `WP-11B`, `WP-01` | CLI становится thin adapter; один process-tree owner обслуживает transports. |
| <a id="wp-11d"></a>`WP-11D` | Registry view и install architecture | `WP-11A` | Reload concurrency реализована один раз; planning отделён от effects и transaction journal. |
| <a id="wp-11e"></a>`WP-11E` | MCP и LazyProxy decomposition | `WP-11A`, `WP-02` | Session, routing, dispatch и recovery разделены; LazyProxy имеет явную state machine. |
| <a id="wp-11f"></a>`WP-11F` | Удаление временных путей и hard enforcement | `WP-11B`, `WP-11C`, `WP-11D`, `WP-11E`, `WP-10` | Нет двух production owners; allowlist закрыт; dead code и историческая структура тестов очищены. |

<a id="release-waves"></a>
## 4. Релизные волны

<a id="figure-2"></a>
```mermaid
graph LR
    V0[Wave 0<br/>Baseline + P0] --> V1[Wave 1<br/>Core correctness + guardrails]
    V1 --> V2[Wave 2<br/>Isolation + lifecycle + install]
    V2 --> V3[Wave 3<br/>Modern MCP + decomposition]
    V3 --> V4[Wave 4<br/>Recovery + release + enforcement]
    V4 --> V5[Wave 5<br/>1.0 documentation and final gates]
```

**Рисунок 2 — рекомендуемые релизные волны; номера версий являются ориентиром, а не контрактом.**

| Волна | Основной состав | Допуск |
|---|---|---|
| 0 | `WP-00` subset, `WP-01` P0 | внутренний security hotfix |
| 1 | `WP-02`, `WP-11A`, начало `WP-11B`, pin/release foundation | публичная beta |
| 2 | `WP-03`, `WP-05`, `WP-06`, `WP-08`, `WP-11C`, `WP-11D` | multi-project beta |
| 3 | `WP-04`, `WP-11E` | protocol beta |
| 4 | `WP-07`, остальной `WP-09`, `WP-11F` | release candidate |
| 5 | `WP-10`, финальный security/architecture review | stable 1.0 |

<a id="pr-registry"></a>
## 5. Архитектурный PR registry

| ID | WP | Ветка | Результат | Зависит от | Planning status |
|---|---|---|---|---|---|
| <a id="pr-11a-01"></a>`PR-11A-01` / `A1` | `WP-11A` | `arch/wp11a-archcheck-core` | Детерминированное ядро archcheck | — | implemented-in-pr |
| <a id="pr-11a-02"></a>`PR-11A-02` / `A2` | `WP-11A` | `arch/wp11a-repository-ratchet` | Production policy, owners, baseline, workers и локальный ratchet | `PR-11A-01` | planned |
| <a id="pr-11a-03"></a>`PR-11A-03` / `A3` | `WP-11A` | `test/wp11a-supervisor-install-contracts` | Supervisor/install characterization и общий golden helper | `PR-11A-02` | planned |
| <a id="pr-11a-04"></a>`PR-11A-04` / `A4` | `WP-11A` | `test/wp11a-mcp-lazy-contracts` | MCP aggregator/LazyProxy characterization | `PR-11A-02` | planned |
| <a id="pr-11a-05"></a>`PR-11A-05` / `A5` | `WP-11A` | `test/wp11a-critical-worker-contracts` | Критические worker contracts | `PR-11A-02` | planned |
| <a id="pr-11a-06"></a>`PR-11A-06` / `A6` | `WP-11A` | `arch/wp11a-complete-worker-inventory` | Полный workers.yaml и zero-unclassified gate | `PR-11A-03`, `PR-11A-04`, `PR-11A-05` | planned |
| <a id="pr-11b-01"></a>`PR-11B-01` / `B1` | `WP-11B` | `refactor/wp11b-composition-root` | Production composition root internal/app | `PR-11A-06` | planned |
| <a id="pr-11b-02"></a>`PR-11B-02` / `B2` | `WP-11B` | `refactor/wp11b-cli-readonly-deps` | Shared API для read-only CLI | `PR-11B-01` | planned |
| <a id="pr-11b-03"></a>`PR-11B-03` / `B3` | `WP-11B` | `refactor/wp11b-cli-mutation-deps` | Shared API для mutating CLI | `PR-11B-02` | planned |
| <a id="pr-11b-04"></a>`PR-11B-04` / `B4` | `WP-11B` | `refactor/wp11b-gui-services` | Process-scoped GUI Services | `PR-11B-03` | planned |
| <a id="pr-11b-05"></a>`PR-11B-05` / `B5` | `WP-11B` | `refactor/wp11b-gui-route-wiring` | GUI и route daemon через shared services | `PR-11B-04` | planned |
| <a id="pr-11b-06"></a>`PR-11B-06` / `B6` | `WP-11B` | `refactor/wp11b-testkit` | Общий process test harness | `PR-11B-05` | planned |
| <a id="pr-11b-07"></a>`PR-11B-07` / `B7` | `WP-11B` | `refactor/wp11b-supervisor-deps` | Instance-owned supervisor dependencies | `PR-11B-06` | planned |
| <a id="pr-11b-08"></a>`PR-11B-08` / `B8` | `WP-11B` | `refactor/wp11b-remove-facades` | Удаление compatibility facades и production test hooks | `PR-11B-07` | planned |
| <a id="pr-11c-01"></a>`PR-11C-01` | `WP-11C` | `refactor/wp11c-supervise-inpackage-split` | Механическое разделение supervise.go внутри internal/cli | `PR-11A-06` | planned |
| <a id="pr-11c-02"></a>`PR-11C-02` | `WP-11C` | `refactor/wp11c-supervisor-core` | Извлечение internal/supervisor с compatibility facade | `PR-11C-01`, `PR-11B-07` | planned |
| <a id="pr-11c-03"></a>`PR-11C-03` | `WP-11C` | `refactor/wp11c-childprocess-core` | Общий internal/childprocess | `PR-11C-02`, `WP-01` | planned |
| <a id="pr-11c-04"></a>`PR-11C-04` | `WP-11C` | `refactor/wp11c-stdio-host-runner` | Миграция StdioHost на childprocess runner | `PR-11C-03` | planned |
| <a id="pr-11c-05"></a>`PR-11C-05` | `WP-11C` | `refactor/wp11c-http-host-runner` | Миграция HTTPHost и удаление duplicate lifecycle owners | `PR-11C-04` | planned |
| <a id="pr-11d-01"></a>`PR-11D-01` | `WP-11D` | `refactor/wp11d-registry-snapshot-view` | Общий RegistrySnapshotView | `PR-11A-06` | planned |
| <a id="pr-11d-02"></a>`PR-11D-02` | `WP-11D` | `refactor/wp11d-serena-registry-view` | Миграция Serena resolver | `PR-11D-01` | planned |
| <a id="pr-11d-03"></a>`PR-11D-03` | `WP-11D` | `refactor/wp11d-lsp-registry-view` | Миграция LSP resolver | `PR-11D-01` | planned |
| <a id="pr-11d-04"></a>`PR-11D-04` | `WP-11D` | `refactor/wp11d-install-plan` | Pure InstallPlan extraction | `PR-11A-03` | planned |
| <a id="pr-11d-05"></a>`PR-11D-05` | `WP-11D` | `refactor/wp11d-install-transaction` | Executor и transaction journal | `PR-11D-04` | planned |
| <a id="pr-11d-06"></a>`PR-11D-06` | `WP-11D` | `refactor/wp11d-remove-duplicates` | Удаление duplicate reload/client/process helpers | `PR-11D-02`, `PR-11D-03`, `PR-11D-05` | planned |
| <a id="pr-11e-01"></a>`PR-11E-01` | `WP-11E` | `refactor/wp11e-mcp-session-routing` | Разделение protocol/session/routing | `PR-11A-04`, `WP-02` | planned |
| <a id="pr-11e-02"></a>`PR-11E-02` | `WP-11E` | `refactor/wp11e-mcp-dispatch-recovery` | Разделение fanout/dispatch/recovery | `PR-11E-01` | planned |
| <a id="pr-11e-03"></a>`PR-11E-03` | `WP-11E` | `refactor/wp11e-lazy-state-machine` | Typed LazyProxy state/event reducer | `PR-11A-04` | planned |
| <a id="pr-11e-04"></a>`PR-11E-04` | `WP-11E` | `refactor/wp11e-timeout-lifecycle` | Централизация timeout и lifecycle policy | `PR-11E-02`, `PR-11E-03`, `WP-08` | planned |
| <a id="pr-11e-05"></a>`PR-11E-05` | `WP-11E` | `refactor/wp11e-docs-comments` | Вынос embedded Markdown и исторических комментариев | `PR-11E-04`, `WP-10` | planned |
| <a id="pr-11f-01"></a>`PR-11F-01` | `WP-11F` | `refactor/wp11f-remove-facades` | Удаление оставшихся compatibility facades | `PR-11B-08`, `PR-11C-05`, `PR-11D-06`, `PR-11E-05` | planned |
| <a id="pr-11f-02"></a>`PR-11F-02` | `WP-11F` | `refactor/wp11f-hard-gates` | Закрытие allowlist и включение hard gates | `PR-11F-01` | planned |
| <a id="pr-11f-03"></a>`PR-11F-03` | `WP-11F` | `refactor/wp11f-contract-tests` | Перегруппировка тестов по устойчивым контрактам | `PR-11F-01` | planned |
| <a id="pr-11f-04"></a>`PR-11F-04` | `WP-11F` | `refactor/wp11f-final-audit` | Независимый architecture review и dead-code sweep | `PR-11F-02`, `PR-11F-03` | planned |

`implemented-in-pr` означает, что реализация существует в отдельном PR; review, merge и локальный полный gate подтверждаются отдельно в GitHub.

<a id="jit-planning"></a>
## 6. Правила детализации WP-00—WP-10

Для каждого пакета перед первым implementation PR выполняется:

1. delta-review актуального `master` против finding snapshot;
2. подтверждение или закрытие связанных R-*;
3. выбор решений D-* с наступившим сроком;
4. characterization/falsifier tests;
5. PR decomposition по одному архитектурному или поведенческому инварианту;
6. обновление [`traceability.yaml`](traceability.yaml).

Таким образом roadmap остаётся полным на уровне результатов и зависимостей, но не закрепляет устаревающие file-by-file предположения на месяцы вперёд.

<a id="final-boundary"></a>
## 7. Конечная граница

Программа завершена, когда одновременно:

- все заявленные work packages имеют выполненные exit criteria;
- GitHub не содержит открытых P0/P1;
- архитектурный и release gates воспроизводимы локально;
- один production owner существует для каждого инварианта;
- временные facades и allowlists удалены либо формально приняты как P3 risk с expiry;
- stable release matrix и документация соответствуют фактическому продукту.

[Вернуться к началу](#top)

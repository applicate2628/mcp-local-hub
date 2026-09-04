<a id="top"></a>
# Решения программы

## Оглавление

1. [Статусы](#statuses)
2. [Реестр решений](#registry)
3. [Правила изменения](#change-rules)

<a id="statuses"></a>
## 1. Статусы

- `accepted` — нормативно; меняется новым ADR/amendment;
- `proposed` — рекомендация до owner approval;
- `superseded` — сохранено для истории, не применяется.

<a id="registry"></a>
## 2. Реестр решений

| ID | Статус | Вопрос | Решение/рекомендация | Срок/trigger | Нормативное обоснование |
|---|---|---|---|---|---|
| <a id="d-001"></a>`D-001` | `proposed` | Нужны ли direct per-daemon ports после unified hub? | оставить только как диагностический/compatibility mode, по умолчанию clients направлять в единый ingress | до `WP-01` | — |
| <a id="d-002"></a>`D-002` | `proposed` | Поддерживать ли MCP `2025-03-26`? | удалить, если нет подтверждённого обязательного клиента; иначе реализовать batch | до `WP-04` | — |
| <a id="d-003"></a>`D-003` | `proposed` | Как изолировать Serena? | dynamic project pool по canonical root | до `WP-03` | — |
| <a id="d-004"></a>`D-004` | `proposed` | Нужен ли owner token? | да, required для stable 1.0 | до `WP-01` | — |
| <a id="d-005"></a>`D-005` | `proposed` | Какой defect registry canonical? | GitHub Issues/Projects либо schema-validated files, но не оба независимо | немедленно; до `WP-10` | — |
| <a id="d-006"></a>`D-006` | `proposed` | Поддержка macOS в 1.0? | только при native evidence; иначе официально preview вне 1.0 GA matrix | до `WP-08` и release-candidate части `WP-09` | — |
| <a id="d-007"></a>`D-007` | `proposed` | Как публиковать npm? | OIDC trusted publishing через protected environment | до `WP-09` | — |
| <a id="d-008"></a>`D-008` | `proposed` | Как обрабатывать bidirectional shared server? | isolate per session/client до готовности полноценного reverse router | до `WP-02` | — |
| <a id="d-009"></a>`D-009` | `proposed` | Общий max MCP message size | default 16 MiB, per-manifest lower override, global hard ceiling | до `WP-01` | — |
| <a id="d-010"></a>`D-010` | `proposed` | Какой результат lock-release failure? | committed-but-audit-uncertain, без автоматического replay | до `WP-06` | — |
| <a id="d-011"></a>`D-011` | `accepted` | Какой общий архитектурный стиль? | модульный монолит; один deployable product, явные внутренние модули | до `WP-11A` | [`ADR-0001`](adr/0001-modular-monolith.md) |
| <a id="d-012"></a>`D-012` | `accepted` | Где создаются production dependencies? | только `internal/app` как корень композиции | до `WP-11B` | [`ADR-0002`](adr/0002-single-composition-root.md) |
| <a id="d-013"></a>`D-013` | `proposed` | Какие file-size budgets применять? | 1000 advisory/1500 production hard/2000 test review как стартовые thresholds | до `WP-11A` | — |
| <a id="d-014"></a>`D-014` | `accepted` | Как хранить историю сложных решений? | текущий invariant в code comment, история и trade-offs в ADR | до `WP-11A` | [`ADR-0006`](adr/0006-invariant-history-separation.md) |
| <a id="d-015"></a>`D-015` | `proposed` | Как моделировать LazyProxy/supervisor transitions? | typed state/event reducer + effect runner | до `WP-11E` | — |
| <a id="d-016"></a>`D-016` | `accepted` | Допустимы ли production test hooks? | нет; только injected dependencies и testkit, исключения по ADR | до `WP-11B` | [`ADR-0002`](adr/0002-single-composition-root.md) |
| <a id="d-017"></a>`D-017` | `proposed` | Как безопасно удалять бывшие client-direct процессы после удаления конфигурации? | Только по persisted former signature с TTL, exact executable/argv/identity proof и fail-closed отказом; process-name kill запрещён. | до `DELTA-003`, `WP-07` и `WP-08` | — |

### Уточнения post-audit

- `D-006`: «native CI» из старого аудита трактуется как native evidence на owned/local runner; обычный hosted GitHub CI остаётся запрещён.
- `D-013`: thresholds 1000/1500/2000 являются proposal и подтверждаются inventory актуального master перед A2.
- `D-017`: возник из review закрытого без merge PR №565; старая ветка не является реализацией решения.
- Структурированные связи `D-* → WP-*` являются нормативными в [`traceability.yaml`](traceability.yaml); этот Markdown-реестр обязан использовать те же zero-padded идентификаторы.

<a id="change-rules"></a>
## 3. Правила изменения

1. Accepted decision не переписывается задним числом.
2. Новый trade-off оформляется ADR или amendment.
3. Deadline наступил — соответствующий WP не начинается без явного решения.
4. Решение, противоречащее owner policy, требует отдельного утверждения.

[Вернуться к началу](#top)

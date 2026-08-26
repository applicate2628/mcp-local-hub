<a id="top"></a>
# Реестр решений D-001—D-016

## Оглавление

1. [Правила](#rules)
2. [Реестр](#registry)
3. [Решения, оформленные ADR](#adr-map)
4. [Источники](README.md#references)

<a id="rules"></a>
## 1. Правила

- `accepted` — решение принято и нормативно.
- `recommended` — рекомендуемый вариант; утверждается до указанного рабочего пакета.
- `open-before-*` — реализация пакета заблокирована до решения.
- `interim` — действует временное правило, не создающее второго источника истины.
- `provisional` — числовое значение проверяется inventory до фиксации.
- `revised` — исходная формулировка исправлена после решения отказаться от hosted CI.

<a id="registry"></a>
## 2. Реестр

| ID | Вопрос | Рекомендуемое решение | Срок решения | Статус | Уточнение |
|---|---|---|---|---|---|
| <a id="d-001"></a>`D-001` | Нужны ли direct per-daemon ports после unified hub? | оставить только как диагностический/compatibility mode, по умолчанию clients направлять в единый ingress | до WP-1 | recommended | — |
| <a id="d-002"></a>`D-002` | Поддерживать ли MCP `2025-03-26`? | удалить, если нет подтверждённого обязательного клиента; иначе реализовать batch | до WP-4 | open-before-WP-04 | — |
| <a id="d-003"></a>`D-003` | Как изолировать Serena? | dynamic project pool по canonical root | до WP-3 | recommended | — |
| <a id="d-004"></a>`D-004` | Нужен ли owner token? | да, required для stable 1.0 | до WP-1 | recommended | — |
| <a id="d-005"></a>`D-005` | Какой defect registry canonical? | GitHub Issues/Projects либо schema-validated files, но не оба независимо | немедленно | interim | До отдельного решения live status хранится только в GitHub; документы не дублируют его. |
| <a id="d-006"></a>`D-006` | Поддержка macOS в 1.0? | native release evidence на каждой заявленной stable-платформе; иначе platform имеет preview-статус и исключается из 1.0 stable matrix | до RC | revised | Stable support требует native release evidence на заявленной платформе; hosted CI не обязателен. |
| <a id="d-007"></a>`D-007` | Как публиковать npm? | OIDC trusted publishing через protected environment | до WP-9 | recommended | — |
| <a id="d-008"></a>`D-008` | Как обрабатывать bidirectional shared server? | isolate per session/client до готовности полноценного reverse router | до WP-2 | recommended | — |
| <a id="d-009"></a>`D-009` | Общий max MCP message size | default 16 MiB, per-manifest lower override, global hard ceiling | до WP-1 | recommended | — |
| <a id="d-010"></a>`D-010` | Какой результат lock-release failure? | committed-but-audit-uncertain, без автоматического replay | до WP-6 | recommended | — |
| <a id="d-011"></a>`D-011` | Какой общий архитектурный стиль? | модульный монолит; один deployable product, явные внутренние модули | до WP-11A | accepted | Зафиксировано ADR-0001. |
| <a id="d-012"></a>`D-012` | Где создаются production dependencies? | только `internal/app` как корень композиции | до WP-11B | accepted | Зафиксировано ADR-0002. |
| <a id="d-013"></a>`D-013` | Какие file-size budgets применять? | 1000 advisory/1500 production hard/2000 test review как стартовые thresholds | до WP-11A | provisional | Порог 1000/1500/2000 проверяется inventory в PR-11A-02 и может быть откорректирован reviewable diff. |
| <a id="d-014"></a>`D-014` | Как хранить историю сложных решений? | текущий invariant в code comment, история и trade-offs в ADR | до WP-11A | accepted | Зафиксировано governance и ADR-0005. |
| <a id="d-015"></a>`D-015` | Как моделировать LazyProxy/supervisor transitions? | typed state/event reducer + effect runner | до WP-11E | recommended | — |
| <a id="d-016"></a>`D-016` | Допустимы ли production test hooks? | нет; только injected dependencies и testkit, исключения по ADR | до WP-11B | accepted | Production test hooks запрещены; временное исключение требует ADR. |

<a id="d-005-interim"></a>
### D-005 — временный порядок статуса

До отдельного owner decision GitHub Pull Request и Issues являются live status. `roadmap.md` не содержит вручную редактируемых процентов выполнения, а `traceability.yaml` не хранит open/closed state.

<a id="adr-map"></a>
## 3. Решения, оформленные ADR

| ADR | Решение |
|---|---|
| [ADR-0001](adr/0001-modular-monolith.md) | D-011: модульный монолит |
| [ADR-0002](adr/0002-single-composition-root.md) | D-012: `internal/app` как composition root |
| [ADR-0003](adr/0003-local-verification.md) | локальная верификация без hosted CI |
| [ADR-0004](adr/0004-architecture-ratchet.md) | точный baseline и ratchet |
| [ADR-0005](adr/0005-characterization-before-extraction.md) | characterization перед extraction |

[Вернуться к началу](#top)

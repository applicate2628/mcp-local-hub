<a id="top"></a>
# ADR-0004: Архитектурный ratchet и точный baseline

**Статус:** accepted<br>
**Дата:** 2026-08-27

## Оглавление

1. [Контекст](#context)
2. [Решение](#decision)
3. [Последствия](#consequences)
4. [Рассмотренные альтернативы](#alternatives)
5. [Нумерованные источники](#sources)

<a id="context"></a>
## 1. Контекст

Немедленная очистка всего legacy debt нереалистична, но разрешение безлимитного allowlist допускает дальнейший рост.

<a id="decision"></a>
## 2. Решение

Фиксировать существующий долг точными fingerprints, owner, work package, remove-by и max metric. Новый, выросший, просроченный, stale или unowned debt блокирует verify.

<a id="consequences"></a>
## 3. Последствия

- Legacy debt становится измеряемым.
- Простое перемещение строки не меняет fingerprint.
- Уменьшение baseline обязательно принимается тем же PR.
- Workers переходят в отдельный ownership registry.

<a id="alternatives"></a>
## 4. Рассмотренные альтернативы

- Полный запрет старого долга сразу — отклонён как блокирующий развитие.
- Нечёткий path allowlist — отклонён как fail-open.

<a id="sources"></a>
## 5. Нумерованные источники

1. [WP-11A plan](../plans/wp-11a-to-11f.md#wp-11a)
2. [PR №602](../README.md#ref-3)

[Вернуться к началу](#top)

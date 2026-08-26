<a id="top"></a>
# ADR-0005: Characterization перед перемещением владельца

**Статус:** accepted  
**Дата:** 2026-08-27

## Оглавление

1. [Контекст](#context)
2. [Решение](#decision)
3. [Последствия](#consequences)
4. [Рассмотренные альтернативы](#alternatives)
5. [Нумерованные источники](#sources)

<a id="context"></a>
## 1. Контекст

Supervisor, install, MCP aggregator и LazyProxy содержат сложные наблюдаемые contracts, которые легко изменить случайно при package extraction.

<a id="decision"></a>
## 2. Решение

До extraction добавлять deterministic characterization tests. Изменение golden требует отдельного compatibility decision; найденный defect исправляется отдельным PR.

<a id="consequences"></a>
## 3. Последствия

- Refactor становится behaviour-preserving.
- Причинно значимый порядок не скрывается сортировкой.
- Rollback может сравниваться с прежним contract.
- A3/A4/A5 предшествуют удалению старых owners.

<a id="alternatives"></a>
## 4. Рассмотренные альтернативы

- Перемещать код и исправлять поведение одновременно — отклонено из-за невозможности локализовать регрессию.
- Полагаться только на unit tests — отклонено как недостаточное покрытие внешних контрактов.

<a id="sources"></a>
## 5. Нумерованные источники

1. [Design principle](../design.md#principles)
2. [Roadmap PR registry](../roadmap.md#pr-registry)

[Вернуться к началу](#top)

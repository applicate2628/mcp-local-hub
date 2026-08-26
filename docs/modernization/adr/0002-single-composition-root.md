<a id="top"></a>
# ADR-0002: Единственный composition root в internal/app

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

CLI и GUI создают долгоживущие сервисы в разных местах, что порождает несколько owners, per-call API construction и глобальные test seams.

<a id="decision"></a>
## 2. Решение

Production dependency graph создаётся только в `internal/app`. CLI и GUI получают typed dependencies; compatibility constructors имеют expiry и removal tests.

<a id="consequences"></a>
## 3. Последствия

- Один process-scoped API и service bundle.
- Reverse-order bounded shutdown.
- Тесты используют instance dependencies.
- Временные facades удаляются в PR-11B-08/PR-11F-01.

<a id="alternatives"></a>
## 4. Рассмотренные альтернативы

- Service locator — отклонён как скрытое глобальное состояние.
- Создание API в handlers — отклонено из-за нескольких owners и lifecycle drift.

<a id="sources"></a>
## 5. Нумерованные источники

1. [Target structure](../design.md#target-structure)
2. [WP-11B plan](../plans/wp-11a-to-11f.md#wp-11b)

[Вернуться к началу](#top)

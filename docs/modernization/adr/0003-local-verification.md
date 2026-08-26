<a id="top"></a>
# ADR-0003: Локальная верификация без hosted CI для обычной разработки

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

Владелец ограничил расход GitHub Actions. При этом отсутствие внешнего CI не должно снижать доказательность PR и релиза.

<a id="decision"></a>
## 2. Решение

Все ordinary PR/merge gates выполняются versioned local scripts и сохраняют evidence в `.reports/`. GitHub Actions используется только release-only workflow публикации.

<a id="consequences"></a>
## 3. Последствия

- Нет автоматических hosted jobs на push/PR.
- Exact commands и exit codes сохраняются локально.
- Codex review не заменяет local gate.
- Release publish проверяет локальный verification manifest.

<a id="alternatives"></a>
## 4. Рассмотренные альтернативы

- Вернуть обязательный GitHub CI — отклонено владельцем.
- Не иметь формализованных gates — отклонено как fail-open процесс.

<a id="sources"></a>
## 5. Нумерованные источники

1. [Governance](../governance.md#local-verification)
2. [Audit decision](../README.md#ref-1)

[Вернуться к началу](#top)

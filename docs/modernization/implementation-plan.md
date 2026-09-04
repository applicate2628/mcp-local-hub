<a id="top"></a>
# Canonical Modernization Documentation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: use `superpowers:executing-plans` or `superpowers:subagent-driven-development` task-by-task.

**Goal:** создать компактный канонический пакет design/roadmap/governance/traceability без изменения production-кода.

**Architecture:** нормативные документы находятся в `docs/modernization`; GitHub остаётся live-status owner; `traceability.yaml` хранит устойчивые связи; исторический аудит не переписывается.

**Tech Stack:** Markdown, YAML, GitHub Pull Request, локальная проверка ссылок и схем.

**Spec:** [`design.md`](design.md)

## Global Constraints

- Hosted CI для обычных PR не добавляется.
- Исторический audit SHA и current baseline SHA не смешиваются.
- Все оглавления, рисунки, ADR и источники имеют ссылки.
- незаполненные placeholders, неизвестные IDs и несуществующие `archcheck` flags запрещены.
- PR является documentation-only.

---

### Task 1: Source-of-truth и идентификаторы

**Files:** `README.md`, `glossary.md`, `decisions.md`

- [x] Зафиксировать иерархию источников истины.
- [x] Ввести `PR-11A-01`-стиль ID и сохранить A1/B1 как aliases.
- [x] Исключить ручной live-status document до решения D-005.
- [x] Разделить audit snapshot, execution base и baseline generated-from.

### Task 2: Design и roadmap

**Files:** `design.md`, `roadmap.md`

- [x] Описать конечную цель 1.0 и целевой модульный монолит.
- [x] Зафиксировать 17 WP и dependency DAG.
- [x] Зафиксировать 34 архитектурных PR для WP-11A—WP-11F.
- [x] Не закреплять преждевременный PR count для WP-00—WP-10.

### Task 3: Governance и исправление противоречий

**Files:** `governance.md`, `plans/wp-11a-to-11f.md`

- [x] Удалить требования общего GitHub CI.
- [x] Исправить `archcheck` команды и YAML examples.
- [x] Устранить release-manifest self-reference.
- [x] Разделить review PASS, local gate и merge status.

### Task 4: Трассируемость и рабочие планы

**Files:** `traceability.yaml`, `plans/wp-00-to-10.md`

- [x] Перенести 36 findings, 236 requirements, 20 AT и 16 decisions из audit snapshot.
- [x] Связать findings с primary/supporting WP.
- [x] Описать just-in-time decomposition для WP-00—WP-10.

### Task 5: Architecture Decision Records

**Files:** `adr/0001`—`adr/0005`

- [x] Зафиксировать modular monolith.
- [x] Зафиксировать single composition root.
- [x] Зафиксировать local verification without hosted CI.
- [x] Зафиксировать architecture ratchet.
- [x] Зафиксировать characterization before extraction.

### Task 6: Самопроверка и PR

- [x] Проверить YAML parser.
- [x] Проверить внутренние Markdown links и anchors.
- [x] Проверить отсутствие незаполненных placeholders и устаревших CI-команд.
- [x] Проверить IDs и неизвестные `ViolationKind` в examples.
- [x] Добавить ссылку на пакет из `docs/architecture-highlights.md`.
- [x] Создать documentation-only PR и запросить Codex review.

[Вернуться к началу](#top)

<a id="top"></a>
# Канонический пакет модернизации `mcp-local-hub`

## Оглавление

1. [Назначение](#purpose)
2. [Источники истины](#sources-of-truth)
3. [Текущее состояние](#current-state)
4. [Порядок чтения](#reading-order)
5. [Состав пакета](#contents)
6. [Правила обновления](#update-rules)

<a id="purpose"></a>
## 1. Назначение

Этот каталог является нормативной точкой входа для программы стабилизации и архитектурного упрощения `mcp-local-hub`. Он не переписывает историю аудита и не подменяет live-состояние GitHub. Его задача — связать:

- неизменяемую доказательную базу;
- актуальный `master`;
- требования и приёмочные тесты;
- рабочие пакеты и Pull Request (PR; запросы на включение изменений);
- решения и Architecture Decision Record (ADR; записи архитектурных решений);
- локальные проверочные доказательства.

<a id="sources-of-truth"></a>
## 2. Источники истины

| Предмет | Канонический источник | Правило |
|---|---|---|
| Исторические findings `R-01—R-36` | [`audits/2026-08-12-audit-register.md`](audits/2026-08-12-audit-register.md) | Самодостаточный реестр всех 36 findings; SHA-256 `328fa4aac0b4343f252a4e7783b639b982d6d25cbfe8406c3019243ea49e6518`. Полный длинный отчёт закреплён исходным SHA-256 `5325de5e644dac96149ac94143c3e47d0895a678fddd031225f7413181f187b4`. |
| Формулировки 236 требований | [`requirements.yaml`](requirements.yaml) | Извлечены из аудита; ID и формулировка доступны без внешнего файла. |
| 20 архитектурных приёмочных тестов | [`acceptance-tests.yaml`](acceptance-tests.yaml) | Содержит test/expected и строку источника. |
| Изменения после аудита | [`deltas/2026-08-27-master-83b49f4.md`](deltas/2026-08-27-master-83b49f4.md) | Не закрывают finding автоматически; требуют revalidation на exact `master`. |
| Целевая архитектура | [`design.md`](design.md) | Нормативные границы и владельцы. |
| Порядок работ | [`roadmap.md`](roadmap.md) и [`traceability.yaml`](traceability.yaml) | Markdown объясняет; YAML является машиночитаемым графом. |
| Правила исполнения | [`governance.md`](governance.md) | Локальные gates, review, rollback, accepted risk. |
| Решения | [`decisions.md`](decisions.md) и [`adr/`](adr/) | `accepted` решения меняются только новым ADR/amendment. |
| Live PR/issue status | GitHub | В репозитории не ведётся конкурирующий ручной `status.md`. |
| Архитектурный долг после A2 | `architecture/baseline.yaml`, `architecture/workers.yaml` | Создаются из актуального дерева, не из старого audit SHA. |

Ветки не являются источником истины сами по себе. Нормативны только:

1. слитый `master`;
2. exact HEAD открытого PR, явно указанного в traceability;
3. неизменяемые evidence-артефакты с hash.

Закрытые, abandoned и superseded ветки используются только как доказательная база для delta-review.

<a id="current-state"></a>
## 3. Текущее состояние

- `master_snapshot`: `83b49f4d616e97613e70070bfbc62e7958c01fc4`;
- audit snapshot: `c0527c1232180dd37cd92da5a328bdcdadae1969`;
- `PR-11A-01` / PR №602: реализация A1 существует в отдельном PR и получила Codex review PASS; merge и полный локальный repository gate отслеживаются отдельно;
- PR №604 уже слит в `master` и является крупным post-audit delta, а не косметическим hotfix;
- этот пакет находится в PR №605 и не включает A2;
- GitHub-hosted Continuous Integration (CI; непрерывная интеграция) для обычной разработки, PR и merge не используется;
- `.github/workflows/npm-publish.yml` остаётся только release-механизмом.

Перед A2 обязательно повторно просканировать актуальный `master` после merge A1 и этого документационного PR. Нельзя генерировать baseline от `c0527c1`.

<a id="reading-order"></a>
## 4. Порядок чтения

```text
README
  ↓
design.md
  ↓
roadmap.md
  ↓
governance.md
  ↓
traceability.yaml + requirements.yaml + acceptance-tests.yaml
  ↓
plans/
  ↓
decisions.md + adr/
  ↓
deltas/
```

<a id="contents"></a>
## 5. Состав пакета

- [`design.md`](design.md) — конечная цель и целевая архитектура;
- [`roadmap.md`](roadmap.md) — Directed Acyclic Graph (DAG; ориентированный ациклический граф), рабочие пакеты и 34 архитектурных PR;
- [`governance.md`](governance.md) — правила реализации и проверки;
- [`implementation-plan.md`](implementation-plan.md) — порядок исполнения ближайших инкрементов;
- [`plans/wp-00-to-10.md`](plans/wp-00-to-10.md) — JIT-планирование продуктовой стабилизации;
- [`plans/wp-11a-to-11f.md`](plans/wp-11a-to-11f.md) — архитектурная программа;
- [`traceability.yaml`](traceability.yaml) — finding→requirement→WP→PR/test;
- [`requirements.yaml`](requirements.yaml) — полные формулировки требований;
- [`acceptance-tests.yaml`](acceptance-tests.yaml) — полные формулировки AT;
- [`decisions.md`](decisions.md) — решения D-001—D-017;
- [`glossary.md`](glossary.md) — термины;
- [`audits/2026-08-12-audit-register.md`](audits/2026-08-12-audit-register.md) — нормативный реестр 36 findings для clean-clone delta-review;
- [`deltas/`](deltas/) — переоценка актуального дерева и веток;
- [`adr/`](adr/) — принятые решения, включая [`ADR-0006`](adr/0006-invariant-history-separation.md) о разделении текущего инварианта и истории.

<a id="update-rules"></a>
## 6. Правила обновления

1. Finding `R-*` не меняется задним числом; новый факт получает `DELTA-*`.
2. Requirement не переформулируется молча; создаётся amendment либо новый ID.
3. `traceability.yaml` меняется в том же PR, который меняет зависимость, owner или статус.
4. Слияние hotfix не закрывает finding по названию; нужен exact-tree regression evidence.
5. Go-goroutine inventory и межъязыковой process-lifecycle inventory ведутся раздельно.
6. После каждого существенного merge выполняется branch/master delta-review.
7. Все ссылки, YAML, PR/WP gates и формулировки требований проверяются локально; GitHub Actions не запускается.
8. Машинный PR-граф считается полным только при явных `deps` и `wp_gates`; неявное наследование запрещено.

[Вернуться к началу](#top)

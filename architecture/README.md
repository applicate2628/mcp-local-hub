<a id="top"></a>
# Архитектурный ratchet репозитория

**Рабочий пакет:** `PR-11A-02` / A2<br>
**База подготовки:** `master@f6df9f65387fd89d1fe492b6b89d076edd80bddb`<br>
**Статус:** draft до генерации и review exact baseline.

## Оглавление

1. [Цель](#goal)
2. [Минимальный состав](#scope)
3. [Что намеренно не добавляется](#non-goals)
4. [Генерация baseline](#baseline)
5. [Worker inventory](#workers)
6. [Критерии готовности](#done)

<a id="goal"></a>
## 1. Цель

A2 подключает уже слитый `archcheck` к реальному репозиторию. Старый архитектурный долг фиксируется точными fingerprints, а новый, выросший, просроченный, stale либо unowned debt должен блокировать локальную проверку.

A2 не рефакторит runtime и не исправляет найденные нарушения. Его задача — сделать дальнейшую деградацию видимой и воспроизводимой.

<a id="scope"></a>
## 2. Минимальный состав

| Файл | Ответственность |
|---|---|
| [`policy.yaml`](policy.yaml) | Сканируемые корни и машинные правила |
| [`owners.yaml`](owners.yaml) | Specific-first назначение владельца и рабочего пакета |
| [`workers.yaml`](workers.yaml) | Отдельный реестр классифицированных Go goroutine |
| `baseline.yaml` | Генерируется только из clean exact checkout и добавляется до merge A2 |
| [`../internal/archguard/repository_contract_test.go`](../internal/archguard/repository_contract_test.go) | Один компактный тест согласованности policy и канонических registries |
| [`../docs/modernization/deltas/2026-09-04-master-f6df9f6.md`](../docs/modernization/deltas/2026-09-04-master-f6df9f6.md) | Актуальная входная delta-review |

Начальные file budgets намеренно консервативны:

```text
production advisory: 1200 строк
production hard:     2200 строк
test review:         2800 строк
```

Перед merge A2 они либо подтверждаются распределением текущего scan, либо меняются одним reviewable diff согласно `D-013`.

<a id="non-goals"></a>
## 3. Что намеренно не добавляется

A2 не создаёт:

- отдельный `doccheck` binary;
- генератор Markdown или Mermaid;
- JSON Schema framework;
- GitHub-hosted CI;
- синхронизацию live-status из GitHub в YAML;
- новый plugin API;
- параллельный dashboard архитектурного долга.

`traceability.yaml` остаётся точным машинным графом, Markdown — объяснением, GitHub — владельцем live-status.

<a id="baseline"></a>
## 4. Генерация baseline

Baseline нельзя вычислять вручную или переносить со старого снимка.

В чистом checkout ветки A2:

```bash
set -euo pipefail

test -z "$(git status --porcelain=v1)"
scan_sha="$(git rev-parse HEAD)"
mkdir -p .reports

go test ./internal/archguard ./tools/archcheck -count=1

go run ./tools/archcheck scan \
  --root . \
  --policy architecture/policy.yaml \
  --report-json .reports/architecture-inventory.json \
  --report-md .reports/architecture-inventory.md \
  > .reports/architecture-inventory.stdout.json

go run ./tools/archcheck baseline \
  --root . \
  --policy architecture/policy.yaml \
  --owners architecture/owners.yaml \
  --generated-from "$scan_sha" \
  --baseline architecture/baseline.yaml \
  --report-json .reports/architecture-baseline-source.json \
  --report-md .reports/architecture-baseline-source.md

go run ./tools/archcheck verify \
  --root . \
  --policy architecture/policy.yaml \
  --owners architecture/owners.yaml \
  --baseline architecture/baseline.yaml \
  --workers architecture/workers.yaml \
  --report-json .reports/architecture-verify-source.json \
  --report-md .reports/architecture-verify-source.md
```

После генерации обязательно проверить:

- `generated_from` равен exact clean commit, который был просканирован до добавления `baseline.yaml`;
- после этого commit изменения в `cmd`, `internal` или `tools` отсутствуют; иначе baseline генерируется заново;
- ни одна baseline entry не получила `owner: architecture-triage`;
- широких path allowlists нет;
- `max_metric` равен текущей фактической метрике;
- причины и сроки удаления соответствуют primary рабочему пакету;
- повторный `verify` завершается кодом `0`.

Если до генерации изменился `master`, ветка обновляется и весь scan повторяется.

<a id="workers"></a>
## 5. Worker inventory

`workers.yaml` на первом шаге пуст. Это не означает отсутствие goroutine.

Команда:

```bash
go run ./tools/archcheck workers \
  --root . \
  --policy architecture/policy.yaml \
  --baseline architecture/baseline.yaml \
  --workers architecture/workers.yaml \
  --report-json .reports/workers.json \
  --report-md .reports/workers.md
```

используется для просмотра текущих Go workers. В A2 они могут оставаться точными baseline entries. Критические workers переходят в реестр в A5, полный `zero-unclassified` достигается в A6.

Native, Python и другие дочерние процессы не считаются Go workers и ведутся отдельным lifecycle inventory.

<a id="done"></a>
## 6. Критерии готовности

A2 готов к merge только когда:

1. `baseline.yaml` создан из exact clean source commit и содержит его SHA в `generated_from`.
2. Между `generated_from` и финальным HEAD нет изменений в `cmd`, `internal` или `tools`.
3. `archcheck verify` проходит.
4. Тест `TestRepositoryArchitectureContracts` проходит.
5. Ни одна текущая entry не назначена временному `architecture-triage`.
6. File budgets подтверждены фактическим распределением.
7. Policy не содержит фиктивных разрешений ради PASS.
8. Полный diff не меняет production runtime.
9. GitHub Actions для обычной разработки не добавлены.
10. Codex review выполнен на exact финальном HEAD либо явно зафиксировано ограничение сервиса без сокрытия результата.

[Вернуться к началу](#top)

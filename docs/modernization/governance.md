<a id="top"></a>
# Governance программы модернизации

## Оглавление

1. [Основные правила](#rules)
2. [Branch и PR policy](#branch-policy)
3. [Локальные gates](#local-gates)
4. [Review](#review)
5. [Baseline и accepted risk](#baseline)
6. [Rollback](#rollback)
7. [Документация и evidence](#evidence)
8. [Release protocol](#release)

<a id="rules"></a>
## 1. Основные правила

1. Один PR меняет один проверяемый инвариант.
2. Characterization предшествует extraction.
3. Поведенческое исправление не маскируется под refactor.
4. Старый и новый owner не остаются одновременно без compatibility facade с owner+expiry.
5. Security checks не отключаются ради переноса кода.
6. Публичные CLI/HTTP/JSON/error/persisted contracts меняются только отдельным compatibility decision.
7. Любое заявление PASS относится к exact HEAD и конкретному gate.

<a id="branch-policy"></a>
## 2. Branch и PR policy

Нормативны:

- `master`;
- exact HEAD открытого PR;
- hash-bound evidence artifact.

Не нормативны сами по себе:

- старые feature branches;
- закрытые PR без merge;
- superseded Codex branches;
- локальные candidate commits;
- branch names без подтверждённого ancestry/status.

Перед использованием старой ветки:

1. compare с актуальным `master`;
2. определить уникальные коммиты;
3. проверить, не слито ли решение через другой PR;
4. выполнить security/architecture review;
5. открыть новый PR от актуального `master`.

Пример: `fix/auto-reaper-client-direct` не возвращается как готовое решение. Его PR закрыт без merge, а review показал фундаментальный разрыв между config-present и config-absent detection. Сохраняется только `DELTA-003`.

<a id="local-verification"></a>
<a id="local-gates"></a>
## 3. Локальные gates

GitHub-hosted CI для обычной разработки, PR и merge не используется.

### 3.1. Минимальный PR gate

До появления versioned `scripts/verify-local.*` используется явный fail-fast запуск, который сохраняет результаты в `.reports/` и не маскирует exit code через `tee`:

```bash
set -euo pipefail
mkdir -p .reports
git rev-parse HEAD > .reports/exact-head.txt
git status --porcelain=v1 > .reports/worktree-status.txt
if [ -s .reports/worktree-status.txt ]; then
  cat .reports/worktree-status.txt >&2
  echo "worktree must be clean before verification" >&2
  exit 1
fi

git diff --check 2>&1 | tee .reports/git-diff-check.txt
go build ./... 2>&1 | tee .reports/go-build.txt
go vet ./... 2>&1 | tee .reports/go-vet.txt
go test -count=1 -timeout 5m ./... 2>&1 | tee .reports/go-test.txt
```

После A2 перед build/vet/test обязательно выполняется полный архитектурный ratchet с двумя отчётами:

```bash
go run ./tools/archcheck verify \
  --root . \
  --policy architecture/policy.yaml \
  --owners architecture/owners.yaml \
  --baseline architecture/baseline.yaml \
  --workers architecture/workers.yaml \
  --report-json .reports/architecture-report.json \
  --report-md .reports/architecture-report.md \
  2>&1 | tee .reports/architecture-verify.txt
```

PowerShell-эквивалент обязан сохранять те же логические артефакты и завершаться исходным ненулевым кодом первой упавшей команды. После `WP-00` ручные блоки заменяются versioned `scripts/verify-local.sh` и `scripts/verify-local.ps1`; семантика отчётов остаётся контрактом.

### 3.2. Affected gates

- concurrency/lifecycle: `go test -race` и repeated/shuffle;
- `test_state_path_env`: tagged suites;
- frontend: test/typecheck/build/E2E;
- Python: package tests, Ruff/type checks, safe no-launch contract tests;
- manifests: schema, immutable pins, release projection `--check`;
- process changes: platform-native containment and join tests;
- docs: YAML parse, ID/DAG/link/anchor/command-schema checks.

### 3.3. Доказательства

Каждый script сохраняет в `.reports/`:

- exact commit;
- clean-tree verdict;
- command;
- exit code;
- tool versions;
- artifact hashes;
- platform.

`.reports/` не является нормативным status registry; это evidence текущего прогона.

<a id="review"></a>
## 4. Review

Порядок:

1. локальный gate;
2. push в отдельную ветку;
3. PR;
4. Codex review exact HEAD;
5. исправление inline findings в том же PR;
6. самостоятельный class-sweep;
7. для security/lifecycle изменений — независимый deep review;
8. повторный review exact HEAD;
9. merge без bypass.

Codex 👍/PASS означает отсутствие замечаний данного review, но не заменяет полный локальный gate и merge status.

<a id="baseline"></a>
## 5. Baseline и accepted risk

A2 создаёт baseline только из актуального `master` после merge зависимостей.

Каждая запись содержит:

- owner;
- primary work package или PR;
- reason;
- remove_by;
- max_metric для измеряемого нарушения.

Запрещены:

- catch-all owner как финальное состояние;
- несколько PR ID, закодированных одной строкой через `/`;
- бессрочный waiver;
- ручное добавление нового нарушения без отдельного решения;
- перенос старого audit SHA в `generated_from`.

Уменьшение долга удаляется в том же PR. Candidate baseline генерируется во временный файл и принимается reviewable diff; деструктивный несуществующий baseline-prune flag не используется.

<a id="rollback"></a>
## 6. Rollback

| Класс | Откат |
|---|---|
| docs/A1/A2 | revert одного PR; runtime не меняется |
| characterization/registry | revert test/evidence commit; найденный реальный дефект не удаляется |
| extraction | revert конкретного wiring PR; compatibility facade остаётся до removal gate |
| release metadata | откат descriptor/projection commit с exact hash verification |
| security hotfix | отдельный emergency revert/forward-fix с threat review |

<a id="evidence"></a>
## 7. Документация и evidence

- audit snapshot неизменяем;
- requirement formulation хранится в `requirements.yaml`;
- новый факт получает `DELTA-*`;
- accepted decision получает ADR;
- live status хранится в GitHub до решения D-005;
- roadmap не содержит ручной копии live status;
- hotfix не закрывает finding без regression evidence;
- historical comments сокращаются только после переноса rationale в ADR/evidence.

<a id="release"></a>
## 8. Release protocol

Обычный PR не запускает GitHub Actions. `npm-publish.yml` используется только для выпуска.

Во избежание self-reference:

```text
C  = проверенный source commit
C2 = release metadata commit, parent(C2)=C
tag -> C2
manifest.source_commit = C
diff C..C2 ограничен разрешёнными release metadata
```

Publish workflow проверяет envelope и payload, но не заменяет локальные tests. Long-lived token заменяется OpenID Connect (OIDC; OpenID Connect) trusted publishing после отдельного решения и проверки.

[Вернуться к началу](#top)

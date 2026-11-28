# Область приёмки Source727d и восстановленного PR #666

## Содержание

1. [Вывод](#вывод)
2. [Деревья и доказательство](#деревья-и-доказательство)
3. [Семь тестовых файлов](#семь-тестовых-файлов)
4. [Условия публикации](#условия-публикации)
5. [Минимальное продолжение](#минимальное-продолжение)
6. [Источники](#источники)

## Вывод

**Полный PASS Source727d, если он будет получен, не квалифицирует restored
head33eaf или public head5ad PR #666.** Между restored33 и Source727d различаются
**802 пути**. Среди различий есть production Go code, тесты, build scripts,
Windows Product contracts и каталог Serena.

Это самостоятельное DATA review. Во время сравнения Go, тесты, модели и native
контроллеры не запускались; Git index, refs и существующие исходники не менялись.
Полная приёмка Source727d относится к отдельному текущему native run. Полная
приёмка exact33 на момент этого снимка — **NOT_RUN**.

Таблица 1. Направление сравнения: recovered33 → Source727d.

| Категория | Изменены | Только Source727d | Только recovered33 |
|---|---:|---:|---:|
| Go files вне _test.go | 149 | 62 | 48 |
| Go test files | 128 | 161 | 49 |
| Остальные пути | 29 | 95 | 81 |
| **Всего** | **306** | **318** | **178** |

Полный path/mode/blob ledger находится в `whole-tree-delta.json`.
Его `source_only` означает Source727d, а `public_only` — restored33; последнее
название не означает, что commit33 уже опубликован. Публичный head на момент
получения GitHub metadata оставался5ad [1].

## Деревья и доказательство

Таблица 2. Точные исходники.

| Роль | Commit | Git tree | Файлы |
|---|---|---|---:|
| Public #666 | 5ad374eb60cadb8ad56ea167e3fb06edb44bdc18 | 0f062e2bc3ae31bb9eab2c44862f1ec1a7a0ddd3 | 3369 |
| Recovered06 | 06f77b3856e43ec5e8e3736123aedb896ae58817 | 645fd0dd7281cbf66a334500fe8aa31cd9469885 | 3369 |
| Recovered33 | 33eaf8af8fc46cb231f48c70f4775922393245b1 | 9e46575153d8a70c98c5629fdaa4767f61401f6c | 3369 |
| Product Source3ebe | 3ebe335a2f666a5279d7fafc31a6f4dc3feedff4 | 67c13b8fc1317b4fd60e3135a3eb8a376fe01817 | 3508 |
| Test Source727d | 727d66753814dc218e26d699e57402db2d13190a | d959ed0a4f8ab02947aec7569f3c0535a3cff533 | 3509 |

Полная рекурсивная public tree получена из GitHub с `truncated=false` [2].
Из path/mode/blob записей независимо пересчитана tree0f062e2b.

Семь afterimages прочитаны полностью из восстановленного checkout и совпали
с точными Git blobs в исходных patches. Замена пяти blobs первого commit
в публичной карте дала tree645fd0dd; замена следующих двух — tree9e465751.
Оба результата совпали с исходными raw commit objects. Git IDs обоих raw objects
также пересчитаны:06f77 и33eaf. История5ad→06f77→33eaf сохранена [3].

Source727d сопоставлен с принятой CLOSED картой3509files, SHA-256
`a3483e469b606fa297a4b33698cd22db7bf25493023f3117feeafb52bb9db5f1`.
Воспроизводимая source capsule уже опубликована отдельно [4].
Сравнение охватывает все пути и modes. Recovered33 отличается от Source3ebe
на796путях; public5ad отличается от Source727d на804путях.

Git ancestry Source727d от33 здесь не утверждается. Raw parent727d —
`0145a2547a3025cc518cf3e2fa89d2e22cb73c08`. DATA реконструкция состава из
раннего source и настоящий Git parent — разные связи.

## Семь тестовых файлов

Таблица 3. Afterimages внутри internal/cli/.

| Файл | Полное совпадение bytes | Дополнительное различие |
|---|---|---|
| supervise_respawn_arm_gen_test.go | Да | Нет |
| supervisor_controller_orphan_reap_test.go | Да | Нет |
| supervise_test.go | Нет | Source727d дополнительно задаёт canonical-path test seam в StatusIPC_ReconcileReady |
| upgrade_gui_fence_windows_test.go | Нет | Exact33 использует JournalAllowance/Context и context-aware process observation; Source727d использует прежние production signatures |
| upgrade_gui_qualified_fixture_windows_test.go | Нет | Source727d выделяет общий receiveWindowsCanonicalEntryFixture; exact33 требует JournalAllowance |
| supervise_reconcile_ipc_test.go | Нет | Source727d дополнительно отключает builtin-route seeding в общей fixture |
| workspace_cmd_test.go | Нет | Exact33 вызывает register с --backend serena; Source727d — без этого аргумента |

`seven-test-files.json` содержит SHA-256 и Git blob IDs всех afterimages.
`five-test-file-deltas.patch` — точный readable diff recovered33 → Source727d:
**25 добавленных и9удалённых строк**. Совпадение двух afterimages и присутствие
похожих исправлений в остальных файлах не заменяют whole-revision qualification.

Материальны также production-различия: меняются `build.ps1`,
`windows-product-artifacts.json`, `servers/serena/manifest.yaml`,
`internal/process/contained_stream_observed.go` и namespace tool.
В recovered33 есть `internal/binaryadmission/windows_product_identity_v2.go`,
которого нет в Source727d. Старые Product/native-owner/Serena inputs нельзя
принимать как совместимые только на основании сходного имени или успешного
предыдущего прогона. Нужна проверка actual33 producer/consumer contracts.

## Условия публикации

### Репозиторные проверки

Точные AGENTS и runbook public5ad [5,6] требуют до code push:

```sh
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

Результаты должны относиться к фактическому проверяемому source. Source/image
qualification связывается с собственной identity; Source727d PASS нельзя
подставить в поля exact33. FAIL/UNKNOWN и неполученные native resources сохраняют
исходный статус.

### Native publication gate

Прочитан неизменённый установленный `check-git-push-gate.py`, SHA-256
`04285948382b6e42c2a1fed826ea98fec3ca5ff98373549361da7ad363f72c7f`.
Путь установки не включён в публичную проекцию. Проверены:

- строки3537–3609: исходные Git/interpreter/entry identities и native pre-push
  registration; запрет --no-verify; current registration;
- строки3798–3909: фактический native Git wire, range receiptV3 и native receiptV4,
  currentness и одинаковое завершённое scanner/policy closure;
- строки3919–3959: named remote, полный source OID, branch ref и настоящий
  git push --verify с исходным ожиданием Git.

Gate **не исполняет Go regression commands**. Он проверяет отдельное условие —
допустимость передачи фактических Git objects. Поэтому его успех не заменяет
repo test requirements и не переносит Source727d qualification на33.

Plan не меняет gate, hook/config, scanner policy, findings, executor или историю
ради обхода отказа. Уже опубликованные recovery capsules [3,4] сохраняют ценные
изменения, но не являются qualification code branch.

## Минимальное продолжение

1. Завершить текущую Source727d acceptance и сохранить фактический результат
   с её commit/tree, closure facts и историческими FAIL/UNKNOWN. Оставшийся
   expected-refusal сохраняет NOT_RUN/UNRESOLVED до установления исходного случая.
2. На существующем restored33 checkout зафиксировать exact source map. Подготовить
   новые изолированные cache/temp/test-resource directories с имеющимся toolchain.
   Fixture prerequisites получать по actual33 contracts; не переименовывать
   предыдущие issuance и receipts.
3. Выполнить обе обязательные literal команды на exact33 через исходного native
   владельца. Сохранить argv, source before/after, реальные package outcomes и
   завершение исходных Job/readers. Native opt-in coverage указать фактически.
4. После собственного получения требуемых результатов снова проверить public head
   и ветку. Передать именно06/33 через существующий native gate в ветку #666,
   без802посторонних изменений и без force. Evidence сохранить в существующем
   evidence PR; описание #666 связать с точным source проверок.
5. Если exact33 обнаружит дефект, внести минимальную отдельную правку на этой ветке
   и получить необходимые результаты нового head. Старые PASS не переименовывать.
   Exact-head внешний review относится к последующему состоянию полного review,
   а не к pre-push gate: runbook располагает его после публикации (Step 4 после
   Step 3). Если он недоступен, PR отмечается как not fully reviewed; это не
   блокирует разрешённый code push после собственных обязательных проверок.
   Model dispatch, CI dispatch и merge этим планом не разрешаются.

Исполнимый отдельный exact33 plan готовится как следующий шаг; настоящий пакет
фиксирует неизменяемое DATA сравнение и не объявляет последующие проверки выполненными.

## Источники

1. [PR #666](https://github.com/applicate2628/mcp-local-hub/pull/666).
2. [Public5ad complete Git tree](https://api.github.com/repos/applicate2628/mcp-local-hub/git/trees/0f062e2bc3ae31bb9eab2c44862f1ec1a7a0ddd3?recursive=1).
3. [Raw commits и patches06/33](https://github.com/applicate2628/mcp-local-hub/tree/5e95aab6d9765701ec6c49ddcecc6cde3293eb65/docs/modernization/verification/recovered-cli-commits-20261128).
4. [Source727d reconstruction](https://github.com/applicate2628/mcp-local-hub/tree/da5e86fd9e8a12731553f6646867808034d705bb/docs/modernization/verification/original-source727d-recovery-20261128).
5. [AGENTS exact public5ad](https://github.com/applicate2628/mcp-local-hub/blob/5ad374eb60cadb8ad56ea167e3fb06edb44bdc18/AGENTS.md).
6. [Operational runbook exact public5ad](https://github.com/applicate2628/mcp-local-hub/blob/5ad374eb60cadb8ad56ea167e3fb06edb44bdc18/docs/agent-runbook.md#pr-review--merge-workflow-mandatory).

Пакет: README, evidence, полный802-path ledger, семь afterimage bindings,
точный пятифайловый diff и SHA-256 manifest. Machine paths, credentials и реальные
process identities в публичный payload не включены.

## Сохранение точных байтов

Локальный `.gitattributes` содержит `* -text` и действует только внутри этой
капсулы: Git не преобразует окончания строк в patch и DATA-файлах. Настройки
проверок, hooks и политика публикации не меняются.

# CLI: точный допуск helper и состояние вызывающего теста

Изменение проверяет полный вызов вспомогательного тестового процесса до создания общего состояния CLI (интерфейса командной строки). Для concurrent-secrets helper оно дополнительно связывает API state root с каталогом vault вызывающего теста. Фактический отрицательный контроль с прежним `TestMain` обнаружил ошибочную привязку и завершил Go-команду кодом 1; кандидат прошёл тот же parent-тест. Прямой CodeGraph helper на контроле оставил один общий package root, на кандидате — ни одного. [CLI evidence (2026/11), `cli-control-oracle`, `cli-owned-helpers`, `native-proof-v2-cli-{control,candidate}`](cli-evidence.zip)

## Оглавление

1. [Исходники и восемь публикуемых файлов](#исходники-и-восемь-публикуемых-файлов)
2. [Контракт изменения](#контракт-изменения)
3. [Целевые тесты и отрицательный контроль](#целевые-тесты-и-отрицательный-контроль)
4. [Прямой нативный CodeGraph proof](#прямой-нативный-codegraph-proof)
5. [Общая композиция и оставшиеся условия](#общая-композиция-и-оставшиеся-условия)
6. [Источники доказательств](#источники-доказательств)

## Исходники и восемь публикуемых файлов

**Таблица 1. Точная идентичность публикационного набора и проверенных входов.** Источники: [code commits (2026/11), кодовая область CLI](source-inputs.json), [source inputs (2026/11)](source-inputs.json), [CLI evidence (2026/11), манифесты и receipts](cli-evidence.zip).

| Объект | Значение |
|---|---|
| Commit с изменением кода | `9ece512541ef7963660f82a8b5fa54cac8fc97e0` |
| Его tree | `16ece5ec32837f09431265dda18464b04b8c29d1` |
| Родитель и физическая база выполнения | `858812988833a84368c60fba1540ae601155fa71` |
| Tree физической базы | `dba685c090952a93a80a1e82403c4646ad8aaed1` |
| Манифест CLI, SHA-256 | `6900e167e575aefa58428fe1091452939ff3d0362bfff128f6389eea263c47b1` |
| Манифест controls, SHA-256 | `011a8f8efa3e9b960efb3845573d31ea3669a5897963c51759fe3a7a47697401` |
| Архив `cli-evidence.zip` | 861 392 байта; SHA-256 `1066444ea05979beef6ab3eeb268a56b1c4fa9c1cb0cafa6f71dd4a5c1287b45` |

Публикационный пакет содержит **8 файлов**, а основная CLI-квалификация использовала **9 Go overlays**, то есть девять адресных замен содержимого файлов при сборке. Дополнительный `internal/cli/gui_force_test.go` относится к canonical-пакету: две PowerShell fixture-команды имеют `-NoProfile -NonInteractive`. Этот девятый файл побайтно совпадает с canonical candidate, SHA-256 `df9944ad7435aa1613bb6353bc45a219d34ab8a683cee2e04edb9d458c51fdc1`; повторного изменения в данном CLI commit нет. Остальные восемь файлов побайтно совпадают с входами CLI ZIP и публикационным манифестом. [CLI evidence (2026/11), `inputs/preserved/manifests/cli-staged/manifest.json`, `cli-candidate/overlay.json`](cli-evidence.zip); [code commits (2026/11), CLI и связанный canonical-вход](source-inputs.json); [canonical GUI source](https://github.com/applicate2628/mcp-local-hub/blob/97ff6da0c428a8c30f1952da470e3ff033a740e4/internal/cli/gui_force_test.go)

**Таблица 2. Восемь файлов CLI commit.** Полные размеры, SHA-256 и Git blob находятся в [source inputs (2026/11)](source-inputs.json).

| Файл | Назначение |
|---|---|
| [settings_registry_test.go](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/settings_registry_test.go) | Ранний dispatch; точный полный массив аргументов; API state binding для concurrent-secrets |
| [testmain_dispatch_test.go](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/testmain_dispatch_test.go) | Табличная классификация и проверки файловых leaf — конечных компонентов пути |
| [testmain_paths_windows_test.go](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/testmain_paths_windows_test.go) | Windows-проверка типа leaf и запрет reparse point |
| [testmain_paths_other_test.go](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/testmain_paths_other_test.go) | Реализация leaf-проверки для других систем через `Lstat` |
| [secrets_audit_test.go](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/secrets_audit_test.go) | Проверка фактического API root настоящим helper до flock и записи vault |
| [secrets_helper_dispatch_test.go](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/secrets_helper_dispatch_test.go) | Parent-регрессия: исходный test executable, собственный vault, один `Cmd.Wait`, отсутствие лишнего package root |
| [daemon_mcpcompat_test.go](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/daemon_mcpcompat_test.go) | Anchored CodeGraph selector, ограниченный `^` и `$`, с обязательным завершающим `--` |
| [testmain_helpers_test.go](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/testmain_helpers_test.go) | Очистка окружения новых и прежних helper-ролей перед повторным запуском тестового бинарника |

## Контракт изменения

`TestMain`, точка входа тестового бинарника, сначала проверяет роль, полный массив аргументов и параметры. Неверные либо смешанные роли получают код 3 до создания общего root. Обычный тестовый запуск проходит к setup, изоляции, аудиту и cleanup пакета. Для семи прежних helper-групп явно перечислены существующие формы вызова; сохраняются специальные формы stale-release, overlay, forensics и production argv. Произвольный дополнительный selector, его split/alias-форма или флаг, меняющий состав запуска, не расширяет допустимый helper-вызов. [CLI source (2026/11), `TestMain`, `classifyCLITestHelperDispatch`, `validCLIOverlayProductionArgv`](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/settings_registry_test.go)

**Таблица 3. Четыре роли, допущенные до общего package root.** Нативное покрытие указано отдельно от проверки классификатора. Источники: [CLI dispatch source (2026/11)](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/settings_registry_test.go), [CLI evidence (2026/11)](cli-evidence.zip).

| Роль | Контракт | Фактически выполнено |
|---|---|---|
| CodeGraph compatibility helper | Маркер `MCPHUB_CODEGRAPH_COMPAT_HELPER=1`; точный anchored selector и terminal `--`; helper должен завершить процесс | Parent-тест с локальным host и отдельный прямой EOF proof |
| Concurrent-secrets | Явные `secrets.age` и `.age-key` в одном root; обязательное значение переменной записи, которое может быть пустым; API root связан до `m.Run` | Parent-регрессия и различающий отрицательный контроль |
| Windows mapped-image child | Точный selector; собственный каталог и ещё отсутствующий `mapped-image-ready` | Классификатор; полный нативный сценарий не выполнялся в этом наборе |
| Windows precontrol child | Маркер `1`, точный selector; завершение либо reap родителем | Классификатор; полный нативный сценарий не выполнялся в этом наборе |

У concurrent-secrets явных путей vault/key недостаточно для всех косвенных обращений API. Ранний dispatch устанавливает `api.SetDaemonStateRootForTest` и сбрасывает strict-mode cache; настоящий helper затем сравнивает read-only API resolver с каталогом vault перед `WithVaultLock`, `OpenVault` и `Set`. Parent-регрессия проверяет, что значение записано в caller vault и дополнительный `mcphub-cli-test-state-*` не появился. [CLI source (2026/11), ранняя ветвь `TestMain`](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/settings_registry_test.go); [настоящий helper](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/secrets_audit_test.go); [parent-регрессия](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/secrets_helper_dispatch_test.go)

Роли, обязанные завершить процесс или быть завершёнными родителем, не принимают неожиданный возврат `m.Run` с нулём: он заменяется кодом 3; исходный ненулевой код сохраняется. Leaf-проверки проверяют тип конечного объекта и на Windows отказывают при reparse point. Владение предками, hard links и конкурентная замена пространства имён остаются вне их контракта. [CLI source (2026/11), `TestMain`](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/settings_registry_test.go); [Windows leaf checker](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/testmain_paths_windows_test.go); [other-platform leaf checker](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/testmain_paths_other_test.go)

## Целевые тесты и отрицательный контроль

В путях ниже опущен общий префикс внутри ZIP `evidence/preserved/`. Оракул — сохранённая проверка ожидаемого результата — расположен в `*.oracle.json`; исходные JSON-события Go находятся в `*.stdout.log`. Команды с тестами использовали `-tags=test_state_path_env -race -json -count=1 -timeout=120s`, точный overlay и ограниченный `-run`. Длительности в Таблице 4 — внешнее время команды, округлённое до миллисекунд. [CLI evidence (2026/11), итоговые receipts и CLI manifest](cli-evidence.zip)

**Таблица 4. Фактические результаты целевых проверок.** PASS/FAIL включают отдельное событие пакета; они не равны количеству самостоятельных тестов.

| Стадия / команда | Результат | Первичный файл внутри CLI ZIP |
|---|---|---|
| `cli-control` / `cli-control` | Компиляция: exit 0; 22,100 с; пустой overlay | `cli-control/cli-control.oracle.json`, `overlay.json`, `receipt.json` |
| `cli-candidate` / `gofmt` | Exit 0; список неотформатированных файлов пуст | `cli-candidate/gofmt.stdout.log`, `.oracle.json` |
| `cli-candidate` / `cli-dispatch-race` | Exit 0; 80 PASS = 78 подслучаев + верхний тест + пакет; 0 FAIL, 0 SKIP; 52,373 с | `cli-candidate/cli-dispatch-race.stdout.log`, `.oracle.json`, `.receipt.json` |
| `cli-candidate` / `cli-candidate` | Компиляция: exit 0; 14,426 с | `cli-candidate/cli-candidate.oracle.json`, `receipt.json` |
| `cli-control-oracle` / `caller-root-control` | **Ожидаемый Go exit 1**; 0 PASS, 2 FAIL, 0 SKIP; 32,433 с; oracle matched | `cli-control-oracle/caller-root-control.stdout.log`, `.oracle.json`, `.receipt.json` |
| `cli-owned-helpers` / `caller-root-and-codegraph` | Exit 0; 3 PASS = два верхних теста + пакет; 0 FAIL, 0 SKIP; 9,782 с | `cli-owned-helpers/caller-root-and-codegraph.stdout.log`, `.oracle.json`, `.receipt.json` |
| `cli-path-checks` / `native-leaf-checks` | Exit 0; 14 PASS = 12 подслучаев + верхний тест + пакет; 0 FAIL, 0 SKIP; 7,433 с | `cli-path-checks/native-leaf-checks.stdout.log`, `.oracle.json`, `.receipt.json` |

Источник Таблицы 4: [CLI evidence (2026/11)](cli-evidence.zip). Классификатор запускался только по `^TestClassifyCLITestHelperDispatch$`. Среди его подслучаев есть mixed roles, отсутствующие маркеры, лишние/повторные/split selectors и явные legacy caller forms. Это таблицы входов, а не исполнение всех legacy subprocess fixtures. [CLI evidence (2026/11), `cli-candidate/cli-dispatch-race.stdout.log`](cli-evidence.zip)

Отрицательный контроль использовал **только два overlay-файла**: `secrets_audit_test.go` с ранним root guard и новый `secrets_helper_dispatch_test.go`; прежний `settings_registry_test.go` оставался физическим исходником. В raw log зафиксированы `helper wait=exit status 3`, `deadline=<nil>`, `roots=1` и сообщение о несовпадении API state root с caller vault root. Отдельные FAIL-события относятся к `TestCLITestBinaryConcurrentSecretsHelperUsesCallerStateRoot` и пакету. `completed=true` у управляющей стадии означает совпадение с заранее объявленным отрицательным оракулом; сам Go-тест действительно завершился неуспешно. [CLI evidence (2026/11), `cli-control-oracle/overlay.json`, `caller-root-control.stdout.log`, `.oracle.json`](cli-evidence.zip)

С девятью кандидатными overlay прошли ровно `TestCLITestBinaryConcurrentSecretsHelperUsesCallerStateRoot` и `TestDaemonCmdCodeGraphDiskManifestPropagatesLegacyProfile`. Первый проверяет запись caller vault и отсутствие лишнего child root; второй использует локальный host, проверяет ответ initialize с протоколом `2024-11-05`, session header и завершение команды. `TestCLITestHelperPathChecks` проверил 11 типовых случаев файлов/каталогов и настоящий `symlink_leaf_refused`; этот последний подслучай имеет PASS, не SKIP. [CLI evidence (2026/11), `cli-owned-helpers/*.stdout.log`, `cli-path-checks/*.stdout.log`](cli-evidence.zip); [parent source](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/secrets_helper_dispatch_test.go); [CodeGraph source](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/daemon_mcpcompat_test.go); [path tests](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/testmain_dispatch_test.go)

## Прямой нативный CodeGraph proof

Отдельный proof использовал массив аргументов `[-test.run=^TestDaemonCmdCodeGraphDiskManifestCompatibilityHelper$, --]`, маркер `1` и закрытый стандартный ввод через `DEVNULL`. Helper получает EOF — признак конца ввода — и завершает процесс кодом 0. В этом прямом сценарии не выполняется обмен протокольными сообщениями; он отличается от parent-теста с локальным host, описанного выше. [CLI evidence (2026/11), `inputs/preserved/prove_api_cli_helpers_v2.py` и native receipts](cli-evidence.zip); [CodeGraph helper source (2026/11)](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/daemon_mcpcompat_test.go)

**Таблица 5. Фактические прямые нативные случаи CLI.** `stdout` и `stderr` — стандартный вывод и вывод ошибок, размеры в байтах.

| Стадия / case | Exit; stdout / stderr | Состояние | Результат |
|---|---|---|---|
| `native-proof-v2-cli-control` / `cooperative` | 0; 0 / 0 | Один `mcphub-cli-test-state-*` после выхода | `oracle_matched=true` |
| `native-proof-v2-cli-candidate` / `cooperative` | 0; 0 / 0 | Дополнительный общий CLI root отсутствует | `oracle_matched=true` |
| `native-proof-v2-cli-candidate` / `invalid-marker` | 3; 0 / 43 | Маркер `2`; собственный обычный файл вместо TEMP/TMP | `oracle_matched=true` |
| `native-proof-v2-cli-candidate` / `extra-argument` | 3; 0 / 43 | `extra` после `--`; тот же тип блокировки allocation | `oracle_matched=true` |

Источник Таблицы 5: [CLI evidence (2026/11), `native-proof-v2-cli-{control,candidate}/receipt.json`, case receipts и streams](cli-evidence.zip). Обе отрицательные проверки выполнялись только на кандидате и получили точную диагностику отказа TestMain. Все четыре непосредственных ребёнка имеют `settled=true`, `timed_out=false`; `GetTempPath2W` разрешил собственные ожидаемые пути, а SHA-256 бинарников до и после совпал. [CLI evidence (2026/11), те же native receipts](cli-evidence.zip)

Бинарники не входят в ZIP, но compile-oracles и native receipts фиксируют их идентичность: control — 60 571 648 байт, SHA-256 `4429c449d309ff931c1458db77d187aa70f5227a9f5c26b830b0daf922da6197`; candidate — 60 657 152 байта, SHA-256 `27d57dda3c151c0ab676d75130bceaa32b015e8735cdfe5543f47f57908127a4`. [CLI evidence (2026/11), `cli-{control,candidate}/*.oracle.json`, `MANIFEST.json`](cli-evidence.zip)

Обе CLI native-стадии использовали proof v2 с SHA-256 `3c12764470607b92c7c55888af1927a887e5d6df670cffafc8e0350dcb51cb34` и runner v2 `b7ed7f7445440a51c7dc56f998d1bb7a11ff1e82c51e4a4d348e3aafa8927641`. В telemetry mode записаны 5 байт `off` с CRLF (возврат каретки и перевод строки), hex `6f66660d0a`, SHA-256 `c281dfd877988953376b6040e2613fcf403e301f3b3203b841564afdae33ad33`. V2 допускает ровно LF или CRLF. Его появлению предшествовал отдельный **API** prelaunch-отказ: `native-proof-api-control` с `completed=false`, `commands=[]` и без helper execution; этот неуспешный результат сохранён в API ZIP. Отказ не приписывается CLI и не скрывается общим успешным состоянием v2. [CLI evidence (2026/11), native receipts и proof script](cli-evidence.zip); [API evidence (2026/11), `evidence/preserved/native-proof-api-control/receipt.json`](https://github.com/applicate2628/mcp-local-hub/blob/b5cd530969b7089e7d7cbaf5308543be8c443f8c/docs/modernization/verification/api-helper-dispatch-2026-11-21/api-evidence.zip)

## Общая композиция и оставшиеся условия

Общая стадия `composed-build-vet` записана во вложенном `recorded-runs/canonical-evidence.zip` комплекта P0: та же физическая база плюс 18 overlay-файлов — 5 API, 9 CLI, 2 daemon, 2 perftools. `go build -overlay … ./...` завершился кодом 0 за 8,267 с, `go vet -overlay … ./...` — кодом 0 за 21,974 с. SHA-256 манифеста `d3cea906438a6975f04a1dc93fc5df12a9aa05cca5b38dc9f4a06c3644c1cca9`. Это самостоятельная проверка сборки и статического анализа общей композиции; девятый CLI файл в ней относится к canonical publication. [Canonical evidence (2026/11), `evidence/preserved/composed-build-vet/receipt.json`, `inputs/preserved/manifests/composed/manifest.json`](https://github.com/applicate2628/mcp-local-hub/blob/f21728d6f3937187fea517c8ca5916b6f69ac7c5/docs/modernization/verification/p0-safe-test-gates-2026-11-21/p0-gates-evidence.zip)

**Таблица 6. Оставшиеся условия и пределы утверждений.** Основание — точные phase manifests, selectors и process flags в [CLI evidence (2026/11)](cli-evidence.zip), а также ограничения [Windows leaf checker](https://github.com/applicate2628/mcp-local-hub/blob/9ece512541ef7963660f82a8b5fa54cac8fc97e0/internal/cli/testmain_paths_windows_test.go).

| Область | Статус и оставшееся условие |
|---|---|
| Все нативные helper-роли | `native_all_roles: NOT_RUN`. CodeGraph и указанная concurrent-secrets регрессия выполнены; Windows mapped-image/precontrol и все прежние роли этим не квалифицированы |
| Полный набор тестов | `full_suite: NOT_RUN`. Приведённые selectors не являются `go test ./...`; полные API/CLI suites не заявляются |
| Восемь файлов как самостоятельная композиция | Проверены база + 9 overlay, включая canonical `gui_force_test.go`. Изолированное выполнение только восьми CLI замен отдельно не заявляется |
| Опубликованная голова | `published_head: NOT_QUALIFIED`. Нужна квалификация точных байтов итоговой головы, если её состав отличается от сохранённых композиций |
| Процессное владение | Proof удерживает исходный `Popen`; parent-регрессия использует исходный `Cmd` и единственный `Wait`. `whole_process_tree_deadline_enforced=false`; завершение всех потомков не доказано |
| Файловое владение | Проверен Windows leaf, включая symlink refusal. Предки, junction/hard-link сценарии и конкурентная замена пути не квалифицированы |
| Другие платформы и внешние интеграции | Реализация `!windows`, полный GUI lifecycle, установленный продукт, реальный Scheduler и внешние модели не исполнялись этими проверками |

## Источники доказательств

1. **[CLI evidence, 2026/11](cli-evidence.zip).** Первичные receipts, raw stdout/stderr, oracle JSON, точные манифесты, девять overlay-файлов и исполнители. Внутренний префикс результатов — `evidence/preserved/`; `MANIFEST.json` содержит размеры и SHA-256 полезных файлов.
2. **[Code commits, 2026/11](source-inputs.json)** и **[CLI source inputs, 2026/11](source-inputs.json)**. Code-only commit на момент фиксации, parent/tree и восемь публикуемых файлов; финальная публикация добавляет отчёт и свидетельства к этому кодовому commit.
3. **[Canonical evidence, 2026/11](https://github.com/applicate2628/mcp-local-hub/blob/f21728d6f3937187fea517c8ca5916b6f69ac7c5/docs/modernization/verification/p0-safe-test-gates-2026-11-21/p0-gates-evidence.zip).** Вложенный `recorded-runs/canonical-evidence.zip`: отдельная общая сборка/vet; SHA-256 `049089bb350e4822833002a117edc1e7912d784f45a992344da2cc3c0cb90075`.
4. **[API evidence, 2026/11](https://github.com/applicate2628/mcp-local-hub/blob/b5cd530969b7089e7d7cbaf5308543be8c443f8c/docs/modernization/verification/api-helper-dispatch-2026-11-21/api-evidence.zip).** Сохранённый первоначальный отказ до запуска helper; SHA-256 ZIP `2a156f8af09d6d0db31353540520e3e0f57cce5765c9103073b12fd04ad61fdb`.
5. **[Local export verification, 2026/11](https://github.com/applicate2628/mcp-local-hub/blob/f21728d6f3937187fea517c8ca5916b6f69ac7c5/docs/modernization/verification/p0-safe-test-gates-2026-11-21/qualification.json).** Проверка полученных архивов. Счётчики и ограничения этой записки сверены дополнительно с raw event logs, phase overlays и case receipts.

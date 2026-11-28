# Full11: FAIL с незавершённым приёмом исходного владения

**Срез: 26 ноября 2026, 00:51:33 UTC.** Управляющая программа (driver) записала `FAIL`, но родитель ещё не принят как завершённый. У CLI остаётся `unknown` для внешнего владельца набора тестов. Полная регрессия не принята; tagged-команда не запускалась. Это отдельный срез, не изменение прежних PASS материализации Source5, обновления 13 тестовых файлов и DATA/двух случаев композиции. [1–5]

## Оглавление

- [1. Подтверждённые результаты](#1-подтверждённые-результаты)
- [2. Что удерживается и где отсутствует путь приёма](#2-что-удерживается-и-где-отсутствует-путь-приёма)
- [3. Сохраняемые ресурсы и допустимые наблюдения](#3-сохраняемые-ресурсы-и-допустимые-наблюдения)
- [4. Точность и границы проверки](#4-точность-и-границы-проверки)
- [Источники](#источники)

## 1. Подтверждённые результаты

Native/IO closure означает приём завершения исходных процессов и ввода-вывода; ordinary — обычная команда под исходным native guard.

**Таблица 1 — команды и границы результата.**

| Область | Наблюдение | Что принято |
|---|---|---|
| Внешний driver | 16 строк: 13 подготовительных, build, vet, default | 15 original native/IO closure, default `unknown` |
| `go build ./...` | exit 0; 243,717917 с | PASS этой команды |
| `go vet ./...` | exit 0; 271,984181 с | PASS этой команды |
| Default full | exit 1; 1332,031 с | FAIL; завершение дерева и потоков не принято |
| Tagged full | Не запускался | `NOT_RUN` |
| Default API | Подготовка 31,008317 с при лимите 30 с | 6 команд закрыты; полный набор не начат |
| Default CLI | Подготовка 24,089545 с; внешний suite-owner 323,852 с при watchdog 320 с | 6 команд закрыты, 7-я — `unknown` |

Точный original driver receipt имеет `finished_utc=00:40:26Z`, не содержит принятой `literal_phases`/coverage и сохраняет `literal_pre_push_commands_passed=false`. Причина: `RuntimeError: an original receiver retained unknown custody`. Поле `finished_utc` — запись приложения, оно не доказывает выход исходного процесса. Проверены 15 original ordinary closure и 30 завершённых worker join в metadata. Issuer сообщает 15 удерживаемых actual Invocations, `worker_joined=true`, отсутствие publication errors. `resource_disposition`, `source_after`, `input_hashes` — `NOT_PERFORMED`; эти finalization-проверки не выполнены. [1]

У сохранённого parent-control `original_pid=482868`, `terminal_exit_received=false`, `parent_capture_files_closed=false`. Терминальный результат исходной DC-сессии 267864 не получен. Запись root `metadata-read-observations.json` от 00:53:25 UTC фиксирует повторное чтение parent-control; его полученные байты тождественны прежнему `state=running`. Это подтверждает неизменность control, а не отдельное наблюдение живого процесса. [4]

API invocation `x28e371b95a91`: проверены метаданные пяти ordinary-команд и исходного inventory-owner, включая direct wait, пустое дерево, закрытые handles, EOF и завершение работников ввода-вывода. API остаётся **closed-failed**, а не частью неизвестного CLI-завершения. Inventory содержит 3523 имени; это перечень, не выполненные тесты. Интервал между стартом inventory-controller и рождением его ребёнка — 18,0665541 с; это составной интервал, не отдельное измерение CreateProcess. Причина задержки не установлена. [2]

CLI invocation `x42df81521075`: те же шесть подготовительных команд имеют доказательства закрытия. Седьмая строка `suite-owner` содержит `TimeoutExpired`, `captures_measured=false`, но не содержит `exit_code`, `original_pid`, `native`, `io` или `completion_receiving`. У invocation отсутствуют `owner`, `canonical_receiving`, `coverage` и `raw_replay`; оба флага `native_custody_settled` и `commands_quiescent` ложны. Inventory из 1988 имён не даёт исходов тестов. Общие 347,945487 с и запуск контроллера также не доказывают выполнение или PASS конкретного теста. [3]

## 2. Что удерживается и где отсутствует путь приёма

**F11-DEFERRED-OWNER-01 — подтверждённый дефект пути владения.** `raw_command(..., defer_output=True)` вызывает внешнюю ветку `GuardedRun.command()`. Она использует `subprocess.run(timeout=320)`; эту строку не добавляют в `RETAINED_COMMANDS`. При тайм-ауте публикуется `unknown`, затем исключение выходит до вызова `finalize_owner_capture()`. [6–7]

Родитель сохраняет исходный объект `Popen` Python-driver и свои два файловых handle до его прямого выхода. Root issuer сохраняет actual Invocation в `records`/`invocations`, исходную строку команды и FAIL-метаданные. Это реальные объекты, но они не заменяют исходные native/IO-объекты внешнего canonical-controller. Закрытые ordinary owners предыдущих команд тоже не являются его владельцами. [6–9]

`receive_failed_worker_closure()` ищет только одну исходную ordinary custody по тождеству Run и row. Для этой external-строки он возвращает `UNKNOWN` с причиной `no one original ordinary custody for this Run`. Finalizer повторяет приём и пишет `failed-original-physical-closure.json` только после `CLOSED`; существующий путь не восстанавливает внешнее владение. Это вывод из точного кода. Стек исходного driver не снимался, поэтому нахождение именно в этом цикле не объявляется отдельным runtime-наблюдением. [6–9]

Исход дерева canonical-controller, его потомков и потоков **неизвестен**. Даже возможное завершение процессов операционной системой или позднее появление `result.json` требует отдельного приёма исходных привязок и физических фактов. Одно имя файла, тайм-аут или живой родитель не заменяют такую приёмку. Ни прежние 14 ordinary-квалификаций, ни два случая композиции не квалифицировали этот новый external-deferred timeout. [6–9]

## 3. Сохраняемые ресурсы и допустимые наблюдения

Сохраняются full11 `runs\literal-final-v11` с `seed-modules`, `isolated-build-cache`, tmp, образами и captures; CLI `runs\x42df81521075` с bundle/profile/suite; Source5 с установленным `internal/api/zz_qualified_api_versioninfo_windows_amd64.syso`; `<TASK_WORKSPACE>\d11` и `s11`. `NOT_RUN` tagged не означает, что s11 уже прошёл проверку ненужности. API evidence сохраняется как closed-failed. Старые holds v7, `x6d9108ff9d42`, source-v1 и удержанные caches не меняются. [2–3,7–9]

Для root перечислены только фиксированные metadata-пути, относительно TASK `<TASK>`:

1. `literal-final-v11-launch.json` — исходный parent wait/exit/capture-close.
2. `runs\literal-final-v11\receipt.json` и `unsafe-stop.json` — состояние driver и причина остановки.
3. `runs\x42df81521075\receipt.json` — точный сохранённый CLI FAIL.
4. CLI `.completed.json` и `.response.json` в `invocations-default`; полный SHA-ключ пути приведён в JSON-приложении.
5. `runs\x42df81521075\suite\result.json` — только возможные исходные control facts. Root получил `ENOENT` при фиксированном metadata-read; исходный suite result не принят.
6. `runs\x42df81521075\failed-original-physical-closure.json` — условный control; текущий ordinary-only receiver не способен получить `CLOSED` для этой external-строки.

Список служит ограничением read-only наблюдений, не новым grant. Не читать и не хешировать новые raw captures, Source или caches; не перемещать их, не запускать successor и не восстанавливать владение по PID. Прежде требуется исходное terminal/native/IO receiving. Это ограничение зависимых full11 ресурсов, не отзыв отдельно принятых immutable DATA-контролей или результата прежнего перемещения пяти ресурсов. [5–9]

## 4. Точность и границы проверки

API/CLI raw JSON восстановлены из транспортного текста **только** переводом LF→CRLF. Получены точные native SHA из их completion-controls: 332910/245800 байт и 5547/4176 CRLF. Полный внешний receipt принят так же: 636357 байт, 7732 CRLF, exact SHA `5acba493…` из нового metadata-only observer-v2. JSON не пересериализовывался; большие native integer обработаны Python без потери uint64. Привязки request SHA/nonce и receipt SHA совпали. Читались control metadata, не содержимое потоков. [2–3]

Внешний счётчик сверён с полным raw original driver receipt и bounded progress-v2. Чтение этого control не является чтением Source/caches или потоков. Все использованные frozen code pins сверены локально с активной композицией. Не менялись literal argv, `5m`, подготовительные 30 с, внешний watchdog 320 с и диагностические 4800 с родителя. Нативных действий, тестов, изменений Source/runtime или GitHub-публикации этим ревью не было. Новое исправление будущего пути не закроет этот UNKNOWN задним числом.

## Источники

Полные локальные пути, размеры и SHA-256 всех controls и исходников находятся в `full11-unknown-evidence.json`.

1. `recovery/full-v11/outer-failure-receipt.json`, SHA `5acba4934a788bfbe67b36b17601ec490aaef9ea40439253e5369f8e7f264901`; `progress-202611260051333756620000.json`, SHA `e62c38f12a8cc44d67d945b0bc1361f14a3a3ac3e1bef31e675d79be87236157`.
2. `default-api-receipt.json`, SHA `9468dd8a9c5cb408a0a954f9c35f64959a461d66d4c9606a72d7b6a739c8f2b0`, и bound API completion в той же папке.
3. `default-cli-receipt.json`, SHA `b528c7d40054aaecdb4e69c119ffe90f161f33e6539c42ea65a8872b54d70c50`, и bound CLI completion.
4. `parent-running-observation.json`, SHA `7c8118b70db47baf56e58d4a1a58b80538c488d098d3441f7bf0226dd223f8c0`; `native-launch-tracking.json`; `metadata-read-observations.json`, SHA `245e56cfe9b83d2f4602a7e106b4e878fceef036e90f0dd1bd0dbb7371631e49`; поздний `parent-late-running-observation.received.txt` byte-identical исходному control.
5. `unsafe-stop.observed.txt`, SHA `c217437c17dd4e315b22166bf394036f27796eece58f8a971baff4a2bc70f28d`.
6. `recovery/qualification/guard-custody.py`, SHA `56d6ccee190472cbdacd5f0ec5f7636df3487f27ec208e0776c20c12a22b72f2`; `command`, `receive_failed_worker_closure`.
7. Bound final adapter `ef393445…`, `raw_command`, `execute`, `OriginalInvocationIssuer`.
8. Bound final core `b679aadd…`, `retain_failed_full_workers`; driver `cf43b7c8…`, `execute`/`execute_gate`.
9. Bound final parent `b031831a…`, original `Popen`/wait; manifest `15756f71…`. Полные hashes — в JSON.

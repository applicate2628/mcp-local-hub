# Композиция Scheduler и Secrets Recovery с исходниками Windows-квалификации B

**Test Source B `001648f4` сохраняет исходники Product `3ebe335a` и 13 отдельных тестовых правок.** Он не содержит полную кодовую базу #660. Из #669 в нём уже есть перевод блокировки Secrets Recovery на существующий реестр освобождения. Ещё требуется сохранять обе причины ошибки в `Unwrap` и добавить регрессии #669. Интегрированное дерево Product и его сборка ещё не выданы.

## Оглавление

- [1. Проверенные исходники](#1-проверенные-исходники)
- [2. Применимость текущей правки A](#2-применимость-текущей-правки-a)
- [3. Что перенести из #669 и проверить в описи](#3-что-перенести-из-669-и-проверить-в-описи)
- [4. Порядок композиции](#4-порядок-композиции)
- [5. Инвентарь кодовых путей #660](#5-инвентарь-кодовых-путей-660)
- [6. Тестовые правки B](#6-тестовые-правки-b)

## 1. Проверенные исходники

| Роль | Коммит | Дерево Git |
| --- | --- | --- |
| Product B | `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4` | `67c13b8fc1317b4fd60e3135a3eb8a376fe01817` |
| Test Source B | `001648f44bf6ef0191f1e21d92f1aecf2a2a3022` | `9700ffd68a009ec714b1962427d53567d07a228d` |
| База локальной правки A, проверенная версия PR #660 | `8d4f2606c9af7e20181e4ad290b29be0e0a2dd0f` | `b830f0afd37de133a98a2d4d21d6384329eee8c4` |
| Проверенная версия PR #669 | `ec2248643d875390c41b519b85424a5ca0ce90d1` | — |

*Таблица 1. Идентичность входов композиции.*

В Git-объекте `001648f4` есть единственный родитель `3ebe335a`. Сравнение деревьев показало 13 изменённых файлов `*_test.go` из таблицы 5. Для всех путей, проверенных в таблицах 2 и 4, а также для Secrets Recovery и описи блокировок Product и Test Source имеют те же Git-объекты содержимого файлов, далее blob.

Точный донор Test Source на Wave — `D:\dev\ChatGPT\bns26\source`. Его неглубокая копия содержит оба коммита и объявляет `001648f4` как `HEAD` при чтении через `git ls-remote --exit-code file:///D:/dev/ChatGPT/bns26/source HEAD`. Отдельный донор `D:\dev\ChatGPT\cache-continuation-20261123-8e223f22\native-consumer-source-v2` объявляет `3ebe335a` как `HEAD` и не содержит объекта `001648f4`. Проверены чтение объектов и объявление `HEAD`. Перенос объектов и подготовка новой рабочей копии остаются этапом композиции.

## 2. Применимость текущей правки A

Кодовая вершина продолжения A — `6206d52e0181ecba220e8ce5665032bf9f3b0866`, дерево `f1aadbcb900465e47f36f5ad6ebcfe477715f840`. Контрольная сумма SHA-256 его кодового diff от `8d4f2606` — `d30dc43c56afdb444d115d640298f5040ca27420b818cd07b90bc5218ee197b9`. Воспроизводимая команда:

```sh
git diff --no-ext-diff --no-textconv --no-color --binary --full-index --src-prefix=a/ --dst-prefix=b/ 8d4f2606c9af7e20181e4ad290b29be0e0a2dd0f 6206d52e0181ecba220e8ce5665032bf9f3b0866 -- internal
```

Поздняя правка `phase_f_lifecycle_test.go` меняет проверки сохранения legacy-задач и autorun OFF/ON. Её исходный blob в B совпадает с исходным blob #660. Поздняя правка `install_parsed_manifest.go` заменяет опасную подсказку `autostart enable` при ошибке сохранённого OFF на просмотр состояния и политики. Обе имеют статус исполнения NOT_RUN.

Из 22 путей локальной правки A у 12 одинаковый исходный blob в `8d4f2606` и B, один новый путь свободен, семь существующих файлов расходятся, два файла присутствуют только в #660. Граф зависимостей также расходится. В B отсутствует базовый механизм отказа от замены и сохранения снимков, файлы контекстов операций и создания из XML, а также часть путей восстановления и тестов. Композиция требует переноса этих зависимостей #660 до применения локальной правки на 22 пути.

Для семи расходящихся файлов проверены исходные и новые строки каждого блока локального патча с тремя строками контекста. В blob B однозначно найдены 40 из 43 исходных блоков по точному совпадению байтов. В столбце «Контекст» указано число совпадений для каждого файла. Новые блоки в B не найдены. Результат применения и сборки предстоит подтвердить на объединённом дереве.

После последней правки подсказки повторно прочитан полный B blob `install_parsed_manifest.go`: `3c13cc7efb9542733d6c56ac1d9c4467ec9567a2`, 143 501 байт. Все 14 исходных блоков актуального diff с трёхстрочным контекстом найдены в нём ровно один раз, новые блоки отсутствуют. Остальные проверенные контексты не изменились; итог 40/43 относится к указанной кодовой вершине.

| Путь | База A | B | Контекст либо действие | Путь менялся в #660 |
| --- | --- | --- | --- | --- |
| `internal/api/install_legacy_scheduler_test.go` | — | — | Новый путь свободен | — |
| `internal/api/install_parsed_manifest.go` | `bd8b5f2e965d` | `3c13cc7efb95` | 14/14 | Да |
| `internal/api/install_parsed_manifest_lane_b_test.go` | `f4756e97580d` | `0dd3495a1274` | 4/5 (нет контекста для строки 121) | — |
| `internal/api/install_parsed_manifest_test.go` | `f1ff936968ed` | `7f2f70f1b11b` | 11/11 | — |
| `internal/api/liveness_task.go` | `12ee5d902075` | `12ee5d902075` | Одинаковый исходный blob | — |
| `internal/api/liveness_task_test.go` | `3df92d53696a` | `3df92d53696a` | Одинаковый исходный blob | — |
| `internal/api/phase_f_lifecycle_test.go` | `1f8384abbed4` | `1f8384abbed4` | Одинаковый исходный blob | — |
| `internal/api/r36_lane_a2_fixes_test.go` | `518ef4d95b67` | `b890390163b0` | 7/7 | Да |
| `internal/api/scheduler_mgmt.go` | `1dab80e6d73a` | `e1b4e4a1d481` | 1/2 (нет контекста для строки 313) | Да |
| `internal/api/scheduler_replacement.go` | `ab3b2b1ca3ca` | — | Предварительно перенести файл #660 | Да |
| `internal/api/scheduler_replacement_test.go` | `1917fba60338` | — | Предварительно перенести файл #660 | Да |
| `internal/api/supervisor_lock_test.go` | `3539a243905a` | `3539a243905a` | Одинаковый исходный blob | — |
| `internal/autostart/autostart.go` | `bf5e0d1d9bf7` | `bf5e0d1d9bf7` | Одинаковый исходный blob | — |
| `internal/autostart/windows.go` | `23a69f5e2598` | `23a69f5e2598` | Одинаковый исходный blob | — |
| `internal/autostart/windows_test.go` | `125ba00eb945` | `125ba00eb945` | Одинаковый исходный blob | — |
| `internal/cli/autostart.go` | `572503380bce` | `572503380bce` | Одинаковый исходный blob | — |
| `internal/cli/scheduler_cmd.go` | `3065d08f8f93` | `3065d08f8f93` | Одинаковый исходный blob | — |
| `internal/cli/setup.go` | `4e5bca5da1b5` | `4e5bca5da1b5` | Одинаковый исходный blob | — |
| `internal/cli/setup_watchdog_test.go` | `720ba26b02d3` | `720ba26b02d3` | Одинаковый исходный blob | — |
| `internal/cli/strict_mode.go` | `c8cee5170974` | `c8cee5170974` | Одинаковый исходный blob | — |
| `internal/cli/strict_mode_test.go` | `f1f4d3183fd7` | `3d6006c89945` | 3/3 | — |
| `internal/scheduler/scheduler.go` | `35f6a5f3ad17` | `79d7af80bf0e` | 0/1 (нет контекста для строки 82) | Да |

*Таблица 2. Совместимость исходных файлов для 22 путей локальной правки A. Хеши blob сокращены до 12 знаков.*

Три блока требуют сверки контекста при переносе.

- [`internal/api/install_parsed_manifest_lane_b_test.go`](../../../internal/api/install_parsed_manifest_lane_b_test.go), блок от строки 121 в `8d4f2606`. В B отличается исходная фикстура workspace. При переносе сохранить общий сценарий workspace с удаляемой и сохраняемой строками и проверку отказа прямого останова на Windows.
- [`internal/api/scheduler_mgmt.go`](../../../internal/api/scheduler_mgmt.go), блок от строки 313 в `8d4f2606`. Перенести `WeeklyRefreshSet` на чтение снимка и отказ от замены существующей задачи. Сохранить результаты переноса базовых функций снимка и отказа.
- [`internal/scheduler/scheduler.go`](../../../internal/scheduler/scheduler.go), блок от строки 82. Добавить общий `ErrTaskReplacementUnowned` рядом с типизированными ошибками базы #660. Соседняя декларация `ErrMutationOutcomeUnknown` отсутствует в B.

У четырёх расходящихся файлов все блоки локализуются по исходному контексту. При переносе по блокам следует сохранить соседние правки B. Вне списка изменённых путей PR #660 находятся три файла с правками B: `internal/api/install_parsed_manifest_lane_b_test.go`, `internal/api/install_parsed_manifest_test.go` и `internal/cli/strict_mode_test.go`.

## 3. Что перенести из #669 и проверить в описи

| Место | Состояние в B | Действие в композиции |
| --- | --- | --- |
| [`secretsInitRecoveryOwner.locked`](../../../internal/api/secrets_init_recovery.go) | Строки 525–533 уже вызывают `lockLeafLedgered` и откладывают `ReleaseAndJoin`. | Сохранить реализацию B. |
| [`SecretsRecoveryPersistenceError.Unwrap`](../../../internal/api/secrets_init_recovery.go) | Строки 78–82 возвращают `InitError`, если задана, и в противном случае `PersistenceCause`. При наличии обеих причин ошибка освобождения недоступна через `errors.Is`. | Перенести `errors.Join(e.InitError, e.PersistenceCause)` из #669. |
| `internal/api/secrets_init_recovery_lock_test.go` | Файл отсутствует. | Перенести три регрессии #669 для результата операции и освобождения блокировки, ошибки захвата и освобождения при панике. |
| [`reviewedRawFlockOwners`](../../../internal/api/supervisor_intent_lock_ownership_test.go) | Строка `internal/api/secrets_init_recovery.go:method:*secretsInitRecoveryOwner.locked` отсутствует. Записи владельцев журнала MCP и аренды наблюдаемого файла состояния уже совпадают с #660. | Сохранить карту B. Если переносится карта A, убрать единственную строку Secrets Recovery. |
| [`reviewedAppliedReleaseClassifierOwners`](../../../internal/api/supervisor_intent_lock_ownership_test.go) | В B есть только существующая запись `releaseAndJoinApplied`. | После переноса транзакционного weekly refresh включить `runWeeklyRefreshTaskTransaction` и `(*API).ApplyWeeklyRefreshSchedule` из #660. |

*Таблица 3. Семантические пересечения с #669 и опись владельцев блокировок.*

Опись прямых вызовов ищет в синтаксическом дереве селектор `New` для импорта `github.com/gofrs/flock`. Вызов `lockLeafLedgered` из Secrets Recovery больше не удовлетворяет этому условию. Проверить отдельно обе карты через `TestSupervisorIntentLockOwnershipProductionInventory`.

## 4. Порядок композиции

1. Подготовить отдельную копию через файловый транспорт Git. Точный тестовый донор отдаёт `001648f4` как `HEAD`. Неглубокое получение без тегов в пустой репозиторий сохранит изоляцию от хранилища донора. После получения сверить коммит, дерево и заголовок родителя с таблицей 1. Для отдельной выдачи Product использовать копию коммита `3ebe335a`.
2. Перенести кодовую базу #660 по проверенному изменению PR с сохранением правок B в совпадающих путях. Сверить каждый из 71 кодового пути из таблицы 4. Из них 32 отсутствуют в B, у остальных 39 отличаются blob. Отличие blob требует проверки блоков изменений и само по себе не доказывает конфликт. Для переноса по общей базе нужны Git-объекты общей базы в новой копии. Отдельно сверить легитимные пересекающиеся изменения B в установке, диспетчеризации задач и миграции записей. Архивы предыдущей квалификации и отчёты планирования #660 имеют собственную область действия.
3. Поверх этой базы перенести локальную правку A на 22 пути. Сверить блоки из таблицы 2, особенно те, у которых не найден исходный контекст в B. Применить остаток #669 и согласовать обе карты владельцев по таблице 3.
4. Для отдельной выдачи Product проверить и зафиксировать его новое дерево. Затем перенести точный тестовый слой из `3ebe335a..001648f4` в дочерний Test Source и сверить все 13 конечных blob с таблицей 5. При сборке только Test Source допустимо начать с точного `001648f4` и сохранить эти blob сквозь всю композицию.
5. Запустить регрессии на новом дереве после завершения композиции. Зафиксировать коммит и дерево, перечень изменённых путей, сведения о сборке нового Product и результат проверок Scheduler, Secrets Recovery, описи владельцев и полного Windows Test Source. Текущие результаты #660 и B остаются доказательством для тех деревьев, на которых выполнялись.

Исходные копии были доступны только для чтения Git-объектов. Таблицы фиксируют совместимость и состав переноса. Выдача нового дерева, сборка и результат интеграционных тестов остаются этапами композиции.

## 5. Инвентарь кодовых путей #660

Перечень получен из фактического diff PR #660 для `8d4f2606`. Исходные blob прочитаны из локального коммита #660 и проверенного коммита B. Пропуск в столбце B означает, что путь отсутствует.

| Путь | #660 | B |
| --- | --- | --- |
| `internal/api/adopt.go` | `f373a742207e` | `3d0c798d13c9` |
| `internal/api/adopt_provenance_events.go` | `cbc9f5fc2b10` | `6b0229e6072f` |
| `internal/api/adopt_scheduler_recovery_test.go` | `10896f0a92c0` | — |
| `internal/api/api_surfaces.go` | `c3b3562334bf` | `cd88c0f65312` |
| `internal/api/install.go` | `07866a41e152` | `57a661fa0ddd` |
| `internal/api/install_hyphen_family_ownership_test.go` | `ca5a91ddc6a5` | `1160197c18d0` |
| `internal/api/install_own_port_test.go` | `28a15ca8f9e6` | `fa8a6439fc54` |
| `internal/api/install_parsed_manifest.go` | `bd8b5f2e965d` | `3c13cc7efb95` |
| `internal/api/install_prune_recovery_test.go` | `23dd868ac73a` | — |
| `internal/api/install_scheduler_snapshot_test.go` | `c1b7ac9d862d` | — |
| `internal/api/install_scheduler_stop_failure_test.go` | `5ca6e7cef4fd` | — |
| `internal/api/install_task_mutation_failure_test.go` | `2605d2a4b611` | — |
| `internal/api/install_task_recovery.go` | `68455083e0e9` | — |
| `internal/api/install_task_recovery_storage_test.go` | `113fa8084dc2` | — |
| `internal/api/install_task_recovery_test.go` | `532eccd53663` | — |
| `internal/api/install_task_rollback.go` | `224ddbf028ec` | — |
| `internal/api/install_task_rollback_test.go` | `efd77caecfe7` | — |
| `internal/api/install_test.go` | `7f9cf0212d38` | `c013912c413d` |
| `internal/api/lsp_auto_register.go` | `f6643d185f7a` | `d8095b711f71` |
| `internal/api/prune_tasks_test.go` | `764599f31295` | `9569cb0158d7` |
| `internal/api/r36_lane_a2_fixes_test.go` | `518ef4d95b67` | `b890390163b0` |
| `internal/api/register.go` | `dd9538903fde` | `e8383d2219a9` |
| `internal/api/register_supervisor.go` | `60eb5d69d64a` | `170f259daab4` |
| `internal/api/scheduler_mgmt.go` | `1dab80e6d73a` | `e1b4e4a1d481` |
| `internal/api/scheduler_mgmt_validation_test.go` | `42d5ef720422` | `dee982a2006c` |
| `internal/api/scheduler_operation_context_test.go` | `6611a300bfd6` | — |
| `internal/api/scheduler_owner_sid_windows_test.go` | `90956ad44477` | — |
| `internal/api/scheduler_replacement.go` | `ab3b2b1ca3ca` | — |
| `internal/api/scheduler_replacement_test.go` | `1917fba60338` | — |
| `internal/api/scheduler_swap.go` | `0a48df4c770b` | `91f6808364b9` |
| `internal/api/scheduler_swap_test.go` | `5535fe72c75e` | `840e736a7fc2` |
| `internal/api/scheduler_upgrade_recovery_test.go` | `da598141bd49` | — |
| `internal/api/scheduler_upgrade_snapshot_test.go` | `13d1349f6644` | — |
| `internal/api/status_enrich.go` | `b651e3532eee` | `367577cadbc4` |
| `internal/api/supervisor_intent_lock_ownership_test.go` | `0e4b14055c6a` | `a066f788c4aa` |
| `internal/api/testhooks.go` | `6dac4500fd4a` | `e9e97971f62b` |
| `internal/api/types.go` | `58ef313b69ba` | `fea04f80d0be` |
| `internal/api/weekly_refresh.go` | `f4e0642e64d8` | `7e6be38ab492` |
| `internal/api/weekly_refresh_applied_result_test.go` | `82973370fa21` | — |
| `internal/api/weekly_refresh_atomic_test.go` | `a71f5cef708d` | `863f09e837e7` |
| `internal/api/weekly_refresh_call_graph_origins_test.go` | `b0fcdc5c74bf` | — |
| `internal/api/weekly_refresh_recovery_test.go` | `bf88dace92f6` | — |
| `internal/api/weekly_refresh_test.go` | `456ddd5d4f96` | `4520dc6ac508` |
| `internal/api/weekly_refresh_testhooks.go` | `fa9d3145e75c` | — |
| `internal/autostart/linux.go` | `6c7e1bc9d581` | `a85e0c1dda46` |
| `internal/autostart/linux_exec_percent_test.go` | `fe201c084d05` | — |
| `internal/cli/install_migration_wiring_windows.go` | `3a9b829f6980` | `cc97ad1067d9` |
| `internal/cli/windows_product_pair.go` | `d331820fe20b` | `51b154002e29` |
| `internal/cli/windows_product_pair_test.go` | `c35c4021d5a9` | `23416aef03f9` |
| `internal/gui/daemons_test.go` | `6a8e4015321e` | `4f60cda4e4c1` |
| `internal/gui/weekly_schedule_applied_result_test.go` | `ab12c5bc04ed` | — |
| `internal/scheduler/entrypoint_migration_windows.go` | `25bf990310b8` | `ebc2e755f362` |
| `internal/scheduler/entrypoint_migration_windows_test.go` | `8526d54320f6` | `03aa14765ad8` |
| `internal/scheduler/enumerate_all_windows.go` | `51e126c93f88` | `23d258300738` |
| `internal/scheduler/scheduler.go` | `35f6a5f3ad17` | `79d7af80bf0e` |
| `internal/scheduler/scheduler_com_windows.go` | `d5bac88c068d` | `d3e19c517345` |
| `internal/scheduler/scheduler_com_windows_test.go` | `9bda7839263b` | `c6a1d0cf93f2` |
| `internal/scheduler/scheduler_command_windows.go` | `26169c7bdc8f` | — |
| `internal/scheduler/scheduler_context.go` | `0222508573a4` | — |
| `internal/scheduler/scheduler_create_conflict_windows_test.go` | `8b6cf918447c` | — |
| `internal/scheduler/scheduler_hidden_task_windows_test.go` | `227475cd7e5f` | — |
| `internal/scheduler/scheduler_last_result_windows_test.go` | `3c81976e7a89` | — |
| `internal/scheduler/scheduler_next_run_windows_test.go` | `e6f0fef40b4b` | — |
| `internal/scheduler/scheduler_noop.go` | `1497b33fa335` | `01996d16a20d` |
| `internal/scheduler/scheduler_operation_context_windows_test.go` | `d4cc7df3a5ab` | — |
| `internal/scheduler/scheduler_owned_run_windows_test.go` | `fc62cac15510` | `747de6435d6c` |
| `internal/scheduler/scheduler_owner_sid_windows_test.go` | `e5f0dd0aa621` | — |
| `internal/scheduler/scheduler_windows.go` | `dab3127459f0` | `ab2062d3bcf7` |
| `internal/scheduler/scheduler_windows_test.go` | `708f20da97a7` | `4fe9b21ce7f1` |
| `internal/scheduler/scheduler_xml_create_windows.go` | `b66c0857bebd` | — |
| `internal/scheduler/scheduler_xml_create_windows_test.go` | `0061f8411f04` | — |

*Таблица 4. Сверка 71 пути исходников и тестов #660 с B. Хеши blob сокращены до 12 знаков.*

## 6. Тестовые правки B

Ни один путь не пересекается с локальной правкой A, перечнем изменённых путей PR #660 или двумя кодовыми путями #669.

| Путь | Test Source B |
| --- | --- |
| `internal/api/adopt_codex_transport_test.go` | `332b49f17cc3` |
| `internal/api/adopt_deadopt_lifecycle_test.go` | `36279734c2f6` |
| `internal/api/adopt_h4_summary_signal_test.go` | `229619d7b027` |
| `internal/api/adopt_lease_namespace_windows_test.go` | `19da9dff6e64` |
| `internal/api/adopt_lease_owner_test.go` | `a0f540b4d982` |
| `internal/api/adopt_mcpcompat_test.go` | `6f336ba8b7b5` |
| `internal/api/adopt_project_plan_test.go` | `74b312a793fa` |
| `internal/api/adopt_provider_working_directory_windows_test.go` | `e86bfecabc0a` |
| `internal/api/adopt_test.go` | `08b88a3e12d4` |
| `internal/api/main_test.go` | `88b0c35c6848` |
| `internal/cli/adopt_mcpcompat_test.go` | `403eefdea619` |
| `internal/cli/adopt_test.go` | `78cdf77fbcbf` |
| `internal/cli/gui_force_test.go` | `e2fe6d9d2ae3` |

*Таблица 5. Точные тестовые пути дочернего коммита B и сокращённые хеши их конечных blob.*

Исходное подтверждение состава изменений — Git-объекты `3ebe335a` и `001648f4`, [PR #660](https://github.com/applicate2628/mcp-local-hub/pull/660) и [PR #669](https://github.com/applicate2628/mcp-local-hub/pull/669).

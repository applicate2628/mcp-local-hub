# Разбор Go-сбоев API, маршрутизации, CLI и демона

**Историческое независимое чтение исходников и сохранённых журналов, без запуска кода.** Исходники: `applicate2628/mcp-local-hub@92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22`; кодовый родитель — `95f2c6df8d8883f009025d2196480cc54c8101af`, следующий коммит меняет план. Здесь описана локальная revision 2 поднабора из девяти тестовых файлов, до общего набора из двенадцати файлов и парного критерия. Ведущий исполнитель отдельно выполнил запуски, сверил прямые квитанции и провёл собственное ревью. Актуальные результаты: [RESULTS.md](RESULTS.md); итоговая область: [AUDIT.md](AUDIT.md). Ссылки на итоговый код закреплены опубликованным коммитом `2cb467aac31259ed4fb665ed0a4f277a8f9c31a2`.

## Оглавление

1. [Основание и классификация](#1-основание-и-классификация)
2. [Общая причина 125 отказов подготовки registry](#2-общая-причина-125-отказов-подготовки-registry)
3. [Подготовленный кандидат и границы](#3-подготовленный-кандидат-и-границы)
4. [Отдельные сбои CLI и нативной сборки](#4-отдельные-сбои-cli-и-нативной-сборки)
5. [Тайм-ауты API и CLI](#5-тайм-ауты-api-и-cli)
6. [Минимальные различающие проверки](#6-минимальные-различающие-проверки)
7. [Проверки этого разбора и остаток](#7-проверки-этого-разбора-и-остаток)

## 1. Основание и классификация

Читался непосредственно ZIP-архив `r17/entry-env-20261120/report/evidence-redacted.zip`, без распаковки его полного содержимого. Основание: `evidence/gates/full-test.stdout.log`, `tagged-api-cli.stdout.log`, `receipt.json` и сохранённый `run_checks.py`. Полный перечень верхнеуровневых отказов, имена вложенных подтестов, номера строк и контрольные суммы журналов находятся в `failure-inventory.json`.

**Таблица 1. Наблюдавшиеся отказы в пределах этого разбора.**

| Пакет | Полный прогон | Прогон с `test_state_path_env` | Что установлено |
|---|---|---|---|
| `internal/api` | Тайм-аут пакета 300,174 с; именованных `--- FAIL` до него нет | Тайм-аут 300,036 с | Из одного снимка стека нельзя вывести взаимную блокировку текущего теста |
| `internal/api/lsp_routing` | 10 верхнеуровневых отказов, 0,137 с | Не входил | Все 10 — отказ сохранения исходной registry-фикстуры по DACL |
| `internal/api/serena_routing` | 18 верхнеуровневых отказов, 1,671 с | Не входил | Все 18 — тот же отказ подготовки; два случая shared observer также не дошли до проверяемого наблюдения |
| `internal/cli` | 3 именованных отказа и тайм-аут 300,047 с | Другие 3 именованных отказа, один из них общий, и тайм-аут 300,041 с | Несколько независимых предпосылок; общей доказанной причины всех CLI-сбоев нет |
| `internal/daemon` | 98 верхнеуровневых отказов, 18,990 с | Не входил | 97 по DACL и один отказ PowerShell до сборки нативной фикстуры |

DACL — discretionary access control list, список дискреционного управления доступом Windows. SID — security identifier, идентификатор субъекта безопасности. CLI — command-line interface, интерфейс командной строки. API — application programming interface, программный интерфейс. LSP — Language Server Protocol, протокол языкового сервера.

В исходном harness все унаследованные ключи `MCPHUB_*` и `GIT_*` удаляются (`run_checks.py:28`), после чего явно заданы только `MCPHUB_NO_AUTO_GUI=1` и `MCPHUB_E2E_SCHEDULER=none` из первого семейства. `USERPROFILE`, `LOCALAPPDATA` и временные каталоги перенаправлены в собственную область. Это доказывает состав введённых harness переменных, но не изоляцию системного Known Folder.

## 2. Общая причина 125 отказов подготовки registry

### Подтверждённое наблюдение

Все 10 LSP, 18 Serena и 97 daemon верхнеуровневых отказов имеют диагностированную причину:

> parent directory not single-user safe

Далее журнал указывает разрешение `S-1-5-11` с маской `0x0001013f` и активную строгую политику. Ошибки возникают при `Save`/`seed.Save`, до основных утверждений соответствующих тестов. Журнал доказывает непригодность конкретного родителя записи и то, что строгий predicate вернул `true`; он **не записывает**, какой вход этого predicate был решающим.

Примеры точек подготовки:

- [`makeRegistryWithLSP`, resolver_test.go:42–54](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/lsp_routing/resolver_test.go#L42): `reg.Save()`; первый вызывающий тест создаёт обычный `t.TempDir()` в строке 58.
- [`makeRegistryWithSerena`, resolver_test.go:33–46](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/serena_routing/resolver_test.go#L33): тот же путь записи.
- [`newTestProxyWithCfg`, lazy_proxy_test.go:310–328](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/daemon/lazy_proxy_test.go#L310): `r.yaml` под `t.TempDir()`, затем `seed.Save()`.
- [`seedReadinessTestRegistry`, lazy_proxy_readiness_test.go:525–536](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/daemon/lazy_proxy_readiness_test.go#L525): отдельный общий создатель фикстуры, который не проходит через предыдущий helper.

### Причина зависимости от состояния машины

Каждый из трёх пакетов — отдельный Go test binary. Импорт `internal/api` не выполняет `internal/api/main_test.go`. В исходном дереве у трёх пакетов нет собственного `TestMain`, который устанавливал бы `api.SetDaemonStateRootForTest`.

В обычной Windows-сборке [`daemonStateDirReadOnly`, state_paths_prod.go:53–62](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/state_paths_prod.go#L53) без явного тестового override использует `resolveKnownFolderProduction`. Тот вызывает [`windows.KnownFolderPath(FOLDERID_LocalAppData, 0)`, state_paths_windows.go:39–44](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/state_paths_windows.go#L39). Перенаправление `USERPROFILE`/`LOCALAPPDATA` не заменяет этот системный источник.

[`OperatorRequiresSingleUserHome`, client_write_init.go:1031–1048](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/client_write_init.go#L1031) сначала проверяет env, затем один раз кеширует решение о persisted intent. В [`readStrictModeFromIntentBestEffort`, строки 1227–1289](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/client_write_init.go#L1227) `true` также означает ошибку разрешения каталога, необычную ошибку чтения либо повреждённый JSON. Следовательно, сообщение «strict mode is active» не доказывает существование у оператора `strict_mode=true`.

Путь отказа: обычный временный родитель → [`SecureWriteClientConfig` и `secureWriteStateFileWithParentRelax`, state_file_helper.go:145–163](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/state_file_helper.go#L145) → ошибка родителя → глобальный strict predicate → отказ fixture `Save`. Продуктовый отказ корректен при этих входах. Не требуется ослаблять его или менять установленную конфигурацию.

### Минимальная коррекция

Для каждого отдельного тестового бинарного файла установить собственный защищённый state root и явно сбросить кеш strict intent после установки и после восстановления. `SetDaemonStateRootForTest` сам сбрасывает Serena stop-read cache, но **не** strict-intent cache: это видно в [testhooks.go:227–255](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/testhooks.go#L227).

Положительные fixture-записи должны иметь защищённый **непосредственный** родитель. Это отдельное условие от изоляции глобального state root. Защита применяется до создания потомков: существующий helper устанавливает `NO_INHERITANCE`, а [CLI TestMain:436–438](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/cli/settings_registry_test.go#L436) уже фиксирует опасность позднего изменения DACL для унаследованного доступа потомков.

## 3. Подготовленный кандидат и границы

**Таблица 2. Изменённые тестовые файлы.**

| Путь | Изменение |
|---|---|
| [internal/api/lsp_routing/main_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/api/lsp_routing/main_test.go) | Новый узкий TestMain: защищённый state root, override, оба сброса кеша, проверяемая очистка |
| [internal/api/serena_routing/main_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/api/serena_routing/main_test.go) | То же; охватывает также external package `serena_routing_test` в общем test binary |
| [internal/daemon/main_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/daemon/main_test.go) | То же и ранний строго ограниченный dispatcher существующих stdio helper-процессов |
| [internal/daemon/host_original_windows_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/daemon/host_original_windows_test.go) | Две env-константы перенесены в общий test-файл; существующий Windows test-файл предоставляет проверку атрибутов каталога |
| [internal/api/lsp_routing/resolver_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/api/lsp_routing/resolver_test.go) | 10 положительных state-root allocations защищены до создания потомков; три чисто геометрических/no-registry `t.TempDir` оставлены |
| [internal/api/serena_routing/resolver_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/api/serena_routing/resolver_test.go) | 17 положительных allocations, включая direct Save и lock-mode сценарии |
| [internal/api/serena_routing/shared_observer_public_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/api/serena_routing/shared_observer_public_test.go) | 2 положительных allocations для внешних наблюдателей |
| [internal/daemon/lazy_proxy_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/daemon/lazy_proxy_test.go) | 16 положительных `r.yaml` родителей; отдельно защищён восстановленный родитель после намеренного отказа |
| [internal/daemon/lazy_proxy_readiness_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/daemon/lazy_proxy_readiness_test.go) | Защищён родитель общего readiness seed |

Размер исторического поднабора: **9 тестовых файлов, +224/−48 строк**. Контрольная сумма `candidate.diff`: `061f34bff0783f63ff6cff55766c1541855d13cbff86b2bd9cbead6e996ecaa7`.

В `TestLazyProxy_Reconcile_FailedWriteDoesNotRecordShadow_RetriesNext` родитель остаётся **файлом** во всём отрицательном участке. Только после исходного `os.Remove(blocker)` в строке 3327 добавлено создание защищённого каталога для положительной восстановительной записи. Проверки отказа и отсутствие преждевременно записанной тени не изменены.

### Особенность helper-процессов демона

В [host_original_windows_test.go:25–109](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/daemon/host_original_windows_test.go#L25) есть пять режимов: `echo`, `final`, `wrapper`, `descendant`, `sentinel`. Оба source-owned producer передают ровно `-test.run=^TestStdioOriginalHelper$` и mode/root env. `wrapper` вызывает `os.Exit(17)`; некоторые другие helpers намеренно прекращаются проверкой владельца процесса.

Поэтому новый TestMain не выделяет отдельный каталог состояния для **точно такого** helper frame. Он проверяет оба поля, допустимый mode, точный argv, абсолютный существующий каталог без `FILE_ATTRIBUTE_REPARSE_POINT`; ошибочные/частичные frames завершаются кодом 2. Нормальные тесты сохраняют полный state fence. В helper-ветви до `m.Run` нет вызовов API/продуктовой логики. Маркерный каталог уже принадлежит родительскому тесту.

Прочитаны все 27 daemon test-файлов. Других self-exec/os.Exit helper-ролей не обнаружено. Остальные дети — отдельные Python/command/native fixture executables, на которые TestMain этого бинарного файла не распространяется. В трёх пакетах не найдены вызовы клиентских configuration adapters или binds supervisor IPC: дополнительных env/IPC/clients stubs не добавлено. IPC — interprocess communication, межпроцессное взаимодействие.

## 4. Отдельные сбои CLI и нативной сборки

### 4.1. Отсутствующий qualified CLI Product

`TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade` падает в обоих прогонах до сценария unreadable intent: full log:667–668, tagged log:576–577. [Тест:386–390](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/cli/install_migration_wiring_windows_test.go#L386) прямо требует абсолютный `MCPHUB_CANONICAL_ENTRY_FIXTURE`; harness эту переменную не задаёт.

Это **доказанная отсутствующая предпосылка запуска**, а не обнаруженный дефект обработки повреждённого intent. Открытый [PR #657](https://github.com/applicate2628/mcp-local-hub/pull/657) уже владеет source-issued Windows test Product; создавать дублирующий PR или заменять вход синтетическими Portable Executable байтами не требуется. Минимальная следующая проверка — подготовленный этим механизмом CLI test Product точного источника, его проверяемые binding/хеши, затем один этот тест. В данном кандидате он не менялся.

### 4.2. PowerShell отказал исполнять CST builder

`TestCstDirectFrontendCrossesGoCapabilityAndFixedLocalPipe`: full log:932–938. `build_test_frontend.ps1` не выполнен; `FullyQualifiedErrorId: UnauthorizedAccess` и локализованное сообщение прямо указывают запрет выполнения сценария.

[Тест:43–53](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/daemon/cst_direct_frontend_e2e_windows_test.go#L43) выбирает `pwsh.exe`, иначе `powershell.exe`, затем вызывает `-NoProfile -File` без отдельной диагностики политики. Доказан отказ **до сборки**, свойства канала и capabilities не проверены этим запуском. Самая дешёвая различающая проверка — read-only сведения о выбранном host и действующей execution policy; далее разрешённый способ выпуска той же фикстуры. Автоматического изменения системной политики, обхода административных ограничений или установки нового PowerShell этот разбор не предлагает и не выполняет.

### 4.3. Сам тест отменяет force-kill happy path до kill

В tagged log:237 записано `context canceled` при ping, :240–241 — отмена **до SIGKILL** и отсутствие попытки kill, :242 — exit 4. Проверяемый тест завершился через 3,53 с.

[`TestForce_KillHappyPath_SeamMocked`:243–251](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/cli/gui_force_test.go#L243) сам запускает отмену после `Sleep(3s)`, а строки 264–265 отвергают exit 4. Связь отмены и отказа доказана журналом; почему подготовка заняла более трёх секунд, не измерено.

Минимальная корректировка — в отдельной тестовой правке связывать отмену с наблюдённым завершением kill/acquire/start, сохранив аварийный конечный тайм-аут. Производственный запрет выполнять destructive step после отмены ослаблять нельзя. Простое увеличение Sleep не доказывает устойчивость.

### 4.4. Маркер respawn cap не появился за 2 секунды

Полный прогон: `TestGuiSupervisorManager_SpawnErrorTripsCapNotPark` и `TestGuiSupervisorManager_CapStopsLoop`, full log:656–659. В обоих случаях уже зафиксированы **3 вызова** spawnFn. Это не подтверждает сообщение-гипотезу теста «loop parked after 1 failure».

В [gui_supervisor_owner.go:735–743](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/cli/gui_supervisor_owner.go#L735) запись `LogHubMcpEvent` стоит **до** проверяемой строки stderr. Реальный logger включает блокирующие mutex/flock, Sync и Close ([hub_mcp_log_observation.go:239–270 и 391–463](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/hub_mcp_log_observation.go#L239)). Тесты проверяют строку по двухсекундным wall-clock deadlines.

**Гипотеза:** задержка синхронного журнала либо планирования задержала маркер. Конкретное место задержки в момент этих отказов не записано; исправление алгоритма respawn по этому журналу не обосновано. Различающая проверка — отдельный запуск этих двух функций с наблюдением входа/выхода audit write; если причина подтвердится, отделить проверку конечного состояния цикла от реального файлового logger в unit-фикстуре. В текущем кандидате изменений нет.

### 4.5. `version` вернул exit 1 без вывода

Tagged log:218–220, `TestReliability_VersionCommandExitsZero`, 9,52 с. [Тест:130–136](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/cli/daemon_reliability_test.go#L130) сначала вызывает общий builder, затем задаёт отдельные 5 секунд дочерней команде. Ошибка не сообщает `ctx.Err()`, время только subprocess или Windows exception status.

Поэтому из 9,52 с нельзя вывести длительность самого `version` или объявить Product-admission причиной. Самая дешёвая проверка — один тест, раздельная длительность build/launch и исходный context status при отказе. Коррекция поведения до уточнения причины не предлагается.

## 5. Тайм-ауты API и CLI

**Таблица 3. Что действительно находилось в стеке на общей отметке 5 минут.**

| Запуск / пакет | Текущий тест и его возраст | Заблокированная операция / исходный путь |
|---|---|---|
| full / API, log:40–154 | `TestExecuteAdoptMigratesEligibleLegacyStateRoot`, 3 с | goroutine 183: Windows CloseHandle → `hub_mcp_log_observation.go:406`, fallback audit после чтения supervisor intent |
| tagged / API, log:43–122 | `TestProviderManagedInstallStateAdvancesBeforeInstall`, 1 с; его `stale_durable_state_prevents_install_call`, 0 с | goroutine 366: FlushFileBuffers → `secure_write_windows.go:330` → provider install phase write |
| full / CLI, log:669–927 | `TestMCPFrontPR588_RollbackRestoresPriorLSPRouterURL`, 16 с | goroutine 2610: FlushFileBuffers → `secure_write_windows.go:330` → MiMoCode config write → LSP client plan |
| tagged / CLI, log:578–640 | `TestRunReconcileMCPFront_FailsClosedWhenRouteNotLive`, 3 с | goroutine 4422: select в `waitForMCPFrontRouteListener`, install_reconcile_mcp_front.go:1187 |

Последний путь имеет внутренний [30-секундный context timeout:959 и 1462](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/cli/install_reconcile_mcp_front.go#L1457), а [poll-loop:1170–1197](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/cli/install_reconcile_mcp_front.go#L1170) проверяет отмену. Снимок на третьей секунде не показывает нарушения этого локального срока.

Паника относится к **суммарному бюджету пакета**. Текущие тесты различаются между прогонами и только что начались; это сильное основание не объявлять их deadlock. Фактические Sync/Close в двух пакетах показывают реальную границу ввода-вывода, но один stack snapshot не доказывает её общую стоимость, проблему диска, антивируса либо конкретный источник задержки. Пер-тестовые durations из `-json` в этих исходных прогонах отсутствуют.

Минимальное продолжение: адресные текущие функции в изолированном окружении, затем один диагностический пакетный `-json` при необходимости, чтобы разложить накопленную длительность. Повышение обязательного `-timeout 5m` само по себе не закрывает исходный критерий.

## 6. Минимальные различающие проверки

1. **Отрицательный контроль родителя при strict-env=1:** неизменённые `TestResolveByPath_SameMtimeRegistryReplacementObservedWithin32Lookups` (LSP), `TestResolveByPath_AbsoluteMatch` (Serena), `TestLazyProxy_InitializeSyntheticNoMaterialize` (daemon) под собственным широким временным родителем должны воспроизвести тот же fixture-gate отказ. Глобальный posture reader обязан быть ограждён до такого исполнения; одного LOCALAPPDATA недостаточно.
2. **Кандидат при той же strict policy:** те же функции должны дойти до исходных семантических утверждений. Затем целевые LSP/Serena пакеты и `^TestLazyProxy` покрывают все затронутые registry-фикстуры без CST builder/Python-кейсов.
3. **Default policy:** package TestMain должен удерживать API state/audit под собственным root и корректно восстановить кеш после вложенных state overrides. Не очищать `MCPHUB_REQUIRE_SINGLE_USER_HOME` глобально.
4. **Child dispatcher:** существующие `TestStdioOriginalFastWrapperDescendantAndForeignSentinel` и `TestStdioOriginalFinalReplyPrecedesExit` проверяют реальные source-owned helper paths. Некорректные mode/root/argv и reparse-root должны быть отвергнуты до выделения TestMain root; это самостоятельная граница, пока проверенная только статически.
5. **Отдельные остатки:** qualified Product, CST policy, timer/oracle и cumulative package time требуют своих узких проверок. Результат registry-контроля не переносится на них.

Это предложения на этапе независимого статического разбора; их последующее выполнение ведущим исполнителем и фактические результаты отделены в [RESULTS.md](RESULTS.md).

## 7. Проверки этого разбора и остаток

Прочитаны канонические repository `AGENTS.md`, соответствующие части runbook, навыки Go testing/verification и performance/diagnostics. Извлечение имён/классов выполнено из исходного ZIP; ошибка разбора не маскировалась как запуск Go.

Шесть исходных копий сравнены с GitHub blob identity по формуле штатного Git blob hash; все совпали. Полный исторический дифф самостоятельно перечитан. Независимый статический рецензент проверил revision 2 поднабора и не нашёл оставшихся существенных дефектов; правильность выполнения этим не доказывается. Идентичности итогового набора находятся в [candidate-manifest-v2.json](candidate-manifest-v2.json); привязка выполнений — в [RESULTS.md](RESULTS.md).

**Граница независимого чтения:** на этом этапе не выполнялись gofmt, сборка, Go-тесты, детектор гонок памяти и нативные запуски. Последующие проверки ведущего исполнителя, включая ещё незавершённые обязательные, учитываются отдельно в [RESULTS.md](RESULTS.md). LSP benchmark `resolver_benchmark_test.go:27` остаётся на обычном `b.TempDir()` перед положительным `WriteStateFileBytesLockHeld`; он не исполняется существующими командами `go test`, потому оставлен отдельным необязательным остатком.

На этапе независимой подготовки создавались только изолированные копии кандидата. Этот этап не менял исходное рабочее дерево, установленные программы, Rust wait/close работу, GitHub PR или системную политику. Текущее состояние подготовки и публикации ведущим исполнителем фиксируется отдельно в [RESULTS.md](RESULTS.md).

## Примечание к опубликованной редакции

Исторический разбор выше сохранён. Код опубликован на `2cb467aac31259ed4fb665ed0a4f277a8f9c31a2`; строгие целевые повторы прошли, однако полный и tagged API/CLI-запуски остались неуспешными. Новые квалификационные отказы, 14 tagged-миграций и короткие текущие тесты при общих тайм-аутах перечислены в [RESULTS.md](RESULTS.md) и [AUDIT.md](AUDIT.md). Это дополнение не пересчитывает исторические журналы и не объявляет их причины устранёнными.

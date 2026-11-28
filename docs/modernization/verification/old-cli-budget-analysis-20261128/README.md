# Исторический CLI: куда ушёл пятиминутный бюджет

## Оглавление

1. [Результат и границы](#1-результат-и-границы)
2. [Исходные данные и закрытие](#2-исходные-данные-и-закрытие)
3. [Как разобран вывод](#3-как-разобран-вывод)
4. [Затраты и незавершённый инвентарь](#4-затраты-и-незавершённый-инвентарь)
5. [Сопоставление с Source59f](#5-сопоставление-с-source59f)
6. [Минимальный дальнейший шаг](#6-минимальный-дальнейший-шаг)
7. [Воспроизведение и состав](#7-воспроизведение-и-состав)

## 1. Результат и границы

В уже закрытом историческом default-прогоне CLI завершённые корневые тесты напечатали суммарно **299,30 секунды** до начала `TestRunSupervisorHeartbeat_StopsOnDone`. Это согласуется с исчерпанием общего пятиминутного бюджета пакета. Само имя прерванного heartbeat-теста не доказывает его зависание.

Исходная классификация сохраняется: **FAIL/CLOSED**, exit 2. Тестовая Source — `727d66753814dc218e26d699e57402db2d13190a`, Product Source — `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`. Здесь выполнен только разбор ранее закрытого вывода как данных. Новые тесты, сборки и модельные запуски не выполнялись.

Результат не переносится на Source59f `59f618ebfbf6d9f52c6994d68586e3c2ebc202a8`, её PR или текущий незавершённый запуск. Его CLI captures, активные Source/cache и сохранённое владение UNKNOWN не читались и не изменялись. Часть тестовых файлов совпадает побайтно, но production-файлы и среда отличаются (§5).

## 2. Исходные данные и закрытие

**Таблица 1. Точные входы DATA-разбора.**

| Роль | Размер, байт | SHA-256 |
|---|---:|---|
| Root receiving исторического full-v6 | 4560 | `f02bf04c80d0e84a5989297f742662ea874bbf7743c78ab3da947305004849d5` |
| Default CLI receipt | 1254316 | `a5bd2bae2fd38c7c28ef516840f613852d211b4c407fa62b0944c10a2c92fc8b` |
| Original CLI stdout | 516257 | `d680bab54bba9cd6043a1f46359076d650a08d9061d550ceee54bff8019f787f` |
| Original CLI native owner result | — | `0e40fffd4461d80c9d72193d611e3e2f6b134b326afb00f7486931504c2319c9` |
| Old Source727d map | 885110 | `a3483e469b606fa297a4b33698cd22db7bf25493023f3117feeafb52bb9db5f1` |
| Source59f actual-byte map | 459779 | `e8c3fc2f577a130f92ba4eee7f8eee7153afc032dee909557cf1156890917c06` |

До чтения stdout были получены точные старые root/CLI receipts. Они подтверждают исходный root exit 2, завершение членов Job, итоговый ActiveProcesses=0, закрытие исходных native handles, пяти I/O handles и двух readers с EOF без ошибки/переполнения. В старом composite также завершён исходный canonical channel. Статусы native custody settled / commands quiescent / captures finalized истинны; custody retained ложно.

ActiveProcesses=0 здесь не используется вместо ожидания исходного process handle: parser отдельно требует RootWaitKnown и RootExitCode=2, HandlesClosed и исходные I/O predicates. Это получение уже существующего доказательства, без нового ожидания процесса, PID reopen или cleanup.

Исторический полный receiving сохраняет 283 команды, 566 captures, 65 package receipts и 130 owner receipts. Этот узкий пакет не получает заново каждый из них: он принимает их pinned root receiving и проверяет конкретную CLI ветку.

Исходные данные и FAIL сохранены в [публичной капсуле full-v6 на неизменном коммите 05fb](https://github.com/applicate2628/mcp-local-hub/tree/05fbffa30f99914c8174966525a70bc9984e7038/docs/modernization/verification/original-full-v6-failure-20261128). В ней stdout представлен прозрачным template с ролью `default-cli/suite-owner.stdout.txt`; восстановленный файл должен совпасть с таблицей 1. Этот пакет повторно не публикует машинные пути из raw captures.

## 3. Как разобран вывод

Parser признаёт событие только после фактического байта SYN `0x16`. Маркер может идти после prompt в середине физической строки. Поэтому обычный текст, похожий на `--- PASS`, не становится результатом теста. В строке 5587 такой настоящий frame завершает `TestStrictModeRecover_OrdinaryIntentFailureStopsBeforeShim`; parser только по началу физической строки теряет этот PASS и получает 1584 вместо 1585.

Корневым считается terminal без отступа и без `/` в имени. Продолжительности вложенных subtests повторно не суммируются: они входят в время корневого теста. Проверены переходы RUN → terminal и RUN → PAUSE → CONT → terminal, отсутствие дубликатов и одновременно активных корневых тестов.

Время — число секунд, напечатанное самим Go в terminal-событии. Сумма 299,30 имеет точность этих округлённых значений; она не является самостоятельной монотонной wall-clock трассой. Начало/конец копирования image, запуска процесса, identity query и cleanup отдельно не измерены.

Полный [ledger](top-level-terminal-ledger.json) хранит 1599 terminal rows: исходное имя, PASS/SKIP, напечатанное время, накопленную сумму, номер физической строки, индекс frame и **байтовое смещение UTF-8**. Смещение не является индексом символа JavaScript.

## 4. Затраты и незавершённый инвентарь

**Таблица 2. Итог frame-aware разбора.**

| Показатель | Значение |
|---|---:|
| Исходный top-level inventory | 1988 |
| SYN frames | 7642 |
| Корневые RUN | 1614 |
| Корневые PASS | 1585 |
| Корневые SKIP | 14 |
| Корневые FAIL terminal | 0 |
| Все PASS terminals, включая вложенные | 2516 |
| Завершённые корневые terminals | 1599 |
| PAUSE / CONT | 14 / 0 |
| Корневые tests без RUN | 374 |
| Корневые tests без terminal | 389 |
| Накопленная напечатанная длительность | 299,30 s |

Ноль FAIL terminals не означает PASS пакета: процесс завершился exit 2 по общему timeout. Четырнадцать обязательных migration roots только объявили RUN и PAUSE; ни одного CONT или terminal для них нет. Это не исполненные тела, не PASS и не SKIP. Оба native canonical controls находятся среди 374 ещё не начатых roots. Последний активный root — heartbeat StopsOnDone. Точные списки приведены в [not-started-or-paused.json](not-started-or-paused.json).

**Таблица 3. Десять самых дорогих завершённых roots в старом прогоне.**

| Тест | Напечатано, s | Строка stdout |
|---|---:|---:|
| `TestPidMatchesMcphub_RejectsForeignPathSameBasename` | 26.91 | 4283 |
| `TestLoadSupervisorCurrentRunning_SkipsLiveNonMcphubPID` | 19.03 | 4286 |
| `TestGUIOwnerUnknownConfirmationMarkerWorker_HeldLockBoundsAllWriteSites` | 15.35 | 5761 |
| `TestReliability_VersionCommandExitsZero` | 12.63 | 711 |
| `TestMigrateSerena_RegistryRollback_SkipsConcurrentlyUnregisteredKey` | 7.41 | 3814 |
| `TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade` | 6.11 | 2362 |
| `TestForce_KillRaceLost` | 5.88 | 1374 |
| `TestGUIOwnerUnknownConfirmationMarkerWorker_HeldLockTimesOutAndReaps` | 5.11 | 5746 |
| `TestAcceptLoop_BudgetBreakerExitsOnFlood` | 5.02 | 5620 |
| `TestRunReconcileMCPFront_ForwardThenRollback_RoundTrip` | 4.29 | 2680 |

Первые пять стоят 81,33 s; первые десять — 107,74 s; первые двадцать — 141,05 s. Всего 58 roots с длительностью не меньше секунды дают 203,16 s. Последний завершённый heartbeat cadence занял 0,04 s и довёл сумму до 299,30 s; затем в строке 8469 начался StopsOnDone.

Эти факты объясняют, почему нахождение heartbeat Sleep в timeout-stack само по себе не указывает первопричину. Дальше оставались 374 не начатых roots плюс 14 отложенных migration bodies; одной небольшой экономии нельзя заранее приписывать достаточность для полного пяти­минутного прогона.

## 5. Сопоставление с Source59f

[Source correspondence](source-correspondence.json) фиксирует точные SHA-256 двух полученных maps. Выбранные совпадающие файлы были прочитаны в закрытом Product export; позднее отдельные файлы прочитаны в новой изолированной Source59f, целиком полученной root. Активная рабочая копия текущего full не использовалась.

**Таблица 4. Конкретные зависимости основных затрат.**

| Файлы и строки | Наблюдение из исходников | Практическая граница |
|---|---|---|
| `process_identity_match_windows_test.go:24–132` | Два tests независимо копируют текущий test image в свежий `mcphub.exe`, запускают отдельный helper, проверяют реальную identity, затем вызывают исходный cmd.Kill/cmd.Wait. | Их суммарные 45,94 s не разделены на copy/Start/predicate/cleanup. Предикаты и свежие процессы необходимо сохранить. |
| `daemon_reliability_test.go:46–96,134–178` | Первый ensureMcphubBinary один раз собирает `go build -tags test_state_path_env ./cmd/mcphub`; последующие callers используют sync.Once. | Исторические 12,63 s включают подготовку binary. Это не 12,63 s чистого исполнения `--version`. |
| `supervise_ensure_alive_marker_worker_test.go:172–209,267–393` | Один test и три subtests действительно удерживают file lock до timeout, проверяют typed error, reaping и освобождение lock. | 20,46 s суммарно соответствуют четырём реальным примерно пятисекундным ожиданиям. Сокращение production budget изменило бы смысл проверок. |
| `supervise_ensure_alive_marker_worker.go:24,263–287,360–371` | Production budget равен 5 s; запуск использует реальный строго содержащий Job owner. | Глобальные lock/test seams в этих tests не допускают бездумного `t.Parallel`. |
| `settings_registry_test.go:293–525` | TestMain различает helper dispatch, создаёт один package state root, выполняет m.Run и штатную cleanup. | Возможный общий read-only artifact должен иметь отдельное понятное владение до конца обоих tests. |
| `supervisor_death_forensics_test.go:888` | Прерванный heartbeat test содержит короткий Sleep; его файл совпадает побайтно. | Совпадение не превращает место прерывания в доказательство deadlock. |

Тестовые файлы таблицы 4 совпадают между старой Source727d и Source59f. При этом `internal/process/pid_identity_windows.go`, `supervise_liveness.go`, `supervise_ensure_alive.go` отличаются. В частности, нельзя переносить общий wall time identity-тестов на другую production реализацию. SHA-сопоставление не заменяет проверку поведения новой ревизии.

## 6. Минимальный дальнейший шаг

Этот пакет не содержит source patch и не объявляет найденную оптимизацию достаточной.

Наиболее узкий кандидат уменьшения дублирования — один package-owned обычный файл, содержащий побайтную копию входящего test image для двух negative identity tests. Каждый test должен по-прежнему создавать отдельный реальный процесс и проверять прежние identity/state predicates; файл должен сохраняться до завершения обоих исходных процессов и очищаться через существующее владение TestMain. Это убирает одно копирование, но без стадийных времён размер экономии неизвестен.

Для reliability helper найдено ровно четыре callers: три в `daemon_reliability_test.go` и один в `gui_force_test.go`. Уже подготовленный tagged native Product имеет `testOnly=true` и `buildTags=[test_state_path_env]`, однако он также имеет GUI subsystem и canonical entry-set contract. Поэтому это не основание без проверки callers подставить его вместо plain Go-built helper. Безопасная альтернатива — отдельно полученный helper с теми же исходниками, tag и argv, если источник теста явно поддерживает такую подготовку и сохраняет отказ на несовпадение.

Нельзя выводить успешное завершение из нынешних нулевых cleanup observations, ослаблять owner predicates, повторно использовать UNKNOWN cache, менять пять минут, пропускать обязательные roots или переносить существующие проверки за пределы их контрактов ради результата. Реальный результат следующего полного прогона относится только к его точной новой Source и отдельным первоначальным owners.

## 7. Воспроизведение и состав

`parse_closed_cli_durations.py` — переносимый DATA-only parser на стандартной библиотеке Python. Он не импортирует runtime-исходники, не запускает процессы, не читает каталог Source/cache, принимает только три заданных файла с точными hashes и читает stdout после проверки pinned closure. Выходная папка должна быть новой.

```text
python parse_closed_cli_durations.py --root-receiving old-root-receiving.json --receipt old-cli-receipt.json --stdout old-cli-stdout.txt --output replay-new
```

Имена входных файлов в примере произвольны; их байты определяются таблицей 1. Parser должен выдать 1585 корневых PASS, 14 SKIP, 299,30 s, ровно 14 paused roots без CONT и активный StopsOnDone. Он также создаёт полный terminal ledger.

При подготовке этого пакета анализ реально выполнен над полученными точными данными в JavaScript/V8; Python-текст самостоятельно прочитан, но **не исполнялся**. Нового native/test получения из этого факта не следует. Root может выполнить его как отдельный DATA replay после собственного review.

`analysis.json` содержит итог и top-20, `top-level-terminal-ledger.json` — все 1599 rows, `not-started-or-paused.json` — невыполненный остаток, `source-correspondence.json` — 12 точных file pairs. `artifact-manifest.json` связывает все остальные файлы этого пакета. `.gitattributes` сохраняет точные bytes без преобразования окончаний строк.

В публичных файлах отсутствуют абсолютные машинные пути, учётные данные, binaries и cache payloads. Исходный FAIL и текущий UNKNOWN остаются отдельными историческими фактами.

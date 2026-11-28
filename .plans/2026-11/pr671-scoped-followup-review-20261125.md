# #671: ARA-01, Windows producer и продолжение P07

Дата: 2026-11-25. Разделы 1–4 сохраняют историю прежнего состава исходников и его результатов. Разделы 5–8 описывают проверенную проекцию P07 поверх опубликованного `4725fb47abc56b616bf96f1608f7ba7cecdcdbd4`, полученные scoped-проверки и отдельную сборку private controller. PASS относится только к указанному источнику и выборке; самостоятельная приёмка ветки #671 и установленного Product не заявляется.

## Оглавление

1. [Состав и порядок включения](#состав-и-порядок-включения)
2. [Проверка границ переноса](#проверка-границ-переноса)
3. [Полученные проверки и их границы](#полученные-проверки-и-их-границы)
4. [Контекст reconcile и тестовые контракты](#контекст-reconcile-и-тестовые-контракты)
5. [Перенос P07 и зависимости композиции](#перенос-p07-и-зависимости-композиции)
6. [Scoped-проверки c894 и a5](#scoped-проверки-c894-и-a5)
7. [Private controller H7742: Source и build](#private-controller-h7742-source-и-build)
8. [Оставшаяся приёмка](#оставшаяся-приёмка)
9. [Источники результатов](#источники-результатов)
10. [Частичное открытие pipe: vendor follow-up](#частичное-открытие-pipe-vendor-follow-up)

## Состав и порядок включения

Кандидат продолжает исходный head #671
`891ef8bfd0764a969db2f800578fd45acd5623d6`; его база не менялась.
Перенесены ровно 38 путей ARA-01 и три пути producer, затем добавлен этот
отдельный документ. Полный интеграционный снимок для сравнения:
`464fb1079517177fa8b5b3db590ccfe7d1093404`, tree
`62097d1865d236d948cd9812868bf4f826b639df`.

Порядок ARA: `ara01-main-frozen.patch` → `ara01-review-followup.patch` →
`ara01-migration-frozen.patch` → `ara01-projection-followup.patch` →
fixture `42c400871c2ad737ac846e61cb06077f2dd61f7c`.
Патчи применились без конфликтов. Проверены их SHA-256, опубликованные в
соответствующих манифестах передачи.

ARA сохраняет отдельную stdio-сессию для каждого клиента, проверяет проектное
разрешение и принимает завершение исходного провайдера до допуска следующего
private provider.
Миграция получает исходные дескрипторы до блокировки, меняет intent под
существующей блокировкой, принимает её освобождение и завершение исходного
процесса, затем снимает только свой stop fence и допускает private provider.
Поправка fixture действительно сохраняет изменённый дескриптор и читает его
обратно до проверки отказа. Mutex-поправка защищает публикацию и чтение
состояния исходной сессии; тест закрывает свой обработчик на каждом выходе.

**Порядок зависимости: #659 → #671.** Реализация
`captureRemovedSupervisorTargets`, `supervisorRemovedCaptures` и связанные
helper-файлы остаются у владельца WL-04 в #659. Здесь добавлен их потребитель,
а не второй механизм владения процессом. Отдельная собираемость #671 до
включения зависимостей не проверена и не заявляется.

Producer перенесён только в `build.ps1`,
`scripts/test-windows-product-build.ps1` и
`docs/testing/serena-qualification.md`. Их согласованное конечное состояние
включает прежние prerequisites: документацию #637 и test-only producer #657;
остальные файлы этих PR сюда не переносились. Producer выдаёт привязанные к исходникам
compiler inputs для двух private lanes и отдельный текущий V4 runtime Product
с тестовым пространством состояния. Это разные роли; выдача данных сборки
сама по себе не является допуском нативного процесса. Сохранены строгие
проверки Source, роли, GUI subsystem, registry, свежего output и сохранение
артефактов неуспешной сборки. Trash применяется только после полного успеха.

## Проверка границ переноса

36 из 38 файлов ARA и все три файла producer побитово совпадают с integration
`464fb107…`: **39 из 41 полных файлов**. Два осознанных исключения:

**Таблица 1. Исторические границы ARA-проекции на `464fb107…`.**

| Файл | Причина отличия от integration | Проверка собственного изменения |
|---|---|---|
| `internal/api/install.go` | В integration также находятся изменения других владельцев: WL-04, port-owner и scheduler | Все 26 добавленных/удалённых строк ARA совпадают с owner delta `33856d8`; чужие изменения не перенесены |
| `internal/api/install_parsed_manifest.go` | В integration также находятся original-capture и scheduler изменения других владельцев | Все 3 добавленные строки ARA совпадают с owner delta `33856d8`; чужие изменения не перенесены |

Добавления `supervisor_removed_cleanup.go` и
`supervisor_removed_cleanup_test.go` отсутствуют в этом кандидате.
Проверка состава до добавления документа дала ровно 41 ожидаемый путь,
без лишних или пропущенных файлов. `git diff --check` прошёл.

**Таблица 2. Исторические точные хеши producer.**

| Файл | SHA-256 |
|---|---|
| `build.ps1` | `36f1ed1da7e6a961b50356a156cf5fc29c532593f3f0dcfb9b1f5cb6dbf59ada` |
| `scripts/test-windows-product-build.ps1` | `58bc6ae1827abfff88e32b946bba966291167648608988360744d29aac4cbc53` |
| `docs/testing/serena-qualification.md` | `ea185b4b17427bcfd8a96401d872ea22cf472c35700e88036e84578197428656` |

## Полученные проверки и их границы

Следующие результаты получены Root на полной композиции с её зависимостями.
Они не являются новым запуском этого самостоятельного PR. Квитанции связаны
с указанными исходниками; исходный controller дождался своего процесса и
закрыл захваты вывода. Для всех перечисленных команд принято состояние
`complete` / `CLOSED`.

**Таблица 3. Scoped-результаты до literal-2.**

| Проверка | Фактические исходники | Результат |
|---|---|---|
| ARA focused: API, зарегистрированный маршрут, daemon | `68369ed4674de6390c8cbc1ed10ad22c83096472` | 23 корневых PASS, 0 SKIP: 15 API, 1 маршрут, 6 функциональных daemon и 1 helper [1] |
| Causal control: игнорировать ошибку исходного завершения | `8ef17fa77c03eb326faf748502ff038e0ed699cd` плюс явно записанная регрессия в отдельной копии | Ожидаемый exit 1; `lost-complete` и `root-only` обнаружили `incomplete original closure admitted private provider` [2] |
| Causal control: принять отсутствие SID как успешное завершение | Та же отдельная копия с другой записанной регрессией | Ожидаемый exit 1; тест обнаружил `pending receipt acknowledged` [2] |
| Восстановленные исходники после двух causal controls | `8ef17fa77c03eb326faf748502ff038e0ed699cd`, обе регрессии сняты, чистота проверена | Оба корневых теста PASS; миграция прошла все 7 подслучаев [2] |
| Producer controls5: разбор PowerShell 7.7.0, DATA и реальный linker | `464fb1079517177fa8b5b3db590ccfe7d1093404` | PASS; GUI subsystem=2, CUI subsystem=3; внутри каждого варианта хеш совпадает в двух путях сборки, между GUI и CUI хеши различаются; точные producer input hashes совпадают с таблицей выше [3] |

Эта таблица сохраняет результаты до последующего полного запуска. На integration `464fb107…` позднее завершился literal-2: build/vet **PASS**, полные default и tagged **FAIL**, исходные ресурсы **CLOSED**. Сборка `cp01-p25e` выполнила 31 команду и получила **compiler/build DATA PASS**; native admission — **NOT_OBSERVED**, обе migration lanes — **NOT_RUN**. CLI panic на отсутствующем контексте произошла до 14 корневых migration cases каждой lane. Пустые свежие каталоги являются её следствием и не доказывают дефект producer.

Итоговая приёмка Product и реального Sequential Thinking 2.2.0 остаётся открытой. Результаты композиции не являются отдельной сборкой или выполнением самостоятельного #671. Установка Product, задачи и политика установленной среды этими проверками не менялись.

## Контекст reconcile и тестовые контракты

Первое продолжение поверх опубликованного `4725fb47abc56b616bf96f1608f7ba7cecdcdbd4` меняло четыре файла. Его единственная производственная правка — нормализация отсутствующего контекста в начале `runReconcileHubMode` (`internal/cli/install.go`). Прямой вызов Cobra-команды до `Execute` может иметь `cmd.Context() == nil`. Обёртка lease нормализовала лишь свою локальную копию, а дальнейшая операция снова читала nil из команды и доходила до `ctx.Err()`. Теперь команда получает `context.Background()` только при nil; существующие отмена, deadline и значения контекста сохраняются.

Регрессия `TestReconcileHubModePreservesCanceledCommandContext` требует исходный отменённый объект контекста и `errors.Is(context.Canceled)`; fake Scheduler запрещает достижение мутации. Прежние nil-context success и dry-run проверки сохранены. `reconcile-context-positive-1` прошёл в default и `test_state_path_env`: пакет 3.105 / 0.285 s, controller 50.048 s. Receipt SHA-256: `058dfd8e40fee66502953c5af56fd7b309002a9a6eb96488d2061c4cc4bf7ff0`.

В `internal/gui/supervisor_restart_spawn_windows_test.go` проверка отсутствия retry явно подаёт `ERROR_FILE_NOT_FOUND` через существующий `WithStart` seam и сохраняет проверку исходной ошибки. Настоящий Windows CreateProcess внутри Job может сначала вернуть `ACCESS_DENIED` при запросе breakaway, поэтому путь несуществующего файла не задавал требуемый класс ошибки. Реальный успешный запуск и отказ обязательного breakaway сохранены. `gui-error-classification-positive-1`: все 3 корневых теста PASS, 0 SKIP, пакет 0.454 s. Receipt SHA-256: `1023d2da3f561fa353ac88d1520287e60693f02bba044b0b03e109128a35e612`.

В `internal/cli/install_migration_wiring_windows_test.go` unreadable-intent проверяет типизированную `json.SyntaxError` и имя intent-файла: строгий ранний readback теперь отказывает до прежней строковой диагностики. Assertions отсутствия runner calls, неизменности прежнего Product и intent bytes сохранены. На момент этой исходной правки её статическое ревью не считалось PASS. В последующем закрытом default CLI дочернем запуске literal-3 этот root получил PASS; полный CLI пакет завершился FAIL [5]. Более поздние scoped-результаты имеют отдельные границы в таблице 5.

Root проверил точный delta; независимое ревью контекста не нашло блокирующих замечаний. Перечисленные scoped receipts относятся к `464fb107…` с закреплёнными working-file hashes; они не переносятся на новый commit. Все их исходные процессы, Job, handles и readers приняты CLOSED. Полная композиция новых семи файлов закреплена как `cc6879be732631bead8365d5e8ce30383173f598`, tree `e14c9466ec83ebb7409fee21b517d176bc7c3915`. Свежие preparation/runtime inputs и полные наборы получают отдельные квитанции в существующем #664; старые FAIL сохранены.

## Перенос P07 и зависимости композиции

Проверенная owner-проекция сохраняет исходный head #671 `4725fb47…` и добавляет два согласованных набора. Это перенос относящихся к #671 изменений из композиции, а не замена файлов целиком снимком другой ветки. Исходный manifest передачи и оба патча закреплены SHA-256 [4].

**Таблица 4. Состав продолжения #671.**

| Набор | Состав | Граница |
|---|---|---|
| `671-p07.patch` | 26 путей: serving aggregate, original Route/peer, rollback, lease/namespace и их тесты | Включает последующие исправления зависимостей и жизненного цикла, дошедшие до `c894da64…` |
| `671-fixtures.patch` | 5 API test-файлов | Readiness, Mixed group, Codex repeat и generic W10; production matcher и admission не ослаблены |
| Уже подготовленные изменения владельца | 5 файлов: контекст reconcile, его regression test, unreadable-intent, DACL и GUI error classification | Эти изменения не дублируются в двух residual-патчах |

Исходная причина P07 находилась до остановки прежнего supervisor: aggregate reservation требовала свободный saved Hub endpoint, хотя допустимый исходный Route ещё обслуживал его. Перенос получает и удерживает исходного serving owner с его peer, namespace, поколением и исходной квитанцией. Для полученного serving Route проверка vacancy выполняется после подтверждённого ухода исходного владельца. Восстановлены проверка неизменённого состояния перед reap, подготовленный rollback до освобождения владельцев и ветвь восстановления serving-состояния без преждевременного запуска второго supervisor.

Зависимости этого пути также включены: `ReadSupervisorRouteForPeer` принимает единственную Route из ответа исходного аутентифицированного peer и отказывает при отсутствующей/неоднозначной записи и отмене; `UpgradeFenceLease` хранит исходные bound lease и namespace. Saved Hub settings gate вызывается через уже полученный namespace, без создания нового API/владельца состояния. Адаптер reap использует тот же удержанный исходный supervisor; повторное получение процесса не заменяет его original proof. Проверка `ValidateOriginal`, отказ при неизвестном результате и veto на неразрешённый rollback сохранены.

**Композиционные prerequisites остаются обязательными:**

- **#659 → #671:** original removed-target capture, completion и связанные типы/helper-файлы WL-04 принадлежат #659. В #671 остаются потребители этих контрактов. Их отсутствие нельзя компенсировать новым независимым владельцем процесса.
- **#660 → композиция #671:** scheduler context binding и veto rollback при неизвестном результате мутации из `115f570d825dede7b559093920f87bd279021acd` должны сохраниться при объединении. Owner-проекция не копирует их как собственный независимый механизм.
- Прежние producer prerequisites #637 и #657, а также private preparation/receiver inputs остаются отдельными ролями и не становятся принятыми из-за применения P07-патча.

Четыре полных P07-файла намеренно отличаются от integration: `internal/api/supervisor_ipc_status_client.go`, его test-файл, `internal/cli/install_migration_wiring_windows.go` и `internal/cli/windows_product_pair.go`. Патчи сохраняют owner baseline; существующие integration PID-generation/console-policy и scheduler изменения должны прийти от своих владельцев. В `internal/api/install_test.go` изменён только контекст удаляемой строки под существующую сигнатуру owner; новая production-сигнатура не вводится. Применимость residual-патчей к закреплённому owner проверена; отдельная сборка и выполнение #671 с ними **NOT_RUN**.

Fixtures приводят ожидания к действующим контрактам. Session-local readiness и Mixed group без полученного protected-front owner сохраняют отказ, отсутствие сессии и outbound-вызовов. Bare Codex install без source-provenance receipt не разрешает повтор по форме alias; исходные policy bytes и отрицательный конфликт остаются проверяемыми. Generic W10 использует `demo` и явно задаёт `tool_timeout_sec = 45` в исходном stdio-входе, сохраняя проверку переноса значения. Тест DACL (списка прав доступа) проверяет именно карту ACE (записей прав): `BA` в поле owner текстового дескриптора SDDL не означает ACE для Administrators. Проверки точного числа trustees и masks сохранены.

Ранние ошибки не переименованы в успех: неполная P07-композиция `381b682…` отказала на компиляции до тестов; последующая `8c561c3…` выявила пропущенные serving/reap/rollback ветви. Исправление жизненного цикла `f759b0a0…` включено в source `c894da64…`; результаты следующего раздела относятся именно к новым запускам.

## Scoped-проверки c894 и a5

Scoped здесь означает проверку явного списка сценариев. Root означает корневой Go-тест, terminal — завершённый тест любого уровня, включая подслучаи. Числа не суммируют корни и подслучаи как независимые проверки. Во всех PASS-строках ниже FAIL=0 и SKIP=0. Квитанции подтверждают завершение исходных процессов и Job (Windows-контейнера процессов), закрытие handles, EOF (конец обоих потоков) и join readers — ожидание завершения обработчиков чтения. Принудительного завершения не было [6–8].

**Таблица 5. Полученные scoped-результаты полной композиции.**

| Run / выборка | Точные исходники | Корни / все terminal | Время команды, s | Результат |
|---|---|---:|---:|---|
| `cli-followup-c894-1`, default affected | `c894da649620a7e3ab23bdf3463b6adf43aadce4` | 6 / 38 | 92.0591045 | PASS / CLOSED |
| Тот же run, `test_state_path_env` | Те же `c894da64…` | 33 / 71 | 97.1011040 | PASS / CLOSED |
| Тот же run, selective rollback control | Те же `c894da64…` | 1 / 2 | 10.4195763 | PASS / CLOSED |
| `api-followup-c894-1`, default refusals | Те же `c894da64…` | 1 PASS, 1 FAIL | 23.2636010 | FAIL / CLOSED; tagged не начат |
| `api-followup-a5b81-1`, default refusals | `a5b81b0ac3bfe807f09d22d75ddee5d9eb6b8f56` | 2 / 2 | 46.9153311 | PASS / CLOSED |
| Тот же run, `test_state_path_env` | Те же `a5b81b0…` | 26 / 127 | 132.6277431 | PASS / CLOSED |

CLI controller `c894` завершился за 199.9114373 s; API controller `a5` — за 180.4540177 s. Это selected source/runtime regression scopes. Они не подтверждают полный набор, private Serena migration, standalone #671, фактическое обновление установленного Product или его время запуска.

Исторический API FAIL `c894` сохраняется: wrong-lock-path fixture сообщила `refused bypass touched supervisor-events.log`. В `a5` изолирован родитель и явно проверено исходное отсутствие тех же трёх leaves. Объяснение исправления поддерживается исходником; конкретная строка старого event log не была сохранена, поэтому её содержимое и фактический emitter не приписываются этой проверке.

Последний полный literal gate остаётся **FAIL** на `cc6879be732631bead8365d5e8ce30383173f598`: оба обязательных набора завершились неуспешно, все 67 исходных дочерних владельцев приняты CLOSED [5]. Scoped PASS не меняет этот исторический результат. Новые source freezes P02 и reliability также требуют собственных квитанций и не наследуют PASS из таблицы 5.

## Private controller H7742: Source и build

Private restore controller — отдельный источник `7742de5bcd0a6675f2852b157dd474581ff57ebd`, tree `fb438e6ab15d8ec679c92ffc1c0b63f4b10f0af9`. Он сохраняет прежнюю private restore реализацию с отдельно проверенными b01 pins. Это не main Source будущего стандартного Product-кандидата.

**Таблица 6. Фактически полученные этапы private controller.**

| Run | Исходных команд | Время controller, s | Принятый результат |
|---|---:|---:|---|
| `private-controller-b01-source-v3-1` | 6, включая HEAD | 191.7161359 | PASS / CLOSED: пять source/compiler/preparation шагов [9] |
| `private-controller-b01-build-v3-1` | 3, включая HEAD | 211.2118432 | PASS / CLOSED: build controller и native validation [10] |

Все исходные команды exit 0; дерево процессов, handles, оба потока и readers завершены. Source-v3 выдал `SourceEpoch = eb6f47785bbf70d465fbce0e92b2d40b64bf1cc420c872d416da4045431deb25`, snapshot SHA-256 `3da62455e7339e374e706dc42653772658fc0315c649f3cca217a6d39e5208ea` и build date `2026-11-25T13:30:58Z`.

Полученная native identity связывает Product SHA-256 `be11ee46cab45c8ffa7b0c077db14631e248daa7a3c801f253fa998f7ad43ec5` и launcher SHA-256 `4e9dd6f7afcd950e45b83d848da934a3e8e68a431b242f76a2c8faaafb92b736` с тем же commit/epoch/date. Все 600 байт identity совпадают с полным stdout завершённого validator; SHA-256 `085eaa8e8c9d1edd067744c12b1c176ae2d67afb99707ea4b78299fed40c50f7`. Это получение результата исходного validator, без заявления о повторном хешировании текущих EXE при документальном ревью.

Проверенные отдельные restore/qualifier script copies меняют только перечисленные pins и пути [11]. Обратные подстановки точно восстанавливают прежний wrapper и ранее проверенный qualifier. Исходные waits, получение обоих потоков, typed native identity, все .66 cohort checks, 20 критериев и строгое `ElapsedTicks < 60 * FrequencyHz` сохранены. Перед будущим действием wrapper по-прежнему требует настоящую свежую preflight receipt и проверяет удержанные фактические файлы.

Прежний `private-controller-b01-source-v2-1` остаётся **RETAINED UNKNOWN**. Анализ исходного owner относит наблюдённую ветвь `WJOB-PROCESS-CREATE` к отказу `CreateProcessW` до resume первого workload; код причины Windows не был сохранён. Позднейшие CLOSED-квитанции v3 не закрывают исходную v2-квитанцию и не заменяют её владельца.

Свежие installed capture, readonly-preflight, restore и обычная .66→.67 qualification в этом пакете **NOT_RUN**. Исторический b01 имеет transaction `40f3a26d38c601b3ff3471e3ad1cbe5c` и receipt `987a62fa146c865c5588e8e0a0f35d3dadbdfbba59d62db3e33ca3218853b832`; это исходные pins для нового наблюдения, а не полученный заново installed-state proof.

## Оставшаяся приёмка

Единый текущий план остаётся в #664: `.plans/2026-11/audit-remediation-20261125.md`; этот документ не создаёт второго плана.

Для P07 необходимы завершённые prerequisite-исправления финального main Source, его собственные inputs и стандартный кандидат 0.4.67 с согласованными package metadata. Существующая release-generation выдаёт пять генерируемых входов — `build.sh`, `build.ps1`, `cmd/mcphub/versioninfo.json`, `npm/package.json`, `npm/packages/win32-x64/package.json`. Изменённые bytes требуют своей Source epoch; их нельзя отождествлять с прежним `cp02` или private controller H7742. Далее нужны проверки полного пакета, свежее наблюдение incumbent b01, исходные preflight/restore receipts и один обычный upgrade с теми же двадцатью критериями.

Исторический реальный upgrade b01 дал **19/20**, единственный FAIL — строгое время меньше 60 s: **65.8537157 s** при exit 0 [12]. Новый результат 20/20 и новый замер **NOT_RUN**. Реальный Sequential Thinking 2.2.0, private migration lanes и два полных literal набора также сохраняют свои отдельные незавершённые критерии. Пятиминутная граница обязательных Go-наборов и P07 <60 s не увеличены.

## Источники результатов

Публичная проекция для существующего #664 находится в координационном
артефакте `.plans/2026-11/integration-audit-20261125/native-continuation-evidence.json`.
Она сохраняет run IDs и хеши прежних исходных квитанций; позднейшие owner-результаты ниже предназначены для следующего согласованного обновления той же проекции. Закрытые
evidence records сохранены в рабочем архиве аудита
`implementation/evidence/integration-native/`; сырые захваты в PR не включены:

1. `ara01-reviewed-positive-2.private.json`; SHA-256 исходной квитанции
   `6d988eb1ccd528fa7dbaa756b4a2adebc09fe16d0d304e9a4a59e04dda5771df`.
2. `ara01-causal-negative-1.private.json`; SHA-256 исходной квитанции
   `e3b1811ff157a8c9363e1aa31c0616826e4a4b3c67dfc1853d4f998f877b0c67`.
3. `qualification-producer-controls-5.private.json`; SHA-256 исходной квитанции
   `f59aa25bf4b7bdfaf33a7f59002929ab375274f07a4d2f069705717c65608f66`.

4. `owner-manifest.json`: `788bad616903e12323cb6603ac63f814e1b7b6359654e4adfdcf68f9daac113e`; `671-p07.patch`: `217753ec612697c4a05d5e19a044f07057e28aca32a7fb5abbb142da1928465a`; `671-fixtures.patch`: `186488705a6f0727d081812335dd1d8e887d1145de61136816637c9c0f6b2ecd`.
5. `literal-current-20261125-3`, окончательная driver receipt: `e6de1329356fd8cb9b4bbdbfed1c69f78dc0f9875442850f6373617996ef5097`. Full default/tagged FAIL, 67 original child closures; source `cc6879be…`.
6. `cli-followup-c894-1/receipt.json`: `15ff014f1fb8ae3cc518c6ee268efcd33febb5dd05c082e34217ff76ce892716`; root receiving: `4ce73fa902c582ca18d421306ef1ccdaf71ec12e2e63ce426a47a097724b9ad4`.
7. `api-followup-c894-1/receipt.json`: `4029be5358b8dbb5a168a29de5702c1a62f07b3fe8cb4cddc42a59f08254ebe7`; tagged NOT_RUN.
8. `api-followup-a5b81-1/receipt.json`: `bf726c42ebb624380e6e0ba02db035062c74b1c23592f2f9f1076de47c0835f3`; root receiving: `50ac3033617fbc6f47aa1a4a3866939bea6ad4c8b343d6ddf98dda85167c0062`.
9. `private-controller-b01-source-v3-1/receipt.json`: `1b2f95b08300238d5e6a4650c81520cad4264f1dbf12d9a401e342881f002371`; root receiving: `2ecf432222f629c0528f3550af948f20a845d25f1d518b148cc4c7cb85e43dee`.
10. `private-controller-b01-build-v3-1/receipt.json`: `9abeb2b14cd4c079ad233a71f2a26314f28fbbaa33b7d3f3757215cd708c0afb`; root receiving: `80cd9d60727dbaa9aecec9aad016d9e8185972048c2a4c2be78ecc0d7a5c77ba`.
11. `private-controller-b01-post-build-bindings.review.json`: `9a3ca652c4643fd34b84d27cb14c11aed2f5b3913fbb9036cecbaa799ff19464`; pin-only patch: `69807ff9ddc9db2eea02df91263c1c4533d1fe0885d4d7039017a64c412062fe`. Restore script: `84cd0a8c3cad3b3fef1aab3ab68fb51296098b359754d172d9349603182d1e62`; qualifier: `6c84a75e6e657953a3bd3f245dc3fc717004027c14011851b76485a9ffe6733b`.
12. Исторический `wave-upgrade-b01b496-1`: summary SHA-256 `80b1ba03c332fbf0fa60eee027584df91bed5db4669d9a1c4aad19e58a6fde92`, gates SHA-256 `663ac37a2370c2c80d7bf629daee62f040ea2542e3e80970f26a6432ec3f0334`.

Новых PR или слияний этот перенос не создаёт. После согласованной публикации требуется точное чтение owner head; scoped receipts остаются привязанными к своим интеграционным исходникам.

## Частичное открытие pipe: vendor follow-up

Набор подготовлен поверх owner head `58bd738c0944cd2706a13bc96fea22ec5a422d45`. Это продолжение существующего #671; API-приёмка результата исходного соединения остаётся отдельным pinned overlay в #664 после #637, #659 и #671. В этот owner-набор входят только четыре vendor-пути из таблицы 7. Нового PR, механизма запуска или владельца native-процесса нет.

Причина обнаружена в обычном error-return пути `DialPipeAccessImpLevel`: после успешного `CreateFile` ошибка чтения pipe metadata теряла исходный handle, а ошибка последующей инициализации отбрасывала результат его закрытия. Теперь сразу после `CreateFile` создаётся один исходный `win32File`. На промежуточном отказе он выполняет единственный `CloseObserved`; `PipeDialError.CloseResult` только принимает ту же квитанцию, сохраняя первичную ошибку и результат закрытия. Нулевое, скопированное или подменённое значение не выдаёт эту квитанцию.

**Таблица 7. Узкий vendor-набор и проверяемое поведение.**

| Путь в `third_party/go-winio/` | Изменение |
|---|---|
| `file.go` | Инициализация того же исходного объекта; uncertain handle хранится приватно в квитанции; `Fd` не выдаёт действующий handle после начала close |
| `pipe.go` | Исходный owner до metadata; единственное observed close при normal-return отказе; timeout wrapper сохраняет совместимый `ErrTimeout` вместе с actual partial-close error graph |
| `pipe_dial_close_observed_windows_test.go` | Три адресные регрессии: реальный post-open отказ metadata, управляемые точки отказа с тем же открытым handle и отказы до открытия; context API совместим с Go 1.21 |
| `UPSTREAM.md` | Обновлённая граница локальной vendor-поправки и происхождение |

Прежние callers `makeWin32File` сохраняют свою ответственность за error cleanup: автоматическое второе закрытие им не добавлено. Квитанция не предоставляет retry/reconnect/kill. Успешно принятое закрытие частично открытого ресурса остаётся обычным отказом; неизвестный либо неуспешный результат сохраняет исходный объект и причины для приёмки верхним владельцем.

Первый scoped gate на integration `afdf474930da60080e9aa132a3a373fcf2009bab` завершился **BUILD FAIL / CLOSED** до выполнения vendor-тестов: retained module объявляет Go 1.21, а пять новых вызовов `testing.T.Context` требуют Go 1.24. Run `p02-coherent-afdf4-scoped-1` занял 52.095 s; receipt SHA-256 `c9d5b6c4a7e60e1df504abb3361487586fb24e1906fd6bbd8a7983051050ed50`. Этот FAIL сохранён.

Исправление касается только нового test-файла: существующий шаблон `context.WithCancel(context.Background())` с `defer cancel()` сохраняет context до возврата root/subtest и отменяет его перед зарегистрированными cleanup. Предварительно отменённый контекст и прежняя истёкшая timeout-ветвь остаются отдельными негативными случаями. Assertions, исходное закрытие, версия модуля, vet и deadlines не менялись. Исходный vendor-патч и test-only поправка применяются последовательно [13–14].

Root применил и форматировал согласованный Source `b6f82b3f531bb3eec197049167cdc5971b44e2b0`, tree `645ddcd88647b8c628ec966f037762886bd56b31`. Четыре итоговых vendor-файла независимо прочитаны; они совпадают с postimages publication manifest. Новый test-файл имеет SHA-256 `c48ebeb2e746927d1f536b3cf5f0f985599e4fc6a168f8dbe1eb912bd8d42d2e`. Source-apply run `p02-compile-fixture-source-1` принят CLOSED/PASS за 9.382942 s, receipt SHA-256 `f8ad51369da17a1c156825502d201d3272e9728b859f0dde5d18d1aa29a20fa1`; source apply не является native acceptance.

**Адресный run `p02-coherent-b6f82-scoped-1` принят PASS / CLOSED на точном Source `b6f82b3…`.** Исходная receipt и пять полных JSONL stdout независимо прочитаны и хеш-проверены. Все десять original commands завершены с exit 0; native process/Job, handles и оба readers приняты закрытыми. Timeout и принудительного завершения не было. Controller занял 123.8721821 s; receipt SHA-256 `91d4dd78cca1e3c445c6330613f8bec3718216edf8cdc844210c14887e80f08a`.

**Таблица 8. Полученные проверки согласованной композиции `b6f82b3…`.**

| Выборка | Roots PASS / terminal PASS | Время команды, s | Результат |
|---|---:|---:|---|
| Vendor, новые dial tests и существующие close tests | 7 / 11 | 3.5447682 | PASS / CLOSED |
| API, default | 109 / 135 | 30.6358586 | PASS / CLOSED |
| API, `test_state_path_env` | 109 / 135 | 23.6797562 | PASS / CLOSED |
| Router, default | 9 / 12 | 15.6682386 | PASS / CLOSED |
| Router, `test_state_path_env` | 9 / 12 | 9.7948718 | PASS / CLOSED |
| Windows CLI, `go test -c` | Тесты не исполнялись | 27.7024083 | Compile-only PASS / CLOSED |
| Windows GUI, `go test -c` | Тесты не исполнялись | 12.0409485 | Compile-only PASS / CLOSED |

Во всех пяти исполняемых выборках FAIL=0 и SKIP=0. API/router строки относятся к полной согласованной композиции и не присваивают эти файлы владельцу vendor. Windows CLI/GUI здесь только скомпилированы. Эти scoped-результаты не заменяют full literal gate, private prepared migration, live Serena или Product/P07 acceptance. Самостоятельный owner head #671 не исполнялся.

Новые vendor roots: `TestPipeDialNativeMetadataRefusalClosesOriginal`, `TestPipeDialFailurePointsRetainOwnerAndAllCauses`, `TestPipeDialPreAcquireRefusalsHaveNoCloseOwner`. Первый использует реальный `CreateFile` и native metadata refusal на изолированном обычном файле; второй добавляет управляемые ошибки после реального открытия и единственного закрытия. Моделируемая ошибка close не является доказательством реального отказа Windows `CloseHandle`. Эти тесты не заменяют существующие named-pipe acceptance, приёмку native creator или same-run live Serena.

Граница: normal-return ошибки после открытия, а не panic и не новые гарантии для unrelated listener/socket cleanup. Persisted schema, admission counts и старые результаты не изменены. Самостоятельная собираемость всего #671 до его прежних зависимостей не заявляется.

13. `p02-pipe-dial-close.vendor.patch`, SHA-256 `a00dd7781f2b6958dd2441a278a4e9213f091ee56414eeb10afc602046873314`; исходный review manifest `p02-pipe-dial-close.manifest.json`, SHA-256 `f739437872796f05bfade2ad5bed6ebd746151974193d3d0fe2b7e813322f4ed`. Он также описывает отдельный API composition-тест, который не входит в эти четыре owner-файла.
14. `p02-pipe-dial-go121.patch`, SHA-256 `22079cb28e703284ac3d286d7e3ed68ecdcb6b6fdf84f16d6337a4aed7ee5e59`; manifest `p02-pipe-dial-go121.manifest.json`, SHA-256 `5a82ad6a6ba0fbba26e51718a90ffdc3cf6333ed7801654e8cc552400f116a2d`.

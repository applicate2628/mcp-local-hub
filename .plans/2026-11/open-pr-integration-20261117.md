# План повышения качества и сокращения очереди PR mcp-local-hub

**R24, 21.11.2026 — действующий план, не готовность к слиянию.** Очередь сокращена с **31 до 26 открытых Draft**. Снимок после пяти закрытий — **21.11.2026, 14:30 МСК**; `master=a7d92d2852c4f6eb8afb0569ef76a545e38aacf7`. Ни одного слияния. Код и границы проверок — §6; последующая сверка документов и очереди хранится в [аудите R24][r24].

Вся прежняя история сохранена в [этом же файле на неизменяемом `cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6`][history]. Якоря §8 ведут к историческим разделам; их квитанции относятся только к исходным источникам/входам.

## Оглавление

1. [Цель, источники и ограничения](#1-цель-источники-и-ограничения)
2. [Результат аудита и ответственность PR](#2-результат-аудита)
3. [Порядок выполнения и внешние входы](#3-порядок-выполнения)
4. [Обязательные критерии приёмки и прежние результаты](#4-обязательные-критерии-приёмки)
5. [Что сознательно исключено](#5-что-сознательно-исключено)
6. [Отчёт по каждому шагу R24](#6-отчёт-по-каждому-шагу)
7. [Ссылки и обозначения](#7-ссылки-и-обозначения)
8. [Индекс сохранённой истории](#8-адресная-диагностика-тайм-аутов-и-зависимостей-17112026)

## 1. Цель, источники и ограничения

Повышать корректность и простоту существующих PR, сохраняя уникальные правки, тесты и доказательства. **Слияние PR в любые ветки, включая auto-merge/squash, агенту запрещено.** Уменьшение очереди допустимо только после доказанного поглощения.

R24 разрешает компактные переносы в существующие целевые PR: один родитель — точная целевая вершина, без переписывания истории и посторонних изменений master. Это заменяет прежний запрет переносов. Публикация и закрытие поглощённых доноров входят в задачу; ветки не удалять.

Не разрешены новые установки/зависимости, платные/квотные исполнители, ручной CI, изменение установленного продукта, политики и полномочий Windows, сообщения и отзывы без отдельного допуска. Удалённая работа — только `D:\dev\ChatGPT`. Подтверждённо ненужное переносить в trash; активное, единственные копии и восстановление сохранять вне него; trash не очищать.

Основания: [правила][rules], [порядок приёмки][runbook], точные головы/деревья/полные различия, исходные квитанции и [R22/R23][r22]. Старые проверки не повторять ради даты; результат прежнего источника не переносить на новый HEAD.

## 2. Результат аудита

**Таблица 1 — ответственность исходных 31 PR.** Пять поглощённых PR закрыты без слияния; исходные ветки сохранены. Остальные 26 остаются Draft; сохранение не означает готовность.

| PR | Ответственность и действие R24 |
|---|---|
| #632, #635, #636, #637 | UV → издатель образа → защищённое пространство → миграционные фикстуры. Отсутствующие PATH guard #632 и telemetry guard #635 перенесены. База #637 изменена на #635; #636 закрыт после побайтного чтения всех его 14 файлов из опубликованного #637. |
| #639, #647 | Входы и Windows-среда подготовки: #639 полностью перенесён в #647 с тестами и идентичностью проекции; #639 закрыт. |
| #643, #645, #651 | Владелец дерева/очистка: #645/#651 полностью перенесены в #643 и закрыты; parent binding, отмена/close errors и обе нативные регрессии сохранены. |
| #646 | Выбор системного Python с managed fallback; сохранить отдельно. |
| #648 | Build-constraint: байты, идентичность, удерживаемый файл, install; сохранить отдельно. |
| #649 | HOME/USERPROFILE квалификации; отдельный владелец от среды сборки. |
| #650, #656 | Привязки/бюджет квалификации и исходный close result соединения: #656 полностью перенесён в #650 и закрыт. |
| #652 | Чтение через исходное дерево; сохранить отдельную квалификацию. |
| #653, #654, #655 | TCP-бюджет/IPv6; предел чтения Ready; settlement до отмены — три самостоятельных контракта. |
| #629 | Relay: ввод/вывод, отмена, сессия; сохранить. #630 уже включён исторически. |
| #657 | Нативный тестовый Windows Product/RunV5; сохранить, не расширять вывод на установленный продукт. |
| #624, #626, #628, #659 | CMake/Relay измерения, Windows-процесс и PID-only контракт — разные потребители. |
| #625, #644, #631 | Rust: владелец/идентичность/Go-интерфейс. В #631 добавлено описание прежней debug-квалификации process_view без изменения кода. #644 не править: гипотеза о пропавшем тексте отозвана после сравнения байтов. |
| #627 | Защищённый reader; сохранить. 11 зависимостей lock-файла не обеспечены квалифицированными локальными байтами. |
| #660 | Scheduler/автозапуск, снимки и восстановление; сохранить адресные завершения R17–R23. |
| #623, #658 | #623 — один компактный аудит R24; #658 — действующий план/тестовая диагностика. Историю сохранить неизменяемыми ссылками. |

Общие файлы не доказывают одинаковой ответственности. #633/#634/#630 исторические, вне этих 31 открытого PR. Для пяти закрытий проверены именно опубликованные байты и неизменённые исходные ветки.

## 3. Порядок выполнения

1. Сохранить головы/базы, различия и описания. Перед записью проверить дрейф; изменившийся кандидат пересчитать.
2. Собрать продолжения таблицы 1; доказать полное включение кода/тестов/свидетельств и отсутствие посторонних правок; единственный родитель — исходная целевая вершина.
3. Прочитать тесты/TestMain/среду, провести своё/независимое ревью. Проверку точной базы с overlay так и обозначать; общий gate и будущий коммит ей не квалифицированы.
4. Опубликовать допущенный кандидат, прочитать байты/дерево/родителя обратно, подтвердить базу #637→#635. Записать квитанции в §6.
5. Лишь после полного опубликованного включения закрыть #639/#645/#651/#656/#636; неполный донор оставить открытым, ветки сохранить.
6. Сократить описания с архивом, опубликовать/перечитать аудит #623, сверить очередь, выполнить подтверждённую trash-first очистку. Новых runtime-решений план не принимает.

**Таблица 2 — действительный остаток и внешние входы.** Ни одна строка не разрешает автоматически установку, смену политики или нативную мутацию.

| Область | Нужный вход / оставшаяся граница |
|---|---|
| Общие Go | Различающая гипотеза по API/CLI-бюджету, audit Flush/Close, GUI join и marker/version; выбранные тела и нужные временные события. Изоляция, receiving, original-entry Job-пара, force-kill и отмена родителя уже выполнены в своих срезах. |
| Serena / Product | Source-bound RunV5 Product, правильный уровень и реальные входы 14 migration-сценариев, исходный издатель/entrypoint/квитанции. Полный #648, внешний квалификатор #652 и настоящие cleanup/close остаются открыты; отдельные примитивы не доказывают весь lifecycle. |
| Windows | CST/PowerShell и допущенный wrong-owner токен. Эффективная политика PowerShell UNKNOWN после ошибки автозагрузки; отказ до builder не обходить иным исполнителем/политикой. |
| Reader #627 | Все 11 зависимостей точного lock-файла, локальные проверенные байты и политика потребителя; реальные stat/read/close/combined failures. Найденные archives/vendor/cargo-home просмотрены; имеются только windows-link/windows-sys с нужными хешами. Глобального отсутствия не доказано. |
| Rust / #631 | Настоящий потребитель исходного ресурса, SID/CreatorIdentity-допуск, межъязыковые вызовы и нативные ошибки. Внешние результаты mh_wait1/WaitForExit и mh_close1/CloseExited не передавались: сначала получить источники/квитанции, не дублировать реализацию. |
| Scheduler / R17 | Допущенные Run/RunOwned/Stop/Delete, активная отмена/срок дерева процессов, завершённое действие, владельцы/права/пароли/состояния. Прежние завершения — §4. |
| Install / prune | Сквозная установка/upgrade; внешняя замена, неопределённость prune, принудительный ImportXML, нативные частичные ошибки и отказы восстановления, расписания, Disabled/unknown, хранение снимков, register/upgrade/weekly. Общего CIM-классификатора нет; один ExportXML roundtrip не доказывает общую эквивалентность. |
| R12/R09/R16 | Пиковая память CMake/индексов и удержание результата потребителем; реальные сеть/маршрут/параллельность/сессия Relay; command line/родитель/CIM и native failures процесса; обоснованные бюджеты. R10/WP-11E и R13–R15 открыты в прежних границах. |
| Платформы / отзывы | Разрешённые POSIX-файловая система/toolchain и Unix/ARM64-входы; внешний отзыв на точных новых головах. Выполненный Linux x86-64 autostart-срез не закрывает всё Unix. COMMENTED/внутреннее чтение не равны APPROVED. |

## 4. Обязательные критерии приёмки

Нужны точный источник/diff, собственное ревью корректности/безопасности/совместимости/сложности, обязательные проверки с отказами/пропусками и внешний результат на текущем HEAD по [порядку репозитория][runbook]. Недостающий критерий не позволяет объявить готовность. Квотные проверки и ручной CI без отдельного допуска не запускаются.

**Таблица 3 — общие результаты; адресные PASS их не заменяют.** Источники: [Go recovery][recovery], [история][history].

| Источник / проверка | Сохранённый итог и причина |
|---|---|
| Исторический recovery-вход: `f755d374fab317e1037deecd5dac8ffabbcdde36` + незакоммиченная `internal/cli/gui_force_test.go` | build/vet PASS; full **FAIL 402,220887 с** (API/CLI/daemon/process); tagged API/CLI **FAIL 313,08449 с**. Это вход исходных квитанций, а не опубликованный позднее `231c80e5` и не нынешний HEAD/R24; [точная атрибуция](https://github.com/applicate2628/mcp-local-hub/blob/b1d932ea8925ef3324d539e5de006daeae65ba2a/docs/modernization/verification/completion-r22-r23-2026-11-21/audit/go-budget/README.md). |
| Новый #660: обычный full | **NOT_RUN**: нет ограждений четырёх TestMain из #658, возможен настоящий KnownFolder вне разрешённого D. |
| Новый #660: tagged API/CLI | **NOT_RUN**: изоляция есть; причина — неквалифицированные Product/entrypoint/Serena-входы и общие блокеры. |
| Смежный Scheduler R23 | Оболочка **FAILED**: 20 корней PASS, 77 тестовых/подтестовых PASS + пакетный PASS, но один `unsupported_platform` SKIP. Весь срез не PASS. |
| Проверки R24 | **PASS выбранной области:** 11 тестовых команд с `-race`, 57 обязательных выполнений корневых тестов, 270 конечных сценариев, 305 тестовых/подтестовых и 11 пакетных PASS, без FAIL/SKIP; 10 команд полного build/vet. Точная база + хешированный overlay; полные default/tagged тесты R24 **NOT_RUN**. Это не квалификация всей опубликованной вершины. |

Обязательные команды остаются `go test -count=1 -timeout 5m ./...` и `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`. Их прежние потоки получены **без `-json`/`-v`**, содержат не-UTF-8 байты. Есть напечатанные длительности пакетов/отказавших корней, но нет всех PASS/полной временной шкалы; восстановить отсутствующие интервалы нельзя. Необъяснённая пауза go-процесса не доказывает deadlock или причину в кэше.

Инвентаризация на чистом `f12932bcca6c288d062f9affd32ea58a7c9eb74f`: четыре `tools/testshards -execute=false`; API normal/tagged **3437/3443** корня, **43/44** группы; CLI **1960/1963**, **25/25** групп. В API по одному перечисленному benchmark; Example/Fuzz нет. **Тела/группы не исполнялись**, TestMain инициализировал/очищал среду. Все 3216 файлов и чистая вершина/дерево совпали до/после. Это состав, а не новый общий PASS. [Свидетельства][r22].

**Таблица 4 — завершения R22/R23 и пределы.** Полные источники, отрицательные контроли и квитанции — [неизменяемый комплект][r22]; новые запуски здесь не заявлены.

| Источник | Выполнено / предел вывода |
|---|---|
| CMake #624 `81dcf67d` | Удалены ссылки отброшенного хвоста; 3 сценария на настоящей трассе 11 827 770 байт сохранили ответ, адресные проверки/разовое измерение выполнены. Пиковая память/производственные бюджеты не доказаны. |
| Relay #629 `b3646f19` | Ожидание session ID не расходует GET-попытки SSE; различающая фикстура с атомарным счётчиком. Прежняя гонка теста сохранена. Настоящий сервер/сеть/нагрузка/срок сессии не квалифицированы. |
| Process #628 `00f120a5` | stopPending memory связана с PID/поколением/стартом при объединённом наблюдателе, без скалярного fallback после его выбора. Старый путь сохранён; реальное переиспользование PID и все native failures не доказаны. |
| #658 `1da98d1c`, `f12932bc` | Диагностика эффективной строгой политики; защищённая LSP-фикстура, фактический `-benchtime=1x` для 7 KiB/near-cap 14 MiB. Без ослабления допуска, заявления ускорения или устойчивого бюджета. |
| #660 `a6319962`, `610fb303` | Компенсация прежних шагов; при неопределённом Delete/Create сохранены причина/восстановление/adopt. Основной `-race`: 14 конечных новых сценариев, 16 PASS + пакет; контроль 16 FAIL + пакет. Нативная неопределённость/внешняя замена/пароли/вся установка не квалифицированы; смежный FAILED см. таблицу 3. |
| Linux `%`, #660 `47313f0b` | База `ba17867125b3728fb734e90b383639a3ff9175ee` + точный overlay: 13 конечных сценариев ×2, 28 PASS с родителями, 4 смежных; контроль 12 FAIL/2 PASS. systemd 257 отверг `%Z`, принял `%%Z`. Без регистрации/запуска служб; `source_commit` квитанций остаётся базой. |
| ExportXML, база `ba17867125b3728fb734e90b383639a3ff9175ee` | 2 регистрации/2 удаления/2 проверки отсутствия, 4 совпавших экспорта; отключённая hidden-задача без триггеров/on-demand. Не действие/установка/матрица владельцев. XML частный: публичные хеши без этих байтов не пересчитать. |
| Rust #625 `287a7772`, #644 `1cf5754b`, #631 `ebfa4ef3` | 53/76/20 выбранных успешных тестов. Go debug `-race` #631: **57 корней, 118 конечных сценариев, 129 тестовых/подтестовых PASS**. Не настоящий Product; `2eb7c114` изменил только README, без нового запуска кода. |

PID-only/status, hidden, положительный/отрицательный LastResult, причины отмены, guards и восстановление текущей задачи уже выполнены в прежних адресных границах. Их не возвращать в разработку. Исторические Serena/RunV5 и дерево `4c600ef` не квалифицируют новые головы PR. Полные идентификаторы и прежние пределы доступны в §8.

## 5. Что сознательно исключено

Не создавать агрегирующий PR или дублирующие ветки; не закрывать уникальный остаток по возрасту/размеру; не ослаблять входные проверки, лимиты или права ради PASS. Не внедрять Rust без решения по конкретному потребителю и измеренных критериев. Не повторять историю без нового вопроса. Сохранять нужные базы и доказательства. Эти ограничения не отменяют разрешённые переносы таблицы 1.

## 6. Отчёт по каждому шагу

**Таблица 5 — фактический результат R24.** Полные идентификаторы, исходные протоколы и последующая сверка публикации доступны в [аудите R24][r24].

| Шаг | Результат и предел |
|---|---|
| Источники | Сохранены исходные 31 описания и метаданные. Для #639→#647, #645/#651→#643, #656→#650 обратное вычитание переноса возвращает исходное целевое дерево. Все 14 файлов #636 прочитаны из нового #637; исходный #636 остаётся предком. |
| Ревью | Выполнены собственное и дополнительные независимые разборы; применены 15 прочитанных профильных навыков. Исправлены найденные ошибки упаковщика и атрибуции исторического Go FAIL. Формальное внешнее APPROVED этим не заменяется. |
| Квалификация | Все 15 фаз завершены, 13 кандидатных Go-файлов и точные исходные деревья связаны манифестами; результаты — таблица 3. Существующие зависимости, автономный режим, записываемые каталоги на D. Нет новой квалификации установленного продукта и срока всего дерева процессов. |
| Публикация кода | Шесть однородительских коммитов: #631 `412cd353`, #635 `f639b97c`, #637 `1ebe00c5`, #643 `44f9a3a9`, #647 `dca92972`, #650 `2a273c10`. Родители/деревья и изменённые байты прочитаны обратно. #637 имеет базу #635 и 30 изменённых файлов в сравнении GitHub; старый общий предок не переписывался. |
| Закрытия | #639/#645/#651/#656/#636 закрыты **21.11.2026, 14:27:01–14:27:12 МСК**, у всех `merged=false`, исходные головы и ветки сохранены. Новых PR не создано. |
| Очередь | Фактический снимок **14:30 МСК** после закрытий: **26 открытых, все Draft**. Изменённые кодовые головы перечислены выше, база изменена только у #637; master прежний. Сверка после публикации плана и аудита приводится отдельной датированной квитанцией. |
| Архив и очистка | Архив R24: **1 778 839 байт**, SHA-256 `099f6b1a26cb99e87fc765ad9ef54370510e06d5c951e91ac4a3472dddc7500d`; 2714 файловых элементов, 2712 входных хешей и два служебных JSON. Все входные публичные байты равны исходным; размер, CRC и хеши проверены после переноса. Проверенные передаточные файлы перенесены в trash; исходные архивы и результаты сохранены. |

Запись шага: **изменение → точный источник → фактическая проверка → предел → остаток**. Различать публикацию текста, сверку байтов, тест и нативную приёмку; код оболочки, PASS корней и итог квалификации могут различаться.

## 7. Ссылки и обозначения

1. [Прежний план `cf5a3b11`][history] — вся история и исходные ссылки.
2. [R22/R23, `b1d932ea`][r22] — архивы/квитанции, `audit/go-budget/README.md`, `audit/go-inventory/RESULTS.md`.
3. [Go recovery, `09565554`][recovery] — квалифицированные входы и последние общие FAIL.
4. [Итоговый аудит R24 в #623][r24] — текущая публикация, архивы и сверка очереди.
5. [Правила][rules] и [приёмка][runbook] — обязательные критерии; слияние агентом запрещено.

**Термины:** PR — запрос на включение изменений; Draft — черновик; HEAD — вершина ветки; SHA-256 — контрольная сумма; blob/tree — содержимое файла/дерева Git. PASS/FAIL/SKIP/NOT_RUN — успех/отказ/пропуск/не запускалось; UNKNOWN — не установлено. Overlay — перечисленная подмена файлов на точной базе. CI — непрерывная интеграция; API/CLI — программный интерфейс/командная строка; FFI — межъязыковой интерфейс; debug/`-race` — отладочная сборка/детектор гонок. PID/SID — идентификатор процесса/безопасности Windows; CIM/COM — управленческая/компонентная модели; XML — формат определения задачи. CST — CST Studio Suite; LSP — протокол языкового сервера; SSE — серверные события; TCP — транспортный протокол; UV — менеджер Python-сред. Product — комплект исполняемых файлов. R09–R24/WP-11E — прежние идентификаторы работ, их содержание в истории.

[history]: https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md
[r22]: https://github.com/applicate2628/mcp-local-hub/blob/b1d932ea8925ef3324d539e5de006daeae65ba2a/docs/modernization/verification/completion-r22-r23-2026-11-21/README.md
[recovery]: https://github.com/applicate2628/mcp-local-hub/blob/0956555462d356bd29a08af1b6df861a4fa10b0a/docs/modernization/verification/go-recovery-qualified-inputs-2026-11-20/RESULTS.md
[r24]: https://github.com/applicate2628/mcp-local-hub/pull/623
[rules]: ../../AGENTS.md
[runbook]: ../../docs/agent-runbook.md#pr-review--merge-workflow-mandatory

## 8. Адресная диагностика тайм-аутов и зависимостей, 17.11.2026

[Диагностика 17 ноября в неизменяемой истории](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#8-адресная-диагностика-тайм-аутов-и-зависимостей-17112026). Текущие задачи — §§1–6.

### Индекс 34 сохранённых HTML-якорей

H01–H34 — указатели на исходные разделы фиксированной истории, не новые этапы разработки.

<a id="go-gates-20261117"></a>
H01. [Go gates](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#go-gates-20261117).

<a id="go-testshards-20261118"></a>
H02. [Go shards](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#go-testshards-20261118).

<a id="platform-preflight-20261118"></a>
H03. [Платформы](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#platform-preflight-20261118).

<a id="measurements-review-20261118"></a>
H04. [Измерения/reader](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#measurements-review-20261118).

<a id="serena-clean-source-20261118"></a>
H05. [Serena: исходник](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#serena-clean-source-20261118).

<a id="serena-cleanup-review-20261118"></a>
H06. [Serena: cleanup](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#serena-cleanup-review-20261118).

<a id="serena-independent-review-20261118"></a>
H07. [Serena: ревью](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#serena-independent-review-20261118).

<a id="relay-uv-product-review-20261118"></a>
H08. [Relay/UV/Product](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#relay-uv-product-review-20261118).

<a id="remaining-after-relay-20261118"></a>
H09. [Остаток Relay](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#remaining-after-relay-20261118).

<a id="rust-measurement-product-review-20261118"></a>
H10. [Rust/измерения](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#rust-measurement-product-review-20261118).

<a id="remaining-after-rust-review-20261118"></a>
H11. [Остаток Rust](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#remaining-after-rust-review-20261118).

<a id="consumer-maps-20261118"></a>
H12. [Потребители](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#consumer-maps-20261118).

<a id="remaining-after-consumers-20261118"></a>
H13. [Остаток карт](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#remaining-after-consumers-20261118).

<a id="r16-port-observation-20261118"></a>
H14. [R16: PID](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#r16-port-observation-20261118).

<a id="remaining-after-r16-port-observation-20261118"></a>
H15. [Остаток PID](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#remaining-after-r16-port-observation-20261118).

<a id="r16-status-observation-20261119"></a>
H16. [R16: status](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#r16-status-observation-20261119).

<a id="remaining-after-r16-status-observation-20261119"></a>
H17. [Остаток status](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#remaining-after-r16-status-observation-20261119).

<a id="r17-contracts-20261119"></a>
H18. [R17: контракты](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#r17-contracts-20261119).

<a id="remaining-after-r17-contracts-20261119"></a>
H19. [Остаток R17](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#remaining-after-r17-contracts-20261119).

<a id="r17-hidden-task-20261119"></a>
H20. [Hidden task](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#r17-hidden-task-20261119).

<a id="remaining-after-r17-hidden-task-20261119"></a>
H21. [Остаток hidden](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#remaining-after-r17-hidden-task-20261119).

<a id="r17-export-native-20261119"></a>
H22. [Нативный экспорт](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#r17-export-native-20261119).

<a id="public-go-read-checkpoint-20261119"></a>
H23. [Go: чтения](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#public-go-read-checkpoint-20261119).

<a id="r17-last-result-20261120"></a>
H24. [LastResult](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#r17-last-result-20261120).

<a id="native-last-result-and-cmd-env-20261120"></a>
H25. [LastResult/CMD](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#native-last-result-and-cmd-env-20261120).

<a id="go-state-isolation-and-negative-last-result-20261120"></a>
H26. [Изоляция/LastResult](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#go-state-isolation-and-negative-last-result-20261120).

<a id="go-recovery-qualified-inputs-20261120"></a>
H27. [Go recovery](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#go-recovery-qualified-inputs-20261120).

<a id="scheduler-create-cim-inputobject-20261120"></a>
H28. [Create/CIM](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#scheduler-create-cim-inputobject-20261120).

<a id="scheduler-prune-resolver-20261120"></a>
H29. [Prune/resolver](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#scheduler-prune-resolver-20261120).

<a id="scheduler-cancellation-20261120"></a>
H30. [Отмена/CLI](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#scheduler-cancellation-20261120).

<a id="install-snapshot-guard-20261121"></a>
H31. [Допуск замены](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#install-snapshot-guard-20261121).

<a id="current-task-recovery-20261121"></a>
H32. [Восстановление](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#current-task-recovery-20261121).

<a id="r22-r23-completion-20261121"></a>
H33. [R22/R23](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#r22-r23-completion-20261121).

<a id="remaining-after-r17-export-20261119"></a>
H34. [Остаток до R24](https://github.com/applicate2628/mcp-local-hub/blob/cf5a3b11eab69e968ae1efeeaf2016b5a7a990b6/.plans/2026-11/open-pr-integration-20261117.md#remaining-after-r17-export-20261119).


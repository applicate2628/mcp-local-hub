# Go: восстановление владения и квалифицированные тестовые входы

**Результат среза:** исправлена проверяемая граница одного force-kill-теста; контрольная пара воспроизвела преждевременную отмену старого варианта и успешное восстановление новым. Кандидат и точная опубликованная редакция прошли целевые повторы с детектором гонок. Отдельно прошли принимающий API/CLI-тест с шестью дочерними командами и две выбранные Windows Job-проверки с подтверждённым завершением исходного процесса и ресурсов. [Контроли (2026/11)][e-controls] [Candidate][e-candidate] [Published][e-published] [API][e-api] [Job][e-job]

**Обе общие тестовые команды остаются FAIL:** полный запуск — **402,220887 с**, tagged API/CLI — **313,08449 с**. Сборка и статическая проверка vet прошли; точные результаты приведены в разделе 5. [Общие проверки][e-gates] Отсутствующий вход RunV5, контракт Serena и отказ чтения политики PowerShell остаются самостоятельными вопросами; результат force-kill на них не переносится. [Квалифицированные входы (2026/11)][e-qualified] [Политика (2026/11)][e-policy]

API — программный интерфейс; CLI — интерфейс командной строки; GUI — графический интерфейс; PID — идентификатор процесса; DACL — избирательный список управления доступом Windows; PE — Portable Executable, формат исполняемых файлов Windows (подсистема 2 — GUI, 3 — консольная). PASS означает успешное завершение проверки, FAIL — неуспех, SKIP — пропуск.

## Содержание

1. [Редакции и свидетельства](#identity)
2. [Одна правка force-kill и её контроли](#force-kill)
3. [Квалифицированные API и Job-входы](#qualified)
4. [Трасса concurrent intent](#trace)
5. [Общие проверки и остаток](#gates)
6. [Аудит](#audit)
7. [Источники](#sources)

<a id="identity"></a>
## 1. Редакции и свидетельства

**Таблица 1 — привязка результата к исходникам и архиву.**

| Объект | Идентичность | Основание |
|---|---|---|
| Базовый #658 | `f755d374fab317e1037deecd5dac8ffabbcdde36` | [Квитанция исходной пробы][e-intent] |
| Дерево основы | `19056c7b39ed2f625cb7f0b5282343c92b3b3a48` | [Квитанция до/после][e-intent] |
| Единственный изменённый файл | `internal/cli/gui_force_test.go`, только `TestForce_KillHappyPath_SeamMocked`; **Git/GitHub +59/−45 строк** | [Опубликованный коммит][s-commit] |
| SHA-256 файла кандидата | `db019916c49da6c5be02298559690b368e431e0c01feb95a786add30b875eba8` | [Manifest v2 и независимое ревью][e-review] |
| Git blob кандидата | `06ec8f6d6a624dce040390532bda824b2553b46a` | [Независимое ревью][e-review] |
| Опубликованная редакция кода | `231c80e547816781ed2d77b34dd476f4400f8221` | [Опубликованный файл][s-candidate] |
| Дерево опубликованной редакции | `ec8f3d481b4d0df50a61924db8d6e6a8fcaa54f0` | [Опубликованный коммит и дерево][s-commit] |
| Архив свидетельств | [evidence-redacted.zip][evidence] | SHA-256 `d12ca2b81b815d18e4a49c659516e8f29c05709542aefb86919b9b5101b178ab`; **298833 байт**, **274 членов** |

Исторические **+61/−47** в manifest и ревью получены Python `difflib`; Git/GitHub иначе выравнивают повторяющиеся строки и дают **+59/−45**. Сравниваются одинаковые исходные и конечные байты, SHA-256 и Git blob кандидата совпадают. Исторические manifest сохранены. Код опубликован в существующем #658 без слияния. [Коммит][s-commit] [Manifest v2][e-static]

API, Job и intent-проба исполнены на чистой основе. Предпубликационные force-kill-проверки связаны с **основой плюс точными байтами одного изменённого файла**: поле дерева коммита в их квитанциях само по себе не описывает незакоммиченную правку. Проверка опубликованной редакции указана отдельно, без переноса результата между редакциями. [Квитанции фаз][e-candidate] [API][e-api] [Job][e-job] [Intent][e-intent]

Публичный ZIP содержит **редактированные копии** квитанций и журналов. В `manifest.json` поля `source_sha256/source_bytes` относятся к исходным приватным байтам, `saved_sha256/saved_bytes` — к опубликованной копии; их нельзя подменять друг другом. Приватные исходные трассы, бинарные файлы и нативные протокольные журналы в публичный архив не переносятся. Нормализованные проекции Remote Desktop Commander также не являются первичными байтами. Исходные файлы проверены упаковщиком; перенесённый архив и каждый его член затем независимо сверены по контрольной сумме и размеру. Результат этой проверки сохранён в [archive-verification.json](archive-verification.json); обратное чтение GitHub фиксируется отдельным итоговым комментарием после публикации. [Формат архива и manifest][e-manifest] Статусы `NOT_RUN` в подготовительных manifest описывают исторический этап статического ревью; последующее исполнение подтверждают квитанции фаз, с указанным выше различием исходных и редактированных байтов. [Manifest v2][e-static] [Контроли][e-controls] [Candidate][e-candidate]

<a id="force-kill"></a>
## 2. Одна правка force-kill и её контроли

Исходный тест отменял контекст по трёхсекундному таймеру и затем отвергал защитный exit 4, возникающий при отмене до принудительного завершения процесса. v2 вызывает конечную операцию `runForceKill` и требует одновременно: активный контекст, код 0, полученную блокировку, текущий PID и сохранённый исходный порт 1. Код 0 без блокировки не принимается. [Исходный тест][s-baseline] [Кандидат][s-candidate] [Контрольные потоки][e-controls]

Исходный ребёнок имеет единственное `Process.Wait`; исходная тестовая блокировка освобождается после наблюдения его выхода. Новая блокировка получает очистку до последующих проверок: успешный `TerminalSettle` предшествует `ReleaseErr`, ошибки наблюдаются тестом. 30-секундный контекст — аварийный ограничитель; его истечение не считается успехом и не гарантирует прерывание любого системного ввода-вывода вместе с cleanup. [Кандидат][s-candidate] [Независимое ревью v2][e-review]

**Таблица 2 — фактически выполненные контроли с одинаковой задержкой первого identity gate на четыре секунды.**

| Вариант | Тест / процесс `go test`, с | Наблюдённая последовательность | Итог |
|---|---:|---|---|
| Инструментированный старый тест | **5,28 / 27,623032** | Вход `ctx=<nil>` → выход `ctx=context canceled`; возврат команды `exit_code=4`; `helper_settled … wait_unlock_error=<nil>` | Ожидаемый FAIL; код `go test` **1** |
| Инструментированный v2 | **5,22 / 18,044218** | Вход и выход `ctx=<nil>`; `exit_code=0 acquired_lock=true`; новый PID, `port=1`; `recovered_lock_settled`; помощник завершён без ошибки ожидания/освобождения | PASS; код `go test` **0** |

Источник таблицы 2: [`forcekill-controls/receipt.json` и оба stdout][e-controls]. Старый вариант содержит явно выделенную безопасную очистку помощника и **не называется нетронутой исходной версией**. Таймер и прежние утверждения сохранены. Общая задержка, диагностические метки и оба контрольных файла не входят в правку кандидата. [Контроли v2: README, manifest и cleanup-only.diff][e-control-design]

Без контрольной задержки точный кандидат проверен с `-race -count=2 -timeout=120s` в `internal/cli` и `internal/gui`. Выбраны `TestForce_KillHappyPath_SeamMocked`, `TestForce_KillRefusesNonMcphubImage` и `TestKillRecordedHolder_CancelledContextRefusesKill`. Получены **6 событий PASS тестов и 2 события PASS пакетов, 0 FAIL, 0 SKIP**; внешний процесс завершился за **78,019253 с**, код 0. `gofmt -d` не дал изменений, `git diff --check` прошёл. [Candidate receipt и candidate-race.stdout.log][e-candidate]

**Граница покрытия:** v2 проверяет восстановление владения, завершение своего ребёнка и освобождение полученного владельца. Он не запускает GUI-сервер и не доказывает получение результата исходной серверной горутины. Соседний `TestForce_KillRewritesPidportWithCurrentPID` остался прежним: публикация нового порта не доказывает полный join, а его предел ожидания команды ограничивается предупреждением. Это существующее ограничение покрытия не объявлено исправленным. [Независимое ревью][e-review] [Соседний исходный тест][s-baseline]

Код приложения, проверка идентичности процесса, запрет kill после отмены, Restart V3 и Rust-пути wait/close не изменены. Разрешающий identity seam остаётся тестовой предпосылкой: этот тест сам по себе не квалифицирует производственный механизм проверки идентичности процесса. [Однофайловый diff][e-static] [Кандидат][s-candidate]

<a id="qualified"></a>
## 3. Квалифицированные API и Job-входы

**Таблица 3 — выбранные успешные запуски; время теста отделено от времени внешнего процесса.**

| Проверка | Подготовка | Наблюдённый результат | Область |
|---|---|---|---|
| `TestAttachExisting_PublicCLIReceiving` | Tagged CLI из `./cmd/mcphub`, путь и SHA-256 переданы consumer; сборки CLI **17,646174 с**, теста **3,130363 с** | Тест **2,68 с**; непосредственный тестовый процесс **4,160731 с**, код 0, без пропуска | Шесть публичных дочерних CLI-команд, изолированные конфигурации, имитированный supervisor status |
| Исходный контроллер `tools/test-job-entry` | Контроллер и GUI-подсистема `process.test.exe` собраны из той же основы и checkout; собственный приватный run-root | Контроллер **2,162777 с**, код 0; два теста и обязательный native-подтест PASS | Исходный владелец Windows Job, назначенного при создании процесса |

Источник таблицы 3: [API receipt и public-cli-receiving.stdout.log][e-api]; [Job receipt, qualified-job-entry.stdout.log и native-result][e-job].

Принимающий CLI связан с SHA-256 `b0e6f756735d1b612d4792a29f90485a71e7b16891ac7d3657187d4c72f903ab`. Это проверка реального публичного приёма CLI в указанной фикстуре; живой supervisor и канонический устанавливаемый Product ею не квалифицированы. Этот consumer не требует producer из #657. [API receipt][e-api] [Контракт принимающего теста][s-api]

Job-набор включает `TestJob_BreakawayEnabledDescendantEscapesClose`, `TestJobInformationTransportPreparationBoundary` и его `real-native-typed-gc-and-creator`. Проверены исходное завершение и native-учёт, а не только поле `success`. [Native-result][e-job]

**Таблица 4 — конечное состояние исходного Job-запуска.**

| Поле native-result | Значение |
|---|---|
| `originalWaitKnown / originalExitCode` | `true / 0` |
| `RootIdentity.Known / RootWaitKnown / RootExitCode` | `true / true / 0` |
| `TerminalAction` | `not-requested` |
| `MembersExited / AccountingKnown` | `true / true` |
| `InitialTotal / FinalActive` | `5 / 0` |
| `HandlesClosed / settlementComplete` | `true / true` |
| `stdoutEOF / stderrEOF` | `true / true` |
| `success / error` | `true / пусто` |

Источник таблицы 4: [`evidence/job-entry/native-run/result.json`][e-job]. EOF означает достижение конца потока. `TerminalAction=not-requested` относится к конечному урегулированию внешнего контроллера; это не утверждение, что выбранные тесты вообще не завершали собственные процессы. Положительный результат ограничен этим Job-набором. [Контракт контроллера][s-job] [Ревью его жизненного цикла][e-job-review]

<a id="trace"></a>
## 4. Трасса concurrent intent

`TestDaemonIntent_ConcurrentWrite` прошёл за **18,10 с**, выбранный запуск пакета — **18,228 с**, процесс `go test` — **76,384408 с**. Из теста и PASS следуют 96 логических обновлений и успешная итоговая проверка состояния. Разность между временем процесса и теста не является отдельно измеренным временем компиляции. [Intent receipt и stdout][e-intent] [Тест][s-intent-test]

**Таблица 5 — накопленные задержки syscall-профиля.**

| Кадр или цепочка | Задержка cum, с |
|---|---:|
| Все учтённые системные вызовы | 141,55 |
| `LockFileEx` | 123,07 |
| `File.Sync → Fsync → FlushFileBuffers` | 18,06 |
| `writeIntentLocked` | 6,72 |
| `AppendIntentAudit` | 5,67 |
| Обёртки inode-anchored read | 5,83 |
| `LogHubMcpEvent` | 5,81 |

Источник таблицы 5: [`evidence/intent-trace-01/syscall-top.stdout.log`][e-trace]. `cum` включает вложенные вызовы. Ожидания разных горутин перекрываются во времени; вложенные кадры повторяют один интервал. Поэтому строки таблицы нельзя складывать в длительность теста, превращать в число вызовов или считать процентами процессорного времени. В sync-профиле также присутствуют ожидания `WaitGroup` и служебных горутин трассировки; они не добавляются к реальному времени теста. [Профили и подробная интерпретация][e-trace]

Профиль дополнил прежний перечень «96 state Sync + 96 audit Sync»: проверяемое чтение может отправить предупреждение о правах доступа в Hub-журнал, который тоже выполняет `Sync`, пока внешняя intent-блокировка удерживается. Предупреждения родительского каталога агрегируются; предупреждения самого файла идут прямо в журнал. Вид и количество событий из `top` неизвестны. Кроме того, ошибка intent-аудита игнорируется вызывающим кодом: PASS не доказывает доставку всех 96 записей аудита. [Intent][s-intent] [Windows-чтение][s-read] [Агрегация][s-parent] [Hub Sync][s-hub]

Наблюдения согласуются с задержкой владельца блокировки на сохранении и ожиданием остальных писателей. Физическая причина задержек носителя, влияние трассировки и причина исчерпания общего пятиминутного бюджета пакетов **не установлены**. Точный счёт операций и эксклюзивное разложение критического пути не получены. [Подробная интерпретация][e-trace]

<a id="gates"></a>
## 5. Общие проверки и остаток

**Таблица 6 — завершённые общие команды; обе тестовые команды неуспешны.**

| Команда | Время внешнего процесса, с | Результат |
|---|---:|---|
| `go build ./...` | **9,33089** | PASS |
| `go vet ./...` | **11,204094** | PASS |
| `go test -count=1 -timeout 5m ./...` | **402,220887** | FAIL: `internal/api`, `internal/cli`, `internal/daemon`, `internal/process` |
| `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/` | **313,08449** | FAIL: `internal/api`, `internal/cli`; **15** верхнеуровневых имён отказа |

Источник таблицы 6: [квитанция и stdout общих проверок][e-gates]. Четыре верхнеуровневых имени отказа: `TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade` при отсутствующем квалифицированном CLI test Product; `TestCstDirectFrontendCrossesGoCapabilityAndFixedLocalPipe` с `UnauthorizedAccess` до builder; `TestJob_BreakawayEnabledDescendantEscapesClose`; `TestJobInformationTransportPreparationBoundary`, включая native-подтест. Именованного отказа force-kill happy path в этом журнале нет. [full-test.stdout.log][e-gates]

Те же четыре верхнеуровневых имени уже были в предыдущем полном срезе; отсутствие force-kill FAIL здесь не является новым улучшением. Таймерный дефект подтверждают исходник и отдельная контрольная пара раздела 2. Два времени полного запуска, **400,435 → 402,220887 с**, не дают основания заявлять ускорение или регрессию производительности. [Предыдущий архив, `evidence/gates/receipt.json`][previous-evidence] [Новый full-test][e-gates] [Контроли][e-controls]

API исчерпал бюджет за **300,047 с**: текущий `TestAttachExisting_FreshCommittedPlanApplyRefusesDrift` указан с возрастом **0 с**. CLI — за **300,103 с**: `TestMCPFrontV3_SerenaConflictClassifierMutationTable` с возрастом **3 с**, его `baseline-marker-is-invalid` — **0 с**. Это снимки в момент общего тайм-аута; они не доказывают взаимную блокировку названных тестов и не превращают 300 секунд в их длительность. [full-test.stdout.log][e-gates]

Tagged-срез содержит **15** верхнеуровневых имён отказа: RunV5 и 14 Serena. Прежний отказ receiving из-за отсутствующего входа не повторился; отдельный `TestAttachExisting_PublicCLIReceiving` действительно имеет PASS **2,68 с** (таблица 3). Отсутствие строки FAIL в полном журнале само по себе не доказывает PASS этого теста. [Tagged stdout][e-gates] [Выбранный receiving stdout][e-api]

В tagged API тайм-аут наступил за **300,069 с**, текущий `TestExecuteDeAdoptT8RoutedSecretPrefilterSharedSkipAndClose` имел возраст **1 с**. В CLI — **300,063 с**, текущий `TestMigrateSerena_AppliedInstallContinuationMatrix` — **4 с**, его `timeout-probe-error-hard` — **0 с**. Эти текущие кадры также не устанавливают причину общего бюджета или взаимную блокировку. [tagged-api-cli.stdout.log][e-gates]

**Точная опубликованная редакция:** `231c80e547816781ed2d77b34dd476f4400f8221`, дерево `ec8f3d481b4d0df50a61924db8d6e6a8fcaa54f0`, чистые HEAD/tree/status перед повтором и после него. Повтор тех же трёх тестов с `-race -count=2` завершился за **16,364898 с**, код **0**, `completed=true`: **6 тестовых и 2 пакетных PASS, 0 FAIL/SKIP**. Это самостоятельное свидетельство опубликованных байтов; предпубликационные 78,019253 с относятся к другому запуску. [Published receipt и published-race.stdout.log][e-published]

**Таблица 7 — конкретные незакрытые вопросы вне однофайловой правки.**

| Вопрос | Что установлено и чего недостаёт | Основание |
|---|---|---|
| RunV5 unreadable intent | Consumer требует `MCPHUB_CANONICAL_ENTRY_FIXTURE` и совпадение реально исполняемого файла с test Product. В основе #658 отсутствует producer command-test Product; отдельный владелец #657 закреплён на `4fa52b60b719664ede441be339e196a8b9766c50`. Его ветка и PASS не перенесены сюда. | [Контракт][s-runv5], [runbook основы][s-runbook], [разбор входов][e-qualified] |
| Serena | В ранее сохранённом tagged-срезе **13** отказов ожидали PE-подсистему **2**, получив **3**; ещё **1** дал `SERENA_BUILD_SOURCE_INVALID`. Старый source literal не имеет `@<40 hex commit>`. Одной замены PE недостаточно: реальная подготовка требует действительных publisher/source/tool inputs и установки. Граница unit-оркестрации и реальной preparation должна быть согласована отдельно. | [Разбор 14 отказов][e-serena], [parser source][s-serena] |
| CST-фронтенд и PowerShell | Отдельный read-only запрос `Get-ExecutionPolicy` не получил значения политики из-за ошибки автозагрузки модуля. **Фактическая действующая политика неизвестна.** Отказ запроса не доказывает конкретную policy; системная политика не менялась и обход не выполнялся. | [Prepare receipt и powershell-policy.stderr.log][e-policy] |
| Общий бюджет и GUI join | Intent-проба не объясняет весь общий бюджет. Полное завершение GUI-сервера не доказано изменённым recovery-тестом; существующий пробел соседнего теста сохранён явно. | [Трасса][e-trace], [ревью v2][e-review], итоговые gates выше |

Остаток содержательной работы — вопросы таблицы 7 и существующего общего плана. Подготовка и проверка архива завершены; отдельный итоговый комментарий в PR фиксирует последующее обратное чтение опубликованных файлов. Запрос на включение изменений (PR, pull request) не слит; внешнее одобрение изменения не заявляется.

<a id="audit"></a>
## 6. Аудит

Собственное и независимое ревью проверили критерий успеха, единственное ожидание ребёнка, порядок завершения владельца и освобождения блокировки, ранние отказы и границу покрытия. Замечание v1 о раннем возврате без результата сервера закрыто **в изменённом тесте** переходом к конечной операции восстановления; исправление полного GUI-пути не заявлено. Статическое ревью дополнено фактическими контролями и целевым race-запуском. [Ревью v2][e-review] [Контроли][e-controls] [Candidate][e-candidate]

Отдельное ревью опубликованного runner и наблюдателя подтвердило исправление коллизии имени журнала до исполнения и проверило точные привязки исходников, критерии PASS, обработку ошибок и предел наблюдения. Оно является внутренним независимым ревью, а не внешним одобрением. [PUBLISHED-HARNESS-REVIEW][e-harness]

**Посленаблюдение:** `post-verify` завершилось за **0,556566 с**, код **0**, `completed=true`, `settled=true`, `ABSENCE_OBSERVED`, `remaining=[]`. Запрос искал только точный маркер рабочего корня этой серии; `whole_process_tree_proven=false`. Он не доказывает отсутствие всех потомков или чужих процессов и ничего не останавливал. [Post-verify receipt и stdout][e-post]

Квитанции выполненных фаз фиксируют отсутствие установки программ, изменения системной политики и интеграции веток. Выходные данные и временные каталоги собственных запусков размещены в разрешённой области `D:\dev\ChatGPT`. Для API, Job и force-kill output защищён DACL, а новые дочерние временные каталоги наследуют разрешения; intent-проба использовала ранее выделенный `runtime/tmp`. Общая Python-обвязка честно указывает `whole_process_tree_deadline_enforced=false`: прямой дочерний процесс наблюдается, общий принудительный срок всего дерева процессов не заявляется. Job имеет отдельное нативное подтверждение завершения из таблицы 4. [Квитанции фаз][e-api] [Job][e-job] [Candidate][e-candidate] [Intent][e-intent]

Проверки защиты приложения не ослаблены, задержки контроля не включены в код, Rust wait/close не затронуты. #657 и #660 не интегрированы; общий набор не объявляется успешным по частным PASS. [Diff v2][e-static] [Квитанции][e-candidate] [Общие проверки][e-gates]

Упаковщик завершился с кодом 0 и подтвердил сохранённые исходные размеры/хеши, порядок команд, отрицательный контроль, Job-завершение и опубликованные PASS. После переноса проверены все **274** члена ZIP, **271** запись исходной/публичной копии и карта **337** исходных файлов; общие коды выхода остались **0, 0, 1, 1** для build, vet, full и tagged. Ограниченный поиск известных форматов секретов и локальных идентификаторов прошёл; он не является доказательством отсутствия любого неизвестного секрета. Исполняемые файлы, исходные trace, ACL и нативные протокольные журналы исключены. Неуспешные подготовительные фазы чтения политики и UTF-16 ACL сохранены; исправление декодирования не меняло системные права. [Проверка переноса](archive-verification.json) [Состав архива](archive-metadata.json) [Ревью упаковщика][e-packager]

В этой серии прочитаны и применены **12 релевантных навыков**, сгруппированных по назначению:

- Диагностика и измерения: `agent-skills:debugging-and-error-recovery`, `engineering-skills-for-go:go-testing-and-verification`, `engineering-skills-for-go:go-performance-and-diagnostics`.
- Go-контракты и ревью: `engineering-skills-for-go:go-language-engineering`, `engineering-skills-for-go:go-project-and-api-design`, `engineering-skills-for-go:review-go-engineering-change`.
- Простота и организация проверки: `engineering-suite-ponytail:code-simplification`, `software-architect:architecture-review`, `superpowers:dispatching-parallel-agents`.
- Привязка исходников и завершение работы: `agent-skills:source-driven-development`, `agent-skills:git-workflow-and-versioning`, `caveman:verify-and-stop`.

<a id="sources"></a>
## 7. Источники

Все обозначения архива ниже относятся к **редактированным копиям** в [evidence-redacted.zip][evidence]; член ZIP указан явно. `manifest.json` отдельно связывает исходные приватные и сохранённые публичные байты. Архив целиком и все 274 члена проверены по параметрам таблицы 1 и [внешним метаданным](archive-metadata.json). Результаты локальной проверки сохранены отдельно; последующее обратное чтение GitHub указывается в итоговом комментарии.

- **Controls (2026/11):** `evidence/forcekill-controls/receipt.json`, `delayed-baseline.stdout.log`, `delayed-candidate.stdout.log` в той же папке.
- **Candidate (2026/11):** `evidence/forcekill-candidate/receipt.json`, `candidate-race.stdout.log`, `candidate-gofmt.stdout.log`, `candidate-diff-check.stdout.log`.
- **API (2026/11):** `evidence/api-receiving/receipt.json`, `public-cli-receiving.stdout.log`.
- **Job (2026/11):** `evidence/job-entry/receipt.json`, `qualified-job-entry.stdout.log`, `evidence/job-entry/native-run/result.json`.
- **Intent (2026/11):** `evidence/intent-trace-01/receipt.json`, `intent-concurrent-trace.stdout.log`, `syscall-top.stdout.log`, `sync-top.stdout.log`; аналитическое дополнение `budgets/TRACE-INTERPRETATION.md`.
- **Gates / Published (2026/11):** `evidence/gates/receipt.json`, соответствующие `build`, `vet`, `full-test`, `tagged-api-cli` stdout; `evidence/published/receipt.json`, `published-race.stdout.log`.
- **Post-verify / harness (2026/11):** `post-verify/receipt.json`, `post-verify/stdout.log`, `post-verify/stderr.log`; `review-forcekill/PUBLISHED-HARNESS-REVIEW.md`, `review-forcekill/PACKAGER-REVIEW.md`.
- **Policy (2026/11):** `prepare/receipt.json`, `prepare/powershell-policy.stderr.log`.
- **Static review (2026/11):** `forcekill/v2/manifest.json`, `forcekill/v2/candidate.diff`, `review-forcekill/REVIEW-v2.md`; `forcekill/controls/v2/README.md`, `manifest.json`, `delayed-baseline/cleanup-only.diff`.
- **Qualified inputs (2026/11):** `qualified-inputs/FINDINGS.md`, `qualified-inputs/JOB-RUN-REVIEW.md`, `qualified-inputs/serena-failures.json`. Исходные прежние Serena-диагностики: [закреплённый предыдущий архив][previous-evidence], член `evidence/gates/tagged-api-cli.stdout.log`.

[evidence]: evidence-redacted.zip
[e-controls]: evidence-redacted.zip "Члены evidence/forcekill-controls/receipt.json и оба контрольных stdout.log"
[e-candidate]: evidence-redacted.zip "Члены evidence/forcekill-candidate/receipt.json и candidate-race.stdout.log"
[e-api]: evidence-redacted.zip "Члены evidence/api-receiving/receipt.json и public-cli-receiving.stdout.log"
[e-job]: evidence-redacted.zip "Члены evidence/job-entry/receipt.json и native-run/result.json"
[e-intent]: evidence-redacted.zip "Члены evidence/intent-trace-01/receipt.json и intent-concurrent-trace.stdout.log"
[e-trace]: evidence-redacted.zip "Члены evidence/intent-trace-01/syscall-top.stdout.log, sync-top.stdout.log и budgets/TRACE-INTERPRETATION.md"
[e-gates]: evidence-redacted.zip "Члены evidence/gates/receipt.json и stdout.log общих проверок"
[e-published]: evidence-redacted.zip "Редактированные evidence/published/receipt.json и published-race.stdout.log"
[e-post]: evidence-redacted.zip "Редактированные post-verify/receipt.json и post-verify/stdout.log"
[e-harness]: evidence-redacted.zip "Член review-forcekill/PUBLISHED-HARNESS-REVIEW.md"
[e-packager]: evidence-redacted.zip "Член review-forcekill/PACKAGER-REVIEW.md"
[e-manifest]: evidence-redacted.zip "manifest.json: source_sha256/source_bytes и saved_sha256/saved_bytes"
[e-policy]: evidence-redacted.zip "Члены prepare/receipt.json и prepare/powershell-policy.stderr.log"
[e-static]: evidence-redacted.zip "Члены forcekill/v2/manifest.json и forcekill/v2/candidate.diff"
[e-review]: evidence-redacted.zip "Член review-forcekill/REVIEW-v2.md"
[e-control-design]: evidence-redacted.zip "Члены forcekill/controls/v2/README.md, manifest.json и delayed-baseline/cleanup-only.diff"
[e-qualified]: evidence-redacted.zip "Член qualified-inputs/FINDINGS.md"
[e-job-review]: evidence-redacted.zip "Член qualified-inputs/JOB-RUN-REVIEW.md"
[e-serena]: evidence-redacted.zip "Члены qualified-inputs/serena-failures.json и qualified-inputs/FINDINGS.md"
[s-commit]: https://github.com/applicate2628/mcp-local-hub/commit/231c80e547816781ed2d77b34dd476f4400f8221
[s-candidate]: https://github.com/applicate2628/mcp-local-hub/blob/231c80e547816781ed2d77b34dd476f4400f8221/internal/cli/gui_force_test.go
[s-baseline]: https://github.com/applicate2628/mcp-local-hub/blob/f755d374fab317e1037deecd5dac8ffabbcdde36/internal/cli/gui_force_test.go
[s-api]: https://github.com/applicate2628/mcp-local-hub/blob/f755d374fab317e1037deecd5dac8ffabbcdde36/internal/api/attach_client_public_cli_receiving_windows_test.go
[s-job]: https://github.com/applicate2628/mcp-local-hub/blob/f755d374fab317e1037deecd5dac8ffabbcdde36/tools/test-job-entry/main_windows.go
[s-intent-test]: https://github.com/applicate2628/mcp-local-hub/blob/f755d374fab317e1037deecd5dac8ffabbcdde36/internal/api/daemon_intent_test.go#L107
[s-intent]: https://github.com/applicate2628/mcp-local-hub/blob/f755d374fab317e1037deecd5dac8ffabbcdde36/internal/api/daemon_intent.go#L835
[s-read]: https://github.com/applicate2628/mcp-local-hub/blob/f755d374fab317e1037deecd5dac8ffabbcdde36/internal/api/hub_mcp_state_read_inode_windows.go#L80
[s-parent]: https://github.com/applicate2628/mcp-local-hub/blob/f755d374fab317e1037deecd5dac8ffabbcdde36/internal/api/state_read_parent_fallback_reporter.go#L142
[s-hub]: https://github.com/applicate2628/mcp-local-hub/blob/f755d374fab317e1037deecd5dac8ffabbcdde36/internal/api/hub_mcp_log_observation.go#L458
[s-runv5]: https://github.com/applicate2628/mcp-local-hub/blob/f755d374fab317e1037deecd5dac8ffabbcdde36/internal/cli/upgrade_gui_qualified_fixture_windows_test.go#L27
[s-runbook]: https://github.com/applicate2628/mcp-local-hub/blob/f755d374fab317e1037deecd5dac8ffabbcdde36/docs/agent-runbook.md#L413
[s-serena]: https://github.com/applicate2628/mcp-local-hub/blob/f755d374fab317e1037deecd5dac8ffabbcdde36/internal/api/serena_build_environment_source.go#L71
[previous-evidence]: https://github.com/applicate2628/mcp-local-hub/blob/6a44cd8efd1b49ea6e938b7cc4796a0249d9d062/docs/modernization/verification/go-state-isolation-2026-11-20/evidence-redacted.zip

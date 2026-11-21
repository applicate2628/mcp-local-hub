# R21 — независимое ревью фактического архива свидетельств

Дата проверки: 20 ноября 2026 года, UTC; времена Windows-запусков ниже — 21 ноября, MSK (UTC+03:00).

**Вывод: блокирующих несоответствий в рассмотренном архиве не найдено.** Журналы поддерживают заявленные ограниченные результаты R21 для опубликованного коммита `ba17867125b3728fb734e90b383639a3ff9175ee`: выбранные тесты прошли, реальный create-if-absent прошёл на двух отключённых задачах текущего пользователя, четыре конфликта сохранили зарегистрированные определения, все три созданные за два запуска тестовые задачи впоследствии удалены с подтверждением отсутствия. Первоначальные unit-test и native отказы сохранены как отдельные FAIL.

Это независимое чтение фактических ZIP/JSON/JSONL-данных и сопоставление с ранее рассмотренными исходниками. Ревьюер не импортировал и не исполнял вложенные скрипты, не запускал тесты или продукт, не обращался к Windows, Task Scheduler либо GitHub. Целостность публичных сохранённых байтов проверена непосредственно; предел повторного вычисления хешей закрытых оригиналов указан в разделе 2.

## Содержание

1. [Архив и целостность](#integrity)
2. [Публичные байты и хеши оригиналов](#hash-scope)
3. [Исходники и опубликованный коммит](#source)
4. [Фактические тесты](#tests)
5. [Нативный отказ, повтор и очистка](#native)
6. [Конфиденциальность и пределы выводов](#limits)
7. [Источники и термины](#sources)

<a id="integrity"></a>
## 1. Архив и целостность

Рассмотрены реальные файлы в `<WORKSPACE>/r21-recovery/transferred` [1–4].

**Таблица 1 — независимо вычисленные идентификаторы.**

| Файл | Байты | SHA-256 |
|---|---:|---|
| `evidence-redacted.zip` | 2 852 497 | `053a27ad38d8957259079208d01fd5c3aad6056ec2490d2afdbc5584a1efb795` |
| `MANIFEST.json` | 1 592 554 | `927744c1bc7209816e01074c2619cf03750c6813d3167e8dcc3f6b150bc82df2` |
| `execution-summary.json` | 340 705 | `b223cbd74e031a0d553af737a3462ab84714f972fbeae08cb33d54760e3440be` |
| Base64, повторно вычисленный из полученного ZIP | 3 803 332 | `e015766e7047525f1c5f56824f963e74d6e1ba48f1ed9758302c986aff96193d` |

В архиве **4204 записи**: 4203 файла, перечисленные манифестом, и сам `MANIFEST.json`. CRC всех записей проверена; дублирующихся или выходящих за относительную область путей и ссылок не найдено. Все **4203 saved SHA-256 и размера** совпали с фактическими извлечёнными в память байтами. Суммарный несжатый объём — 7 754 817 байт. Внешние manifest/summary побайтово совпадают с одноимёнными записями ZIP; размеры и хеши соответствуют `archive-metadata.json`.

2531 запись отмечена как отредактированная для удаления частных значений. Проверка целостности охватывает и их сохранённые публичные байты. Base64-файл отдельно ревьюеру не передавался: его строка в таблице 1 получена заново из ZIP и совпала с transport metadata, что не подменяется заявлением о чтении исходного транспортного файла.

<a id="hash-scope"></a>
## 2. Публичные байты и хеши оригиналов

В шести основных фазах и двух сохранённых первоначальных фазах независимо сверены **45 command rows и 580 observation rows**: соответствие phase/command receipt, start/launch records, прямой process identifier, `settled=true`, `timed_out=false`, отсутствие ошибок захвата, а также связи с двумя потоками каждого процесса. Получено **1250 stream-origin links** — столько же, сколько заявлено в summary.

Из этих 1250 потоков **1247 исходных хешей и размеров вычисляются непосредственно из публичных байтов**. Для трёх отредактированных потоков проверены сохранённые публичные хеши и согласованность записанных original hashes между command receipt и манифестом; сами закрытые исходные байты не доступны:

- `evidence/control/clone.stderr.log`;
- `diagnostics/initial/evidence/control/clone.stderr.log`;
- `diagnostics/initial/evidence/candidate/current-task-recovery-race.stdout.log`.

Для четырёх дополнительных native фаз аналогично проверены **410 stream-origin links**: **407** original hashes вычислены непосредственно, а три stdout отредактированы: первоначальный native harness, read-only observer и final native harness. Их публичные JSON-события и полные saved hashes проверены; original hashes сверены как записи происхождения. Таким образом, «1250 проверенных связей» не означает «1250 закрытых оригиналов независимо перечитаны».

<a id="source"></a>
## 3. Исходники и опубликованный коммит

**Таблица 2 — закреплённые источники и публикация.**

| Объект | Идентификатор |
|---|---|
| Базовый коммит | `c55d9ca619f6f9301ba1113615370fc6218d608f` |
| Базовое дерево | `06a85c6efe53f5a4a8babbcadb7818752bb7acee` |
| Опубликованный коммит | `ba17867125b3728fb734e90b383639a3ff9175ee` |
| Опубликованное дерево | `1c803a8ef8227fe6c289be69c7897aee1549372b` |
| `source-manifest.json`, SHA-256 | `f3e12a0bed9f4024e4e00734eb35da859c23defc86c8b0963ac9469d80890534` |
| `product.patch`, SHA-256 | `13a064fecffb545388a9fdbaa054283f59d4b3cb5bc74c430b2b81c2c908e7e0` |
| `candidate.patch`, SHA-256 | `c0d88845076887b04d4a55da35c57dc6df49511f68575fad776f0a2dbfed4c2d` |
| Execution Runner, original SHA-256 | `6f69fc636f8ec47b725f07238f7ea2180cee9fb8efbd8e8a7275f4665ea87e61` |
| Checkout Runner, SHA-256 | `7dd36a8c3daaf38729bd39a47f5ee2feafca259561a79d58d9ec0c2461a35e98` |
| Final packer, original SHA-256 | `8daf6ac8f380e12aaff277ca1811365e80586e13bac55dbda0be14ebb7a25d97` |

Девять candidate SHA-256 в манифесте совпали с ранее рассмотренными локальными исходниками; независимо вычислены и соответствующие Git blob identifiers. Проверены все 13 имеющихся локальных копий девяти путей — расхождений нет. Product patch не изменён при исправлении двух `%q`-ожиданий теста.

Native receipts привязаны к тем же девяти SHA-256 на c55 с ровно ожидаемым набором изменений; снимки до/после совпадают. В опубликованной проверке и отдельной очистке: точный HEAD ba178, указанное дерево, единственный родитель c55, пустые index/worktree. Соответствующие сохранённые Git stdout для HEAD, tree, parent и changed-path set независимо сопоставлены с receipt и manifest. Это проверка фактических сохранённых данных, а не новое обращение к удалённой ветке.

<a id="tests"></a>
## 4. Фактические тесты

События Go JSON прочитаны из сохранённых stdout; terminal lists сверены с command receipts, полные множества имён с кратностью — с frozen manifest. Для отрицательного контроля проверены требуемые диагностические маркеры. Для всех восьми test streams нет SKIP. Числа тестов в таблице 3 включают parent/subtest events и повторы; package event указан отдельно.

**Таблица 3 — результаты без смешения первоначальной и окончательной попыток.**

| Выполнение | Test PASS | Test FAIL | Package | Exit | Вывод |
|---|---:|---:|---|---:|---|
| Первоначальный control | 0 | 15 | 1 FAIL | 1 | Ожидаемый отрицательный контроль |
| Первоначальный candidate/API | 26 | 16 | 1 FAIL | 1 | FAIL, сохранённая диагностика |
| Окончательный control | 0 | 15 | 1 FAIL | 1 | Ожидаемый отрицательный контроль |
| Окончательный candidate/API | 42 | 0 | 1 PASS | 0 | PASS |
| Окончательный candidate/scheduler | 42 | 0 | 1 PASS | 0 | PASS |
| Adjacent snapshot/pass-A/pass-B | 38 | 0 | 1 PASS | 0 | PASS |
| Published/API | 42 | 0 | 1 PASS | 0 | PASS |
| Published/scheduler | 42 | 0 | 1 PASS | 0 | PASS |

Положительные выбранные наборы исполнялись с `-race -count=2`; оба отрицательных control — с `-count=1`. `build ./...` и `vet ./...` завершились с exit 0. Полные тестовые наборы всего репозитория не заявлены.

В первоначальном candidate stdout ровно **10 сообщений RECOVERY-DIAGNOSTIC и 2 RECOVERY-DELETE**; иных RECOVERY failure markers нет. Сохранённый phase error — `Test command did not settle with the required exit`: frozen Runner отклоняет exit 1 раньше сравнения counts. Команда при этом фактически settled и не timed out. Исправление literal упаковщика не меняет исходный FAIL и не делает эту попытку допуском финального кандидата.

Control выполнен дважды — до и после минимальной поправки тестового ожидания. Окончательный candidate переиспользует завершённый второй control. Summary корректно содержит `control_executions=2`, `initial_attempt_control_and_candidate_retained=true`, `final_candidate_control_reused=true`.

<a id="native"></a>
## 5. Нативный отказ, повтор и очистка

Все durable JSONL сопоставлены с stdout до заключительного final. Имена проверены против точных manifest names, а native source/wrapper hashes — против ранее независимо рассмотренных локальных файлов [5–6].

**Таблица 4 — фактические нативные результаты.**

| Фаза | Записи stdout | Проверенный результат |
|---|---:|---|
| Первоначальный native harness | 5 событий + final | FAIL; U8 Create подтверждён, created=1/deleted=0; semantic verification отказал, cleanup не выполнен |
| Observer | 1 диагностический JSON | Read-only; ровно run_level=false из 18 field checks; остальные 17 true; full arguments 109 байт и SHA совпали; disabled=true |
| Final fresh native harness | 23 события + final | PASS; created=2/deleted=2; оба формата, четыре защищённых конфликта, два подтверждённых отсутствия |
| Отдельный retained cleanup | 5 событий + final | Только исходный U8; Delete вернул nil; затем ErrTaskNotFound; deleted=1, absence_confirmed=true |

В final transcript точные количества: preflight 2, invalid_bytes 2, create_attempt 2, create_result 2, export_observation 2, conflict_attempt 4, conflict 4, cleanup_attempt 2, cleanup_complete 2, complete 1. Для UTF-8 text и UTF-16LE with BOM проверены успешное создание, конфликт точного и lowercase имени, `already_exists=true`, отсутствие ошибки export и неизменность полного native XML hash при каждом конфликте. Перед каждым cleanup закреплён тот же полный hash; после — absence=true. Статус обеих созданных задач Disabled.

Исправление RunLevel затрагивает oracle harness: отсутствующий элемент допускается как документированный LeastPrivilege default; явное пустое или иное значение запрещено. SID в XML сохраняет строгое сравнение, а account name из Status.Owner разрешается в SID перед cleanup. Это проверено по исходнику и диагностике; продуктовые девять файлов не изменялись [5–7].

**Таблица 5 — ключевые native pins в фактических launch records.**

| Объект | SHA-256 |
|---|---|
| Первоначальный harness | `7691f5c760e0b024eb1d45c145d2f729efac9fd3e54daae29d3cc88fa4913c27` |
| Final fresh harness | `c8549b8b459927ff49dd283c8af27c33c4124c8b15694df316cd6ecde91a82d0` |
| Final native wrapper | `e56aba13445ea3099c328cf3a107de27c8a88de259140d63cee17a15412afd48` |
| Final native executable, записанный hash | `e1b2f61d6e5d2c3a13db5c34d4c081483ae624e68a1e2809e04581a70a2b06e2` |
| Отдельный cleanup Go | `e471a1ebbd12bc4210e6687ad2bc4fb660dea03fcea48ab107b652650b482554` |
| Cleanup wrapper | `b8d94e765b022344868128d57dd0f5600497d5b9bf4f28e58670e9fd59b1190f` |
| Cleanup executable, записанный hash | `54b3bada5f3a5707d00da3a507bfb1dec45c405d3c9d24ca90c0f7662313ca16` |

Executable hashes — записи проверенного запуска: бинарные файлы не передавались в публичном архиве и ревьюером заново не хешировались. Original harness/wrapper pins совпали с независимо хешированными локальными исходниками; formatting hashes и overlay/executable links согласованы с phase receipts.

Отдельная очистка связывает исходный manifest hash `266d268b…`, исходный successful-create events hash `d7ff7695…`, failed-native receipt hash `93a6333b…`, observer receipt hash `c6485bb1…` и полный observed export hash `5bbf1050…`. Полные значения сопоставлены, включая 1702 байта текущего export. До единственного Delete указаны disabled=true и owner_sid_matches=true. Подтверждение отсутствия записано в **02:40:44.514596 MSK**, завершение wrapper receipt — **02:40:45.385385 MSK**. Final native wrapper завершился в 02:37:05.168553 MSK.

Итого подтверждены три созданных fixture и три последующих проверенных удаления: исходный U8 и два свежих final-run имени. Исходный U16 не создавался. Первоначальный native receipt по-прежнему `completed=false/state=failed`; успешная очистка не переименовывает его в PASS.

<a id="limits"></a>
## 6. Конфиденциальность и пределы выводов

XML-файлов в архиве **нет**. Три private XML records сохранены только как пути, размеры и SHA-256: observed original export 1702 байта; final UTF-8 fixture export 3354 байта; final UTF-16 fixture export 3356 байт. Эти private hashes согласованы с observer/export/cleanup events; закрытые XML-байты ревьюер не читал. В дополнительных native данных не найдено XML-текста, включая проверенные Base64-значения с UTF-8/UTF-16 decoding; по всем публичным записям не найдено пользовательских SID вида S-1-5-21-… или неотредактированных Windows user-profile paths. Это предметная проверка, а не обещание обнаружения любой возможной секретной строки.

Архив позволяет независимо подтвердить сохранённые события и их связи, но не превращает журнал в повторное наблюдение живой системы. Нативная квалификация ограничена текущим пользователем InteractiveToken, двумя синтетическими отключёнными задачами без триггеров и fixture scope. Не квалифицированы другие учётные режимы, восстановление credentials/DACL, широкая атомарность install или реальная конкурирующая замена внешним процессом.

Cleanup использует проверку, затем отдельный Delete при явно принятом отсутствии внешних параллельных writers в эти точные имена. Атомарное compare-and-delete не заявляется. Ограничено ожидание непосредственного дочернего процесса; deadline/settlement всего дерева процессов не доказаны. Summary сохраняет эти пределы. `native_execution_outcome_verified_by_packer=false` также корректен: автоматический packer проверяет дополнительные материалы как данные; выводы раздела 5 выполнены отдельным независимым чтением actual transcripts.

<a id="sources"></a>
## 7. Источники и термины

1. [evidence-redacted.zip](evidence-redacted.zip): фактические receipts/streams, `source-manifest.json`, `publication-input.json`, исходная диагностика, native manifests/observations и launch pins. Внутренние пути приведены в тексте.
2. [MANIFEST.json](MANIFEST.json): граница original/saved bytes, hashes и private artifacts.
3. [execution-summary.json](execution-summary.json): сводка, независимо сопоставленная с потоками.
4. [archive-metadata.json](archive-metadata.json): transport/container hashes.
5. [NATIVE-FAILURE-REVIEW.md](NATIVE-FAILURE-REVIEW.md): ранее выполненное статическое ревью harness, observer и отдельного cleanup; actual-archive проверка настоящего документа расширяет прежние атрибутированные сообщения ведущего.
6. [REPORT.md](INDEPENDENT-REVIEW.md): API/backend contract, ограничения, source pins и первичная проверка исполнителей.
7. Microsoft, [MS-TSCH §3.2.5.4.2 SchRpcRegisterTask](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-tsch/849c131a-64e4-46ef-b015-9d4c599c5167): документированный default RunLevel; источник независимо прочитан при предшествующем обзоре.

**Термины.** SHA-256 — криптографическая контрольная сумма конкретных байтов; CRC — контрольная сумма целостности записи ZIP. JSONL — последовательность JSON-записей по одной на строку. SID — идентификатор учётной записи Windows; DACL — список разрешений доступа. BOM — метка порядка байтов; UTF-16LE означает младший байт первым. API — программный интерфейс; native здесь означает фактический Windows Task Scheduler, а не подставной тестовый объект. Fixture — изолированный тестовый объект. PASS/FAIL относятся к указанному выполнению, не ко всему проекту.

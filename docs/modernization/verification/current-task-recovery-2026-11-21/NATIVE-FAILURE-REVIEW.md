# R21: независимый разбор первого нативного отказа

Дата: 20 ноября 2026 года, UTC. Базовый коммит продукта: `c55d9ca619f6f9301ba1113615370fc6218d608f`. Рассмотренный frozen source manifest: `f3e12a0bed9f4024e4e00734eb35da859c23defc86c8b0963ac9469d80890534`.

**Диагноз установлен наблюдателем ведущего и согласован с первичной спецификацией: COM-экспорт опустил RunLevel, а harness ошибочно требовал буквальное наличие LeastPrivilege.** MS-TSCH задаёт LeastPrivilege при отсутствии RunLevel. Исправленный harness отличает отсутствие поля от явного пустого/другого значения; остальные существенные проверки сохраняются. Дополнительно Status.Owner представлен именем учётной записи, поэтому перед cleanup требуется разрешение этого имени в тот же SID. Продуктовые девять файлов не меняются. По сообщению ведущего, отдельная новая квалификация прошла: две свежие задачи созданы и после проверок удалены. После независимого ревью отдельной программы очистки также удалена первоначальная U8-задача, её отсутствие подтверждено. Исходный запуск остаётся FAIL; успешные новая квалификация и последующая очистка записаны отдельно.

Это статический обзор. Ревьюер не выполнял нативных операций, не запускал harness/observer, не обращался к GitHub и не менял продукт, тесты, harness или wrapper. Результаты фактического Windows-запуска ниже атрибутированы ведущему исполнителю. В этой работе используются только локальные исходники, хеширование, AST-проверка Python и первичная документация Microsoft.

## Содержание

1. [Установленные факты и точная граница отказа](#facts)
2. [Нормализация XML и необходимые инварианты](#normalization)
3. [Независимое ревью наблюдателя](#observer)
4. [Установленный диагноз и минимальное исправление](#correction)
5. [Условия точечной очистки сохранённой задачи](#cleanup)
6. [Статус проверки и источники](#sources)
7. [Термины](#terms)

<a id="facts"></a>
## 1. Установленные факты и точная граница отказа

Ведущий сообщил о первом запуске реального harness: `created_tasks=1`, `deleted_tasks=0`; CreateXMLIfAbsent для U8 вернул успех, затем получена ошибка `export changed the principal, logon type, action, arguments, or working directory`. Девять файлов продукта не менялись; финальные unit/build проверки до этого завершились. Эти runtime результаты ревьюер самостоятельно не воспроизводил.

Рассмотренный harness имеет SHA-256 `7691f5c760e0b024eb1d45c145d2f729efac9fd3e54daae29d3cc88fa4913c27`. Его исходники позволяют точно локализовать сообщение [1].

**Таблица 1 — что успел и чего ещё не успел подтвердить этот запуск.**

| Участок harness | Наблюдение или следствие из control flow |
|---|---|
| `158–166` | Успех Create учитывается до вывода результата; после него ошибка проверки не означает отсутствие задачи. |
| `168–173` | ExportXML вернул результат без ошибки; затем verifyDefinition вернул aggregate error. |
| `298–299` | Эта более ранняя проверка не выдала ошибку: Task/namespace, URI, marker, отсутствие XML-триггеров, один principal и один Exec прошли. |
| `302–303` | Не совпало хотя бы одно из восьми условий, перечисленных в таблице 2. Само сообщение не называет нужное поле. |
| `305–306`, `175–177` | Проверка disabled/hidden/demand-disabled settings внутри validator и наблюдение runtime Status ещё не выполнялись. |
| `179–188` | Сохранение native export и добавление в owned список расположены после validation; до них выполнение не дошло. |
| `205–208` | Cleanup не достигнут. Повторное создание или удаление из самого факта ошибки не следует. |

**Таблица 2 — восемь независимых сравнений в месте отказа.**

| Поле/связь | Ожидание исходного harness |
|---|---|
| Principal.UserId | Точный текущий SID, использованный при формировании XML |
| Principal.id | Литерал `Fixture` |
| Principal.LogonType | `InteractiveToken` |
| Principal.RunLevel | `LeastPrivilege` |
| Actions.Context | Совпадает с Principal.id |
| Exec.Command | Точный несуществующий action path в выделенной области D: |
| Exec.Arguments | Полная строка с Unicode, кавычками и обратными слешами |
| Exec.WorkingDirectory | Точный каталог первого native run |

Ведущий затем прочитал сохранённый файл задачи напрямую, без её изменения. По его сообщению, SID, literal ID/Context, logon/run level, command, working directory и settings соответствуют ожиданию; Unicode в arguments присутствует. Это ещё не устанавливает полное побайтовое равенство arguments и не подтверждает, что product ExportXML возвращает тот же Unicode-текст.

Предложенный `.ps1`-способ наблюдения был остановлен ExecutionPolicy до выполнения; ведущий сообщил, что политика не менялась и не обходилась. Дальнейший наблюдатель компилируется из Go и обращается к существующим product read methods. Этот обзор не трактует заблокированную попытку как состоявшееся чтение.

<a id="normalization"></a>
## 2. Нормализация XML и необходимые инварианты

Principal.id — идентификатор определения внутри XML; Actions.Context ссылается на него как IDREF. UserId задаёт отдельную security identity. Поэтому переименование непустого локального ID при одновременном сохранении ссылки Context теоретически может сохранять смысл определения. Документация подтверждает эту связь, но не доказывает, что Windows в данном запуске действительно переименовала ID [4–6].

В текущем случае такое переименование **не установлено**, а прямое чтение файла задачи, по сообщению ведущего, показывает прежний `Fixture`. Ослабление проверки ID сейчас было бы неподтверждённой гипотезой.

**Таблица 3 — допустимость будущей минимальной поправки зависит от наблюдения.**

| Фактически выявленное отличие | Возможная ограниченная корректировка | Что сохраняется обязательным |
|---|---|---|
| Только local ID label изменён; Context следует новому непустому ID | Сравнивать непустой ID и Context=ID вместо литерала `Fixture` | Один principal, тот же SID, logon/run level, action и settings |
| UserId или Status.Owner представлены именем учётной записи | Сначала read-only разрешить имя в SID и сравнить SID; не подменять identity строковым сходством | Точный исходный security principal и явное происхождение разрешения имени |
| Пропущено поле с документированным default | Сначала проверить default и фактическое нативное свойство | Семантика исходного principal/settings должна совпасть; пустая строка сама по себе не доказательство |
| Изменены Unicode, кавычки, backslashes или прочие байты arguments в product ExportXML | Не менять ожидаемую строку ради PASS; локализовать преобразование в product read/transport path | Полное значение arguments и исходные сохранённые байты |
| Изменены command, working directory, SID, logon type или run level без доказанной эквивалентности | Сохранить отказ и задачу; квалификация не пройдена | Никакого автоматического принятия другого определения |

Нормализацию XML сериализации следует отличать от изменения текста внутри параметров запуска. Внешние отступы или порядок атрибутов не равны изменению аргумента процесса. Для последующих conflict checks остаётся строгая проверка полного native export до/после: она проверяет сохранность уже зарегистрированного occupant, а не равенство native export исходной сериализации.

<a id="observer"></a>
## 3. Независимое ревью наблюдателя

Полностью прочитан Go observer [2], SHA-256 `2188d225edcc42d8fb8e82502f4e06bb504ebe04e07f7bef628f8b0e3121f6e7`, и Python wrapper [3], исходно переданный с SHA-256 `9b13c4c42b1884c2092214232e4d7e75019f1dbaf529fed7c875fc67b5d20ba2`.

В Go observer нет операций регистрации, удаления, запуска, остановки или enable. После scheduler.New выполняются ровно два метода для фиксированного U8 имени: ExportXML и Status. Аргументы командной строки не принимаются. Текущий SID сверяется с SID исходного manifest; исходный manifest и реконструированный ожидаемый input получают SHA-256.

Точные байты, возвращённые ExportXML, сохраняются в новом `diagnostics/native-observation/retained-task.xml` **до XML decoding и validation**. Файл содержит исходное представление UTF-8 COM text; декларация внутри него может говорить UTF-16. Он намеренно не перекодируется. Повторное использование каталога и перезапись файлов отклоняются; Write/Sync/Close учитываются existing helper.

Observer применяет прежний verifyDefinition и дополнительно раскладывает все его существенные предикаты на field booleans. Для arguments сохраняются полные lengths/SHA-256 и индекс первого отличающегося байта; «Unicode присутствует» не заменяет сравнение всей строки. SID и raw XML в stdout не печатаются. Status.Owner сравнивается с SID буквально и отдельно; эта проверка не маскируется как resolved identity.

Независимое сравнение локальных текстов подтвердило: fixture constants и весь suffix taskXML/verifyDefinition/writeNewSynced/escape/hash/errorText побайтово совпадают с исходным harness [1]. Следовательно, observer не устраняет отказ подменой ожидаемых данных или правил.

Python wrapper использует отдельную фазу Runner, фиксированный Go hash, exclusive formatted copy и виртуальный Go overlay; оригинальный harness не вызывается. Сборка offline, исходное дерево не дополняется реальными файлами. Native suppression меняется только на время observer-read и возвращается в finally. Прямой дочерний процесс имеет 120-секундный deadline; недоказанный запуск отмечается null/unresolved. У wrapper не найдено mutation/owner-scope blocker.

Перед выполнением ведущему рекомендовано повторить `admit(candidate)` после build и сравнить postbuild original observer hash с фиксированным HARNESS_SHA, а manifest — с первоначальным admitted hash. В исходно прочитанном wrapper postbuild hashes записывались без этих двух повторных admission comparisons, а окончательная проверка source выполнялась после чтения. Это ограниченная provenance корректировка; она не требует нового native действия.

`completed=true` в observer означает завершённое чтение и разбор. Он намеренно не означает, что все field_matches=true или исходная квалификация прошла: `original_verify_error` остаётся независимым диагностическим результатом. AST-проверка Python прошла без импорта/выполнения. Последующий результат observer передан ведущим и разобран в разделе 4.

<a id="correction"></a>
## 4. Установленный диагноз и минимальное исправление

Ведущий сообщил результат рассмотренного product-path observer: из 18 field predicates не совпал только run_level; остальные 17 совпали. В точном COM export элемент RunLevel отсутствует. SID совпал буквально; вся строка arguments — 109 байт — совпала по длине и SHA-256, а не только наличием Unicode. Триггеры отсутствуют, settings соответствуют ожиданию; Status показывает disabled. Status.Owner — шестибайтное имя учётной записи, поэтому его буквальное сравнение с SID также не является корректным сравнением identity.

Независимо вновь открыта первичная спецификация MS-TSCH SchRpcRegisterTask, §3.2.5.4.2: пункт о RunLevel требует использовать LeastPrivilege при отсутствии элемента [8]. Поэтому исходный harness имел дефект oracle: Go string объединял отсутствующий элемент и явную пустую строку. Ни изменение principal ID, ни повреждение Unicode/arguments этим наблюдением не подтверждены. Изменять продуктовый ExportXML ради обязательного включения native default не требуется.

Рассмотрен исправленный harness [10], SHA-256 `c8549b8b459927ff49dd283c8af27c33c4124c8b15694df316cd6ecde91a82d0`. Полная дельта к исходному варианту имеет четыре предметные части.

**Таблица 4 — проверенные изменения harness.**

| Изменение | Точное условие и предел |
|---|---|
| Новый run ID `3a9720bbbd994a5a815f6e093fb38d79` | Два свежих имени и новые файлы; первая retained U8-задача не затрагивается. |
| RunLevel представлен `*string` | nil допускается как документированный default; явные пустая строка, HighestAvailable и прочие значения отвергаются. |
| Status.Owner разрешается через `user.Lookup` | Cleanup требует `ownerErr==nil` и `owner.Uid==expectedSID`; XML UserId по-прежнему сравнивается с SID буквально. Согласно Go API, User.Uid на Windows является SID [9]. |
| Export-файл сохраняется до verifyDefinition | При будущем semantic failure остаётся native material. Owned entry по-прежнему создаётся лишь после успешной верификации и disabled Status. |

Полные сравнения SID, ID=`Fixture`, LogonType, Context linkage, command, arguments, working directory, settings и full native hash при conflicts/cleanup сохранены. RunLevel default принят только по подтверждённому случаю и спецификации; общий validator не ослаблен.

Рассмотрен final wrapper [11], SHA-256 `e56aba13445ea3099c328cf3a107de27c8a88de259140d63cee17a15412afd48`. Diff к ранее полностью проверенному wrapper содержит только новые source/hash/run ID, artifact/overlay/package paths и отдельную phase. Postbuild candidate admission, source/executable pins, учёт неопределённого запуска и точный transcript oracle сохранены. AST parse прошёл без исполнения. Оставшихся blocking findings для этой пары не найдено.

После ревью ведущий сообщил actual PASS нового запуска: точные 23 event rows плюс final, `created_tasks=2`, `deleted_tasks=2`, одинаковые девять product-source hashes до и после; wrapper завершился в 02:37:05 по московскому времени. Это отдельная квалификация с новыми именами. Ревьюер не запускал и не наблюдал её через удалённые инструменты, поэтому данный результат атрибутирован ведущему. Он не заменяет и не исправляет задним числом сохранённый первоначальный FAIL.

<a id="cleanup"></a>
## 5. Условия точечной очистки сохранённой задачи

Первоначальный cleanupOwned не является обработчиком этого незавершённого запуска: он требует две проверенные owned entries, тогда как U8 остановился до добавления первой, а U16 не был создан. Нельзя запускать исходную двухзадачную программу повторно ради очистки: её create/preflight sequence относится к новому пустому namespace, а output directory намеренно не переиспользуется.

Точечный cleanup можно обосновать после диагноза и отдельного ревью конкретного ограниченного действия при следующих условиях:

1. Исходные immutable receipts связывают точный U8 name, успешный Create result, run marker, текущий principal и рассмотренный source/executable; это не одна лишь проверка существования имени.
2. Observer сохранил полный текущий export; выявленное отличие объяснено, а SID, URI/marker, отсутствие триггеров, единственный ожидаемый action, command/arguments/workdir и disabled/demand-disabled settings подтверждены. Status также подтверждает отключённую задачу и ожидаемого владельца.
3. Непосредственно перед единственным Delete повторное чтение совпадает с принятым полным export hash, и остальные проверки сохраняются. Эта сумма закрепляет наблюдение текущего определения; она сама по себе не доказывает происхождение задачи.
4. Delete адресован только исходному точному U8 name. U16, lowercase aliases и prefix-based cleanup не входят в это действие. Предварительный durable receipt называет цель и согласованный hash.
5. При любой неопределённости, несовпадении или отказе дальнейших mutations нет. После успешного Delete отдельно наблюдается отсутствие; ошибка этого чтения не превращается в «cleanup complete».

Как и исходно согласованная bounded cleanup, это read-then-delete при явном отсутствии внешних параллельных writers в изолированное fixture name. Task Scheduler Delete не предоставляет expected-version/expected-hash аргумента; атомарность сравнения с удалением не заявляется. Сам данный статический обзор не выполняет cleanup и не подтверждает runtime-состояние задачи.

### 5.1. Проверенная программа очистки

Полностью прочитан `<WORKSPACE>/r21-recovery/native-retained-cleanup/main.go` [12]: 235 строк, 13 928 байт, SHA-256 `e471a1ebbd12bc4210e6687ad2bc4fb660dea03fcea48ab107b652650b482554`. Независимо пересчитаны размер и хеш. Блокирующих дефектов для отдельно разрешённой очистки одного собственного fixture при указанном предположении об отсутствии параллельных записей не найдено.

**Таблица 5 — условия и пределы очистки, проверенные по исходнику.**

| Место | Условие | Поведение и конкретный предел |
|---|---|---|
| `49–69` | Нет аргументов командной строки; новый `ownedevents.log` создан с exclusive-флагом | Цель нельзя подменить аргументом; intent записывается и синхронизируется до native calls. Ошибка записи прекращает выполнение. |
| `70–80`, `107–138` | Текущий SID, пять exact-size/SHA provenance-файлов, исходный manifest и ровно пять исходных событий | Проверяются исходный run ID, оба объявленных имени, disabled/hidden/no-trigger policy, hash исходного U8 input и однозначно успешный U8 `create_result` с пустым error и `outcome_uncertain=false`. Наличие задачи само по себе не даёт права на удаление. |
| `81–93` | ExportXML и Status только точного исходного U8 | Полные 1702 байта и SHA-256 должны совпасть с сохранённым observer export; semantic validator проверяет URI/marker, principal/SID, action/arguments/workdir и settings. Status должен быть Disabled; `user.Lookup(status.Owner).Uid` должен совпасть с текущим SID. |
| `94–99` | Все проверки прошли и `delete_attempt` успешно синхронизирован | Ровно один вызов `Delete(nameUTF8)`. При ошибке нет повторной mutation, а успешный return учитывается до возможного отказа записи результата. |
| `100–104` | Delete вернул успех | Отдельный ExportXML должен вернуть `ErrTaskNotFound`. Иной результат не считается подтверждённой очисткой. |
| `148–199` | XML содержит отсутствующий RunLevel | Только nil принимается как документированный default; явная пустая строка или другое значение запрещены. Полный pinned export hash остаётся дополнительным обязательным условием. |

Пять закреплённых исходных материалов, прочитанных программой, представлены в таблице 6. Их содержимое на Windows ревьюер самостоятельно не считывал; это проверенные литералы и проверки в рассмотренном исходнике. Ведущий подтвердил происхождение этих материалов в исходном запуске и observer.

**Таблица 6 — неизменяемые материалы, требуемые до удаления.**

| Материал | Байты | SHA-256 |
|---|---:|---|
| Первоначальный `manifest.json` | 1353 | `266d268b6e62c69918aaf8e167096a327fa50449ef1446d214876c592b9fb49b` |
| Первоначальный `observations.jsonl` | 915 | `d7ff7695beb9a674de0f977cccd401f086ac7069789d4aee9dcb79467e3925e2` |
| Receipt первоначальной неуспешной qualification | 113326 | `93a6333b7b5c89b970f819f5aa506f659e72c865cf22c5c65c3255b70f8bb487` |
| Receipt отдельного observer | 112742 | `c6485bb1479866ad6d7097946578a7fc93db1abf9f2be6273a100c2660ee588a` |
| Сохранённый полный `retained-task.xml` | 1702 | `5bbf10501bf82e822aa692bc99ac27017249e75fbee34ab59eafa4fb032231ce` |

Все task mutations ограничены строкой 96: `Delete(nameUTF8)`. В исходнике нет Create, ImportXML, Run, Stop или Enable. U16 используется только при проверке исходного manifest/preflight; lowercase aliases не адресуются. При неопределённом Delete result счётчик обозначает лишь число успешных return, а не число фактических изменений: поле `counter_basis` объясняет это в final. Такое выполнение остаётся неуспешным и не даёт разрешения автоматически повторять удаление.

### 5.2. Проверенная обёртка очистки

Полностью прочитан `<WORKSPACE>/r21-recovery/cleanup_run.py` [13]: 77 строк, 5127 байт, SHA-256 `b8d94e765b022344868128d57dd0f5600497d5b9bf4f28e58670e9fd59b1190f`. Размер и SHA независимо совпали; Python AST успешно разобран без импорта или исполнения. Также повторно проверены используемые части ранее рассмотренного Runner [14].

Wrapper фиксирует опубликованный коммит `ba17867125b3728fb734e90b383639a3ff9175ee`. `admit("published", COMMIT)` вызывается до сборки, после сборки и после cleanup; он требует чистый index/worktree, точные девять candidate hashes, единственного родителя c55 и точный changed-path set. Это не допущение, что локальные candidate-файлы уже равны опубликованному коду. Сам SHA опубликованного Git-объекта закрепляет его дерево; wrapper не допускает иной HEAD.

Для Go создаётся отдельная exclusive formatted copy; compilation использует виртуальный overlay без записи в source tree. Offline build и существующие инструменты/кеши наследуются из Runner; загрузок или установок нет. После сборки повторно сравниваются исходный Go hash и первоначальный source-manifest hash, затем закрепляются original/formatted Go, overlay, executable, wrapper и manifest. Override `MCPHUB_E2E_SCHEDULER` действует лишь на время native cleanup и восстанавливается в finally.

Ожидание непосредственного дочернего процесса ограничено 180 секундами. Унаследованная обработка timeout адресует сохранённый handle непосредственного процесса; остановка всех потомков не заявляется. Если запуск попытались выполнить, но не получено надёжного launch-файла, native-operation state остаётся null/unresolved, а не false.

Успех требует ровно пяти durable событий в порядке `cleanup_intent → provenance_verified → delete_attempt → delete_result → absence_observation` и отдельного final stdout. JSON stdout должен совпасть с durable log; stderr должен быть пуст. Все записи адресованы только точному U8 имени; отдельно проверяются полный export hash, disabled/owner predicates, успешный Delete return и отсутствие. Final требует `deleted_tasks=1`, `delete_attempted=true`, `absence_confirmed=true`, `completed=true`; после этого проверяются неизменность всех pins и повторный source admission. Неполный transcript, ошибка или несовпадение не превращаются в PASS.

### 5.3. Сообщённый результат отдельной очистки

После завершения независимого статического ревью ведущий сообщил об успешном фактическом выполнении этой отдельной программы: wrapper завершён, получены ровно пять событий плюс final, единственный Delete вернул nil, последующий ExportXML вернул `ErrTaskNotFound`; окончание — 02:40:44 по московскому времени. Published source admission после выполнения завершён, исходники не изменились. Это сообщение ведущего, а не результат собственного запуска ревьюером.

Таким образом, по совокупности двух запусков квалификации и отдельной очистки ведущий подтвердил создание трёх fixtures и последующее проверенное удаление всех трёх: первоначальный U8 и два новых final-run имени. Исходный U16 не создавался. Отчёт не переносит результат очистки обратно на неуспешную первоначальную qualification и не утверждает, что read-then-delete стал атомарной операцией.

<a id="sources"></a>
## 6. Статус проверки и источники

На текущем этапе: product patch не изменён; исходная native qualification остаётся неуспешной. Product-path observer ведущего локализовал ошибку harness в неучтённом default RunLevel; первичный контракт независимо проверен. Исправленный fresh harness, его wrapper и отдельные cleanup Go/wrapper прошли статическое ревью. Состояния разных запусков не объединяются (таблица 7).

**Таблица 7 — граница между статическим ревью и фактическими результатами ведущего.**

| Этап | Состояние | Основание |
|---|---|---|
| Первоначальная native qualification | FAIL; один успешный Create, ноль Delete | Сообщение ведущего; исходные материалы сохранены отдельно. |
| Product-path observer | Чтение завершено; локализовано отсутствующее RunLevel | Сообщение ведущего; исходник наблюдателя и правила проверки рассмотрены независимо. |
| Fresh final qualification | PASS; 23 event rows плюс final; два Create, два проверенных Delete | Сообщение ведущего; source9 до/после совпали. Ревьюер runtime не выполнял. |
| Публикация product9 | Коммит `ba17867125b3728fb734e90b383639a3ff9175ee`; parent c55; tree `1c803a8ef8227fe6c289be69c7897aee1549372b`; diff9, +786/−8 | Сообщение ведущего; в данной задаче ревьюер к GitHub не обращался. |
| Published повтор API/scheduler | PASS: 42 API + package и 42 scheduler + package; Go exit 0; source exact ba178 | Сообщение ведущего. |
| Отдельная очистка первоначального U8 | SUCCESS: пять событий плюс final; один успешный Delete, затем ErrTaskNotFound; завершение 02:40:44 MSK | Сообщение ведущего после статического ревью Go e471a1… и wrapper b8d94e…; published source неизменён. |

### 6.1. Уточнение проверки сохранённой ошибки в упаковщике

Ведущий сообщил, что первый фактический проход упаковщика остановился на несовпадении ожидаемого literal ошибки первоначального unit-test candidate. Это отдельная ошибка packaging oracle, а не новый отказ продукта или повтор native qualification. Старый упаковщик и его неуспешные материалы, по сообщению ведущего, сохранены.

Независимо просмотрен порядок в frozen Runner [14]: `check_test_oracle` на строках 194–195 сначала отвергает `exit_code != expected_exit_code`, выдавая `Test command did not settle with the required exit`; только после этого строки 196–200 проверяют terminal-event counts. Первоначальный кандидат имел actual exit 1 при expected exit 0, поэтому прежний literal `Unexpected test selection, skips or terminal-event counts` не соответствовал реально достигнутой ветви. Текст ошибки не означает, что процесс не завершился: фактические settled/timed_out проверяются отдельно.

Независимая byte-level проверка [15] установила ровно одну замену ожидаемого literal и две добавленные строки комментария. Обратная замена этих трёх строк точно восстановила прежний SHA-256 `ebb43aca4d8d899572ef3923b994abcf99aac6b23f6319c6f4b7eaa9f50ba5f5`; новый SHA-256 — `8daf6ac8f380e12aaff277ca1811365e80586e13bac55dbda0be14ebb7a25d97`. Обе версии прошли AST parse; ревьюер их не импортировал и не исполнял.

Условия `completed=false`, `state=failed`, `error_type=RuntimeError` сохранены. `command_files` продолжает требовать settled=true, timed_out=false, отсутствие ошибок capture и совпадение hashes исходных потоков. `test_oracle(qualifying=false)` по-прежнему сверяет фактические terminal events с raw JSON; первоначальные 26 PASS / 16 test FAIL плюс package FAIL остаются диагностикой, а `qualifies_final_candidate=false` не меняется. Для этой точной поправки блокирующих замечаний нет. Проверка фактического архива будет отдельным чтением готовых байтов, без запуска упаковщика или продукта ревьюером.

### 6.2. Применённые навыки и источники

Применены ранее полностью прочитанные навыки `go-project-and-api-design` с `references/api-evolution.md`, `review-go-engineering-change` с `references/finding-standard.md`, `go-data-consistency` с `references/isolation-and-ambiguity.md`, а также Ponytail `code-simplification`: точное происхождение результата, неопределённость внешней операции, минимальный контракт и отсутствие ненужного rollback framework.

1. [Исходный native harness](native-original.go), SHA-256 `7691f5c760e0b024eb1d45c145d2f729efac9fd3e54daae29d3cc88fa4913c27`.
2. [Read-only Go observer](native-observer.go), SHA-256 `2188d225edcc42d8fb8e82502f4e06bb504ebe04e07f7bef628f8b0e3121f6e7`.
3. [Observer wrapper](observer_run.py), первоначально рассмотренный SHA-256 `9b13c4c42b1884c2092214232e4d7e75019f1dbaf529fed7c875fc67b5d20ba2`.
4. Microsoft Learn, [Principal.Id](https://learn.microsoft.com/en-us/windows/win32/taskschd/principal-id), просмотрено 20.11.2026: local principal identifier и ActionCollection.Context.
5. Microsoft Learn, [actionsType](https://learn.microsoft.com/en-us/windows/win32/taskschd/taskschedulerschema-actionstype-complextype): Context как IDREF.
6. Microsoft Learn, [Task Scheduler Schema](https://learn.microsoft.com/windows/win32/taskschd/task-scheduler-schema): PrincipalKey и ContextKeyRef; [Principal.UserId](https://learn.microsoft.com/en-us/windows/win32/taskschd/principal-userid): security user identity.
7. [Ранее выполненное независимое ревью](INDEPENDENT-REVIEW.md): продуктовый контракт, frozen sources, ограничения native harness и wrappers. Настоящий документ фиксирует последующий фактический отказ и не переименовывает его в PASS.
8. Microsoft Learn, [MS-TSCH §3.2.5.4.2 SchRpcRegisterTask](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-tsch/849c131a-64e4-46ef-b015-9d4c599c5167), повторно независимо открыт 20.11.2026, пункт RunLevel: отсутствие элемента означает LeastPrivilege.
9. Go, [os/user.Lookup и User](https://pkg.go.dev/os/user), просмотрено 20.11.2026: Lookup разрешает имя, поле User.Uid на Windows содержит SID.
10. [Исправленный fresh harness](native-final.go), SHA-256 `c8549b8b459927ff49dd283c8af27c33c4124c8b15694df316cd6ecde91a82d0`.
11. [Final native wrapper](native_final_run.py), SHA-256 `e56aba13445ea3099c328cf3a107de27c8a88de259140d63cee17a15412afd48`.
12. [Отдельная программа очистки исходного U8](native-retained-cleanup.go), SHA-256 `e471a1ebbd12bc4210e6687ad2bc4fb660dea03fcea48ab107b652650b482554`.
13. [Обёртка отдельной очистки](cleanup_run.py), SHA-256 `b8d94e765b022344868128d57dd0f5600497d5b9bf4f28e58670e9fd59b1190f`.
14. [Общий qualification Runner](check_recovery.py), ранее полностью рассмотренный SHA-256 `6f69fc636f8ec47b725f07238f7ea2180cee9fb8efbd8e8a7275f4665ea87e61`; для данного cleanup повторно проверены begin, admit, run и finish.
15. [Упаковщик публичных свидетельств](pack_public.py), SHA-256 `8daf6ac8f380e12aaff277ca1811365e80586e13bac55dbda0be14ebb7a25d97`; здесь рассмотрены точная поправка initial_diagnostics, неизменные command_files/test_oracle и README упаковщика. Полное независимое ревью всего упаковщика этим утверждением не заявляется.

<a id="terms"></a>
## 7. Термины

- **XML — Extensible Markup Language:** формат определения задачи.
- **COM — Component Object Model:** компонентный интерфейс Windows, через который product ExportXML читает Task Scheduler.
- **SID — security identifier:** идентификатор учётной записи Windows; не то же самое, что XML Principal.id.
- **IDREF:** ссылка XML-атрибута на элемент с определённым идентификатором ID.
- **UTF-8 / UTF-16LE:** кодировки Unicode; LE означает младший байт первым.
- **SHA-256 — Secure Hash Algorithm, 256 бит:** хеш конкретных сохранённых байтов, не свидетельство происхождения объекта.
- **AST — abstract syntax tree:** синтаксическое дерево исходника, получаемое без исполнения программы.
- **Harness / observer / wrapper:** соответственно проверочная программа, программа только наблюдения и средство её сборки/запуска.
- **Unknown outcome:** ошибка не устанавливает, был ли внешний эффект применён.
- **Owned entry:** локальная запись о создании и последующей верификации fixture; одного имени для неё недостаточно.
- **PASS:** подтверждённое прохождение конкретной проверки, а не произвольный успешный exit диагностического наблюдателя.
- **MSK:** московское время, UTC+03:00; времена фактических запусков переданы ведущим.

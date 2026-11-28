# B: позднее физическое завершение исходного EXTERNAL owner

**Статус: модель Python и целевой Windows Go-тест прошли; реальный native late
receiving ещё не квалифицирован. Runtime отключён.** Кандидат принадлежит
существующему #664.
Это будущий executor. Старый full11, его неизвестные состояния, удержанные процессы,
вывод, Source и Cache не использовались и не переопределялись.

## Содержание

1. [Источник и дефект](#источник-и-дефект)
2. [Что изменено](#что-изменено)
3. [Границы закрытия](#границы-закрытия)
4. [Проверки](#проверки)
5. [Следующий шаг приёмки](#следующий-шаг-приёмки)
6. [Воспроизведение сохранённой проверки](#воспроизведение-сохранённой-проверки)

## Источник и дефект

Владелец изменения — existing [#664](https://github.com/applicate2628/mcp-local-hub/pull/664),
head `0c255490461fed0a3ee3d43acdd5216e2e064a53`. Историческая ветка Python использовала
`subprocess.run(timeout=...)`: её обработчик таймаута завершал direct controller,
а оригинальный `Popen` не попадал в registry, используемый поздним receiving.
Удержанный глобальной переменной Go `session` тоже не защищал оригинальные объекты:
`main` безусловно вызывал `os.Exit` после неполного результата.

**Уже выполненную работу переиспользуем.** Основа Python — native-qualified named-v2
`5246b97e6cfcc60c39284fab6941509c1e770ec4738aec0a4b218cccb9d1e5a8`.
Его оригинальные `Popen`, файлы, одноразовое закрытие и отказ от `kill` сохранены.
Его семь существующих native fixture результатов относятся только к исходной
named-v2: receipt прямо указывает synthetic callback, отсутствие late physical
receiving и отсутствие Go qualification. Новые изменения эти результаты не наследуют.

Adapter взят побайтно из actual `final-gates-v14-root`:
`ef393445772f2efbf977d76719528af6706b1e1e394bfd3e0c2e3f1dd60ba450`.
Его actual core `qualified_windows_products_consumer_v1.py` также получен и проверен:
`b679aadd8455cb70c8a445e3abf99484edaf76208fffc0f8347100d12c5976c8`.
Go owner/controller и исходный DATA receiver взяты из восьми exact input-файлов,
доставленных root из разрешённого helper-каталога `final-gates-v8`. Их оригиналы
остаются в соседней `external-owner-inputs` без изменений.

Для outer используется существующий `ContainedStreamReceiptV1.ResourcesFinalized()`
из [#671](https://github.com/applicate2628/mcp-local-hub/pull/671),
head `ff8128a5f717fae75b263de55c382933a0bc9121`.
Windows-проверка использовала композицию P02
`b6f82b3f531bb3eec197049167cdc5971b44e2b0`, содержащую этот метод. Новый product
API, reflection fallback или дополнительный процессный менеджер не добавлены.

## Что изменено

| Участок | Минимальное изменение | Исходная власть над ресурсом |
|---|---|---|
| `guard-custody.py` | Callback физического получения записывается до запуска; поздний путь вызывает его после возврата того же `Popen.wait(0)` | Сам retained `Popen`, исходная строка command, исходные открытые файлы named-v2 |
| `literal_go_exec_adapter_ready_v1.py` | Canonical callback привязывает argv, cwd и копию исходного plan до `raw_command` | Замыкание находится в том же custody; PID используется только как дополнительное сопоставление |
| `controller/main_windows.go` | Барьер перед `os.Exit` при неизвестном результате | Тот же глобально удержанный `session` |
| `controller/cases_windows.go` | Сохраняется полученный `nativeReturn` | Тот же `c.Done`, без второго `Owner.Run` |
| `owner/.../physical_windows_amd64.go` | `ReceiveFailedPhysical` получает поздние исходные завершения | `nativeReturned`, `rootWaitReturned`, `stdout.done`, `stderr.done`, оригинальные handle-объекты |
| `controller/physical_windows.go` | Отдельная публикация физического закрытия после original case/outer receiving | `s.outerDone`, оригинальный receipt, исходные borrowed sinks |
| `canonical_receiving_v4.py` | Новый DATA-only `receive_failed` | Проверяет данные; не создаёт ownership и не открывает файлы |

Поздний путь читает только два control metadata-файла: исходный `result.json` и,
если обычное завершение не доказано, новый `failed-physical-closure.json`.
Первый файл не перезаписывается. Callback хранит собственные реальные read handles;
первая ошибка чтения/закрытия удерживается, не переоткрывается и не повторяется.
Вывод, исходники, Cache и replay этот путь не читает и не хеширует.

Normal canonical receiving использует **тот же prebound one-shot reader**, что и
late receiving. Ошибка закрытия normal reader не разрешает позднее открыть новый
Reader и объявить CLOSED. Metadata SHA в normal tail вычисляется из тех же успешно
прочитанных и закрытых исходных bytes; второго чтения `result.json` нет.

Это исправляет конкретный дефект независимого ревью: прежний normal reader мог
отказать при закрытии, после чего late receiver открывал новый reader и проверял
только его. Общая опека хранит первую ошибку исходного reader и оставляет UNKNOWN.
[Ревью и отрицательная регрессия](reviews/independent-review.json) отдельно
фиксируют условие, последствие и исправление.

Существующий `OriginalInvocationIssuer.retain_failed_invocations()` уже вызывает
унаследованный `OriginalRun.retain_failed_full_workers()`. В exact core этот метод
сам вызывает `self.receive_failed_worker_closure(now + 30)` и ждёт только CLOSED
при неизменных original states и запретах. Поэтому финализатор не переписан:
новый callback получает управление через уже существующий hook. После закрытия
core публикует отдельный `failed-original-physical-closure.json`; исходный receipt
остаётся FAIL. Хешируется только его исходная control metadata.

`REVIEW_COMPLETE` у adapter установлен в `False`: сборка ещё требует согласованной
композиции, новых путей и точных SHA. Старые runtime pins не заменены предположениями.

## Границы закрытия

Физическое закрытие — отдельный факт от успешной квалификации. Сохраняются original
FAIL, `Complete`, `Unknown`, `NativeComplete`, `IOComplete`, `CompleteReceivedAt`,
`Failure` и исходные сроки. После позднего закрытия команда остаётся неуспешной,
`commands_quiescent()` не становится true, новый запуск и capture access запрещены.

Ошибка чтения, отсутствие EOF или overflow не делают уже вернувшийся worker и
успешно закрытые оригинальные handles живыми. Late receiver может подтвердить такой
физический факт, но никогда не разрешает использовать его вывод для qualification.

Неполная debug ledger, missing event, pending Continue, чужой Owner, отсутствующий
оригинальный root wait либо неуспешное одноразовое закрытие означают **HOLD**.
Новый event loop, повторный `Cmd.Wait`, поиск PID, получение replacement handle или
завершение процесса для имитации cleanup отсутствуют.

**Frozen outer остаётся ограничением.** `ResourcesFinalized()` проверяет original
source-owned receipt, но этот receipt уже является снимком. Если он вернул false,
публичный caller не имеет исходных внутренних `stdoutDone/stderrDone/waitDone`
для позднего обновления. Повторное чтение того же receipt не создаёт новые факты.
Кандидат сохраняет controller и его оригинальные объекты в HOLD; полной поддержки
этой ветки не заявляет.

Новый callback привязан только к canonical `suite-owner`, исходник которого
получен и проверен. Для остальных named/deferred owner сохраняется удержание
named-v2; после отказа без собственного prebound physical receiver они остаются
HOLD. Общая квалификация всех EXTERNAL вариантов этим кандидатом не заявляется.

## Проверки

Проверено локально без дочерних OS-процессов и без Windows API:

- Различающий guard model: named-v2 сохраняет оригиналы, но late closure остаётся
  UNKNOWN; candidate получает CLOSED на тех же объектах. У candidate 15 инвариантов
  PASS, original FAIL и запреты неизменны. Отрицательный результат baseline ожидаем.
- DATA/adapter tests: девять тестов, включая 23 отказа для подмены binding/PID/сроков,
  выдачи запрещённых разрешений, pending worker/event и неуспешных original closes.
- Получение после исходного cleanup deadline разрешает только physical fact.
  Original result до/после сравнивается, его содержимое не меняется.
- При ошибке закрытия metadata reader исходный объект удерживается, close/open
  повторно не вызываются. Callback действительно передаётся до возможного timeout.
- Ошибка закрытия Reader именно в actual normal `receive_completion` остаётся
  UNKNOWN при двух поздних попытках actual guard: один open, один close, original
  capture files сохранены, чтение нового result/physical файла не выполняется.
- Связка проверена через actual issuer и точный метод existing core, извлечённый
  через AST без импорта native/cache setup. Fake Popen, actual candidate guard и
  callback завершают existing finalizer на тех же объектах; original receipt
  побайтно неизменен. Синтетические DATA-факты не выдаются за native qualification.
- Known-no-create допускает приобретённый и закрытый pin с ещё неполным image
  evidence, а также thread ID без приобретённого handle; частичные close flags
  не принимаются.
- AST обычного command path, close helper, capture finalizer, quiescence и deferred
  finalizer совпадает с 5246. Весь исходный `canonical_receiving_v4.py` сохранён
  как точный байтовый префикс.

Первоначальные девять Python tests использовали actual v14 core с SHA-256
`b679aadd8455cb70c8a445e3abf99484edaf76208fffc0f8347100d12c5976c8`;
их [исходный PASS-лог](checks/final-python-tests.log) сохранён. Для воспроизведения
модель теперь читает уже опубликованный v13 core с полным SHA-256
`e11118b22a6d805fd9984c885fb646e29638777b46b2227c788864c8d6d7953d` и проверяет AST
метода `Run.retain_failed_full_workers`:
`1d32f9a1b4488234e6eff022998334aecf407b0e5a3acbf333fe98f95a2aa8cc`.
AST и текст именно этого метода совпадают у обоих core; равенство целых файлов не
заявляется. [Provenance](reviews/core-method-provenance.json) связывает обе версии.
После изменения только fixture path, full-file pin и AST assertion выполнен один
новый прогон: [9 tests PASS](checks/public-core-python-tests.log). Runtime-кандидат
этой правкой не менялся; большой core и его посторонняя дельта не дублируются.

### Windows Go: компиляция и OS-pipe/controller unit

На exact P02 выполнены четыре исходные команды: проверка HEAD, `gofmt` десяти
Go-файлов, целевой `go test` и `git diff --exit-code`. Все четыре завершились с
exit 0; получены исходные native process/tree/handle завершения и присоединены
все восемь readers. Принудительного завершения Job и превышения cleanup deadline
не было. Родитель получил exit 0 через 32.5679192 s; его original controller и
capture handles закрыты. [Квитанция](native-go-v1/receipt.json) и
[родительский приём](native-go-v1/root-receiving.json) сохранены отдельно.

`TestFailedPhysicalOuterOriginalReturnAndFrozenIO` дал **1 root + 2 subtests +
1 package PASS, 0 FAIL/SKIP**. Он использует реальные `os.Pipe` и проверяет
original outer pending, returned worker с read error/без EOF и frozen incomplete
worker. Повторный poll не повторяет закрытие, исходный failure/deadline record
остаётся неизменным. Значения native receipt создаёт fixture; настоящий debug
owner, `Owner.Run` и end-to-end late receiving здесь **не запускались**.
Race detector и отдельный `go build` не выполнялись; компиляция получена в составе
`go test`. [Полные Go-события](native-go-v1/failed-owner-controller-test.stdout.txt)
и [разбор области](native-go-v1/receiving-review.json) не расширяют этот результат
до общей нативной квалификации.

После `gofmt` изменился только `controller/physical_windows.go`. В candidate
сохранён placeholder пространства имён; SHA-256 файла после форматирования —
`2b36397be3d45ebf0bb378eb89ba8a8086ff8d17b4c76d58b474f78fb5d63429`.
Замена только `__CANONICAL_EXTERNAL_STAGE__` на исходное имя stage восстанавливает
все десять SHA из квитанции целевого теста. [Точная карта](native-go-v1/source-binding.json)
различает candidate bytes и staged bytes.

Исходная квитанция Windows имеет CRLF, 36 604 байта и SHA-256
`8b8c8a5172331b8b258c7765ef4d52f8c9cbfb8ad68512e8ee474c74ea480216`.
Локальный импорт был LF; обратная замена только окончаний строк восстановила этот
точный hash. В публикуемой проекции `native-go-v1/receipt.json` сохранены CRLF.
Два непустых capture-файла получены от root; шесть пустых воспроизводятся из
закрытых исходных квитанций с длиной 0 и SHA-256 пустых bytes.

Сделан собственный source/diff review и независимый review Go/Python payload.
В найденной цепочке normal → late metadata receiving исправление перечитано;
дополнительных подтверждённых дефектов в просмотренном scope не установлено.
Это не заменяет native late-owner qualification. Прежний model-only manifest
сохранён в [reviews/pre-native-go-v1](reviews/pre-native-go-v1/candidate-manifest.json).

## Следующий шаг приёмки

1. Root принимает точный кандидат, source binding и сохранённый scoped Go-result.
   Путь публикации результатов — существующий #664.
2. Квалифицировать новый связанный native путь на свежих исходных объектах: поздний
   original return, late reader, read failure после закрытия, неизвестный frozen
   outer, одноразовый close refusal. При HOLD сохранять те же объекты без kill.
3. Только после приёмки fresh composition обновлять exact paths/pins и разрешать
   новый full campaign. Исторический full11 от этого не получает новый verdict.

## Воспроизведение сохранённой проверки

[REPRODUCE.md](REPRODUCE.md) описывает materialization двух model tests и проверку
уже существующего public core без импорта его runtime. Тринадцать candidate
источников публикуются под `sources/candidate` с добавленным `.txt`; штатный
`go test ./...` не включает эти документальные копии. Тесты и baseline также
сохраняются как `.txt` и копируются в отдельную временную область только для
явного воспроизведения модели.

Никакой новый runtime framework, активирующий config, DLL, бинарный owner или
право читать старые UNKNOWN scopes этим пакетом не выдаётся. Значение
`REVIEW_COMPLETE=False` у adapter и `adapter_runtime_enabled=false` сохранены.

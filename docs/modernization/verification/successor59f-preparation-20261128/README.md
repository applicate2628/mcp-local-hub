# Подготовка Source59f: закрытые producer v1/v2 и точные runtime-входы

## Содержание

1. [Область и результат](#1-область-и-результат)
2. [Source и сохранённые изменения](#2-source-и-сохранённые-изменения)
3. [Первый producer: FAIL/CLOSED](#3-первый-producer-failclosed)
4. [Диагностика и отдельное продолжение Product](#4-диагностика-и-отдельное-продолжение-product)
5. [Что собрано и что подготовлено](#5-что-собрано-и-что-подготовлено)
6. [Исходное владение процессами](#6-исходное-владение-процессами)
7. [Воспроизведение точных байтов](#7-воспроизведение-точных-байтов)
8. [Состав и границы доказательств](#8-состав-и-границы-доказательств)
9. [Связанные опубликованные источники](#9-связанные-опубликованные-источники)

## 1. Область и результат

Этот пакет сохраняет две **закрытые подготовительные фазы** на Source `59f618ebfbf6d9f52c6994d68586e3c2ebc202a8`. Первая завершилась **FAIL/CLOSED**, вторая — **PASS/CLOSED**. Слово CLOSED означает получение завершения исходного владельца процесса и его потоков ввода-вывода по исходным receipts; PASS второй фазы относится к подготовке двух Product. **Результата полного acceptance в этом пакете нет.**

Здесь находятся точные исходники вспомогательных программ, использованные планы, карта Source, исходные результаты и captures, причина отказа первого producer, узкое исправление и неизменяемые runtime-входы. Записи `NOT_RUN` и `PREPARED_NOT_EXECUTED` в сохранённых документах относятся к моменту их создания. Они не заменяют результат более позднего полного прогона.

**Таблица 1. Две отдельные подготовительные фазы.**

| Фаза | Исходный результат | Команды | Время исходного parent, с | Область |
|---|---|---:|---:|---|
| helper-and-product-v1 | FAIL/CLOSED, exit 1 | 10 из 12 | 221.0126576 | Первые девять команд прошли; десятая отказала до вызова Product builder |
| product-only-v2 | PASS/CLOSED, exit 0 | 4 из 4 | 465.2672403 | Новый независимый export и два Product; helper/parser повторно не запускались |

Машиночитаемые основания: [evidence.json](evidence.json), [native-run-ledger.json](native-run-ledger.json), [исходное receiving первого producer](templates/receipts/producer-v1/root-receiving.json.template) и [исходное receiving Product-only](templates/receipts/product-v2/root-receiving.json.template). Полные execution receipts сохранены, включая все карты входов до и после команд; краткий ledger не подменяет их.

## 2. Source и сохранённые изменения

Точный commit: `59f618ebfbf6d9f52c6994d68586e3c2ebc202a8`; tree: `03e8488a44e67f1ea7e8dc2ef0ece53fee5ae8e1`; parent: `74e7563db244ede5984834233ce8d59072e92c0a`. Карта содержит **3369 tracked paths, 69 340 536 исходных байтов**. Сохраняются [карта фактических SHA-256](templates/source/source59f-input-sha256.json.template) и [полная карта Git mode/blob](sources/source/source59f-git-map.json); реальные входы каждого producer связаны с этими данными и повторно измерены вокруг команд.

Цепочка test-only изменений начинается после восстановленного `33eaf8af8fc46cb231f48c70f4775922393245b1`: семь ранее опубликованных test fixes дали Source04e, два литерала migration manifest — Source74e, затем проверка ошибки исходного `Process.Wait` — Source59f. Последняя дельта и точный afterimage включены здесь: [original-breakaway-wait.patch](sources/source/wait-fix/original-breakaway-wait.patch) и [исправленный тест](sources/source/wait-fix/afterimages/internal/cli/supervisor_spawn_breakaway_windows_test.go.txt).

Семь предыдущих test fixes и два manifest literals уже сохранены в отдельных опубликованных капсулах, ссылки приведены в §9. Отдельный raw Git commit object для 59f здесь не экспортирован. Его идентичность подтверждает закрытая native операция commit-v10 в отдельном publication trail. Эта капсула сохраняет точные file maps и финальный patch; она не выдаёт их за новый экспорт commit object.

Ни исторический Source727d, ни Source4, ни отдельные A–D/public heads не получают квалификацию от этой подготовки. Source59f имеет собственные входы, commit/tree и будущий полный результат.

## 3. Первый producer: FAIL/CLOSED

Планировался cache copy, staging, шесть helper builds, пять локальных тестов парсера, marked Product, native Product и итоговый сбор compile DATA. Фактически исполнены первые десять команд.

**Таблица 2. Первые девять успешных команд и исходный отказ.**

| № | Команда | Exit | Физическое завершение |
|---:|---|---:|---|
| 1 | copy-closed-warm-caches | 0 | CLOSED |
| 2 | stage-exact-source-and-helpers | 0 | CLOSED |
| 3–8 | validator, test2json, job, contained, namespace, CLI helper builds | 0 | CLOSED для каждой |
| 9 | check-foreground-framed-parser | 0 | CLOSED |
| 10 | build-marked-product | 1 | CLOSED |

Штатный Product builder не был вызван десятой командой: отказ произошёл раньше, в проверке разрешённого исполняемого файла Go внутри wrapper. Команды native Product и итоговый сбор compile DATA не исполнялись. Первоначальный общий FAIL сохранён; его нельзя заменить на PASS из-за успешных первых девяти команд.

После получения полного CLOSED root отдельно получил шесть helper outputs как **COMPILED_INPUT_DATA_ONLY**: [compiled-helpers receiving](templates/receipts/producer-v1/compiled-helpers.json.template). Это допускает их использование как конкретных скомпилированных входов, но не присваивает первому producer общий успех или результат полного тестирования. Его кэш не использован для продолжения.

Полные [execution.json](templates/receipts/producer-v1/execution.json.template) и [parent.json](templates/receipts/producer-v1/parent.json.template) имеют исходные SHA-256 `76d74ba0d51f044ff4601efece213eeb2d345ec63a1b31539b20231f0492a19b` и `e900a860923c9b122436d06feffc2a5e815214d98d56a6982d54af009f846735`. Root receiving проверил 3398 фактических входов, 20 карт до/после команд и 20 command captures. Ещё два parent captures пусты и также сохранены.

## 4. Диагностика и отдельное продолжение Product

[Metadata-only диагностика Get-Command](templates/diagnosis/command-resolution.json.template) показала два результата `Get-Command go -CommandType Application`. Свойство `.Source` стало массивом, который строковое преобразование склеило в одну строку. Строгое сравнение этой строки с единственным pinned Go path отказало. Обычный resolver при этом выбирал именно ожидаемое первое приложение. Для Git был один результат.

Диагностика запрашивала только метаданные PowerShell; Go, Git и Product не запускались. Сохранён [исходный probe](templates/diagnosis/probe_command_resolution.ps1.template). В новом wrapper изменён один блок: обычное разрешение имени, требование ровно одного результата типа Application, затем прежнее точное сравнение пути. [Полный узкий diff](diagnosis/product-wrapper-v1-to-v2.patch) и оба исходника сохраняются по первоначальным хешам. Политики запуска и native guard не изменялись.

Отдельный Product-only v2 выполнил четыре команды: копирование исходного PASS/CLOSED warm donor в свежие кэши; независимый export всех 3369 Source файлов; marked Product; native Product. Новый export и output roots изолированы. Уже созданный приватный native parent принят только после исходного CLOSED и проверки прежних file identity, ACL и отсутствия bundle; ACL не переписывался.

Кэш-донор остаётся прежним закрытым warm-cache на Source04e. Его байты используются как compile DATA: они не передают тестовую квалификацию Source59f. Исходный failed-producer cache не используется. Сохраняются [receipt копирования](templates/receipts/product-v2/cache_copy.json.template) и [receipt Source export](templates/receipts/product-v2/source_export.json.template).

Полный [execution v2](templates/receipts/product-v2/execution.json.template) имеет SHA-256 `a5992a80253c4e3f14de1f1ccecf37d8c24b897652b78a5c56598027ba054c12`. Root receiving проверил 3397 входов, восемь карт до/после, восемь command captures, все 3369 export files и метаданные обоих Product. Parent duration составила 465.2672403 с; deadline не превышен.

## 5. Что собрано и что подготовлено

[Binary provenance](binary-provenance.json) содержит размер и SHA-256 каждого из шести helper executables, двух трёхфайловых Product bundles и compiler resource. Сами executables, COFF resource и кэши не публикуются как source. Для каждого Product сохранены actual build plan, fixture, entry observation, preparation scope и текстовая identity JSON.

Marked Product создан штатным `build.ps1` из точного Source59f export и полного Publisher snapshot. Его source epoch — `861852f807c36673755014ef4657bf10252b4e75505bc8b73819bcf8cded7a3a`. Отдельный native Product создан через поддержанный tagged test build без Publisher source epoch. Эти две роли не переименовываются друг в друга.

Точные runtime templates, их diffs, reviewed manifest и подготовленные runtime files включены отдельно. [Фактический runtime manifest](templates/runtime-prepared/runtime-manifest.json.template) имеет SHA-256 `bb532e4fec38ea60cad8a105ed2fad7495ee76440f9546a5100802f604fd0f7d`; [фактическая конфигурация](templates/inputs/final-bindings/literal-successor-bindings.json.template) — `53c5e8d351c70a184a851b7da4170e0bafdccfdedc262fe931030ffaf83f65d2`.

Composer выполнил только DATA-подстановку. [Его receipt](templates/runtime-prepared/preparation.json.template) имеет состояние PREPARED_NOT_EXECUTED. Отдельных parent/execution receipts native теста для этой операции нет, и они не выдуманы. Подготовленные исходники не импортировались и не исполнялись при упаковке.

Будущий полный acceptance использует исходные команды без sharding, селекторов и увеличения Go timeout:

```text
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

В [evidence.json](evidence.json) сохранены точные 14 migration roots, два canonical controls, три исходных API stderr roots и три дополнительных CLI stderr roots. В namespace также обязательны три исходных namespace tests и peer transport test. Наличие имён в конфигурации не означает их выполнение или PASS. Из четырнадцати migration roots тринадцать проходят реальную production preparation; TestMigrateSerena_CallsInstallParsedManifest_NotDiskWrite использует существующую inert test seam, которая изолирует проверку переданного manifest. Новые mock не вводились; пакет не добавляет пропуски, неподдержанный UV cache import или частную Source727d схему.

Пять успешно выполненных unit tests относятся только к auxiliary parser: unframed spoof, корректная frame после prompt и one-byte chunks, длинный обычный вывод/слишком большая frame, повторный PASS, FAIL/SKIP требуемого root. Их [точные тесты](sources/inputs/foreground/main_windows_test.go.txt) и [исходный stdout](sources/captures/producer-v1/check-foreground-framed-parser.stdout.txt) сохранены. Они не являются полным API/CLI прогоном.

## 6. Исходное владение процессами

Каждая из двух подготовительных фаз сохраняет original outer process object, Wait/Dispose outcome, original command owner bindings, native settlement и завершение исходных reader threads. Полные records содержат исходные PID, Job/native fields, terminal exit, hashes captures и карты входов. Ненулевой functional exit и известное физическое завершение учитываются отдельно: поэтому первый результат остаётся FAIL/CLOSED.

Будущий foreground CLI owner использует поддержанный Source `RunForeground` и разрешённый BreakawayOK. Его scope — original root, original Job members и исходный ввод-вывод, плюс точные существующие test-owned cleanup assertions для escaped actors. Это не универсальное доказательство закрытия произвольного escaped tree. Для вложенного GoProcess сохраняется фактическое успешное `Cmd.Wait` и путь release по тестовым assertions; отдельного receipt успешного CloseHandle каждого вложенного процесса не заявляется.

Owner разбирает только реальные frame boundaries `0x16`, удерживает четыре набора исходных ordinary path/image handles и на UNKNOWN сохраняет исходный custody. Финальная v4 добавляет один defer удержания custody при unwind; [точный v3→v4 diff](sources/inputs/foreground/v3-to-v4.patch) сохранён. Receiver не принимает UNKNOWN как завершение и не передаёт Source/cache authority по отсутствующему cleanup proof.

API namespace запускает точную копию входящего Go image в защищённой роли и проверяет исходный image hash отдельно от execution copy. Штатный Source helper остаётся точным; tagged attach получает отдельный actual native Product. Роль marked CLI image не навязывается API.

## 7. Воспроизведение точных байтов

[source-bindings.json](source-bindings.json) описывает **133 оригинала, 21 400 213 байтов**: исходные относительные роли, размер, SHA-256, опубликованный файл и каждую прозрачную подстановку. Из них 59 сохранены exact bytes, 74 — как transparent templates. Двадцать четыре пустых capture originals ссылаются на один пустой payload; их отдельные оригинальные роли и receipts сохранены.

Пути конкретного компьютера и личный SID владельца не включены в public leaf. Вместо них стоят видимые `@@RECOVERY_...@@` markers. Допустимы только literal и double-backslashes transformations; нет кодирования, скрытых defaults или извлечения значений из окружающей среды. Native IDs и остальные факты receipts сохранены. Значения ролей передаются отдельно оператором, который имеет право на исходные данные.

Используется прежний [reconstruct_sources.py](reconstruct_sources.py). Единственное изменение его кода — pin нового bindings-файла. Имена legacy schema сохранены для совместимости формата; они не присваивают этому пакету историческую Source727d квалификацию. Поле `source_file_count` в этом формате считает все сохранённые оригиналы, включая receipts.

Пример запуска DATA-восстановления из разрешённой рабочей области:

```text
python CAPSULE/reconstruct_sources.py --roles-json operator-roles.json --output reconstructed-preparation
```

Здесь CAPSULE обозначает каталог данного пакета; output должен отсутствовать, а его parent — уже существовать. JSON оператора должен содержать ровно роли из `required_roles`. Renderer сначала проверяет все опубликованные payloads, число markers, исходные размеры и SHA-256 для каждого восстановленного файла. Только затем создаёт новый output и пишет файлы через exclusive create. Он не импортирует сохранённые scripts, не запускает Go/Git/модели, не обращается в сеть, не меняет gate и не переносит никакую квалификацию.

[data-reconstruction-proof.json](data-reconstruction-proof.json) сохраняет историческое независимое восстановление всех 133 оригиналов в памяти для первоначального bindings SHA-256 `f6291141e747d821f22f174a0b4db38e808fb6bd9b8ed53eabee7f87f5e56090` в commit `747cc17a966b0a0cbcdfbe4607e392c771009ba4`. Этот receipt не изменён и не изображает новый запуск renderer.

Три опубликованных исходника с расширением `.go` теперь хранятся с суффиксом `.go.txt`, чтобы обычный `go test ./...` не считал доказательные afterimages исходниками пакетов. В `source-bindings.json` изменились только три `published_path` и соответствующий pin renderer; все 133 исходных роли, размеры, SHA-256, substitutions и bytes payloads сохранены. Renderer по-прежнему восстанавливает исходные имена `.go` только в явно указанном новом DATA-каталоге. Hash manifest проверяет текущий publication layout; `.gitattributes` с `* -text` сохраняет окончания строк.

## 8. Состав и границы доказательств

Полные 32 capture originals — 20 command и два parent для v1, восемь command и два parent для v2 — сохранены после получения исходного CLOSED. При этом рассматриваются только закрытые фазы подготовки. Captures активного/последующего полного acceptance при создании этого пакета не читались и сюда не включены.

Пакет не содержит native binaries/cache payloads, credentials, private operator role values или raw Source59f commit object. Последний отдельно связан с закрытым publication trail. Сохраняется exact Wait patch/afterimage, но ни Source worktree, ни pub checkout/report, ни исходные планы/receipts при упаковке не изменялись.

Отдельный исторический Source727d full FAIL, остальные прежние результаты и публикационные receipts остаются в своих капсулах. Этот пакет их не заменяет. Оставшийся исторический expected-refusal check не считается выполненным на основании DWORD refusal, parser tests или успешной Product-подготовки.

## 9. Связанные опубликованные источники

[existing-public-inputs.json](existing-public-inputs.json) содержит точные immutable URLs и исходные SHA-256 unchanged guard, Windows Job producer, command runner, private-scope/launcher templates, contained controller и original Job owner source.

Предыдущие Source test changes:

1. [Семь test fixes после Source33](https://github.com/applicate2628/mcp-local-hub/blob/05fbffa30f99914c8174966525a70bc9984e7038/docs/modernization/verification/source33-successor-test-fixes-20261128/README.md).
2. [Два manifest literals после Source04e](https://github.com/applicate2628/mcp-local-hub/blob/05fbffa30f99914c8174966525a70bc9984e7038/docs/modernization/verification/successor14-manifest-pin-20261128/README.md).

Отдельный `successor-publication-trail-20261128` сохраняет 11 прежних операций stage/commit/push/warm-cache, 213 originals и 148 captures. Эти данные здесь не дублируются; их публикацией управляет root независимо. Ссылки служат provenance и не доказывают новый полный PASS.

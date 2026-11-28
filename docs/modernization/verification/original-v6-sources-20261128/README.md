# Сохранение исходников подготовки original v6/v7

Пакет сохраняет **22 точные версии исходных файлов**, которые отсутствовали как отдельные Git blobs в проверенных деревьях пяти существующих PR. Общий размер оригиналов — **422939 байт**. Здесь 12 файлов сохранены побайтно; у 10 опубликованы прозрачные templates с 18 явно описанными заменами значений путей. Ещё пять нужных исходников уже доступны публично в точных байтах и перечислены через immutable links [2].

Это сохранение кода и DATA-реконструкции. Оно не заявляет полного PASS, не запускает сохранённые исходники и не изменяет исторические FAIL/UNKNOWN.

## Содержание

1. [Состав](#inventory)
2. [Замены и точное восстановление](#reconstruction)
3. [Публичные основания и уже сохранённые входы](#public-bases)
4. [Проверки и границы результата](#verification)
5. [Источники](#sources)

<a id="inventory"></a>
## 1. Состав

**Таблица 1. Независимые группы сохраняемых исходников.**

| Группа | Новых точных версий | Назначение |
|---|---:|---|
| V6 templates и preparer | 8 | Композиция literal route, namespace/phase receiving, migration input receiving и запуск через исходного владельца |
| Private Product drivers, DATA copiers и v7 probe | 8 | Подготовка private inputs, копирование завершённых кешей и immutable artifacts, получение graph, чтение JSON date |
| External preparer и привязанный private scope | 2 | Два новых qualified scopes и сборка исходных ext17 helpers |
| Отсутствующие точные ext17 Go inputs | 4 | Contained receiver, controller cases/main и owner events |

Точный исходный `templates-manifest.json` сохранён отдельно как [11], включая его собственный `qualification=NOT_RUN` и provenance. Это дополнительный metadata artifact, не одна из 22 source revisions.

Девятый файл набора v6 templates — `canonical_receiving_v4.py` — уже побайтно опубликован. Вместе с тремя owner-файлами и `controller/inputs_windows.go` он находится в [2]. Эти пять файлов здесь не дублируются.

**Таблица 2. Все 22 сохранённые версии. Полные SHA-256 и Git blob identifiers находятся в [1]; короткие значения ниже предназначены для сопоставления строк.**

| Путь относительно inputs role | Публичная форма | Байты оригинала | SHA-256 оригинала |
|---|---|---:|---|
| `build_private_inputs_v6.ps1` | template | 15785 | `ef594d10af88…` |
| `build_private_inputs_v7.ps1` | template | 15802 | `f4abd18efc76…` |
| `probe_private_date_v7.ps1` | точные байты | 2145 | `4a3ead5ca5a2…` |
| `copy_closed_caches_v6.py` | template | 9433 | `2577deae6370…` |
| `copy_original_artifacts_v6.py` | template | 6880 | `fd95917c2dd4…` |
| `invoke_fixed_build_utf8.ps1` | template | 6294 | `4cb35eb80e02…` |
| `launch_literal_v6.py.in` | точные байты | 7711 | `adf9f26a647a…` |
| `literal_go_exec_adapter_v6.py.in` | точные байты | 77605 | `6ee83887864e…` |
| `literal_migration_receiving_v6.py.in` | точные байты | 35572 | `6a95ae02fe24…` |
| `prepare_literal_v6.py` | template | 12329 | `9c3cd5b7aae8…` |
| `private_migration_scope_v6.py` | template | 12532 | `a56e6730bc39…` |
| `private_run_scope.py.in` | точные байты | 12458 | `ce16701c0c89…` |
| `qualified_windows_products_consumer_v6.py.in` | точные байты | 36122 | `716e2e0aeb7e…` |
| `receive_private_graph_v6.py` | точные байты | 6899 | `1d6895719d79…` |
| `run_literal_go_gate_v6.py.in` | точные байты | 43670 | `a1aecc2d2db8…` |
| `run_partitioned_go_gate_v6.py.in` | точные байты | 9851 | `9b81714df735…` |
| `external-prepare-candidate/prepare_external_controllers_v6.py` | template | 11572 | `867c1924449f…` |
| `external-prepare-candidate/private_external_scope_v6.py` | template | 12601 | `0903fe1b0539…` |
| `external-source-original/literal_contained_test_receiver_v4.go` | template | 17383 | `59fa2cc432b8…` |
| `external-source-original/controller/cases_windows.go` | точные байты | 19075 | `9b1af2364631…` |
| `external-source-original/controller/main_windows.go` | точные байты | 23646 | `c48b5b339d7e…` |
| `external-source-original/owner/canonicalcaseowner/events_windows_amd64.go` | точные байты | 27574 | `3b3137d2bdc6…` |

Имена `.in` в точных копиях сохраняют исходный смысл: это входные templates прикладного preparer. Они могут содержать свои исходные `@@…@@` markers. Новые публикационные markers имеют отдельный префикс `@@RECOVERY_` и описаны в [1].

Исходные `build_private_inputs_v6.ps1` и `build_private_inputs_v7.ps1` различаются ровно двумя строками: `#Requires -Version 7.0` становится `7.5`, а чтение bindings через `ConvertFrom-Json -AsHashtable` получает `-DateKind String`. Обе версии и отдельный probe сохранены. Source-preservation пакет не пересматривает FAIL v6 и не приписывает runtime-результат самому факту наличия v7 source.

<a id="reconstruction"></a>
## 2. Замены и точное восстановление

**Таблица 3. Полный набор публикационных замен.**

| Marker | Явный operator input | Преобразование | Вхождения |
|---|---|---|---:|
| `@@RECOVERY_WORK_AREA_RAW@@` | `approved_work_area` | UTF-8 значение без изменения | 15 |
| `@@RECOVERY_WORK_AREA_ESCAPED@@` | `approved_work_area` | Каждая обратная косая черта удваивается для исходного Python string literal | 2 |
| `@@RECOVERY_WINDOWS_POWERSHELL_HOME@@` | `windows_powershell_home` | UTF-8 значение без изменения | 1 |

Первая роль — исходная одобренная рабочая область оператора. Вторая — исходный каталог штатного Windows PowerShell, использованный loader. Значения не включены ни открытым текстом, ни в закодированном виде, ни как defaults или примеры. Их должен явно предоставить оператор, у которого есть исходный контекст. Это позволяет восстановить исторические байты; произвольная другая рабочая область не считается тем же оригиналом и не пройдёт исходный SHA-256.

Templates меняют только эти 18 вхождений. Все остальные байты, включая проверки путей, budgets, lifecycle predicates, исходные hashes и обычные test fixtures, сохранены. Фрагменты регулярных выражений для даты не являются путями и не редактировались. После чтения реальных строк это уточнило предварительную оценку количества templates до фактических **12 exact + 10 templates**.

[3] принимает operator-owned JSON с ровно двумя string keys — `approved_work_area` и `windows_powershell_home` — и новый отсутствующий output directory. Файл с role values должен храниться отдельно от публичного пакета.

```text
python reconstruct_sources.py --roles-json <PRIVATE_ROLES_JSON> --output <NEW_APPROVED_OUTPUT_DIRECTORY>
```

Порядок работы renderer:

1. Проверить встроенный SHA-256 `source-bindings.json` и hashes всех 22 опубликованных payload files.
2. Проверить точное число каждого marker и применить только заявленные преобразования.
3. Сопоставить размеры и SHA-256 всех 22 восстановленных оригиналов **до создания output directory**.
4. Создать исключительно новый каталог, записать исходники с сохранением их role-relative paths и повторно проверить записанные байты.
5. Сохранить DATA receipt без исходных значений ролей.

Renderer не импортирует сохранённые модули, не запускает PowerShell/Go/тесты, не обращается в сеть, не запускает Git и не меняет gate или approvals. Он не перезаписывает существующий output и не удаляет частичные результаты при ошибке. Пять точных публичных inputs из [2] получаются отдельно по закреплённым ссылкам и сверяются с их hashes.

<a id="public-bases"></a>
## 3. Публичные основания и уже сохранённые входы

Сопоставление проверено по полным GitHub tree listings без усечения для PR #631, #637, #664, #665 и #666. Для #664 использован snapshot `da5e86fd9e8a12731553f6646867808034d705bb`; остальные exact commits приведены в [2]. Сравнение выполнено по Git blob identity реальных локальных bytes. Отсутствие 22 versions относится к этим пяти snapshots, а не ко всей истории GitHub.

Уже точные публичные входы:

- `canonical_receiving_v4.py`: SHA-256 `6ad13e7c57e604111552fd553f10a66a174c9ba9b96dc37484dfba071a9bf2a0`.
- `controller/inputs_windows.go`: `345197b965bd59a97c43e846ab35a37393852cbac7dfe5cabc0f0a109576e6cc`.
- `owner/canonicalcaseowner/capture_windows_amd64.go`: `e734df342f5b863ffe9916fa71d9c7712b5c305346662419bccc7716517e0053`.
- `owner/canonicalcaseowner/native_windows_amd64.go`: `8729ac9c525e664c5d7f279b277a93b9e06841e6757a14de508719d026335f87`.
- `owner/canonicalcaseowner/owner_windows_amd64.go`: `7276febe485aea1791fc6757ad1c710a108e3c60e7aa263e656ad985dfe22294`.

Публичные full-v13 sources [5] и failed-owner candidate [6] содержат основания и родственные версии ряда v6 modules. Они не равны новым скомпонованным `.in` файлам побайтно. Капсула сохраняет именно текущие версии и не подставляет более новый failed-owner producer вместо исходного ext17.

Исходный Product Source3ebe уже воспроизводим через публичный source capsule [7]. Исходный Source727d — через отдельную public delta capsule [8]. Текущие controller `windows_job.py`, `guard-custody.py` и переносимый runner сохранены ранее в [9]. Эти большие или уже сохранённые основания не копируются повторно.

<a id="verification"></a>
## 4. Проверки и границы результата

[4] фиксирует побайтную DATA-проверку: все 22 входа прочитаны и совпали с исходными размерами/SHA-256; после 18 замен и обратной подстановки operator values в памяти восстановлены все **422939 исходных байт**, расхождений нет. Файл renderer при подготовке не запускался и не импортировался. Его алгоритм и границы записи прочитаны статически.

Новый пакет записан отдельно от текущих inputs и публикационной рабочей копии. Исходные frozen files, evidence v1/v2, текущий report, Git index, refs и runtime не менялись. Во время подготовки не выполнялись сохранённые sources, компиляция, тесты, модели или публикация. Уникальные входы здесь сохраняются как проверяемый код и DATA.

Этот пакет не включает текущие execution plans, runtime bindings и сырые captures; они имеют отдельную evidence/publication область. Новая final bindings candidate и последующие итоговые receipts требуют собственного сохранения после принятия их владельцем. Полная приёмка Source727d, отдельная приёмка exact33eaf, 14+2 checks, stderr scenarios, ожидаемый оставшийся отказ и cache handoff учитываются по своим исходным receipts. Ни один из этих результатов не выводится из успешной DATA-реконструкции.

<a id="sources"></a>
## 5. Источники

[1] [source-bindings.json](source-bindings.json) — точные 22 original/published pins, пути и все replacement counts.

[2] [existing-public-inputs.json](existing-public-inputs.json) — пять уже публичных точных inputs и все пять snapshot commits.

[3] [reconstruct_sources.py](reconstruct_sources.py) — отдельный DATA renderer с обязательными operator role values.

[4] [data-reconstruction-proof.json](data-reconstruction-proof.json) — результаты выполненного in-memory DATA-сопоставления без source execution.

[5] [Публичные full-v13 sources](https://github.com/applicate2628/mcp-local-hub/tree/da5e86fd9e8a12731553f6646867808034d705bb/docs/modernization/verification/cache-owner-custody-20261126/sources/full-v13).

[6] [Публичные failed-owner candidate sources](https://github.com/applicate2628/mcp-local-hub/tree/da5e86fd9e8a12731553f6646867808034d705bb/docs/modernization/verification/failed-owner-receiving-20261126/sources/candidate).

[7] [Source3ebe capsule в PR #631](https://github.com/applicate2628/mcp-local-hub/tree/c80fd6b4e6f9806eb7d94726a979e9e89d46ec75/experiments/rust-process-owner/ffi/verification/stdio-consumer-2026-11-26/source-capsule-3ebe).

[8] [Source727d reconstruction capsule](https://github.com/applicate2628/mcp-local-hub/tree/da5e86fd9e8a12731553f6646867808034d705bb/docs/modernization/verification/original-source727d-recovery-20261128).

[9] [Ранее сохранённые controller sources](https://github.com/applicate2628/mcp-local-hub/tree/da5e86fd9e8a12731553f6646867808034d705bb/docs/modernization/verification/cache-acceptance-recovery-20261128/sources).

[10] [artifact-manifest.json](artifact-manifest.json) — byte lengths и SHA-256 всех остальных файлов пакета; `.gitattributes` сохраняет эти байты в Git.

[11] [Исходный templates-manifest.json](metadata/templates-manifest.json) — точные 3691 байт, SHA-256 `573b788670b38cc6675fa1e9fc100cda95385049cc9d744363bca1cc87caa89b`.

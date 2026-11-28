# Восстановление Source4 и контура полной приёмки

**Source4 сохранён как точный текстовый патч и исходный Git commit. Полная приёмка остаётся открытой.** Здесь зафиксированы ранее закрытый адресный результат **24 CLI + 4 API PASS**, новый ожидаемый отказ запуска **FAIL / CLOSED_PRESTART_FAIL** и неизменённые исторические FAIL/UNKNOWN. Исходные 717 CLI-сценариев и два отсутствующих commit этим пакетом не восстановлены.

## Содержание

1. [Точный Source и область изменения](#1-точный-source-и-область-изменения)
2. [Воспроизведение Source4](#2-воспроизведение-source4)
3. [Полученные результаты](#3-полученные-результаты)
4. [Отказы и неизвестные состояния](#4-отказы-и-неизвестные-состояния)
5. [Остаток приёмки и публикация](#5-остаток-приёмки-и-публикация)
6. [Источники и проверяемость](#6-источники-и-проверяемость)

## 1. Точный Source и область изменения

**Таблица 1. Идентичность исходников.**

| Состояние | Commit | Tree |
|---|---|---|
| Уже опубликованная восстановительная основа e66 | `e66f5c6fab7da2a2eff16ff1f0891991ca1a839c` | `4df69e99dca3f808b26e6a5bf7721bdd0ba3a91c` |
| Source4, единственный parent e66 | `4ac73379a10e09766fe8274a129129a2b565f757` | `5b24c5f32541316bb8d24ad75a306c250b14b0b3` |
| База публикации этого пакета, #664 | `078a70fd412e0b6c6f5be210154825addfed1736` | `629a159252b1f45a7924609f2e94428f1c30dbcc` |

Source4 содержит 3867 tracked-файлов. Пять изменений относительно e66 относятся только к тестам: canonical helper dispatch и его отрицательным проверкам; двум положительным required-secret fixtures; неиспользуемому пути сохранения F1; отмене и присоединению исходного EventLoop в F6/F2. Производственный код этой дельтой не изменён. Полные before/after SHA-256 и after Git blob ID находятся в [source4-manifest.json](source4-manifest.json). [1]

Патч [source4.patch](source4.patch) сохранён без изменения байтов: **11152 байта**, SHA-256 `f39a10a395675c93068ff8f19a98de1ce856b616de3fc911e28d6b4a2e88ef89`. [source4.commit](source4.commit) сохраняет исходный commit object; его Git object ID должен совпасть с Source4 в таблице 1. Повторное чтение surviving checkout подтвердило commit/tree, пустой status и успешный `git apply --reverse --check --whitespace=error`. Это проверка сохранённого Source, не новый тестовый прогон. [1]

Source4 — база этой отдельной линии восстановления. Более новые композиции Source6d/Source52 и пара Product `b8d44b8cdfa2f57d17d80a2a59f3f7b6a6ec4647` / Test `a1798d121cec86cfe825db0fe4f29f840f4c2563`, включая P2-подготовку другой линии A–D, в Source4 не входят. Их опубликованные исходники и результаты сохранены в текущем #664. Результаты одной линии не переносятся на другую; Source4 не объявляется актуальной композицией всех открытых PR.

Пакет хранит воспроизводимые доказательства. Патч не применяется к code-файлам публикационной рабочей копии. Отдельные изменения A–D текущего #664 сохраняются; в частности, там уже имеется собственное исправление F6/F2. Последующее обновление #665/#666 должно переносить только отсутствующую дельту своего владельца, сохраняя более строгие текущие fixtures.

## 2. Воспроизведение Source4

Сначала восстановите e66 по [существующей инструкции и капсуле P02](../composed-functional-source-20261126/README.md#4-восстановление-source). Неизвестная старая история до P02 остаётся неизвестной; этот пакет не добавляет отсутствующих предков.

Следующие команды используют отдельную восстановительную Git database и новый временный index. `snapshot_repo`, `capsule_dir` и `temporary_index` задаются оператором как абсолютные пути к этой database, данному каталогу и новому собственному временному index. Рабочие исходники исходного прогона не меняются.

```sh
export GIT_INDEX_FILE="$temporary_index"
git -C "$snapshot_repo" read-tree 4df69e99dca3f808b26e6a5bf7721bdd0ba3a91c
git -C "$snapshot_repo" apply --cached --whitespace=error "$capsule_dir/source4.patch"
git -C "$snapshot_repo" write-tree
git -C "$snapshot_repo" hash-object -w -t commit --stdin < "$capsule_dir/source4.commit"
unset GIT_INDEX_FILE
```

Каждая команда должна завершиться успешно. `write-tree` должен выдать `5b24c5f32541316bb8d24ad75a306c250b14b0b3`, а `hash-object` — `4ac73379a10e09766fe8274a129129a2b565f757`. Проверьте пять postimage по manifest. Эти команды описывают воспроизведение; публикационный пакет не приписывает им нового фактического исполнения. Принятое исходное получение Source и нынешняя обратная проверка патча указаны отдельно.

## 3. Полученные результаты

**Таблица 2. Адресный Source4 и новая проверка отказа.**

| Запуск | Квалификация | Физическое завершение | Граница вывода |
|---|---|---|---|
| `focused-v1`, CLI | 24 root PASS, 0 FAIL/SKIP | Получены исходные native owners/readers | Все 10 выбранных F1 roots и 14 F6/F2 roots |
| `focused-v1`, API | 4 root PASS, 0 FAIL/SKIP | Получены исходные native owners/readers | Два helper classifiers и два положительных required-secret/readiness roots |
| `pwsh-highbit-refusal-v2` | **FAIL / CLOSED_PRESTART_FAIL**; ожидаемый отказ принят | Parent exit 1; ресурсы закрыты | Native process creation отказал, ребёнок не создан и не исполнялся |

В `focused-v1` выполнены четыре команды: два inventory и два адресных test image. Все четыре завершились с exit 0; native handles закрыты, исходные readers присоединены, наблюдаемые входы не изменились. Parent занял 4.411061099963263 с. Исходная execution-квитанция: `bd0c2a5c7bb8098a8935959f7086fb512c4cf5943cb40df1164b6878b1987b49`. В [evidence.json](evidence.json) приведены все 28 корневых исходов и hashes исходных закрытых журналов. Проверка не является полным CLI/API и не заменяет обязательные команды. [2]

В новой версии `inputs-error-dword-v4` native producer сохраняет unsigned DWORD — беззнаковое 32-битное значение `GetLastError`; guard привязан к исправленным байтам. Runner не изменён. На прежнем отказе создания процесса теперь получены исходный parent exit 1 и `physical_complete=true`: `created=false`, `command_executed=false`, child exit отсутствует, captures `NOT_CREATED`, readers `NOT_STARTED`. Наблюдаемое значение native error — `3221684229`. Это подтверждает корректно полученный ожидаемый отказ, а не успешный запуск PowerShell или полную квалификацию runtime. [3]

Точные pins:

| Вход | SHA-256 |
|---|---|
| Исправленный `windows_job.py` | `d94583001a33ef34adb67feec34b4f4b0bf3626df9f63da3c7a7c85bf3e0b552` |
| Перепривязанный `guard-custody.py` | `e7c663f4ba2fd558005bc110173609cb19465ff33d3f4678aeb53b5ba00e0924` |
| Исходный исполненный `run_owned_commands.py` | `6d16652d4893273b7e9d584920892ce5f5917622e7e242a62eb78ec824bcfb93` |
| Parent ожидаемого отказа | `3005b897df93cc7c70ca1c917683ed937971bb96229faa894e7e1bf85a6946c0` |
| Execution ожидаемого отказа | `cd7c448f1c3cf337e13e4ad77001c6692627e639624f70ede09cd8a9b08d1562` |

Новый отдельный `helper-observation-v2` получил **PASS** в трёх расположениях одного test image: original, npm-layout и direct-layout. Исходный лимит каждого запуска — 10 с; native exit 0, исходный процесс получен, оба канала закрыты, отсутствующий canonical parent не создан. Измеренные total intervals — 0.15087080001831055, 1.7474114999640733 и 1.1322292999830097 с соответственно. Source не изменялся. Это наблюдение не заменяет полный canonical-root набор: прежний `controller-finish-v3` остаётся FAIL. Parent `4d22c7e6a6f93049a4d3703077629a9efdf7448b81740c8e7d21de84975f8a8f`; execution `2794ee7ca14c7d51fa5db256daee3e45463a2bd398f16ba29453dbc455d1a75b`. Наблюдения перечитаны после закрытия; hashes parent/execution переданы исходным владельцем. [2]

Отдельный более ранний `runtime-and-helper-observation-v1` сохраняет **FAIL**: runtime inventory достиг исходного срока 180 с; original parent и потоки закрыты. Его первая команда receiver-data-refusals завершилась exit 0 за 0.067 с, но не превращает весь batch в PASS. Execution `e10d4991f2a634ddb26510a34b3b5bba768e69fb4da33dc2ab73966132cf608d`; здесь записана закрытая квитанция по сообщению исходного владельца. Его FAIL сохранён независимо от следующей новой попытки. [2]

Новый `runtime-readback-v2` отдельно получил **PASS**: все **1276 файлов / 52 каталога / 310287418 байт** совпали с сохранённым manifest; native probe вернул **PowerShell 7.7.0 / .NET 11.0.0**. Обе команды exit 0, original owners/readers закрыты, входы неизменны; parent — 162.3973291000584 с. Fresh readback использовал 600 с на полное чтение файлов, с опорой на ранее измеренные 410/264 с namespace/copy. Предыдущий отказ по сроку 180 с сохраняется; сроки обеих Go-команд и helper 10 с не менялись. Hashes parent/execution/inventory приведены в `evidence.json`. Это приёмка сохранённых runtime-байтов и version probe, без Product/full-suite вывода. [2]

### Сохранённый исполнительный код

В [sources](sources) сохранены пять точных исходников `inputs-error-dword-v4`: native owner, guard, DATA-only receiver check и оба observer. Runner опубликован отдельно как [переносимый шаблон](sources/run_owned_commands.py.template), а не как точные исполненные байты. Размеры и SHA-256 каждого публичного файла, а также исходный SHA-256 исполненного runner, разделены в `artifact-manifest.json` и `evidence.json`. Controller и observer не импортировались и не исполнялись при подготовке публикации; проверялось только восстановление данных.

В шаблоне изменены ровно три содержимых строковых literals: один путь в raw string заменён на `__APPROVED_WORK_AREA_RAW__`, два представления того же пути в обычных строках — на `__APPROVED_WORK_AREA_ESCAPED__`. Остальные байты runner сохранены. Исходный машинный путь, его закодированная копия, значение по умолчанию или пример с этим значением в публичном пакете отсутствуют.

[reconstruct_runner.py](sources/reconstruct_runner.py) принимает явно переданную оператором утверждённую рабочую область, подставляет её обычные UTF-8 байты и экранированные обратные слеши в соответствующие literals, затем до записи требует исторический SHA-256 `6d16652d4893273b7e9d584920892ce5f5917622e7e242a62eb78ec824bcfb93`. Для точного воспроизведения оператор должен передать исходную утверждённую границу. Иное значение, неполный шаблон или изменённые байты отвергаются; новый output создаётся исключительно при отсутствии прежнего файла. Утилита не импортирует runner, не запускает команды и не предоставляет разрешение на рабочую область.

`$approvedWorkArea` и `$newRunnerFile` в следующем примере — значения, которые оператор задаёт сам; публичный пакет их не заполняет:

```powershell
python -I -S -B sources/reconstruct_runner.py sources/run_owned_commands.py.template "$approvedWorkArea" "$newRunnerFile"
```

При подготовке публикации восстановление выполнено только в памяти и побайтно сопоставлено с исходным runner: **29603 B, точный исходный SHA-256**. Исходный реально исполненный локальный файл не изменён. После восстановления runner по-прежнему требует явный план и hash его байтов, payload/runtime/input pins и изоляцию. Частные планы и execution captures не публикуются.

[receiver-data-refusals.json](receiver-data-refusals.json) сохраняет безопасный исходный результат DATA-проверки: исходная квитанция остаётся FAIL; три допустимых unsigned-кода принимаются именно как закрытый отказ, **18 повреждённых вариантов отвергаются**. Проверка не создаёт процессы и не читает captures. Её PASS не подменяет оставшийся ранее согласованный expected-failure control. [2]

## 4. Отказы и неизвестные состояния

Историческая сводка **717 CLI roots = 712 PASS + 5 SKIP** объединяет разные запуски и ревизии. Она не является одним полным PASS. Соответствие текущему inventory — **2099 CLI Test roots, 3695 API Test roots и 1 Benchmark** — остаётся `UNRESOLVED`; listing не исполняет тела тестов.

GitHub API вновь вернул `422: No commit found` для `33eaf8af8fc46cb231f48c70f4775922393245b1` и `06f77b3856e43ec5e8e3736123aedb896ae58817`. Проверенная локальная исходная repository также не предоставила эти объекты. Это ограниченное наблюдение доступности, не доказательство окончательной утраты. Source4 не выдаётся за восстановление этих двух commits.

**Таблица 3. Сохранённые отказы и неизвестные состояния.**

| Запуск / история | Неизменённая оценка |
|---|---|
| `controller-finish-v3` | FAIL при подтверждённом physical CLOSED; selected canonical-manifest test command exit 1 |
| `canonical-controller-build-v1` | UNKNOWN; terminal parent exit не получен |
| `pwsh-probe-v1` | UNKNOWN; terminal parent exit не получен |
| `serena-product-inputs-v1` | UNKNOWN; terminal parent exit не получен |
| Прежний full-v8 | Исходные FAIL/UNKNOWN сохраняются; нового original-owner closure proof нет |
| Прежний full-v9 | Исходные FAIL/UNKNOWN и отдельно исторический physical CLOSED сохраняют собственную область |
| Ранние source-preparation попытки | CLOSED timeout и CLOSED_PRE_RESUME_FAIL остаются отказами |

В `controller-finish-v3` команда выбрала `TestCanonicalManifestDirIgnoresExecutableContextAndExistence` и `TestCanonicalManifestDirRejectsUncleanCanonicalPathWithoutIO`. Здесь зафиксирован FAIL команды, без утверждения, что каждый выбранный root завершился FAIL. Parent SHA-256 `6932a89976f4c98d6484d0cad9c30005e22a6bd366bd8496f52e5d848f13932b`; execution `aca65241f615640d459f04f12637b8ecc0ccf2bd60a9df573208e03c0cb4e6eb`. Более поздний ожидаемый отказ этот результат не переоценивает. [3]

Кэши UNKNOWN-запусков не приняты к повторному использованию. Передача кэша от завершённого CLI требует собственной приёмки исходных процессов, потоков и независимой копии данных. Такой handoff пока не принят.

## 5. Остаток приёмки и публикация

Обе точные команды на Source4 остаются **NOT_RUN**:

```sh
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

Нужно принять окончательные Source/Product/runtime/private inputs, выполнить полный текущий CLI и получить его закрытый кэш, затем 14 migration и 2 canonical controls в обоих режимах. Три обязательных stderr-сценария и оставшийся ранее согласованный expected-failure control остаются явными критериями; новая DWORD-проверка не зачтена вместо него. Отдельно [native late-owner область](../failed-owner-receiving-20261126/README.md#следующий-шаг-приёмки) сохраняет NOT_QUALIFIED для late original return/reader, read failure после закрытия, frozen outer и одноразового close refusal. Связь именно этой области с исходным согласованным пунктом пока не установлена. Сроки обеих обязательных Go-команд сохраняются.

После фактических результатов остаются собственное и независимое ревью точных дельт, публикация у существующих владельцев #637/#664/#665/#666 через штатный native gate, обратное чтение GitHub и учёт ресурсов. Новые PR и слияние не требуются. Этот пакет сохраняет Source4 и доказательства; готовность Product, standalone PR heads и полной композиции не заявлена.

## 6. Источники и проверяемость

1. [source4-manifest.json](source4-manifest.json), [точный патч](source4.patch), [исходный commit object](source4.commit) и [публичная основа e66](../composed-functional-source-20261126/README.md).
2. [evidence.json](evidence.json), раздел `focused_source4`: исходные hashes parent/execution/stdout, terminal outcomes и полный список выбранных корней.
3. [evidence.json](evidence.json), разделы `controller_correction`, `controller_finish_v3` и `unresolved_parent_snapshots`: публичная проекция полученных фактов с hashes исходных контрольных файлов.
4. [Единый план и сохранённая история](../../../../.plans/2026-11/open-pr-integration-20261117.md#cache-acceptance-recovery-20261128).

Raw private captures и машинные пути в пакет не включены. Рабочая область задаётся оператором только при восстановлении runner из переносимого шаблона. Hash исходной квитанции связывает проекцию с исходным документом, но без его байтов сам по себе не позволяет пересчитать этот документ. Source4, в отличие от частных execution-квитанций, воспроизводится из публичной основы и двух точных текстовых входов. `artifact-manifest.json` связывает опубликованные файлы пакета.

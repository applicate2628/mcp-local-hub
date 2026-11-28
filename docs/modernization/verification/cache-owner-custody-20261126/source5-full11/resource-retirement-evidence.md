# Перенос пяти закрытых ресурсов в trash

**Принят PASS только операции переноса пяти ресурсов.** По фактической приёмке root нативный сеанс Remote Desktop Commander **430560** завершился с кодом **0**; отображённая длительность — **42,07 с**. Все пять исходных идентичностей сохранены после переноса, исходные directory handles закрыты. Полный регрессионный PASS из этой операции не следует. [1,5]

## Область и результат

`TASK` — `<TASK>`. Все назначения находятся в `TASK/trash/closed-source4-and-probe-caches-v1/<имя ресурса>`. Журналы и итоговый receipt сохранены отдельно, в `TASK/closed-source4-and-probe-caches-retirement-v1`, вне trash. [1,2,9–13]

**Таблица 1. Ровно пять перенесённых объектов.** Для каждой строки получены PREPARED → MOVED и `source_absent=true`; volume serial — `275429471`.

| Ресурс | Исходный путь относительно TASK | File index до = после | Основание идентичности |
| --- | --- | --- | --- |
| `source4-partial-tree` | `native-consumer-source-v4` | `562949961648634` | preflight, current-only |
| `startup-build-cache` | `runs/native-start-probe-v1/build-cache` | `12947848931019505` | preflight, current-only |
| `startup-module-cache` | `runs/native-start-probe-v1/module-cache` | `12947848931019506` | preflight, current-only |
| `provider-build-cache` | `runs/provider-helper-qualification-v1/build-cache` | `844424938301922` | создание + preflight |
| `provider-module-cache` | `runs/provider-helper-qualification-v1/module-cache` | `844424938301923` | создание + preflight |

У Source4 и двух startup-кешей основание — точное наблюдение preflight (**current-only**); идентичность первоначального создания для них не заявлена. У двух provider-кешей дополнительно совпали идентичности создания. Во всех пяти случаях целиком равны preflight → before → after: `native.volume_serial`, `native.file_index`, `stat.device`, `stat.inode`. Это проверка тех же каталогов при rename, а не новый хеш содержимого кеша. [1–4,9–13]

Индексы двух startup-кешей превышают диапазон целых чисел, точно представимых JavaScript. Оригинальные JSON разобраны Python с точными целыми; в компактной JSON-проекции идентификаторы записаны десятичными строками. Исходные шесть файлов не переписаны. [1,9–13]

## Проверка способа переноса

Прочитан активный issuer `4394614ae7c2a3b7da7e44fb06ff3a9c5504a1e6253ac1b86415cb6ab6f4e38e`. Восемь функций операций с каталогами и журналом побайтно совпали с ранее квалифицированным исполнителем. Код удерживает handles всех пяти исходных каталогов до первого переноса, сверяет полную идентичность, требует отсутствующее назначение и тот же том. Выполняется rename по исходному handle; удаления, копирования, замены назначения, изменения ACL, повторов или автоматического отката нет. ACL — список прав доступа к объекту. [2,3,6]

Каждый журнал записан эксклюзивно; PREPARED и MOVED проходят flush и fsync. Положительный итог назначается после выхода из `ExitStack`, когда контексты исходных directory handles закрылись. Полученный receipt содержит `original_directory_handles_closed=true`; ERROR-записей и ошибок перемещения нет. [1,3,9–13]

## Сохранённые данные и границы

Три требуемых исполняемых образа, исходные доказательства и данные восстановления находятся вне пяти перемещённых корней. Их пути проверены по закреплённым метаданным; **новое чтение бинарных файлов ради хешей не выполнялось**. Активные Source5, full11 и его собственные module/build caches не пересекаются с областью переноса. Это разделение проверено по точному manifest `15756…`; никаких результатов текущего full11 операция переноса не подтверждает. [2,4,5,7,8]

Приём шести текстовых файлов воспроизводит точный квалифицированный writer: compact JSON, UTF-8, LF и завершающий LF. Локальные файлы совпали с сохранённым transport text и канонической записью без потери uint64. **Отдельной нативной команды измерения хешей не было**; это не заявляется как независимое чтение бинарных файлов. [1,3,5,9–13]

Trash не очищался. Source4 сохраняет CLOSED_FAIL, исходная удерживаемая область — UNKNOWN. Автор выполнил только локальную проверку и создал этот отдельный отчёт; оперативные файлы и прежние публикационные документы не изменены. [1,3,5,8]

## Источники

Пути относительны локальному рабочему набору. Хеши указывают точные принятые файлы, включая реконструированные текстовые копии; их происхождение описано выше.

**[1] receipt.** `recovery/closed-preparation-retirement-v1/actual/receipt.json`; SHA-256 `674c290a6322e0bebabc7e90202d29df03a03a40e31f6a86ede4977cd3181bab`.

**[2] active_plan.** `recovery/closed-preparation-retirement-v1/plan.json`; SHA-256 `f9038834bc91229bd4f9496dd27525a780f031d5f8dfcf517b1e602803334c49`.

**[3] active_issuer.** `recovery/closed-preparation-retirement-v1/retire_closed_preparation_resources.py`; SHA-256 `4394614ae7c2a3b7da7e44fb06ff3a9c5504a1e6253ac1b86415cb6ab6f4e38e`.

**[4] preflight.** `recovery/closed-preparation-preflight-v1/actual.json`; SHA-256 `ec32933134bafffb85ec8bdf1114374352b163da5435e60a64fa736b79ebfa65`.

**[5] root_actual_review.** `recovery/closed-preparation-retirement-v1/actual/root-review.json`; SHA-256 `0f0acb057aedc7aa6971ef6d7a204e782eacf34ea8ee23014922cba23aae30d4`.

**[6] qualified_rename_source.** `recovery/cleanup-v9/retire_closed_v9_resources_v2.py`; SHA-256 `40ecbc3c44811ab92d167601d0716fd9ae9d49afb9062a17b19a1cf75cafce24`.

**[7] live_manifest.** `recovery/runtime-v14-preparation-v1/bound-final-v1/actual-manifest.json`; SHA-256 `15756f71977804cb683ac3cc27f15b7eae1c2b869d28db94c2f501816ee807c8`.

**[8] root_scope_observation.** `recovery/closed-preparation-preflight-v1/root-observation.json`; SHA-256 `f0b2fd9dbb76fa05de41893264fc94c3ba6e83145ddb3c60a91c6cb95d042516`.

**[9] source4-partial-tree_journal.** `recovery/closed-preparation-retirement-v1/actual/source4-partial-tree.jsonl`; SHA-256 `069159ec6dded6f22161a9beaa6e520bf238908c6bea3bfb1f841c4e778cd93c`.

**[10] startup-build-cache_journal.** `recovery/closed-preparation-retirement-v1/actual/startup-build-cache.jsonl`; SHA-256 `da080f19640213a526cdb1c91be2c3744372307855b0f175f612e3a3798fc2a4`.

**[11] startup-module-cache_journal.** `recovery/closed-preparation-retirement-v1/actual/startup-module-cache.jsonl`; SHA-256 `93f2bd62f0c843260fddfb9a0e1edd523a9d86782efd1377bed1e2951129f66c`.

**[12] provider-build-cache_journal.** `recovery/closed-preparation-retirement-v1/actual/provider-build-cache.jsonl`; SHA-256 `15002d116396cef4dbd6329f07bbd428cc71fa266a25b8fa57e9907aed8e6b6f`.

**[13] provider-module-cache_journal.** `recovery/closed-preparation-retirement-v1/actual/provider-module-cache.jsonl`; SHA-256 `8fb78a36cbbb95706be872c7588c9ccbed421b72c5eab782dc2fa4a1d5c64c6b`.

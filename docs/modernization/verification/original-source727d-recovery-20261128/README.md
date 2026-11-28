# Восстановление точного исходного Source 727d

**Исходный Source `727d66753814dc218e26d699e57402db2d13190a` восстановлен: tree `d959ed0a4f8ab02947aec7569f3c0535a3cff533`, 3509 tracked-файлов.** Этот пакет сохраняет небольшую дельту от уже опубликованной точной базы Source `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`, исходные Git commit objects и проверяемое соответствие всех изменённых файлов.

У namespace-патча опубликован переносимый шаблон: шесть значений одного исторического work-area prefix заменены нейтральным маркером. Для получения **точных исходных байтов** оператор явно передаёт первоначальный approved work area. Этот параметр не имеет встроенного значения или закодированной копии. Renderer принимает только результат с исходным размером и SHA-256. Это ограничение публичного воспроизведения указано явно; шаблон не объявляется точным исходным патчем.

## Содержание

1. [Публичная база и идентичности](#1-публичная-база-и-идентичности)
2. [Точная дельта](#2-точная-дельта)
3. [Восстановление из публичных данных](#3-восстановление-из-публичных-данных)
4. [Что непосредственно проверено](#4-что-непосредственно-проверено)
5. [Границы результата и история](#5-границы-результата-и-история)
6. [Источники](#6-источники)

## 1. Публичная база и идентичности

Полный Source3ebe уже сохранён в существующем #631 на закреплённом commit [`c80fd6b4e6f9806eb7d94726a979e9e89d46ec75`](https://github.com/applicate2628/mcp-local-hub/tree/c80fd6b4e6f9806eb7d94726a979e9e89d46ec75/experiments/rust-process-owner/ffi/verification/stdio-consumer-2026-11-26/source-capsule-3ebe). Его [manifest](https://github.com/applicate2628/mcp-local-hub/blob/c80fd6b4e6f9806eb7d94726a979e9e89d46ec75/experiments/rust-process-owner/ffi/verification/stdio-consumer-2026-11-26/source-capsule-3ebe/manifest.json) связывает patch, raw commit и полную опись. Дублировать большой опубликованный патч здесь не требуется. [1]

**Таблица 1. Публичная база.**

| Роль | Точное значение |
|---|---|
| Публичный Git anchor | `62c853e8a9e9b2817f5fcaf15018450b76798690` |
| Tree anchor | `32b55d2e5708462a2bf4cddb4a5cc7143ddf59e4` |
| Восстанавливаемая база | `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4` |
| Tree базы | `67c13b8fc1317b4fd60e3135a3eb8a376fe01817` |
| Опись базы | 3508 файлов, 57144526 байт |
| `source-files.json` SHA-256 | `706c4f284ff9ec4aefd8c61ee18809a6e5424d350206d1403b96006468b78c00` |
| Raw3ebe commit | 276 байт; SHA-256 `a951fc2799e12e93e118ea1251438fb8ca908c86786f0bf65a4e522e04bfe537` |
| Опубликованный полный patch | 3471196 байт; SHA-256 `10af6f1b1b179c04299c13024ca75b4dc868fb945574a3e941b34ac343d7c03e` |
| Git blob полного patch | `4e3de11f96f0b3136b7f749f1319f9d5f11e8be6` |

В этой проверке получены полный публичный inventory и raw3ebe commit. Все 3508 имён, Git modes и blob IDs совпали с восстановленными immutable Git-объектами. Raw commit побайтно совпал с исходным объектом и дал тот же commit ID. Доступность публичного anchor и tree также проверена.

Повторное получение большого patch через использованный connector не завершилось: размер файла не поддержан. Его размер, Git blob и SHA-256 выше получены из публичного tree/manifest, а не заявляются повторно вычисленными от скачанного patch. Ранее опубликованная [квитанция реконструкции](https://github.com/applicate2628/mcp-local-hub/blob/c80fd6b4e6f9806eb7d94726a979e9e89d46ec75/experiments/rust-process-owner/ffi/verification/stdio-consumer-2026-11-26/source-capsule-3ebe/reconstruction.json) отдельно фиксирует PASS применения публичного anchor и patch; нового native replay этой большой базы в данном шаге не выполнялось.

## 2. Точная дельта

**Таблица 2. Две стадии восстановления.** Git tree идентифицирует полный набор имён, modes и содержимого файлов.

| Стадия | Переход | Изменения | Полный итоговый tree |
|---|---|---|---|
| 1 | 3ebe → 0145 | 27 изменённых `*_test.go`; 3508 файлов | `3e394f52ea67468edb20b3590586334acffb716c` |
| 2 | 0145 → 727d | 3 изменённых + 1 новый namespace-файл; 3509 файлов | `d959ed0a4f8ab02947aec7569f3c0535a3cff533` |

Стадия 1 сохраняет накопленные тестовые изменения API/CLI. Стадия 2 сохраняет две реализации namespace-контроллера, изменение существующего теста и новый тест исходного отказа до создания дочернего процесса. Полные before/after mode, blob, размер и SHA-256 всех 31 путей приведены в [changed-files.json](changed-files.json). [2]

`0145a2547a3025cc518cf3e2fa89d2e22cb73c08` — исходный root commit импорта: у него нет parent. Его tree совпадает с ранее полученным `25f9a79851dc7e3e409cfb823b54d590fb1ac550`. Patch 3ebe→0145 описывает переход содержимого и не придумывает Git-parent между этими commits. Настоящий единственный parent `727d` — `0145`; оба точных raw objects включены. Исходный 3ebe имеет parent `c3b8c802be190c335b84ff7323a14d9eb63518b6`; shallow boundary у c3 не означает, что у самого c3 отсутствует parent.

**Таблица 3. Точные полезные данные.**

| Файл / роль | Байты | SHA-256 |
|---|---:|---|
| `3ebe-to-0145.patch`, точный patch | 66642 | `f76c246cadbc3d9a8fa7e1b5eaa2555a50d1e1aae4d0088fd4b8b06d9ac404b2` |
| Исходный `0145-to-727d.patch`, результат renderer | 14766 | `d2f167d9e76c06d8398d02f5bd446e08756c662323f87733ecf9ae8ebb647722` |
| `0145-to-727d.patch.template`, опубликованный шаблон | 14814 | `a118ee2b3454f57210a5bbfc45d181ab1bc60697205953806836b3d0332e08c6` |
| `source-0145.commit` | 390 | `f4b2fc2384953024c20cf9a9b4e1f536d8e2b5bd99d3e3249a2a65bbb56092c5` |
| `source-727d.commit` | 430 | `e208eff80fa6a228f3434917d6457dcdb14d461b054952f3e869fb9394ca0cb2` |

В шаблоне заменён только один approved-work-area prefix в шести вхождениях добавленных тестовых fixtures. Нейтральный маркер `__APPROVED_WORK_AREA__` не содержит исходного значения. Единственный другой абсолютный Windows path в новом тесте — намеренно недопустимая синтетическая fixture отрицательной проверки, а не значение пути пользователя. Все остальные patch bytes сохранены; исходный локальный patch и Source не менялись.

## 3. Восстановление из публичных данных

Следуйте [рецепту опубликованной Source3ebe capsule](https://github.com/applicate2628/mcp-local-hub/blob/c80fd6b4e6f9806eb7d94726a979e9e89d46ec75/experiments/rust-process-owner/ffi/verification/stdio-consumer-2026-11-26/source-capsule-3ebe/README.md#2-reconstruction-recipe) в отдельном новом checkout. Полученный index/tree должен точно равняться `67c13b8fc1317b4fd60e3135a3eb8a376fe01817`. Сохраните исходные line endings; scoped `.gitattributes` этого дополнения отключает текстовую нормализацию.

Сначала получите исходный namespace patch. Имена ниже обозначают явно задаваемые роли, а не реальные пути:

```text
python -I -S -B reconstruct_namespace_delta.py --approved-work-area ORIGINAL_APPROVED_WORK_AREA --output NEW_NAMESPACE_PATCH
```

`reconstruct_namespace_delta.py` сверяет template hash и ровно шесть маркеров, подставляет только переданное значение и проверяет размер 14766 и SHA-256 из таблицы 3. Неверный параметр даёт отказ до записи. Выход создаётся только новым файлом; существующий файл не перезаписывается. Скрипт не вызывает Git, не применяет patch, не исполняет Source и не обращается к сети.

Далее примените обе стадии по порядку с `git apply --index --binary --whitespace=nowarn`. После первой стадии `git write-tree` должен дать tree 0145 из таблицы 2, после второй — tree 727d. Проверяйте exit code каждого шага и останавливайтесь при несовпадении; корректировать контекст, line endings или применять `--3way` для получения другого дерева нельзя.

Точные raw commits можно проверить посредством `git hash-object -t commit`: результаты обязаны равняться `0145a2547a3025cc518cf3e2fa89d2e22cb73c08` и `727d66753814dc218e26d699e57402db2d13190a`. Для отдельного восстановленного Git-репозитория эти же объекты могут быть записаны после проверки соответствующих деревьев. Ни parent c3, ни исходные private refs для восстановления tree 727d не нужны: сохранённый root0145 замыкает точную непосредственную историю 727d.

Этот рецепт описывает восстановление исходных байтов. Он не запускает продукт или проверку приёмки и не выдаёт разрешения использовать исторические кэши.

## 4. Что непосредственно проверено

[reconstruction-evidence.json](reconstruction-evidence.json) отделяет новые DATA-проверки от опубликованной ранее реконструкции базы. Получены и проверены:

- Полное равенство публичной описи Source3ebe и recovered immutable Git inventory: 3508 path/mode/blob записей, без пропусков или лишних путей.
- Точное равенство raw3ebe commit публичному объекту и его Git ID.
- Применение всех **99 + 12 hunks** двух новых patch как данных к immutable beforeimages; каждый afterimage совпал с исходным Git blob.
- Пересборка полных Git tree identities для обеих стадий из всех 3508/3509 записей; результаты совпали с таблицей 2.
- Исходные raw0145/raw727 commit IDs и указанные в них trees/parent.
- Побайтное равенство исходного namespace patch и результата замены шести маркеров операторским значением в памяти. Renderer прошёл синтаксический разбор; отдельного запуска renderer или native Git reconstruction этого дополнения не было.

Работа использовала только immutable Git-объекты двух новых независимых восстановленных репозиториев. Git indexes, refs и настройки не менялись. Исходные исторические worktrees и caches не читались.

Отдельный native `original-source-restore-v2` ранее завершил materialization PASS/CLOSED: 9 команд, parent 47.35906030004844 с, exit 0. Parent SHA-256 `665099615fc911e5db3eef74de7c28e06ea35fcece33d290f5d0439fb3268f20`, execution `890fecd2b2a4e7a1acf36b0d5f3fb7135518d7eeb32498c8610bd7f0965ac4e5`; все исходные владельцы и читатели закрыты. Полученные manifest хеши: 727d — `a3483e469b606fa297a4b33698cd22db7bf25493023f3117feeafb52bb9db5f1`; 3ebe — `c63d8b13331a752c85c5e62f4c4ce0a2ee19cc0c129c83b4dd6ed99fbd944b46`. Это независимая проверка восстановленных файлов из retained Git objects, а не новый прогон публичного большого patch и не full-suite qualification.

## 5. Границы результата и история

Source727d, Product Source3ebe и ранее отдельно сохранённый fallback Source4 являются разными снимками. Этот пакет не переносит их qualification на текущие публичные PR или независимое продолжение A–D. Два восстановленных commits #666 сохранены в [отдельной capsule](../recovered-cli-commits-20261128/README.md); история [717 CLI-корней](../historical-cli717-recovery-20261128/README.md) также имеет собственные source revisions и закрытые исходные результаты.

Старые full-v8/full-v9 FAIL/UNKNOWN, original-source-restore-v1 Bootstrap HOLD/UNKNOWN и исходный full-v9 Source/cache postclosure NOT_RUN сохраняются. Восстановление файлов не превращает эти статусы в PASS. Новые обязательные full commands, 14 migration + 2 canonical controls в обоих режимах, три stderr-сценария и оставшийся expected-refusal control требуют собственных результатов на заявленном окончательном составе. Исторический namespace prechild-refusal код не является доказательством прохождения отдельного оставшегося control.

Капсула сохраняет код и доказательства его идентичности. Native publication gate, внешнее ревью точного PR head, runtime qualification и слияние этим материалом не объявляются выполненными.

## 6. Источники

1. [Публичная Source3ebe capsule на точном commit](https://github.com/applicate2628/mcp-local-hub/tree/c80fd6b4e6f9806eb7d94726a979e9e89d46ec75/experiments/rust-process-owner/ffi/verification/stdio-consumer-2026-11-26/source-capsule-3ebe): исходная полная опись, raw commit, patch, реконструкционная квитанция.
2. [changed-files.json](changed-files.json) и [reconstruction-evidence.json](reconstruction-evidence.json): exact before/after mapping и новые DATA-проверки.
3. [artifact-manifest.json](artifact-manifest.json): размеры, SHA-256 и Git blob IDs всех файлов этого пакета.
4. [3ebe-to-0145.patch](3ebe-to-0145.patch), [source-0145.commit](source-0145.commit), [source-727d.commit](source-727d.commit), [namespace template](0145-to-727d.patch.template) и [renderer](reconstruct_namespace_delta.py): сохранённые точные данные и явно параметризованный транспорт.

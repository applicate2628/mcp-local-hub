# Source5 / update2: приёмка 13 тестовых изменений

**PASS только для обновления исходного состояния.** В `native-consumer-source-v5` принят новый частный commit `001648f44bf6ef0191f1e21d92f1aecf2a2a3022` с единственным родителем Product `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`. Полная регрессия этим этапом не выполнялась. Идентификатор частной тестовой композиции отличается от публичных PR heads; результат не переносится на их текущие версии. Этот отдельный черновик дополняет замороженный отчёт о материализации Source5. [1–5,10–12]

## Оглавление

1. [Точный состав](#точный-состав)
2. [Команды и закрытие](#команды-и-закрытие)
3. [Границы приёмки](#границы-приёмки)
4. [Источники](#источники)

## Точный состав

В карте по-прежнему **3508 файлов**. Изменены ровно **13 файлов `_test.go`**, остальные **3495** совпадают с Product. Все **2012 файлов, чьи пути не заканчиваются на `_test.go`**, побайтно совпадают по закреплённым SHA-256. Добавленных и удалённых путей нет. Все 13 пар before/after совпали с ранее рассмотренной `delta-map.json`; рабочее дерево принято чистым и закоммиченным. [2–5,8,10,11]

SHA-256 исходной карты: `671d60c8f1e96de28eb616b9e7aeacccae50d00359ab27a59418bea61648ee16`. SHA-256 новой карты: `08bfb69180f70b6f2cfd2c46429c0f4047e449f6bb4508501151d17578a12e16`. Отдельные файлы карт совпали целиком с картами внутри исходного receipt. Преобразование связано с принятой базовой материализацией Source5 через её точный root `374f4bb359fa81b35de0f3ace9ca73c9ed88d91298f2d7d74ce219022afcba50`. [2–5,12]

13 путей перечислены в JSON-проекции и исходной delta-map. В их числе — существующие исправления случайно фиксированных портов, два файла повторного использования provider-helper внутри владельца `TestMain` и рассмотренная функция GUI force. Отдельный диагностический overlay с дополнительными измерениями времени в эти 13 файлов не включён. Это состав исходников, а не доказательство прохождения тестов. [8,9]

## Команды и закрытие

Родитель принял исходный процесс с кодом 0 за **19.0421809 с**. Все **10 нативных команд** имеют полученное завершение, пустое дерево процессов и закрытые дескрипторы. Все **20 stdout/stderr потоков** достигли конца данных, закрыли каналы и файлы; исходные читатели завершены. Два собственных файла вывода родителя закрыты отдельно. [1–3,10,11]

Выполненные аргументы каждой команды сравнены с выражениями активного исполнителя, извлечёнными из его синтаксического дерева без импорта или запуска. Порядок совпал с активным планом. Совпали 21 вход плана и 22 входа receipt, включающего сам план. Для каждой команды сохранены рабочий предел **30 с**, предел завершения **10 с**, исходный владелец, `cwd=Source5` и `stdin_mode=null`. Флаги управляющего Python — `-I -S -B`; диагностический предел родителя — 600 с. [1,2,6,7]

**Таблица 1. Точные суффиксы аргументов после общего префикса Git.** `<PATCH>` обозначает закреплённый patch по пути `<TASK>/test-source-update-inputs-v2/test-source.patch`. Время округлено до миллисекунд; точные значения сохранены в JSON. [2,6,7,9]

| Команда | Аргументы после префикса | Время, с |
| --- | --- | ---: |
| `head-before` | `rev-parse --verify HEAD` | 0.093 |
| `files-before` | `ls-files --cached -z` | 0.221 |
| `status-before` | `status --porcelain=v1 --untracked-files=all -z` | 0.831 |
| `apply-check` | `apply --check --index <PATCH>` | 0.149 |
| `apply-test-delta` | `apply --index <PATCH>` | 0.415 |
| `staged-paths` | `diff --cached --name-status -z` | 0.328 |
| `private-commit` | `-c user.name=ChatGPT -c user.email=chatgpt@local.invalid commit --no-gpg-sign -m "test: align native regression fixture boundaries"` | 0.560 |
| `candidate-parents` | `rev-list --parents --max-count=1 HEAD` | 0.129 |
| `files-after` | `ls-files --cached -z` | 0.205 |
| `status-after` | `status --porcelain=v1 --untracked-files=all -z` | 0.219 |

Общий префикс включает закреплённый `git.exe`, `--no-optional-locks`, `--no-replace-objects`, настройки `gc.auto=0`, `maintenance.auto=false`, `core.autocrlf=false`, `core.fsmonitor=false`, `core.untrackedCache=false`, `core.longpaths=true`, пустой `credential.helper`, собственный `core.hooksPath=<RUN>/empty-hooks` и `-C <SOURCE>`. Полный список аргументов приведён в JSON. Проверка `apply --check --index` предшествует единственному применению `apply --index`. Команд Go и запуска тестов в этом перечне нет. [2,6,7]

## Границы приёмки

Отдельный нативный DATA-приёмщик повторно проверил все **20 raw captures** и **3508 физических исходных файлов**. Этот локальный отчёт проверяет точные управляющие записи, карты, аргументы и связи с кодом/планом; он **не является ещё одним чтением** этих raw captures или дерева Source5. Независимый фактический slice находится в `independent-activation-review.json`, а не в предшествующем binding review. [3,7,10,11]

Хеши вычислены по исходным байтам. Переводы строк не преобразованы, оригинальные JSON не переписаны; Python сохранил точность целых чисел, а 64-разрядные идентификаторы объектов в эту проекцию не переносились. Прежние два Source5 evidence-файла проверены по хешам и оставлены без изменений.

**Таблица 2. Следующие отдельные результаты.** `PENDING` означает отсутствие соответствующей приёмки в этом срезе, а не утверждение об отсутствии уже начатой работы root.

| Этап | Статус этого среза |
| --- | --- |
| Базовая материализация Source5 | PASS ранее принятого Product; исходный отчёт сохранён |
| Update2, 13 тестовых файлов | PASS обновления исходников |
| Rebinding-v11, issuer/parent v12 | PENDING отдельной фактической приёмки |
| Два случая квалификации runtime14 | PENDING отдельной фактической приёмки |
| Обе обязательные полные команды full11 | PENDING; полного PASS нет |

Допуск update2 остаётся `immutable-tracked-source-only`: он не выдаёт права на кеши, очистку, установку или готовность продукта. Source4 остаётся CLOSED_FAIL, full-v10 — FAIL, исходная удерживаемая область — UNKNOWN. Квалификация Serena и прежняя матрица 14 случаев владения процессами не повторялись этим этапом. Основной план, оперативные исходники/runtime и публичные PR автор этого черновика не менял. [1–3,10,11]

## Источники

Пути относительны локальному рабочему набору. SHA-256 относится к оригинальному файлу, а не к проекции.

**[1] parent.json.** `recovery/source-v5-update-v2-root/actual/parent.json` — 3528 байт; SHA-256 `deb49a9aec43da26b4b5903e8797d2c6646f9b6d4d45df072d9dbfebd76c239a`.

**[2] receipt.json.** `recovery/source-v5-update-v2-root/actual/receipt.json` — 947460 байт; SHA-256 `0ee814004ae40631bfc24da8e1afd996094defeac036f8eaafcd785e44d00818`.

**[3] root-receiving.json.** `recovery/source-v5-update-v2-root/actual/root-receiving.json` — 3574 байт; SHA-256 `6ab9ba365a968f27d5f30d85e6074df16158912ce8e22c9b215f9c397fce33c1`.

**[4] source-before.json.** `recovery/source-v5-update-v2-root/actual/source-before.json` — 423970 байт; SHA-256 `671d60c8f1e96de28eb616b9e7aeacccae50d00359ab27a59418bea61648ee16`.

**[5] source-after.json.** `recovery/source-v5-update-v2-root/actual/source-after.json` — 423970 байт; SHA-256 `08bfb69180f70b6f2cfd2c46429c0f4047e449f6bb4508501151d17578a12e16`.

**[6] source-update-plan.json.** `recovery/source-v5-update-v2-root/source-update-plan.json` — 4811 байт; SHA-256 `ab0a7e5f886e85f9d95853f4b618d750d72f8c8e484be6704662e3e7c2084680`.

**[7] issue_test_source_update_v2.py.** `recovery/source-v5-update-v2-root/issue_test_source_update_v2.py` — 45976 байт; SHA-256 `26764b122777ef21d6756c05d1cd3131e16fb92330762675c3b0a75b2e76f7ac`.

**[8] delta-map.json.** `recovery/source-v5-update-v2-root/delta-map.json` — 4417 байт; SHA-256 `747eb2b7d2632da92912b32bc3ed2128ab11ad44c05a8cc7ddecef654eef3d92`.

**[9] test-source.patch.** `recovery/source-v5-update-v2-root/test-source.patch` — 29007 байт; SHA-256 `588eebd14517a0133ff3762f1db23b866a740d4fcb1e3a18a6c4d89debfd0113`.

**[10] peer-actual-slice.** `recovery/rebinding-v12-preparation/independent-activation-review.json` — 4314 байт; SHA-256 `c20ccaae561ce041fa9282b706784b842afbab9d87be0f81c48ed339802c71b0`.

**[11] root-review.json.** `recovery/source-v5-update-v2-root/actual/root-review.json` — 974 байт; SHA-256 `f76ba6ba20caf94ca5f6769250c8807b9982080bf4673530da774413291402b6`.

**[12] base-root.** `recovery/source-v5-preparation-v1/actual/root-receiving.json` — 3750 байт; SHA-256 `374f4bb359fa81b35de0f3ace9ca73c9ed88d91298f2d7d74ce219022afcba50`.

# Source59f: закрытый отказ API, диагностика и сохранение исходных данных

## Оглавление

1. [Результат и границы](#1-результат-и-границы)
2. [Закрытый API и история receiving](#2-закрытый-api-и-история-receiving)
3. [Изоляция Source и закрытые этапы](#3-изоляция-source-и-закрытые-этапы)
4. [Наблюдение копирования и Sync](#4-наблюдение-копирования-и-sync)
5. [Три точных объекта Git](#5-три-точных-объекта-git)
6. [Независимые DATA-проверки прежних пакетов](#6-независимые-data-проверки-прежних-пакетов)
7. [Побайтное восстановление](#7-побайтное-восстановление)
8. [Что остаётся открытым](#8-что-остаётся-открытым)

## 1. Результат и границы

На Source **59f618ebfbf6d9f52c6994d68586e3c2ebc202a8**, tree **03e8488a44e67f1ea7e8dc2ef0ece53fee5ae8e1**, default API завершил original admission отказом **FAIL/CLOSED до входа в contained full suite**. Все **3460** найденных top-level test roots имеют результат **NOT_RUN**; семь обязательных API checks также **NOT_RUN**. Ноль failed test cases в этом отчёте означает отсутствие запущенной suite, а не успешную приёмку.

Пакет сохраняет **136 исходных файлов, 2293303 bytes**: original API receipts и шесть captures, контрольные request/response/replay records этого закрытого API, три версии DATA receiver, closed native stages 16–20, исходник и результат отдельной copy/Sync пробы, три raw Git commit objects и независимые root DATA verification receipts. Побайтные дубликаты хранятся одним payload с несколькими записями в [source-bindings.json](source-bindings.json). Все исходные SHA-256 и пути-роль перечислены там; [evidence.json](evidence.json) содержит проверяемую сводку.

**Source** здесь — зафиксированное дерево исходников. **Receipt** — исходная квитанция владельца операции. **Receiving** — проверка этой квитанции и её связей. **CLOSED** описывает завершение процессов и потоков конкретной операции; **PASS** относится к явно указанному виду проверки. **DATA** означает чтение и проверку сохранённых байтов без повторного выполнения сохранённых программ.

## 2. Закрытый API и история receiving

Первичный API receipt имеет SHA-256 **e1cba7d99294f4e18816ebc91a8fd137c3533be5d9ed433071de4c89b66fc709**. В нём три original commands: чтение Go build information, получение списка тестов и namespace admission. Их native/IO completion получен; outputs составляют шесть файлов. Собственно test suite не началась. Список тестов был получен штатным inventory вызовом; это не выполнение перечисленных tests.

Первичная ошибка: `original namespace admission refused before a contained stage: context deadline exceeded`. В namespace receipt явно записано **contained_stage_entered=false**. Сохранённый `proof/inputs.json` имеет SHA **4279192e1bfb3fd2e6b14aa0fbac5891189ec1c4cebe19691a1eaacd1507578e**. Для следующего `proof/binary.json` зафиксировано отсутствие файла; автор повторно проверил только этот явно разрешённый metadata path. Наличие executable payload по другим путям не проверялось.

**Таблица 1. История DATA receiver; каждый результат сохранён отдельно.**

| Receiver | Классификация исходного API | Результат DATA-проверки | Причина |
|---|---|---|---|
| v1 | UNKNOWN | NOT_RECEIVED | Контрольный reader ожидал поле `kind` у inventory record; остановился с KeyError до payload access |
| v2 | FAIL/CLOSED | FAIL | Physical closure и шесть captures получены; reader затем потребовал test2json conversion, которая отсутствовала после prechild refusal |
| v3 | FAIL/CLOSED | PASS | Добавлена точная ветвь получения prechild refusal: нет converter/suite coverage, inventory переводится в NOT_RUN |

Исходники, две точные patches и все три receipts присутствуют в пакете. Их изменения относятся к DATA receiver; исходный API запуск не повторялся этим получением. Последний результат имеет SHA **703394309664fda84d940eeb2b0a3c091f2355453836d9907f38d0dad91aa494**. Он явно запрещает перенос closure/qualification на full run, CLI и cache handoff.

Начальные сроки сохранены: test timeout **5 min**, namespace admission bootstrap **5 s**, API adapter total **355 s**. Recorded original API duration — **34.677347199991345 s**, preparation — **12.505174800055102 s**, suite-owner call — **18.394 s**. Эти поля описывают разные интервалы и сами по себе не устанавливают, какой внутренний шаг исчерпал admission deadline.

Первичные байты: [API receipt](templates/api/original-receipt.json.template), [namespace refusal](templates/api/namespace-owner.json.template), [six capture bindings](source-bindings.json), [v3 receiving](templates/api/root-api-receiving-v3.json.template).

## 3. Изоляция Source и закрытые этапы

**Таблица 2. Отдельные закрытые native операции.**

| Этап | Original result | Commands / capture files | Подтверждённый результат |
|---|---|---:|---|
| 16 | FAIL/CLOSED | 1 / 2 | Native HEAD read завершён; controller отверг ожидание другого HEAD до изменения Source |
| 17 | PASS/CLOSED | 7 / 14 | Создан отдельный worktree точного Source59f; identity и clean status получены |
| 18 | PASS/CLOSED | 7 / 14 | Получены raw commits 04e/74e/59f, собрана DATA copy probe, сохранена Go build metadata; worktree clean |
| 19 | PASS/CLOSED | 3 / 6 | Выполнена отдельная DATA copy/Sync observation; worktree clean |
| 20 | PASS/CLOSED | 6 / 12 | Публикационный worktree штатно продвинут fast-forward с 05fb до уже опубликованного 27d90e62; сохранён чужой checkpoint |

Всего — **24 native commands**, **48 command captures** и **10 отдельных пустых controller streams**. Для каждого сохранены config, original command receipt, outer receipt, root receiving и независимая DATA audit-квитанция. Native waits и reader closure относятся к original objects; автор этого пакета повторно native операции не выполнял.

У этапа 16 ожидался **27d90e626465dc9a3a012e50785d62785d4c5e7e**, фактический HEAD был **05fbffa30f99914c8174966525a70bc9984e7038**. Этот FAIL сохранён. Этап 17 использовал исправленное ожидаемое значение. Его проверка отсутствия новой local branch штатно вернула ожидаемый exit 1; она не заменяет отдельно запрошенный исторический expected-refusal check.

Изолированный worktree имеет отдельные рабочие файлы и использует существующую Git object database. Эта изоляция не является получением closure старого UNKNOWN scope. Статус main/CLI описан в разделе 8.

## 4. Наблюдение копирования и Sync

Исходник standalone probe и его README сохранены вместе с actual result. Probe прочёл только ранее закрытую копию API image размером **38868992 bytes**, SHA **d237704913484fe2931860793badbfdaadb3bb216f5c436b8d01bdf2301d3e60**. До и после пробы совпали hash, file identity и размер. Копии использовались как данные; их не запускали.

**Таблица 3. Одно наблюдение каждого режима; значения в миллисекундах.**

| Интервал | With Sync | Without Sync |
|---|---:|---:|
| Copy + hashing | 36.3576 | 35.4182 |
| Sync | 255.9278 | 0, вызов не запрошен |
| Destination close | 0.4631 | 0 |
| Readback + hashing | 22.2272 | 133.3735 |
| Total | 314.9757 | 168.7917 |

Исходные значения в receipt записаны в наносекундах; таблица получена делением каждого `*_ns` на **1000000**. Наносекунда — миллиардная доля секунды, миллисекунда — тысячная. Нулевое измеренное значение close сохраняется как записанное наблюдение, без предположения о математически нулевой стоимости.

Режим **with-sync** выполнен первым, **without-sync** вторым; порядок не рандомизирован. По одной паре наблюдений нельзя выделить причинный эффект Sync из состояния файлового кэша и scheduling. Наблюдаемые **255.9278 ms** существенно меньше original bootstrap limit **5 s** и не объясняют сами по себе исходный отказ. Изменение admission/test deadlines и удаление Sync этим пакетом не выполнялись.

Actual probe receipt и stage19 stdout побайтно одинаковы: **2312 bytes**, SHA **b6c58b89f636960f2d23b55472ed8c647e42a3aa9855a3cad9c060101d35a400**. Оба original role paths ссылаются на один payload. [Probe result](templates/probe/probe-receipt.json.template); [probe source](templates/probe/source/main_windows.go.template).

## 5. Три точных объекта Git

Raw bytes получены original native `git cat-file commit` на этапе 18. Автор независимо вычислил Git object SHA-1 как DATA из стандартного заголовка с типом `commit`, длиной raw body, NUL separator и самим body. Все три identity совпали; author/committer metadata и сообщения сохранены буквально.

**Таблица 4. Цепочка exact commit objects.**

| Commit | Parent | Raw bytes |
|---|---|---:|
| 04e5b4eadd6407cbff4e04ba453546d278314bd7 | 33eaf8af8fc46cb231f48c70f4775922393245b1 | 277 |
| 74e7563db244ede5984834233ce8d59072e92c0a | 04e5b4eadd6407cbff4e04ba453546d278314bd7 | 280 |
| 59f618ebfbf6d9f52c6994d68586e3c2ebc202a8 | 74e7563db244ede5984834233ce8d59072e92c0a | 270 |

Raw objects: [04e](exact/stage18/captures/raw-source04e-commit.stdout.txt), [74e](exact/stage18/captures/raw-source74e-commit.stdout.txt), [59f](exact/stage18/captures/raw-source59f-commit.stdout.txt). Trees и SHA-256 каждого body перечислены в [evidence.json](evidence.json). Этот пакет добавляет точные commit bytes к уже сохранённым source deltas; получение объектов не означает PASS тестов.

## 6. Независимые DATA-проверки прежних пакетов

Supplement receipts сохраняются буквально, без замены их исторических значений:

- Original root audit проверил **608 artifact files в шести capsules**, byte reconstruction **133 preparation originals**, **333 новых capture originals**, **20 prior-full originals**, **4 prior-API originals**.
- Отдельный exact verifier подтвердил **283 original commands, 566 captures, 65 package receipts**, включая **285 различных payload hashes** и **277 empty captures**. Исторический full-v6 остаётся **FAIL/CLOSED**.
- Owner replay независимо восстановил все **130 original owner receipts /348272 bytes**.
- Ранее root восстановил **213 original trail files**, соответствующие **74 native commands /148 command captures**; stage-v11 FAIL сохранён.
- Новый old-CLI parser v2 дал **PASS_EXACT_LEDGER** для **1599 ledger rows /456870 bytes**, SHA **6a687a7e45efcf069e46e0bbafd36edcf09247e8f09e49467c59278925c34ea8**. Его root receipt и summary сохранены отдельно от прежнего 608-file audit.

Сохранены три root verify PowerShell scripts. Первая old-CLI parser попытка завершилась DATA failure при ASCII decode; успешная проверка artifact bytes не являлась успешным выполнением этой попытки. Исправление и исходный failure документируются отдельным **old-cli-budget-parser-v2-20261128** capsule. Здесь сохранены original v1/v2 verification scripts и отдельный v2 root result; текущая API/CLI qualification из этих DATA checks не выводится.

## 7. Побайтное восстановление

Файлы **exact/** содержат исходные bytes. В **templates/** заменены только явные environment-dependent paths и original owner SID на нейтральные role markers. Native process IDs, handles, timings, nonces, exit codes, FAIL/UNKNOWN fields и остальные значения сохранены. Никаких скрытых, encoded или default host values в public package нет.

Для восстановления нужны **10 explicit operator role values**. Полная карта и descriptions находятся в [source-bindings.json](source-bindings.json). Values хранятся вне public package и передаются оператором. Используется ранее проверенный DATA reconstructor; изменён только binding SHA.

~~~text
python -I -S -B reconstruct_sources.py --roles-json "$OriginalRoleValues" --output "$NewAbsentDataDirectory"
~~~

Программа проверяет SHA/размер каждого public payload, точные marker counts, затем SHA/размер всех **136 originals** до первой записи. Output directory должен отсутствовать. Она записывает original role paths и reconstruction receipt, проверяет written readback; содержимое сохранённых программ не импортируется и не выполняется. Неверные role values отвергаются исходными hashes.

Автор выполнил полный in-memory DATA round trip **136/136**, проверил все три Git object SHA-1, actual original text/size и закреплённые receipt hashes. Новый Python reconstructor автор не запускал. Root receiving этой публикационной копии выполняется отдельно. `.gitattributes` фиксирует `-text`, сохраняя original LF/CRLF и progress CR bytes.

## 8. Что остаётся открытым

На момент parent status observation main full59f имел **FAIL**, причину retained **UNKNOWN**, живой parent и **physical_complete=false**; default invocation markers — **14 started /14 completed**, CLI unsettled, API subtree closed, state invocations — **0**. Это явно обозначено как parent observation: raw UNKNOWN payload и новая full/CLI receiving автором не читались и не выполнялись.

Изолированный worktree, закрытый API и успешные DATA checks не выдают разрешение на UNKNOWN captures/cache и не закрывают общий acceptance. Исторический отдельно запрошенный expected-refusal control остаётся самостоятельным unresolved пунктом; prechild отказ API и ожидаемый branch-absence exit 1 его не подменяют.

Stage21 и более поздние diagnostics остаются вне этого frozen snapshot. Source/cache, original active/UNKNOWN resources и operator role values продолжают сохраняться по собственным правилам receiving и disposition. Файлы этим пакетом не перемещались.

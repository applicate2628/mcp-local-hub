# R25 — Rust: подтверждённые исходники и граница параллельной работы

## Содержание

1. [Главный вывод](#главный-вывод)
2. [Матрица передачи](#матрица-передачи)
3. [Точные локальные ветки](#точные-локальные-ветки)
4. [Последний незавершённый пункт](#последний-незавершённый-пункт)
5. [Состояние передачи Wave → Comet](#состояние-передачи-wave--comet)
6. [Ограничения проверки](#ограничения-проверки)
7. [Локальные доказательства](#локальные-доказательства)

## Главный вывод

Исходники параллельной Rust-работы **найдены**. Состояние «source отсутствует» для общего плана больше неверно. Самая новая наблюдённая рабочая копия находится на Wave: `D:\dev\ChatGPT\mcp-rust-contained-stream-20261120`, ветка `review/rust-contained-stream-20261120`, точный HEAD (текущий коммит Git) `a217c7799a0214967bf86211542e508162c680b5`.

Read-only Git-проверка 21 ноября 2026, 12:15:30 UTC (координированное всемирное время) вернула эту ветку и пустой `status --porcelain=v1 --untracked-files=all`, все коды завершения 0. Ветка обновлялась сегодня; её действующий отчёт активирован в 12:01:24 UTC. Это отдельная работа с собственными подготовленными входами и явной остановкой перед новой установочной попыткой. Рабочую копию, результаты, `D:\dev\ChatGPT\se2` и ожидающий вариант v2 следует сохранять без изменений.

Для чтения кода и ревью доступен именно `a217c7799a0214967bf86211542e508162c680b5`. Проверенные исходники более ранней независимой receiving-ветки доступны на `900fba3eb7c97477ca24dbf6e6cda749ed6d5de3` (код `fb0475da337f87caaa08a7fd894633a6ac4e58d2`). Наличие кода не закрывает текущие общие проверки проекта и не означает, что передача на Comet принята.

## Матрица передачи

**Таблица 1. Источник → сохранённые доказательства → наблюдаемое состояние → возможное действие.**

| Источник | Доказательства | Сейчас наблюдено | Что возможно в R25 |
|---|---|---|---|
| Wave, `mcp-rust-contained-stream-20261120`, `a217c779…` | Живые HEAD/branch/status/log; `mcp-rust-contained-stream-work-20261120\regression_resume_plan_20261121.md`; прямое чтение `serena_single_diagnostic_v2\execution-approval.json`, `READY-proposal.json`, `READY-root-review.json` | Чистая текущая ветка. Вариант v2: `approved=false`, `automatic_repeat_authorized=false`, `installation=NOT_RUN`, `owner_or_test_execution_for_v2=NOT_RUN`; каталог `runs\serena-single-state-v2` отсутствует (ENOENT при `get_file_info`) | Ревью фиксированного коммита и согласование зон ответственности. Продолжение или импорт активной линии без координации не требуется |
| Wave, `mcp-rust-receiving-followup-20261120`, `900fba3e…` | Живые HEAD/branch/status/log; implementation/audit receipts; сохранённые bundle/ZIP и recovery manifest | Чистая receiving-ветка. Код `fb0475da…`, отдельный документационный коммит `900fba3e…`. Самостоятельный полный архив есть на Wave; на Comet эта поставка ещё не принята | Ревью ровно кода `fb0475da…` или полного снимка `900fba3e…`. Его исторические результаты сохранять с исходной привязкой |
| Wave, `mcp-rust-supervisor-resume-20261119`, `9ccd96a0…` | Живые HEAD/branch/status/log; `native-commit.json`, `process-commit.json`, `receiving-commits.json`, `documentation-finalization.json` | Чистый исторический supervisor-снимок; код завершён на `4c8742c4…`, отчёт на `9ccd96a0…` | Использовать как точку сравнения, не как последний HEAD параллельной работы |
| Wave, `mcp-product-rust-custody-retry-20261119`, `f0ffbde1…` | Живые HEAD/branch/status/log; дополнительная source-only ветка в receiving recovery manifest | Чистая отдельная ветка `spike/product-rust-members-20261119` с альтернативным supervisor/argv0 вариантом. Действующий receiving-аудит предупреждает, что перенос этого старого варианта поверх новой линии заменил бы её более новый интерфейс | Сравнение исходников. Не переносить старый вариант вслепую; его историческая квалификация не включена в новый evidence ZIP |
| Comet, `mcp-product-rust-members-ffi-20261118`, `60316c33…` | Живые HEAD/branch/status; receiving setup/plan; каталог recovery | Получатель чистый, остался на прежнем `60316c33…`. Каталог `.scratch\recovery\rust-receiving-followup-20261120` отсутствует; `incoming` пуст | Нельзя объявлять новый source/evidence доставленным. Не возобновлять большой перенос в рамках этой проверки |
| Comet, `mcp-r23-rust-20261121` | `RESULTS.md`, source manifest | Самостоятельная проверка публичных снимков; отчёт прямо исключает импорт параллельной wait/close работы. Tested #631 — `ebfa4ef32b57cfa022acdcfcb4a2745f32b815c6`; #644 — `1cf5754b81c850a3569be269642b49605895eeb2` | Сохранять различие между публичной веткой, испытанным снимком и параллельными локальными ветками |

Пути в таблице 1 раскрываются относительно `D:\dev\ChatGPT` на названном устройстве. Названия файлов не являются командами к исполнению.

## Точные локальные ветки

**Таблица 2. Наблюдённые ссылки Git.**

| Устройство / рабочая копия | Ref | Точный HEAD | Пустой porcelain status |
|---|---|---|---|
| Wave / `D:\dev\ChatGPT\mcp-rust-contained-stream-20261120` | `refs/heads/review/rust-contained-stream-20261120` | `a217c7799a0214967bf86211542e508162c680b5` | Да, 12:15:30 UTC |
| Wave / `D:\dev\ChatGPT\mcp-rust-receiving-followup-20261120` | `refs/heads/fix/rust-receiving-regressions-20261120` | `900fba3eb7c97477ca24dbf6e6cda749ed6d5de3` | Да, общий снимок 12:14:12 UTC |
| Wave / `D:\dev\ChatGPT\mcp-rust-supervisor-resume-20261119` | `refs/heads/review/rust-supervisor-resume-20261119` | `9ccd96a07d2b198463272ab8fad43e3ba99aa65c` | Да, общий снимок 12:14:12 UTC |
| Wave / `D:\dev\ChatGPT\mcp-product-rust-custody-retry-20261119` | `refs/heads/spike/product-rust-members-20261119` | `f0ffbde1beaf5a4ffbb4fa4630aa5d0f18332e02` | Да, общий снимок 12:14:12 UTC |
| Comet / `D:\dev\ChatGPT\mcp-product-rust-members-ffi-20261118` | `refs/heads/spike/product-rust-members-ffi-20261118` | `60316c3318c169bf3d0deb42318128712ba7acac` | Да, 12:14:57 UTC |

Предварительные подсказки о `a7ff841e` и `b3f76787` подтверждены в истории **contained-stream**, а не как текущий HEAD supervisor-resume:

- `a7ff841e12aaabfc766611efa80c7a70a6ffc007` — `fix(cli): reuse accepted creator proof during abort`.
- `b3f767877c7d4c9faaaac9845c688b7011955d3e` — `fix(process): decode integrity SIDs from pinned token buffers`.

Далее текущая линия содержит исправления runtime input, очистки, потоков файла, интерпретатора и диагностики. Последние три наблюдённые записи: `8870041775d02cbee46c8de656675b390c7d009f`, `057a42bee37a570762334dfe5cdcb35bdb327ea3`, `a217c7799a0214967bf86211542e508162c680b5`. Полный наблюдённый хвост из 16 коммитов сохранён в JSON.

## Последний незавершённый пункт

Действующий файл — `D:\dev\ChatGPT\mcp-rust-contained-stream-work-20261120\regression_resume_plan_20261121.md`, 1180 строк в момент чтения. Файл с суффиксом `final_20261121.md` более старый и не является действующим handoff. Активация v5 записана в `report_activation_after_single_v5.json`, 2026-11-21 12:01:24.839627 UTC; сохранённый SHA-256 действующего документа — `1d542981a73d3424448c273f1230bc3e5268931fe8e8758d871221151c646146` (это значение сохранённой записи, не новая наша полная перепроверка файла).

По принятому отчёту прежний единственный запуск `serena-single-state-v1` завершился `RECEIVED_FAIL`, с внешним exit 1 и вложенным exit 86. Сохранённая диагностика исходного UV (менеджера Python-пакетов) сообщает exit 1 при сборке `proxy-tools==0.1.0`, Windows `os error 267`. Автор параллельной работы затем подготовил короткий evidence parent `D:\dev\ChatGPT\se2`, private state v6 и controller v2. Мы эти результаты не выполняли и не переоценивали.

Непосредственно прочитанные текущие записи v2 согласованы:

- `execution-approval.json`: `source_commit=a217c779…`, `run=serena-single-state-v2`, `test=TestMigrateSerena_CallsInstallParsedManifest_NotDiskWrite`, `lane=test_state_path_env`, `approved=false`, `automatic_repeat_authorized=false`.
- `READY-proposal.json`: `READY_FOR_EXPLICIT_SINGLE_INSTALLATION_CONSENT`, `installation=NOT_RUN`, `owner_or_test_execution_for_v2=NOT_RUN`, 54 подготовленных входа и по 3470 source/private-export файлов; историческая проверка свежести выполнена 11:42:01 UTC.
- `READY-root-review.json`: `READY_UNAPPROVED`, `new_runtime_started=false`, `new_installation_authorized=false`.
- Наша текущая операция `get_file_info` для `D:\dev\ChatGPT\mcp-rust-contained-stream-work-20261120\runs\serena-single-state-v2` вернула ENOENT.

Таким образом, первоначальная точка «завершить token binding test» уже пройдена в другой линии. Остаток теперь — подготовленная новая одиночная проверка Serena с отдельным согласием; пара общих тестовых команд остаётся незакрытой. Это контекст для разграничения работ, а не просьба выполнять установку в R25.

## Состояние передачи Wave → Comet

На Wave каталог `D:\dev\ChatGPT\.scratch\recovery\rust-receiving-followup-20261120` содержит `receiving-followup-source.bundle`, `receiving-followup-evidence.zip`, `LOCAL-VERIFIED.json`, `recovery-manifest.json`, `bundle-verification.json`, `selection.json`, `source-before.json`, `transfer-receipt.json` и README.

**Таблица 3. Идентичности из сохранённого recovery manifest.**

| Объект | Размер | SHA-256 / привязка |
|---|---:|---|
| `receiving-followup-source.bundle` | 843101 байт | `de8c3cbb47bc744cb20b476776536cdcce2d31edc5f024d6de3cae5d60cfdf3f` |
| `receiving-followup-evidence.zip` | 408730894 байт | `6259fff7513fdfb532c4bf82299737ffd79f614bb7d1bad3610dddc64fed44cd` |
| Основная bundle-ветка | — | `fix/rust-receiving-regressions-20261120` → `900fba3eb7c97477ca24dbf6e6cda749ed6d5de3` |
| Дополнительная source-only ветка | — | `spike/product-rust-members-20261119` → `f0ffbde1beaf5a4ffbb4fa4630aa5d0f18332e02` |
| Обязательный предок bundle | — | `562313b643406d685d4fee357dc697c31e18f819` |

Сохранённая локальная верификация от 20 ноября 10:20:54 UTC сообщает 680 логических файлов, 355 уникальных payload и 28 выбранных receipts, `LOCAL_VERIFIED`, `comet_verified=false`. Она явно исключает изменяющуюся NEWER работу и старую квалификацию дополнительной spike-ветки. Мы подтвердили наличие файлов и прочитали идентичности; архив и bundle заново не хешировали и не распаковывали.

На Comet соответствующий окончательный recovery-каталог отсутствует, а staging `incoming` пуст. В `parts-small-v3` сохранилась неполная последовательность частей архива. Нового подтверждения Comet не найдено. Сохранённый `lan-transfer-v3\server-completed.json` на Wave фиксирует окончание 20 ноября 15:33:06 UTC, `bytes_served=0`, `client_finished=false`, `stop_requested=true`. Перенос не возобновлялся.

## Ограничения проверки

1. Проверка читала текущие Git-ссылки, status, log, worktree metadata, каталоги и сохранённые текстовые записи. Git выполнялся с отключёнными optional locks, fsmonitor, untracked cache, auto maintenance/GC и lazy fetch. Checkout, fetch, commit, rebase, ref import и network Git не использовались.
2. Ни один сохранённый Python/PowerShell runner не исполнялся. Не запускались продукт, тесты, сборки, установка, загрузка зависимостей, CI (непрерывная интеграция) или платные сервисы.
3. «Чистая копия» означает пустой porcelain status с включёнными обычными untracked-файлами. Это не доказывает неизменность игнорируемых каталогов и отсутствие активности другого чата между наблюдениями. Свежая активация отчёта подтверждает необходимость сохранять эту отдельную линию.
4. Сохранённые PASS/FAIL и SHA-256 из документов остаются историческими утверждениями авторов соответствующих receipts. Их полная независимая реверификация здесь не проводилась. Наша свежая проверка подтверждает местонахождение, ссылки, состояние Git и прямые v2-флаги.
5. Публикация соответствующих локальных коммитов на GitHub этим подзаданием не проверялась. Root владеет GitHub-операциями. PR (запрос на включение изменений) не создавался, не менялся и не сливался.
6. На Comet первый запуск стандартной PowerShell оболочки пытался загрузить профили; системная политика отказала. Политика не менялась. Последующие команды использовали `cmd` и PowerShell `-NoProfile -NonInteractive`.

## Локальные доказательства

- `current-observed.json` — нормализованные текущие Git-наблюдения и три непосредственно прочитанных документа v2.
- `remote-read-evidence.json` — исходные ответы удалённых read-only инструментов с устройствами, временными отметками и маркерами частичного чтения, включая отчёт и recovery-квитанции.

Эти файлы — промежуточные материалы R25 для root. Удалённые рабочие копии, архивы, результаты и активный v2 не изменены.


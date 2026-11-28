# Source5: DATA, composition и фактическая проверка перед запуском

**Приняты PASS перепривязки DATA и двух случаев квалификации композиции. Нативная запись RUNNING подтверждает прохождение проверок перед запуском full11. Итог полной регрессии в этом срезе не получен.** Частная тестовая композиция: `001648f44bf6ef0191f1e21d92f1aecf2a2a3022`; исходный Product: `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`. Публичные PR heads имеют отдельные идентичности, и PASS на них не переносится. [1–13]

## Оглавление

1. [Перепривязка DATA](#перепривязка-data)
2. [Два случая композиции](#два-случая-композиции)
3. [Финальная привязка и RUNNING](#финальная-привязка-и-running)
4. [Границы результата](#границы-результата)
5. [Источники](#источники)

## Перепривязка DATA

Запуск `serena-evidence-rebinding-v11`, выполненный issuer/parent версии v12, завершён с **кодом 0 за 41.179 с**. Исходный родитель получил завершение процесса и закрыл оба файла вывода. Приняты точные original/result/output связи. [1–3]

Для default и state локально проверены точные копии исходных fixtures, которые root отдельно перечитал на нативной стороне. В каждом JSON изменено **единственное поле `evidence_parent`**. Идентичности исходных образов, Product, source epoch, каталогов, инструментов и seed-артефактов сохранены. Записи создания двух новых родителей d11/s11 приняты по исходным идентификаторам. [2,3,11]

**Таблица 1. Исходные и новые fixtures.** Полные пути локальных копий приведены в JSON-версии этого отчёта.

| Профиль | SHA-256 исходного fixture | SHA-256 нового fixture | Новый родитель |
| --- | --- | --- | --- |
| default | `af37fdf488e76089fe8a2a9ff073db77e160c63360384ee6fef263bee49d719a` | `7e963e574401820e7502f61bfddd9ea2867844f4920597c9f930c0f4c2b7e0d9` | d11 |
| state | `583ecfd7859d903a0e3835cc150075164f08660d9258aa2b66859cce20416b52` | `e3349958d50c928f0c4c9cd32a112cb3e81b18a6cf97d595c547fe46d364a143` | s11 |

DATA-результат связан с точным grant и update2. Карта Product осталась прежней, а отдельная тестовая идентичность указывает на `001648…`; эти две идентичности не смешаны. Этот этап выполнил **0 команд и 0 тестов**, не перекомпилировал артефакты и не выдал нового cache handoff. Он не читал физическое дерево consumer Source5. Наличие записей первоначального создания d11/s11 не является утверждением об их текущей пустоте во время полного прогона. [2,3,10,11]

## Два случая композиции

Контроллер композиции завершён с **кодом 0 за 4.731 с** при прежнем watchdog 60 с. Исходный процесс получен, файлы вывода закрыты, все исходные работники завершены. Приёмка выполнила полный импорт driver и owner runtime, приняла Source5, grant и фактический путь `.scratch` внутри Source5. Шесть закреплённых импортированных helpers совпадают с соответствующими файлами финального manifest. [4–8,11]

**Таблица 2. Фактические два случая.** Это управляемые сценарии отказа на реальном Windows owner; они не заменяют полную регрессию.

| Случай | Результат | Подтверждённая граница |
| --- | --- | --- |
| `begin-registration-before-first-command` | PASS | Тот же фактический Run зарегистрирован до первой команды; исходная ошибка сохранена; нативных команд в этом случае нет |
| `outer-run-finalization-retains-original-workers` | PASS | Исходный Run удержан до завершения читателей; выполнена одна физическая приёмка завершения; FAIL неизменен, raw не измерен, последующий запуск отклонён, исходная ошибка сохранена |

Совместимость Product/TestSource принята для 3508 файлов: 13 тестовых изменений, 3495 неизменённых путей, все 2012 нетестовых путей неизменны. Прежняя матрица 14 случаев владения процессами и квалификация Serena этим этапом не повторялись. [5,7,11]

## Финальная привязка и RUNNING

Все **8 файлов manifest**, grant и qualifier побайтно неизменны между composition и финальным набором. Проверены **19 точных входных связей** и сохранность **184 определений функций runtime**. Финальное изменение parent ограничено `REVIEW_COMPLETE` и хешем manifest; у manifest изменены статус и два хеша полученных composition controls. Команды и бюджеты остались прежними. [8,9,11]

Обязательные команды полного прогона:

```text
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

Получена нативная запись parent с `state=running` и исходным **PID 482868**. Она содержит exact manifest `15756…`, composition root `ddd262…`, cases `ab07…`, runner `ed5c…`, Source `001648…` и DATA-связки. В закреплённом коде parent проверка байтов root/cases/runner и их содержимого выполняется до `Popen`; запись RUNNING сохраняется только после успешного создания исходного процесса. Поэтому этот срез подтверждает **фактическое прохождение проверок перед запуском**. [8,9,12,13]

В той же записи `terminal_exit_received=false` и `parent_capture_files_closed=false`; кода завершения и итогового дочернего receipt в ней нет. Из RUNNING не выводятся завершение, отсутствие текущих дочерних процессов или PASS любой полной команды. Более поздний прогресс build/vet/tests не включён в этот ограниченный срез. [12,13]

## Границы результата

Первоначальная локальная копия composition root восстановлена по точному закреплённому writer: UTF-8, LF, отступ 2 и завершающий LF. Нативный digest cases совпал. Отдельная PowerShell-проверка хеша осталась **NOT_RUN** из-за тайм-аута проверки команды; новый процесс не стартовал. Позднейшая нативная запись RUNNING добавляет отдельное доказательство: parent уже успешно проверил фактические root/cases/runner SHA перед запуском. Прежнее ревью `51785…` сохранено, а изменение границы доказательств оформлено отдельным supplement. [4–7,9,11–13]

Raw-копию нативного parent получил root; локальная проверка принимает её точные байты и связи, без нового удалённого запроса. JSON разобран Python без потери точности целых чисел и без JavaScript round-trip. Оригинальные controls не пересериализованы. Исходные fixtures проверены как локальные JSON-копии; физические экспортные артефакты и дерево Source5 автор повторно не читал. [11–13]

Этот отчёт не закрывает КТ-2 или итоговую интеграцию: нужны итоговые оригинальные parent/receipt и PASS обеих полных команд. Source4 сохраняет CLOSED_FAIL, full-v10 — FAIL, исходная удерживаемая область — UNKNOWN. Замороженные Source5/update2 документы, основной план и оперативные файлы не изменены. [11–13]

## Источники

Пути относительны локальному рабочему набору. Хеши относятся к указанным исходным файлам; этот отчёт является отдельной проекцией.

**[1] data_parent.** `recovery/rebinding-v12-preparation/actual/parent.json` — SHA-256 `1811aa6a2d84de0c3665bc61774167620b29f3871aa78d337c415df2f331b747`.

**[2] data_result.** `recovery/rebinding-v12-preparation/actual/rebinding.json` — SHA-256 `33bdcf7b39a6d5813f5ef488706a52628ddcb862c9ba269866589914139973e7`.

**[3] data_root_review.** `recovery/rebinding-v12-preparation/actual/root-review.json` — SHA-256 `e0071567efb7b381e6483a9acdc7ed4c4970387fdd432197f7599c116ace8a78`.

**[4] composition_root.** `recovery/runtime-v14-preparation-v1/composition-actual/root-receiving.json` — SHA-256 `ddd2628a66e7d54b93538bd6ff03341ffde82cd90548f1a540da481d62a4abc4`.

**[5] composition_cases.** `recovery/runtime-v14-preparation-v1/composition-actual/cases.json` — SHA-256 `ab07f3e5be0bfed53e884dceabc1ef69c8ac50725eb850189dd12d286fcd49db`.

**[6] composition_launch.** `recovery/runtime-v14-preparation-v1/composition-actual/launch.json` — SHA-256 `a2367501d8337fc322ff2abd7c4e4ffa61a394fff1fad62b4670d7285468133a`.

**[7] composition_runner.** `recovery/runtime-v14-preparation-v1/bound-final-v1/composition/qualify_runtime_custody.py` — SHA-256 `ed5cf35884ad0821345234159ea31ceea1ae928e067de372169fe4b9e1cc9a54`.

**[8] final_manifest.** `recovery/runtime-v14-preparation-v1/bound-final-v1/actual-manifest.json` — SHA-256 `15756f71977804cb683ac3cc27f15b7eae1c2b869d28db94c2f501816ee807c8`.

**[9] final_parent_code.** `recovery/runtime-v14-preparation-v1/bound-final-v1/launch_literal_final_v14.py` — SHA-256 `b031831ac7ca2425b970df6afb01a69382a6e5d1cd83c7910a6e2a4177612faa`.

**[10] retained_grant.** `recovery/runtime-v14-preparation-v1/bound-final-v1/runtime/retained-input-access.json` — SHA-256 `e24d58882397ffdbcdb41bb20a4f12921f0c10d66329b641809c9ed4b81d0aec`.

**[11] independent_actual_review.** `recovery/runtime-v14-preparation-v1/independent-actual-handoff-review.json` — SHA-256 `51785e887eb11808bf5814f354da5c3d6ab006a95c2dc3a9a2e20a60852e4a12`.

**[12] native_prelaunch_supplement.** `recovery/runtime-v14-preparation-v1/independent-native-prelaunch-supplement.json` — SHA-256 `045fef79b41cf0aa24768c97fd0e624027fd77c59c5a816f0830e025eec49834`.

**[13] native_running_control.** `recovery/full-v11/parent-running-observation.json` — SHA-256 `7c8118b70db47baf56e58d4a1a58b80538c488d098d3441f7bf0226dd223f8c0`.

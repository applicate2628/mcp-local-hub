# #637: подготовка parsed-install и original interlock

Дата: 2026-11-25. Это продолжение существующего #637 поверх `8fd3c20592655c03461676d95c41ba2993e81b61`. Документ описывает применённые 17 source-файлов и отдельные закрытые проверки полной композиции. Он не создаёт второго плана; текущий план остаётся в #664: `.plans/2026-11/audit-remediation-20261125.md`.

## Состав и границы

Порядок source-проекции: `637-prepared.patch` → `637-observed-interlock-v2.patch` → gofmt → `637-transfer10-v3.patch` → gofmt. Исходные патчи и промежуточные результаты сохранены [1–3]. После последнего шага получены точные 17 postimages; независимый readback текущих owner-файлов совпал с этими хешами [4].

Подготовка сохраняет исходный API/publication owner между package preparation без supervisor interlock и последующим выбором workspace под registry lock. Фактический consumer получает окончательный набор дескрипторов и исходный bounded context. Миграция использует существующий `StateFileObservedLease`: bypass принадлежит точному вошедшему borrow, закрытие принимается от исходного lease, UNKNOWN либо прежний poisoned owner запрещает автоматический recovery. Старый GUI auto-register caller в этом наборе ещё не исправлен; его последующее исправление и original capture composition имеют отдельные review-пакеты.

Transfer10 убирает десять прежних API-positive roots, вызывавших обычный `NewAPI` без требуемой prepared assembly. Все шесть удаляемых блоков совпадают с рассмотренными исходниками; положительные требования перенесены в существующие qualified migration cases. Это изменение владельца проверки, а не доказательство выполнения перенесённых native сценариев. Основной private inventory сохраняет 14 migration roots; расширенные negative controls требуют согласованного producer/receiver inventory.

Три source-файла owner-проекции намеренно отличаются от полных integration blobs: `install_parsed_manifest.go`, `supervisor_lock.go`, `private_serena_migration_case_windows_test.go`. Чужие API/lifecycle и fixture-loader изменения не скопированы ради совпадения файла. Зависимости #659/#660 и подготовленные private compiler/runtime inputs должны прийти через закреплённую композицию #664; отдельная собираемость всего #637 этим переносом не заявляется.

## Принятые scoped-результаты

Scoped означает запуск явного списка сценариев. Root — корневой Go-тест; terminal включает также подслучаи. Числа root и terminal не складываются. Все строки таблицы 1 относятся к integration `762b5b32bbe590a53961dc9e47d98ab8065111dd`, run `p02-scoped-762b5-1` [5]. Ни один PASS не переносится на новый source commit автоматически.

**Таблица 1. Закрытые результаты исходного scope.**

| Пакет / режим | Roots PASS | Все terminal PASS | Время команды, s | FAIL / SKIP |
|---|---:|---:|---:|---:|
| API, default | 12 | 18 | 45.2275665 | 0 / 0 |
| CLI, default | 42 | 84 | 82.3442240 | 0 / 0 |
| API, `test_state_path_env` | 12 | 18 | 29.2527171 | 0 / 0 |
| CLI, `test_state_path_env` | 42 | 84 | 34.0992245 | 0 / 0 |

Исходный controller завершён с exit 0 за 191.2678187 s. Приняты все пять original commands, включая HEAD; у каждого завершены native process/Job, handles, оба потока и readers. Custody не оставлена UNKNOWN. Четыре полных stdout и три source/runtime receipts независимо прочитаны, их SHA-256 совпали с исходными квитанциями [4–5]. Source apply/format также закрыты: 11 commands для prepared/interlock и 8 для Transfer10; это source-операции, а не дополнительные native acceptance.

**Private prepared migration lanes, реальный provider/live Serena и Product acceptance — NOT_RUN в этом scope.** Тесты с моделируемыми callback outcomes подтверждают только соответствующее ветвление; реальные lower-owner проверки сохраняют свою точную область. Последующие auto-register/capture/vendor изменения не включены в эти 17 файлов и требуют собственного coherent Source и отдельной квитанции.

Перед freeze исходный owner должен заново проверить Git HEAD, ровно ожидаемые 17 изменённых source-путей плюс этот документ, все postimage hashes и `git diff --check`. Read-only подготовка документа не выполняла commit, push, тесты или установленный Product.

## Источники

1. `637-prepared.patch`, SHA-256 `45cbfadd381de1d83d64d037b74965f9b15d02f53a271123093f53a952340731`.
2. `637-observed-interlock-v2.patch`, SHA-256 `3871122f1207987a48d4a538804f4f68ebe1740cf0f1ffae07bf7485143c8a5e`; source apply receipt `pr637-prepared-interlock-source-1/receipt.json`, SHA-256 `6e09ac33804ab828508a402b302b6e802f1b9416bb238a2ab5d54252bcb781de`.
3. `637-transfer10-v3.patch`, SHA-256 `13d9ff8df7535db8a6c5aa2ab32782dda8e8e65fdd6417107483d1b05142e595`; source apply receipt `pr637-transfer10-source-1/receipt.json`, SHA-256 `3425eb9e75265a17786880f99ab4c0b503e9774a6542f65524787b3908a5e599`.
4. `pr637-freeze-input-verification.json`: точные 17 postimages и независимое получение закрытых source/runtime receipts и четырёх stdout; DATA для проверки freeze, не новый lifecycle controller.
5. `p02-scoped-762b5-1/receipt.json`, SHA-256 `3c9ee5f0ddff3d4ec2b3daf456dcb108b5ba2dfeea2b4f842a403b81dd54611d`; исходные stdout hashes приведены в verification JSON [4]. Исторические full FAIL и отдельные private/native gaps остаются в #664.

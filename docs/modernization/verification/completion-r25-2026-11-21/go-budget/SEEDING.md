# R25: узкая правка подготовки невалидных входов mutation table

> **Исторический кандидат, отклонённый для публикации.** Этот документ сохраняет первоначальное обоснование обычной записи файлов. После измерений ускорение не подтвердилось; окончательный #661 сохраняет `WriteStateFileAtomic`. Актуальное решение — [VALIDATION-ORACLE.md](VALIDATION-ORACLE.md).

## Оглавление

1. [Решение и владелец](#1-решение-и-владелец)
2. [Проверяемая ответственность](#2-проверяемая-ответственность)
3. [Точные ожидаемые ошибки](#3-точные-ожидаемые-ошибки)
4. [Совместимость файловой фикстуры](#4-совместимость-файловой-фикстуры)
5. [Кандидат и безопасная выборка](#5-кандидат-и-безопасная-выборка)
6. [Собственное ревью](#6-собственное-ревью)

## 1. Решение и владелец

Подготовлен **test-only candidate**, который заменяет только создание 30 невалидных файловых входов `TestMCPFrontV3_SerenaConflictClassifierMutationTable`: тот же `json.MarshalIndent`, затем `os.WriteFile(..., 0o600)` в отдельной прежней `apitest.HardenedTempDir`. Одновременно два отрицательных утверждения усилены: read и rollback обязаны вернуть конкретную семантическую ошибку данного случая. Произвольная ошибка доступа или чтения больше не удовлетворяет проверке.

Основа кандидата — **master `a7d92d2852c4f6eb8afb0569ef76a545e38aacf7`**. Файл `internal/cli/install_reconcile_mcp_front_v3_test.go` имеет blob `8cd1aabdaa136403d37352361092fe612fc14844` на master, current #637 и current #658. Diff каждой из этих двух PR-веток от общего предка master не меняет данный файл. Последняя правка файла — `64b0eddb3a86369a107b03596f795b84882b6b07`, уже предок master; более раннее происхождение — #588. #637 владеет другими Serena migration/qualification fixtures, а не этой mutation table.

Поэтому candidate является самостоятельной небольшой тестовой ответственностью от master. Его не следует вставлять в #637 или #658 ради сокращения числа PR. Ветку и PR эта работа не создавала.

Состояние: исходники и патч проверены статически; **gofmt, компиляция и нативное выполнение кандидата ещё не выполнялись**. Совместимость Windows-файла с реальным reader должна подтвердиться точным запуском владельца runtime. Ускорение кандидата пока не измерено. Предшествующее измерение исходной таблицы находится в [RUNTIME.md](RUNTIME.md).

## 2. Проверяемая ответственность

Текущий код сначала проверяет явный ожидаемый класс каждого из 34 входов. Только при ожидаемом `mcpFrontSerenaConflictInvalid` он создаёт новый отдельный `recovery.json`, вызывает reader, затем rollback и требует отсутствие вызова pin-reader. Между созданием и чтением файла нет конкурирующего потребителя, рестарта или имитации аварии. Исходный тест не проверяет атомарную публикацию именно этих 30 файлов или их сохранность после сбоя. Источник: [исходная mutation table](https://github.com/applicate2628/mcp-local-hub/blob/2d12d9b63e60387591d27515aef3487c0e3ea560/internal/cli/install_reconcile_mcp_front_v3_test.go).

**Таблица 1. Изменяемый механизм и сохраняемые проверки.**

| Часть сценария | После candidate |
|---|---|
| 34 имени, порядок, мутации и ожидаемые классы | Сохранены |
| 4 допустимых случая | Как прежде завершаются после проверки классификатора |
| Общий `v3Journal` и его `persist` | Сохранён реальный production-путь надёжной записи |
| `finishSerenaAttempt` общей исходной записи | Сохранён реальный production-путь надёжной записи |
| Создание 30 невалидных документов | Тот же MarshalIndent; прямая запись только нового тестового файла |
| `readMCPFrontReconcileReport` | Тот же настоящий reader, decoder и validator; дополнительно ожидается семантическая ошибка |
| `runRollbackMCPFrontWithReader` | Тот же настоящий вход отката; дополнительно ожидается та же семантическая причина отказа |
| `readerCalls == 0` | Сохранено |
| Production `WriteStateFileAtomic`, secure writer и FlushFileBuffers | Без изменений |

Уменьшается число побочных production-вызовов записи при подготовке отрицательных входов. Специальные тесты настоящих durable journal transitions не меняются. В частности, существующий `TestMCPFrontV3_MalformedPreconditionConflictDispositionRefusesBeforePinReadOrWrite` в том же файле продолжает сохранять изменённый журнал через `journal.persist`, проверять причину отказа и неизменность его байтов после отката; candidate его не затрагивает.

## 3. Точные ожидаемые ошибки

`readMCPFrontReconcileReport` выполняет anchored read → strict JSON decode → `validateMCPFrontReconcileReport`. `runRollbackMCPFrontWithOps` выполняет те же этапы до восстановления, settlement и pin-reader. Ошибки чтения и разбора возвращаются отдельно; semantic validator возвращает обычные `fmt.Errorf`. Для рассматриваемых ветвей нет общего типизированного error или sentinel, который можно было бы корректно проверить через `errors.Is`.

Прежний `err != nil` был недостаточен: например, отказ DACL, ошибки открытия и декодирования удовлетворяли ему, даже если до нужного domain validator исполнение не дошло. Candidate проверяет стабильный специфичный фрагмент сообщения в обоих вызовах. Это не новый production-код ошибки и не изменение API. Источники: [reader, validator и rollback](https://github.com/applicate2628/mcp-local-hub/blob/2d12d9b63e60387591d27515aef3487c0e3ea560/internal/cli/install_reconcile_mcp_front.go), [полное отображение 30 случаев](seeding-contract.json).

**Таблица 2. Семантические причины отказа, установленные по порядку проверок исходников.**

| Случаи | Число | Ожидаемый фрагмент и его владелец |
|---|---:|---|
| Остальные невалидные изменения authority shape | 20 | `has an invalid authority shape`; validator после `classifyMCPFrontSerenaConflict` |
| `applied-without-pin-is-invalid`, `retained-no-receipt-without-pin-is-invalid` | 2 | `has mismatched pin binding`; `requireMCPFrontSerenaPinBinding`, раньше classifier gate |
| `surface-is-invalid` | 1 | `carries a serena pin binding`; активная LSP-операция не может содержать Serena binding |
| `client-is-invalid`, `language-is-invalid`, `row-identity-is-invalid` | 3 | `does not match its exact identity`; ключ строки не соответствует её данным |
| `active-plan-generation-is-invalid` | 1 | `active plan generation 2 does not match artifact generation 1` |
| `report-generation-is-invalid` | 1 | `active plan generation 1 does not match artifact generation 2` |
| `active-plan-row-key-is-invalid` | 1 | `active generation operation references unplanned row "other"` |
| `active-plan-operation-is-invalid` | 1 | `serena operation "remove" is not supported`; `validateMCPFrontOperation` |

Все 30 вариантов получены из исходников, а не из исполнения кандидата. Особые 10 причин указаны явно в таблице Go; оставшиеся 20 используют общий фрагмент `has an invalid authority shape`. Ошибки `open`, `not single-user safe`, неверный владелец, отсутствие файла, превышение размера и strict JSON decode не содержат этих фрагментов. Поэтому несовместимый файловый вход приведёт к отказу теста, а не к ложному PASS.

Это сильнее сравнения reader с ошибкой, вычисленной повторным вызовом того же validator: ожидаемые причины заданы данными теста и отдельно проверены по исходникам. Существующая независимая таблица ожидаемых classifier-классов остаётся основной семантической проверкой всех 34 случаев.

## 4. Совместимость файловой фикстуры

`api.WriteStateFileAtomic` сначала делает `json.MarshalIndent(payload, "", "  ")`, затем передаёт полученные байты в secure writer. Candidate использует тот же сериализатор и параметры. Новый файл создаётся только внутри новой директории данного подслучая; чтение начинается после успешного возврата `os.WriteFile`. В рассматриваемом reader нет требования наличия соседнего `.lock` как условия допуска чтения. Источники: [state-file helper](https://github.com/applicate2628/mcp-local-hub/blob/2d12d9b63e60387591d27515aef3487c0e3ea560/internal/api/state_file_helper.go), [экспортируемый anchored reader master](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/state_read_inode_anchor.go).

Важная граница Windows: `0o600` само по себе **не доказывает нужный DACL файла**. `HardenedTempDir` защищает родительский каталог и задаёт ACL с `NO_INHERITANCE`; из этого нельзя вывести наследование такого же ACL новым файлом. Его фактический допуск зависит от Windows security descriptor, используемого при создании файла, и проверяется настоящим reader. Источник: [HardenedTempDir](https://github.com/applicate2628/mcp-local-hub/blob/2d12d9b63e60387591d27515aef3487c0e3ea560/internal/api/apitest/hardened_tempdir_windows.go).

На master reader проверяет владельца и DACL родителя, открывает файл относительно удерживаемого родительского handle, отказывает reparse/directory-входам, затем проверяет владельца/DACL самого файла и читает байты из того же handle. Запись или административные права постороннего SID на файл отвергаются; strict policy остаётся действующей. Candidate ничего в этих проверках не меняет и не добавляет разрешающих policy-флагов. Если новый файл не пройдёт допуск, усиленные domain assertions выявят это. Источник: [Windows reader master](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/hub_mcp_state_read_inode_windows.go).

Следовательно, **структурная совместимость и корректность oracle подтверждены исходниками; успешный нативный допуск файла ещё не подтверждён**. Это оставшаяся обязательная проверка перед заявлением о готовности, а не основание заранее менять DACL-политику или вводить новый helper/API.

## 5. Кандидат и безопасная выборка

Файлы: [invalid-seeding.patch](invalid-seeding.patch), [полный candidate](../archive/README.md), [seeding-contract.json](seeding-contract.json). Патч меняет один существующий root: **+43 / −22 строки**, без импортов, новых зависимостей, production-файлов или дополнительного тестового слоя.

SHA-256:

- исходный файл: `0c02d2d971651ed037bfa8e184da26fa6e94849b9bcb1090b5ea0277eeeb2f47`;
- candidate: `129cfb1db6243af558261cc2899c172667b756529ae67ee2cc1cd3a5951b7e25`;
- патч: `81fda39721148d69efacc9b342bd4b3f02d23b683fe9eeecbc71412f993c61ca`.

Нативная квалификация должна использовать только прочитанную выборку и точный candidate поверх указанного master. При использовании существующего overlay-runner команда имеет вид:

```text
go test -overlay <pinned-candidate-overlay.json> -json -count=1 -p=1 -timeout=5m -run '^TestMCPFrontV3_SerenaConflictClassifierMutationTable$' ./internal/cli
```

Ожидается 35 `pass` для тестов плюс один `pass` пакета, без `fail`/`skip`, и код возврата 0. Непосредственный немодифицированный candidate не содержит диагностических маркеров: **190 фаз не являются требованием к этому запуску**. Если нужен новый фазовый замер, инструментирование должно быть отдельным явно учтённым overlay поверх candidate.

Обзор эффектов TestMain и выбранного пути сохранён в подготовительном [README.md](README.md). Сборка/запуск должны остаться в уже подготовленном изолированном окружении без загрузок и установки. Положительные нативные Serena migration fixtures в эту выборку не входят.

Для количественного сравнения до/после требуется согласованная база и одинаковое инструментирование. Уже полученный диагностический результат относится к #658, а candidate — к master: полные Go-деревья различаются в 20 файлах. В выбранном Windows reader разница состоит только из текста двух strict-DACL ошибок, и рассматриваемые gates одинаковы; тем не менее это разные полные входы сборки. Текущие 4.78 с и 4.2347134 с служат исходным диагностическим наблюдением; они не объявлены строгой контрольной половиной пары master/candidate. Новый контроль на master, если нужен для сравнения, решает именно эту конкретную разницу входов, а не обновляет старый результат ради свежести.

## 6. Собственное ревью

Статически проверено:

- `git apply --check` успешен на исходных байтах, совпадающих с master;
- вне выбранной функции байты файла не меняются; имена и порядок 34 подслучаев сохранены;
- реальные общие `v3Journal` и `finishSerenaAttempt`, все classifier-ожидания и `readerCalls == 0` сохранены;
- семантические ошибки для 10 особых случаев соответствуют более ранним validator-ветвям; остальные 20 доходят до отказа authority shape;
- оба чтения остаются реальными, включая Windows owner/DACL-проверки; отказ допуска не удовлетворит новому oracle;
- прямой seed не повторяет production-алгоритм атомарной записи и не создаёт нового API;
- новая локальная переменная `err` от MarshalIndent корректно переиспользуется при вызове rollback; область действия кратких `if err := ...` не меняет её.

Не выполнены: gofmt, компиляция, нативный допуск candidate и измерение после изменения. Нового окна «успешно без доказательств» не введено: если допуск или domain expectation отличаются, точная выборка должна отказать.

DACL — discretionary access control list, список управления доступом Windows; ACL — access control list. SID — security identifier, идентификатор субъекта безопасности. LSP — Language Server Protocol. JSON — JavaScript Object Notation. Handle — удерживаемый системный дескриптор объекта. Oracle здесь означает конкретное условие, по которому тест признаёт поведение корректным.


## Публикационная привязка ссылок

Относительные ссылки исходного рабочего отчёта приведены к неизменяемым источникам GitHub либо к описанию публичного архива. Файлы исполнения находятся внутри `archive/public.zip`; ссылки на контракт архива не означают отдельную публикацию каждого потока. Точные исходные имена и замены перечислены в [PUBLICATION-LINKS.json](../PUBLICATION-LINKS.json). Результаты и численные данные исходного отчёта при этом не изменены.

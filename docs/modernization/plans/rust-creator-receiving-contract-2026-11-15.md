# Принимающий контракт Rust identity у настоящего Go-владельца

**Дата:** 15 ноября 2026 года (МСК). **Статус:** принимающая граница проверена по исходникам; самостоятельный native/FFI query реализован и квалифицирован отдельно [8]. Продуктовый Rust backend не реализован и не принят этим документом.

**Связь с планом:** этап 1, R01/R03, [единственная доска остатка](rust-independent-execution-plan-2026-11-12.md#remainder). Этот документ уточняет принимающий контракт и условия проверки; отдельной очереди задач или дополнительного PR не создаёт.

## Оглавление

1. [Проблема и исходные версии](#problem)
2. [Выбранная граница и альтернативы](#decision)
3. [Владелец, наблюдение и квитанция](#contract)
4. [Сохранение поведения и проверяемые различия](#verification)
5. [Порядок следующей реализации](#implementation)
6. [Источники и термины](#sources)

<a id="problem"></a>
## 1. Проблема и исходные версии

Опубликованный #631 на `2e4bf6f983fbf1747ca286016a6eb485a834b852` квалифицирует `AdmitIdentity`: наблюдение исходного процесса внутри одноразового допуска с возможным последующим `ResumeThread`. Продуктовый `ProvisionalProcess.ObserveCreatorIdentity` имеет другую роль: получает наблюдение, проверяет исходного владельца и квитанцию форматирования SID, затем выпускает связанную с ним capability. Эта функция сама не выдаёт решение о resume и не расходует admission [1, 2].

Поэтому преобразование `IdentityObservation → CreatorIdentitySnapshot` не является достаточным адаптером. Callback, который только сохраняет поля и возвращает allow, меняет состояние процесса до возврата из нового «observer». Callback с deny расходует единственный допуск. Повторное наблюдение после уже состоявшегося admission через этот метод также невозможно. Это различие контрактов, а не дефект квалифицированного `AdmitIdentity`.

Теперь отдельный `mh_query_identity1` / `Owner.QueryIdentity()` реализует именно наблюдение в экспериментальной границе: без policy, resume и расходования admission. Native-код `7f99d400`, FFI-код `e69bc4c1`; результаты, исправленное замечание review и failed broad gates записаны в [8]. Это не устраняет перечисленные далее требования настоящего продуктового owner.

Продуктовые исходники прочитаны на `17bc78fc602efa204a74596409c939a3bf1ddc42`. Свежий master `a7d92d2852c4f6eb8afb0569ef76a545e38aacf7` содержит последующие параллельные изменения; точное сравнение не меняет перечисленные ниже runtime-файлы владельца, identity и formatter [7]. Нативный родитель — #644 `84119b51`, на #625 `c5672f49`. Перед будущей правкой принимающей области эту проверку требуется повторить.

**Таблица 1. Реальные зависимости принимающей стороны.**

| Место | Что проверяет или выполняет код | Следствие для Rust-подключения |
|---|---|---|
| `provisional.go::startProvisionalWithJobContext` | Создаёт настоящий `ProvisionalProcess` с owner/cmd/job и передаёт его retention callback до native creation | Custody должна быть сохранена до создания; ненулевой owner вместе с ошибкой нельзя потерять |
| `creator_identity.go::ObserveCreatorIdentity` | Проверяет context, owner/cmd/job, close/terminal state, `identityInFlight`, полноту наблюдения и действительную SID-квитанцию | Копия значений не создаёт capability и не заменяет правила жизненного цикла |
| `start_with_job_windows.go::creatorBinding` | Проверяет реальный cmd.Process, live Job/process/thread handles, at-create binding и тождество исходных объектов | Фиктивные cmd/Job или повторное открытие по PID недопустимы |
| `original_member_provisional.go::closeWithMemberPermitMode` | Проверяет permit и готовность, требует уже опубликованный root wait (иначе возвращает pending), закрывает owner и отдельно настоящий Go Job | Удаления проверки job != nil недостаточно; Rust-owned Job должен закрываться только своим owner |
| `provisionalWindowsChild.wait` | Сохраняет настоящий cmd.Wait, ProcessState и возможный `*exec.ExitError` | Числовой exit code Rust не даёт права изготовить эти Go-объекты |
| `SIDFormatObservation.ValidateInput` | Проверяет тождество input owner, действительность связанной квитанции и неизменность актуальных байтов | Совпадение SID-строки или digest не заменяет исходный буфер и private issuer |

<a id="decision"></a>
## 2. Выбранная граница и альтернативы

**Рекомендация по текущим исходникам:** сохранить существующий Go путь, квалифицированный `AdmitIdentity` и новый read-only query; реализовать закрытую границу реального владельца внутри `internal/process` вместе с настоящим Rust-адаптером. Подключение backend допускается после проверки всех используемых lifecycle-возможностей.

**Таблица 2. Рассмотренные варианты.**

| Вариант | Преимущество | Риск / ограничение | Решение этого разбора |
|---|---|---|---|
| Сохранить текущий Go owner | Действующие identity, receipt, wait и retirement остаются целыми | Сам по себе не подключает Rust и не доказывает выгоду переноса | Сохраняется как действующая реализация и исходный эталон |
| Вызвать AdmitIdentity из product observer и переложить поля | Мало нового кода | Observer расходует admission и может вызвать resume; нет настоящей capability/receipt | Не подходит по семантике |
| Сделать cmd/Job из PID или открыть process повторно | Внешне удовлетворяет текущим полям структуры | Иной ресурс, второй владелец, несоответствие at-create/close/wait контрактам | Не допускается |
| Private owner/custody contract и отдельное read-only наблюдение | Сохраняет единственного владельца и позволяет отдельно квалифицировать backend | Нужны явные lifecycle/exit/member/cleanup соответствия и тесты принимающей стороны | Рекомендуется; конечные сигнатуры ещё не объявлены стабильным API |

Новый публичный конструктор `CreatorIdentity` из snapshot, digest, PID или SID не нужен. Обобщать все process runners и вводить реестр владельцев для этой задачи также не требуется. Существующие непубличные observer/terminal interfaces следует переиспользовать там, где их контракт совпадает.

<a id="contract"></a>
## 3. Владелец, наблюдение и квитанция

### 3.1. Сохранённый владелец до создания

Ближайшая продуктовая граница — узкое lifecycle-подмножество нынешнего `containedChild`: ожидание, terminal action, observation и закрытие у того же настоящего owner. Привязанный к `*exec.Cmd` метод `start` не обязан входить в этот общий жизненный цикл [3].

Go implementation сохраняет реальные cmd/Job, стандартное ожидание и свои текущие проверки. Будущая Rust implementation удерживает собственный native owner. Верхний `ProvisionalProcess` проверяет наличие исходной private capability, а не искусственно заполненные поля cmd/job.

Retention должен произойти до native creation, как сейчас. После передачи любых native ресурсов тот же owner возвращается и при ошибке, включая неизвестный поздний результат. Статус «процесс не создавался» допустим только при доказанном отсутствии передачи ресурсов/эффекта. Закрытие Rust-owned Job через дополнительный `p.job.Close()` недопустимо.

Публичный `StartProvisionalWithJob(job, cmd, …)` продолжает обслуживать переданный настоящий Go Job. Его нельзя молча переключить на Rust, создающий другой Job. Действующий контракт и принимающая композиция будущего Rust owner должны различаться явно, без фиктивного wrapper.

### 3.2. Отдельное наблюдение без допуска

Необходимая новая native-операция читает факты через тот же retained process и конкретный at-create Job. Она:

- не вызывает ResumeThread, не принимает policy decision и не расходует admission;
- может повторно наблюдать живой retained owner в состояниях, разрешённых принимающим lifecycle, в том числе после запуска;
- сохраняет отдельные query и token-close errors; успешный SID требует успешного закрытия временного token;
- передаёт проверяемые schema/version/length и Go-owned копии без process/token handles;
- не удерживает callback или чужой указатель после возврата.

Существующий admission export не меняет смысла. Возможность нового read-only query проверяется явно; недоступность не разрешает автоматический вызов admission или реконструкцию процесса по PID.

Продуктовый context проверяется до входа и после наблюдений. Ожидание доступа к owner должно соблюдать context; отмена наблюдателя не означает отмену уже выполняющегося синхронного Windows-вызова и не выдаётся за доказанное отсутствие эффекта. Полученные после отмены значения не выпускают новую capability.

### 3.3. Настоящая SIDFormatObservation

**Таблица 3. Передача результата без подмены полномочий.**

| Шаг | Реальный вход и операция | Сохраняемый результат |
|---|---|---|
| Native query | Исходный retained process, запрос TokenUser и явное закрытие token | Проверенный бинарный SID либо отдельные query/close errors |
| Принимающий private owner | Собственный проверенный и выровненный Go SID buffer, без изменяемых внешних aliases | Удерживаемый input owner; его происхождение связано с тем же native owner |
| Существующий formatter | `FormatSIDObserved` на этом SID: conversion, decode, явный LocalFree | Настоящая квитанция с private issuer и отдельными primary/cleanup outcomes |
| Проверка входа | `ValidateInput` с тем же `*windows.SID` и полученной строкой | Подтверждение тождества input owner и неизменности его байтов |
| Product observer | Проверки фактов, context/custody и квитанции | `CreatorIdentity`, связанная с исходным `ProvisionalProcess` |

Go буфер должен жить столько, сколько квитанция может повторно читать его через `inputRead`. Передать публичный изменяемый `IdentityObservation.TokenUserSID` в будущий конструктор полномочий недостаточно; принимающий owner получает и удерживает свои наблюдения.

Квитанция подтверждает форматирование конкретного входа. Происхождение SID от исходного процесса подтверждает отдельно закрытая граница владельца. Её нельзя восстановить из строки, snapshot, SHA-256 или флага COMPLETE. Ошибка преобразования, декодирования либо освобождения запрещает успешную capability; исходные и cleanup ошибки сохраняются раздельно [4].

### 3.4. Custody и retirement

Действующие `identityInFlight`, `closeAttempted` и `terminalStarted` остаются условиями входа. Query получает временное право использования в существующем механизме `originalMemberCustody`. После закрытия входа новые наблюдения запрещены. Перед выдачей permit terminal entry требует завершения уже вошедших операций: ожидает в пределах переданного context либо возвращает pending. Permit принадлежит именно этому owner [5].

Срок завершения остаётся одним абсолютным deadline: повторный запрос не создаёт новый интервал. Закрытие original handles запрещено до возврата исходного root waiter и публикации его результата. Native entry, terminal request, root exit, завершение текущих членов и ошибки закрытия остаются разными фактами.

Одна mutex в экспериментальной Go FFI не реализует весь этот протокол. Полный member/terminal outcome и настоящие Go wait errors также не выводятся из нынешнего `mh_out.COMPLETE` [3, 5].

<a id="verification"></a>
## 4. Сохранение поведения и проверяемые различия

**Таблица 4. Необходимые различающие проверки следующей реализации; пока не исполнены для нового адаптера.**

| Область | Положительный результат | Отрицательное различие |
|---|---|---|
| Подготовка owner | Retention получает тот же owner до создания; он сохраняется при поздней ошибке | Заменённый owner и потерянная partial custody отвергаются |
| Read-only query | Несколько наблюдений до/после допустимого запуска; исходные PID/birth/Job сохраняются | Query не вызывает resume и не расходует admission; отказ query не превращается в разрешение |
| Context и занятый slot | Допустимый query освобождает slot на любом возврате | Отмена до входа/после native query и параллельный identity query не выпускают capability |
| SID input | Реальный formatter, тот же удерживаемый буфер, настоящая квитанция | Изменённые байты, иной input owner с тем же SID, отсутствие private issuer и подмена capability диагностическим snapshot отвергаются; принимающий owner не принимает квитанцию чужого входа |
| Ошибки formatter | Conversion/decode/free успехи подтверждены отдельно | Query, conversion, decode и free failures не превращаются в успешный SID или capability |
| Retirement | Уже вошедшее наблюдение завершается до закрытия owner | Новые query после seal и permit другого owner отвергаются; deadline не продлевается |
| Wait/terminal/close | Реальный Go путь сохраняет RawExitError; root/member/action/close исходы независимы | Кешированное, частичное или неизвестное завершение не становится новым успешным действием |
| Совместимость | Прежний Go starter и caller-owned Job работают с тем же смыслом | Нет скрытого второго Job, fallback к admission и нового владельца по PID |

Нужны проверки поведения принимающей стороны и реальные native-прогоны там, где меняется граница ресурсов. Моки или успешная компиляция сами по себе не подтверждают Windows lifetime. Новая квалификация query [8] относится к самостоятельному native/FFI примитиву; она не объявляется результатом будущего продуктового адаптера или backend.

<a id="implementation"></a>
## 5. Порядок следующей реализации

**Завершён самостоятельный результат:** отдельный read-only native/FFI query в продолжениях существующих #644/#631 [8]. Admission API сохранён; отсутствие скрытого resume/расходования допуска проверено настоящим последующим admission и повреждённым routing control.

Ponytail Code Simplification и Architecture Review уточнили порядок: query не зависит от предварительного формального выделения Go-интерфейса. Извлечение только wait/terminate/close не устраняет реальные cmd/Job gates, исходный wait и раздельное закрытие owner/Go Job. Поэтому private boundary следует реализовать вместе с настоящим адаптером, при сохранении текущего Go поведения.

В существующем этапе 1 остаются **два связанных результата**, без новых верхнеуровневых этапов:

1. **Реализовать private lifecycle/custody boundary вместе с настоящим Rust-адаптером.** Сохранить current API, retention-before-create, исходные cmd/Job/wait errors там, где действует Go, и все используемые Rust root/member/action/close outcomes. До изменения сверить актуальные master и параллельные PR; не дублировать их исправления.
2. **Квалифицировать принимающего потребителя с настоящей SID-квитанцией.** Получать её через действующий formatter на удерживаемом Go input, сохранить ownership/context/retirement и пройти таблицу 4 с самостоятельным и независимым ревью. Без этого успешный query не выпускает продуктовую capability.

Эти результаты принадлежат одной принимающей ответственности. Число PR определяется реальным scope; отдельные тесты, журналы либо формальное извлечение интерфейса не требуют дополнительных PR. Установка, merge, платная внешняя проверка и исключение из общих pre-push gates не следуют из этого порядка автоматически.

Решение о Rust backend можно пересмотреть, если новая версия принимающего owner уже устранила зависимости либо измерения не обосновывают перенос. Сохранение Go остаётся допустимым доказательным итогом.

<a id="sources"></a>
## 6. Источники и термины

1. [CreatorIdentity: capability и context/custody gate](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/creator_identity.go), [native identity observer и formatter](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/creator_identity_windows.go).
2. [Rust identity admission](https://github.com/applicate2628/mcp-local-hub/blob/2e4bf6f983fbf1747ca286016a6eb485a834b852/experiments/rust-process-owner/ffi/src/identity.rs), [Go вызывающая сторона](https://github.com/applicate2628/mcp-local-hub/blob/2e4bf6f983fbf1747ca286016a6eb485a834b852/experiments/rust-process-owner/ffi/go/bridge_windows.go).
3. [ProvisionalProcess и startup/wait](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/provisional.go), [containedChild](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/contained_stream.go), [реальная Windows binding](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/start_with_job_windows.go).
4. [SIDFormatObservation](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/sid_format_observation.go), [FormatSIDObserved и ValidateInput](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/sid_format_observation_windows.go).
5. [Original member custody](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/original_member_custody.go), [готовность Windows owner к закрытию](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/original_member_owner_windows.go), [provisional closure](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/original_member_provisional.go).
6. [Аудит опубликованного FFI, исправление runner и пределы доказательств](https://github.com/applicate2628/mcp-local-hub/blob/2e4bf6f983fbf1747ca286016a6eb485a834b852/experiments/rust-process-owner/ffi/verification/identity-2026-11-15/AUDIT.md).
7. [Изменения master между прочитанной основой и свежим снимком](https://github.com/applicate2628/mcp-local-hub/compare/17bc78fc602efa204a74596409c939a3bf1ddc42...a7d92d2852c4f6eb8afb0569ef76a545e38aacf7).
8. [Опубликованные результаты read-only native/FFI query, review и ограничения](https://github.com/applicate2628/mcp-local-hub/blob/2fd3f017a9e5ccd0460293d71ea34aceb62f245e/experiments/rust-process-owner/ffi/verification/query-2026-11-15/RESULTS.md), [native suite](https://github.com/applicate2628/mcp-local-hub/blob/71891067d0c24387ba873a3e4276b0462ae365ae/experiments/rust-process-owner/verification/query-2026-11-15/RESULTS.md). Результаты опубликованы в черновиках #631 (`2fd3f017`) и #644 (`71891067`); принимающий продуктовый адаптер и полные обязательные проверки остаются открытыми.

**Метод:** Architecture Review, API and Interface Design, Architecture Decisions и Go Testing and Verification; самостоятельное чтение исходных владельцев и отдельный read-only разбор. Самостоятельное и отдельное ревью уточнили неблокирующий close при неопубликованном wait и точную силу проверки SID issuer/input; в тексте нет ожидания, которого код не выполняет, или реестра issuer, которого код не содержит. Квалификация самостоятельного query записана отдельно в [8] и не заменяет проверку принимающего продукта.

**Термины:** capability — полномочие, связанное с настоящим владельцем; custody — сохранённое владение ресурсами и право их завершить; retention — передача владельца на удержание до создания; retirement/seal — запрет новых операций и завершение уже вошедших перед закрытием; permit — разрешение конкретного владельца на terminal/close; receipt — квитанция о реально выполненных операциях и их исходах; FFI — интерфейс между языками; ABI — двоичный интерфейс; SID — идентификатор безопасности Windows; PID — идентификатор процесса; Job — объект Windows, удерживающий группу процессов; raw handle — системный дескриптор; deadline — абсолютный срок окончания; SHA-256 — криптографический хеш; backend — реализация механизма за принимающим интерфейсом.


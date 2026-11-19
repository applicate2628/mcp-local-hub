# R17: аудит публичного Go-пути Scheduler

**Дата:** 19 ноября 2026 года. **Исходная версия:** [#660, `1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3`](https://github.com/applicate2628/mcp-local-hub/tree/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3).

**Область:** конструктор, `Status`, `ExportXML`, `List`/`ListContext`, запуск PowerShell, разбор JSON, ошибки, состояния и отмена. Это самостоятельное чтение исходников до исполнения новой Go-пробы; код продукта не изменён. Последующий ограниченный результат шести проверок записан отдельно в [отчёте об исполнении](RESULTS.md). Ранее выполненные искусственные контроли и семь нативных PowerShell-чтений сохраняют свою отдельную область доказательства.

## Содержание

- [1. Действующий путь](#1-действующий-путь)
- [2. Подтверждённые находки и ограничения](#2-подтверждённые-находки-и-ограничения)
- [3. Минимальные шесть проверок](#3-минимальные-шесть-проверок)
- [4. Остающаяся нативная квалификация](#4-остающаяся-нативная-квалификация)
- [5. Источники](#5-источники)

## 1. Действующий путь

`New()` при `MCPHUB_E2E_SCHEDULER=none` возвращает тестовую пустую реализацию. Обычный Windows-конструктор получает текущего пользователя, отбрасывает доменную часть имени и строит путь `schtasks.exe` из `SystemRoot`/`WINDIR`. Конструктор не проверяет доступность службы Scheduler или PowerShell. [Источник 1, строка 160](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler.go#L160); [источник 2, строка 31](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_windows.go#L31).

Чтения используют фиксированный PowerShell-сценарий через `powershell.exe -NoProfile -NonInteractive -Command`; executable разрешается по имени, запрос передаётся как JSON через stdin. Преамбула задаёт UTF-8; `NoConsole` устанавливает `HideWindow` и `CREATE_NO_WINDOW`. `cmd.Output()` отделяет stdout от stderr: непустой диагностический поток сам по себе не является отказом. [Источник 3, строка 115](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_com_windows.go#L115); источники 4 и 6.

`Status` и `ExportXML` используют фоновый контекст; `ListContext` принимает контекст вызывающего кода, заменяя `nil` фоновым. Общая обёртка задаёт предельный срок контекста 15 секунд. `Status` отвергает отсутствующий объект задачи, `ExportXML` — отсутствующий объект или пустое XML. Значения состояния 1/2/3/4 переводятся в Disabled/Queued/Ready/Running; остальные дают `ErrUnavailable`. `ExportXML` возвращает UTF-8-байты строки XML. [Источник 2, строка 241](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_windows.go#L241), [строка 350](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_windows.go#L350); [источник 3, строка 137](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_com_windows.go#L137).

Именованные чтения перечисляют скрытые и видимые задачи корня через `GetTasks(1)`; List сохраняет `GetTasks(0)` и фильтр префикса. После PowerShell Go дополнительно фильтрует список по учётной записи. Это дополнительная граница относительно прежней V2. [Источник 3, строка 91](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_com_windows.go#L91); [источник 2, строка 365](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_windows.go#L365).

## 2. Подтверждённые находки и ограничения

### Дефект данных: `LastResult` всегда равен нулю

Публичный `TaskStatus.LastResult` определён как результат последнего запуска либо `-1`, если запусков не было. PowerShell-ответ этого свойства не содержит; `taskStatusFromCOM` оставляет поле с нулевым значением Go. **При ненулевом результате последнего запуска или отсутствии запусков публичное значение неверно.** Оно выглядит как успешный код 0. Это подтверждённое расхождение исходников с контрактом поля, без заявления о нативном воспроизведении на Comet. `NextRun` также не заполняется, но отдельное обязательство его заполнения интерфейсом не установлено. [Источник 1, строка 56](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler.go#L56); [источник 3, строка 180](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_com_windows.go#L180).

### Неоднозначность фильтра принципала

Конструктор сохраняет короткое имя; `WindowsUsersEquivalent` отбрасывает доменную часть сравниваемой строки. Для текущего `DOMAIN_A\alice` строка `DOMAIN_B\alice` также совпадёт с `alice`. Если мост вернёт SID-строку текущего пользователя, она не совпадёт с его коротким именем. Это точные свойства сравнения; соответствующие нативные случаи на Comet не воспроизведены. Обход прав Scheduler или фактическое включение чужой задачи в список этим аудитом не доказаны. Здесь принципал означает учётную запись выполнения, а не владельца дескриптора безопасности. [Источник 2, строка 379](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_windows.go#L379); [источник 1, строка 84](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler.go#L84).

Последующее чтение пяти сохранённых положительных ответов V2, после проверки их исходных SHA-256, показало: значения `owner` не имели ни SID-префикса, ни разделителя домена. Приватные значения не раскрываются. Это характеристика тех сохранённых ответов; проблемные варианты SID/разных доменов и корректность положительного публичного List этим не проверены. Новых запросов к Планировщику для этого чтения не было. [Безопасная запись формы принципала](history.json).

### Диагностика отмены, ошибок и предельного срока

- После `cmd.Output()` любое ненулевое `ctx.Err()` превращается в `ErrUnavailable: timeout`; причины `context.Canceled` и `context.DeadlineExceeded` не сохраняются для `errors.Is`. `ContextLister` обещает отмену underlying-операции, но не эту форму ошибки. Без требований потребителей потеря причины классифицируется как диагностическое ограничение, а не доказанное нарушение обещания интерфейса. [Источник 3, строка 124](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_com_windows.go#L124); [источник 1, строка 142](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler.go#L142).
- COM-исключение всегда сериализуется как `scheduler_unavailable`; HRESULT передаётся, но публичный обработчик его не использует. Go-ветви `permission_denied`/`task_corrupt` сами по себе не доказывают классификацию нативных отказов: существующий тест этих ветвей подменяет runner. [Источник 3, строка 113](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_com_windows.go#L113), [строка 150](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_com_windows.go#L150); источник 5.
- `WaitDelay` не задан. `CommandContext` по умолчанию завершает непосредственный процесс; ожидание каналов ввода/вывода может продолжаться до их закрытия потомками. Строгое завершение всей операции и освобождение всех каналов за 15 секунд не доказаны. Предстартовая отмена этого пробела не закрывает. Источники 3 и 6.

## 3. Минимальные шесть проверок

Программа вызывает настоящий `New()` без подмены runner. До конструктора она отвергает `MCPHUB_E2E_SCHEDULER=none`; после него требует ненулевой Windows-backend без ошибки и наличие `ContextLister`. Используются уникальные строковые имя и префикс; задача с этим именем **не создаётся**. Префикс List не содержит начального `\`.

**Таблица 1. Условия приёмки, выбранные до исполнения; фактические результаты приведены в [отчёте](RESULTS.md).**

| № | Вызов | Условие приёмки |
|---|---|---|
| 1 | `Status(absentName)` | `errors.Is(err, ErrTaskNotFound)`; нулевой `TaskStatus` |
| 2 | `ExportXML(absentName)` | Та же ошибка; XML не возвращён |
| 3 | `List(uniquePrefix)` | Нет ошибки; ноль строк |
| 4 | `ListContext(nil, uniquePrefix)` | Нет ошибки; ноль строк |
| 5 | `ListContext(preCanceled, uniquePrefix)` | Контекст отменён до вызова; ненулевая ошибка; ноль строк |
| 6 | `ListContext(preExpired, uniquePrefix)` | Срок истёк до вызова; ненулевая ошибка; ноль строк |

Для строк 5–6 таблицы 1 отдельно фиксируются фактические `errors.Is` для `ErrUnavailable`, `context.Canceled` и `context.DeadlineExceeded`. На закреплённом коде ожидается `ErrUnavailable`; это наблюдаемая диагностика, а не новый постоянный контракт. Успешное пустое чтение не проверяет положительный фильтр учётной записи.

Квитанция должна связать исходник и исполняемую пробу, конструктор/backend, шесть исходов, длительности и наблюдённое завершение. Числовой код внешней команды указывается только при его фактическом получении. При неожиданном положительном ответе фиксируется отказ проверки без публикации XML или учётных данных. Новых `Create`/`ImportXML`/`Delete`/`Run`/`Stop`, установки, изменения execution policy или запуска lifecycle-теста таблица 1 не предусматривает.

## 4. Остающаяся нативная квалификация

Даже после подтверждённого успеха всех шести строк таблицы 1 остаются:

1. Положительные публичные `Status`/`ExportXML`/`List`: существующая видимая/скрытая задача, точность XML и принципала, числовые состояния и правильность состава списка. Подходящую уже существующую доступную задачу можно только читать; при её отсутствии пробел сохраняется без автоматического создания фикстуры.
2. Ненулевой результат последнего запуска и отсутствие запусков; корректировка `LastResult` требует отдельного изменения и проверки, данный probe её не выполняет.
3. Реальная матрица прав доступа, отказов чтения и недоступности службы; вложенные пути, регистр и остальные случаи Unicode. Исходная корневая область и чувствительное к регистру сравнение не расширены.
4. Отмена уже выполняющегося чтения, достижение внутреннего срока, завершение дочернего процесса и освобождение каналов. Два заранее завершённых контекста этих свойств не доказывают.
5. `RunOwned`, обычный запуск, Stop и производственный Delete; полные Go-критерии, устанавливаемый Product и остальные направления R17. Прежние полные FAIL сохраняются; шесть чтений их не заменяют.

Оба прежних точных допуска на recovery/V2 использованы. Нового разрешения на создание/удаление задач или изменение политики выполнения этот аудит не устанавливает. Исправление накопленных проблем не включается автоматически в ограниченный hidden-lookup PR.

## 5. Источники

1. [Публичные типы, фильтр пользователя, ContextLister и New — `scheduler.go`](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler.go).
2. [Конструктор и публичные Windows-методы — `scheduler_windows.go`](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_windows.go).
3. [PowerShell-сценарий, транспорт, ошибки и состояния — `scheduler_com_windows.go`](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_com_windows.go).
4. [Параметры создания процесса — `noconsole_windows.go`](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/process/noconsole_windows.go#L33).
5. [Подменённый runner и границы тестов — `scheduler_com_windows_test.go`](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_com_windows_test.go#L17).
6. [Официальный контракт Go `os/exec`: CommandContext, Output и WaitDelay](https://pkg.go.dev/os/exec#Cmd).

## Термины и сокращения

COM — Component Object Model, компонентная модель Windows; JSON — формат структурированных данных JavaScript Object Notation; XML — расширяемый язык разметки; SID — идентификатор безопасности Windows; HRESULT — 32-битный код результата Windows; stdin/stdout/stderr — стандартные потоки ввода/вывода/ошибок; PASS/FAIL — успешный/неуспешный результат конкретной проверки; Product — устанавливаемый комплект продукта.

# R17: контракты Task Scheduler и автозапуска перед удалением собственного COM

**Дата:** 19 ноября 2026 года (МСК). **Исходник:** `master a7d92d2852c4f6eb8afb0569ef76a545e38aacf7`; соответствующие 50 Git blob совпадают с документационной веткой #623 на `1e04fea613edc909b13c19f8dad4db5977b3bd69`. [Манифест исходников](scheduler-contracts-2026-11-19.sources.json).

**Результат этапа:** завершена карта восьми операций «контракт → действующая реализация → документированный системный интерфейс → недостающая гарантия», прослежены потребители и область существующих тестов. Установлены конкретные несоответствия создания, поиска скрытых задач, сохранности при замене и проекции состояния. Это дефекты и границы прочитанного существующего кода, а не регрессии ещё не выполненной замены COM. **Исполняемый код не изменён, исправления и удаление COM не реализованы; новых тестовых PASS/FAIL нет. R17 и этап 4 остаются открытыми.**

Первый кандидат на удаление собственного COM — чтение определения одной задачи через `ExportXML`, если системный командный интерфейс позволит сохранить обязательные исходы. Для `Status`, защищённого запуска и разрушительных действий готовность такого перехода не установлена. Go остаётся допустимой окончательной реализацией; новый язык, общий реестр процессов и новая система транзакций из этой карты не следуют. Следующие исправления и критерии проверки приведены в [§8](#next).

## Оглавление

1. [Область и доказательства](#scope)
2. [Восемь операций](#operations)
3. [Расхождения платформенного слоя](#findings)
4. [Дефекты у потребителей](#consumers)
5. [Идентичность, запуск и завершение](#identity)
6. [Автозапуск: OFF, завершение, выход пользователя](#lifecycle)
7. [Что проверяют существующие тесты](#evidence)
8. [Решение и следующий ограниченный контракт](#next)
9. [Проверки этого этапа и незавершённая квитанция R16](#verification)
10. [Источники](#references)
11. [Термины и сокращения](#terms)

<a id="scope"></a>
## 1. Область и доказательства

Зафиксированы 50 файлов: пакет `internal/scheduler`, пакет `internal/autostart`, относящиеся к ним API/CLI-потребители недельного обновления и изменения определений, тесты и `go.mod`. Байты сверены с Git blob; эти же файлы на указанной вершине #623 идентичны master. Манифест определяет набор полученных источников, а не утверждает полное ревью всего репозитория. Производственные пути, названные в выводах, прочитаны непосредственно. Длинный `weekly_refresh_atomic_test.go` изучен выборочно по модели планировщика, ветвям восстановления, внешнему определению и освобождению блокировки; полный аудит его анализатора графа вызовов не заявляется.

Собственное чтение дополнено независимыми проходами по системным контрактам, потребителям и смыслу тестов. Авторы карт и итоговый составитель перепроверяли конкретные места исходника; отдельный неавторский проход проверил условия основных выводов и границы тестовых свидетельств. Это встроенная совместная работа над ревью, не внешний бот и не независимая продуктовая приёмка. Все рекомендации применяются в существующих владельцах операций, без формального воспроизведения ролей или новых обязательных слоёв.

COM — компонентная модель Windows; здесь собственный слой COM состоит из фиксированного сценария PowerShell и обмена JSON, структурированными текстовыми запросами/ответами. XML описывает определение задачи. SID — идентификатор безопасности учётной записи; HRESULT — код результата системного вызова. Их точные различия с именем задачи, учётной записью выполнения и результатом приложения объяснены в [§5](#identity); остальные сокращения — в [§11](#terms).

В таблицах «успех» означает только фактически проверяемый возврат соответствующего владельца. Регистрация определения, запрос запуска, наблюдаемое состояние планировщика и готовность/завершение приложения — отдельные факты. Наличие имени функции или успешной фикстуры не расширяет этот контракт.

<a id="operations"></a>
## 2. Матрица операций

<a id="table-1"></a>
**Таблица 1. Действующий backend — нижележащая реализация — и пробелы эквивалентной замены.**

| Операция | Фактическая реализация на a7d92d2 | Успех, ошибки и пределы | Документированный системный интерфейс | Что требуется для удаления собственного COM |
| --- | --- | --- | --- | --- |
| `Create` | Go формирует XML, записывает временный файл UTF-16LE с маркером порядка байтов и вызывает `schtasks /Create /TN name /XML file /F`. [2, строки 205–225](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go#L205) | Нулевой код команды даёт `nil`; иначе возвращается обёрнутый процессный отказ с текстом вывода. Нет контекста и deadline — предельного срока. `/F` допускает замену существующей задачи, вопреки интерфейсу. | `schtasks /Create /XML`, `/F`: [10](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-create), [11](https://learn.microsoft.com/en-us/windows/win32/taskschd/schtasks). | COM здесь уже отсутствует. Нужно разрешить противоречие create-versus-replace, то есть создание против замены, после проверки политики вызывающего кода. Не переносить `/F` в новый общий контракт молча. |
| `ImportXML` | Проверяет кодировку, сохраняет корректные байты UTF-16LE либо преобразует UTF-8, затем тот же `/Create /XML /F`. [2, строки 252–322](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go#L252) | Замена существующей задачи здесь прямо предусмотрена комментарием для rollback — восстановления предыдущего определения. Проверяется кодировка, но не смысл XML, не владелец и не `Hidden`. Ошибки команды не переводятся в типизированные scheduler-ошибки. Контекста нет. | Создание из XML документировано в [10](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-create). | COM здесь уже отсутствует. Сохранить Unicode и намеренную семантику восстановления; отделить восстановление определения от восстановления состояния и экземпляров исполнения. |
| `ExportXML` | PowerShell → `Schedule.Service` → корневая папка → `GetTasks(0)` → точное сравнение имени → `$t.Xml`. Go возвращает байты строки. [2, строки 237–249](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go#L237), [3, строки 91–111](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows.go#L91) | Нет видимого совпадения → `ErrTaskNotFound`; пустая задача/XML → `ErrTaskCorrupt`; COM exception — исключение — → `ErrUnavailable`. Скрытая задача ошибочно считается отсутствующей. Экспорт содержит определение, не снимок процесса. | `IRegisteredTask.Xml` [17](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nn-taskschd-iregisteredtask); системная CLI-альтернатива `/Query /TN path /XML` [12](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-query). | Экспорт через CLI возможен по форме интерфейса. Требуется доказать классификацию отсутствия/отказа и обработку кодировки фактического вывода, включая скрытые задачи и допустимые пути. |
| `Status` | Тот же COM-поиск; читает числовой `State`, XML и `Definition.Principal.UserId`; `taskStatusFromCOM` принимает только значения 1–4. [2, строки 350–359](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go#L350), [3, строки 164–178](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows.go#L164) | Типизированное состояние не зависит от текстовой локализации. 0 или неизвестное значение → `ErrUnavailable`. `LastResult` остаётся 0, `NextRun` пустым. Чтения состояния, XML и принципала раздельные. | `TASK_STATE`: 0 неизвестно, 1 выключено, 2 в очереди, 3 готово, 4 выполняется [18](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/ne-taskschd-task_state). Состояние, последний результат и XML — разные свойства [17](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nn-taskschd-iregisteredtask). | `/Query /XML` не заменяет оперативное состояние. Документация `/FO CSV` не даёт основания считать отображаемые значения числовым, независимым от языка протоколом. Нужен конкретный проверяемый способ сохранить этот контракт. |
| `List` / `ListContext` | COM перечисляет только нескрытые задачи корневой папки, отбирает имя по префиксу; Go переводит состояние и оставляет совпавшую короткую учётную запись. [2, строки 361–389](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go#L361) | При неизвестном состоянии или отказе чтения строки возвращается ошибка всего списка. Это не молчаливый пустой список. `ListContext` передаёт контекст; обёртка ограничивает его 15 секундами. Сравнение владельцев не является сравнением SID. | `GetTasks(0)` исключает скрытые задачи; `TASK_ENUM_HIDDEN` включает их [15](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-itaskfolder-gettasks). `/Query` имеет XML/CSV-формы [12](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-query). | Явно зафиксировать область списка: корень/вложенные папки, скрытые задачи, фильтр учётной записи. Сохранить отмену, неизвестное состояние и отказ чтения; не превращать их в «список пуст». |
| `Run` и отдельная способность `RunOwned` | Обычный `Run`: `schtasks /Run /TN name`. `RunOwned`: собственный COM-сценарий проверяет ожидаемое определение и SID, повторно читает XML, затем вызывает `$t.Run($null)` и требует ненулевой результат. [2, строки 325–333](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go#L325), [3, строки 54–105](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows.go#L54), [4](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_owned_run_windows.go) | `Run`: только результат команды, без контекста или проверок ожидаемой задачи. `RunOwned`: контекст, полный запрос, допустимая GUI-форма, SID принципала, enabled/state и наблюдаемое изменение XML. Несовпадение → `ErrTaskCorrupt`; нет положительного подтверждения → `ErrUnavailable`. Нет fallback на `Run`. | `/Run` запускает сохранённую задачу по имени [13](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-run). COM `Run` возвращает интерфейс нового экземпляра; при `AllowDemandStart=false` может завершиться без ошибки, но не запустить задачу [19](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-iregisteredtask-run). | Обычный `Run` уже не использует COM. Замена `RunOwned` одним `/Run` потеряет его проверки. Ни последовательность query/run, ни удержанный COM-объект сами по себе не доказывают атомарность против внешней замены. Нужно сохранить реальную ограниченную гарантию и честный смысл положительного результата. |
| `Stop` | COM-поиск; если числовое состояние не 4, выдаёт `task_not_running`; иначе `$t.Stop(0)`, затем `ok=true`. Go превращает not-found и not-running в `nil`. [2, строки 335–341](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go#L335), [3, строка 108](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows.go#L108) | Проверок ожидаемого определения/SID нет. После вызова нет проверки состояния/экземпляров. Скрытая задача → ложный успешный no-op. Значение `S_FALSE` требует отдельной проверки поведения COM Automation в PowerShell; см. §5. | `IRegisteredTask::Stop` различает `S_OK` и `S_FALSE` [20](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-iregisteredtask-stop). `/End /TN` предназначен для экземпляров программы, запущенных указанной задачей [14](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-end). | Сохранить различение «отсутствует», «не выполняется» и «нельзя остановить». Успешный exitcode команды не расширять до завершения произвольных дочерних процессов. Если потребитель требует защищённой остановки, обычный `Stop` такого контракта сейчас не предоставляет. |
| `Delete` | После COM-поиска выполняет **`$f.DeleteTask($t.Name,0)`**, то есть удаляет по имени. `ErrTaskNotFound` поглощается. [2, строки 229–235](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go#L229), [3, строка 107](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows.go#L107) | Нет проверки ожидаемого определения или SID, нет Stop или ожидания завершения. Удержанный `$t` не превращает `DeleteTask(name)` в удаление конкретной версии объекта. Скрытая задача → ложный успешный no-op. | `DeleteTask` принимает имя [16](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-itaskfolder-deletetask). `/Delete` удаляет регистрацию и прямо не обещает прерывать уже запущенную программу [21](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-delete). | CLI-удаление по имени соответствует форме операции, но требует сохранения отсутствия/отказа и точного допустимого имени. Оно не заменяет остановку и не создаёт атомарную проверку владельца. Не вводить wildcard — маску имён — вместо конкретного имени. |

<a id="findings"></a>
## 3. Дефекты и расхождения контрактов

### 3.1. `Create` заменяет задачу вопреки обещанию интерфейса

**Условие:** задача с указанным именем уже существует и текущая учётная запись вправе её перезаписать. **Место:** `Scheduler.Create` в [1, строки 109–112](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler.go#L109) обещает ошибку, тогда как [2, строка 220](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go#L220) передаёт `/F`. Microsoft документирует принудительное создание при уже существующем имени и подавление предупреждения [10, 11](#references).

**Последствие:** вызывающий код не получает обещанного сигнала конфликта; прежнее определение может быть заменено. Предварительный `Delete` у известных потребителей не доказывает отсутствие чужой регистрации в последующем промежутке. Это конкретное расхождение исходника и системного контракта, а не вывод из названия функции или гипотетического падения теста.

**Минимальная область исправления:** согласовать контракт создания с политикой замены у потребителей. Из этого аудита не следует разрешение автоматически убрать `/F` везде: у `ImportXML` замена явно нужна для восстановления. Новый тест должен отвечать конкретно на вопрос о повторном `Create`, а не просто копировать список аргументов. В этом проходе нативная задача не создавалась.

### 3.2. Скрытая задача превращается в доказанное отсутствие

**Условие:** в корне планировщика существует доступная задача с точным именем и `<Hidden>true</Hidden>`. Такая задача допускается `ImportXML`: подготовка импорта проверяет кодировку, но не запрещает `Hidden`. **Место:** общий COM-поиск в [3, строки 91–93](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows.go#L91) использует `GetTasks(0)`. По Microsoft этот флаг исключает скрытые задачи [15](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-itaskfolder-gettasks).

Цепочка полностью видна в коде: скрытая задача → отсутствует в выбранной коллекции → `task_absent` → `ErrTaskNotFound` → `Status`/`ExportXML` сообщают отсутствие, а `Stop`/`Delete` возвращают `nil` без вызова системной операции. Обычный `Run` не проходит через эту коллекцию, поэтому общая схема доступа к одной задаче оказывается несогласованной.

**Последствие:** восстановленная скрытая задача может остаться зарегистрированной или выполняющейся после внешне успешного `Delete`/`Stop`; отсутствием нельзя обосновывать дальнейшую замену либо пропуск восстановления. Это установленный путь в исходнике при документированном допустимом входе. Факт наличия такой задачи на пользовательской машине не утверждается; нативное воспроизведение не выполнялось.

**Минимальное требование к исправлению:** точечный поиск по известному имени должен устанавливать отсутствие самой задачи, а не её отсутствие в отфильтрованном списке. `ITaskFolder::GetTask(path)` предоставляет точечный интерфейс [23](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-itaskfolder-gettask); при любом выбранном backend всё равно потребуется классификация реального отказа. Расширение перечисления само по себе не создаёт проверку владельца или атомарность удаления.

### 3.3. Граница классификации: объявленные типы ошибок шире реального источника классификации

`schedulerCOM` умеет потреблять `permission_denied` и `task_corrupt`, но рабочий PowerShell `catch` всегда выдаёт `scheduler_unavailable` и сериализует `HResult`. Go-код не использует `HRESULT` для классификации. Проверки ошибочных ответов в тесте `TestSchedulerCOMMapsTypedAccessCorruptAndUnavailable` подставляют готовые поля `Kind`; они не показывают, что реальные Windows-отказы доступа или повреждения дадут эти виды ошибок. [3, строки 111–160](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows.go#L111), [6, строки 36–57](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows_test.go#L36).

Реально достижимые локальные `ErrTaskCorrupt` существуют: неполный запрос `RunOwned`, несовпавшее определение, пустой export/status payload — полезная нагрузка ответа. Это не тождественно классификации нативной ошибки планировщика. Консервативное `ErrUnavailable` сохраняет отказ; само по себе оно не разрешает опасное действие. Здесь установлен пробел публичного обещания/доказательств, а не автоматически новый дефект безопасности.

### 3.4. Граница данных: `LastResult` и `NextRun` не являются наблюдёнными значениями

Интерфейс описывает `LastResult` как код последнего запуска, с `-1` для никогда не выполнявшейся задачи. В COM-конверте этих полей нет; `taskStatusFromCOM` их не задаёт. Поэтому `LastResult=0` означает нулевую инициализацию Go, а не подтверждённый успешный выход программы; пустой `NextRun` также не является прочитанным расписанием. Microsoft предоставляет для последнего результата и следующего запуска отдельные свойства [17](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nn-taskschd-iregisteredtask). Источник: [1, строки 51–58](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler.go#L51), [3, строки 29–34 и 164–178](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows.go#L29).

Это конкретное расхождение модели данных. Пользовательское последствие зависит от потребителей полей и не объявляется доказанным в рамках этого платформенного чтения. Будущая замена не должна считать эти нули эталонными данными успешного запуска.

<a id="consumers"></a>
## 4. Дефекты у потребителей

### 4.1. Потеря прежнего определения при отказе замены

**Условие:** существующая исправная задача успешно удалена, последующий Create возвращает ошибку и не оставляет пригодной задачи.

**Места:** [Windows Enable, Delete/Create](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/windows.go#L112-L134); [legacy WeeklyRefreshSet](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/scheduler_mgmt.go#L328-L337). В обоих путях нет снимка и возврата прежней задачи. В общем SchedulerUpgrade тот же исход достижим при ошибке Export, поскольку [ошибка подавляется до Delete](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/scheduler_mgmt.go#L115-L148).

**Последствие:** неудачная попытка изменения снимает ранее работавший автозапуск/расписание. Ошибка Create видна, но предыдущая работающая конфигурация не сохранена. Комментарий Windows Enable об `atomic-from-the-user-perspective` не соответствует достижимой ветви. Это вывод из порядка операций и отсутствия восстановления; падающий нативный прогон не заявляется.

### 4.2. Ошибка восстановления скрыта от результата upgrade

**Условие:** снимок получен, Delete успешен, Create неуспешен, последующий ImportXML также неуспешен.

**Места:** [обычная серверная задача, строка 146](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/scheduler_mgmt.go#L143-L148); [lazy proxy, строка 305](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/scheduler_mgmt.go#L303-L307).

**Последствие:** оператор получает только ошибку Create и не узнаёт причину/факт неудачного восстановления. Уже существующая недельная транзакция сохраняет обе ошибки и проверяет восстановленный XML; следовательно, возвращать в остальных путях ложное полное восстановление не требуется для совместимости.

### 4.3. Общий upgrade изменяет расписание вопреки своему контракту

**Условие:** у распознаваемой per-server weekly-refresh задачи существующее расписание отличается от воскресенья 03:00; либо её trigger/settings были изменены оператором при сохранении допустимого имени.

**Место:** публичный комментарий обещает сохранить trigger и менять только Command/WorkingDirectory [строки 23–30](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/scheduler_mgmt.go#L23-L30), но [строки 130–142](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/scheduler_mgmt.go#L130-L142) создают новый TaskSpec с воскресеньем 03:00 либо одним logon trigger.

**Последствие:** успешный ремонт пути к исполняемому файлу сбрасывает прежнее расписание и часть настроек. Наличие теста сохранения Friday 09:45 относится только к `upgradeWorkspaceWeeklyRefreshTask`; на обычный per-server путь этот тест не распространяется. У lazy proxy восстановление аргументов из registry объявлено намеренным, поэтому само по себе обновление этих аргументов здесь дефектом не названо.

### 4.4. Восстановление после успешного Create может уничтожить внешнее текущее поколение

**Условие:** наш Create успешно записал задачу; до контрольного Export другой автор изменил/заменил задачу с тем же именем. Export возвращает внешнее определение, не совпадающее с requested spec.

**Цепочка:** [строки 319–328](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/weekly_refresh.go#L319-L328) вызывают `restoreWeeklyRefreshTaskAfterPostCreateFailure` и при ошибке наблюдения, и при несовпадении. Затем [строки 193–196](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/weekly_refresh.go#L191-L208) импортируют prior без допуска текущего поколения; если prior отсутствовал, [строки 211–219](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/weekly_refresh.go#L211-L224) удаляют текущую задачу. Windows ImportXML использует `/F`.

**Последствие:** возврат ошибки конфликта способен одновременно перезаписать либо удалить изменение внешнего автора. Блокировка файла сериализует согласованных писателей приложения, но внешнего автора не останавливает. Это отдельный подтверждаемый по коду сценарий, а не утверждение о замеченной гонке на машине пользователя.

Тест [CreateFailurePreservesForeignCurrent](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/weekly_refresh_atomic_test.go#L321-L341) проверяет другую ветвь — Create уже вернул ошибку. Тест восстановления после ошибки проверки [строки 3307–3356](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/weekly_refresh_atomic_test.go#L3307-L3356) подменяет ответ Export, сохраняя собственную задачу в модели; внешней замены он не моделирует. Дополнительная повторная query без системного compare-and-replace также не создаст атомарности.

### 4.5. Отключённая задача отображается как включённая и остановленная

**Условие Windows:** зарегистрированная каноническая задача имеет правильные Command/Args, но отключена в Task Scheduler. Status выдаёт Disabled, XML содержит Enabled=false.

**Место:** [windowsTaskStatusState](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/windows.go#L265-L269) переводит любое не-Running состояние в EnabledStopped. [Drift-предикат](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/windows.go#L356-L387) проверяет путь/argv, но не Enabled.

**Последствие:** публичная метка `enabled-stopped` не соответствует фактическому выключению задачи и своему [обещанию запуска при следующем входе](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/autostart.go#L103-L106). Изменение Enabled влияет на fingerprint, но не исправляет State.

**Аналогичная потеря различия на POSIX:** Linux игнорирует `is-enabled` при выборе State и подавляет ошибки `is-active`; macOS при существующем plist подавляет ошибку print. Поэтому disabled unit, неудачный bootstrap либо недоступный user manager становятся успешным `enabled-stopped`, хотя ни активация, ни наблюдение состояния не подтверждены. Условие — совпадающий файл определения и ошибка опроса менеджера с пустым выводом: [Linux, строки 298–323](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/linux.go#L298-L323), [macOS, строки 204–230](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/darwin.go#L204-L230). Это дефект публичной проекции наблюдения; нативное функционирование менеджера здесь не проверялось.

### 4.6. Главная справка CLI предлагает несуществующие формы weekly-refresh

**Условие:** оператор следует `scheduler --help` и вводит `scheduler weekly-refresh set SUN 03:00` или `scheduler weekly-refresh disable`.

**Место:** [примеры строк 34–37](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/cli/scheduler_cmd.go#L29-L39) расходятся с [реальными флагами/обработчиком](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/cli/scheduler_cmd.go#L73-L102): подкоманд set/disable нет, нужны `--set "SUN 03:00"` или `--disable`.

**Последствие:** оба документированных примера доходят до отсутствующих флагов и возвращают `--set or --disable is required`; требуемая операция не выполняется. Это ограниченный дефект справки, не причина менять архитектуру планировщика.

### 4.7. Linux: допустимый `%` в пути меняет выполняемый файл

**Условие:** путь к бинарному файлу содержит буквальную последовательность, совпадающую со specifier systemd, например синтетический путь `/opt/example/release-%n/mcphub`.

**Место:** [render unit и quoteSystemdExecArg](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/linux.go#L153-L191) заключают путь в кавычки и экранируют обратную косую черту/кавычку, но оставляют `%`.

**Последствие:** systemd раскрывает `%n` в полное имя unit — `mcphub-supervisor.service`, вследствие чего пытается запустить другой путь. Для буквального `%` документирован `%%`; кавычки это раскрытие не подавляют [24, 25](#references). Возможный результат — отказ запуска или запуск файла из другого существующего каталога. Никакой такой файл/задача в этом проходе не создавались. Правило раскрытия `$` в последующих аргументах не переносится автоматически на первый аргумент; отдельный дефект `$` не установлен.

<a id="identity"></a>
## 5. Идентичность задачи, запуск и завершение

**Таблица 2. Разные виды идентичности и свидетельств.**

| Что проверяется | Что действительно делает код | Чего это не доказывает |
| --- | --- | --- |
| Имя/путь задачи | Для COM выбирает корневую папку и точное, чувствительное к регистру совпадение `Name`; `RunOwned` отдельно запрещает вложенные пути. | Одно и то же имя не означает одну и ту же версию определения во времени. Интерфейс общего `Create` не вводит такой же явной проверки пути. |
| Определение задачи | `RunOwned` получает XML-снимок, проверяет форму, затем повторно читает старый объект и `GetTask(name)`, сравнивает строки XML и повторяет проверки. | Чтения и Run не образуют документированной атомарной операции. Совпадение прочитанных снимков не блокирует последующего внешнего автора. |
| Учётная запись | `Owner` в COM-конверте — `Definition.Principal.UserId`, то есть принципал выполнения. `List` сравнивает короткие имена; `RunOwned` переводит два UserId — из XML и нативного определения — в SID. | Это не отдельная проверка автора регистрации либо владельца дескриптора безопасности. Короткое имя из `List` не является идентичностью SID. |
| Экземпляр выполнения | `RunOwned` требует ненулевой объект, возвращённый `Run`. Обычный `Run` проверяет только результат `schtasks`. | Ни одно из этих условий не устанавливает готовность GUI, успешный выход приложения или сохранение состояния Running после возврата. |
| Завершение | `Stop` выполняет системный вызов без последующего наблюдения экземпляров; `Delete` удаляет по имени. | Удаление регистрации не является барьером завершения приложения. Остановка конкретной задачи не доказывает завершение произвольных связанных процессов. |

Основание таблицы 2: [1–4](#references), документированные раздельные свойства и методы [16–20, 23](#references).

`RunOwned` содержит полезные проверки: полный ожидаемый запрос; канонический абсолютный путь; единственную допустимую GUI-команду; пространство имён и форму XML; точный URI — идентификатор задачи — и привязку action к principal; SID учётной записи; допустимые аргументы `gui --no-browser` с необязательным `--strict-mode`; XML/native Enabled и числовое состояние. Сценарий запрещает DTD — Document Type Definition, определение типа XML-документа — и отключает внешний resolver. Проверки повторяются после нового получения задачи. Они уменьшают область допустимого запуска и обнаруживают наблюдённое изменение. [3, строки 54–105](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows.go#L54).

При этом сам код прямо сообщает, что вызывающая сторона держит кооперативную lease — блокировку, соблюдаемую участниками данного протокола, — а проверки не обеспечивают атомарный compare-and-run, то есть запуск лишь при совпадении определённой версии задачи, против внешних авторов. Рассмотренные Microsoft-контракты `GetTask`, `Run` и `DeleteTask` такой гарантии не дают. Поэтому удержанный `$t` не используется как доказательство неуязвимости к подмене. Для `Delete` особенно важно, что последующая мутация снова принимает строковое имя. Это предусмотренная граница текущей схемы; самостоятельного доказательства эксплуатации или нового обязательного усложнения архитектуры здесь нет.

Для `Stop` есть отдельный конкретный вопрос к нативной границе. Microsoft различает `S_OK` — остановлены доступные пользователю экземпляры — и `S_FALSE` — пользователь не смог успешно остановить экземпляры [20](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-iregisteredtask-stop). Общие документы COM/.NET объясняют, что возможны несколько успешных HRESULT и что их сохранение может требовать специального объявления интерфейса [22](https://learn.microsoft.com/en-us/dotnet/standard/native-interop/preserve-sig). Код не проверяет статус `Stop` явно и не наблюдает результат остановки. Однако здесь **не установлено**, как именно используемый PowerShell Automation marshaling — преобразование между COM и управляемым кодом — передаст конкретный `S_FALSE`: без этого нельзя заявлять доказанное ложное `nil` при нативном отказе. Нужен один адресный источник либо разрешённый эксперимент, если этот сценарий становится обязательным критерием.

Срок 15 секунд в `schedulerCOM` — срок контекста, переданного `exec.CommandContext`. В коде нет `WaitDelay` или собственного протокола завершения дерева процессов. Из этого аудита не следует строгая 15-секундная граница завершения всех потомков и освобождения всех каналов. `Create`, `ImportXML` и обычный `Run` используют вообще `exec.Command` без контекста. [3, строки 113–143](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows.go#L113), [2](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go#L205).

<a id="lifecycle"></a>
## 6. Автозапуск: OFF, завершение, выход пользователя

**Таблица 3. Фактическая проекция состояния и её пределы.**

| Сценарий | Windows | Linux | macOS |
|---|---|---|---|
| Явный OFF | ProjectAutorunDisabled вызывает Disable, затем независимо Status; сохраняет обе ошибки. Успех требует `StateAbsent`. Disable выполняет только Delete задачи | Disable вызывает `disable --now`, удаляет unit, затем daemon-reload. Ошибки менеджера допускаются как предупреждения; Status при отсутствии файла возвращает absent | Disable игнорирует ошибку bootout и удаляет plist; Status при отсутствии файла возвращает absent |
| Чистое завершение владельца | Автозапуск задаёт `RestartOnFailure=false`; в его определении нет автоматического перезапуска владельца по exit. Поведение отдельного runtime/watchdog этим не доказано | `Restart=on-failure`: exit 0 и документированные сигналы чистого завершения не вызывают автоматический restart [24](#references) | `KeepAlive=true`: launchd должен поддерживать процесс работающим, включая новое создание после чистого выхода. Это не эквивалент `on-failure` [28](#references) |
| Аварийное завершение | Отсутствие RestartOnFailure в этой задаче остаётся прежним; восстановление дерева средствами supervisor — отдельный контракт | `on-failure`, задержка 5 секунд, с действующими ограничениями systemd на частоту запусков [24](#references) | `KeepAlive=true`; точный результат на целевой macOS не измерен |
| Выход из сеанса | XML использует `InteractiveToken`, которому нужна существующая интерактивная сессия [26](#references). Работа после logout не обещается | Enable пытается выполнить `loginctl enable-linger`, но ошибка даёт предупреждение и не отменяет Enable. Без подтверждённого linger нельзя обещать работу после logout [27](#references) | Загрузка выполняется в пользовательском GUI-домене; запуск после выхода из сеанса этим контрактом не обеспечен [29](#references) |
| Менеджер отсутствует или недоступен | Ошибка получения Status сохраняется; `ErrUnavailable` в обычном Status переводится в `ErrStatusObservationUnavailable`. Но есть старый текстовый обход absent и зависимость от корректности нижнего уровня | При отсутствии executable systemctl Enable сохраняет unit, предупреждает и возвращает nil. При ошибке соединения с имеющимся менеджером reload/enable возвращает ошибку. Status подавляет ошибки `is-enabled`/`is-active` | Ошибка bootstrap возвращается, хотя plist уже записан; Status игнорирует ошибку print и может вернуть enabled-stopped |
| Сохранено, но не активно | Disabled/Queued/Ready сворачиваются в enabled-stopped; неизвестное числовое состояние COM-слой возвращает ошибкой. XML-флаг Enabled участвует в fingerprint, но не в State | Файл может существовать без enabled-ссылки или доступного user manager. Это не отдельное состояние; State всё равно stopped/running/drifted | Файл может существовать после неудачного bootstrap; это не отдельное состояние, enabled-флаг в fingerprint задан как true |
| Немедленный запуск | StartOwner вызывает Run по имени. Защищённый вариант дополнительно допускает только абсолютный обычный Product без перенаправления и требует `OwnedRunner` с GUI-ролью и SID текущего пользователя | `systemctl --user start <unit>` | `launchctl kickstart -k <target>`; после ответа команды состояние/готовность не наблюдаются |

Источники таблицы 3: [общий OFF](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/autostart.go#L208-L229), [Windows Disable/Status](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/windows.go#L172-L269), [Windows настройки задачи](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go#L109-L156), [Linux linger](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/linux.go#L94-L112), [Linux enable/disable/status](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/linux.go#L203-L336), [macOS plist/enable/disable/status](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/darwin.go#L94-L230), [Linux StartOwner](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/start_linux.go#L8-L13), [macOS StartOwner](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/start_darwin.go#L8-L14).

Важная граница OFF: этот слой проверяет снятие регистрации/файла автозапуска и не устанавливает завершение ранее запущенного дерева. При выявленном ложном отсутствии скрытой задачи (§3.2) даже успешный ответ Disable/Status не доказывает фактического удаления регистрации. Windows Disable не вызывает Stop, а POSIX-реализации могут удалить файл после неудачной остановки. Документация `schtasks /delete` отдельно говорит, что удаление задачи не прерывает уже работающую программу [21](#references); текущий Delete использует COM DeleteTask и также не содержит Stop/ожидания. Поэтому новое чтение или новый backend не должны превращать `absent` в более сильное обещание «процесс завершён». Полное намерение OFF и блокировка допуска принадлежат вызывающему коду, что прямо указано перед ProjectAutorunDisabled; эти внешние policy-владельцы в настоящем срезе не просмотрены целиком.

Общий пакет `scheduler` на Linux и macOS возвращает `ErrNotImplemented`; отдельные реализации `autostart` используют systemd и launchd. Поэтому Windows-замена не должна приписывать общему Scheduler существующую POSIX-реализацию или переносить семантику одного менеджера на другой. Тестовый `MCPHUB_E2E_SCHEDULER=none` возвращает пустое чтение и отвергает изменяющие операции; это не свидетельство состояния ОС. [Scheduler](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler.go#L148-L170), [тестовая реализация](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_noop.go).

`KeepAlive=true` на macOS действительно отличается от `Restart=on-failure`. Неточен комментарий, приравнивающий эти политики; без полного требования вызывающего сценария сама политика перезапуска после чистого выхода не объявляется дефектом. Гипотеза о цикле только из `After=default.target` и `WantedBy=default.target` не подтверждена: systemd допускает явный обратный порядок вместо автоматического [25, 30](#references). Эти два вопроса не превращаются в необоснованные правки поведения.

<a id="evidence"></a>
## 7. Что покрывают прочитанные тесты

**В этом проходе ни один тест не запускался.** Таблица 4 описывает только устройство существующих проверок, а не новый результат PASS.

**Таблица 4. Доказательная область существующих тестов.**

| Файл | Реальная проверка в исходнике | Не подтверждает |
| --- | --- | --- |
| `scheduler_windows_test.go` [5](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows_test.go) | Формирование XML, триггеры, настройки перезапуска, Windows-экранирование аргументов, сравнение коротких имён, выбор пути `schtasks`. | Нативную регистрацию, поведение повторного `Create`, скрытые задачи или фактический запуск процесса. |
| `scheduler_com_windows_test.go` [6](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows_test.go) | Подставленные типы ответа и числовые состояния; PowerShell-проверки сериализации отрицательного HRESULT и Unicode с заменённым обращением к сервису. | Реальную классификацию доступа/повреждения планировщика, отсутствие задачи в системном сервисе или запуск/остановку задачи. |
| `scheduler_com_target_windows_test.go` [7](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_target_windows_test.go) | Только при `MCPHUB_TARGET_SCHEDULER_QA=1` создаёт уникальную нескрытую задачу в корне; проверяет отсутствие, Create/Status/Export/List/Run, наблюдение Running, Stop и Delete. | Скрытую задачу, конфликт Create, чужой SID, подмену определения, `S_FALSE`, атомарность или завершение всех потомков. В этом аудите запуск запрещён и не выполнялся. |
| `scheduler_owned_run_windows_test.go` [8](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_owned_run_windows_test.go) | Допуск запроса/положительный ответ и 26 случаев на производственном PowerShell-проверяющем коде с искусственными объектами task/folder; изменение второй XML-выборки; контроль счётчика вызовов fake Run. | Настоящую привязку COM-объекта к изменяемой регистрации, запуск ОС, внешнюю гонку или atomic compare-and-run. |
| `xml_import_windows_test.go` [9](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/xml_import_windows_test.go) | Unicode, преобразование UTF-8 → UTF-16LE, сохранение уже корректных байтов, отказ на неверных кодировках/суррогатах. | Семантическую допустимость определений, скрытые задачи, конфликт регистрации или реальное восстановление планировщика. |

Названия тестов и текст их диагностик не расширяют эту область. В частности, строка `native Run count` в тесте `RunOwned` относится к счётчику искусственного PowerShell-метода `Run`; это не квитанция системного Task Scheduler.

**Таблица 5. Область доказательства исходников тестов; запусков в R17 нет.**

| Группа | Что проверяется кодом тестов | Что они не доказывают |
|---|---|---|
| Общий autostart | Строковые метки State и ненулевой backend | Реальный системный запуск или полноценный OFF |
| Windows autostart | Выбор argv, strict/owner mode, отсутствие мутации при неверном mode, регистрацию вызовов Delete/Create, drift, fingerprint, различие обычного/строгого XML-наблюдения | В fake методы Run/Stop/ImportXML вообще паникуют; замена с потерей старой задачи и реальное завершение процесса не моделируются |
| Windows guarded StartOwner | Отказ nil/cancelled context, пустого/относительного/несуществующего пути, неподдержанного capability; правильный OwnedRunRequest | Проверяется текстовый fixture-файл и fake capability; это не запуск установленного Product и не испытание Task Scheduler |
| Scheduler management validation | Workspace weekly/lazy proxy прерываются при Export error; workspace weekly сохраняет Friday 09:45 | Общий per-server upgrade с подавленным Export error, потеря rollback error и сохранение произвольных настроек не покрыты этими тестами |
| Scheduler swap | В in-memory fake: свежая/существующая задача, ошибка создания, успешный/неуспешный import | Системная атомарность, внешний автор между успешным Create и Export |
| Выбранные weekly atomic tests | Сериализация согласованных писателей, чужое поколение после ошибки Create, сохранение ошибок release, восстановление после ошибочного/неразбираемого контрольного Export | Полный внешний compare-and-replace; настоящий HRESULT, ACL — список управления доступом, native lock/COM settlement |
| WeeklyRefreshAll и CLI | Выбор enabled registry rows, предупреждения по строкам, порядок вызовов kill-before-Run, CLI wiring/JSON | Kill fixture только записывает событие; существующий процесс/порт не прекращается. Windows-only CLI-тесты пропускают POSIX, общего Scheduler там нет |
| Linux/macOS autostart | Файлы и команды через fake; режимы drift/status/strict | Native service manager, реальный logout, чистый/аварийный выход, `%` в executable path и успешное прекращение процесса после OFF |

Источники таблицы 5: [общие тесты](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/autostart_test.go), [Windows fake и тесты](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/windows_test.go), [guarded admission](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/start_windows_test.go), [mgmt validation](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/scheduler_mgmt_validation_test.go), [swap](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/scheduler_swap_test.go), [weekly fake и failure cases](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/weekly_refresh_atomic_test.go#L62-L359), [release/post-create tests](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/weekly_refresh_atomic_test.go#L3156-L3356), [WeeklyRefreshAll tests](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/weekly_refresh_test.go#L13-L171), [CLI tests](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/cli/weekly_refresh_test.go#L43-L97).

`TestSchedulerUpgrade_NoopWhenEmpty` пропускается при любой ошибке scheduler и не проверяет содержимое результатов; это не доказательство успешного полного upgrade на нативном пустом планировщике. [Исходник теста](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/scheduler_mgmt_test.go#L62-L72).

Отдельная точность тестового анализа: `TestWindowsBackend_EnableReplacesExistingTask` ведёт раздельные списки `createCalls` и `deleteCalls`; он проверяет порядок двух Delete и число Create, но не взаимный порядок Delete→Create единым журналом. В PowerShell-фикстурах `RunOwned` искусственный `GetTasks($flags)` возвращает задачу независимо от флага; такие случаи не выявляют исключение скрытых задач настоящим `GetTasks(0)`. Это обнаруженные пределы проверок, а не новые результаты их выполнения. [Autostart fixture](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/autostart/windows_test.go#L221-L253), [RunOwned fixture](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_owned_run_windows_test.go).

<a id="next"></a>
## 8. Решение и следующий ограниченный контракт

### 8.1. Сохранить владельцев и разделить результаты

`Create`, `ImportXML` и обычный `Run` уже вызывают `schtasks`; переводить их на новый язык ради удаления собственного COM не требуется. Для первого удаления зависимости выбран **кандидат чтения определения одной задачи**, а не замена всех восьми операций. До реализации нужно установить фактические байты и исходы `/Query /TN <exact-name> /XML` на разрешённой системе: подтверждённое отсутствие, отказ доступа, недоступный менеджер, повреждённый ответ и корректное определение. `/HRESULT` в справке Query относится к коду завершения управляющей команды; это не `LastResult` приложения и не готовая таблица всех ошибок любых команд [12].

Пока такая эквивалентность не доказана, действующий COM-путь не объявляется заменённым. XML сохраняет определение; из него нельзя получить Running, очередь или последний результат. Чтение CSV также не доказывает независимость значений от языка. `RunOwned` сохраняет все проверки ожидаемого продукта/роли/SID и запрет запасного обычного Run; новая последовательность query/run не должна называться атомарной. Остановка/удаление и восстановление остаются обязанностями конкретных владельцев, а не побочными эффектами универсального наблюдателя.

Для ошибок перед разрушительным действием достаточно сохранять неизвестность и прекращать зависимую мутацию. Из карты не следует необходимость вводить новый реестр, общий журнал или формальную обёртку. При исправлении восстановления нельзя копировать существующий weekly-путь без устранения §4.4: он лучше сохраняет часть ошибок, но не защищает чужое определение после успешного Create. Повторное чтение уменьшает обнаруживаемое окно, однако не предоставляет системной атомарности.

### 8.2. Исправления и проверки по конкретному вопросу

**Таблица 6. Остаток R17 после завершённой карты.** «Проверка» здесь — требование к следующему изменению; ни одна строка не объявлена выполненной.

| Область и владелец | Минимальный следующий результат | Достаточный различающий вопрос проверки |
|---|---|---|
| Создание и точечный поиск, `scheduler` | Согласовать отказ при существующем имени отдельно от намеренного Import replacement; скрытая доступная задача не становится absent | Повторный Create не меняет исходное определение при ожидаемом конфликте; Hidden=true находится и не даёт ложного успешного Stop/Delete. Проверка аргумента `/F` одна этого не доказывает |
| Замена определений, `autostart.Enable` и API writers | Не удалять при неполученном обязательном снимке; сохранять первоначальную ошибку и ошибку восстановления; уточнить допустимую политику восстановления | Delete succeeded/Create failed; Export failed before Delete; Create+Import both failed; отсутствие prior и неизвестный текущий результат |
| Weekly transaction | Сохранить внешнее текущее определение при несовпадении после успешного Create и не выполнять разрушительный rollback на неизвестном результате | Модель действительно заменяет запись внешним определением после успешного Create; проверить оба исходных состояния prior. Не подменять это тестом ошибки Create |
| Upgrade trigger и публичное состояние | Сохранить обещанное расписание; различать Disabled, известную остановку и недоступное наблюдение | Per-server расписание, отличное от Sunday 03:00; Windows Enabled=false; POSIX совпадающий файл и ошибка менеджера. Проверить наблюдаемый результат вызывающего API/CLI |
| Небольшие независимые дефекты | Исправить формы справки CLI; экранировать буквальный `%` в Linux пути, сохранив Windows-поведение | Примеры `--set`/`--disable` соответствуют обработчику; системный разбор возвращает исходный буквальный путь, а не раскрытый `%n`. Отдельная корректировка комментария macOS не меняет KeepAlive |
| Удаление собственного COM | Сначала квалифицировать чтение определения; затем отдельно защищённый запуск и разрушительные действия, если сохраняются обязательные свойства | UTF-8/UTF-16LE, локаль, hidden/absent/denied/unavailable, неверный SID/изменённый XML; честная граница запуска, остановки, окончания и отмены |

Смысл §4.4 — не требование создать недоступный compare-and-replace средствами нескольких команд. При неизвестной/чужой текущей записи безопасный результат должен сохранять конфликт, исходную причину и необходимость явного восстановления у владельца; автоматическое возвращение старого XML не является безусловным правом. Обязательный фактический контракт уточняется до кодовой правки и проверяется на его настоящем потребителе.

Возможность выполнить нативные проверки не выдаётся самим наличием теста. Работа с собственными задачами ОС требует уже действующего разрешения на точную фикстуру; реальная конфигурация пользователя не меняется. Нельзя закрыть пробел заменой на собственный COM/WMI binding, Registry Run или недокументированный TaskCache. Документированная команда Windows не является обещанием, что внутренняя реализация ОС вообще не использует COM.

<a id="verification"></a>
## 9. Проверки этого этапа и незавершённая квитанция R16

**Выполнено:** сверены точные исходные Git blob и равенство 50 файлов между master и исходной вершиной #623; прочитаны платформенный слой, указанные потребители и относящиеся к выводам тесты; проверены первичные системные документы. Собственное ревью дополнено отдельными чтениями. Структурная проверка итогового документа и планов, сравнений опубликованных байтов и точных коммитов отражается в сопровождающем COMMENT-ревью соответствующего PR. Исходный набор в манифесте не включает секреты или содержимое установленной конфигурации.

**Не выполнялось:** Go/Rust тесты, сборки, измерения, изменение задач ОС, установка, изменение Windows-политик или установленного Product. Локальная команда `go version` не подтвердила доступный Go toolchain; другая среда не использовалась как обход заблокированной нативной квалификации. Ни один source finding не назван падающим нативным прогоном. Общие Go-критерии остаются FAIL по ранее сохранённым командам; эта документационная работа их не пересчитывает.

**Незавершённый повтор R16 на #628:** исходник `453e856347cabcebb289ec1c9af70f12af3376c0`, запуск `published-exact / contract-race` около 01:14 МСК 19 ноября. Ранее получены чистый checkout, равенство исходников и START; итог, квитанция и завершение процесса не получены. При попытке восстановления около 03:04 МСК сервис явно сообщил, что Comet offline; старые сохранённые ответы недоступны. Это не доказательство успеха, ошибки, выключения машины или окончания процесса. Новый дублирующий прогон не запускался. Нужные исходники, журналы и каталоги восстановления остаются вне trash; окончательная очистка удалённой области не заявляется. Допубликационные результаты #628 и его [отчёт](https://github.com/applicate2628/mcp-local-hub/blob/453e856347cabcebb289ec1c9af70f12af3376c0/docs/modernization/verification/status-observation-2026-11-19/RESULTS.md) остаются действительными в своей области, но не подставляются вместо отсутствующей квитанции.

После возобновления доступа первым действием остаётся чтение состояния и квитанции **того же** запуска, без повторного выполнения. Независимая карта R17 выполнена при сохранённом блокере R16. Обе реализации R16 — PID-only в #659 и обычный Windows status в #628 — не возвращаются в невыполненную разработку.

<a id="references"></a>
## 10. Источники

Все ссылки на код закреплены на точном коммите `a7d92d2852c4f6eb8afb0569ef76a545e38aacf7`. Документация Microsoft прочитана 19 ноября 2026 года; её содержание не является квитанцией работы на конкретной машине.

1. [Интерфейс Scheduler и TaskStatus](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler.go).
2. [Windows backend](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows.go).
3. [Собственный PowerShell/JSON-слой COM](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows.go).
4. [OwnedRunner и допуск RunOwned](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_owned_run_windows.go).
5. [Тесты XML, аргументов, путей и коротких имён](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_windows_test.go).
6. [Тесты COM-протокола](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_windows_test.go).
7. [Нативная проверка жизненного цикла, с явным включением](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_com_target_windows_test.go).
8. [Фикстуры guarded RunOwned](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/scheduler_owned_run_windows_test.go).
9. [Тесты кодировок XML для импорта](https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/scheduler/xml_import_windows_test.go).
10. [Microsoft: schtasks create](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-create).
11. [Microsoft: Schtasks.exe](https://learn.microsoft.com/en-us/windows/win32/taskschd/schtasks).
12. [Microsoft: schtasks query](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-query).
13. [Microsoft: schtasks run](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-run).
14. [Microsoft: schtasks end](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-end).
15. [Microsoft: ITaskFolder::GetTasks](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-itaskfolder-gettasks).
16. [Microsoft: ITaskFolder::DeleteTask](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-itaskfolder-deletetask).
17. [Microsoft: IRegisteredTask](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nn-taskschd-iregisteredtask).
18. [Microsoft: TASK_STATE](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/ne-taskschd-task_state).
19. [Microsoft: IRegisteredTask::Run](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-iregisteredtask-run).
20. [Microsoft: IRegisteredTask::Stop](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-iregisteredtask-stop).
21. [Microsoft: schtasks delete](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-delete).
22. [Microsoft: Implicit method signature translations in .NET interop / PreserveSig](https://learn.microsoft.com/en-us/dotnet/standard/native-interop/preserve-sig).
23. [Microsoft: ITaskFolder::GetTask](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-itaskfolder-gettask).
24. [systemd.service: Restart и Command Lines](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html). Веб-документ на дату чтения относится к systemd 262; это не установленная версия целевой среды.
25. [systemd.unit: Specifiers и Default Dependencies](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html). `%n` — имя unit, `%%` — буквальный процент; явный обратный порядок может заменить автоматический.
26. [Microsoft: TASK_LOGON_TYPE](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/ne-taskschd-task_logon_type).
27. [systemd: loginctl / enable-linger](https://www.freedesktop.org/software/systemd/man/latest/loginctl.html).
28. [Apple Open Source: launchd.plist(5), KeepAlive / SuccessfulExit](https://github.com/apple-oss-distributions/launchd/blob/main/man/launchd.plist.5).
29. [Apple: Creating Launch Daemons and Agents](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html). Архивное официальное руководство; не квалификация текущей macOS.
30. [systemd upstream: unit_add_default_target_dependency](https://github.com/systemd/systemd/blob/main/src/core/unit.c). Прочитанный upstream на дату аудита, не доказательство конкретной установленной сборки.
31. [systemd upstream: config_parse_exec](https://github.com/systemd/systemd/blob/main/src/core/load-fragment.c). Первый токен проходит `unit_path_printf`; это подтверждает применимость раскрытия specifier к пути executable.

<a id="terms"></a>
## 11. Термины и сокращения

- **COM** — Component Object Model, компонентная модель Windows; **WMI** — Windows Management Instrumentation, интерфейсы управления Windows. Замена одного собственного binding другим не устраняет зависимость.
- **API / CLI / GUI** — программный интерфейс / интерфейс командной строки / графический интерфейс.
- **XML / JSON / CSV** — язык разметки / структурированный текстовый формат / табличный текстовый формат с разделителями.
- **SID** — Security Identifier, идентификатор безопасности учётной записи или группы. Принципал задачи — учётная запись её выполнения; он не тождествен владельцу дескриптора безопасности.
- **HRESULT** — 32-разрядный код результата Windows/COM; **exit code** — код завершения конкретного процесса. `LastResult` относится к результату задачи, а не к процессу команды Query.
- **UTF-8 / UTF-16LE** — кодировки Unicode с 8-битными единицами либо 16-битными единицами в порядке байтов от младшего к старшему. **DTD** — определение типа XML-документа; внешний resolver — механизм разрешения внешних ссылок.
- **OFF** — явное выключение автозапуска; **backend** — реализация платформенного механизма; **fixture** — контролируемый тестовый вход/окружение; **native** — реальное выполнение на соответствующей операционной системе.
- **Fingerprint** — отпечаток выбранных полей спецификации; **rollback** — попытка восстановления; **generation** — конкретное состояние определения задачи во времени; **lease** — кооперативная блокировка участвующих в протоколе писателей.
- **PR** — запрос на включение изменений; **Draft** — черновик; **PASS / FAIL** — успешный / неуспешный результат конкретной проверки. Источник, тестовая модель и результат нативного выполнения различаются.

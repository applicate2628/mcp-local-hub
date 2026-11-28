# R17: интерпретация единственного наблюдения отсутствующего объекта CIM

## Оглавление

1. [Переданное наблюдение](#observation)
2. [Интерпретация](#interpretation)
3. [Границы вывода](#limits)
4. [Первичные источники](#sources)

<a id="observation"></a>
## 1. Переданное наблюдение

Источник фактов этой секции — сообщение основного исполнителя об одном запуске; рецензент не повторял запуск и не перечитывал удалённую квитанцию. Наблюдатель SHA-256 `c1eb1a4172bf89a308e06c65c0134aed404ae66e54d2b61907676917da4b7cce` сначала прошёл `Parser.ParseInput`: 0 ошибок, 936 токенов. Затем завершился за **1,4817944000242278 с** с `completed=true`, `classification=ERROR_OBSERVED`. Windows PowerShell 5.1.22621.6133, ScheduledTasks 1.0.0.0; стандартный поток ошибок пуст, непосредственно созданный процесс завершён. Продуктовый код не выполнялся.

**Таблица 1 — наблюдённые типы и коды.**

| Шаг | Тип исключения | HRESULT, десятичный / шестнадцатеричный | Категория PowerShell |
|---|---|---|---|
| COM `GetTask` до | `System.IO.FileNotFoundException` | `2147942402` / `0x80070002` | 14 — `OperationStopped` |
| `Get-ScheduledTask`, буквальное имя и корень, `-ErrorAction Stop` | `Microsoft.PowerShell.Cmdletization.Cim.CimJobException` | `2148734209` / `0x80131501` | 13 — `ObjectNotFound` |
| COM `GetTask` после | `System.IO.FileNotFoundException` | `2147942402` / `0x80070002` | 14 — `OperationStopped` |

Обе контрольные цепочки одинаковы; цепочка команды получения содержит один элемент. Для всех цепочек `chain_truncated=false`. Наблюдатель не записал `NativeErrorCode`, `StatusCode` и числовые поля `ErrorData`: он извлекает их только для `CimException`, которого в наблюдённой цепочке нет. Это не доказывает отсутствие каких-либо внутренних данных в самом модуле. HRESULT — 32-разрядный код результата Windows; COM — компонентная объектная модель Windows; CIM — общая модель управленческих данных.

<a id="interpretation"></a>
## 2. Интерпретация

**Новый различающий результат получен:** при указанной версии и параметрах отсутствие буквального имени представлено ошибкой `CimJobException` с категорией `ObjectNotFound`. Успешного возврата пустого массива этот запуск не наблюдал. Число объектов, которые могли поступить до ошибки, после прерванного выражения не измерялось.

Значения категорий подтверждаются официальным перечислением: 13 — `ObjectNotFound`, 14 — `OperationStopped`; `ResourceUnavailable` имеет значение 21 [1]. Категория — общая диагностическая классификация, а не универсальное доказательство отсутствия именно задачи [2]. Поэтому различие категорий 14 у контролей и 13 у команды не делает результаты противоречивыми: контролируемое условие допускалось по `0x80070002` в цепочке, а не по категории.

`FileNotFoundException` использует `0x80070002` [3]. Напротив, `0x80131501` — общий `COR_E_SYSTEM`, используемый `SystemException`; его нельзя переименовывать в код отсутствующей задачи или сопоставлять с `ErrTaskNotFound` без дополнительных условий [4].

В официальном открытом исходном коде PowerShell класс `CimJobException` наследует `SystemException` [5]. `QueryJobBase.OnCompleted` умеет сформировать ошибку `ObjectNotFound` через `CimJobException.CreateWithFullControl` без вложенного исключения при результате поиска, для которого требуется ошибка отсутствия [6]. Это **согласуется** с наблюдённой одноэлементной цепочкой. Это объяснение возможного механизма, а не доказательство точного пути в установленном бинарном Windows PowerShell 5.1: его совпадение с процитированным открытым коммитом не проверялось. Полный идентификатор ошибки не сохранялся, поэтому конкретный `CmdletizationQuery_NotFound_TaskName` наблюдённым не объявляется.

Ожидаемая ошибка команды была перехвачена и превращена в разрешённые метаданные; поэтому пустой stderr и завершённый процесс совместимы с `ERROR_OBSERVED`. `completed=true` означает завершение наблюдения с обоими допущенными контролями; оно не означает успех миграции на CIM.

<a id="limits"></a>
## 3. Границы вывода

Для будущего адаптера подтверждена необходимость отдельной обработки ошибки на этапе получения объекта: одной проверки числа успешно возвращённых объектов недостаточно. Этот случай не даёт готового универсального отображения `CimJobException`, `ObjectNotFound` или `0x80131501` в `ErrTaskNotFound`.

Не проверены скрытые задачи, отказ доступа, недоступность службы/класса CIM, поиск по маске, произвольный Unicode, XML-экспорт и продуктовые потребители. Два контроля не доказывают атомарность интервала. Поведение без `-ErrorAction Stop` не измерялось. Общий `semantic_absence_verdict` остаётся `null`; производственный COM-экспорт сохраняется. Новых нативных запусков или изменений кода для этой интерпретации не было.

<a id="sources"></a>
## 4. Первичные источники

1. Microsoft. [ErrorCategory](https://learn.microsoft.com/en-us/dotnet/api/system.management.automation.errorcategory?view=powershellsdk-7.6.0). В справке указана применимость к Windows PowerShell 5.1.
2. Microsoft. [Windows PowerShell Error Records](https://learn.microsoft.com/en-us/powershell/scripting/developer/cmdlet/windows-powershell-error-records). Категория как общая диагностика; идентификатор и сообщение ошибки имеют разные назначения.
3. Microsoft. [FileNotFoundException](https://learn.microsoft.com/en-us/dotnet/api/system.io.filenotfoundexception?view=netframework-4.8.1). Код `COR_E_FILENOTFOUND=0x80070002`.
4. Microsoft. [SystemException](https://learn.microsoft.com/en-us/dotnet/api/system.systemexception?view=netframework-4.8.1). Код `COR_E_SYSTEM=0x80131501`.
5. PowerShell/PowerShell, коммит `ba2221aed027041b6620c5fe1c0675567f14452a`. [CimJobException.cs](https://github.com/PowerShell/PowerShell/blob/ba2221aed027041b6620c5fe1c0675567f14452a/src/Microsoft.PowerShell.Commands.Management/cimSupport/cmdletization/cim/CimJobException.cs). Объявление класса и `CreateWithFullControl`.
6. Тот же открытый коммит. [QueryJobBase.cs](https://github.com/PowerShell/PowerShell/blob/ba2221aed027041b6620c5fe1c0675567f14452a/src/Microsoft.PowerShell.Commands.Management/cimSupport/cmdletization/cim/QueryJobBase.cs). `OnCompleted`, формирование категории `ObjectNotFound`. Этот коммит не идентифицируется с установленной реализацией Windows PowerShell 5.1.

# A1: незамкнутая граница динамического Unwrap

Историческая записка по исходникам сохранена с исправленными публикационными ссылками. Последующий независимый аудит сырых событий находится в [итоговом отчёте](README.md#7-независимая-сверка-и-воспроизведение-аудита).

## Содержание

- [Зафиксированный результат](#зафиксированный-результат)
- [Точное происхождение вызовов](#точное-происхождение-вызовов)
- [Почему перечисление локальных реализаций отклонено](#почему-перечисление-локальных-реализаций-отклонено)
- [Минимальное следующее доказательство](#минимальное-следующее-доказательство)
- [Неизменность квалифицированного кандидата](#неизменность-квалифицированного-кандидата)
- [Источники](#источники)

## Зафиксированный результат

`TestWeeklyRefreshLockOrderInventory` остаётся **FAIL**. По сообщению владельца
квалификации, четыре корня направленных фикстур прошли, а фактическая проверка
производственного графа остановилась на единственной показанной границе:

```text
unresolved typed boundary from errnoIs at secure_create_client_config_windows.go:315:10 (func() error: local function or method has no indexed body)
```

Владелец сохранил 9 сырых событий этого запуска, код выхода 1, отсутствие
SKIP и таймаута; SHA-256 сырого результата:
`7b8d67ab89e4003de303533a5d9ddc19a815a4e542a79981954fe0fc7b78190d`.
Эта записка основана на чтении исходников; повторного исполнения или
независимого аудита сырого результата здесь не было. FAIL означает незакрытое
доказательство, а не установленную взаимную блокировку при выполнении.

## Точное происхождение вызовов

В `errnoIs` и `ntStatusIs` локальный интерфейс имеет вид
`interface { Unwrap() error }`. Переменная `u` получается утверждением типа
`cur.(unwrapper)`; динамический вызов `u.Unwrap()` не имеет одного
статически известного тела. `go/types` — стандартная библиотека информации
о типах Go — правильно связывает селектор с объявлением интерфейсного
метода текущего пакета. У этого объявления нет собственного тела [1].

**Таблица 1. Производственные места передачи ошибок.**

| Получатель | Место вызова | Источник аргумента |
|---|---|---|
| `errnoIs` | `isAlreadyExistsErr`, строки 298 и 301 [1] | Параметр `err` классификатора; происхождение не закреплено его типом `error`. |
| `ntStatusIs` | `isAlreadyExistsErr`, строки 292 и 295 [1] | Тот же параметр. |
| `isAlreadyExistsErr` | `secureCreateClientConfigIfMissingImpl`, строка 150 [1] | `renameErr` от `ntRenameRelativeNoReplace`. В текущем теле источники — `windows.UTF16FromString`, обёртка `fmt.Errorf`, `windows.NtSetInformationFile`, строки 239–272. |
| `isAlreadyExistsErr` | `mkdirOrVerifyRealDirWindowsObserved`, строка 161 [2] | Ошибка от `ntCreateRelativeObserved`. При доказанном `b=nil` сохраняется обычная ветвь `ntCreateRelativeWithShareAccessObserved`: преобразование имени, форматированная обёртка и `windows.NtCreateFile` [3]. |
| `isAlreadyExistsErr` | `openOrCreateWindowsAdoptLeaseLeaf`, строка 176 [4] | Ошибка от `ntCreateRelative`, строка 173. Это дополнительный производственный вызывающий, без заявления о его достижимости из текущего weekly-корня. |
| `ntStatusIs` | `isRetryablePostRenameOpenErrWindows`, строки 644–653 [3] | Классификатор получает как ошибку от `ntOpenRelative` через строку 624, так и `herr` от `postRenameOpenFailHook()` через строки 610–612. |
| `ntStatusIs` | `windowsRepairErrIsSharingViolation`, строка 203, и `windowsRepairErrIsAccessDenied`, строка 207 [5] | Их вызывающий `openWindowsStateFileForDACLRepair` передаёт ошибку от `ntOpenRelativeWithShareAccess`, строки 115–122. |

`postRenameOpenFailHook` объявлен как переменная `func() error`. Комментарий
обозначает её как тестовый шов и предписывает оставлять `nil` в production [6].
Само название и комментарий не доказывают её значение для анализатора.
Поэтому утверждение «все входы `ntStatusIs` всегда являются только нативными
ошибками» нельзя вывести из сигнатуры или перечисления вызывающих. Наличие
этого пути также не означает, что он достигнут из текущего weekly-корня:
это требует отдельного доказательства достижимости и происхождения аргумента.

## Почему перечисление локальных реализаций отклонено

Рассматривалось узкое правило для двух владельцев — `errnoIs` и `ntStatusIs`:
сохранить рёбра ко всем локальным реализациям точного метода `Unwrap() error`,
включая методы значений, указателей и встроенных полей. Оно сохранило бы
скрытую блокировку внутри локального метода, но **не замкнуло бы множество
динамических целей**.

Импортированные проектные пакеты также содержат реализации. Например,
`scheduler.EntrypointTaskTransitionError.Unwrap` и
`scheduler.EntrypointTaskPartialProgressError.Unwrap` возвращают `Cause`
[7]. Их наличие не доказывает передачу таких объектов в текущий классификатор,
но опровергает попытку считать инвентарь одного пакета полным множеством
возможных методов для произвольного значения `error`.

В существующем `weeklyBuildTypedCallGraph` точный локальный `*types.Func`
получает ребро к телу; неразрешённый метод текущего пакета получает ошибку.
Для `*types.Func` из другого пакета ветвь заканчивается без ребра и без
ошибки (`weekly_refresh_atomic_test.go`, строки 1989–2006) [8]. В этой ветви
нет отдельного точного перечня разрешённых импортированных методов. Это
существующий предел анализа; переносить его на неизвестную динамическую
цель `Unwrap` как новое обоснование безопасности нельзя. Импортированный
метод не становится безопасным только из-за другого имени пакета.

Предложенное перечисление локальных реализаций не внесено. Исключение по
имени `Unwrap`, общая уступка для хранилища или изменение производственного
кода ради зелёного результата также не внесены.

## Минимальное следующее доказательство

Следующий самостоятельный срез должен отслеживать **происхождение ошибки
для конкретного вызова**, а не присваивать всему классификатору свойство
«нативный»:

1. Привязать аргумент `error` к конкретному результату исходного вызова.
   Проверять назначения, передачу параметров и возвратов, обёртки
   `fmt.Errorf` с `%w`, ветви наблюдения и подменяемые обработчики.
2. Закрепить точные внешние источники/обёртки и границы их допуска; неизвестный
   импортированный проектный метод не считать терминальным автоматически.
3. Сохранять тела всех доказанно возможных локальных методов и транзитивных
   помощников. Для методов указателей, встроенных полей и обобщённых типов
   либо иметь соответствие исходному телу, либо оставлять границу неизвестной.
4. После каждого `Unwrap()` сохранять доказанное множество источников
   следующего элемента цепочки. Не смешивать известный контекст с неизвестным
   контекстом другого вызывающего. Назначения, захват изменяемого значения,
   неоднозначные обёртки, callback-ошибки и незамкнутые циклы должны завершать
   доказательство отказом.
5. Проверить отрицательные случаи: блокировка настроек в локальном
   `Unwrap`, неизвестная импортированная реализация, ошибка от callback,
   подмена аргумента и смешение контекстов. Положительный случай должен
   подтверждать только точную допущенную нативную цепочку.

Общее свойство «нераспознанная интерфейсная диспетчеризация остаётся
неразрешённой» уже покрывают случаи `same-shaped unreviewed helper remains
unresolved`, `another parameter remains unresolved` и `expanded interface
remains unresolved` в `TestWeeklyTypedExportXMLBoundary` [8]. Поэтому нового
характеризующего теста в замороженные файлы не добавлено.

## Неизменность квалифицированного кандидата

После разбора `Unwrap` оба файла A1 не изменялись. Повторно прочитанные
контрольные суммы совпали с `a1-inputs-formatted.json` [9]:

- `weekly_refresh_atomic_test.go` —
  `002951c839ab9b929913b5bea068356778f2ff8f37b12083b641baa851df8ac0`.
- `weekly_refresh_call_graph_origins_test.go` —
  `12da1b0b5a720197dd2e6d57b6363acca9efeda60a5f088e903498c008089ac2`.

## Источники

1. [secure_create_client_config_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/203c7bacef98c0445549512a2b6cab61eca6dcd1/internal/api/secure_create_client_config_windows.go): `secureCreateClientConfigIfMissingImpl`, `ntRenameRelativeNoReplace`, `isAlreadyExistsErr`, `errnoIs`, `ntStatusIs`.
2. [secure_create_client_config_parent_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/203c7bacef98c0445549512a2b6cab61eca6dcd1/internal/api/secure_create_client_config_parent_windows.go): `mkdirOrVerifyRealDirWindowsObserved`.
3. [secure_write_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/203c7bacef98c0445549512a2b6cab61eca6dcd1/internal/api/secure_write_windows.go): `openPostRenameVerifyHandleWindows`, `isRetryablePostRenameOpenErrWindows`, `ntCreateRelativeObserved`, `ntCreateRelativeWithShareAccessObserved`.
4. [adopt_lease_owner_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/203c7bacef98c0445549512a2b6cab61eca6dcd1/internal/api/adopt_lease_owner_windows.go): `openOrCreateWindowsAdoptLeaseLeaf`.
5. [state_dacl_repair_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/203c7bacef98c0445549512a2b6cab61eca6dcd1/internal/api/state_dacl_repair_windows.go): `openWindowsStateFileForDACLRepair` и два классификатора ошибок.
6. [secure_write_client_config.go](https://github.com/applicate2628/mcp-local-hub/blob/203c7bacef98c0445549512a2b6cab61eca6dcd1/internal/api/secure_write_client_config.go): объявление `postRenameOpenFailHook`, строки 130–135.
7. [entrypoint_migration_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/203c7bacef98c0445549512a2b6cab61eca6dcd1/internal/scheduler/entrypoint_migration_windows.go): методы `Unwrap`, строки 87 и 899–903.
8. `inputs/a1/internal/api/weekly_refresh_atomic_test.go` в [сыром архиве](raw-evidence.zip): замороженный анализатор и существующие направленные фикстуры.
9. `a1-inputs-formatted.json` в [сыром архиве](raw-evidence.zip): точные размеры и контрольные суммы квалифицированных входов A1.

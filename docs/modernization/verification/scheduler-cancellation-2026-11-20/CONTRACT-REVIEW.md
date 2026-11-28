# Контракт отмены Windows ListContext

**Объект:** `applicate2628/mcp-local-hub`, PR (запрос на включение изменений) #660, точный коммит `f5fb00370a833255eacb992a7cd32f2e68da7399`. **Статус:** независимое чтение исходников и локального `r19-cancellation/candidate.patch`; исполнение и публикация не выполнялись. Конкретных дефектов локального кандидата в проверенном объёме не найдено.

## Оглавление

1. [Подтверждённый дефект и последствие](#1-подтверждённый-дефект-и-последствие)
2. [Минимальная правка и совместимость](#2-минимальная-правка-и-совместимость)
3. [Проверяемые инварианты кандидата](#3-проверяемые-инварианты-кандидата)
4. [Существующие CLI-тесты и реальные побочные эффекты](#4-существующие-cli-тесты-и-реальные-побочные-эффекты)
5. [Границы доказательств](#5-границы-доказательств)
6. [Источники и прочитанные навыки](#6-источники-и-прочитанные-навыки)

## 1. Подтверждённый дефект и последствие

Две независимые операции стирают программно различимую причину отмены [1]:

- `internal/scheduler/scheduler_com_windows.go:132–133`: после `cmd.Output()` ненулевая `ctx.Err()` заменяется `fmt.Errorf("%w: timeout", ErrUnavailable)`. И `context.Canceled`, и `context.DeadlineExceeded` исчезают из дерева ошибок; слово `timeout` также не отличает явную отмену от истечения срока.
- Там же, строка 153: `fmt.Errorf("%w: bridge execution: %v", ErrUnavailable, err)` сохраняет исходную ошибку раннера только как текст. Даже исправление первой точки само по себе не восстановит `errors.Is` у публичного вызывающего кода.

`windowsScheduler.ListContext` в `scheduler_windows.go:367–385` передаёт контекст в `schedulerCOM`, при ошибке возвращает `nil, err` без дополнительной обёртки. `List` использует `context.Background()`, `ListContext(nil, ...)` тоже сохраняет существующий допуск пустого контекста. Интерфейс `ContextLister` объявлен в `scheduler.go:145–149`; его комментарий обещает возможность отмены нижележащей операции, но не описывает точную форму цепочки ошибок [2].

**Наблюдаемое следствие в потребителе, установленное по коду:** `reconcileSchedulerListFnDefault` передаёт контекст и правильно оборачивает результат через `%w` (`supervise_reconcile_ipc.go:56–69`). Обработчик `handleReconcile` сначала проверяет `errors.Is(schedErr, context.DeadlineExceeded/Canceled)` (367–368), а потом допускает `ErrUnavailable` и продолжает с `schedTasks=nil` (370–372). Поэтому утраченная причина выбирает ветвь допустимой недоступности вместо уже существующего результата `RECONCILE_TIMEOUT`; его кадр содержит `Retryable=true`, `Final=true` (179–183). Это причинный вывод из условий ветвления, а не новый нативный опыт или утверждение о конкретном уже наблюдённом ответе работающего продукта [3].

Историческая серия публичного Go на `1b870e04...` уже фиксировала оба `errors.Is(context.*)=false` при `ErrUnavailable=true`. Она не переносится на новый коммит как выполненный тест. Дефект текущего `f5fb003...` подтверждён отдельно чтением и сверкой байтов [1, 8].

## 2. Минимальная правка и совместимость

Локальный кандидат меняет ровно причинные точки:

```go
if err := ctx.Err(); err != nil {
    return schedulerCOMResponse{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
}
// ...
if err != nil {
    return schedulerCOMResponse{}, fmt.Errorf("%w: bridge execution: %w", ErrUnavailable, err)
}
```

Вторая обёртка должна сохранять именно возвращённую раннером ошибку; замена её новым чтением `ctx.Err()` потеряла бы самостоятельные ошибки раннера при живом контексте. В первой точке `ctx.Err()` читается один раз. Возвращать только `ctx.Err()` недостаточно: это убрало бы прежнюю классификацию `ErrUnavailable`.

Публичные сигнатуры, зависимости, срок `schedulerCOMDeadline=15*time.Second`, процессный запуск, формат JSON (JavaScript Object Notation — формат обмена данными), PowerShell-сценарий и семантика частичных ответов остаются прежними. Ненулевая ошибка раннера по-прежнему отвергает даже ответ с `OK=true` и строками. Продуктовый раннер по-прежнему скрывает самостоятельную ошибку исполнения за `ErrUnavailable: bridge`, а ошибку декодирования — за `ErrUnavailable: protocol`; кандидат не добавляет к выдаче содержимое потоков процесса [1].

Множественные `%w` поддерживаются стандартной библиотекой и доступны при заявленном `go 1.26.2` [5, 9]. `errors.Is` проходит дерево ошибок. При этом прямой `errors.Unwrap` поддерживает только `Unwrap() error`, а множественный `%w` предоставляет `Unwrap() []error`: это изменение формы дерева, которое следует назвать явно. В изученных потребителях классификация идёт через `errors.Is`. Полный аудит всех внешних потребителей не выполнялся; поисковые результаты по основной ветке использовались только для нахождения файлов, после чего релевантные файлы читались на точном `f5fb003...` [3, 10].

Общая точка `schedulerCOM` используется и другими Windows-операциями. В частности, `RunOwned` заранее остановленный контекст возвращает непосредственно как `ctx.Err()` (`scheduler_owned_run_windows.go:25–38`); этот ранний путь кандидат не меняет. Для ошибки уже начавшегося обращения через общий мост причина станет доступна вместе с `ErrUnavailable`. Не следует объявлять единый новый контракт ранней отмены для всех методов [4].

## 3. Проверяемые инварианты кандидата

**Таблица 1 — точный объём двух новых тестовых функций, пять конечных случаев.**

| Тест / случаи | Проверка | Какую границу различает |
|---|---|---|
| `TestSchedulerCOMPreservesRunnerErrors/canceled`, `/deadline`, `/bridge` | Подменённый раннер возвращает `fmt.Errorf("runner: %w", cause)` одновременно с частичным успешным ответом. Итог обязан удовлетворять `errors.Is(err, ErrUnavailable)` и `errors.Is(err, cause)`; `response.OK=false`, `response.Task=nil`, `response.Tasks=nil` | Отдельно обнаруживает потерю причины во внешнем `%v`; случай с самостоятельным sentinel исключает исправление только через чтение внешнего контекста |
| `TestWindowsListContextPreservesStoppedContext/canceled` | Настоящий производственный раннер, контекст отменён до вызова; `ctx.Err()==context.Canceled`; возвращены `ErrUnavailable` и `context.Canceled`, задачи отсутствуют | Публичный путь через обе обёртки; обнаруживает неисправленную нижнюю точку |
| `TestWindowsListContextPreservesStoppedContext/deadline` | Настоящий раннер, срок `time.Unix(1, 0)` уже истёк до вызова; `ctx.Err()==context.DeadlineExceeded`; возвращены `ErrUnavailable` и `context.DeadlineExceeded`, задачи отсутствуют | Отдельная стандартная причина без ожидания таймера |

Старый тест общего `bridge lost` не удалён без замены: его утверждение `ErrUnavailable` перенесено в более сильный `/bridge` случай с проверкой исходной причины и отбрасывания ответа. Подмены `schedulerCOMRun` восстанавливаются через существующий `t.Cleanup`; новые тесты не используют `t.Parallel` и не добавляют совместно исполняемые горутины.

Проверка `tasks==nil` в кандидате соответствует нынешней строке `return nil, err`. Это более узкое утверждение о реализации, чем общий публичный инвариант «ноль задач»; его прохождение не следует описывать как новое общеобязательное соглашение о nil-срезе. Обратные проверки отсутствия противоположной причины и поддержка пользовательского `context.Cause` в кандидате не заявлены. Текущая правка сохраняет стандартный `ctx.Err()`, а не расширенный пользовательский cause.

Ни тесты, ни отрицательный контроль в этом независимом чтении не запускались. Различающий отрицательный контроль при последующем исполнении: исходный `f5fb003...` обязан провалить проверки сохранения причины; вариант с исправленной только внешней обёрткой по-прежнему должен проваливать оба публичных случая.

## 4. Существующие CLI-тесты и реальные побочные эффекты

CLI — интерфейс командной строки; IPC — межпроцессное взаимодействие. Ниже перечислены точные существующие имена из `internal/cli/supervise_reconcile_ipc_test.go` [6].

**Таблица 2 — адресные регрессионные проверки потребителя.**

| Имя, строка | Что действительно проверяет | Подмены и ограничения |
|---|---|---|
| `TestReconcileIPC_HandlerTimeoutCancelsSchedulerList`, 1176 | Передачу контекста через настоящий `reconcileSchedulerListFnDefault` в `ContextLister`, наблюдение `DeadlineExceeded` и кадр `RECONCILE_TIMEOUT` | Фабрика scheduler возвращает fake; `ListContext` ждёт `ctx.Done()` и возвращает `ctx.Err()`. `apply=false`. Глобальный срок временно сокращён до 25 мс; если предшествующая обработка исчерпает его до fake, тест способен не достичь канальной проверки — это условие возможного отказа, не наблюдённый здесь отказ |
| `TestReconcileIPC_HandlerTimeout`, 1236 | Сохранение штатного 25-секундного срока и передачу deadline в list-замену; результат dry run | Подменена вся list-функция; она возвращает одну строку. `apply=false`. Проверяется остаток срока в `(24s,25s]`, поэтому тест зависит от времени подготовки до точки наблюдения |
| `TestHandleReconcileFrameFailureRetainsCancellation`, 2028 | Сохранение одновременно `context.Canceled` и причины ошибки записи, удержание кадра `RECONCILE_TIMEOUT`, единственную попытку записи | List-функция подменена, соединение выдаёт заданную ошибку. `apply=true` с `SettleTarget`; реальный Serena admission вызывается, но fixture оставляет `deps.supervisorLock=nil`, и `validateHeldSupervisor` возвращает `SERENA_REPAIR_ORIGINAL_SUPERVISOR_REQUIRED` до registry/manifest/preparation acquisition [7] |

Эти тесты **не подменяют все побочные эффекты**. Общий `newReconcileTestFixture` (183–301) создаёт реальный временный каталог с Windows DACL (списком управления доступом), пишет `supervisor-intent.json`, открывает журнал, создаёт настоящий EventLoop и при завершении отменяет его с ожиданием. Обычные события машины состояний записываются fake-обработчиком вместо spawn/terminate, но reap-события направляются настоящим методам. Первые два теста используют настоящий preview Serena: он может создавать/удерживать файловые блокировки и читать registry, хотя `commit=false` пропускает запись результатов ремонта. Реестр перенаправлен fixture через `LOCALAPPDATA`/`XDG_STATE_HOME` [6, 7].

`TestMain` находится в `internal/cli/settings_registry_test.go:292–510`, без build tag. До `m.Run()` он устанавливает no-op `reconcileSpawnFn` (414), изоляцию IPC (416), создаёт и защищает временный root (432–440), задаёт `api.SetDaemonStateRootForTest(tmp)` (441), подменяет frozen-stdio admission (446), применяет окружение client-config sandbox (449) и его аудит (493), задаёт `MCPHUB_STATE_DIR_OVERRIDE` (478–485). В `testhooks.go:227–242` тестовый override действительно записывается в `daemonStateRootOverride` и защищён `testing.Testing()` [11].

Для этих внутрипроцессных сценариев `test_state_path_env` не является обязательным условием перенаправления: и обычный `state_paths_prod.go:38–42`, и `state_paths_envfallback.go:53–56` сначала применяют уже установленный override. Тег добавляет переносимое через окружение перенаправление дочерних исполняемых файлов; в трёх выбранных сценариях такой дочерний `mcphub` не является проверяемым участником. Значения `TEMP`/`TMP` должны указывать в разрешённую область на D **до запуска TestMain**: собственный `os.MkdirTemp` выполняется раньше sandbox environment [11, 12].

Для узкой проверки потребителя достаточно первых двух тестов. Третий отдельно проверяет доставку отмены при ошибке записи кадра и не является доказательством настоящего моста Планировщика/PowerShell. Нативные lifecycle-тесты, создающие/запускающие задачи, в эту рекомендацию не входят.

После дополнительного чтения изоляция первых двух сценариев подтверждена по конкретным путям: `ClientConfigSandboxEnvironment` строит все перенаправления внутри переданного root; `PrepareClientConfigSandboxDirectories` проверяет весь список на принадлежность root до `MkdirAll`; `DefaultRegistryPath` использует заданный fixture `LOCALAPPDATA`. `NewAPI` только выделяет структуры и фиксирует время. Пользовательских изменений разрешений или системных настроек вне новых временных roots в прослеженной ветви не найдено. Это чтение исходников, а не нативное наблюдение отсутствия любых внешних эффектов. Точные фрагменты сохранены рядом в `CLI-SOURCE-SNIPPETS.md` [15–17].

## 5. Границы доказательств

Заранее остановленный контекст не допускает запуска дочернего процесса в `os/exec.Cmd.Start`: проверка `ctx.Done()` находится до `os.StartProcess`. Поиск `powershell.exe` может выполняться, но это не запуск PowerShell. Поэтому публичные случаи кандидата проверяют реальный Go-путь обработки ошибок, не обращаясь к работающему COM (Component Object Model — компонентная объектная модель Windows) Планировщика [13].

Новые тесты не квалифицируют отмену после начала PowerShell, достижение внутреннего срока 15 секунд, завершение потомков, освобождение всех дескрипторов или конечный срок `Output()`. `CommandContext` по умолчанию отменяет основной процесс; `WaitDelay` не задан, и ожидание потоков может зависеть от их закрытия потомками. Это установленное ограничение механизма, а не экспериментально найденная утечка в данном продукте. Добавлять Job Object, менять wait/close, вводить отдельные горутины или объявлять устранение этих ограничений для исправления классификации причин не требуется [1, 14].

Проверка кандидата проведена по требованиям, корректности двух обёрток, сохранению классификации, отсутствию новых зависимостей и изменений жизненного цикла. Компиляция, `go test`, `go vet`, `-race`, удалённые команды, изменения рабочих веток, установки, merge/rebase/cherry-pick и публикация в этом задании не выполнялись. Параллельная работа Rust wait/close не затрагивалась.

## 6. Источники и прочитанные навыки

**Таблица 3 — привязка исходников.** Локальные три сохранённых файла сопоставлены через `git hash-object` с объектами каталога GitHub точного `f5fb003...`; все три совпали. Другие названные исходники прочитаны через commit-pinned URL (адрес с фиксированным коммитом).

| Файл | Git blob |
|---|---|
| `internal/scheduler/scheduler_com_windows.go` | `b8413756cb7c613644fa2dea73a4812eeeb96b80` |
| `internal/scheduler/scheduler_com_windows_test.go` | `db4d939a672a62927f12e5ef007f90a32871b5e9` |
| `internal/scheduler/scheduler.go` | `198e6b7cb3cef3b234a346b1c8b92f9001dac2d4` |
| `internal/scheduler/scheduler_windows.go` | `7d19a7cbd928e61a82b1bc58d0d5bdf3b6a5070c` |

1. [scheduler_com_windows.go, точный #660](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/scheduler/scheduler_com_windows.go).
2. [scheduler_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/scheduler/scheduler_windows.go) и [scheduler.go](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/scheduler/scheduler.go).
3. [supervise_reconcile_ipc.go](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/cli/supervise_reconcile_ipc.go).
4. [scheduler_owned_run_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/scheduler/scheduler_owned_run_windows.go).
5. [go.mod](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/go.mod).
6. [supervise_reconcile_ipc_test.go](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/cli/supervise_reconcile_ipc_test.go).
7. [serena_intent_repair.go](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/api/serena_intent_repair.go).
8. [Исторические RESULTS публичной Go-серии](https://github.com/applicate2628/mcp-local-hub/blob/2b5e44f92e10d27af9d559bd3e66ed9fb2e03840/docs/modernization/verification/scheduler-public-go-2026-11-19/RESULTS.md); в этом чтении история взята из `r17-continuation/plan.updated.md:985–1007`, первичный журнал заново не исполнялся и не анализировался.
9. [Стандартная библиотека: fmt.Errorf](https://pkg.go.dev/fmt#Errorf).
10. [Стандартная библиотека: errors.Is и errors.Unwrap](https://pkg.go.dev/errors), [Context.Err](https://pkg.go.dev/context#Context).
11. [CLI TestMain](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/cli/settings_registry_test.go), [testhooks.go](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/api/testhooks.go), [HardenedTempDir Windows](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/api/apitest/hardened_tempdir_windows.go).
12. [state_paths_prod.go](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/api/state_paths_prod.go), [state_paths_envfallback.go](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/api/state_paths_envfallback.go).
13. [Исходник Go: os/exec.Cmd.Start](https://go.dev/src/os/exec/exec.go) — проверка уже завершённого контекста перед `os.StartProcess`.
14. [Стандартная библиотека: os/exec.CommandContext и Cmd.WaitDelay](https://pkg.go.dev/os/exec).
15. [client-config sandbox environment](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/clients/config_path_sandbox_testenv.go).
16. [DefaultRegistryPath](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/api/workspace_registry.go).
17. [NewAPI](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/api/api.go).

Фактически прочитаны `go-project-and-api-design/SKILL.md` и `references/api-evolution.md`, `go-concurrency-lifecycle/SKILL.md`, `go-performance-and-diagnostics/SKILL.md`. Применены сохранение контрактов ошибок/отмены, раздельное описание причин и жизненного цикла, минимальная область правки и пределы диагностических выводов. Измерений производительности не было, улучшение времени/ресурсов не утверждается.

# #659: force-stop через исходного владельца процесса

## Содержание

1. [Проблема и контракт](#проблема-и-контракт)
2. [Изменение](#изменение)
3. [Наработка Wave](#наработка-wave)
4. [Проверки](#проверки)
5. [Границы и остаточный план](#границы-и-остаточный-план)
6. [Термины и источники](#термины-и-источники)
7. [Дополнение WL-04 после публикации](#дополнение-wl-04-после-публикации)

## Проблема и контракт

WL-04: наблюдение числового PID, владельца порта, пути образа или SID не даёт права завершать процесс. Между наблюдением и последующим открытием процесса PID может быть повторно использован. Исправление сохраняет единственного исходного владельца процесса и Windows Job и передаёт запрос ему.

**Таблица 1. Производитель, потребитель и проверяемый факт.**

| Участок | Контракт |
|---|---|
| API → Supervisor | Отдельный `force_stop` v1 по аутентифицированному IPC. Status и команда используют одно соединение; native peer проверяется до команд |
| Status → цель | Полный кортеж: task, PID, StartedAt, PIDGeneration, ExpectedPort. Несовпадение или старый протокол — отказ |
| FIFO → creator | Проверяется текущий runtime tracker, running intent и исходный сохранённый `ProvisionalProcess`; hydrated PID не приобретает полномочий |
| Creator → waiter | Одна terminal completion cell; force и естественное завершение присоединяются к одному исходному результату |
| Waiter → событие | Только первоначальный waiter публикует реальный выход. Новый искусственный crash/restart не создаётся |
| Closure → порт | Root exit, закрытие дерева и handles — отдельные факты. Состояние порта наблюдается отдельно и может уже отражать successor |

## Изменение

Команда выполняется в существующей FIFO контроллера. Успешный результат требует private terminal closure и совпадения исходной идентичности. Повтор точной уже принятой цели возвращает её сохранённый результат; смена tracker на successor не перенаправляет запрос. При потере ответа клиент не повторяет действие. Отмена присоединения не стирает исходного владельца или причину ошибки.

Force не записывает `Desired=stopped`; обычная политика повторного запуска сохраняется. Обычная terminate-ветка и force фиксируют свой выбор под одним mutex, чтобы не выполнить два разных завершения одного поколения. Контекст клиента ограничен 12 секундами, серверного запроса — 5 секундами. Это ограничения ожидания, а не доказательство возможности прервать любой синхронный вызов ОС.

Два прежних числовых leaf API теперь возвращают типизированный `ErrOriginalProcessOwnerRequired`: `taskkillProcessTreeByPID` и non-Windows `stopForceKillSupervisorPIDTree`. Windows-адаптер приходит в первый leaf. Production callers сохраняют отказ в своих error/warning каналах, включая install/remove/restart. Он не становится разрешением повторить действие через порт. Предупреждения говорят `cleanup incomplete`; обещание отсутствующего CLI-флага удалено.

Изменённые исторические тесты успешного PID fallback заменены проверками нового контракта. Модельные classifier seams не выдаются за нативное доказательство. Тест реальных отказных leaf сначала проверяет отсутствие побочного действия в их теле и только затем вызывает положительный тестовый PID.

## Наработка Wave

Из дерева `c3b8c802be190c335b84ff7323a14d9eb63518b6` адресно перенесён consumer `daemonWaitExitCode`. Ранее waiter читал исходный `CreatorRootExitSnapshot`, но код для события вычислял только через `exec.ExitError`; известный native exit code мог превратиться в `0`. Теперь Known snapshot имеет приоритет, включая известный ноль и полный Windows DWORD. Unknown не заимствует код из чужого ExitError; прежний fallback используется только для legacy `cmd.Wait`. Wait error и gate закрытия исходных ресурсов сохранены.

Регрессия адаптирована к уже допущенному `TestFormatChildExit_RealProcessShowsExitCode`. Native force fixture использует уже допущенный `TestProductionTerminateFn_HelperSleep`. Новые роли помощников и второй package-root не добавлены. Остальные Wave TestMain/secrets фрагменты не подменяют более строгие текущие контракты.

## Проверки

**Таблица 2. Фактические результаты на базе `a3568dfe3426afaca4176d57a2310097e5214e77`.**

| Проверка | Результат и область |
|---|---|
| API force/протокол/отказные leaf/seal | PASS, Go package 14.479 s |
| Промежуточный native fixture после переиспользования helper | PASS, 0.543 s |
| Причинный контроль старой классификации exit | Ожидаемый FAIL: Known 7/259/2147483649/4294967295 и сохранение кода при ошибке observer |
| Окончательный CLI force + terminal cell + root exit | PASS, Go package 0.862 s |
| Исходные native команды | Root reaped, Job active-zero, handles closed, оба reader joined; не осталось неподтверждённых команд |
| Независимое ревью | Полный первоначальный delta и отдельный последний consumer/helper delta проверены другим исполнителем; Root выполнил собственное чтение |

Подробные argv, исходные hashes, stdout/stderr hashes, длительности команд и receiving находятся в [машинном evidence](force-stop-audit-evidence-20261125.json). Время Go package отличается от времени компиляции/команды и полного контроллера. Два unsupported-platform теста закономерно пропущены на Windows; POSIX-поведение не объявлено исполненным.

Реальный native fixture проверяет исходный creator, закрытие дерева и одну публикацию выхода. Wire fixture использует `net.Pipe` и проверяет framing, exact-generation, старый протокол, неверный/неполный/потерянный ответ без повторного действия. Это разные уровни доказательства.

## Границы и остаточный план

1. Проверить окончательную композицию с Scheduler, route aggregate и helper dispatch, затем обязательные Go gates.
2. Отдельно квалифицировать полный успешный обмен по реальному аутентифицированному Windows named pipe. Текущий direct-controller native тест этого не заменяет.
3. Для автоматической очистки уже удаляемого descriptor заморозить полное исходное поколение до изменения intent, затем обратиться к тому же исходному creator. Нынешний PID-only снимок даёт только явный отказ/предупреждение; нельзя выбирать новую цель по свежему status после удаления.
4. На этой базе CLI `stop` не имеет `--force`; реализованы существующий API `StopWithOpts(Force=true)` и отдельный IPC verb. Новая пользовательская команда не заявлена.
5. Не переносить focused PASS на полный Product или установленный Hub. Публикация — существующий #659; merge запрещён.

## Термины и источники

PID — числовой идентификатор процесса; SID — идентификатор пользователя Windows; IPC — межпроцессное взаимодействие; FIFO — последовательная очередь поступивших действий; CLI — командная строка; DWORD — 32-разрядное беззнаковое число Windows.

Источники: текущие `internal/api/supervisor_ipc_force_stop_client.go`, `supervisor_force_stop.go`, `stop_force_supervisor.go`; `internal/cli/supervise_force_stop.go`, `supervise.go` и соответствующие регрессии; указанный commit Wave. Применены уже прочитанные Go Concurrency Lifecycle, Go Service Boundaries, Go Testing and Verification, architecture review, source-driven development и Ponytail: единый владелец, причинная ошибка, ограниченное ожидание, reuse существующих helper/creator и разделение исполнения от модели.

## Дополнение WL-04 после публикации

Исходная опубликованная база — `9043acae5a778b9b123b195d3c04cb213e5e568d`, тот же #659. Следующий delta подготовлен отдельно; приведённые выше исторические PASS к нему не относятся. **gofmt всех 15 Go-файлов и Windows cross-compile пакетов `./internal/api`, `./internal/cli`: PASS** с Go 1.27.4, `GOOS=windows GOARCH=amd64 CGO_ENABLED=0`, `-mod=readonly`, offline module/cache environment. Компиляция не исполняет тесты. **Функциональные Go tests и Windows execution нового delta: NOT_RUN исполнителем; выполняет Root.** `git diff --check` выполнен без ошибок. Исходные hashes, команды и границы свидетельств находятся в [замороженном manifest](force-stop-removed-frozen-20261125.json). Новый PR и merge не выполнялись.

### Уточнённый разрыв

Workspace install, global install и uninstall снимали только task→PID до удаления, затем вызывали reconcile и отказной числовой leaf. После удаления повторный обычный `force_stop` не мог пройти running-descriptor admission; новый status мог уже описывать successor. Дополнительно workspace install держал intent flock до возврата из `executeParsed`, вызывая reconcile раньше release. Apply-path reconcile содержит `ConvergeGeneratedSupervisorImage` с собственным intent mutation lock, поэтому возврат зависел от lock, удерживаемого ожидающим клиентом. При обычном uninstall stop tombstone и Scheduler Delete предшествовали удалению descriptor; захват исходного creator должен предшествовать и этим операциям.

### Контракт изменения

**Таблица 3. Узкий маршрут удаления.**

| Фаза | Реализация и проверяемый факт |
|---|---|
| До первой мутации | `captureRemovedSupervisorTargets(ctx, []SupervisorDaemon)`; полный descriptor fingerprint и полный runtime tuple; native owner/peer validation, hello, status и preparation на одном соединении |
| Admission на FIFO | `force_stop_removed` / `retain_removed` сохраняет ссылку на уже существующий `daemonCreatorOperation`; не вызывает terminate и не меняет intent |
| Durable removal | Существующий intent flock и writer; authoritative removed set пересчитывается под lock; новая/изменённая строка не наследует capture |
| Release | Workspace `WithIntentLockHeld` завершается и исходный release/Close принимается до IPC; на ошибке release cleanup не заявляется успешным |
| Reconcile | Если он первым выбирает stop точного captured original, существующий termination guard вызывает тот же original creator и terminal completion cell; PID leaf не получает полномочий |
| Complete | `complete_removed` принимается только на первоначальном соединении, с первоначальным capture/tuple; отсутствие descriptor проверяется в cache и свежим существующим intent reader; read error или reappearance — отказ |
| Original result | Сохранённая terminal cell переживает tracker retirement; root exit, tree closure и handle closure относятся к исходному creator; исходный waiter остаётся единственным издателем child exit |
| Close/error | Все captures закрываются на успехе, no-op, rollback и ошибках; неоднозначный ответ потребляет capture, повторный exchange/redial запрещён; никакого PID/port fallback |

Новый verb нужен для безопасной совместимости: прежний `force_stop` v1 игнорирует неизвестные JSON-поля. Если отправить ему preparation только как новое поле, старый сервер начнёт termination. `force_stop_removed` у старого dispatcher возвращает unknown-command до эффекта; новый dispatcher запрещает removal-phase в обычном `force_stop`.

Подготовка всего предварительного набора имеет один общий срок 12 секунд; каждый сетевой exchange наследует оставшийся срок и собственный верхний предел 12 секунд, серверный FIFO-запрос — существующие 5 секунд. Оставлен исходный server idle timeout 60 секунд: долгая install-операция может потерять capture и получит честный cleanup-incomplete. Длительности ограничивают ожидания; они не объявляют синхронные вызовы ОС прерываемыми. Не добавлены таймеры keepalive, повторное получение authority или новые process/Job owners.

Для строгого consumer, включая отдельную ARA-01 migration-композицию: вызвать `captures.validate(originalDescriptor)` **до** durable removal для каждой выбранной строки, затем после removal/release/reconcile — `captures.complete(ctx, originalDescriptor)`. Ошибка Complete запрещает успешную private publication и требует сохранения pending-state. Best-effort install/uninstall сохраняет существующие warning-каналы. ARA-01 caller принадлежит другому change owner и не включён в этот delta.

### Регрессии и отрицательные контроли

**Таблица 4. Подготовленные проверки; исполнение NOT_RUN.**

| Проверка | Что проверяет |
|---|---|
| Existing removed-target API tests, обновлённые с PID mocks на captured completions | Три caller path; capture перед flock; completion после удаления и reconcile; retained-but-not-removed capture закрывается; workspace reconcile/complete выполняются без intent lock |
| `TestUninstallCapturesOriginalBeforeStopIntentAndSchedulerDeletion` | Manifest-backed и retired-manifest uninstall; capture предшествует stop tombstone и Scheduler Delete |
| `TestRemovedSupervisorCapture*` | Новый/изменённый descriptor не наследует capture; отказ подготовки, root-only result, lost response остаются warnings; no-op не теряет connection close |
| `TestRemovedForceStopOlderServerRefusesPreparationBeforeEffect` | Модель прежнего сервера: unknown verb отказ, счётчик прежнего destructive effect остаётся нулевым |
| `TestRemovedForceStopLostResponseConsumesOriginalConnection` | Модель framing: потеря ответа не допускает второй exchange |
| `TestForceStopRemovedCaptureAdmissionAndOriginalResult` | Нет preparation / другое соединение / target / close / reappearance / read failure — без native action; reused numeric PID и новый tracker не заменяют исходную failure cell |
| `TestForceStopRemovalReconcileJoinsOriginalFailure` | Reconcile получает сохранённую исходную ошибку; не входит в legacy PID action и не стирает cause |
| `TestForceStopAuthenticatedWindowsPipeOriginalCompletion` | Реальный защищённый Windows named pipe, native peer validation, production dispatcher/FIFO, настоящий child/Job, original completion и один waiter exit; explicit, removed-client-first и removed-reconcile-first |

Native fixture переиспользует уже допущенный `TestProductionTerminateFn_HelperSleep` и original `fixtureSpawnWorkers`. Контролируемыми остаются только посторонний port inventory для portless fixture и момент подтверждающего reap scan. Действительный original creator receipt, pipe и identity validators не подменяются. Проверка неправильного sidecar creation-time обязана отказать до подключения; положительный путь обязан использовать ровно одно реальное соединение. Это не квалификация установленного Product, Scheduler service mutation или полноценного user uninstall.

Подготовлены отдельные отрицательные patches: `force-stop-removed-old-verb-negative-20261125.patch` и `force-stop-removed-reconcile-negative-20261125.patch`. Они не входят в production delta. Первый направляет preparation в старый destructive verb; второй отключает передачу retained original в reconcile guard. Ожидаемый FAIL должен быть записан отдельно и не заменяет положительное исполнение. Не применять оба в положительную копию.

### Собственное статическое ревью и навыки

Проверены call order до первой stop/descriptor mutation, authoritative removed set, old-server compatibility, generation/connection ownership, mutex order (capture → records; native calls вне records mutex), сохранение natural-exit completion, original waiter publication, Close на всех возвращениях и отдельные error/warning каналы. Создание successor не получает capture; новый PID/status не запрашивается в Complete. После gofmt Windows cross-compile двух затронутых пакетов прошла; реальные Windows assertions пока требуют независимого Root-run.

Фактически прочитаны и применены: Architecture Review — identity/trust boundaries и staged contract; Go Concurrency Lifecycle — один owner/terminal join, bounded waiting, capture lifetime; Go Security Hardening и process-execution-boundary reference — original creator вместо PID reacquire, native peer gate; Go Testing and Verification и deterministic-concurrency reference — causal barriers, отделение моделей от kernel evidence; Ponytail — reuse existing FIFO, creator, waiter, fingerprint и fixture; ранее прочитанные Go Data Consistency / Service Resilience — отсутствие effect replay при неопределённом результате. Навыки не перечитывались ради количества.

**Остаточный план:** Root независимо читает замороженный delta и сверяет hashes → последовательно исполняет API/CLI focused и реальные named-pipe cases → отдельно проверяет отрицательные контроли → принимает receiving/ресурсное завершение → составляет с ARA-01 caller и обновляет evidence существующего #659. Полная композиция и Product qualification остаются отдельными воротами; merge запрещён.

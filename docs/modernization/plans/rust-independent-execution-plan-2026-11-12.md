# Полный план работ MCPHub → Rust

**Редакция остатка:** 14 ноября 2026 года. **Статус:** шесть отслеживаемых этапов, подробно проверяемых по существующим RX-контрактам. Полнота плана означает решение по каждому R01–R24, а не обязательство переписать все 24 участка. Исследования и тестовый код не означают готовую миграцию.

**Цель:** получить проверенные улучшения надёжности и производительности, после принятия #616 в master; интегрировать только полезные реализации у существующих владельцев.

**Архитектура:** web и предметное Go-ядро сохраняются. Rust — нативные ресурсы и подтверждённые интенсивные операции. Один конкретный ресурс имеет одного владельца. Vendor SDK и чужие проекты не переписываются; собственные COM-мосты не переносятся на другой язык.

**Спецификация:** [аудит и архитектурный план](rust-cross-platform-plan-2026-11-12.md), [схема](rust-cross-platform-boundaries.svg), [WP-11B–E](wp-11a-to-11f.md), [governance](../governance.md). Этот файл в PR #623 остаётся единственным планом: таблица 1 ведёт текущий остаток, RX-разделы сохраняют подробные условия приёмки, а коммиты соответствующих PR хранят доказательства. R01–R24 отличаются от исторических findings R-01–R-36.

**Исполнение:** последовательно внутри задачи, параллельно только независимые области. Применять Executing Plans, TDD для новой логики, подходящие архитектурные/системные навыки и самостоятельное ревью перед заявлением о готовности. Обязательное независимое ревью не заменяется авторским и не запускается за счёт платной квоты без разрешения.

## Оглавление

0. [Текущий остаток: шесть этапов и ближайшая работа](#remainder)
1. [База, разрешения и рабочий цикл](#baseline)
2. [Полное покрытие R01–R24](#coverage)
3. [Начальные измерения](#measure)
4. [Нативные ресурсы и потребители](#native)
5. [Файлы, идентичность и секреты](#files)
6. [COM, Python и сторонние поставки](#dependencies)
7. [Остальные вычислительные и потоковые участки](#dataplane)
8. [Supervisor и desktop-интеграция](#late)
9. [Интеграция, откат и завершение](#integrate)
10. [Проверки, методики и источники](#review)


<a id="remainder"></a>
## 0. Текущий остаток: шесть этапов

**Контрольная точка:** master `17bc78fc602efa204a74596409c939a3bf1ddc42` после merge #616; документационная база #623 `21442f167b02bb3de5b2ba42a9df1adfed672254`. Нативный owner — #625 `c5672f49`; межъязыковая граница — #631 `eb4ca1d4`; наблюдения исходного процесса — опубликованный #644 `8e80fad6` [9–11]. #631 и #644 — разные дочерние ветки #625; проверка их совместимости не является слиянием.

**Отслеживаются шесть результатов, а не 24 независимых переписывания.** Основной этап назначен каждому R01–R24 ровно один раз: 2 + 4 + 6 + 4 + 6 + 2 = 24. RX-07 Grafana сохранён дополнительно. Подробные критерии ниже действуют полностью; укрупнение не отменяет платформенные, ресурсные или продуктовые проверки. Сейчас ни один из шести этапов целиком не закрыт.

**Таблица 1. Единственная текущая доска остатка.**

| № | Результат и основное покрытие | Уже подтверждено | Остаток и критерий завершения | Зависимости / состояние |
|---|---|---|---|---|
| **1** | **Нативный владелец и идентичность** — R01, R03 | #625: исходные process/thread/Job, допуск, выбранные fault/crash-сценарии. #644: время создания, конкретный Job и wait, 62 native-проверки в каждом профиле. #631: реальные Go/Rust-вызовы | TokenUser SID и очистка; отдельно версионированное наблюдение через ABI; сохранение исходной Go capability и SID-formatting receipt, отказ при недоступной возможности; оставшиеся применимые acquisition/setup/failure-классы. Закрывается после принятия конкретного контракта и квалификации заявленной возможности у реального потребителя | **В работе.** Ближайший пункт — SID в #644. ABI следует за native-контрактом |
| **2** | **Потоки и существующие потребители** — R02, R08, R11, R21 | #631: временные inheritable-копии Go streams, отмена наблюдателя, пять конечных pipe-профилей. #629 уже содержит #630 | Довести применимые input/output/backpressure/cancel/drain/join и concurrent-inheritance условия; затем подключать по одному launcher/runner/LLDB/LSP-потребителю без нового job owner. Закрывается по доказанному retain-Go либо принятому механизму каждого выбранного потребителя | Rust-подключение после нужных возможностей **1**; проверка объединённого #629 независима. **Частичные результаты есть** |
| **3** | **Файловые и чувствительные операции** — R04, R05, R06, R07, R18, R22 | #627: read-only Windows pilot, 15 native-проверок | Полный применимый read-контракт; публикация/блокировки, тот же удерживаемый executable image, streaming/archive outcomes, карта копий тестовых секретов. Для каждого R — обоснованный итог; при переносе сохранены identity/caps/no-follow/close и разные visible/durable/verified/cleanup результаты | Чтение можно продолжать отдельно. Запись/артефакты — после сверки конкретных transaction/WP-контрактов. **Пилот есть, приёмка открыта** |
| **4** | **Устранение собственных COM/shell/Python-обвязок** — R16, R17, R19, R20; дополнительно RX-07 | #628: измерение двух process-полей; архитектурные границы выделены | Полный caller/field map; scheduler по операциям read/run/stop/delete; собственные ctypes через принятые owners; отдельное pre-main доказательство CST; квалификация того же Grafana upstream binary. Закрывается по конкретным устранённым зависимостям либо обоснованному сохранению Go/C, без новых vendor SDK и скрытого COM fallback | Наблюдение/scheduler могут завершиться без Rust. ctypes зависит от нужных частей **1–3**. **Контрактный остаток открыт** |
| **5** | **Измеренные решения о переносе** — R09, R10, R12, R13, R14, R15 | #624/#626: CMake и relay baselines; #629: общее исправление relay | Допустимые реальные нагрузки и сравнение current → minimal Go → связный Rust там, где данные обосновывают пилот. Учитывать весь executable, materialization, bounded publication, FFI и очереди. Закрывается шестью проверяемыми retain/improve/replace решениями с нужной семантической квалификацией | Baseline независим; R10 ждёт нужного WP-11E, R14 — общего snapshot. Серии на одном хосте последовательно. **Данные частичные, решения открыты** |
| **6** | **Архитектурное завершение и продуктовая приёмка** — R23, R24; общие gates всех этапов | Принятый master известен; отдельные source-проверки не равны установленному продукту | Сравнить актуальный supervisor и обоснованность native desktop; квалифицировать принятые изменения по заявленным ОС/архитектурам, review/recovery/package/install условиям, удалить только доказанно заменённое. Закрывается 24 итогами с дальнейшими владельцами, выполненными gates принятых изменений и явным решением RX-07 | После соответствующих решений **1–5**; обязательная приёмка каждого включаемого потребителя выполняется до его включения, а не откладывается до конца. **Открыт** |

### 0.1. Как обновлять статус без раздувания плана

- После завершённого изменения обновлять в его PR доказательства, source SHA, команды, результат и ограничения; в таблице 1 менять только соответствующую строку и ближайшее действие.
- Допустимые итоги кандидата: **сохранить действующую реализацию**, **минимально улучшить её**, **принять квалифицированный Rust-перенос**; это включает обоснованное сохранение Go или C. «Нужны данные», «не запускалось», «unsupported» и «заблокировано» сами по себе не означают завершение требуемой возможности. Обоснованное решение не реализовывать необязательную возможность фиксирует её границу явно.
- Один PR сохраняет одну ответственность. Не создавать PR на каждый профиль теста, отказ, платформу или файл журнала; не объединять независимые owners ради меньшего числа PR. Использовать существующие #623–#629/#631/#644; #630 уже включён в #629 и отдельно не интегрируется.
- Одновременно выполнять одну активную реализацию и, при пользе, одну независимую область анализа. Пропускать заблокированную платформу можно для продолжения независимой работы, но её статус остаётся not-run/blocked.
- Статусы общей ветки и accepted master сверять перед затрагивающим их действием. Защищённая параллельная работа сейчас включает #632–#643; её файлы/checkout/результаты не переписывать. Наличие принятого #616 не очищает оставшиеся broad-suite, native/installed/release gates.
- Установка, оплачиваемые/квотные внешние запуски и явный bot/CI dispatch по-прежнему требуют отдельного действующего разрешения. Укрупнение плана не меняет эти границы.

### 0.2. Ближайший исполнимый пункт этапа 1

**Владелец изменения:** существующий native identity PR #644, `experiments/rust-process-owner/`. Применить API/Interface Design, Rust Resource Lifecycle и Unsafe Checker; сверять Win32-контракт с первичными Microsoft источниками.

1. Получить **TokenUser SID** из удерживаемого исходного process handle, открыть собственный token handle только с правом запроса. TokenUser не заменять TokenOwner; не открывать процесс заново по PID.
2. Выполнить ограниченное получение/проверку/копирование SID; отдельно сохранить ошибки наблюдения и закрытия token handle. Не вернуть успешный SID при отказе закрытия, не повторить закрытие через Drop после неоднозначного результата; сохранить исходного владельца процесса.
3. Выполнить различающие native-тесты правильного пользователя, отказов запроса/очистки и сохранности owner, проверить существующий Go/Rust consumer, провести своё и отдельное ревью. Опубликовать код и нужные evidence в том же PR. Политика допуска и SID-formatting receipt остаются следующей проверяемой границей.

**После этого:** в #631 — отдельно версионированное foreign observation с сохранением ABI1, затем реальный product capability/consumer. Перенос полей в структуру или вычисление digest не создаёт полномочий исходного владельца.

### 0.3. Аудит укрупнения перед продолжением

**Таблица 2. Найденные расхождения и принятые исправления плана.**

| Наблюдение | Последствие | Исправление |
|---|---|---|
| Исторический checkpoint ещё называет identity неопубликованной; прежний старт начинается с #624 | Повтор уже выполненной работы и неверная текущая зависимость | Указан #644 и следующий native SID-пункт; исторический [branch audit](../verification/branch-audit-2026-11-14/RESULTS.md) сохранён как снимок своего времени |
| Подробные RX-разделы и старые unchecked-списки затрудняют обзор текущего остатка и могут восприниматься как новая очередь | Лишние пилоты и дублирование статусов | Шесть результатов в одной таблице; нижние RX-списки стали критериями, retain-Go остаётся полноценным итогом |
| #631 уже проверяет конечные pipe/cancellation-сценарии; #630 уже объединён с #629 | Повторное создание механизмов и отдельного шага интеграции | В остатке перечислены только непокрытые продуктовые условия; #629 проверяется как единый PR |
| Поля identity, два поля #628 и исторические timings можно ошибочно принять за полную приёмку | Неполная capability, legacy snapshot или новый performance-вывод без данных | Ограничения перенесены в строки 1, 4, 5; результаты привязаны к испытанным source SHA |
| При сжатии легко потерять RX-07, pre-main CST, retained image, секреты и раздельные outcomes | Формально короткий, но неполный план | RX-07 сохранён отдельно; все 24 R назначены один раз; подробные требования и native/product/independent gates оставлены |

**Результат аудита:** самостоятельная проверка покрытия и зависимостей плюс два независимых read-only разбора плана/топологии. Финальное чтение нового текста исправило неточное описание прежнего плана и расширило общий перечень итогов для допустимого сохранения C bootstrap. Шесть этапов выбраны как минимальная практичная группировка с разными условиями завершения: дальнейшее слияние смешало бы ресурсный контракт, устранение зависимости и измерительное решение. Это не утверждение о математически минимальном числе операций. Отдельное ревью не выдаётся за native-прогон или обязательную итоговую независимую приёмку.

<a id="baseline"></a>
## 1. База, разрешения и рабочий цикл

**Таблица 3. Исторические исходные состояния 12 ноября; текущая база указана в разделе 0.**

| Объект | Закреплённый исходный снимок | Назначение |
|---|---|---|
| master | `76d11b33bd37e30125525ce8e0e582122f29e91b` | Независимые измерительные копии |
| #623 | `1ad6134691c739d220e6ff88552932ba5da82f47` до этой редакции | Единственный PR плана; расширять на месте |
| #624 | `64d21d4ecb1660a5748d9cebee0528fd92ed16b7` начального benchmark | Развивать в этой ветке, не дублировать PR |
| #616 | `92bc059c87d958db2474aee5c61c560c3e48e86f` | Read-only эталон foreground/admission, не автоматически принятая production-база |
| selected activation / 0.4.60 | Отдельная незавершённая работа | Наследовать будущие контракты; повторно сверить затрагиваемую область перед интеграцией |

### RX-00 — общий порядок для каждого задания

- Проверить current HEAD, изменения владельца и существующие PR. Сначала выяснить, не выполнена ли задача уже параллельно; не повторять старую реализацию.
- Работать в собственной изолированной копии. Допустим полный immutable archive без изменения исходного Git worktree/refs; неполный модуль с потерянными `replace`-зависимостями не использовать. Source SHA и тестовый diff фиксировать раздельно.
- Записать входы, хеши, команды, toolchain, ОС/архитектуру, workload, ограничения CPU/RAM, принадлежащие тесту каталоги и процессы. Сохранить короткий журнал результата и следующих действий вне trash.
- Выполнить baseline только затронутой области. Для новой логики сначала различающий отрицательный тест, затем минимальное исправление, положительный прогон, собственное ревью требований/корректности/безопасности/совместимости/сложности.
- Зафиксировать исход: оставить Go; минимально улучшить Go; развить Rust-пилот; квалифицировать интеграцию; нужны конкретные данные. При новой информации исправлять этот план и причины решения, не подменять историю проверок.

Это общие шаги каждой задачи, ниже не копируются многократно. Команды, возвращаемые значения и ABI окончательно закрепляются у принимаемого владельца перед кодом, а не выдумываются для ещё не принятой интерфейсной границы.

**Разрешения и ограничения:** не менять #616, чужие PR/активные деревья, рабочие конфигурации, секреты, автозапуск и серверы. Не запускать GUI-окна, vendor jobs, модели, bots/CI или платные агенты. Не устанавливать toolchain/зависимости без отдельного разрешения: Go с локальным toolchain/cache, Rust `--offline --locked` после согласованного lockfile. Недоступная среда блокирует только зависимый опыт. Публикация собственных результатов в согласованных PR не означает разрешения merge/release/установки.

Предлагаемый предел незавершённой разработки — две независимые области. Нагрузочные серии на одном физическом хосте выполняются последовательно. Не выполнять blanket-kill по имени процесса; прекращать только собственный подтверждённый test-child. Подтверждённо ненужное переносить в trash после сохранения необходимых evidence; корзину не очищать.

**Текущий порядок:** активный пункт этапа 1 ниже; этапы 3–5 могут продвигаться независимо после своих входных условий. Начальный порядок с расширением #624 и relay baseline уже выполнен частично и не повторяется автоматически. Реализация Rust-relay/парсера следует за измеренным основанием.

<a id="coverage"></a>
## 2. Полное покрытие R01–R24

**Таблица 4. Каждый кандидат имеет исполняемую задачу и самостоятельный итог.**

| Кандидат | Задание ниже | Решение до начала переноса |
|---|---|---|
| R01 Process owner | RX-02 | Первый изолированный pilot |
| R02 Pipes/capture/drain | RX-03/io | Общий механизм, не новый supervisor |
| R03 Process/IPC identity | RX-03/identity | Не смешивать наблюдение и полномочия |
| R04 Anchored read | RX-04/read | Параллельный read-only pilot |
| R05 Publish/write/remove | RX-04/write | После актуального transaction-контракта |
| R06 Locks/leases | RX-04/locks | Только часть нужной файловой операции |
| R07 PE/image admission | RX-08/image | Проверить bounded parser и тот же исходный образ |
| R08 Launcher | RX-03/consumers | Потребитель проверенного owner |
| R09 Relay | RX-01/relay → RX-08/relay | Сначала полная стоимость executable |
| R10 StdioHost/HTTPHost | RX-08/transport | После WP-11E и профиля |
| R11 LLDB/LSP pumps | RX-03/io | Сохранять финальный ответ и EOF |
| R12 CMake traces | RX-01/CMake → RX-08/parse | Parse + indexes + bounded publication |
| R13 CMake graph/log parsers | RX-08/graph | Не дублировать существующего semantic owner |
| R14 Registry lookup | RX-08/registry | Сначала общий snapshot и оптимизированный Go |
| R15 Events/logs | RX-08/events | Политика потерь не меняется от языка |
| R16 Native observations | RX-01/observe → RX-05a | Устранить WMI/CIM shell без ослабления identity |
| R17 Scheduler/autostart | RX-05b | Отказ от COM; Rust необязателен |
| R18 ZIP/copy/hash | RX-08/artifacts | Сохранить результаты публикации и очистки |
| R19 CST bootstrap | RX-06/bootstrap | До-main гарантии; небольшой C stub допустим |
| R20 Python ctypes | RX-06/native | Только наш OS-код, не vendor SDK |
| R21 Runners | RX-03/consumers | Один механизм для существующих владельцев заданий |
| R22 Secrets lifetime | RX-08/secrets | Карта копирования; не новая криптография |
| R23 Supervisor целиком | RX-09/supervisor | Позднее сравнение сложности, не обязательная миграция |
| R24 Tray/activation | RX-09/desktop | Только доказанная native-потребность, web сохраняется |

RX-07 — отдельное сокращение Python-launcher готовым upstream binary; не ещё один Rust-кандидат. Mesh IO и NablaMesh MCP — отдельная задача [NablaMesh #183](https://github.com/applicate2628/NablaMesh/pull/183), не дополнительный декодер Хаба.

<a id="measure"></a>
## 3. Начальные измерения

### RX-01/CMake — развитие #624

**Файлы:** `internal/vcpkgmcp/cmaketrace/parse_benchmark_test.go`, `trace_benchmark_contract_test.go`. Производственные `parse.go`, `cmaketrace.go`, `projection.go` и `publicresult/publicresult.go` не менять для удобства benchmark.

**Интерфейсы:** `parseTraceStream(ctx, reader, Limits)`; `Trace(ctx context.Context, args Args, deps Deps) Result`; `publicresult.MarshalIndent(result)`. Последний — реальный bounded encoder публикации, а не неограниченный `json.Marshal` внутреннего Result [1–4].

- Построить детерминированные входы 1 000/50 000 записей: повторяющиеся и уникальные пути, смешанные команды, длинные Unicode-аргументы; 200 000 повторяющихся записей — отдельный scale-case. Случаи превышения лимитов не выдавать за обработку полного входа.
- Проверить поля и порядок записей, include-chain, полный индекс строк/файлов, `defer`, flags и round-trip JSON на малом входе. Большой результат обязан сохранять честную информацию об усечении и projection omissions. Existing malformed/version/limit/cancel regressions остаются.
- Измерять отдельно `BenchmarkParseTraceStream`, `BenchmarkTracePublicResult`, `BenchmarkTraceResultJSON`, `BenchmarkTraceEndToEnd`. Последний — file→Trace→bounded JSON, НЕ весь MCP/HTTP/solver. Генерация/запись/preflight вне таймера; не удерживать ненужный preflight при измерении live memory.
- Выполнить smoke всей матрицы; timing-серии минимум из 10 повторов, начиная с `-benchtime=1s`. Увеличить выборку при недостаточном качестве данных. Фиксировать GOMAXPROCS; профили собирать отдельно от timing-серий.
- Отдельным процессом измерить peak RSS при заранее созданном входе и удержанном результате. Allocation traffic не называть peak/live memory. Добавить разрешённую реальную обезличенную трассу, когда доступна; до этого выводы synthetic-only.

```sh
go test ./internal/vcpkgmcp/cmaketrace ./internal/vcpkgmcp/publicresult -count=1
go vet ./internal/vcpkgmcp/cmaketrace ./internal/vcpkgmcp/publicresult
go test ./internal/vcpkgmcp/cmaketrace -run '^$' -bench '^Benchmark(ParseTraceStream|TracePublicResult|TraceResultJSON|TraceEndToEnd)$' -benchtime=1x -count=1 -benchmem
go test ./internal/vcpkgmcp/cmaketrace -run '^$' -bench '^Benchmark(ParseTraceStream|TracePublicResult|TraceResultJSON|TraceEndToEnd)$/^repeated$/^records_50000$' -benchtime=1s -count=10 -benchmem
```

**Выход:** корректный стенд, CPU/allocation profiles, отдельная память, raw-series и границы выводов. #624 расширяется на месте. Первые исторические два повтора не означают завершение этой задачи.

### RX-01/relay — существующий транспорт без смены алгоритма

**Файлы:** `internal/daemon/relay.go`, `relay_test.go`, `relay_qualification.go`, `internal/cli/relay.go`; новые тестовые `relay_benchmark_test.go`/`relay_load_test.go` только если нужного теста ещё нет. **Интерфейс:** `(*HTTPToStdioRelay).Run(context.Context) error`.

- Локальный fake HTTP endpoint и owned pipes: initialize/session, JSON/SSE, уведомления, разрыв, idle stdin, slow output, cancellation, финальная строка перед EOF. Проверять ID/сессии и отсутствие потери/дублирования, не только exit code.
- Воспроизведённые дефекты отделить от benchmark и Rust-изменения, сверив параллельные исправления.
- `BenchmarkHTTPToStdioRelay` измеряет локальную обработку. Отдельный процессный опыт — 1/8/32 экземпляра на payload 1 КиБ и 64 КиБ, плюс отдельные границы фактических лимитов. Учитывать server/driver отдельно.
- Записать idle CPU/RSS, readiness, throughput, queue wait, errors, p50/p95/p99 и sample count. Для open-loop задержку отсчитывать от запланированного поступления. Не делать вывод по p99 при недостаточной выборке.

**Выход:** установленная причина затрат и решение, открывать ли RX-08/relay. Размеры — тестовая матрица, не утверждение о production-нагрузке. Никаких настоящих поставщиков/моделей.

### RX-01/observe → RX-05a — системное наблюдение

**Файлы:** `internal/api/processes.go`, `process_probe_exec.go`, `internal/process/process_rss_windows.go`, `pid_identity*`; эксперимент `<scratch>/rx-observe/`.

- Составить caller/field map: PID, image, creation identity, owner, RSS; отдельно диагностические поля и authority перед effect. Проверить текущие batch/cache и частоту повторного запроса.
- На своих test-child измерить существующий и минимальный native Go/Rust пути без COM/WMI. Не собирать команды и окружение чужих процессов; не читать недокументированный PEB ради заполнения поля.
- Проверить exit между наблюдениями, denied, unsupported и неизвестное поле. Неполный snapshot не даёт права kill и не означает отсутствия.
- Принимать минимальную реализацию, устраняющую зависимость; Go-native допустим окончательно. Включение в health/cleanup/install — отдельная интеграция.

<a id="native"></a>
## 4. Нативные ресурсы и потребители

### RX-02 — foreground contract и Rust-пилот (R01)

**Эталон:** `internal/process/foreground_windows.go` на принятом master `17bc78fc602efa204a74596409c939a3bf1ddc42` после #616: `RunForegroundAdmitted(...) (ForegroundResult,error)`. Собственные/заимствованные ресурсы, root result, admission, cancel и incomplete settlement сохраняются раздельно [5].

- Заморозить один сценарий partial-create→admission→execute→wait/stop→settle. Инъекции отказов acquisition/admission/resume/wait/close и ожидаемый владелец фиксируются до реализации.
- Переиспользовать test-child: exit 0/7, final response, idle read, stop signal и один собственный потомок. Отрицательные проверки должны обнаруживать неправильный result/owner, а не только наличие функции.
- Реализовать `<scratch>/rx-process-owner/{Cargo.toml,Cargo.lock,src/lib.rs,src/windows.rs,tests/lifecycle.rs}`. Не создавать production Cargo workspace, scheduler, retry-policy и универсальный plugin API.
- Проверить обычный выход, отказ допуска до ResumeThread, отмену на фазах, late error, retained incomplete owner, borrowed stdio, concurrent stop/query. PID-reopen не заменяет исходный handle.
- Проверить узкий Go↔Rust ABI в тестовом host: allocator/lifetime, версии/размеры структур, допустимые конкурентные вызовы, отсутствие retained Go pointers/unwind. `Drop` не доказывает wait/kill. Не делать fallback-запуск через Go после unknown outcome.
- Для Linux/macOS реализовывать только явно определённую возможность. Не вызывать Go/Python/allocator/locks в post-fork child; strict unsupported отказывать до эффекта. На недоступной платформе — not-run, не PASS.

**Проверка:** `cargo test --manifest-path <scratch>/rx-process-owner/Cargo.toml --offline --locked` на каждой доступной целевой ОС, затем native fault evidence. Требования runtime/toolchain и конкретные dependencies записать до запуска; отсутствие cache не разрешает установку.

**Выход:** проверенный механизм и конкретные дополнительные гарантии владения; запуск в рабочем Хабе ещё не разрешён его тестами.

### RX-03/io — каналы и двусторонние потоки (R02, R11)

**Файлы:** `internal/daemon/host_original.go`, `internal/process/contained_stream*`, `internal/lldb/bridge.go` и существующие consumers.

- Зафиксировать владельца каждого конца pipe и каждого reader/writer; для каждой блокировки назвать событие прекращения. Отдельно: message not sent / partial write / fully sent / result observed.
- Добавить native tests early peer exit, idle stdin, заполненная очередь, медленный читатель, окончательный ответ перед EOF, cancellation и авария ребёнка. Заимствованные потоки не закрывать ради выхода теста.
- Расширить проверенный owner R01 каналами только там, где это создаёт завершённый контракт. Сохранить результат процесса отдельно от результата drain/join/close; лимиты bytes/concurrency и retained owner после timeout.

**Выход:** общий reusable I/O lifecycle; перенос MCP JSON-семантики сюда не включён. Исправление зависания в Go допустимо раньше Rust.

### RX-03/identity — идентичность и полномочия (R03)

**Файлы:** `internal/api/supervisor_status_server_identity_windows.go`, `internal/process/pid_identity*`, `internal/gui/probe*`, текущие IPC adapters.

- Разделить observation-only capability, исходного creator и права управляющего клиента; перечислить сведения, которые нельзя восстановить из PID/JSON.
- Испытать замену executable/endpoint, exit/reuse PID, смену generation, denied token и смену owner между probe/effect на собственных фикстурах.
- Перенести только нативную capability-операцию в R01/R03; policy/admission остаётся Go. Windows pipes и Unix peer credentials имеют отдельную квалификацию.

**Выход:** ни snapshot, ни произвольный Rust pointer не усиливают полномочия; stale observation не управляет новым процессом.

### RX-03/consumers — launcher и инструментальные runners (R08, R21)

**Файлы:** `cmd/mcphub-launcher/main_windows.go`, `internal/cli/supervisor_launch*`, `internal/{cbuild,gdb,vtune,oneapirun,drmemory,kimi,sonnet}`.

- Для каждого изменяемого runner записать исходный job owner, время жизни запроса/задания и effective launch path. Выбрать одного потребителя за PR.
- Подключать существующего владельца к принятому R01/R02, не создавать новый runner/job state. Сохранить CUI/GUI console intent, argv/env, stdout/stderr/EOF и executable identity.
- Тестами доказать: отмена наблюдателя не уничтожает долговечное задание; late cleanup не стирает outcome; нет orphan/повторного запуска и неожиданного окна.

**Выход:** одинаковый проверенный механизм у нескольких потребителей, не семь независимых Rust-реализаций. Настоящие model/vendor jobs и квоты не используются для тестов без отдельного разрешения.

<a id="files"></a>
## 5. Файлы, идентичность и секреты

### RX-04/read — закреплённое чтение (R04)

**Файлы:** `internal/api/state_read_beneath_root_windows.go`, `state_read_inode_anchor*`, платформенные аналоги. **Входной контракт:** `ReadStateFileBeneathRootNoFollow(ctx, root, relativeComponents, expectedSHA256)` сверить на принимаемой базе.

- На owned fixture root проверить bounded read, identity, no-follow, hash, права и close-error; fixture paths не указывают на реальное состояние Хаба.
- Проверить подмену компонента пути, symlink/reparse, denied, oversized file, non-ASCII и lossless Unix byte paths; не подменять anchored operation сочетанием canonicalize→open.
- Сравнить Go и `<scratch>/rx-file-read/`; выделить OS syscall cost и memory отдельно. Реализация не меняет vault/registry/schema.

**Выход:** квалифицированный read-контракт либо оставить Go. Это не приёмка записи.

### RX-04/write и RX-04/locks — публикация и удержание (R05, R06)

**Файлы:** `internal/api/secure_write_{windows,posix}.go`, `secure_create_client_config*`, `internal/clients/config_lock.go`, действующие lease-helpers. **Зависимость интеграции:** затрагиваемые WP-11D/0.4.60.

- Зафиксировать preimage/expected identity, временный файл, синхронизацию, условную публикацию и cleanup; различить visible/durable/verified/settled. Не создавать второй журнал транзакций.
- Ввести инъекции crash/failure до и после publish, partial write/flush, подмену destination, foreign writer, stale lock и timeout. Проверять фактические bytes/metadata и сохранность чужого объекта.
- Переиспользовать имеющуюся lock-библиотеку и owner; Rust guard допускается только внутри той же операции. Не переписывать `third_party/gofrs-flock` и не выдавать локальный mutex за межпроцессную блокировку.
- Проверить filesystem-specific гарантии Windows/Linux/macOS; неподдержанные network/mount cases не получают локальные гарантии автоматически.

**Выход:** один безопасный механизм publish/lock. Ошибка после commit не объявляется rollback; сохранённый результат и cleanup возвращаются раздельно.

### RX-08/image — bounded executable admission (R07)

**Файлы:** `internal/binaryadmission/windows_pe.go`, `windows_versioninfo.go`, `source_prepare*`.

- Создать corpus положительных/повреждённых PE и VERSIONINFO: offsets, длины, арифметическое переполнение, лишние секции, Unicode, version/subsystem mismatch. Удерживать тот же исходный файл на всём пути проверки.
- Сравнить текущий bounded Go-parser с минимальным улучшением и только затем Rust; fuzz oracle сравнивает supported/unsupported/error и extracted facts, а не отсутствие panic.
- Подключение к launcher/setup отдельное: checksum доказывает identity, не trusted publisher; PE-факты не становятся proof Unix process identity.

**Выход:** ограниченный parser/retained-image путь или обоснованное сохранение Go; без нового общего загрузчика.

### RX-08/secrets — время жизни секретов (R22)

**Файлы:** `internal/secrets/vault.go`, `resolver.go`, потребители env/IPC.

- Проследить на тестовом секрете все копии plaintext, string conversion, logging и передачу subprocess. Не читать реальные секреты.
- Сначала устранить ненужные копии/логи у их владельца в Go. Нативный secret buffer допустим, только если потребители действительно могут его использовать без постоянного обратного копирования.
- Проверить ошибку decrypt/resolve, cancellation, late failure и освобождение; не обещать полное стирание всех копий из managed memory.

**Выход:** доказанное сокращение lifetime/copy footprint либо сохранение Go. Криптография/age/формат vault не переписываются.

<a id="dependencies"></a>
## 6. COM, Python и сторонние поставки

### RX-05b — устранение собственных COM-мостов (R17)

**Файлы:** `internal/scheduler/scheduler_windows.go`, `scheduler_com_windows.go`, `internal/autostart/`.

- По каждой операции Create/Import/Export/Status/List/Run/Stop/Delete заполнить current contract→documented system interface→gap. Definition и live state не смешивать.
- На безопасных fixtures проверить XML UTF-16/UTF-8, локали, absent/denied/unavailable, неправильного владельца и изменившееся определение.
- Отдельными PR сначала чтение, затем guarded launch и разрушительные действия. Не объявлять query-then-run атомарным, не заменять gap COM/WMI-binding, Registry Run или недокументированным TaskCache.
- Нативная приёмка только в собственных разрешённых задачах ОС; сохранить OFF, clean-exit/crash, logout, missing manager и stored-not-active. Реальную конфигурацию пользователя не менять.

**Выход:** конкретная операция без собственного COM с прежними guarantees; Go допустим окончательно. `schtasks` не обещает отсутствие COM внутри ОС.

### RX-06/native и RX-06/bootstrap — собственный Python/C (R20, R19)

**Файлы:** `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_*windows.py`, `cst_saved_field_transfer.py`; `native/cst-runtime/mcphub_cst_runtime.c`.

- Для ctypes вызовов выделить acquisition/transfer/close owner и обязательства fail-path; согласовать с RX-02/03/04, не зеркалировать job/provenance/session state.
- Перенести один завершённый механизм, оставив thin Python-клиента и SDK поставщика. Проверять partial handle transfer, denied path, deadline и отсутствие сохранённых временных Python-указателей.
- Для bootstrap отдельно доказать порядок до Python/import/main, loader/наследование/отзыв полномочий; маленький C stub оставить, если Rust-runtime не обеспечивает эти же условия.
- Повторить safe no-launch contract tests и только затем отдельно разрешённые vendor/native сценарии. Test-success branch не выдавать за готовый CST-worker.

**Выход:** меньше собственного unsafe/ctypes обслуживания без собственного CST SDK. PyO3/embedding CPython не добавляются по умолчанию. Снятие GIL не доказывает thread-safety SDK.

### RX-07 — убрать Python-launcher готового upstream binary

**Владелец:** актуальная строка Grafana в принимаемой версии каталога; не править v2 параллельно мигрирующему v3.

- Закрепить тот же upstream release/assets/hash и CLI/stdio/tools/auth contract. Наличие release asset не является испытанием executable.
- Подготовить одну проекцию каталога и тест существующего installer/admission, без нового downloader и форка сервера.
- Разделить archive inspection и разрешённый запуск. Не скачивать/устанавливать пакеты и не подключать реальную Grafana с секретом без действующего разрешения.

**Выход:** тот же официальный сервер без лишней Python-оболочки после проверки. Чужие Python MCP, FastMCP, NumPy, Pydantic, vendor SDK и имеющиеся научные тесты остаются upstream. Для HFSS официальный PyAEDT/gRPC исследуется отдельно без собственного SDK, COM fallback и автоматического открытия сети.

<a id="dataplane"></a>
## 7. Остальные вычислительные и потоковые участки

Каждая задача ниже начинается только после своего baseline/contract gate; это не разрешение массового переписывания. Один PR — один проверяемый инвариант.

### RX-08/relay (R09)

**Путь:** существующий `mcphub relay`, файлы RX-01/relay.

- Зафиксировать наблюдаемую причину затрат и достаточность минимальной Go-правки. Сохранить session headers, initialize order, JSON/SSE IDs, cancellation, reconnect, body/event caps и final-response drain.
- Создать один standalone Rust-прототип на тот же corpus/нагрузку; тесты дифференцируют wire/error/outcome. Не менять HTTP/MCP semantics ради красивого результата.
- Сравнить всю замену executable, включая startup/RSS/security/IO, а не Go-wrapper плюс Rust-child. Реальный launcher/discovery/identity переход — отдельная приёмка.

**Выход:** квалифицированная замена relay либо решение оставить Go.

### RX-08/parse и RX-08/graph (R12, R13)

**Файлы:** `internal/vcpkgmcp/cmaketrace/`, `internal/cmakegraph/cmakegraph.go`, `internal/vcpkgmcp/cmakewrap/tool.go`, `lastfailure/`.

- По RX-01 выбрать parse, compact representation или indexes, а не произвольную функцию. Данные передавать крупными блоками, стоимость FFI считать отдельно.
- Corpus: valid/malformed/truncated, unsupported version, limits, Unicode, отмена; compare canonical results, порядок и все incompleteness/omission flags. Реальный bounded publisher обязателен.
- Для graph/log дополнительно сохранять unresolved/reason/conditional, циклы, root/node/byte caps и exact workspace confinement. `cmakewrap` остаётся thin consumer, не второй resolver.
- Измерить минимум улучшенный Go и Rust на полном выбранном конвейере, включая materialization/public encoding. Не переписывать CMake/vcpkg/чужие engines.

**Выход:** перенос одного измеренного участка с parity или документированный отказ от миграции.

### RX-08/transport (R10)

**Файлы:** `internal/daemon/host.go`, `host_original.go`, `internal/mcpcompat`, `internal/mcproute`. **Зависимость:** нужные WP-11E contracts.

- Измерить body read, envelope parse, ID rewrite, pending map, cancellation и output separately. Один результат не превращает весь MCP-stack в Rust scope.
- Корпус нескольких клиентов с одинаковыми внешними ID: верный session→internal-ID mapping, отмена только своей работы, partial-write неизвестность, медленный stdin, notification/response separation и byte/concurrency bounds.
- Сравнить сохранение raw payload/минимальный Go и связный Rust data-path; маленький Rust-parser с многократным копированием обратно в Go не принимать без общего выигрыша.

**Выход:** ровно одна цельная обработка сообщения с прежними policy/session owners и измеренным эффектом.

### RX-08/registry (R14)

**Файлы:** `internal/api/workspace_registry*`, `lsp_routing/resolver_benchmark_test.go`, `serena_routing/`. **Зависимость:** общий snapshot/view после соответствующих WP.

- Использовать существующий benchmark и реальные размеры, измерить повторные чтения/parse/cache invalidation.
- Проверить immutable snapshot publication, generation coherence, invalid refresh, concurrent readers и stale lookup. Последний хороший снимок не смешивать с новой политикой.
- Rust допускается для read-only индекса, только если оправдан всей ценой границы. Durable authority, запись и миграция остаются Go; нет второго реестра.

**Выход:** быстрее/дешевле lookup либо исправление Go; без изменения источника истины.

### RX-08/events (R15)

**Файлы:** `internal/gui/events.go`, `internal/urlredact`, фактические log/event consumers.

- Разделить lossy telemetry, bounded live-log view и долговечный результат операции. Измерить queue/copy/redaction/disk отдельно.
- Проверить slow subscriber, overflow, reconnect, redaction, log rotation и отказ записи; UI восстанавливает snapshot, а неизвестный outcome не отображает completed.
- Переносить только измеренный formatter/buffer/IO path. Не менять policy доставки и не превращать события в новый authoritative journal.

**Выход:** bounded поток с прежней семантикой и честными dropped counters, либо достаточное Go-исправление.

### RX-08/artifacts (R18)

**Файлы:** `internal/api/export_bundle.go`, `project_service_provider.go`, `internal/binaryadmission/source_prepare*`; актуальные WP-11D/0.4.60.

- Проверить ZIP finalization, short write, read/close/flush failures, источник изменился, hash mismatch, staging/publish и cleanup. Исправления результата Go не откладывать ради Rust.
- Profile copy/hash/compress/IO на разрешённых артефактах; не выдавать накопленные байты за peak memory.
- Переносить законченную streaming operation через того же installer/admission owner. Archive path/expansion limits и условный rollback сохраняются; нового installer нет.

**Выход:** надёжная обработка результата и при доказанной пользе один Rust-stream.

<a id="late"></a>
## 8. Supervisor и desktop-интеграция

### RX-09/supervisor (R23)

**Файлы:** `internal/cli/supervisor_controller.go`, `supervise*`, `internal/daemon/lazy_proxy.go`, будущий `internal/supervisor`.

- После WP-11B/C/E и pilots сравнить: оставить Go + узкий native-owner против целой Rust-ответственности. Перечислить callbacks, duplicated states, locks, поколения и actual control flow, не сравнивать с устаревшим монолитом.
- Если целый перенос проще, отдельно заморозить IPC/wire/state/restart/admission contracts, source executable identity, сроки действия и selected scope. Затем новый implementation plan данного bounded решения в том же roadmap.
- При реализации заменить существующий supervisor, не добавить второго. Проверить cold start, stale generation, clean stop/crash, shared-child exit, deferred activation, installed intent OFF и recovery без расширения полномочий.

**Выход:** обоснованный retain/replace verdict; вся ветвь не обязательна для завершения остальных улучшений.

### RX-09/desktop (R24)

**Файлы:** `internal/tray/win32_windows.go`, `internal/gui/focus_window_windows.go`, текущие native adapters.

- Сначала установить конкретный дефект или отсутствующий требуемый платформенный сценарий. При отсутствии необходимости зафиксировать «сохранить», не создавать пилот ради Rust.
- При необходимости изменить один lifecycle/activation path: single instance, неблокирующая команда, закрытие окна не уничтожает backend. Сохранить отдельный message loop и разрешения.
- Нативную GUI-проверку проводить только согласованным способом без всплывающих окон на рабочем столе. Headless test не доказывает focus/accessibility.

**Выход:** точечная platform-интеграция или сохранение Go. Web, Qt/GPUI и новый GUI-фреймворк не входят в задачу.

<a id="integrate"></a>
## 9. Интеграция, откат и завершение

- Для каждого принятого пилота заново сверить master/#616/selected/0.4.60, новые callers и already-merged исправления. Повторить только затронутые проверки, не теряя required gates.
- Обновить существующие ADR/traceability/ownership inventories при изменении принятой границы; не создавать параллельный status registry. Не включать runtime только потому, что стенд прошёл.
- Один PR переводит одного потребителя/инвариант. Новый owner используется только для нового lifetime после подтверждённого завершения прежнего; live handoff не добавлять как побочную цель.
- На неизвестном исходе запрещён автоматический повтор другой реализацией. Поздняя ошибка cleanup не стирает выполненную публикацию/transfer/результат ребёнка.
- Подтвердить native runtime только на реально проверенных ОС/архитектурах/filesystems. Targets: Windows/amd64; Linux/amd64,arm64; macOS/amd64,arm64 — цели квалификации, не уже готовые поставки. COM-free и no-Python не тождественны.
- До merge выполнить обязательные repo, affected и независимые security/lifecycle gates с действующим разрешением. Не подменять missing reviewer своим PASS; not-run/unsupported/failed/baseline failure остаются видимыми.
- После замены всех callers убрать подтверждённо заменённую реализацию, не держать вечные dual owners. Сохранить полезные regressions, evidence и поддерживаемый rollback; работу с trash выполнять по общей политике.

**Критерий завершения программы:** по каждому R01–R24 есть доказательный verdict и дальнейший владелец; принятые изменения квалифицированы, отклонённые не висят как обещанный rewrite. Нет требования мигрировать весь Go или удалить все `.py`.

<a id="review"></a>
## 10. Проверки, методики и источники

**Таблица 5. Evidence каждого результата.**

| Что фиксировать | Что нельзя подменять |
|---|---|
| Source/base SHA, test diff/hash, команды, exit, native ОС/arch/toolchain | Старый PASS не относится к новому HEAD |
| Fixture/hash, нагрузка, repetitions, GOMAXPROCS, cache state, concurrency | Синтетический вход не доказывает production-эффект |
| CPU/wall/queue/errors, alloc traffic, RSS/peak RSS, handles/threads | B/op и Go heap не равны памяти всего процесса |
| Семантические outcomes, negative/fault tests, caps, omission flags | Компиляция/unit test не доказывают свойства ОС |
| Current→minimal Go→Rust; цена FFI и всех процессов | Выигрыш алгоритма не приписывать языку |
| Неизвестные условия и решение retain/improve/replace | Отсутствие результата не выдавать за успех или доказанное отсутствие |

Минимум 10 повторов — исходная настройка, не магический критерий. Нет заранее придуманного обязательного x2. Профили могут учитывать setup/preflight, несмотря на ResetTimer: анализировать функции и отдельно описывать границы профиля. Особо большие данные и приватные пути не публиковать. Timing и точные memory profiles не смешивать в одной сравнительной серии [6].

При исполнении используются: Writing Plans и Executing Plans; Software Architect / Architecture Review; Ponytail для исключения дублирования; Go Performance and Diagnostics, Testing and Verification, Concurrency Lifecycle, Project/API Design, Distributed Coordination и Security Hardening; Intuitive Software Design для результатов/восстановления; Rust Unsafe Checker и Resource Lifecycle для FFI и владельцев. Читать подходящий навык на конкретной стадии; не запускать внешние модели под видом навыка.

**Саморевью этой редакции:** каждый из 24 ID сопоставлен задаче; устранены неопределённые «позже» для parser/transport/events/artifacts/secrets/desktop; consumers не превращены в новые owners; COM-ветвь не требует Rust; сохранены зависимости будущей основной ветки; benchmark JSON уточнён до фактического `publicresult.MarshalIndent`. Условия приёмки отличаются от реализованных свойств. Подробные evidence сохраняются в соответствующем PR. Только таблица 1 этого файла содержит укрупнённый текущий статус; нижние RX-списки являются требованиями, а не второй очередью незавершённых задач.

### Источники

1. [PR #624](https://github.com/applicate2628/mcp-local-hub/pull/624), [начальный benchmark](https://github.com/applicate2628/mcp-local-hub/blob/64d21d4ecb1660a5748d9cebee0528fd92ed16b7/internal/vcpkgmcp/cmaketrace/parse_benchmark_test.go).
2. [CMake parseTraceStream](https://github.com/applicate2628/mcp-local-hub/blob/76d11b33bd37e30125525ce8e0e582122f29e91b/internal/vcpkgmcp/cmaketrace/parse.go).
3. [Trace/Result/indexes](https://github.com/applicate2628/mcp-local-hub/blob/76d11b33bd37e30125525ce8e0e582122f29e91b/internal/vcpkgmcp/cmaketrace/cmaketrace.go).
4. [Bounded publisher](https://github.com/applicate2628/mcp-local-hub/blob/76d11b33bd37e30125525ce8e0e582122f29e91b/internal/vcpkgmcp/publicresult/publicresult.go), [MCP caller](https://github.com/applicate2628/mcp-local-hub/blob/76d11b33bd37e30125525ce8e0e582122f29e91b/internal/vcpkgmcp/vcpkgserver/helpers.go).
5. [Foreground/admission exact #616](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/foreground_windows.go), [PR #616](https://github.com/applicate2628/mcp-local-hub/pull/616).
6. [Go testing](https://pkg.go.dev/testing), [Go diagnostics](https://go.dev/doc/diagnostics), [Go cgo](https://pkg.go.dev/cmd/cgo).
7. [Rust Child](https://doc.rust-lang.org/std/process/struct.Child.html), [Unix CommandExt](https://doc.rust-lang.org/std/os/unix/process/trait.CommandExt.html), [Rust FFI](https://doc.rust-lang.org/nomicon/ffi.html).
8. [Полная исходниковая карта и платформенные ограничения](rust-cross-platform-plan-2026-11-12.md#candidates).

9. [Текущий native owner #625](https://github.com/applicate2628/mcp-local-hub/pull/625), [identity #644 и его проверенный результат](https://github.com/applicate2628/mcp-local-hub/blob/8e80fad662ef20bd5cbc2d1ca4808a9494d6f30e/experiments/rust-process-owner/verification/identity-2026-11-14/RESULTS.md).
10. [Go/Rust boundary #631](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/README.md), [точный остаток admission](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/outcome-2026-11-14/ADMISSION-PARITY.md).
11. [Принятый Go CreatorIdentity](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/creator_identity.go), [историческая сверка и синхронизация веток](../verification/branch-audit-2026-11-14/RESULTS.md).

**Термины:** SID — security identifier, идентификатор безопасности Windows; capability — связанное с исходным владельцем полномочие; Job — объект Windows для управления группой процессов; drain/join — дочитывание потока и завершение обслуживающей работы; DLL — динамическая библиотека; PR — запрос на включение изменений; RX — пакет исполнения; WP — рабочий пакет основной модернизации; MCP — Model Context Protocol; SDK — набор интерфейсов поставщика; COM — Component Object Model; FFI — вызов другого языка; ABI — двоичный контракт; RSS — резидентная память; SSE — серверный поток событий; EOF — конец потока; PID — идентификатор процесса; CLI — интерфейс командной строки; GIL — блокировка интерпретатора Python; PE — Portable Executable. B/op — накопленные выделенные байты за операцию; p50/p95/p99 — процентили задержки; КиБ — 1024 байта. Native evidence — реальное выполнение на указанной ОС, а не только cross-compile.

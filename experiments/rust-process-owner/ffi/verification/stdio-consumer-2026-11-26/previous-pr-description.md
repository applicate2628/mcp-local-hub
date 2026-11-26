<!-- mcphub-current-audit:2026-11-25:start -->
## Текущий результат — 25 ноября 2026 года

**Статус: Draft, без слияния.** Обычная квалификация Windows x86_64 выполнена на точном исходном коммите `e4c929a868bd434ab6800760610abccd06a7d826`. Подготовленная локальная вершина `5c0ee465c3f3ccf0d141872aaa3c87b2ab228cf3` добавляет только README и файл результатов; она ещё не отправлена после отказа первой проверки публикации. Новых запусков на коммите документации этим не заявляется.

### Что изменено и почему

В существующую ветку включён опубликованный родитель #644 `ba14bd241370afd2ecb4ad2daad25356177f6013`: его 13 нативных исходников получены точно, все 60 файлов границы Go ↔ C ↔ Rust сохранены при включении. Следующий коммит меняет две строки теста Rust: обход тех же трёх кодов освобождения через `enumerate` устраняет замечание Clippy и сохраняет все семь масок и индексные ожидания. Определения интерфейса ABI и исполняемый код FFI не менялись этой правкой теста.

Базовая ветка остаётся `spike/rx02-native-identity-20261114`. Роли #625, #644 и #631 сохранены: политика фонового запуска, нативный владелец и языковая граница соответственно. Проверки ниже не заявляют интеграцию экспериментального владельца в установленный Product.

### Проверено на исходниках e4c929a

| Профиль | Полученный результат |
| --- | --- |
| Rust: форматирование и Clippy | PASS; использованы завершённые результаты на том же исходном коммите |
| Rust FFI, debug | 20 тестов PASS; собраны исходный дочерний образ и DLL |
| Rust FFI, release | 20 тестов PASS; собрана DLL |
| Go `-race`, debug DLL | 57 корневых тестов / 129 терминальных результатов PASS |
| Go `GOEXPERIMENT=cgocheck2`, debug DLL | 57 / 129 PASS |
| Go `-race`, release DLL | 57 / 129 PASS |
| Go `GOEXPERIMENT=cgocheck2`, release DLL | 57 / 129 PASS |
| Go `vet` | PASS |

Во всех четырёх профилях Go отсутствуют FAIL, SKIP, пропущенные, лишние и повторные терминальные случаи; точный набор из 129 имён совпал. Корневые и вложенные результаты — разные способы подсчёта одного набора, их не складывают. Проверка `-race` относится к выполненным обращениям Go и не доказывает отсутствие гонок в C/Rust.

Получены хеши исходников, инструментов, трёх фактически использованных исполняемых артефактов и завершённых квитанций. Каждый успешный запуск завершён с получением исходных объектов процессов, Job и потоков ввода/вывода; принудительного завершения Job в этих успешных запусках не было. Команды, ограничения времени, точные входы и результаты приведены в [README, таблица 8](https://github.com/applicate2628/mcp-local-hub/blob/5c0ee465c3f3ccf0d141872aaa3c87b2ab228cf3/experiments/rust-process-owner/ffi/README.md#current-native-qualification-2026-11-25) и [qualification.json](https://github.com/applicate2628/mcp-local-hub/blob/5c0ee465c3f3ccf0d141872aaa3c87b2ab228cf3/experiments/rust-process-owner/ffi/verification/continuation-2026-11-25/qualification.json).

### Сохранённые отказы и границы

Первый запуск остановился на проверке входного хеша до Cargo; второй — на замечании Clippy. В третьем Cargo сборки дочернего образа вернул 0, но исходный Job потребовал завершения: вся попытка остаётся FAIL, конкретный оставшийся участник и причина не были зарегистрированы. Последующий успешный запуск с уже подготовленным результатом не превращает этот холодный отказ в PASS.

Исторические проверки совместимости, внедрения отказов и иные операционные системы заново не запускались. Полная приёмка Hub, установленного Product и передача полномочий настоящему потребителю CreatorIdentity остаются открытыми. `FinishContext` отменяет наблюдение ожидания, а не останавливает процесс и не закрывает чужой ввод/вывод. Подтверждённого внешнего ревью кода без замечаний на новой вершине нет.

[Исторический аудит #631](https://github.com/applicate2628/mcp-local-hub/blob/cc7b33a9e69028165cbab7042c536f846e6f901f/docs/modernization/verification/pr-state-audit-2026-11-21/README.md#pr-631). Предыдущие доказательства и ограничения сохранены ниже.


### Область решения проверки публикации

Первая попытка отправки остановлена на `range-coverage`; квитанция `ae6f5b8386c038098a5382f033d268ae79bc8b2ced55bfba7f9b0d1787f50e33` сохраняет FAIL. Штатный сбор сведений завершён с полным закрытием исходных ресурсов: `cc93a8a544299cae82f1b5f2ace1badf58ccdeec0f2005b0d5273801f2379c7f`.

Независимое ревью и повторная проверка владельцем задачи разрешают две конкретные области README: строки 444/448, байты `[52660,52674)` и `[52833,52847)`. Это сохранённый исторический текст о разрешённом рабочем каталоге и иллюстративный параметр `LabRoot` с `your-owned-lab`. В проверенных областях нет имени профиля, учётных данных, идентификатора клиента или значения из журнала выполнения.

| Привязка | Точное значение |
| --- | --- |
| Путь | `experiments/rust-process-owner/ffi/README.md` |
| Пакет фактического сканера | `f8fce0e5a9ba92ef562f5982137117a8a1f83efbce111c2bcd17490c868d47da` |
| SHA-256 пакета | `953997994017297738f32809fec979ec6cc99c5e6eb5ca451bdbc9bfddf82aad` |
| Git blob | `f358c20d7801e96b8be8f88bbf567ca78144af7a` |
| SHA-256 полного контекста, 59 377 байт | `ac53f3075ab1cd4744f5bee22499f2b2d50c19ac1a7825ab0380b5e8799f0b0e` |
| Детектор | `machine-path-prefix-v1`, meaning `99b2ba95d5986407a96b9246f5499f24eba89e6c0bb23a746290b3cb766ab0f5` |
| Фактическое решение ревьюера, SHA-256 | `b2dadbbc4060b202b44046b3626013b799d573173a5bac8c46ae61450932faac` |

Разрешение действует только для этого пакета, репозитория и рабочей копии, режима `range-blob-text`, точного набора коммитов `1cf5754b81c850a3569be269642b49605895eeb2`, `5c0ee465c3f3ccf0d141872aaa3c87b2ab228cf3`, `94568686c49374a318338d15613b19e1b93377d9`, `ba14bd241370afd2ecb4ad2daad25356177f6013`, `e4c929a868bd434ab6800760610abccd06a7d826`, указанного пути, режима файла 100644, содержимого и двух областей. Изменение любой привязки требует нового фактического ревью. При отзыве или сужении разрешённой области публикации либо новых данных о частном содержимом решение подлежит отзыву до следующей зависимой публикации. Прежние независимые решения сохраняются.

**Текущее состояние этой процедуры:** решение подготовлено и получено; штатное принятие политикой, предварительная сверка её изменения и повторный полный скан ещё предстоят. Удалённая вершина остаётся `701535e3439d74495080463976d610a42303e5cf`.

**Остаточный план:** принять нового реального потребителя исходного владельца и его результаты завершения; отдельно закрыть требуемые проверки совместимости, отказов и других платформ; сохранить Draft до выполнения критериев. PR не сливается.
<!-- mcphub-current-audit:2026-11-25:end -->

<details>
<summary>Предыдущее описание и исторические результаты — сохранены без изменения</summary>

## Problem and behavior

The Go/Rust experiment needs an observation-only identity query that does not secretly admit or resume a process. Optional C export `mh_query_identity1` and Go `Owner.QueryIdentity()` observe the same retained native process owner and original Job. ABI1 (application binary interface version 1) and `mh_admit_identity1` keep their existing contracts; a missing query export returns explicit unsupported without falling back to admission. The C callback only copies data.

Query permits observations before/after admission, including denial and `finish(Pending)` without Stop. It refuses after a Stop attempt or terminal finish. Errors return an empty Go observation and retain query/token-close failures. Current observations include the Windows security identifier (SID), creation time, original Job membership and wait/image states; historical PID/image-only admission is not the current protocol. Full product `CreatorIdentity`, authority and resource-lifetime policy remain separate.

## R24 documentation correction

Published head `412cd353d495a890b47992c0eae672a247862508` adds **two README lines only** over `2eb7c114103ef5c60939cd114110ebe667901166`. Readback matched Git blob `68354b368994dcd7303f6ea48d59639d5c495bde`. The paragraph restores the exact inherited native `process_view()` qualification: **30 unit + 43 integration + 3 compile-fail documentation checks = 76 debug checks**, including the independent repeat; the full native release profile was not repeated in that qualification. No executable source or test changed, and no runtime rerun was needed for this text correction.

The review dependency remains #625 / #644 / #631 with distinct ownership, observation and language-boundary scopes. #644 already contained the close-failure qualification; an initial hypothesis of missing text was withdrawn after direct comparison. Parallel wait/close work was not imported.

## Historical evidence

[R23 evidence](https://github.com/applicate2628/mcp-local-hub/blob/b1d932ea8925ef3324d539e5de006daeae65ba2a/docs/modernization/verification/completion-r22-r23-2026-11-21/README.md) executed exact runtime source `ebfa4ef32b57cfa022acdcfcb4a2745f32b815c6`, before both README continuations:

- Debug Rust foreign-function-interface (FFI) tests: 20 PASS; child and dynamic-library builds succeeded.
- Final Go-only `-race -count=1`: 57 roots, 129 test/subtest PASS events covering 118 terminal cases, and package PASS; no FAIL/SKIP. These are alternative counts, not additive coverage.
- Earlier environment/fixture failures remain retained. Missing identity fixtures caused 4 terminal failures / 6 FAIL events; the Go-only continuation corrected the environment, not the product. The earliest readiness-script version was not recovered. Release, additional build-tag paths and reader #627 were NOT_RUN in R23.

[The earlier immutable query report](https://github.com/applicate2628/mcp-local-hub/blob/2fd3f017a9e5ccd0460293d71ea34aceb62f245e/experiments/rust-process-owner/ffi/verification/query-2026-11-15/RESULTS.md) retains separate debug/release FFI 20+20 PASS, Go race/cgocheck2 selections of 57 roots+72 nested cases per profile, compatibility checks and corruption controls. These historical results are not new release qualification of the current head.

## Limits

On historical product base `17bc78fc602efa204a74596409c939a3bf1ddc42`, build/vet exited 0; full Go and state-tag API/CLI exited 1, with 300-second timeouts. Those FAIL gates remain. Product authority, context-aware cancellation, resource sealing/retirement and the complete root/member/action/close contract are not established. No fresh full review or runtime PASS is inferred from R24; keep Draft to #644.

## Evidence archive

[R24 audit and source manifests](https://github.com/applicate2628/mcp-local-hub/blob/b21402535ac4df032408d7bf06200ddd0557abf1/docs/modernization/verification/completion-r24-2026-11-21/README.md) · [Original PR descriptions, preserved verbatim](https://github.com/applicate2628/mcp-local-hub/blob/b21402535ac4df032408d7bf06200ddd0557abf1/docs/modernization/verification/completion-r24-2026-11-21/original-pr-descriptions.zip). The archive retains the complete historical narrative, failed attempts and original source references; earlier results remain limited to those inputs.


</details>

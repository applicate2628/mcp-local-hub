<!-- mcphub-current-audit:2026-11-21:start -->
## Актуальное состояние после #662 — 21 ноября 2026 года

**Статус:** Draft. Полное поглощение полезного остатка текущим master не доказано; условия готовности остаются открытыми.

**Вершина PR:** `04e1e19650318725850225ebadc521cbf97dadd9`. **Базовая ветка:** `master`, её текущая вершина — `75dbc6406123e22468f168b861d5e0622c9d5769`. Здесь указана фактическая текущая вершина ветки, а не историческое поле `base.sha` ответа GitHub.

**Технический владелец:** internal/daemon: измерения Relay и минимальный executable-control. Автор PR — `applicate2628`; назначенных исполнителей и запрошенных ревьюеров в GitHub на момент сверки нет. Технический владелец обозначает компонент, а не назначение человека.

**Контракт:** Нагрузка проверяет точное содержание JSON-RPC ответов, отсутствие лишних ответов и версии протокола; минимальный executable остаётся контрольным образцом.

**Текущее состояние относительно master `75dbc640`:** GET в текущей фикстуре возвращает 405; потоковые проверки относятся к POST-SSE. Производственное исправление #629 не входит в эту ветку автоматически.

**Следующий необходимый результат:** Добавить репрезентативный длительный GET-SSE и переподключение, ограничения параллельности, отмену и ошибки вывода. Измерять точный выбранный источник с #629; прежние 1/8/32 процесса не равны установленной топологии.

**Внешнее ревью:** **REVIEW_PENDING:** подтверждения внешнего кодового ревью без замечаний на текущем SHA нет. Исторические отзывы сохранены и учтены отдельно.

**Предел проверок этого аудита:** статический разбор исходников, владельцев, контрактов между источниками данных и потребителями, деревьев Git и существующих квитанций. Новые сборки, проверки Go/Rust, нативные и продуктовые проверки, а также `go vet`, не выполнялись (**NOT_RUN**); прежние результаты относятся только к указанным в них источникам и входам.

[Подробный аудит этого PR](https://github.com/applicate2628/mcp-local-hub/blob/cc7b33a9e69028165cbab7042c536f846e6f901f/docs/modernization/verification/pr-state-audit-2026-11-21/README.md#pr-626) · [Единый актуальный план и критерии приёмки](https://github.com/applicate2628/mcp-local-hub/blob/4c262ba522a2fbe8e040e4bb270f2b47bd579345/.plans/2026-11/open-pr-integration-20261117.md#3-порядок-выполнения).
<!-- mcphub-current-audit:2026-11-21:end -->

<details>
<summary>Предыдущее описание и исторические результаты — сохранены без изменения</summary>

## Problem and behavior

Measure Relay workload and executable overhead before choosing a Rust migration. The PR adds controlled JSON and server-sent-events (SSE) workloads, semantic response checks and an opt-in Windows executable probe. Its minimal Go executable uses the unchanged production Relay as an experimental control; it is not a separately qualified Product or replacement distribution.

Head `04e1e19650318725850225ebadc521cbf97dadd9` retains the oracle correction from `a00cbd013348163526b2ec3cd213f27a2c126c70`: initialize and call responses must use JSON-RPC 2.0, and the joined reader/child must leave no extra response. The old probe accepted a duplicate final response or version 1.0 and could incorrectly record a measurement. Production Relay and dependencies were not changed by this correction; input/output ownership work belongs to #629.

## Historical verification

[Immutable oracle report and receipts](https://github.com/applicate2628/mcp-local-hub/blob/04e1e19650318725850225ebadc521cbf97dadd9/docs/modernization/verification/relay-oracle-2026-11-18/RESULTS.md): eight targeted Windows/amd64 executions compared old/new probes against the same child. Correct streams passed both. Duplicate-final, wrong-initialize-version and wrong-call-version streams incorrectly passed the old probe and produced expected FAIL with no measurement file under the corrected probe. Version controls stop before the complete workload. Affected vet, formatting and tagged RelayLoad with the race detector also passed.

On exact clean `a00cbd013348163526b2ec3cd213f27a2c126c70`, **full build/vet passed; full `go test ./...` and full state-tag API/CLI failed**, including recorded 300-second timeouts and fixture/environment refusals. The tested Go bytes are retained at the documented head. These failures are not waived by focused success.

The archived earlier 1/8/32-child experiment used 16 sequential 1-KiB requests per child. Median summed working sets were 17.41/138.84/556.38 MiB for the full executable and 11.36/90.39/362.49 MiB for the minimal control. MiB means 1,048,576 bytes; shared pages are counted repeatedly. These are neither unique nor peak memory. The first long series that lost deliveries remains excluded from performance conclusions; socket exhaustion was a hypothesis, not a proven cause.

## Limits and R24 status

The real-consumer map identifies six opt-in Relay adapter classes, not installed concurrency. The short GET-to-405 fixture does not qualify a sustained GET-SSE/reconnect session or saturated POST workers. A minimal control omits Product/discovery admission and #629 input ownership. Open-loop tail latency, long duration, cancellation/output failures and native Linux/macOS remain separate work.

R24 edits documentation only and repeats no load series or full review of unchanged runtime. The recorded full FAIL/NOT_RUN limits remain. No production speedup, Rust requirement, installed-product acceptance or current-head formal external approval is claimed; keep Draft.

## Evidence archive

[R24 audit and source manifests](https://github.com/applicate2628/mcp-local-hub/blob/b21402535ac4df032408d7bf06200ddd0557abf1/docs/modernization/verification/completion-r24-2026-11-21/README.md) · [Original PR descriptions, preserved verbatim](https://github.com/applicate2628/mcp-local-hub/blob/b21402535ac4df032408d7bf06200ddd0557abf1/docs/modernization/verification/completion-r24-2026-11-21/original-pr-descriptions.zip). The archive retains the complete historical narrative, failed attempts and original source references; earlier results remain limited to those inputs.


</details>

## 22 November 2026: official SDK consumer qualification

Commit `279887a4ec79186672c331e075c70e0b4555daa8` updates two test files: correct SDK initialization in the load fixture and a Windows consumer profile using real SDK transports through Relay.

The exact preserved-source composition passed 20 test/subtest events and five package events, including the selected race checks. `go build ./...` and `go vet ./...` passed with the same pinned overlay. Three fresh non-race processes accounted for 576 measured calls and 627 total server calls including warmup, held and canceled requests; all measured payloads matched. Active HTTP requests, tools and server sessions were zero after close.

[Exact sources, commands, raw evidence and measurements](https://github.com/applicate2628/mcp-local-hub/blob/279887a4ec79186672c331e075c70e0b4555daa8/docs/modernization/verification/relay-sdk-consumer-profile-20261122/README.md). Memory is whole-process and sampled; these short synthetic cases establish neither a production service target nor a causal improvement. GET requests were observed as 0/0/4 in the three non-race profiles. Standalone branch acceptance, the full Go suite, installed-product acceptance, POST-origin resumption and external review of this published commit remain open. Draft status is retained.


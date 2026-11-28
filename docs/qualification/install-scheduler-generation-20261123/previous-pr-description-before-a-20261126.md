<!-- audit-remediation-20261125-current -->
## Аудит и исправления · 25.11.2026

Опубликован проверенный тематический commit `8d4f2606c9af7e20181e4ad290b29be0e0a2dd0f`. PR остаётся **Draft**; слияний и новых PR нет.

NextRun получает исходный локальный COM DATE с различением zero и недоступного поля. Deadline операции передаётся существующему Scheduler backend. Unknown mutation сохраняет причину и останавливает rollback до inverse; известная LSP-компенсация получает один отдельный срок.

**Проверено:** Windows: весь Scheduler PASS (37.948 s); API transaction/context regressions PASS (8.228 s); CLI V3 rollback regressions PASS (0.597 s). Три замечания независимого caller-path review и происхождение no-dispatch marker исправлены. Собственное чтение diff и независимое ревью выполнены; точный состав и границы доказательств сохранены в документах ниже.

**Общий состав:** `14af01ea06063a2c2306aeaa1accf454151d7e6b`, tree `386923930b874526a0e6896bde1857c4ecc95d63`. На этой композиции `go build ./...` и `go vet ./...` — PASS; оба обязательных полных набора — **FAIL**. Default: 56 пакетов PASS, 8 FAIL, 11 без тестов; tagged API/CLI — FAIL. Отсутствующая Product/Job qualification, исчерпанные сроки и ошибки фикстур разбираются в [#664](https://github.com/applicate2628/mcp-local-hub/pull/664). Эти общие результаты не выдаются за полный PASS отдельной вершины PR.

**Границы:** Capture budgets: stdout 16 MiB, stderr 64 KiB. Реальные task mutations и Product recovery требуют своей приёмки; backend без ContextBinder не обещает прерывание. Codex review на новую точную вершину не подтверждён; историческое одобрение не переносится автоматически.

[scheduler-audit-remediation-20261125.md](https://github.com/applicate2628/mcp-local-hub/blob/8d4f2606c9af7e20181e4ad290b29be0e0a2dd0f/.plans/2026-11/scheduler-audit-remediation-20261125.md) · [scheduler-audit-evidence-20261125.json](https://github.com/applicate2628/mcp-local-hub/blob/8d4f2606c9af7e20181e4ad290b29be0e0a2dd0f/.plans/2026-11/scheduler-audit-evidence-20261125.json)

**Остаточный план:** завершить указанные реальные сценарии и свежую квалификацию; обновлять этот же PR по установленной причине, сохраняя ошибки и исходное владение ресурсами. Слияние остаётся вне работы агента.
<!-- /audit-remediation-20261125-current -->

---

### 24 November 2026: named KnownFolder caller census

Current head: `892fa82e0e1aa5746d97365f96a9b69ef7934168`. The production lock-order barrier now checks exact file/function callers instead of a numeric call-site allowance. The separately composed #637 observer is admitted at its reviewed site; unknown or duplicate callers remain refused. The #665 console-policy delta stays with its own owner, and analysis budgets are unchanged.

The standalone #660 candidate and the composed Source candidate passed the full production barrier under standard and `test_state_path_env` selections. Same-count unknown-caller controls failed as required in both selections. [Exact evidence and limitations](https://github.com/applicate2628/mcp-local-hub/blob/892fa82e0e1aa5746d97365f96a9b69ef7934168/docs/qualification/known-folder-census-20261124/README.md). The issued Source `b4265083…` and its Product/Serena artifacts are unchanged; the census candidate is not a newly issued Product.

Full standard/tagged repository acceptance and external review of this new head remain open. Historical evidence below retains its original source scope.

---

### Продолжение: install / rollback / prune — 24 ноября 2026, МСК

Точная новая голова: `d7a45916e090f823f7df9bdd5b14c636b1b17b47`. Из трёх владельцев удалены разрушительные операции по имени без исключительного права на поколение. Существующая задача вызывает отказ со снимком; неизвестный эффект создания не вызывает повтор; компенсация удерживает текущую задачу и причины неполного восстановления; prune сообщает именованное предупреждение. Совпадающий XML другого поколения не даёт разрешения на удаление.

[Отчёт, точный состав и сырые свидетельства](https://github.com/applicate2628/mcp-local-hub/tree/d7a45916e090f823f7df9bdd5b14c636b1b17b47/docs/qualification/install-scheduler-generation-20261123). 11 проверенных Go-файлов. Финальные выборки: 126 PASS + 87 PASS + производственный барьер 2 PASS + смежные контракты 92 PASS; build/vet успешны. Два события SKIP — один Unix-only случай в двух Windows-выборках, причина разобрана. Отрицательный контроль на прежнем коде: ожидаемый exit 1, 18 FAIL; первая ошибка сравнения экранированного пути в тесте исправлена, исходные FAIL сохранены. Код и документы прочитаны обратно побайтно.

Draft сохраняется. Полные стандартные/тегированные наборы, нативная системная квалификация, оставшиеся владельцы удаления и внешнее ревью новой головы открыты. Исторические разделы ниже не являются допуском текущей головы; прежние успешные удаления install/prune заменены новым контрактом отказа.

---

## Current qualification — 2026-11-23

Exact head: `58d7c948e410fdb5969abb9a8a4febfea8f14150`. **Draft; external review of this head and full-suite qualification remain open.**

Weekly refresh and `SchedulerUpgrade` now refuse replacement of an observed task without exclusive namespace authority. Fresh-create errors with uncertain effect preserve the observed occupant; matching XML is not generation ownership. The `errnoIs → Unwrap` proof and exact reader callback contexts remain fail-closed with unchanged analysis budgets.

Standalone validation on this PR's own pinned base plus the 17 changed Go files passed: 127 origin/context events, 2 production-lock-order events, 92 Scheduler contract events, 49 atomic/owner events, 9 GUI events and 4 registration events; zero FAIL/SKIP. Standard `go build ./...` and `go vet ./...` passed. These are targeted test events, not full-suite qualification.

[Exact-source evidence, negative controls, SDK body review and remaining delete owners](https://github.com/applicate2628/mcp-local-hub/blob/58d7c948e410fdb5969abb9a8a4febfea8f14150/docs/qualification/scheduler-generation-error-origins-20261123/README.md).

Remaining A2 owners include install replacement/rollback/pruning and other name-based deletions. #669 independently fixes the secrets recovery lock; composing it here requires removal of this branch's obsolete raw-lock census row. Previously issued Source/Product/Serena artifacts exclude these new changes. No donor was closed and no merge, native task mutation or workflow dispatch was performed.

---

## Retained earlier description and evidence

## Подтверждённое применение weekly schedule — 23 ноября 2026, МСК

**Текущая вершина:** `026928c31977788444f6b3e3504324ae21106446`, один родитель `203c7bacef98c0445549512a2b6cab61eca6dcd1`. PR остаётся Draft.

При подтверждённом изменении задачи и настроек ошибка освобождения блокировки раньше теряла applied-классификацию, и настоящий маршрут GUI возвращал ложное `updated:false`. Теперь два владельца операции сохраняют признак подтверждённого применения и все исходные причины ошибок. Неподтверждённое создание, запись с ошибкой и восстановление прежнего состояния этим признаком не помечаются.

[Полный отчёт, точные исходники и сырые события](https://github.com/applicate2628/mcp-local-hub/blob/026928c31977788444f6b3e3504324ae21106446/docs/modernization/verification/weekly-result-owner-20261122/README.md) · [Открытая граница поколения Scheduler](https://github.com/applicate2628/mcp-local-hub/blob/026928c31977788444f6b3e3504324ae21106446/docs/modernization/verification/weekly-result-owner-20261122/weekly-generation-contract.md).

- API: 14 конечных сценариев PASS; настоящий маршрут GUI → API: 6 PASS после исправления ошибочной XML-фикстуры. Валидные отрицательные контроли воспроизводят отсутствие applied-классификации.
- Направленный анализ происхождения вызовов: 68 конечных фикстур PASS. **Производственный анализ порядка блокировок остаётся FAIL** на динамическом `errnoIs → Unwrap`; неизвестная граница не освобождена от проверки.
- Смежная выборка: 36 именованных PASS и PASS пакета; полные `go build ./...` и `go vet ./...` завершились с кодом 0 на объявленной композиции. Во всех тестовых командах SKIP=0; все неуспешные попытки и их причины сохранены.
- Квалификация относится к 3309 физическим файлам и точным overlays. Её список из 30 владельцев блокировок отличается от публикуемого списка из 28; результатов самостоятельной полной ветки эти проверки не утверждают.
- Все 14 опубликованных файлов, единственный родитель и итоговое дерево прочитаны обратно и совпали. Собственное ревью и независимый аудит выполнены; внешнее ревью **этого коммита** остаётся REVIEW_PENDING.

Открыты динамический граф вызовов, владение поколением перед удалением, отдельный потерянный `Unlock` в secrets recovery, полные стандартные/тегированные Go-тесты, Product и системная квалификация. Предшествующие исправления двух upgrade-ветвей и восстановления weekly-refresh сохранены.

<details>
<summary>Предыдущие описания и результаты точных исторических вершин</summary>

## Recovery continuation — 22 November 2026

**Current head:** `203c7bacef98c0445549512a2b6cab61eca6dcd1`. This pull request remains Draft.

Both upgrade callers discarded restoration errors and could force-import over a task that occupied the name after a failed replacement. They now retain the private snapshot before mutation and use the existing create-only recovery capability, preserving the original and recovery errors. Weekly-refresh uses the same recovery owners, keeps unverified occupants intact, and no longer turns a failed Create into success merely because XML matches. Returning the original dynamic scheduler preserves its optional XMLCreator capability.

[Exact sources, raw events, controls and limits](https://github.com/applicate2628/mcp-local-hub/blob/203c7bacef98c0445549512a2b6cab61eca6dcd1/docs/modernization/verification/scheduler-upgrade-recovery-20261122/README.md).

- Upgrade checks: 21 named PASS plus package; 47 adjacent PASS plus package. Old-owner control: 1 PASS, 20 FAIL plus failed package.
- Weekly tagged checks: 21 named PASS plus package; 83 adjacent PASS plus package; composed upgrade: 50 PASS plus package. Old-owner control: 21 FAIL plus failed package. No SKIP.
- **Lock-order source analysis remains FAIL**, with its raw output preserved: four calls of the existing secure writer's local cleanup closure have unresolved origins. The new exact ExportXML boundary checks pass; no blanket storage exemption was added.
- All 15 publication files, the single parent and tree were independently read back. Tests qualify the explicitly listed 85881298-based composition, not the whole standalone branch.
- Native generation ownership, the pre-existing applied-release result projection mismatch, full standard/tagged suites, Product/native acceptance and exact-head external review remain open.

The preceding [snapshot-admission checkpoint](https://github.com/applicate2628/mcp-local-hub/blob/63ba3e771cb5a165e276bf7c481e10a3d3a19837/docs/modernization/verification/scheduler-upgrade-snapshot-20261122/README.md) and its qualified results remain preserved.

<details>
<summary>Previous description and historical head-specific results</summary>

## Stop failures remain visible — 22 November 2026

The four legacy lifecycle paths discarded `Scheduler.Stop` failures. That could report a successful stop, run a task after its stop failed, and release targeted stop intent. Each path now preserves the task's error, blocks its subsequent restart and continues processing its peers.

**Current head:** `b575b2c1c44c03bd14da281f8b81f50c6aec4bb9`, one parent `368e8b6cb8e95d9e5ebbf9305f75aa740991b1ab`. Production scope is four checked calls in `internal/api/install.go`, with one focused regression file. All five publication files were read back byte-for-byte.

- Negative control: 24 leaf scenarios, 8 PASS / 16 FAIL; including groups/root, 8 test PASS / 21 test FAIL plus package FAIL, exit 1. Raw messages separately expose 16 false successes, eight forbidden restarts and four lost stop intents.
- Candidate: all 24 leaves PASS (29 test/subtest PASS plus package PASS). Nine neighbouring roots give 25 test/subtest PASS plus package PASS, including both current-master BackendRunHold checks. Zero FAIL/SKIP in the candidate runs; race detector enabled.
- Full build/vet passed on the declared composition: physical `858812988833a84368c60fba1540ae601155fa71` plus 39 preserved replacements and one new test. These are composition results, not full standalone-branch acceptance.
- The preceding owner-SID and immediate pre-delete XML recheck correction at `368e8b6…` remains included. Native Run/RunOwned/Stop/Delete, cancellation, task replacement and complete process-tree settlement still require their own qualification.
- Own and independent source reviews and own raw-evidence verification completed. External review of this new head remains pending. Keep Draft; no full default/tagged Go acceptance is claimed.

[Stop-error report and complete source/evidence composition](https://github.com/applicate2628/mcp-local-hub/blob/b575b2c1c44c03bd14da281f8b81f50c6aec4bb9/docs/modernization/verification/scheduler-stop-errors-20261122/README.md) · [Preserved SID/pre-delete report](https://github.com/applicate2628/mcp-local-hub/blob/368e8b6cb8e95d9e5ebbf9305f75aa740991b1ab/docs/modernization/verification/scheduler-sid-predelete-20261122/README.md).

---

<details>
<summary>Previous Scheduler description and historical qualification</summary>

## Композиция с master и BackendRunHold — 22 ноября 2026

**Текущая вершина:** `0bdb725ed261c532eba07d645c2a14880d12b9fd`. PR остаётся **Draft**, без слияния. Этот инкремент публикует доказательства и не меняет активный продуктовый код ветки.

На точном master `75dbc6406123e22468f168b861d5e0622c9d5769` согласованы все 27 Go-файлов #660 и пять файлов изоляции API TestMain из #665. В единственном пересекающемся `install_parsed_manifest.go` добавлена только проверка `len(installSchedulerRecoveryTasks(err)) == 0`; оба master-инварианта `BackendRunHold` сохранены.

Два новых теста проверяют сохранение запрета запуска backend при подготовке установки и безопасной временной пробной записи. Они сохранены **как неактивные overlays** вместе с точными входами: их fixtures принадлежат master и не добавлены в старую самостоятельную тестовую сборку #660.

### Фактические результаты

- Отрицательный контроль с историческим целым файлом #660: Go exit **1**, два корневых FAIL и FAIL пакета, 0 PASS/Skip. Он показывает риск неверной подмены старого файла, а не доказывает ошибку обычного Git merge.
- Кандидат под `-race`: **10 корней + 16 подтестов PASS, отдельно PASS пакета**, 0 FAIL/Skip.
- Полные `go build ./...` и `go vet ./...` с точными overlays — exit 0.
- Во всех пяти фазах 3270 исходных файлов совпали до/после. Дополнительные полные инвентаризации не нашли посторонних, отсутствующих или изменённых compile inputs.
- [Отчёт, собственное ревью и полный архив](https://github.com/applicate2628/mcp-local-hub/blob/0bdb725ed261c532eba07d645c2a14880d12b9fd/docs/modernization/verification/scheduler-hold-composition-20261122/README.md) содержат точные команды, пять квитанций, raw-журналы и 33 подмены.

### Открытые условия

Это квалификация композиции, не самостоятельной опубликованной ветки. Реальные Task Scheduler Run/RunOwned/Stop/Delete, права и учётные данные, отмена/всё дерево процессов, внешняя замена, сквозное восстановление, хранение снимков и full Go остаются открыты. Отдельно зафиксирована неоднозначность короткого имени владельца между доменами; следующий шаг должен учитывать SID (идентификатор безопасности Windows).

Собственное и дополнительное внутреннее ревью выполнены. Внешнее ревью точного опубликованного коммита — **REVIEW_PENDING**.

<details>
<summary>Предыдущие результаты и описание — сохранены без изменения</summary>

<!-- mcphub-current-audit:2026-11-21:start -->
## Актуальное состояние и продолжение после #662 — 21 ноября 2026 года

**Статус:** Draft. Восстановление устаревших задач исправлено в опубликованной адресной области; полные и нативные условия готовности остаются открытыми.

**Вершина PR:** `1cffaab021686a7ae27a5ffc038f280160f96a60`, один родитель `610fb30348c10be7b645b6ce43c862397b190f60`. **Базовая ветка:** `master`; её вершина при сверке — `75dbc6406123e22468f168b861d5e0622c9d5769`. Составная проверка с этим master не выполнялась.

**Технический владелец:** scheduler + API install/adopt: задачи и восстановление.

**Последнее изменение:** `pruneObsoleteServerTasks` сохраняет снимок на диске до удаления, прекращает дальнейшее прореживание при неподтверждённом результате ошибочного Delete и восстанавливает только свободное имя через существующий `CreateXMLIfAbsent`. Предшествующие компенсации и их причины/имена доходят до install/adopt; чужая задача не замещается. Существующий владелец восстановления переиспользован. Именованный поиск hidden-задач и прежние исправления сохранены.

**Проверено на точной базе с четырьмя подменами файлов:** отрицательный контроль — 18 тестовых/подтестовых FAIL + 1 FAIL пакета; кандидат с `-race` — 18 PASS + 1 PASS пакета, без пропусков. Оба результата относятся к тем же шести корневым тестам и 13 конечным сценариям. Полные `go build ./...` и `go vet ./...` с этими подменами — PASS.

**Соседняя выборка:** 106 тестовых/подтестовых PASS + 1 PASS пакета, один существующий платформенный SKIP; команда exit 0, обёртка **FAILED**, `completed=false`. Пропуск не скрыт и общий квалифицированный PASS не заявлен.

**Границы:** полные default/tagged тесты, нативные Task Scheduler и установленный Product — **NOT_RUN**. Результаты прямых дочерних процессов не доказывают завершение всего дерева. Изменение хеша Git между замерами среды и точный состав свидетельств отражены в отчёте. Исходные байты и все семь опубликованных файлов прочитаны обратно и сверены.

**Следующий необходимый результат:** безопасные полные входы, композиция с master с сохранением `BackendRunHold`; затем реальные Run/RunOwned/Stop/Delete, права и учётные данные, отмена/срок всего дерева, внешняя замена, восстановление и хранение снимков. Равенство XML не является атомарной гарантией поколения.

**Ревью:** собственное и независимое чтение точного исправления и свидетельств выполнены. Внешнее ревью опубликованного SHA остаётся **REVIEW_PENDING**; нового внешнего квотного запуска или ручного CI не выполнялось.

[Отчёт и свидетельства](https://github.com/applicate2628/mcp-local-hub/blob/1cffaab021686a7ae27a5ffc038f280160f96a60/docs/modernization/verification/prune-recovery-2026-11-21/README.md) · [Действующий план](https://github.com/applicate2628/mcp-local-hub/blob/f9966f008aed4d0cfa80d88cc80c12d08e03634f/.plans/2026-11/open-pr-integration-20261117.md#3-порядок-выполнения) · [Исторический аудит до исправления](https://github.com/applicate2628/mcp-local-hub/blob/cc7b33a9e69028165cbab7042c536f846e6f901f/docs/modernization/verification/pr-state-audit-2026-11-21/README.md#pr-660).
<!-- mcphub-current-audit:2026-11-21:end -->

<details>
<summary>Предыдущее описание и исторические результаты — сохранены без изменения</summary>

## Problem and behavior

Scheduler consumers must distinguish an absent task from a failed observation and must preserve recovery data when replacement or rollback cannot be confirmed. This PR retains hidden-task lookup, signed LastResult handling and original cancellation causes; destructive replacement/pruning requires a successfully retained nonempty definition. An unavailable snapshot does not authorize deletion.

Head `610fb30348c10be7b645b6ce43c862397b190f60` includes recovery of the current task and earlier completed steps. After an uncertain current Delete or Create of a previously absent task, one ExportXML observation checks the prior definition/absence. Unconfirmed outcomes retain causes and names through the existing incomplete-recovery error; outer adopt preserves recovery material and original state. The classifier does not retry mutation. Separate Linux autostart handling escapes literal `%` as `%%` and recognizes the earlier configuration.

## Historical evidence with exact inputs

[Immutable R22/R23 reports, manifests and receipts](https://github.com/applicate2628/mcp-local-hub/blob/b1d932ea8925ef3324d539e5de006daeae65ba2a/docs/modernization/verification/completion-r22-r23-2026-11-21/README.md):

| Selection and source | Recorded outcome |
|---|---|
| R22 earlier-step rollback: `ba17867125b3728fb734e90b383639a3ff9175ee` + pinned overlays | Control: 6 test PASS / 36 FAIL and package FAIL. Candidate: core 84, backend 8 and adjacent snapshot 15 test PASS, each with package PASS; build/vet exit 0. |
| R23 current mutation: `a63199628c1689b4b77a50abfc7432396a1893f4` + pinned overlays | Control: 16 test FAIL plus package FAIL. Candidate with race detector: 16 test PASS plus package PASS; build/vet exit 0. |
| R23 Linux quoting: `ba17867125b3728fb734e90b383639a3ff9175ee` + pinned overlays | Control: 12 FAIL / 2 PASS. Candidate: 28 PASS events (13 terminal cases repeated twice plus parents), adjacent 4 PASS. Installed systemd analyzer rejected `%Z` with exit 1 and accepted `%%Z` with exit 0. No service ran. |

**The R23 adjacent wrapper remains FAILED:** the command exited 0, all 20 roots passed, and it recorded 77 test PASS plus package PASS, but one explicit Linux/macOS-only `unsupported_platform` SKIP occurred on Windows. The receipt is retained unchanged; it is not relabeled PASS.

A separate raw-export roundtrip on `ba17867125b3728fb734e90b383639a3ff9175ee` confirmed 2 registrations, 2 deletions and 2 absence checks; all 4 raw exports matched byte-for-byte. Private XML hashes are published, not the private XML itself. This does not establish atomic object generations, full credentials or an installed-product transaction.

[Earlier snapshot/prune evidence](https://github.com/applicate2628/mcp-local-hub/blob/4173a05ea67fcd73632c05b37624fb4ffe1765c7/docs/modernization/verification/scheduler-prune-resolver-2026-11-20/RESULTS.md) retains its own exact `f5fb00370a833255eacb992a7cd32f2e68da7399` result: 26 prune and 12 adjacent test/subtest PASS events, each plus package PASS, under race detection and two repeats. Model tests did not perform real deletion/restore through that helper.

## Limits and R24 status

The whole `610fb303...` head was not executed as one qualification input. Prior full Go FAIL remain; later broad commands are NOT_RUN for distinct reasons: ordinary-suite TestMain isolation is unqualified, while tagged API/CLI has known Product/entrypoint/Serena prerequisite failures. Native active cancellation, complete tree deadlines/settlement, real Run/RunOwned/Stop and production Delete, external replacement races, passwords/rights, snapshot lifetime and end-to-end installation remain open. No general CIM (Common Information Model) absence classifier or canonical-export equivalence is established.

R24 changes this description only. No new native run, full-head PASS or formal external approval is asserted; keep Draft.

## Evidence archive

[R24 audit and source manifests](https://github.com/applicate2628/mcp-local-hub/blob/b21402535ac4df032408d7bf06200ddd0557abf1/docs/modernization/verification/completion-r24-2026-11-21/README.md) · [Original PR descriptions, preserved verbatim](https://github.com/applicate2628/mcp-local-hub/blob/b21402535ac4df032408d7bf06200ddd0557abf1/docs/modernization/verification/completion-r24-2026-11-21/original-pr-descriptions.zip). The archive retains the complete historical narrative, failed attempts and original source references; earlier results remain limited to those inputs.


</details>

</details>

</details>

</details>

</details>

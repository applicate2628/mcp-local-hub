<!-- source-v4-preparation-20261126:start -->
## Продолжение подготовки — 26 ноября 2026, МСК

Source-v4 завершился закрытым **FAIL**: checkout превысил 120 секунд. Приняты исходные владельцы трёх команд и шесть потоков чтения; полного допуска нового Source нет. Обновление 13 тестовых файлов и новый полный прогон пока **NOT_RUN**. Предыдущий full-v10 остаётся FAIL, исторический UNKNOWN старой области и внешнее REVIEW_PENDING сохранены.

Проверяемые результаты и точные SHA: [закрытый отказ подготовки](https://github.com/applicate2628/mcp-local-hub/blob/fe76130b88da9a854219c2bd4143244cefb3b31c/docs/modernization/verification/cache-owner-custody-20261126/source-v4-closed-failure-20261126.md). Дальнейшая подготовка использует свежий Source-v5 с диагностическими событиями Git в исходном stderr и прежними сроками команд. Эта документация не выдаёт PASS текущей публичной голове или полной регрессии.
<!-- source-v4-preparation-20261126:end -->

### Предыдущее описание контрольной точки

<!-- provider-helper-followup-20261126:start -->
## 26 ноября 2026 — адресная квалификация и подготовка полной регрессии

Текущая документационная вершина: [`0cd8f7679af2db43f736897b6ca8c8e688644608`](https://github.com/applicate2628/mcp-local-hub/commit/0cd8f7679af2db43f736897b6ca8c8e688644608). [Единый план](https://github.com/applicate2628/mcp-local-hub/blob/0cd8f7679af2db43f736897b6ca8c8e688644608/.plans/2026-11/open-pr-integration-20261117.md#provider-helper-qualification-20261126).

**Provider helper: 3 PASS, 0 SKIP.** Два тестовых изменения сохраняют успешно собранные helper-образы в корне активного TestMain; каждый сценарий создаёт собственные дочерние процессы. Пять исходных native-команд и 11 I/O workers завершены, source/образы неизменны. [Точный состав и доказательства](https://github.com/applicate2628/mcp-local-hub/blob/0cd8f7679af2db43f736897b6ca8c8e688644608/docs/modernization/verification/cache-owner-custody-20261126/provider-helper-qualification-20261126.md). Исправление опубликовано в [#665@d30326c](https://github.com/applicate2628/mcp-local-hub/commit/d30326c099ced46984cee38d4455498d3209a967); оба файла перечитаны и сверены по байтам.

[Отдельное наблюдение запуска](https://github.com/applicate2628/mcp-local-hub/blob/0cd8f7679af2db43f736897b6ca8c8e688644608/docs/modernization/verification/cache-owner-custody-20261126/startup-observation-20261126.md) завершилось PASS. Старый интервал до рождения процесса 27,9665 с не воспроизведён; причина прежнего отказа не установлена.

**Полный v10 остаётся FAIL.** Эти проверки не подтверждают PASS обеих обязательных команд или точных текущих публичных heads. Следующий этап — физическая Source-v4 с 13 тестовыми изменениями, фактическая приёмка новой композиции и полный прогон с прежними сроками. Product/Serena остаются привязаны к прежнему 3ebe. Внешнее ревью: REVIEW_PENDING. PR открыт как Draft.

<!-- provider-helper-followup-20261126:end -->

### Предыдущее описание контрольной точки

<!-- cache-owner-custody-20261126:start -->
## 26 ноября 2026 — исходные владельцы и завершившийся полный прогон

**Draft, открыт, не слит.** Документационная вершина: [`0b6d3c3bc6f2c404b6ff8134c14b5cb3411f3889`](https://github.com/applicate2628/mcp-local-hub/commit/0b6d3c3bc6f2c404b6ff8134c14b5cb3411f3889). [Единый план](https://github.com/applicate2628/mcp-local-hub/blob/0b6d3c3bc6f2c404b6ff8134c14b5cb3411f3889/.plans/2026-11/open-pr-integration-20261117.md#cache-owner-custody-checkpoint-20261126) и [отчёт с исходниками и доказательствами](https://github.com/applicate2628/mcp-local-hub/blob/0b6d3c3bc6f2c404b6ff8134c14b5cb3411f3889/docs/modernization/verification/cache-owner-custody-20261126/README.md) сохраняют аудит от 25 ноября и все прежние результаты.

Контракт корневого владельца прошёл **14/14 нативных сценариев**, фактическая композиция v13 — **2/2**. Проверены исходные объекты процессов и потоков, отказ при смерти клиента и сохранение UNKNOWN. Это ограниченные квалификации исполнителя.

Полный `literal-final-v10` на частном Test Source `aca81a1743ccfbf3e10902769a9b071eb911919e` завершился **FAIL, exit 1**, за 2295,136 с. Build и vet прошли. В default получены 62 PASS из 64 адаптеров; API/CLI не начали полные наборы, поскольку подготовка превысила неизменённые 30 с. Оба tagged-набора остановились по прежнему общему сроку 5 минут. Получены завершения всех 19 внешних команд; повторная независимая приёмка шести отказавших вложенных владельцев отделена от остальных 124 квитанций, которые заново не перечитывались.

Опубликованы только адресные тестовые изменения: #665 — `03719d32a13a20f3254eccc148684efae3c3287a`, #666 — `50d94a7854d98b31e2c29e35e06cefedc1a2d496`. Дубли уже покрытых #664 случаев исключены. Все десять файлов прочитаны обратно с совпадением байтов. Исторический частный ledger для 36 затронутых тестов содержит **33 PASS / 3 NOT_RUN**; это не запуск публичных heads.

Прежний PASS настоящей Serena остаётся привязан к Product `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`. Текущий #637 содержит отдельные производственные изменения; старый PASS их не квалифицирует. Прежняя проблемная область UNKNOWN не изменена.

Четыре закрытых ресурса прежнего v9 перенесены в trash с сохранением исходной идентичности и журналами PREPARED → MOVED. Важные отчёты и результаты сохранены вне trash.

**КТ-2 и итоговая приёмка открыты. Внешнее ревью точных heads — REVIEW_PENDING.** Далее: доказать и исправить причину задержки подготовки, квалифицировать изменённый участок, получить PASS обеих полных команд, завершить синхронизацию и внешнее ревью. Слияний, ручных CI-запусков и платных ревью в этом продолжении не было.
<!-- cache-owner-custody-20261126:end -->

<details>
<summary>Предыдущие описания, аудит и результаты — история соответствующих исходников</summary>

<!-- audit-remediation-20261125-current -->
## Аудит и исправления — 25.11.2026

Текущий commit: `62199b68b2fc59095edacfe47d6990d48accbb2f`. Новых PR и слияний нет; PR остаётся Draft.

В этот PR включены четыре test-only исправления (99+/8-): явный owner legacy-фикстур без изменения DACL, портовые предпосылки adopt, ожидание исходного recovery Execute и Close Sonnet при Fatal. Остальное — аудит, матрица 19 замечаний/13 заявок, результаты и воспроизводимый состав. Применены 53 релевантных навыка; реестр сохранён в исходном аудите.

**Общая приёмка FAIL.** На `14af01e` build/vet PASS; default: 56 пакетов PASS, 8 FAIL, 11 без тестов; tagged API/CLI FAIL. На `c730cae` source replay 38/38 деревьев PASS; legacy 24+24, adopt 2, Sonnet 3 и VTune 1 PASS; recovery timing остаётся FAIL. Fatal/Goexit control дал ожидаемый exit 1 и подтверждённую очистку. Full `c730` NOT_RUN. Новая Product qualification и внешнее Codex-ревью точного head не подтверждены.

Семь тематических обновлений опубликованы: #624, #629, #655, #660, #628, #671, #659; exact heads/readback в evidence. ARA-01 (реальный provider/install/reconcile/rebind), WL-04 (actual authenticated named pipe и removed-descriptor cleanup), CP-01 и приёмка заявок остаются открытыми.

Документы в `.plans/2026-11/`: `audit-remediation-20261125.md`, `audit-implementation-status-20261125.md`, `audit-publication-readback-20261125.json`, `audit-followup-review-20261125.json`; состав и десять native receipts — `integration-audit-20261125/`.

[Матрица замечаний и заявок](https://github.com/applicate2628/mcp-local-hub/blob/62199b68b2fc59095edacfe47d6990d48accbb2f/.plans/2026-11/audit-implementation-status-20261125.md) · [Результаты native проверок](https://github.com/applicate2628/mcp-local-hub/blob/62199b68b2fc59095edacfe47d6990d48accbb2f/.plans/2026-11/integration-audit-20261125/native-evidence.md) · [Воспроизведение состава](https://github.com/applicate2628/mcp-local-hub/blob/62199b68b2fc59095edacfe47d6990d48accbb2f/.plans/2026-11/integration-audit-20261125/README.md)

Публикация `62199b68b2fc59095edacfe47d6990d48accbb2f` подтверждена штатным native push gate: exit 0, все 6 команд завершены, исходные Job/handles/readers закрыты, контроллер 16.293 s. SHA-256 исходной квитанции: `5ed9ee1ddbc208c8637fac84984e0d3eb52e3d381a2a2a8969698fd7abd942a8`. Независимый GitHub readback совпал; open/Draft/unmerged. Первое отклонение из-за синтетических canaries сохранено в отчёте; публичный лог очищен.

**Остаточный план:** допустимые Product/controller fixtures → конкретные причины FAIL и незавершённые сценарии → один общий прогон → сверка заявок и точных heads. PR не сливать.
<!-- /audit-remediation-20261125-current -->

<!-- cache-continuation-20261124:start -->
## 24 ноября 2026 — продолжение проверки кэша

**Draft (черновик), открыт, не слит.** Вершина кодового среза: [`c4bd4a0ff5a72b780bd00b44fbb25623b0db4dfd`](https://github.com/applicate2628/mcp-local-hub/commit/c4bd4a0ff5a72b780bd00b44fbb25623b0db4dfd). Обновление добавлено к прежней вершине без принудительной перезаписи истории; база `perf/rx16-native-observation-20261112` сохранена.

В проверке конкурентного воспроизведения GUI (графического интерфейса) сохранена исходная причина неожиданной ошибки. Изменена одна диагностическая строка; условия успешности, сроки и владение процессами прежние.

**Проверено:** собственное статическое ревью требований, корректности, совместимости и владения ресурсами; независимое ревью; точное применение изменений к актуальным исходникам; обратное чтение всех изменённых файлов из GitHub с совпадением байтов; состав diff и сохранение предшествующей истории. Подтверждённых нерешённых замечаний в этом diff не найдено.

**Не выполнено на этой новой вершине:** компиляция и тесты Go, две полные регрессионные команды и настоящая квалификация установленной Serena. Их статус — **NOT_RUN**, готовность к слиянию не заявляется. Успехи и отказы прежних исходников сохраняют собственную область применимости. Слияния PR и ручной запуск CI (непрерывной интеграции) не выполнялись.

**Локальная контрольная точка:** на отдельной композиции `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4` настоящая установка и потребитель Serena в headless-профиле приняты (КТ-1). Сборка и vet прошли; обычная полная регрессия остановлена до выполнения тестов internal/api, а tagged-команда не запускалась. Неопределённость исходного ожидания потоков удерживает КТ-2 открытой; отдельный кандидат исправляет подтверждённый дефект обработки исключения join (2 отрицательных контроля, 5 целевых сценариев PASS, 12 настоящих изолированных дочерних процессов PASS). Первоначальная задержка не воспроизведена, прежний UNKNOWN и unsafe-stop неизменны, кандидат не включён в полный запуск. Результаты этой частной композиции не переносятся на публичную вершину PR. [Единый актуальный отчёт и остаток](https://github.com/applicate2628/mcp-local-hub/blob/fc13bdaba6e11716f7c99a0b4f679ad94887d92f/.plans/2026-11/open-pr-integration-20261117.md#cache-validation-checkpoint-20261124).
<!-- cache-continuation-20261124:end -->

<details>
<summary>Предыдущие описания и результаты — история соответствующих исходников</summary>

<!-- cache-console-stack:2026-11-23:start -->
## Текущее состояние — 23 ноября 2026 года

**Draft; PR открыт и не слит.** Вершина: `b884b1ea90edd041204bc15ac6c9f0f3fb8b6da0`; дерево: `63f5fc0be17b71deb182a8b3a06d79cf28ac44e3`. База: `perf/rx16-native-observation-20261112`. Различие PR с этой базой содержит 105 путей; оно сохраняет ранее существовавший объём ветки.

Ветка приняла общую консольную политику #628; база PR теперь указывает на эту зависимость. Добавлены 14 адресных настроек фоновых запусков. Существующие test-entry изменения, владение исходными процессами и все документальные изменения вершины `5268b91f…` сохранены.

Обновление сохраняет два настоящих родителя: исходную вершину этой ветки и фактически опубликованную зависимость. Принудительной перезаписи истории нет. Выполнены собственное и независимое ревью, проверка исходных файлов, трёхсторонних слияний, полного дерева и обратное чтение GitHub.

**Сборки и тесты именно этой новой вершины — NOT_RUN.** Адресный V3 PASS относится к отдельному коммиту `c3b8c802be190c335b84ff7323a14d9eb63518b6`; новые деревья содержат также изменения текущих публичных баз. Этот результат, исторические прогоны и успешная сборка Product не заменяют две полные команды и внешнее ревью нового SHA. Их условия остаются открытыми.
<!-- cache-console-stack:2026-11-23:end -->

<details>
<summary>Сохранённое описание, контракты и предшествующие результаты</summary>

## Current qualification — 2026-11-23

Exact documentation head: `5268b91f178bf598efb78287291ca43909294b0b`. **Draft; full standard/tagged suites, runtime lifecycle and exact-head external review remain open.**

The previously issued Source `4c3eb47d002ea729fe80370a75506a1d0c9650ac` now has three separately built and verified inputs: ordinary Product, command-test Product #657, and a `serena-qualification` publisher. All builders exited 0. Source identity, PE roles, artifact hashes and canonical-entry bindings were read back; Serena was rejected as ordinary Product. These artifacts exclude new #660, #663 and #669 changes.

[Build/admission evidence and artifact locations](https://github.com/applicate2628/mcp-local-hub/blob/5268b91f178bf598efb78287291ca43909294b0b/docs/modernization/verification/publisher-roles-20261123/README.md) · [Updated residual A–D plan](https://github.com/applicate2628/mcp-local-hub/blob/5268b91f178bf598efb78287291ca43909294b0b/docs/modernization/verification/publisher-roles-20261123/residual-plan.md).

Native recovery namespace qualification is blocked by `CreateFileW` error 5 at `C:\\Program Files\\WindowsApps` under the required `READ_CONTROL` ancestor contract. The source/temp/Go paths pass the same read-only probe. No ACL or execution-policy change was made.

#660 is now `58d7c948e410fdb5969abb9a8a4febfea8f14150`: standalone targeted events 283 PASS / 0 FAIL / 0 SKIP; standard full build/vet pass. Independent #669 is `ec2248643d875390c41b519b85424a5ca0ce90d1`: recovery unlock failures and both causal branches retained. Composing them needs the single obsolete raw-lock census row removed.

Historical failed attempts and NOT_RUN records are retained with their original scope. Verified unused duplicates moved directly to trash; source, products, evidence and recovery remain outside it. No donor closure, merge, installer, native Scheduler mutation or workflow dispatch occurred.

---

## Retained earlier description and evidence

## Остаточные контракты: контрольная точка 23 ноября 2026 года (МСК)

**Документальная вершина:** `2a0657728b45d7dc1c654101c323f883424b7da3`. PR остаётся **Draft**. Публикация сохраняет самостоятельные обязанности исходных PR и обновляет канонический остаток A–D.

- [Канонический остаточный план A–D](https://github.com/applicate2628/mcp-local-hub/blob/2a0657728b45d7dc1c654101c323f883424b7da3/.plans/2026-11/open-pr-integration-20261117.md#p0-next-residual-20261122).
- [Serena Source: состав, фактические попытки, независимый аудит и следующие publisher-входы](https://github.com/applicate2628/mcp-local-hub/blob/2a0657728b45d7dc1c654101c323f883424b7da3/docs/modernization/verification/serena-source-preparation-20261122/README.md).
- [Контрольная точка: проверки, Rust, очередь, обратное чтение и уборка](https://github.com/applicate2628/mcp-local-hub/blob/2a0657728b45d7dc1c654101c323f883424b7da3/docs/modernization/verification/residual-next-closeout-20261122/README.md).

### Подтверждённый результат

Для согласованной композиции из 109 изменений подготовлен действительный Source: 3323 файла, 50 823 826 байт, дерево `8928e54342bc700b07f977a1d8f22bbaf986ad04`, локальный коммит `4c3eb47d002ea729fe80370a75506a1d0c9650ac`. Штатный helper выдал снимок и `source_epoch=4e07a48c4d77d0cdd086dcda1f704cae8d2ccab8067a65811f414ea6de55482d`; отдельное обратное чтение совпало. Этот локальный Source-коммит отличается от документального коммита PR.

Независимый аудит пересчитал дерево, коммит, полный состав и epoch, проверил 121 вход и все 27 команд v3. Прежние отказы v1/v2 сохранены. У v2 дочерний Git продолжил работу после тайм-аута внешней обёртки; поздний полный вывод не заменяет исходную квитанцию FAILED и не закрывает общий срок завершения дерева.

#660 опубликован как `026928c31977788444f6b3e3504324ae21106446`, #663 — `67f89401c31736399c84d866828241db8c12d8cb`; их 14 и 8 изменённых путей прочитаны обратно побайтно. Собственные производственные изменения остаются в этих владельцах. Новые A1/A2 #660 и все изменения #663 не входят в данный Source-состав.

Rust reader #627 и контракты Go-потребителей прочитаны; для отдельного a217 process-компонента получены 64 точных native source файла. Новая квалификация Rust этим чтением не заявляется. В trash перенесены 15 завершённых transport-дублей после полного сравнения с сохраняемыми ZIP; исходники, журналы и восстановление остаются вне trash.

### Открытые условия

Source PASS не является выпуском Product или запуском Serena. Ordinary Product, Serena qualification publisher и state-isolated Product #657 имеют разные проверенные интерфейсы; их входы и ограничения сохранены в пакете. Полные стандартные/тегированные тесты, реальные установки и lifecycle, платформенное исполнение и измерения остаются открытыми. Производственный барьер weekly на динамическом Unwrap и общий архитектурный барьер остаются FAIL. Требуется внешнее ревью точных новых коммитов.

### Фактическое обратное чтение этой публикации

После обновления ветки повторно получены Git-объекты `2a0657728b45d7dc1c654101c323f883424b7da3`: проверены единственный родитель, ожидаемое дерево, точный набор 19 изменённых документальных путей, режимы, blob и все байты. Исходные области canonical вне заменённого остаточного раздела сохранены. Квитанция приведена ниже; текст PR затем также читается обратно отдельно.

<details>
<summary>Квитанция точного коммита и файлов</summary>

```json
{
  "observed_utc": "2026-11-22T21:57:17.127616+00:00",
  "commit": "2a0657728b45d7dc1c654101c323f883424b7da3",
  "parent": "8fc6780873e8d2580ea889c572535c5fb8a430dd",
  "tree": "4d78f3aaaffa2fd399113be99ada187f8b6ae4e7",
  "method": "Fetched Git objects; all path/mode/blob bindings and exact bytes",
  "unexpected_changed_paths": [],
  "files": [
    {
      "path": ".plans/2026-11/open-pr-integration-20261117.md",
      "bytes": 163908,
      "sha256": "8106ce4d98a315135c712e98940e18a32b0447f3899ce059bcee6f34a1b77703",
      "blob": "4221de27b445026ce9d5aa4e01746cb76af2947f",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/residual-next-closeout-20261122/C-source-triage/A217-SOURCE-ACQUIRED.md",
      "bytes": 11714,
      "sha256": "a8105ced051f1d141f64d1329a6af168eb5d3d964e9402f205c75278aab867ce",
      "blob": "d4e263cd8ae31376c5fc08f6ca2c85bd1c8ed0bd",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/residual-next-closeout-20261122/C-source-triage/README.md",
      "bytes": 29069,
      "sha256": "0fc71a7b2b3a2043a845ed8eb3e7032bf84e3a4cc13e54475f235bb01fffbd96",
      "blob": "29f4654e33b2541763af63e9fdc1398820cd7cb2",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/residual-next-closeout-20261122/C-source-triage/archive.json",
      "bytes": 2272,
      "sha256": "cec544cd005a1da9a7a51f306343abb0ea8ccf5e2fff908d61833a52fad3acb5",
      "blob": "9371e5ebacdf55cdc08f195da1c94108626994fd",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/residual-next-closeout-20261122/C-source-triage/source-evidence.zip",
      "bytes": 1259474,
      "sha256": "7176c8987e29f409c57bd26e88f6228af8b5a77eef39bc48c928b87d2fb8c3e0",
      "blob": "22e8b574e479e47959152934d9bd1e8ec4d14395",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/residual-next-closeout-20261122/README.md",
      "bytes": 22862,
      "sha256": "57ec905b5535c3e956e9c945b06a687474f7d23660e10ca71e6c7e19c6a9af47",
      "blob": "ec3dfa56f6f0ef5d601fc003ed88256821e7833d",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/residual-next-closeout-20261122/archive.json",
      "bytes": 3308,
      "sha256": "6835de63fbf006ef99d1b0de8defea323cee651ecb1cdbd35712eface5587871",
      "blob": "32419c76df0a44af8cdeb52996694d542da00d94",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/residual-next-closeout-20261122/queue-audit/README-pre664-publication-20261122T213449Z.md",
      "bytes": 14052,
      "sha256": "4c77c112aca57ba376cacb2a7623525c4409566110e2c89b099fcd2e2cfc99b9",
      "blob": "040ee075845300751a04526a7c71b1043063b1b7",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/residual-next-closeout-20261122/queue-audit/README.md",
      "bytes": 13419,
      "sha256": "e754838e7c918b58996e7ff3a478edcedaaefd27a4f69c6eebfdfeb000203fd8",
      "blob": "7708d50fddffca699e4496af934e00bf7201778e",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/residual-next-closeout-20261122/queue-audit/queue-snapshot-pre664-publication-20261122T213449Z.json",
      "bytes": 37488,
      "sha256": "fd4a172e3efe4ccbad9fdc04ff0da5b1101fb363cc944050b545864fa1a77884",
      "blob": "3eae3f6807056e5be547948bf69718e1211b6d56",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/residual-next-closeout-20261122/queue-audit/queue-snapshot.json",
      "bytes": 38459,
      "sha256": "0847f00055158aeb9f3448cd9290b0868b0d5ee6f244571d2f28f3364c9cfaba",
      "blob": "d7020f1636bdb6c7c80a89866c46a209dab3db41",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/residual-next-closeout-20261122/verification-evidence.zip",
      "bytes": 34109,
      "sha256": "51ef6e1423373613cf3904bea18a13528a32956a7b12e82a15d2b995329df52d",
      "blob": "a6b639d308ccf229a920e5bfc68f088950f5ec1f",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/serena-source-preparation-20261122/README.md",
      "bytes": 22773,
      "sha256": "c5f3321024058eb4496df6fc51d1a0d539fc24c91e78ce12725735dead39eedd",
      "blob": "5c31ec775afbc341dd15a49f0d966a1565affd4e",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/serena-source-preparation-20261122/archive.json",
      "bytes": 92778,
      "sha256": "a60e70645270269732b6a8ae3e6b0bc864c327b1fd8f9741f2722e420e7c1422",
      "blob": "d963cb962e72a6caf56b195477f0cefe4b616cf8",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/serena-source-preparation-20261122/audit-report.md",
      "bytes": 18893,
      "sha256": "b91973d8349a9e59b8d7103700bf233f2c6dd7d3836573ab0377ae226f6fc7d7",
      "blob": "8cdfdabb8f52f0b50ea01940576ac88dc7b94f74",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/serena-source-preparation-20261122/audit.json",
      "bytes": 11520,
      "sha256": "1f6380b03d21f0eeab4846f875c0e8527a6eb91cff367b1cbc90f8fadfb46289",
      "blob": "8be70a4918d6ad23c63f87cf9ec07c85aca2f501",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/serena-source-preparation-20261122/bundle.zip",
      "bytes": 2506234,
      "sha256": "ff5bafb6681cf4e9dfc68dfcf17e4e5a995e321a918f6c14526be7dfc07c2213",
      "blob": "b45d145834c8221a7cc1932c4688e3fbdd466953",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/serena-source-preparation-20261122/replay.py",
      "bytes": 34095,
      "sha256": "bb198f617f37a64ada61cd26c7fc68296b2a34dbdb3412623203e4b1ffeea7de",
      "blob": "1c5ae8d8f73f3e0faee7420aa2a7fffa1ac06ba8",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    },
    {
      "path": "docs/modernization/verification/serena-source-preparation-20261122/verification.json",
      "bytes": 11763,
      "sha256": "e7c47a863579e52283ac7ee52b3c422021b86cb894fe0b5bc53efa373bc423ac",
      "blob": "773dbbbd58c3643ad72b8abc7b2438e5e7fe0a89",
      "mode": "100644",
      "type": "blob",
      "bytes_equal": true
    }
  ],
  "complete": true
}
```

</details>

### Очередь после обновления головы

Наблюдение 22 ноября 2026 года, 21:56:07–21:56:08 UTC: 30 открытых PR, все Draft; master `75dbc6406123e22468f168b861d5e0622c9d5769`. Подтверждены текущие головы #655/#660/#663 и новый #664 `2a0657728b45d7dc1c654101c323f883424b7da3`. Ревью точных текущих голов отсутствуют. Наблюдение предшествует обновлению этого описания; оно не требует постоянства `updated_at` и не объявляется атомарным.

<details>
<summary>Полный снимок очереди и точных ревью после публикации головы</summary>

```json
{"schema":3,"repository":"applicate2628/mcp-local-hub","scope":"Short read-only queue/review observation after #664 documentary head publication; PR description closeout is still owned by root","observation_window_utc":{"started":"2026-11-22 21:56:07 UTC","last_read":"2026-11-22 21:56:08 UTC","atomic_snapshot":false},"publication_boundary":{"pr664_head":"2a0657728b45d7dc1c654101c323f883424b7da3","after_head_publication":true,"final_pr_description_readback":"NOT_ASSESSED; root is updating the description after byte readback","updated_at_stability_required":false},"master":{"requested_ref":"master","method":"mcp__codex_apps__github_fetch_commit","sha":"75dbc6406123e22468f168b861d5e0622c9d5769","url":"https://github.com/applicate2628/mcp-local-hub/commit/75dbc6406123e22468f168b861d5e0622c9d5769","message":"Merge pull request #662 from applicate2628/codex/verified-master-20261121\n\nIntegrate verified Hub source updates through 0.4.65","warning":"The current master was requested directly. Per-PR base_sha values are separately retained metadata."},"collection":{"endpoint":"https://api.github.com/repos/applicate2628/mcp-local-hub/pulls?state=open&sort=created&direction=asc&per_page=100","pages":[{"page":1,"count":30},{"page":2,"count":0}],"read_count":1},"counts":{"open":30,"draft":30,"ready_for_review":0},"pull_requests":[{"number":623,"title":"docs: cross-platform Rust roadmap, COM boundaries and targeted Python reduction","url":"https://github.com/applicate2628/mcp-local-hub/pull/623","state":"open","draft":true,"head":{"ref":"docs/rust-cross-platform-no-com-20261112","sha":"7a7263e7ff341496aeeb5431d53f6fed388a8ed0","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-12T16:26:02Z","updated_at":"2026-11-21T18:32:38Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":624,"title":"perf(cmaketrace): retain bounded records and qualify terminal cancellation","url":"https://github.com/applicate2628/mcp-local-hub/pull/624","state":"open","draft":true,"head":{"ref":"perf/rx01-cmaketrace-benchmark-20261112","sha":"ee20f6009a8581c8141352cdbd261e1a97a0e805","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-12T16:43:20Z","updated_at":"2026-11-22T13:42:12Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":625,"title":"spike(rust): qualify process-owner lifecycle, launch context and native failures","url":"https://github.com/applicate2628/mcp-local-hub/pull/625","state":"open","draft":true,"head":{"ref":"spike/rx02-native-process-owner-20261112","sha":"287a7772a4be75ed9fb2f9ba1feb68cf035ed907","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-12T19:00:31Z","updated_at":"2026-11-21T18:29:21Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":626,"title":"test(perf): relay workload and executable-size control before Rust migration","url":"https://github.com/applicate2628/mcp-local-hub/pull/626","state":"open","draft":true,"head":{"ref":"perf/rx01-relay-baseline-20261112","sha":"279887a4ec79186672c331e075c70e0b4555daa8","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-12T19:05:45Z","updated_at":"2026-11-22T13:37:27Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":627,"title":"spike(rust): anchored verified file reader with native replacement and cap tests","url":"https://github.com/applicate2628/mcp-local-hub/pull/627","state":"open","draft":true,"head":{"ref":"spike/rx04-anchored-read-20261112","sha":"6615d239fe066eaa5fa78c22a5272d6602528274","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-12T19:27:39Z","updated_at":"2026-11-21T18:29:25Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":628,"title":"fix(status): observe Windows process identity and RSS through one handle","url":"https://github.com/applicate2628/mcp-local-hub/pull/628","state":"open","draft":true,"head":{"ref":"perf/rx16-native-observation-20261112","sha":"00f120a533f0e0e4bc7b6a56871528ff6dd9fde4","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-12T19:37:12Z","updated_at":"2026-11-21T18:29:27Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":629,"title":"fix(relay): bound responses and preserve negotiated session ownership","url":"https://github.com/applicate2628/mcp-local-hub/pull/629","state":"open","draft":true,"head":{"ref":"fix/relay-output-failure-contract-20261113","sha":"4b6ec39930cb5a0498dba068b99dbf29ba7129b7","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-12T21:36:24Z","updated_at":"2026-11-22T11:14:11Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":631,"title":"spike(ffi): qualify versioned original-process identity across Go and Rust","url":"https://github.com/applicate2628/mcp-local-hub/pull/631","state":"open","draft":true,"head":{"ref":"spike/rx02-go-rust-boundary-20261113","sha":"701535e3439d74495080463976d610a42303e5cf","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"spike/rx02-native-identity-20261114","sha":"71891067d0c24387ba873a3e4276b0462ae365ae","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-13T20:31:52Z","updated_at":"2026-11-21T18:29:32Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":632,"title":"fix(serena): discover UV from the qualified execution PATH","url":"https://github.com/applicate2628/mcp-local-hub/pull/632","state":"open","draft":true,"head":{"ref":"fix/serena-qualified-uv-path-20261114","sha":"ff4ba7f85fef35e19b39dced17d9df47bb9237a0","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-14T19:06:52Z","updated_at":"2026-11-21T18:29:34Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":635,"title":"test(serena): build a source-issued native qualification publisher","url":"https://github.com/applicate2628/mcp-local-hub/pull/635","state":"open","draft":true,"head":{"ref":"fix/serena-qualification-publisher-20261114","sha":"f639b97ccc56eb18d314d52951d1b5d167d11e5f","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"fix/serena-qualified-uv-path-20261114","sha":"8cead428a6cee57c597e2bd3ed7b3877a6445d30","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-14T19:17:29Z","updated_at":"2026-11-21T18:29:36Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":637,"title":"test(serena): run positive migration fixtures through real preparation","url":"https://github.com/applicate2628/mcp-local-hub/pull/637","state":"open","draft":true,"head":{"ref":"fix/serena-integration-qualification-20261114","sha":"1ebe00c5dbac448488d980072e689b1126875dde","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"fix/serena-qualification-publisher-20261114","sha":"f639b97ccc56eb18d314d52951d1b5d167d11e5f","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-14T19:18:01Z","updated_at":"2026-11-21T18:29:38Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":643,"title":"fix(serena): retain original owners through install and cleanup","url":"https://github.com/applicate2628/mcp-local-hub/pull/643","state":"open","draft":true,"head":{"ref":"fix/serena-cleanup-parent-observation-20261114","sha":"daaa04c14bc3944babd4efbc3a125eaf495e6534","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-14T20:21:00Z","updated_at":"2026-11-21T21:46:52Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":644,"title":"spike(rust): observe original process identity while retaining ownership","url":"https://github.com/applicate2628/mcp-local-hub/pull/644","state":"open","draft":true,"head":{"ref":"spike/rx02-native-identity-20261114","sha":"1cf5754b81c850a3569be269642b49605895eeb2","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"spike/rx02-native-process-owner-20261112","sha":"c5672f49f65713c7555e8841e46de75b045da5f8","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-14T20:39:03Z","updated_at":"2026-11-21T18:29:42Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":646,"title":"fix(serena): prefer qualified system Python discovery","url":"https://github.com/applicate2628/mcp-local-hub/pull/646","state":"open","draft":true,"head":{"ref":"fix/serena-python-discovery-preference-20261114","sha":"1df32782e7ade719e1d7f8c6153b4455a6ea740a","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-14T21:05:25Z","updated_at":"2026-11-21T18:29:44Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":647,"title":"fix(serena): own Windows Git subprocess environment","url":"https://github.com/applicate2628/mcp-local-hub/pull/647","state":"open","draft":true,"head":{"ref":"fix/serena-executable-discovery-20261114","sha":"dca92972e90c5280e6c217409acfc6fd0dfb38ea","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-14T21:49:24Z","updated_at":"2026-11-21T18:29:46Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":649,"title":"fix(serena): bind qualification home to the owned directory","url":"https://github.com/applicate2628/mcp-local-hub/pull/649","state":"open","draft":true,"head":{"ref":"fix/serena-qualification-home-20261114","sha":"73caa97a860f1eaec205ec84c1502c47ce05cdc0","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-14T22:30:58Z","updated_at":"2026-11-21T18:29:50Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":650,"title":"fix(serena): preserve qualification bindings and request budgets","url":"https://github.com/applicate2628/mcp-local-hub/pull/650","state":"open","draft":true,"head":{"ref":"fix/serena-protocol-binding-20261114","sha":"2a273c10f6e5c0651e89860f88ef0b6a0e3748ef","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"17bc78fc602efa204a74596409c939a3bf1ddc42","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-14T22:56:55Z","updated_at":"2026-11-21T18:29:53Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":653,"title":"fix(serena): validate TCP query budgets, Windows headers and IPv6 zones","url":"https://github.com/applicate2628/mcp-local-hub/pull/653","state":"open","draft":true,"head":{"ref":"fix/serena-tcp-query-budget-20261115","sha":"90fd6f69ea4a59856375e29b34cebf4953cb2ff4","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"a7d92d2852c4f6eb8afb0569ef76a545e38aacf7","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-15T11:09:36Z","updated_at":"2026-11-21T18:30:11Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":654,"title":"fix(serena): receive Ready readback with its original byte bound","url":"https://github.com/applicate2628/mcp-local-hub/pull/654","state":"open","draft":true,"head":{"ref":"fix/serena-ready-readback-20261115","sha":"d01fb6e73013d6a47628b1a9cbecd2d5f3b8e32c","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"a7d92d2852c4f6eb8afb0569ef76a545e38aacf7","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-15T12:16:59Z","updated_at":"2026-11-21T18:30:13Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":655,"title":"fix(serena): settle preparation before canceling work context","url":"https://github.com/applicate2628/mcp-local-hub/pull/655","state":"open","draft":true,"head":{"ref":"fix/serena-finalize-context-20261115","sha":"1a5a7dd04fe732df5c539860dde813c8c703586d","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"a7d92d2852c4f6eb8afb0569ef76a545e38aacf7","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-15T12:29:35Z","updated_at":"2026-11-22T16:14:00Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":657,"title":"test(windows): issue a native canonical command Product fixture for RunV5","url":"https://github.com/applicate2628/mcp-local-hub/pull/657","state":"open","draft":true,"head":{"ref":"fix/canonical-command-test-product-20261116","sha":"f20bc4ae5a369e7c0be0f7ad4a0f0ba9ea758a57","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"a7d92d2852c4f6eb8afb0569ef76a545e38aacf7","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-16T18:49:05Z","updated_at":"2026-11-22T15:01:32Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":659,"title":"fix(api): preserve unknown port observations in stop decisions","url":"https://github.com/applicate2628/mcp-local-hub/pull/659","state":"open","draft":true,"head":{"ref":"fix/api-port-pid-observation-20261118","sha":"a3568dfe3426afaca4176d57a2310097e5214e77","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"a7d92d2852c4f6eb8afb0569ef76a545e38aacf7","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-18T21:08:14Z","updated_at":"2026-11-21T18:30:22Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":660,"title":"fix(scheduler): preserve task query results and hidden lookups","url":"https://github.com/applicate2628/mcp-local-hub/pull/660","state":"open","draft":true,"head":{"ref":"fix/scheduler-hidden-task-lookup-20261119","sha":"026928c31977788444f6b3e3504324ae21106446","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"a7d92d2852c4f6eb8afb0569ef76a545e38aacf7","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-19T03:53:04Z","updated_at":"2026-11-22T21:10:33Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":661,"title":"test(cli): require semantic Serena conflict validation failures","url":"https://github.com/applicate2628/mcp-local-hub/pull/661","state":"open","draft":true,"head":{"ref":"test/serena-conflict-validation-oracle-20261121","sha":"23ed6b5c6a942812a72e345c5d886b3310e9017e","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"a7d92d2852c4f6eb8afb0569ef76a545e38aacf7","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-21T13:02:46Z","updated_at":"2026-11-21T18:30:26Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":663,"title":"fix(archguard): preserve baseline ownership after route extraction","url":"https://github.com/applicate2628/mcp-local-hub/pull/663","state":"open","draft":true,"head":{"ref":"fix/archguard-mcproute-baseline-20261121","sha":"67f89401c31736399c84d866828241db8c12d8cb","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"75dbc6406123e22468f168b861d5e0622c9d5769","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-21T20:40:19Z","updated_at":"2026-11-22T21:24:57Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":664,"title":"test: preserve package isolation and qualify GUI helper ownership","url":"https://github.com/applicate2628/mcp-local-hub/pull/664","state":"open","draft":true,"head":{"ref":"fix/p0-test-entry-preserve-20261121","sha":"2a0657728b45d7dc1c654101c323f883424b7da3","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"75dbc6406123e22468f168b861d5e0622c9d5769","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-21T21:12:55Z","updated_at":"2026-11-22T21:55:01Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":665,"title":"test(api): validate helper roles before creating package fixtures","url":"https://github.com/applicate2628/mcp-local-hub/pull/665","state":"open","draft":true,"head":{"ref":"fix/api-test-helper-dispatch-20261121","sha":"b5cd530969b7089e7d7cbaf5308543be8c443f8c","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"fix/p0-test-entry-preserve-20261121","sha":"f21728d6f3937187fea517c8ca5916b6f69ac7c5","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-21T21:16:57Z","updated_at":"2026-11-21T21:16:57Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":666,"title":"test(cli): bind helper admission and caller state before package setup","url":"https://github.com/applicate2628/mcp-local-hub/pull/666","state":"open","draft":true,"head":{"ref":"fix/cli-test-helper-dispatch-20261121","sha":"09de8e98d1aa6f122215dbe869bc6f7d0ce083f5","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"fix/p0-test-entry-preserve-20261121","sha":"f21728d6f3937187fea517c8ca5916b6f69ac7c5","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-21T21:17:58Z","updated_at":"2026-11-21T21:17:58Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":667,"title":"fix(perftools): bound discovery probes and isolate stdout fixture","url":"https://github.com/applicate2628/mcp-local-hub/pull/667","state":"open","draft":true,"head":{"ref":"test/perftools-stdout-explicit-env-20261121","sha":"1eb934595d3c21954d0bddb934be57a3fe3138cf","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"fix/p0-test-entry-preserve-20261121","sha":"f21728d6f3937187fea517c8ca5916b6f69ac7c5","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-21T21:18:01Z","updated_at":"2026-11-22T08:55:38Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]},{"number":668,"title":"fix(api): retain reader close failures and consume handles once","url":"https://github.com/applicate2628/mcp-local-hub/pull/668","state":"open","draft":true,"head":{"ref":"fix/go-reader-close-errors-20261122","sha":"1b7655ae9a0dec7d6821d8419087fd92ccadbb62","repo":"applicate2628/mcp-local-hub"},"base":{"ref":"master","sha":"75dbc6406123e22468f168b861d5e0622c9d5769","repo":"applicate2628/mcp-local-hub"},"created_at":"2026-11-22T11:09:24Z","updated_at":"2026-11-22T11:09:24Z","merged_at":null,"requested_reviewers":[],"requested_teams":[]}],"focused_review_audit":[{"number":655,"url":"https://github.com/applicate2628/mcp-local-hub/pull/655","state":"open","draft":true,"merged":false,"mergeable":true,"head_sha":"1a5a7dd04fe732df5c539860dde813c8c703586d","head_ref":"fix/serena-finalize-context-20261115","base_ref":"master","base_sha_reported_by_pr":"a7d92d2852c4f6eb8afb0569ef76a545e38aacf7","updated_at_from_get_pr_info":"2026-11-22T16:14:00Z","updated_at_from_collection":"2026-11-22T16:14:00Z","head_state_draft_agree_with_collection":true,"reviews":[{"id":5959916312,"node_id":"PRR_kwDOSJ-2SM8AAAABYz0bGA","state":"COMMENTED","commit_id":"2defe618259f7db4144824d19927b032300d160d","submitted_at":"2026-11-18T15:43:14Z","author":"applicate2628","url":"https://github.com/applicate2628/mcp-local-hub/pull/655#pullrequestreview-5959916312"}],"reviews_page_counts":[1,0],"current_head_review_count":0,"current_head_approved_review_count":0,"requested_reviewers":null,"requested_team_reviewers":null},{"number":660,"url":"https://github.com/applicate2628/mcp-local-hub/pull/660","state":"open","draft":true,"merged":false,"mergeable":true,"head_sha":"026928c31977788444f6b3e3504324ae21106446","head_ref":"fix/scheduler-hidden-task-lookup-20261119","base_ref":"master","base_sha_reported_by_pr":"a7d92d2852c4f6eb8afb0569ef76a545e38aacf7","updated_at_from_get_pr_info":"2026-11-22T21:10:33Z","updated_at_from_collection":"2026-11-22T21:10:33Z","head_state_draft_agree_with_collection":true,"reviews":[{"id":5969075719,"node_id":"PRR_kwDOSJ-2SM8AAAABY8jeBw","state":"COMMENTED","commit_id":"1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3","submitted_at":"2026-11-19T03:54:03Z","author":"applicate2628","url":"https://github.com/applicate2628/mcp-local-hub/pull/660#pullrequestreview-5969075719"}],"reviews_page_counts":[1,0],"current_head_review_count":0,"current_head_approved_review_count":0,"requested_reviewers":null,"requested_team_reviewers":null},{"number":663,"url":"https://github.com/applicate2628/mcp-local-hub/pull/663","state":"open","draft":true,"merged":false,"mergeable":true,"head_sha":"67f89401c31736399c84d866828241db8c12d8cb","head_ref":"fix/archguard-mcproute-baseline-20261121","base_ref":"master","base_sha_reported_by_pr":"75dbc6406123e22468f168b861d5e0622c9d5769","updated_at_from_get_pr_info":"2026-11-22T21:24:57Z","updated_at_from_collection":"2026-11-22T21:24:57Z","head_state_draft_agree_with_collection":true,"reviews":[],"reviews_page_counts":[0,0],"current_head_review_count":0,"current_head_approved_review_count":0,"requested_reviewers":null,"requested_team_reviewers":null},{"number":664,"url":"https://github.com/applicate2628/mcp-local-hub/pull/664","state":"open","draft":true,"merged":false,"mergeable":true,"head_sha":"2a0657728b45d7dc1c654101c323f883424b7da3","head_ref":"fix/p0-test-entry-preserve-20261121","base_ref":"master","base_sha_reported_by_pr":"75dbc6406123e22468f168b861d5e0622c9d5769","updated_at_from_get_pr_info":"2026-11-22T21:55:01Z","updated_at_from_collection":"2026-11-22T21:55:01Z","head_state_draft_agree_with_collection":true,"reviews":[],"reviews_page_counts":[0,0],"current_head_review_count":0,"current_head_approved_review_count":0,"requested_reviewers":null,"requested_team_reviewers":null}],"prior_snapshot":{"path":"queue-snapshot-pre664-publication-20261122T213449Z.json","sha256":"fd4a172e3efe4ccbad9fdc04ff0da5b1101fb363cc944050b545864fa1a77884","observation_window_utc":{"started":"2026-11-22 21:31:46 UTC","reviews_read_by":"2026-11-22 21:34:14 UTC","last_mutable_state_read":"2026-11-22 21:34:49 UTC","atomic_snapshot":false},"unchanged":true},"changes_since_pre664_snapshot":{"added_pr_numbers":[],"removed_pr_numbers":[],"changed_pr_metadata":[{"number":664,"changes":{"head_sha":{"before":"8fc6780873e8d2580ea889c572535c5fb8a430dd","after":"2a0657728b45d7dc1c654101c323f883424b7da3"},"updated_at":{"before":"2026-11-22T19:18:36Z","after":"2026-11-22T21:55:01Z"}}}],"master_changed":false,"counts_changed":false,"existing_review_ids_and_commit_ids_changed":false},"read_only":true,"mutations_performed":[],"publications_created":false,"ci_or_external_model_runs_started":false,"limits":["This is a short observation interval, not an atomic snapshot.","updated_at values are observations; root may update the PR description during closeout, and unchanged updated_at is not a gate.","No final PR-body readback, source-byte publication audit, new review, or CI run is claimed by this snapshot.","mergeable=true does not establish tests, external review, acceptance or Ready status.","Queue metadata does not prove donor branches lack unique code; no closure decision is made."],"source_methods":[{"tool":"mcp__codex_apps__github_fetch","resource":"open PR collection","pages":[1,2],"per_page":100},{"tool":"mcp__codex_apps__github_fetch_commit","commit_sha":"master"},{"tool":"mcp__codex_apps__github_get_pr_info","pr_numbers":[655,660,663,664]},{"tool":"mcp__codex_apps__github_fetch","resource":"pulls/{number}/reviews","pr_numbers":[655,660,663,664],"pages":[1,2],"per_page":100}]}
```

</details>

<details>
<summary>Прежнее описание — история до этой контрольной точки</summary>

## Остаточные контракты и композиция Serena/Product — 22 ноября 2026 года

**Текущая вершина:** `8fc6780873e8d2580ea889c572535c5fb8a430dd`. Основная публикация этого прохода — `27e9dbc53f49e0c65e6eef85cae147d1dafb2c04`; последующий коммит фиксирует обратное чтение и завершённую уборку. PR остаётся **Draft**, этапы A–D открыты.

[Актуальный остаточный план A–D](https://github.com/applicate2628/mcp-local-hub/blob/8fc6780873e8d2580ea889c572535c5fb8a430dd/.plans/2026-11/open-pr-integration-20261117.md#p0-next-residual-20261122) · [Квитанция публикаций и уборки](https://github.com/applicate2628/mcp-local-hub/blob/8fc6780873e8d2580ea889c572535c5fb8a430dd/.plans/2026-11/residual-closeout-20261122.json).

- **GUI / #663 `65fea0b…`:** владение зависимостью GUIOwnerLifecycle уже реализовано из приобретения исходной блокировки до Server. Выполнено дополнительное собственное и независимое чтение; прежние GUI-запуски повторно не заявляются.
- **Scheduler / #660 `203c7bac…`:** обе upgrade-ветви и weekly recovery сохраняют материал и ошибки, восстановление отказывается от замены занятого имени. Проверки с подставным scheduler прошли на точно перечисленном составе. **Weekly lock-order gate остаётся FAIL** на четырёх вызовах локального cleanup защищённого writer.
- **Serena/Product:** сохранены 37 основных и 74 расширенных входа, manifest, генератор и архив всех 111 вариантов. Пять пересечений проверены независимо; master-версия builder 0.4.65 сохранена. [Состав, роли образов и пределы](https://github.com/applicate2628/mcp-local-hub/blob/8fc6780873e8d2580ea889c572535c5fb8a430dd/docs/modernization/verification/serena-product-composition-20261122/README.md). Полное будущее дерево рассчитано; чистый source commit, source epoch, издатель и Product не выпускались.
- Свежая сверка: 30 открытых PR, все Draft. Прежние внешние отзывы #655/#660 относятся к старым коммитам; точные новые головы ожидают требуемого внешнего ревью. Новых внешних запусков и ручного CI не было.

Все 15 файлов #660 и семь файлов основной публикации #664, их родители и деревья прочитаны обратно. Завершённая собственная копия Comet перемещена прямо в trash; исходные сохранённые входы и общие кэши не затронуты, trash не очищался. Новых PR, закрытий, слияний, force push и удаления ветвей нет.

**Следующий проход:** разрешить конкретные границы анализа weekly с сохранением проверки блокировок; исправить передачу применённого результата при release-ошибке до GUI; затем допустить полный чистый Source и отдельные образы согласованной Serena/Product-композиции. Полные Go-наборы и реальные системные/установочные сценарии остаются открытыми.

[Предыдущая контрольная точка](https://github.com/applicate2628/mcp-local-hub/blob/19732eb7f58d1381a5cd042975a3ecfd32256669/.plans/2026-11/open-pr-integration-20261117.md).

<details>
<summary>Предыдущие контрольные точки и исторические результаты</summary>

## P0 — продолжение и порядок A–D, 22.11.2026

Этот инкремент обновляет общий план, адресную сверку владельцев и доказательства. Производственные изменения опубликованы отдельно в существующих #663, #660 и #657. **Полный план остаётся открытым; PR сохраняет Draft.**

- **#663, `d39b99db427eee026af12f8120c24477a2d9612a`:** восемь manifest-интерфейсов используют принадлежащий серверу объект API; семь исчезнувших baseline-записей удалены, остальные 2403 блока сохранены. 68 тестовых/подтестовых PASS и PASS пакета с детектором гонок, полные build/vet указанного исходника прошли. Архитектурная проверка сохраняет **FAILED: New=388, Stale=128, Grown=42; 72 workers включены в New**.
- **#660, `b575b2c1c44c03bd14da281f8b81f50c6aec4bb9`:** четыре операции сохраняют ошибку остановки и не перезапускают задание после неё; адресный Restart сохраняет stop intent. Кандидат: 29 тестовых/подтестовых PASS; соседние проверки: 25 PASS; два пакетных PASS. Различающий контроль имеет 16 листовых FAIL. Полные build/vet согласованной композиции прошли; нативные действия Task Scheduler не выполнялись.
- **#657, `477f6e3f…` → `f20bc4ae5a369e7c0be0f7ad4a0f0ba9ea758a57`:** точное сравнение относительного состава Product и отдельная односимвольная коррекция разбора PowerShell 5.1. Inventory: контроль 9 PASS / 2 FAIL, два кандидата по 11 PASS. Native parser: опубликованный исходник дал три ошибки, исправленный и составной вход — ноль. Это не новая сборка или выпуск Product.
- **Порядок A → B → C → D:** исправить производственные P0; квалифицировать полный жизненный цикл; выполнить оставшиеся измерения и оценку Rust; завершить аудит PR. Удалены устаревшие требования заново реализовать SID и inventory. Финальный аудит повторяет только проверки изменившихся границ.
- **Очередь:** повторные выборки открытых PR и открытых Draft дали одинаковые 30 номеров. Сверка семи ключевых владельцев не установила нового полностью поглощённого PR. Новых PR, закрытий, слияний, force push и удаления веток нет.
- **Windows cleanup:** шесть собственных рабочих/контрольных каталогов на Comet и Asus перенесены напрямую в trash после сохранения доказательств: 6852 файла, 552 каталога, 144220102 байта. Полные описи совпали; исходные пути отсутствуют. Наблюдения процессов ограничены доступными командными строками и известными дочерними запусками. Trash не очищался.

**Первый коммит плана:** `7afeefca988945b8a75355f89a9ab16dfaa7376d`, один родитель `58670482426cd60aee35bf09b8d945a9809eb854`; четыре файла прочитаны обратно побайтно. Собственное ревью исходников, безопасности, совместимости, сложности и свидетельств завершено; независимые внутренние чтения его дополнили.

**Текущая голова #664:** `4637df8fc3236d9307840d67e993892da1b09ae1`, один родитель `7afeefca988945b8a75355f89a9ab16dfaa7376d`. После первой публикации шесть локальных временных каталогов перенесены напрямую в trash: 3608 файлов, 264 каталога, 67121830 байт. Полные описи совпали; исходные пути отсутствуют. Рабочий `mcphub-base` и обязательные результаты сохранены для оставшихся этапов. Добавлены helper, квитанция, полные описи и подтверждение первого коммита; пять файлов продолжения, включая уточнённый план, прочитаны обратно. [Локальная квитанция](https://github.com/applicate2628/mcp-local-hub/blob/4637df8fc3236d9307840d67e993892da1b09ae1/.plans/2026-11/p0-continuation-local-cleanup-20261122/local-cleanup.json).

Полные default/tagged Go-наборы, согласованные нативные Product/Serena входы и установка/отмена/восстановление, оставшиеся профили Relay/CMake, настоящий Rust consumer и внешнее ревью актуальных голов остаются открытыми. Зависимости не устанавливались; установленный Product и политика Windows не изменялись. Ручной CI и внешние квотные исполнители не запускались.

[План A–D](https://github.com/applicate2628/mcp-local-hub/blob/4637df8fc3236d9307840d67e993892da1b09ae1/.plans/2026-11/open-pr-integration-20261117.md) · [Адресная сверка](https://github.com/applicate2628/mcp-local-hub/blob/4637df8fc3236d9307840d67e993892da1b09ae1/.plans/2026-11/continuation-scope-audit-20261122.md) · [Квитанция и точные результаты](https://github.com/applicate2628/mcp-local-hub/blob/4637df8fc3236d9307840d67e993892da1b09ae1/.plans/2026-11/p0-continuation-closeout-20261122.json).

<details>
<summary>Прежние описания и результаты — исторические источники сохранены</summary>

## Reader/Relay — контрольная точка 22.11.2026

Этот коммит #664 обновляет только общий план и свидетельства. Код опубликован отдельно: Reader [#668](https://github.com/applicate2628/mcp-local-hub/pull/668) на `1b7655ae9a0dec7d6821d8419087fd92ccadbb62`; Relay [#629](https://github.com/applicate2628/mcp-local-hub/pull/629) на `4b6ec39930cb5a0498dba068b99dbf29ba7129b7`. Обратное чтение подтвердило все 13 и 11 публикационных файлов соответственно.

- Reader: передача владения до закрытия, сохранение первичной и дополнительных ошибок закрытия, отклонение частичного результата, точное различение обычного отсутствия. 30 тестовых/подтестовых PASS + PASS пакета с детектором гонок; различающий control FAIL; полные build/vet указанной композиции прошли. Linux проверен компиляцией без исполнения.
- Relay: согласованная версия MCP (Model Context Protocol), принятие сессии только после полного вывода, сохранение серверного retry, ограниченное завершение непринятой сессии и точные JSON ID. 250 тестовых/подтестовых PASS + 5 пакетных PASS, четыре различающих control-группы; полные build/vet указанной композиции прошли. Настоящий потребитель Go SDK получил 32/32 сообщения при GET-SSE reconnect. Возобновление POST-origin потока не реализовано; память и нагрузочная квалификация #626 остаются открытыми.
- P0: привязка прежней квалификации #663 сохраняет FAILED и New=389, Stale=128, Grown=42 (72 workers уже внутри New); нового исполнения или ослабления порогов нет.
- Windows: собственная рабочая копия перенесена непосредственно в trash после публикации. До/после совпали 7 042 файла, 483 каталога и 136 416 620 байт; исходный путь отсутствует. Доступные строки процессов не содержали точного пути; границы наблюдения и полный инвентарь сохранены в квитанции.

Полные стандартные и tagged Go-наборы — NOT_RUN. Внешнее ревью точных опубликованных голов — PENDING. Не было слияния, принудительной перезаписи ветвей, закрытия доноров, удаления ветвей, установок, нативных Scheduler/installed Product/Serena действий или ручного CI. После обратного чтения опубликованного плана шесть собственных локальных каталогов также перенесены в trash. До/после совпали 7 224 файла, 288 каталогов с шестью корнями и 234 260 451 байт; все исходные пути отсутствуют. Все 77 закреплений обязательных результатов повторно проверены вне trash. Два наблюдения видимого /proc не нашли ссылок на переносимые каталоги; границы наблюдения сохранены. [Полная локальная квитанция](https://github.com/applicate2628/mcp-local-hub/blob/58670482426cd60aee35bf09b8d945a9809eb854/.plans/2026-11/reader-relay-local-cleanup-20261122.json). Trash не очищался.

Актуальный [план](https://github.com/applicate2628/mcp-local-hub/blob/58670482426cd60aee35bf09b8d945a9809eb854/.plans/2026-11/open-pr-integration-20261117.md), [квитанция](https://github.com/applicate2628/mcp-local-hub/blob/58670482426cd60aee35bf09b8d945a9809eb854/.plans/2026-11/reader-relay-closeout-20261122.json) и индексированный архив связывают исходники, выполнение, публикацию и уборку. Очередь после публикации кода: 30 открытых PR, все Draft; `master` остаётся `75dbc6406123e22468f168b861d5e0622c9d5769`.

---

<details>
<summary>Ранее опубликованное описание #664 — сохранено без изменения</summary>

## Problem and scope

The useful test-isolation and canonical-plan changes in #658 were based on an older master. Reusing its daemon TestMain after the new private stdio fixture allocated package state before a helper that exits the process, leaving that state behind.

This compact replacement preserves all 28 original #658 payloads at checkpoint `858812988833a84368c60fba1540ae601155fa71`, based on current master `75dbc6406123e22468f168b861d5e0622c9d5769`. It then adds exact early daemon-helper admission and 39 pure dispatch cases. The ordinary TestMain tail and existing original-helper validator are byte-identical. The GUI continuation below also binds two force-kill fixtures to their original process and one Cmd.Wait.

API helper dispatch (#665), CLI caller-root binding (#666) and the perftools stdout fixture (#667) have separate small PRs. Architecture ownership is tracked in #663.

## GUI continuation — 22 November 2026 (Moscow)

Two force-kill tests now retain the original helper Process, join its sole Cmd.Wait, and require the actual kill/hook/lock and active pid-port observations. A no-op kill control preserves cleanup ownership and fails with both expected diagnostics; an early exit code alone cannot satisfy the assertions.

- Exact composition: physical `858812988833a84368c60fba1540ae601155fa71` plus canonical daemon2 and GUI2 overlays; code `8f6a89b491c3dca11830688b8b158bf973b19363`.
- Three targeted Windows race roots: 10 test/subtest PASS + 3 package PASS, zero FAIL/SKIP. Test compilation and full build/vet passed.
- Negative control: Go exit 1, test FAIL + package FAIL, zero PASS/SKIP. Matching its oracle does not relabel those failures.
- Existing PowerShell image, lookup environment, D temporary state and before/after tool/source bytes are pinned. Rewrite reaches its own loopback GUI and a read-only HKCU query. No installed scheduler or Hub mutation.
- [GUI report and archive](https://github.com/applicate2628/mcp-local-hub/blob/d16d60adeb2726169d00b17b8c13324ded76fc67/docs/modernization/verification/gui-original-owner-2026-11-22/README.md); [publication readback](https://github.com/applicate2628/mcp-local-hub/blob/37ccf7703659b8965fce8237820c268bbf5a35a9/docs/modernization/verification/gui-original-owner-2026-11-22/publication-readback.json).

Own full-diff/call-graph review completed. This qualifies the test helper; it does not establish production PID-owner safety, all GUI worker joins, whole-process-tree settlement, POSIX or full Go/product acceptance.

## Evidence and limits

- Daemon dispatch: 39 subtests + root + package PASS, zero FAIL/SKIP, with the race detector.
- Real stdio control left one package root; candidate left none. Invalid calls and a deliberately missing fixture fail before allocation; printed PASS is not accepted as the process result.
- Three explicit full build/vet compositions passed. The report names exact physical source commits and overlays.
- Full default/tagged Go tests remain NOT_RUN. External-tool and Product/Serena prerequisites remain; production PID ownership and complete GUI worker settlement are separate open boundaries.
- The initial formatting refusal and API proof prelaunch refusal are preserved. No whole-process-tree deadline or installed-product qualification is claimed.
- Source preservation, raw command evidence and the full-Go admission audit are in the [report](https://github.com/applicate2628/mcp-local-hub/blob/9ca27f6734ec3587ad0bd155d81d22b1a4960b84/docs/modernization/verification/p0-safe-test-gates-2026-11-21/README.md) and its linked archive.

Own full-diff review and independent focused reviews completed. Required external review on this new head is pending; this PR remains Draft. No merge, force push, installed-state change, manual CI or external quota-consuming review was performed.

## Superseded PR closed after verified preservation

#658 was closed without merge at 2026-11-21T21:20:44Z after all 28 original responsibilities and the published replacement tree were read back. Its original branch still points to `f9966f008aed4d0cfa80d88cc80c12d08e03634f`; the preservation checkpoint remains in this history. [Readback proof](https://github.com/applicate2628/mcp-local-hub/blob/6d83f3a41e86cd13b38f34777ae15238a86a55d9/.plans/2026-11/p0-publication-readback-20261122.json) and [closeout record](https://github.com/applicate2628/mcp-local-hub/blob/9ca27f6734ec3587ad0bd155d81d22b1a4960b84/.plans/2026-11/p0-publication-closeout-20261122.json) retain the evidence. This closure does not imply merge or full acceptance.

## Canonical plan continuation — CMake, 22 November 2026

The plan records the separate #624 terminal-cancellation correction at `5ee707f388dbcdd448d910ab7c2c65b4cdad0424`: negative Go exit1 / 4 PASS / 6 FAIL, candidate 19 test/subtest PASS + package PASS, affected-package build/vet passed. All 21 published package Go blobs match the qualified master75-plus-five-overlay composition. No CMake code is added to this PR. [Current order and remaining consumer work](https://github.com/applicate2628/mcp-local-hub/blob/a908f98a9d55e25cb16f9062608b888280fc47be/.plans/2026-11/open-pr-integration-20261117.md#3-порядок-выполнения) · [Closeout](https://github.com/applicate2628/mcp-local-hub/blob/a908f98a9d55e25cb16f9062608b888280fc47be/.plans/2026-11/cmake-terminal-closeout-20261122.json).

## Canonical continuation — bounded probes, Relay and Scheduler, 22 November 2026

This documentation increment records three independently scoped results in existing Draft PRs; it adds no perftools, Relay or Scheduler production code to this PR.

| PR and published head | Result and qualified checks |
| --- | --- |
| #667 — `1eb934595d3c21954d0bddb934be57a3fe3138cf` | Bounds combined version-probe output to 64 KiB, uses a five-second execution context and one-second pipe wait. The focused Windows race run has 13 test/subtest PASS + package PASS; the finite overflow control fails as expected. Full build/vet passed for the named composition. |
| #629 — `f11cd6cf1f4031f3a2d7db60fb5d4edf19d0a4da` | Bounds finite HTTP responses and each SSE event, preserves error and output ownership, prevents POST redirect replay, and commits only completed event cursors. Recovered evidence retains 204 test/subtest PASS + package PASS and five failing negative controls without rerunning those phases. Newly executed neighbouring tests have 36 test/subtest PASS + package PASS; full tagged build/vet passed. This is the documented 16-overlay composition, not standalone #629 or master. |
| #660 — `0bdb725ed261c532eba07d645c2a14880d12b9fd` | Adds evidence only. Current-master composition preserves both BackendRunHold behaviours and stale-recovery handling: 26 test/subtest PASS + package PASS, two failing roots in the old-whole-file overlay control, full build/vet passed. New tests remain archived overlays because the old PR branch lacks the current-master fixtures. |

- [Perftools report](https://github.com/applicate2628/mcp-local-hub/blob/1eb934595d3c21954d0bddb934be57a3fe3138cf/docs/modernization/verification/perftools-version-probe-2026-11-22/README.md), [Relay report](https://github.com/applicate2628/mcp-local-hub/blob/f11cd6cf1f4031f3a2d7db60fb5d4edf19d0a4da/docs/modernization/verification/relay-response-qualification-20261122/README.md), [Scheduler report](https://github.com/applicate2628/mcp-local-hub/blob/0bdb725ed261c532eba07d645c2a14880d12b9fd/docs/modernization/verification/scheduler-hold-composition-20261122/README.md).
- [Latest canonical plan](https://github.com/applicate2628/mcp-local-hub/blob/b4bb85c2a099916974c55a69490c6644702b6fe0/.plans/2026-11/open-pr-integration-20261117.md#3-порядок-выполнения), [publication and cleanup record](https://github.com/applicate2628/mcp-local-hub/blob/b4bb85c2a099916974c55a69490c6644702b6fe0/.plans/2026-11/continuation-closeout-20261122.json), [final readback and remaining receipt moves](https://github.com/applicate2628/mcp-local-hub/blob/b4bb85c2a099916974c55a69490c6644702b6fe0/.plans/2026-11/continuation-final-readback-20261122.json).
- Published source/evidence bytes were read back at exact heads. Three own Windows roots were moved directly to trash after publication, with 12,977 files, 845 directories and 812,386,316 bytes preserved. Five residual JSON copies were subsequently verified and moved. Initial cleanup stops and transport observations are retained; no deletion attempt or trash emptying occurred.
- Own review and independent focused internal reviews completed. External review of these exact published heads remains PENDING. Full default/tagged Go tests, native Scheduler operations, whole-process-tree settlement and installed-product acceptance remain unqualified.
- The latest queue read contains 29 open Draft PRs. This increment did not merge, close or create a PR, force-push, invoke manual CI, install tools, change installed Scheduler/Hub state, or request a quota-consuming external review.


</details>


</details>


</details>


</details>


</details>


</details>

</details>

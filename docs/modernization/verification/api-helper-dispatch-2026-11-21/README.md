# API: допуск тестовых helper до создания общего состояния

Изменение переносит проверку известных вспомогательных процессов тестов в начало `TestMain`, точки входа тестового бинарника API (программного интерфейса приложения). Это позволяет helper, завершающему процесс через `os.Exit`, выполнить свою узкую роль без создания общего каталога состояния пакета. Для настоящего vault helper сохранённый контроль оставил один такой каталог, кандидат — ни одного; оба завершились кодом 0 с одинаковым протоколом готовности. Эти результаты относятся к конкретной базе и пяти адресным заменам файлов, указанным ниже. [API evidence (2026/11), `native-proof-v2-api-{control,candidate}/receipt.json`](api-evidence.zip)

## Оглавление

1. [Исходники и состав](#исходники-и-состав)
2. [Поведение изменения](#поведение-изменения)
3. [Выполненные проверки](#выполненные-проверки)
4. [Отказ до запуска и исправление proof](#отказ-до-запуска-и-исправление-proof)
5. [Общая композиция и оставшиеся условия](#общая-композиция-и-оставшиеся-условия)
6. [Источники доказательств](#источники-доказательств)

## Исходники и состав

**Таблица 1. Точная идентичность публикационного набора и проверенных входов.** Источники: [code commits (2026/11), кодовая область API](source-inputs.json), [source inputs (2026/11)](source-inputs.json), [API evidence (2026/11), манифесты и итоговые результаты](api-evidence.zip).

| Объект | Значение |
|---|---|
| Commit с изменением кода | `4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee` |
| Его tree | `c5a9a64048cf567cf37770d32d148967a0296198` |
| Родитель и физическая база выполнения | `858812988833a84368c60fba1540ae601155fa71` |
| Tree физической базы | `dba685c090952a93a80a1e82403c4646ad8aaed1` |
| Манифест кандидата, SHA-256 | `6e419ddce2447add5ddaadd7023fcf9052c4237122eb8383bc0cd93bd27cddbc` |
| Манифест controls, SHA-256 | `011a8f8efa3e9b960efb3845573d31ea3669a5897963c51759fe3a7a47697401` |
| Архив `api-evidence.zip` | 614 993 байта; SHA-256 `2a156f8af09d6d0db31353540520e3e0f57cce5765c9103073b12fd04ad61fdb` |

Проверки исполнялись на физическом `source-preserved` с указанными commit/tree. Go overlay — это адресная замена содержимого файлов для сборки — подставлял пять API-файлов; контроль имел пустой `Replace`. Все пять файлов в `candidate/` побайтно совпадают с входами архива и записями `source-inputs.json` и манифеста квалификации. Снимки до и после сохранены в результатах. Это связывает доказательства с конкретными байтами, но не обозначает отдельный запуск опубликованной головы ветки. [API evidence (2026/11), `api-control/overlay.json`, `api-candidate/overlay.json`, `inputs/preserved/manifests/api-staged/manifest.json`](api-evidence.zip); [source inputs (2026/11)](source-inputs.json)

**Таблица 2. Пять публикуемых файлов; все изменения находятся в тестовом коде.** Полные размеры, SHA-256 и Git blob каждого файла находятся в [source inputs (2026/11)](source-inputs.json).

| Файл | Назначение |
|---|---|
| [main_test.go](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/main_test.go) | Ранний dispatch, отказ при неверном вызове, различение возвращающихся и обязанных завершить процесс ролей |
| [main_helper_dispatch_test.go](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/main_helper_dispatch_test.go) | Классификация полного массива аргументов, роли и параметров без файловых или процессных действий |
| [main_helper_dispatch_validation_test.go](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/main_helper_dispatch_validation_test.go) | Табличные проверки разрешённых и недопустимых входов |
| [secrets_test.go](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/secrets_test.go) | Точный anchored selector vault helper: имя теста ограничено `^` и `$` |
| [intent_audit_caller_oracle_test.go](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/intent_audit_caller_oracle_test.go) | Точный selector caller-start-time helper |

## Поведение изменения

Классификатор вызывается до перенаправления окружения и `os.MkdirTemp` общего API fixture. Для активной роли разрешена ровно форма `[exe, -test.run=^ExactTestName$]`; конфликт ролей, неверное значение маркера, неподходящий selector или дополнительный аргумент приводят к коду 3. Обычный запуск без helper-роли проходит к существующей изоляции, аудиту и cleanup пакета. [API source (2026/11), `TestMain` и `classifyAPITestHelperDispatch`](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/main_test.go); [классификатор](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/main_helper_dispatch_test.go)

**Таблица 3. Контракты ролей по коду; выполнение всех ролей нативно не заявляется.** Источник: [API classifier (2026/11)](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/main_helper_dispatch_test.go).

| Роль | Проверяемые входы и сохранённая семантика |
|---|---|
| Caller-start-time oracle | Абсолютный нормализованный state path; неотрицательная задержка с проверкой переполнения при переводе миллисекунд в наносекунды; отдельный helper до `m.Run` |
| Vault flock | Маркер `MCPHUB_TEST_HOLD_VAULT_LOCK=1`, явные vault/ready/release paths; роль должна завершить процесс |
| Upgrade-fence holder | Маркер `1`, явный state path; роль должна завершиться или быть завершена родителем |
| Adopt lease | Только `A-settle-window`, `wait-try`, `wait-try-hold`, `try`, `crash`; проверяются state path и manifest entry; только `crash` обязан не возвращаться |
| Stdio admission | Только `ready`, `legacy`, `hang`; номер порта от 1 до 65 535; роль должна не возвращаться |

Для non-returning роли неожиданное возвращение `m.Run` с нулём превращается в ошибку 3; ненулевой результат сохраняется. Returning adopt-роли сохраняют код `m.Run`. Проверка пути требует абсолютную нормализованную форму без нулевого символа; она не доказывает владение произвольным файловым объектом, его предками или устойчивость к подмене пути. [API source (2026/11), `TestMain`](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/main_test.go); [API classifier (2026/11), `validAPITestHelperPath`](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/main_helper_dispatch_test.go)

## Выполненные проверки

Во всех следующих путях внутри API ZIP опущен общий префикс `evidence/preserved/`. Оракул — сохранённая проверка ожидаемого результата — находится рядом с командой в `*.oracle.json`; исходные события Go находятся в `*.stdout.log`. Счётчики PASS включают события верхнего теста и пакета; они не обозначают столько же независимых тестов. [API evidence (2026/11), `api-candidate/api-dispatch-race.stdout.log` и `.oracle.json`](api-evidence.zip)

**Таблица 4. Форматирование, целевые тесты и компиляция.** Длительность — внешнее время команды из receipt, округлённое до миллисекунд.

| Стадия / команда | Фактический результат | Первичный файл внутри API ZIP |
|---|---|---|
| `api-control` / `api-control` | Компиляция тестового бинарника, exit 0; 34,460 с | `api-control/api-control.oracle.json`, `receipt.json` |
| `api-candidate` / `gofmt` | Exit 0; список файлов, требующих форматирования, пуст | `api-candidate/gofmt.stdout.log`, `gofmt.oracle.json` |
| `api-candidate` / `api-dispatch-race` | Exit 0; 22 PASS = 19 подслучаев + 2 верхних теста + пакет; 0 FAIL, 0 SKIP; 49,886 с | `api-candidate/api-dispatch-race.stdout.log`, `.oracle.json`, `.receipt.json` |
| `api-candidate` / `api-candidate` | Компиляция тестового бинарника, exit 0; 25,581 с | `api-candidate/api-candidate.oracle.json`, `receipt.json` |

Источник Таблицы 4: [API evidence (2026/11)](api-evidence.zip). Целевой запуск использовал `-tags=test_state_path_env -race -json -count=1 -timeout=90s ./internal/api -run ^TestClassifyAPITestHelperDispatch(RejectsUnsafeInputs)?$` и точный overlay. В журнале есть оба верхних теста: `TestClassifyAPITestHelperDispatch` и `TestClassifyAPITestHelperDispatchRejectsUnsafeInputs`. Это проверка классификации входов; она не исполняет все процессы, роли которых представлены таблицей. [API evidence (2026/11), `api-candidate/api-dispatch-race.receipt.json` и `.stdout.log`](api-evidence.zip)

Нативный proof запускал настоящий `TestVaultFlockHelperProcess` с точным selector, собственными путями vault/ready/release и заранее созданным release. Helper вызывает `WithVaultLock`, записывает `ready` и завершает процесс; этот сценарий не открывает vault через API callbacks. Стандартный ввод закрыт через `DEVNULL`; исходный `Popen` удерживает handle непосредственного ребёнка. [API proof (2026/11), `inputs/preserved/prove_api_cli_helpers_v2.py`](api-evidence.zip); [vault helper source (2026/11)](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/secrets_test.go)

Заранее созданный release проверяет cooperative вход, готовность и выход. Конкуренция за блокировку, длительное удержание, deadline и принудительное завершение этого holder не исполнялись данным proof; полные parent-тесты rotate/delete не входят в его selector. [API proof (2026/11), `case_protocol`, `exercise`](api-evidence.zip); [vault helper и parent-тесты (2026/11)](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/secrets_test.go)

**Таблица 5. Фактические нативные случаи API.** `stdout` и `stderr` — стандартный вывод и вывод ошибок; значения указаны в байтах.

| Стадия / case | Exit; stdout / stderr | Готовность и состояние | Результат |
|---|---|---|---|
| `native-proof-v2-api-control` / `cooperative` | 0; 0 / 0 | `ready`; один `mcphub-api-test-state-*` после выхода | `oracle_matched=true` |
| `native-proof-v2-api-candidate` / `cooperative` | 0; 0 / 0 | `ready`; общий API root отсутствует | `oracle_matched=true` |
| `native-proof-v2-api-candidate` / `invalid-marker` | 3; 0 / 49 | Ready отсутствует; TEMP/TMP указывают на собственный обычный файл, блокирующий allocation | `oracle_matched=true` |
| `native-proof-v2-api-candidate` / `extra-argument` | 3; 0 / 49 | Ready отсутствует; тот же тип блокировки allocation | `oracle_matched=true` |

Источник Таблицы 5: [API evidence (2026/11), `native-proof-v2-api-{control,candidate}/receipt.json` и case receipts/streams](api-evidence.zip). Неверный маркер равен `2`; лишний аргумент — `-test.v=true`. Обе отрицательные проверки выполнялись только на кандидате и получили точную диагностику отказа TestMain. Для всех четырёх вызовов записаны `settled=true`, `timed_out=false`, одинаковые SHA-256 бинарника до и после, а `GetTempPath2W` разрешил ожидаемый собственный путь. [API proof (2026/11), итоговые записи и скрипт v2](api-evidence.zip)

Бинарники исключены из ZIP; их идентичность сохранена в compile-oracles и нативных записях: control — 53 060 096 байт, SHA-256 `5573fc5f1ec2668e0652217a16fa1b3187c701b8d38b89cf38de994256c02f13`; candidate — 53 089 792 байта, SHA-256 `1326f26f2b3fbe2ef4a91d41a497f8a223b1e15cb18440e682c9923fd41fce0f`. [API evidence (2026/11), `api-{control,candidate}/*.oracle.json`, `MANIFEST.json`](api-evidence.zip)

## Отказ до запуска и исправление proof

Первый `native-proof-api-control` сохранён как **FAILED**: `completed=false`, `commands=[]`, в cooperative case нет `execution`, `oracle_matched=false`; физический source snapshot до и после одинаков. API helper тогда не запускался. Это утверждение относится именно к helper-командам: служебные Git-наблюдения исполнителя присутствуют отдельно. Причиной было строгое сравнение telemetry mode с LF, тогда как текстовый writer на Windows записал CRLF. LF — перевод строки; CRLF — возврат каретки и перевод строки. [API evidence (2026/11), `native-proof-api-control/receipt.json`, исходные `prove_api_cli_helpers.py` и `p0_preserved_runner_v2.py`](api-evidence.zip)

Отдельный proof v2 разрешает ровно две формы: `off` с LF либо CRLF, сохраняя фактические байты и их SHA-256. В успешных случаях записаны 5 байт, hex `6f66660d0a`, SHA-256 `c281dfd877988953376b6040e2613fcf403e301f3b3203b841564afdae33ad33`. Первый отказ не перезаписан. SHA-256 нового proof — `3c12764470607b92c7c55888af1927a887e5d6df670cffafc8e0350dcb51cb34`; runner v2 остался `b7ed7f7445440a51c7dc56f998d1bb7a11ff1e82c51e4a4d348e3aafa8927641`. [API evidence (2026/11), `native-proof-v2-api-control/receipt.json`, `inputs/preserved/prove_api_cli_helpers_v2.py`](api-evidence.zip)

## Общая композиция и оставшиеся условия

Общая стадия `composed-build-vet` находится во вложенном `recorded-runs/canonical-evidence.zip` комплекта P0. На той же базе она использовала 18 overlay-файлов: 5 API, 9 CLI, 2 daemon и 2 perftools. `go build -overlay … ./...` завершился кодом 0 за 8,267 с, `go vet -overlay … ./...` — кодом 0 за 21,974 с; манифест SHA-256 `d3cea906438a6975f04a1dc93fc5df12a9aa05cca5b38dc9f4a06c3644c1cca9`. Это отдельная проверка сборки и статического анализа общей композиции. [Canonical evidence (2026/11), `evidence/preserved/composed-build-vet/receipt.json`, `inputs/preserved/manifests/composed/manifest.json`](https://github.com/applicate2628/mcp-local-hub/blob/f21728d6f3937187fea517c8ca5916b6f69ac7c5/docs/modernization/verification/p0-safe-test-gates-2026-11-21/p0-gates-evidence.zip)

**Таблица 6. Границы результатов и условия дальнейшей квалификации.** Основание — точные selectors, список стадий и явный флаг ограничения на дерево процессов в [API evidence (2026/11)](api-evidence.zip), а также [API path predicate (2026/11)](https://github.com/applicate2628/mcp-local-hub/blob/4177993228bd70c95d61f0e35fe1d8f5b5f2f1ee/internal/api/main_helper_dispatch_test.go).

| Область | Статус и оставшееся условие |
|---|---|
| Все нативные API-роли | `native_all_roles: NOT_RUN`. Нативно проверен vault helper; caller-time, fence, adopt и stdio lifecycle требуют собственных запусков |
| Полный набор тестов | `full_suite: NOT_RUN`. `go test ./...` не выполнялся в этих доказательствах |
| Полная опубликованная голова | `published_head: NOT_QUALIFIED`. Выполнялась база + overlay; при иной итоговой композиции нужна отдельная квалификация её точных байтов |
| Процессные гарантии | Удерживался исходный direct-child handle; `whole_process_tree_deadline_enforced=false`. Завершение всех потомков этим proof не установлено |
| Владение путями | Parent-owned fixture является предпосылкой. Предки, hard links, конкурентная подмена пути и произвольные входные пути не квалифицированы |
| Product / Scheduler / внешние модели | Установленный продукт, реальный планировщик и внешние сервисы не входят в приведённые selectors и native cases |

## Источники доказательств

1. **[API evidence, 2026/11](api-evidence.zip).** Первичные receipts, потоки, oracle JSON, точные манифесты, кандидатные файлы и Python-исполнители. Внутренний префикс результатов — `evidence/preserved/`; `MANIFEST.json` содержит размер и SHA-256 каждого полезного файла.
2. **[Code commits, 2026/11](source-inputs.json)** и **[API source inputs, 2026/11](source-inputs.json)**. Code-only commit на момент фиксации, parent/tree, пять файлов и их хеши; финальная публикация добавляет отчёт и свидетельства к этому кодовому commit.
3. **[Canonical evidence, 2026/11](https://github.com/applicate2628/mcp-local-hub/blob/f21728d6f3937187fea517c8ca5916b6f69ac7c5/docs/modernization/verification/p0-safe-test-gates-2026-11-21/p0-gates-evidence.zip).** Вложенный `recorded-runs/canonical-evidence.zip`: отдельная общая сборка и vet; SHA-256 `049089bb350e4822833002a117edc1e7912d784f45a992344da2cc3c0cb90075`.
4. **[Local export verification, 2026/11](https://github.com/applicate2628/mcp-local-hub/blob/f21728d6f3937187fea517c8ca5916b6f69ac7c5/docs/modernization/verification/p0-safe-test-gates-2026-11-21/qualification.json).** Успешная проверка полученных архивов; опубликованные здесь счётчики дополнительно сверены с raw Go event logs и case receipts, а не только с этим сводным файлом.

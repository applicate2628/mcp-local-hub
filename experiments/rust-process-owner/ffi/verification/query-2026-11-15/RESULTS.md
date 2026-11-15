# Read-only identity query через Rust / C / Go

**Дата:** 15 ноября 2026 года. **Область:** Windows AMD64, экспериментальные native owner и вызывающая сторона Go.
**Код query:** `e69bc4c14bae2f76ef451bad69a2129345d43c8f`.
**Native-код:** `7f99d4001c43c77c0fab478566e42b8bfb4d12e1`.
**Предыдущая FFI-реализация:** `2e4bf6f983fbf1747ca286016a6eb485a834b852`.
Положительная квалификация записана на merge `4da82d55` с ещё не закоммиченным FFI overlay;
все **45 входных файлов** затем сверены с зафиксированным кодом. Позднейшее слияние
`5187b29b` добавляет только native evidence.

## Оглавление

1. [Реализованный контракт](#contract)
2. [Фактически выполненные проверки](#checks)
3. [Контрольные мутации](#controls)
4. [Ревью и применение навыков](#review)
5. [Общие проверки и граница публикации](#gates)
6. [Артефакты, воспроизведение и ограничения](#evidence)

<a id="contract"></a>
## 1. Реализованный контракт

Добавлены `mh_query_identity1` в [Rust FFI](../../src/identity.rs) и
`Owner.QueryIdentity()` в [Go](../../go/bridge_windows.go).
Они используют тот же retained owner, исходный process и at-create Job.
Сборщик `observe`, копирование `copyIdentity` и проверка report остаются общими
с существующим admission; сценарии допуска и чтения разделены явно.

Новый вызов не расходует admission, не вызывает ResumeThread, stop, finish либо
публикацию нового root exit. Он допускает повторное чтение до/после допуска,
включая denial и Pending без Stop; после попытки Stop либо терминального Finish
получает отказ до чтения. Нативный borrowed view не выдаёт raw handle.

C callback имеет отдельный тип с результатом `void`: он только копирует данные.
Go не принимает пользовательскую policy для query и выдаёт принадлежащие Go копии
только после проверки schema, report, status, единственного receiver, одинакового
PID и согласованности cached exit с wait-state. При любой ошибке результат пуст.

Новый export загружается независимо от `mh_admit_identity1`.
Оба старых варианта DLL продолжают загружаться; отсутствие query возвращает
`ErrIdentityQueryUnsupported` без автоматического перехода к admission.
Размеры ABI1, прежние exports и одноразовая семантика admission сохранены.
Полный C-контракт — [identity_v1.h](../../include/identity_v1.h).

<a id="checks"></a>
## 2. Фактически выполненные проверки

**Таблица 1. Положительная квалификация.**

| Проверка | Фактический результат |
|---|---|
| Native debug, самостоятельный повтор root | 30 unit + 43 integration + 3 compile-fail = **76 passed** |
| FFI `cargo test --offline --locked`, debug | **20 passed**, 0 failed |
| FFI та же команда с `--release` | **20 passed**, 0 failed |
| Go `-race`, debug DLL | **57 верхнеуровневых + 72 вложенных**, 0 skips/failures |
| Go `GOEXPERIMENT=cgocheck2`, debug DLL | **57 + 72**, 0 skips/failures |
| Go `-race`, release DLL | **57 + 72**, 0 skips/failures |
| Go `cgocheck2`, release DLL | **57 + 72**, 0 skips/failures |
| Старый Go из `2e4bf6f9` с новой DLL | **54 + 68**, 0 skips/failures |
| Новый Go со старой ABI1 DLL | **1 passed**: unsupported query сохраняет явный AdmitChecked |
| Новый Go с admission-only identity DLL | **1 passed**: unsupported query сохраняет явный AdmitIdentity |
| Native/FFI fmt, Clippy с `-D warnings`; Go formatting/vet | Exit 0 |

Сравнение PID/времени рождения и SID использует независимое сообщение реального child.
Копии переживают закрытие владельца и выгрузку DLL.
Отсутствие resume проверяется также успешным последующим admission с проверкой исходного
suspend count; один отсутствующий marker сам по себе недостаточен.
Конкурентный сценарий исполнялся с детектором гонок; он не является самостоятельным
доказательством отсутствия overlap. Удержание mutex проверено чтением пути вызова.

<a id="controls"></a>
## 3. Контрольные мутации

**13 отдельных запусков тестов, 28 команд: 15 сборок и 13 исполнений.**
Нормальная копия прошла baseline. Каждый из десяти producer controls меняет только
новый query export, сохраняя общий сборщик и admission. Во всех десяти случаях
проверены отказ, пустой результат и последующий настоящий явный AdmitIdentity того же owner.

**Таблица 2. Различающие контрольные случаи.**

| Контроль | Ожидаемый и полученный результат |
|---|---|
| Поздняя порча report | Отказ после уже выполненного копирования |
| Пропущенный receiver | Отказ |
| Повторный receiver | Первая копия не выдаётся |
| Неверная schema наблюдения | Отказ до выдачи результата |
| Query error 5 и token-close error 6 | Сохранены оба поля и обе error causes |
| Только token-close error 6 | Ошибка query не выдумана |
| Известный root exit вместе с NotSignaled | Отказ из-за противоречия |
| Различный PID | Отказ |
| Settlement от read-only query | Отказ |
| ACTIVE_KNOWN от read-only query | Отказ |
| Скрытый admission внутри query | **Ожидаемый FAIL**, оба под-теста: повторное чтение выявляет расходованный admission |
| Удалена проверка known-root / wait-state | **Ожидаемый FAIL**: испорченное наблюдение стало доступно вызывающему |

Два token-контроля — синтетическая порча отчёта **после реального успешного
query и закрытия token**. Они доказывают перенос/отклонение ошибок через границу,
а не возникновение соответствующих Windows failures.

Root лично повторил строгий и ослабленный consumer на одной и той же damaged DLL:
exit 0 и ожидаемый exit 1 с диагностикой `failed query exposed an observation`.
Первичный отдельный интерпретатор этих двух логов ошибся на PowerShell scalar
`.Count`; исходные logs/receipt сохранены, исправленная отдельная интерпретация
обернула выборку в массив. Исполнения не переименованы и не повторялись ради счётчика.

<a id="review"></a>
## 4. Ревью и применение навыков

**Ponytail Code Simplification:** исключено преждевременное обобщение продуктового Go
lifecycle. Следующий private contract будет реализован вместе с настоящим адаптером.
Повторно использованы существующие сборщик и валидаторы; новые owners, реестры,
зависимости и общий режим query/admission не добавлены.

**Architecture Review + API and Interface Design:** разведены наблюдение и полномочие
запуска; зафиксированы lifecycle states, borrowed lifetime, независимые optional exports,
void receiver, раздельные query/close failures и границы продуктовой capability.

**Incremental Implementation:** квалифицирован самостоятельный полезный native/FFI срез
в существующих зонах #644/#631. Продуктовый Go runtime не менялся.

Root прочитал весь native и FFI runtime diff и выполнил свои проверки.
Независимое architecture review нашло один P2: полный ответ принимался при cached
root exit и NotSignaled того же процесса. Исправление проверяет только нужную импликацию
`ROOT_KNOWN => IdentitySignaled`; обратная не вводилась.
Повторное независимое чтение этой дельты подтвердило закрытие замечания.
Дополнительно убран ненужный публичный panic sentinel и сделан строгим выбор legacy mode.
Подтверждённых открытых замечаний по этому срезу не осталось; formal bot PASS не заявляется.

<a id="gates"></a>
## 5. Общие проверки и граница публикации

**Таблица 3. Повтор CLAUDE.md pre-push gates на неизменённой продуктовой основе `17bc78fc`.**

| Команда | Exit / результат |
|---|---|
| `go build ./...` | 0 |
| `go vet ./...` | 0 |
| `go test -count=1 -timeout 5m ./...` | **1**; 8 922 top-level pass, 67 fail, 131 skip; вложенные 6 215/17/2 |
| `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/` | **1**; 4 464 top-level pass, 66 fail, 65 skip; вложенные 2 948/16/1 |

Порядок чисел вложенных тестов: pass/fail/skip. В каждом широком запуске три test cases
остались без завершающего события при тайм-ауте API/CLI 300 секунд.
Full suite повторяет тот же список failed test names, что предыдущая записанная
квалификация; в tagged suite новых failed names нет, один прежний failure отсутствует.
Это не доказательство исправления этих тестов нашим query.

Ошибки включают существующие process Job/breakaway и transport preparation проверки,
а также API/CLI. Нового broad run на master `a7d92d28` не выполнялось.
Продуктовые `internal`, `cmd`, `go.mod`, `go.sum` сверены между началом и концом;
Rust/FFI находится в отдельном вложенном модуле.

Общие pre-push условия остаются незакрытыми. Предыдущее исключение для draft-публикации
подготовленного identity admission не трактуется как автоматическое разрешение нового
push. Код и доказательства подготовлены для тех же #644/#631; merge и bot/CI dispatch
не выполнялись.

<a id="evidence"></a>
## 6. Артефакты, воспроизведение и ограничения

[logs.zip](logs.zip) содержит **123 записи**: реальные команды, outcomes, hashes,
журналы qualification/gates, runners, точные producer/consumer mutations и исправленную
интерпретацию root controls. [manifest.json](manifest.json) связывает original/published
hashes и проверяет каждую извлечённую запись; [SHA256SUMS](SHA256SUMS) проверяет пакет.
Исполняемые файлы, caches и private binary SID fixtures исключены. Текстовые SID
и имя Windows-профиля скрыты; оригинальные файлы исполнения не переписывались.

**Таблица 4. Ключевые SHA-256.**

| Артефакт | SHA-256 |
|---|---|
| Новая debug DLL | `f68b247c6c7f4cd38bad578bc54ae48ebecc25dd8c1106d576b42d4420add3f1` |
| Новая release DLL | `06bfae6e22faa728e962e5baa3d2ce8dc9995bb7467a50fed738afef1ba3135a` |
| Same damaged DLL для строгого и ослабленного consumer | `9c56e80699bec105b43cf8022f009456856de1fdb05279b3e663e9bdc054c2fe` |
| Архив журналов | `9f9370562ded4048fa3332f92846a4ba1bc20431d7f83de9eaeab50537bd97ee` |

Runner `runners/qualify-query.ps1` требует явные owned paths и отказывает при уже
существующих выходах qualification/compatibility. Он использует установленные инструменты,
offline caches и проверяет hashes двух старых DLL. Для полного воспроизведения на другом
хосте эти старые DLL надо получить/собрать из зафиксированных baseline исходников;
битовая идентичность сборки на другом toolchain не предполагается.
`controls/run-query-controls.ps1` берёт runtime source текущего проверяемого checkout
и записывает его фактические hashes; новый Run сохраняет отдельную историю.

Полный native release suite не повторялся; не проверены другие ОС/архитектуры, установленный
продукт и включение Rust backend. Query не создаёт CreatorIdentity, SID-formatting receipt,
context-aware cancellation, custody seal, retirement или product root/member outcomes.
Синхронные системные чтения не обещают deadline. Успешное позднее чтение не устраняет
неопределённость прежнего token-close failure.

**Термины:** FFI — интерфейс между языками; ABI — двоичный интерфейс; DLL — динамическая
библиотека Windows; SID — идентификатор безопасности; PID — идентификатор процесса;
Job — объект Windows для группы процессов; receiver — синхронная функция копирования;
custody — удержание ресурсов и полномочий завершения; receipt — квитанция реально
выполненных операций; retirement — запрет новых операций и завершение уже вошедших.

# C: исходники Rust reader #627 и граница Go-адаптера

**Итог узкого чтения:** полный исходник #627 получен и проверен по Git blob. Нового доказанного дефекта обработки ошибок в просмотренном алгоритме не найдено; остаются конкретные непроверенные ветви `stat`/`close`/совмещённых ошибок. Настоящего Go-адаптера в этом PR нет. Реализация адаптера сейчас потребует явного решения несовпадающих контрактов, а не простой подмены вызова.

Здесь `stat` — чтение метаданных через уже открытый дескриптор; `close` — попытка его закрытия; совмещённая ошибка — первоначальный отказ вместе с одним или несколькими отказами закрытия. PR означает запрос на включение изменений. Ни Rust, ни Go, ни проектные тесты, ни установщики в этом участке не запускались; зависимости не устанавливались и не загружались. Код не изменялся. Сохранённые в архиве исходники — точные прочитанные копии для ревью.

**Файлы публикационной копии.** [source-evidence.zip](source-evidence.zip) содержит все 96 файлов исходного каталога без изменения байтов, включая исторические `README.md` и `A217-SOURCE-ACQUIRED.md`. В архив добавлен `PACKAGE_FILES.json` — перечень исходных файлов с размерами и контрольными суммами SHA-256; всего в ZIP 97 файлов. Размер, контрольная сумма и сведения о проверке архива приведены в [archive.json](archive.json). Ссылки с пометкой «путь в архиве» ведут к ZIP; после распаковки используйте указанный путь. Текст двух внешних документов отличается только оформлением публикации и навигацией.

## Содержание

1. [Точные источники](#1-точные-источники)
2. [Владелец и передача ошибок](#2-владелец-и-передача-ошибок)
3. [Имеющиеся проверки и пробелы](#3-имеющиеся-проверки-и-пробелы)
4. [Настоящий Go-потребитель и отсутствующий адаптер](#4-настоящий-go-потребитель-и-отсутствующий-адаптер)
5. [Минимальный следующий самостоятельный срез](#5-минимальный-следующий-самостоятельный-срез)
6. [Отдельный источник a217c779](#6-отдельный-источник-a217c779)
7. [Что подтверждено и что не выполнялось](#7-что-подтверждено-и-что-не-выполнялось)

## 1. Точные источники

Свежий GitHub connector и повторное чтение 22 ноября 2026 года, 21:00:04 UTC, дали #627 **open, Draft**, head `6615d239fe066eaa5fa78c22a5272d6602528274`, head branch `spike/rx04-anchored-read-20261112`, base branch `master`. Поле PR `base_sha=17bc78fc602efa204a74596409c939a3bf1ddc42` историческое. Отдельный `fetch_commit(master)` вернул действительный master `75dbc6406123e22468f168b861d5e0622c9d5769`.

Получены полный recursive tree с `truncated=false`, точные 10 изменённых файлов PR и весь семифайловый `experiments/rust-anchored-read`. Для каждого полученного файла Base64 декодирован в байты и заново вычислен Git blob SHA-1, совпавший с GitHub. SHA-256 и опубликованные ссылки сохранены в [source-pins.json](source-evidence.zip) (путь в архиве: `source-pins.json`); полные исходные ответы — в [connector-snapshot.json](source-evidence.zip) (путь в архиве: `connector-snapshot.json`).

**Таблица 1. Основные исходники #627.**

| Путь относительно репозитория | Байты | SHA-256 |
|---|---:|---|
| `experiments/rust-anchored-read/src/lib.rs` | 1857 | `7ab79fa5526e760c04f7326747f3fb09a1b713ce2b13a03925909f780b3a6672` |
| `experiments/rust-anchored-read/src/windows.rs` | 12134 | `bbb0e8b57b84b1e5a5a6cc8941e19fb1a5b2bffda42cffc8628953c29a8d45ff` |
| `experiments/rust-anchored-read/src/read_tests.rs` | 14670 | `f77765527f80c93677d2ec3cb6991a2fa64c446b50c9c4f563136443603aa6b4` |
| `experiments/rust-anchored-read/Cargo.toml` | 392 | `37b8d9e94146f8a889838b5e06bd6a9474549660331ccc475ebaabcccb85302c` |
| `experiments/rust-anchored-read/Cargo.lock` | 3056 | `6cd7b98899c6afe742b1ca177e947b360e78b2662c4b6b7778eb1e32e619e026` |

Ссылки на точные прочитанные копии: [lib.rs](source-evidence.zip) (путь в архиве: `source-6615d239/experiments/rust-anchored-read/src/lib.rs`), [windows.rs](source-evidence.zip) (путь в архиве: `source-6615d239/experiments/rust-anchored-read/src/windows.rs`), [read_tests.rs](source-evidence.zip) (путь в архиве: `source-6615d239/experiments/rust-anchored-read/src/read_tests.rs`), [README](source-evidence.zip) (путь в архиве: `source-6615d239/experiments/rust-anchored-read/README.md`). Все три Rust-файла прочитаны целиком.

Для сравнения интерфейса отдельно получены пять полных файлов Go из свежей головы #668 `1b7655ae9a0dec7d6821d8419087fd92ccadbb62`, open Draft. Это только чтение текущего контракта: результаты тестов #668 здесь не используются и не распространяются на Rust. Идентичности — [context-source-pins.json](source-evidence.zip) (путь в архиве: `context-source-pins.json`), ответы — [context-snapshot.json](source-evidence.zip) (путь в архиве: `context-snapshot.json`).

## 2. Владелец и передача ошибок

Публичная точка — `lib.rs:47`, `read_verified(root, components, cap, expected, canceled)`. Она принимает обязательный SHA-256 размером 32 байта, положительный предел не более 1 МиБ и атомарный флаг отмены; на Windows вызывает `windows::read_with_hook`, на других платформах возвращает `Unsupported`. Один МиБ равен 1 048 576 байтам.

`windows.rs:45`, `read_with_hook`, владеет `Vec<OwnedHandle>` и итоговой выдачей байтов. `read_inner` сначала удерживает root, затем следующий компонент до освобождения прежнего родителя. Одновременно удерживается не более двух дескрипторов. После завершения внутреннего чтения внешняя функция опустошает список с конца и собирает ошибки закрытия.

**Таблица 2. Точные ветви, которые должен различать следующий срез.**

| Место | Условие | Реализованное следствие |
|---|---|---|
| `windows.rs:93–94` | `inspect(..., "stat-root")` не проходит после root acquisition | Root уже в списке владельца; первоначальная ошибка возвращается через общий cleanup |
| `windows.rs:113–119` | `stat-component` не проходит после открытия следующего каталога | Старый и новый дескрипторы сохранены; общий cleanup пытается закрыть оба |
| Там же | Прежний parent не закрывается после успешного `stat-component` | `close-parent` становится первоначальной ошибкой; новый parent остаётся у владельца и проходит cleanup |
| `windows.rs:123` | Ошибка `stat-final` | Сохранены final и его parent; общий cleanup проходит оба |
| `windows.rs:143–167` | Ошибка настоящего `ReadFile` после уже прочитанных байтов | Частичный `Zeroizing<Vec<u8>>` не выдаётся; сохраняется `read-final` и Win32-код |
| `windows.rs:64–68` | Есть первоначальная ошибка и хотя бы один отказ cleanup | Первоначальные category/operation/native сохраняются; к ним добавляется `cleanup_failures` |
| `windows.rs:69–77` | Чтение и SHA-256 успешны, но close не подтверждён | Байты уничтожаются через `drop`; возвращается ошибка `close`, первый native-код и весь список cleanup-кодов |
| `windows.rs:333–343` | `CloseHandle` возвращает отказ | Владение `OwnedHandle` потреблено через `into_raw_handle`; численное значение повторно не закрывается |

`inspect` вызывает именно `GetFileInformationByHandle` и отдельно отвергает reparse point или неверный тип объекта (`windows.rs:309–330`). `close_once` вызывает именно `CloseHandle`; ошибку получает немедленно из `last_os_error`. Код поддерживает отделение первоначальной ошибки от cleanup, но сам по себе этот разбор не доказывает нативные отказы или отсутствие оставшегося открытым ресурса при отказавшем close.

Существующий тестовый интерфейс `Step` содержит только `BeforeOpen(usize)` и `BeforeRead { requested }` (`windows.rs:35–44`). У него нет точки получения текущего native handle и нет вызова перед `inspect` или `close_once`. Поэтому возврат произвольного `ReadError` из такого hook не упражняет ветвь отказа соответствующего системного вызова.

## 3. Имеющиеся проверки и пробелы

В текущем `read_tests.rs` ровно **16 определений `#[test]`**. Полные имена начинаются с `windows::tests::`; таблица 3 показывает оставшуюся часть имени.

**Таблица 3. Имеющиеся корни тестов.**

| Тест | Строка | Что различает |
|---|---:|---|
| `reads_exact_verified_bytes_empty_and_unicode` | 48 | Пустое содержимое, вложенный путь и Unicode |
| `rejects_invalid_input_before_open` | 61 | Отказ входных имён и cap до открытия |
| `canceled_before_open_has_typed_result` | 84 | Отмена до root open |
| `checksum_mismatch_and_initial_size_limit_return_no_data` | 99 | Несовпадение digest и исходный размер выше cap |
| `final_directory_and_missing_file_preserve_native_failure` | 106 | Отказ final open, сохранение NT-кода и операции; это не отказ stat |
| `root_and_intermediate_and_final_reparse_are_rejected` | 116 | Отказ для ссылок на трёх уровнях |
| `component_swapped_to_link_before_open_is_rejected` | 144 | Подмена ещё не открытого компонента ссылкой |
| `retained_parent_does_not_reopen_changed_path` | 169 | Удерживаемый parent после переименования |
| `retained_final_handle_reads_original_after_path_replacement` | 195 | Удерживаемый final после замены имени |
| `growth_after_stat_hits_one_byte_sentinel_without_returning_data` | 219 | Рост после успешного stat и запрос одного контрольного байта |
| `mid_read_cancellation_returns_no_partial_result` | 252 | Отмена перед вторым чтением |
| `injected_native_phase_error_preserves_category_and_closes_handles` | 278 | Возврат `Win32(123)` из первого component hook; системный stat/read/close при этом не вынуждается отказать |
| `native_read_failure_after_partial_read_preserves_error_and_releases_lock` | 301 | Настоящий отказ второго `ReadFile` на независимой блокировке диапазона файла |
| `repeated_success_and_failure_do_not_leak_handles` | 390 | Стабильное число process handles в повторениях успешного чтения, checksum mismatch, missing и cap failure |
| `root_ancestor_reparse_is_not_silently_followed` | 421 | Ссылка в предке root |
| `exact_cap_still_checks_eof_and_returns_complete_payload` | 438 | Чтение ровно cap и дополнительная проверка конца файла |

В native read fixture второй запрос равен двум байтам после уже прочитанных 4096 из файла длиной 4097; независимый handle блокирует один байт на смещении 4096. Проверяются код 33 (`ERROR_LOCK_VIOLATION`), отсутствие cleanup-ошибок, явное снятие блокировки и последующее полное чтение. Это конкретный системный отказ чтения, не модель отказа stat/close.

Опубликованный [evidence.json](source-evidence.zip) (путь в архиве: `source-6615d239/docs/modernization/verification/reader-native-read-2026-11-21/evidence.json`) содержит исторический stdout попытки v2: 16 passed, 0 failed/ignored/measured/filtered out. Его pin тестового файла `d16433d1db44f873e570d4cd65e2ac6f749ebc55` / SHA-256 `f7776552…` совпал с полученным текущим файлом. Это чтение сохранённой записи, не свежий повтор HEAD `6615d239…`; сама запись отдельно указывает `runtime_authentication=NOT_ESTABLISHED`.

**Открытые проверки:** настоящий отказ `GetFileInformationByHandle` после приобретения root/intermediate/final; отказ parent close при продвижении; отказ final close и parent close после успешного чтения; первоначальная stat/read/cancel/checksum ошибка вместе с одним или двумя отказами cleanup; одна попытка закрытия каждого приобретения; отсутствие байтов результата при любом неподтверждённом закрытии. Нынешний repeated-handle-count тест не индуцирует отказ close и не покрывает его исход.

## 4. Настоящий Go-потребитель и отсутствующий адаптер

Весь #627 состоит из семи файлов Rust-эксперимента и трёх документов/свидетельств. Изменений Go в его точном списке нет. `Cargo.toml` не задаёт C-совместимую библиотеку; в полном Rust-исходнике нет `extern "C"`, экспортов FFI, заголовка или Go binding. FFI означает интерфейс вызова между языками. `read_verified` принимает Rust `Path`, `OsString`, `AtomicBool` и возвращает Rust `Result<Zeroizing<Vec<u8>>, ReadError>`. Наличие Rust функции не является готовой точкой вызова из Go.

**Таблица 4. Сравнение с конкретными текущими Go-владельцами #668.**

| Контракт | Прочитанный Go-код | Что отсутствует у #627 |
|---|---|---|
| Обычный проверенный state read | `state_read_beneath_root_windows.go:22`, `ReadStateFileBeneathRootNoFollow`; 1 МиБ и обязательный digest | Нужен настоящий способ вызова и владение результатом/ошибкой на границе языков |
| Backup capture | Там же, строки 30–33; `ReadClientConfigBackupBeneathRootNoFollow`, 16 МиБ и разрешённый пустой digest при захвате | Rust ограничен 1 МиБ и обязательным `[u8;32]`; пустой digest не выражается |
| Отмена | Go сохраняет `ctx.Err()` в `StateFileReadError.Cause`, например строки 78–79 и 147–149 | Rust хранит только `Canceled`; distinction между `context.Canceled` и `context.DeadlineExceeded` не переносится автоматически |
| Причины и компонент | `state_read_beneath_root_common.go:28–43`: category, operation, component, cause/Unwrap; строки 81–92 сохраняют несколько причин через `errors.Join` | Rust не сохраняет component; cleanup-вектор хранит только NT/Win32-коды, без собственной operation/component у каждой причины |
| Чистое отсутствие | `stateReadBeneathRootAbsent`, common строки 64–78, принимает только прямой одиночный open-missing результат; joined error не принимается | Требуется отдельное точное отображение отсутствия, исключающее `cleanup_failures`; одной категории `UnsafeObjectOrIo` недостаточно |
| Компенсация backend | `managed_backend_install.go:65–95`, `prepareOwnedBackendCompensation`, читает `.inverse.json` с пустым digest и создаёт запись лишь при чистом отсутствии | Наивная подмена нарушит backup policy или разрешит неправильное создание после ошибки cleanup |
| Закрепление backup | `install_reconcile_mcp_front.go:3456–3492`, `(*mcpFrontReconcileJournal).pinBackup`, читает с пустым digest, вычисляет SHA-256 и только затем пишет pin | Требуется сохранить однократное чтение исходных байтов, cap, отмену и отказ до записи pin |

Прочитанные копии: [Go common](source-evidence.zip) (путь в архиве: `go-source-1b7655ae/internal/api/state_read_beneath_root_common.go`), [Go Windows reader](source-evidence.zip) (путь в архиве: `go-source-1b7655ae/internal/api/state_read_beneath_root_windows.go`), [caps](source-evidence.zip) (путь в архиве: `go-source-1b7655ae/internal/api/state_read_caps.go`), [managed backend](source-evidence.zip) (путь в архиве: `go-source-1b7655ae/internal/api/managed_backend_install.go`), [CLI pinning](source-evidence.zip) (путь в архиве: `go-source-1b7655ae/internal/cli/install_reconcile_mcp_front.go`).

README #627 честно закрепляет сравнение с прежним Go `a7d92d28…`; перед интеграцией требуется учитывать показанную более новую границу чистого отсутствия #668. Это не основание перенести его тестовый результат на Rust. Для Rust по-прежнему не получены измеримая выгода у реального потребителя или отдельное продуктовое требование, оправдывающее дополнительную границу языков.

## 5. Минимальный следующий самостоятельный срез

**Первым предлагается проверить завершение чтения у существующего Rust owner, без Go-адаптера и без расширения product scope.** Область: `windows.rs` и `read_tests.rs`, при необходимости один narrowly scoped тестовый файл этого же crate. Публичный `read_verified`, зависимости и потребители пока не меняются.

Начать с небольшого per-invocation механизма наблюдения/подстановки ровно `inspect` и `close_once`, если естественной детерминированной fixture недостаточно. Сохранить реальные native операции в обычном вызове, передачу `OwnedHandle` до закрытия и одну попытку на приобретение; не вводить процессный global hook или общий новый backend framework. Затем одна таблица различающих случаев должна проверить переходы таблицы 2: одиночные stat/parent-close/final-close и первичный отказ вместе с cleanup, с точными native-кодами, сохранением первоначального error и отсутствием байтов результата.

Такой детерминированный слой подтвердит только передачу исходов и владение в алгоритме. **Подставной возврат кода не закрывает нативную квалификацию `GetFileInformationByHandle`/`CloseHandle`.** Реальная native fixture требует отдельно доказать, что она индуцирует отказ именно вызванной операции, сохраняет владение всеми своими ресурсами и не закрывает чужой/повторно использованный численный handle. Нельзя создавать кажущийся отказ путём безусловного закрытия дескриптора, которым всё ещё владеет `OwnedHandle` reader.

После собственного ревью этих изменений — затронутые тесты на закреплённом доступном Windows toolchain и имеющихся зависимостях, с сохранённым отрицательным контролем там, где найден действительный дефект. В этом чтении команда запуска не выполнялась и новые доказательства native failures не созданы.

Go-адаптер — отдельный следующий срез только после выбора реального потребителя и смысла такой интеграции. Для минимального пилота обычный state reader ближе к существующему Rust-контракту, чем backup consumer; это инженерный вывод из таблицы 4, не реализованный выбор. Нельзя тихо уменьшить backup cap, потребовать отсутствующий digest, терять context/cause либо свести совмещённый отказ к отсутствию.

## 6. Отдельный источник a217c779

**Последующее получение завершено:** [A217-SOURCE-ACQUIRED.md](A217-SOURCE-ACQUIRED.md) фиксирует фактический immutable commit, его дерево и все 64 полных native source файла с проверенными Git blob/SHA-256. Описанная ниже первоначальная недоступность через GitHub сохраняется как история поиска; общий source blocker для этого закреплённого компонента снят. Более новая живая ветка и её чистота не подменяют выбранный коммит.

Полный идентификатор найден в опубликованных R25 [AUDIT](source-evidence.zip) (путь в архиве: `r25-audit.md`) и [HANDOFF](source-evidence.zip) (путь в архиве: `a217-r25-handoff.md`): `a217c7799a0214967bf86211542e508162c680b5`. Последнее указанное там наблюдение — Wave, `D:\dev\ChatGPT\mcp-rust-contained-stream-20261120`, ветка `refs/heads/review/rust-contained-stream-20261120`, чистый porcelain 21 ноября 2026 года в 12:15:30 UTC. Это историческое наблюдение, не нынешняя проверка clean.

Свежие `fetch_commit` сокращённого и полного SHA в `applicate2628/mcp-local-hub` оба вернули GitHub 422 `No commit found`. Это не доказывает, что локальной параллельной работы нет. Исходный canonical остаток надо читать как недоступность её полных байтов через использованный GitHub путь.

По следующему явному поручению root проверена доступность через Remote Desktop Commander: Wave сейчас Online; `get_file_info` указанного D-каталога успешен, `isDirectory=true`. Исходные ответы сохранены в [wave-availability.json](source-evidence.zip) (путь в архиве: `wave-availability.json`). Одного такого ответа недостаточно для HEAD, чистоты или полноты исходников. Root уже поручил следующий отдельный read-only сбор текущей commit identity и необходимых файлов; он не означает повтор установки v2. Исторические `approved=false` и `automatic_repeat_authorized=false` из handoff относятся к той установочной попытке и не препятствуют обычному чтению исходников.

## 7. Что подтверждено и что не выполнялось

Подтверждены свежая и повторно прочитанная голова #627, полный состав его Rust-crate, точные байты и функции, наличие 16 текущих тестовых корней, конкретные непокрытые ветви, отсутствие адаптера в этом PR и исходные требования действующих Go-потребителей. Сохранены первичные ответы, копии исходников и контрольные суммы; самостоятельное ревью выполнено по прочитанному коду.

Не выполнялись сборки, Rust/Go тесты, проектные runner, установки, загрузки зависимостей, публикации, комментарии или закрытия PR. Никакой PASS #668 не перенесён на #627. Первоначальная проверка доступности Wave не объявлена проверкой полных исходников `a217…`; завершённое последующее получение имеет собственную точную привязку в указанном дополнении.

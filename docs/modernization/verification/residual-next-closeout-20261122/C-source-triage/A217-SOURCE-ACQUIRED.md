# A217: фактический источник получен с Wave

**Прежний блокер недоступности исходников снят для полного закреплённого native-компонента:** получены все 64 файла Rust/Cargo/C-заголовков/Go-моста и его тестов из неизменяемого коммита `a217c7799a0214967bf86211542e508162c680b5`, всего **649751 байт**. Каждый Git blob и SHA-256 проверен независимо после получения. Это получение данных для следующего ревью, без новой квалификации native поведения или Go/Rust.

[Основной отчёт](README.md) · [Архив исходных файлов](source-evidence.zip) · [Проверка архива](archive.json)

Ссылки с пометкой «путь в архиве» ведут к `source-evidence.zip`. Исторический текст этого документа сохранён там без изменений как `A217-SOURCE-ACQUIRED.md`; внешний документ дополнен только оформлением публикации и навигацией.

## Содержание

1. [Идентичность и граница источника](#идентичность-и-граница-источника)
2. [Полнота полученного среза](#полнота-полученного-среза)
3. [Что действительно исполнялось](#что-действительно-исполнялось)
4. [Следующий остаток](#следующий-остаток)

## Идентичность и граница источника

**Таблица 1. Фактические идентификаторы.**

| Объект | Значение |
|---|---|
| Устройство и рабочая копия | Wave, `D:\dev\ChatGPT\mcp-rust-contained-stream-20261120` |
| Указатель worktree `.git` | `D:/dev/ChatGPT/mcp-product-rust-custody-retry-20261119/.git/worktrees/mcp-rust-contained-stream-20261120` |
| Символический HEAD | `refs/heads/review/rust-contained-stream-20261120` |
| Прочитанное значение живой ветки | `31de3217b9b25492a23c288160936a6e571bc4ba` |
| Неизменяемый коммит для получения исходников | `a217c7799a0214967bf86211542e508162c680b5` |
| Его дерево | `ca118ff1ec513cc830bb8b96b4d6782d23915a26` |
| Его единственный родитель | `057a42bee37a570762334dfe5cdcb35bdb327ea3` |
| SHA-256 фактически прочитанного Git EXE до/после | `177bc8a9af84fa3a400c694c975fda731d048279c074e1a93947ce8b067ebb48` |

Сначала filesystem API Remote Desktop Commander подтвердил устройство и каталог, затем прочитаны `.git`, worktree `HEAD`, `commondir` и branch ref. Прямой read-only `git cat-file -p` вернул raw commit; пересчёт формата объекта Git локально дал точно `a217c779…`. Весь последующий экспорт использовал `commit:path` с этим полным SHA. Более новая живая ветка не использована как источник файлов. **Чистота worktree не установлена и не заявлена.**

Свежие GitHub-запросы полного и сокращённого SHA до этого возвращали 422. После успешного локального получения с Wave некорректно оставлять общий остаток «a217 source недоступен». Корректная граница: GitHub не отдаёт этот объект через проверенный путь; указанный native source теперь сохранён и доступен для чтения, остальная ветка не объявлена импортированной.

## Полнота полученного среза

Список взят из фактически прочитанного `W\native_source_files_a217_v1\comparison.json`, где `W` — `D:\dev\ChatGPT\mcp-rust-contained-stream-work-20261120`. Он сохранён как [a217-pinned-comparison.json](source-evidence.zip) (путь в архиве: `a217-pinned-comparison.json`).

Одним `git ls-tree -r -z --long` прочитан полный перечень `experiments/rust-process-owner` на фиксированном коммите. В нём **115 листьев**. Отдельным `git cat-file --batch` получены raw commit и все 64 выбранных blob. Набор из 64 точно совпал со всеми `.rs`, `.go`, `.h`, `.toml`, `.lock`, `go.mod`/`go.sum` этого дерева; остальные 51 путь — документация, старые журналы, проверочные PowerShell-скрипты и архивы свидетельств. Они перечислены в квитанции, но их содержимое не объявлено полученным или переисполненным.

Для каждого приобретённого файла проверены четыре независимые связи: запрошенный `commit:path`; mode/type/size/blob из `ls-tree`; заново вычисленный Git blob SHA-1 из полученных сырых байтов; совпадение SHA-256 с историческим 64-файловым inventory. Все проверки прошли. Конкатенированный batch полностью разобран по его byte lengths, лишних или недостающих байтов нет.

**Таблица 2. Основные полученные входы для следующего ревью.**

| Область | Точные пути внутри `experiments/rust-process-owner` |
|---|---|
| Rust владелец | `src/lib.rs`, `src/windows.rs`, `src/launch_context.rs`, `src/job_members.rs`, `src/token_user.rs` |
| Rust C ABI | `ffi/src/lib.rs`, `identity.rs`, `members.rs`, `process_borrow.rs`; три заголовка `ffi/include/*_v1.h` |
| Реальный Go-мост этой отдельной process-линии | `ffi/go/bridge_windows.go`, `admission_windows.go`, `finish_context_windows.go`, `wait_windows.go`, `stdio_windows.go`, `outcome_windows.go`, `identity_windows.go`, `members_windows.go`, `environment_windows.go` |
| Исходники проверок и зависимостей | Полные Rust и Go test files, Cargo manifests/locks и `ffi/go/go.mod` из того же закреплённого набора |

Например, в полученном Go-мосте доступны пакетная функция `Open`, методы `(*Library).New/NewWithEnvironment` и `(*Owner).WaitForExit/CloseExited/Release/FinishContext`; Rust `ffi/src/lib.rs` содержит C-экспорты `mh_wait1`, `mh_close1`, `mh_finish1`, `mh_release1`. Это карта мест следующего чтения, а не вывод о корректности их алгоритмов или наличии product consumer. Этот process-мост не является отсутствующим адаптером файлового reader #627.

Полные файлы лежат внутри [source-evidence.zip](source-evidence.zip), в каталоге `source-a217c779/experiments/rust-process-owner`. Индекс с длинами, Git blob и SHA-256: [a217-source-acquisition.json](source-evidence.zip) (путь в архиве: `a217-source-acquisition.json`). Raw commit: [a217-commit.raw](source-evidence.zip) (путь в архиве: `a217-commit.raw`). Исходный ответ транспорта: [wave-object-transport.json](source-evidence.zip) (путь в архиве: `wave-object-transport.json`); сжатые исходные байты: [a217-source-capture.b64](source-evidence.zip) (путь в архиве: `a217-source-capture.b64`). Метаданные worktree, точные ответы и явно частично прочитанные фрагменты живого отчёта — [wave-identity-evidence.json](source-evidence.zip) (путь в архиве: `wave-identity-evidence.json`).

## Что действительно исполнялось

Только существующие средства чтения: filesystem API и Git `cat-file`/`ls-tree`. Для передачи 64 raw blob использован собственный memory-only PowerShell transport, [capture_a217_readonly.ps1](source-evidence.zip) (путь в архиве: `capture_a217_readonly.ps1`), который запускает ровно два read-only Git-вызова, захватывает байты, проверяет неизменность Git EXE и сжимает ответ в памяти. Он не импортирует код проекта, не загружает DLL, не строит и не запускает Go/Rust. У Git отключены optional locks, lazy fetch, fsmonitor, untracked cache, auto GC/maintenance и сетевые протоколы; унаследованные `GIT_*` удаляются перед заданием явного окружения.

Первая транспортная попытка остановилась в CMD из-за слишком длинной строки до запуска PowerShell; её ответ сохранён. Тот же видимый read-only script затем передан как сжатая строка в памяти, в пределах длины CMD. Итоговый процесс завершился с **exit code 0**, runtime **4,92 с**; полный ответ захвачен и проверен. На Wave не создавались файлы, не менялись Git refs/index/worktree, не выполнялись checkout/fetch/commit, проектные runners, тесты, сборки, установки или загрузки зависимостей.

## Следующий остаток

Можно проводить source review точного wait/close/Go–C–Rust контракта по сохранённым 64 файлам. Нельзя заново восстанавливать его по предположениям или подменять текущими файлами более новой живой ветки. Потребитель Product, доказательство пользы Rust, общая квалификация и полное сравнение всех последующих ветвей остаются самостоятельными задачами.

Живой regression report уже содержит историю v2/v3/v4 и подготовку v5; из него прочитаны только отмеченные фрагменты, а не все 2066 строк. Поэтому старое `v2 NOT_RUN` в R25 handoff — историческое состояние, которое нельзя представлять как текущий итог параллельной работы. Новые сохранённые результаты других запусков здесь не переаттестованы и разрешения на них не выводились. Чтение исходников не является повтором установки.

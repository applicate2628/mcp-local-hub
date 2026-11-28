# Разбор сохранённых общих Go-проверок

**Результат:** архив доступен и проверен, но оба целевых журнала — обычный текст Go, а не JSON-события. Полная разбивка времени по корневым тестам из этих байтов не восстанавливается. Ни одна проверка Go, Cargo или конвейер непрерывной интеграции не запускалась.

## Содержание

1. [Источник и метод](#источник-и-метод)
2. [Известные времена](#известные-времена)
3. [Самые долгие пакеты](#самые-долгие-пакеты)
4. [Видимые длительности корневых тестов](#видимые-длительности-корневых-тестов)
5. [Снимки тайм-аутов](#снимки-тайм-аутов)
6. [Следующий проверяемый вопрос](#следующий-проверяемый-вопрос)
7. [Пределы и воспроизведение](#пределы-и-воспроизведение)

## Источник и метод

[Архив на точном опубликованном коммите](https://github.com/applicate2628/mcp-local-hub/blob/cf5e71ffd88abe53c611f8ee06b536b3228e60e7/docs/modernization/verification/go-recovery-qualified-inputs-2026-11-20/evidence-redacted.zip): **298 833 байта**, SHA-256 `d12ca2b81b815d18e4a49c659516e8f29c05709542aefb86919b9b5101b178ab`. Проверены Git blob, контроль целостности ZIP, состав и размеры/хеши всех **274 членов** по [метаданным](https://github.com/applicate2628/mcp-local-hub/blob/cf5e71ffd88abe53c611f8ee06b536b3228e60e7/docs/modernization/verification/go-recovery-qualified-inputs-2026-11-20/archive-metadata.json). Исходный ZIP сохранён в рабочем каталоге вне trash; в каталоге публикации этого отчёта архив и метаданные повторно не размещаются.

Обозначения: ROOT — корневой тест, без пути вложенного подслучая; API — программный интерфейс; CLI — интерфейс командной строки; IPC — межпроцессное взаимодействие; CST — название интегрируемого продукта электромагнитного моделирования; JSON — текстовый формат структурированных данных JavaScript Object Notation; UTF-8 — 8-битная форма кодирования Unicode. Wall time — фактически прошедшее время команды; elapsed в строке Go — напечатанная длительность указанного теста или пакета.

Исторический вход квитанции: `f755d374fab317e1037deecd5dac8ffabbcdde36` плюс незакоммиченная правка `internal/cli/gui_force_test.go`. Это не проверка текущего #658. `source_sha256/source_bytes` относятся к приватным оригиналам; опубликованные `saved_sha256/saved_bytes` проверены непосредственно. Исходные приватные байты заново не получались.

Квитанция задаёт `go test -count=1 -timeout 5m ./...` и tagged-команду для API/CLI; опций `-json` и `-v` нет. В каждой stdout-копии найдено **0 событий Go JSON**. Full содержит 86 байт, не декодируемых как UTF-8; tagged корректен как UTF-8. Анализ использует побайтные ASCII-шаблоны и не перекодирует исходники. Номера строк относятся к опубликованным членам ZIP.

Корневой тест — имя без `/`; вложенные подслучаи отдельно не суммируются. Напечатанные завершения отнесены к блоку, закрывающемуся строкой результата соответствующего пакета. Этот порядок текстовых блоков не используется как хронология параллельного выполнения.

## Известные времена

**Таблица 1 — внешние команды и пакеты.**

| Запуск | Внешняя команда, с | Самый долгий пакет, с | Сумма длительностей пакетов, с; не wall time | Начало МСК, 20.11.2026 | Следующая команда МСК |
|---|---:|---:|---:|---|---|
| full-test | 402.220887 | 300.103 | 1420.018 | 2026-11-20T11:49:38.698525+03:00 | 2026-11-20T11:56:20.920509+03:00 |
| tagged-api-cli | 313.08449 | 300.069 | 600.132 | 2026-11-20T11:56:20.920509+03:00 | 2026-11-20T12:01:34.006715+03:00 |

Обе команды завершились с кодом 1 и `settled=true`. Квитанция не хранит точный UTC-момент выхода процесса. Следующая команда начинается после его завершения. Выражение «начало + seconds» — вычисленная оценка, не наблюдённое событие: runner записывает UTC до сохранения квитанции, а монотонный отсчёт начинает после него. Время последнего JSON/текстового события и интервалы run→pass/fail отсутствуют.

Пакетные и тестовые времена могут перекрываться. Суммы в таблице 1 нельзя считать длительностью команды. Разности внешнего времени и максимума пакета — 102.117887 с и 13.01549 с — также не измеряют отдельно компиляцию или простой; начала пакетов неизвестны.

## Самые долгие пакеты

**Таблица 2 — десять самых долгих пакетов full; tagged содержит только API 300.069 с и CLI 300.063 с, оба FAIL.**

| Пакет | Статус | Напечатано, с | Строка full |
|---|---|---:|---:|
| `mcp-local-hub/internal/cli` | FAIL | 300.103 | 724 |
| `mcp-local-hub/internal/api` | FAIL | 300.047 | 189 |
| `mcp-local-hub/internal/gui` | ok | 240.305 | 744 |
| `mcp-local-hub/internal/daemon` | FAIL | 188.065 | 736 |
| `mcp-local-hub/internal/api/daemon_env_overlay` | ok | 97.414 | 192 |
| `mcp-local-hub/internal/clients` | ok | 35.042 | 725 |
| `mcp-local-hub/internal/process` | FAIL | 26.958 | 760 |
| `mcp-local-hub/internal/oneapirun` | ok | 24.944 | 752 |
| `mcp-local-hub/internal/binaryadmission` | ok | 23.774 | 197 |
| `mcp-local-hub/internal/api/serena_routing` | ok | 20.083 | 194 |

Full содержит 73 результата пакетов: 58 PASS, 4 FAIL, 11 без тестовых файлов. Даже успешный GUI-пакет занял 240.305 с; анализ только двух тайм-аутов не объясняет весь внешний запуск.

## Видимые длительности корневых тестов

**Таблица 3 — полнота напечатанных корневых завершений.**

| Запуск / пакет | Напечатанные ROOT FAIL | Сумма напечатанных длительностей, с |
|---|---:|---:|
| full-test / `mcp-local-hub/internal/cli` | 1 | 0.00 |
| full-test / `mcp-local-hub/internal/daemon` | 1 | 0.31 |
| full-test / `mcp-local-hub/internal/process` | 2 | 0.00 |
| tagged-api-cli / `mcp-local-hub/internal/cli` | 15 | 8.03 |

Full показывает только четыре завершившихся ROOT FAIL: CST 0.31 с, RunV5 и два Job-теста по напечатанным 0.00 с. Tagged показывает RunV5 0.00 с и 14 Serena-отказов, всего 8.03 с. Значение 0.00 — округлённая запись, не доказательство нулевой длительности. PASS-корни не перечислены; полные slowest-10 и распределение 300 с между корнями неизвестны. Эти частичные суммы не вычитаются из времени пакета как неперекрывающиеся интервалы.

**Таблица 4 — десять самых долгих только среди напечатанных tagged ROOT FAIL; это не десять самых долгих тестов пакета.**

| Тест | Напечатано, с | Строка tagged |
|---|---:|---:|
| `TestMigrateSerena_PostCommitTimeoutSupervisorNotAlive_FailsLoud` | 0.89 | 602 |
| `TestMigrateSerena_LiveSupervisor_ReapClearsTheGateBeforeWrite` | 0.81 | 589 |
| `TestMigrateSerena_EmptyFirstSnapshot_SupervisorRunning_LateReapClearsGate` | 0.79 | 630 |
| `TestMigrateSerena_EmptyFirstSnapshot_WorkspaceRegisteredInWindow_RecomputesStart` | 0.62 | 623 |
| `TestMigrateSerena_CatalogDynamicPool_RuntimeLegacyMissing_Proceeds` | 0.60 | 577 |
| `TestMigrateSerena_ConcurrentRegisterInWindow_AppearsInCommittedIntent` | 0.58 | 620 |
| `TestMigrateSerena_NilSpecRowsHealedBeforeSpawn` | 0.57 | 586 |
| `TestMigrateSerena_AlreadyMigrated_RegistryDrift_RefansOut` | 0.57 | 626 |
| `TestMigrateSerena_DrivesSupervisorRestart` | 0.55 | 582 |
| `TestMigrateSerena_NoRunningSupervisor_SkipsReap_WritesAndStarts` | 0.47 | 617 |

## Снимки тайм-аутов

**Таблица 5 — только имена из `running tests` при панике.**

| Запуск / пакет | Имя | Напечатанный возраст | Строка |
|---|---|---:|---:|
| full-test / `mcp-local-hub/internal/api` | `TestAttachExisting_FreshCommittedPlanApplyRefusesDrift` | 0s | 72 |
| full-test / `mcp-local-hub/internal/cli` | `TestMCPFrontV3_SerenaConflictClassifierMutationTable` | 3s | 638 |
| full-test / `mcp-local-hub/internal/cli` | `TestMCPFrontV3_SerenaConflictClassifierMutationTable/baseline-marker-is-invalid` | 0s | 639 |
| tagged-api-cli / `mcp-local-hub/internal/api` | `TestExecuteDeAdoptT8RoutedSecretPrefilterSharedSkipAndClose` | 1s | 69 |
| tagged-api-cli / `mcp-local-hub/internal/cli` | `TestMigrateSerena_AppliedInstallContinuationMatrix` | 4s | 638 |
| tagged-api-cli / `mcp-local-hub/internal/cli` | `TestMigrateSerena_AppliedInstallContinuationMatrix/timeout-probe-error-hard` | 0s | 639 |

Это снимки выполнявшихся тестов, не восстановленный список всех начатых тестов без завершения. Возраст текущих корней — 0/3 с и 1/4 с; он не равен 300-секундному бюджету пакета. Последние имена не устанавливают причину тайм-аута.

## Следующий проверяемый вопрос

В трёх из четырёх снимков текущий поток находится в `FlushFileBuffers` через `secureWriteClientConfigToResolvedParentObserved`, `secure_write_windows.go:330`: full CLI сохраняет состояние; tagged API создаёт vault-фикстуру; tagged CLI сохраняет registry в `seedSerenaWorkspaceWithPort`. Full API вместо этого ожидает сообщение приветствия локального supervisor IPC в `readSupervisorIPCLine`. Точные строки и прикладные кадры сохранены в JSON.

**Гипотеза для следующего адресного измерения:** повторяющиеся синхронные сохранения при подготовке фикстур могут создавать заметную накопленную стоимость. Нужны число вызовов и их интервалы на одной проверенной группе тестов, с разделением подготовки и проверяемого действия. Эти снимки не дают длительность отдельных `FlushFileBuffers`, не доказывают неисправность носителя и не объясняют весь бюджет. Повтор последнего теста без такой гипотезы не обоснован.

Предшествующий доступный шаг — новая инвентаризация компилируемых Test/Example/Fuzz-корней на точном #658 и план частей. Она даст состав, но не длительности. Последующее исполнение выбранной части требует отдельной проверки её тел и входов. Общий предел 5m и обязательные полные команды остаются прежними.

## Пределы и воспроизведение

`analyze_go_budgets.py` читает ZIP и метаданные, проверяет их и создаёт этот Markdown и JSON. Он не импортирует код проекта, не запускает дочерние процессы и не изменяет исходный архив. Для повторения используйте новый выходной каталог:

```text
python analyze_go_budgets.py --archive <путь-к-старому-evidence-redacted.zip> --metadata <путь-к-старому-archive-metadata.json> --output <новый-каталог>
```

Входы для воспроизведения: [старый evidence-redacted.zip](https://github.com/applicate2628/mcp-local-hub/blob/cf5e71ffd88abe53c611f8ee06b536b3228e60e7/docs/modernization/verification/go-recovery-qualified-inputs-2026-11-20/evidence-redacted.zip) и [старый archive-metadata.json](https://github.com/applicate2628/mcp-local-hub/blob/cf5e71ffd88abe53c611f8ee06b536b3228e60e7/docs/modernization/verification/go-recovery-qualified-inputs-2026-11-20/archive-metadata.json). Оба закреплены на том же неизменяемом коммите.

[Опубликованное описание исходного среза](https://github.com/applicate2628/mcp-local-hub/blob/cf5e71ffd88abe53c611f8ee06b536b3228e60e7/docs/modernization/verification/go-recovery-qualified-inputs-2026-11-20/RESULTS.md) и `evidence/gates/receipt.json` в архиве задают источник и команды. Честно закрыто чтение и разложение доступных наблюдений; полная ROOT-телеметрия исторических запусков отсутствует, а общие FAIL сохраняются.

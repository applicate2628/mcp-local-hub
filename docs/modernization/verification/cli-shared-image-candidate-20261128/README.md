# Кандидат: общий foreign image для двух CLI identity tests

## Область

Это **draft, не применённый и не исполненный**. Он относится к точной Source `59f618ebfbf6d9f52c6994d68586e3c2ebc202a8`, tree `03e8488a44e67f1ea7e8dc2ef0ece53fee5ae8e1`, и меняет два `_test.go` файла. Ожидаемый полный tree после ровно этого patch: `5be2e2ef1f006da03a8ff7ed993b5e649d619116`.

[Точный patch](share-foreign-test-image.patch), [две afterimages](afterimages/internal/cli/) и [метаданные](evidence.json) готовы для собственного review и штатного применения root. Рабочие Source, Git, текущие inputs/cache и UNKNOWN владельцы не менялись.

## Причина и граница доказательства

В старом уже закрытом Source727d CLI прогоне `TestPidMatchesMcphub_RejectsForeignPathSameBasename` напечатал 26,91 s, а `TestLoadSupervisorCurrentRunning_SkipsLiveNonMcphubPID` — 19,03 s. Их тестовый файл совпадает с Source59f побайтно. Каждый test копирует один и тот же входящий test image в отдельный файл с именем `mcphub.exe`, запускает helper-процесс, выполняет свою проверку и вызывает исходные Kill/Wait.

Эти 45,94 s включают копирование, запуск, проверку и ожидание завершения. Capture не измеряет их раздельно. Поэтому patch устраняет доказанное повторное копирование, но **не обещает экономию 45,94 s или успешный полный прогон за пять минут**. Production identity implementation между старой Source и Source59f отличается.

Полный старый разбор хранится отдельно в `old-cli-budget-analysis-v1`, manifest `bcfeae39f1c96fc4656e5f0490497649545919dbc437e6577d464dd6b5df8252`. Он не передаёт текущей Source тестовую квалификацию.

## Что меняется

В существующем TestMain сохраняется путь уже созданного и защищённого package root. Windows helper один раз создаёт под ним `pid-match-helper/mcphub.exe`, копируя реальные bytes из `os.Executable()`. Каталог и файл должны отсутствовать; файл создаётся с O_EXCL. Закрытие output проверяется до использования. sync.Once сохраняет успешный путь или исходную ошибку; ошибка приводит к FAIL вызывающего test.

Оба tests вызывают этот helper, но каждый запускает **новый исходный exec.Cmd и отдельный процесс**, использует тот же sentinel и helper argv, сохраняет своё state fixture и прежние native identity assertions. Файл остаётся до завершения m.Run и штатной cleanup package root. Это обычная копия; hard link и подстановка другого Product не используются.

В случае panic/timeout существующий TestMain не достигает post-m.Run cleanup. Patch не добавляет отдельной очистки, попытки закрыть UNKNOWN ресурсы, ожидания по заново открытому PID или объявления closure по ActiveProcesses=0.

## Что проверено статически

- Два прежних callers найдены bounded read-only search; оба передавали ровно `mcphub.exe`.
- Новые identifiers отсутствовали в полученной изолированной Source59f.
- Оба base SHA-256 совпадают с root-received exact3369 input map.
- Полный исходный Git tree воспроизведён из всех 3369 mode/blob entries до вычисления after-tree.
- Patch сохраняет тестовые bodies, deadlines, helper dispatch, Start/Kill/Wait и итоговые assertions; меняется только получение общего image path.
- Source, production code, команды, tags и обязательный inventory не менялись.

Go compiler, gofmt, vet и tests для этого кандидата **не запускались**. Перед применением root должен самостоятельно прочитать patch; после применения штатный формат/compile и обязательные полные команды относятся уже к новому точному Source head.

## Требуемая проверка

```text
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

Между этими tests нет нового параллелизма. Ни один из двух roots не заменён subtest, SKIP, выборочным запуском, искусственным native результатом или ранее полученным PASS. Оставшиеся 374 старых не начатых roots и 14 paused migration bodies не получают статуса от этой правки.

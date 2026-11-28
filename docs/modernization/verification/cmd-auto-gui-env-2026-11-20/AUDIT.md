# Аудит минимального кандидата изоляции окружения

## Основа и границы

- Репозиторий: `applicate2628/mcp-local-hub`; PR (запрос на включение изменений) [#658](https://github.com/applicate2628/mcp-local-hub/pull/658).
- При подготовке GitHub подтвердил открытый PR и точный head `32b3c4679a6f323c82529a07df9a3a87fc17d6f7`.
- Исходник: [cmd/mcphub/main_test.go](https://github.com/applicate2628/mcp-local-hub/blob/32b3c4679a6f323c82529a07df9a3a87fc17d6f7/cmd/mcphub/main_test.go), Git blob `2c6a59c6df43eb47c76f48de8a5515b9c32ebbd8`.
- Кандидат добавляет только пять строк `t.Setenv(NoAutoGUIEnv, "")`, по одной в начале каждого выбранного теста. Производственный код и существующие проверки отказа не изменены.
- Это исторический статический аудит до исполнения: команды тестов и сборки, удалённые процессы и внешнее ревью здесь не запускались. Последующие выполненные проверки приведены в RESULTS.md. Публикация и интеграция веток этим аудитом не выполнялись.

## Причина правки

Сохранённый `r17/last-result-20261120/report/evidence-redacted.zip`: `run_checks.py:42` задаёт `MCPHUB_NO_AUTO_GUI=1`, а `evidence/gates/receipt.json:182` подтверждает это окружение. Пять тестов ожидают разрешённый автоматический запуск, но не фиксируют эту предпосылку. В [main.go](https://github.com/applicate2628/mcp-local-hub/blob/32b3c4679a6f323c82529a07df9a3a87fc17d6f7/cmd/mcphub/main.go) строки 116–117 выполняют ранний возврат; строки 43–44 классифицируют запуск без подкоманды как CLI (интерфейс командной строки). Именно эти результаты записаны в исходном журнале. Полный пакет завершился за 0,194 секунды; его пять отказов не являются тайм-аутами.

Согласно [документации Go testing.T.Setenv](https://pkg.go.dev/testing#T.Setenv), `t.Setenv` задаёт значение и регистрирует восстановление исходного окружения при завершении теста. Этот метод нельзя использовать в параллельном тесте или тесте с параллельным предком. Пустая строка не признаётся отказом функцией `autoLaunchGUIOptedOut` в `main.go:173–175`.

## Проверка отсутствия реального запуска GUI

GUI — графический интерфейс. Номера строк относятся к началу функций в исходнике и кандидате.

| Тест | Исходник → кандидат | Проверенная защита |
|---|---:|---|
| `TestPrepareInvocationWindowsBareLaunchReturnsWithoutInlineCobra` | 14 → 14 | До вызова `prepareInvocation` установлен `launchBareGUIFn`, который только увеличивает счётчик и возвращает `true, nil`; исходная функция восстанавливается через `t.Cleanup`. |
| `TestPrepareInvocationBareLaunchFailureRemainsVisible` | 63 → 64 | Координатор заменён функцией, возвращающей заранее созданную ошибку; исходная функция восстанавливается через `t.Cleanup`. |
| `TestPrepareInvocationNormalizedDebugBareStaysInline` | 75 → 77 | Подмена координатора установлена. На Windows явная отладочная политика направляет вызов в `RouteInvocationArgs`, который лишь копирует/дополняет список аргументов. Команда `gui` не исполняется; ожидается ноль вызовов координатора. |
| `TestPrepareInvocationNormalizedPlainBareCallsDetachedCoordinator` | 95 → 98 | До `prepareInvocation` установлен координатор-заглушка, который только увеличивает счётчик и возвращает `true, nil`; исходная функция восстанавливается через `t.Cleanup`. |
| `TestClassifyWindowsEntrypointModeExhaustiveRootCommands` | 248 → 252 | Тест не вызывает `prepareInvocation` или координатор. Он получает `NewRootCmd().Commands()` и классифицирует аргументы; `Execute`, `Run` и `RunE` не вызываются. |

Дополнительная цепочка по точному head:

- [windows_console_policy_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/32b3c4679a6f323c82529a07df9a3a87fc17d6f7/internal/cli/windows_console_policy_windows.go), строки 8–16: нормализация только преобразует аргументы и возвращает политику.
- [root.go](https://github.com/applicate2628/mcp-local-hub/blob/32b3c4679a6f323c82529a07df9a3a87fc17d6f7/internal/cli/root.go), строки 48–53: `RouteInvocationArgs` только формирует список аргументов.
- `root.go:56–58, 123–125, 272`: конструктор корневой команды создаёт объект команды GUI.
- [gui.go](https://github.com/applicate2628/mcp-local-hub/blob/32b3c4679a6f323c82529a07df9a3a87fc17d6f7/internal/cli/gui.go), строки 467–799: `newGuiCmdReal` создаёт команду и регистрирует флаги; настоящий `startGuiServer` находится внутри замыкания `RunE` на строке 780. Перечисление команд это замыкание не вызывает.
- В пяти выбранных функциях нет `t.Parallel`, `main()` или выполнения команд Cobra.

Это проверка достижимости по исходникам; она не представляется как наблюдение работающего процесса.

## Существующие проверки отказа для последующего контроля

Основная соседняя проверка — `TestClassifyWindowsEntrypointModeBareOptOutKeepsCobraHelp`: истинные значения `1`, `true`, ` TRUE `; режим CLI; отсутствие вызова координатора; видимая справка корневой команды.

Также уже существуют и оставлены без изменений:

- `TestPrepareInvocationPreservesNonBareAndOptOutRoutes`, подслучай `bare opt out`: аргументы сохранены, координатор не вызван.
- `TestShouldAutoLaunchGUI_EnvOptOut`: варианты значения окружения, включая пустое, отсутствующее, `0`, `1` и `true`.
- `TestShouldAutoLaunchGUI_OptOutDoesNotLeakIntoThePureSeam`: переменная окружения не меняет чистую проверку аргументов.

Минимальный последующий Windows-контроль: пять изменённых тестов плюс основная соседняя проверка при внешнем `MCPHUB_NO_AUTO_GUI=1`. Более полный локальный контроль этой границы может включить все четыре существующие проверки выше. Здесь эти команды не выполнялись.

## Что проверено и что осталось

Проверена сохранность исходных байтов по Git blob SHA-1, точная текстовая дельта, число добавлений/удалений, отсутствие иных изменений и пути до реального запуска GUI. У кандидата ровно **+5 / −0**, без изменений импортов или утверждений тестов. Сборка, исполнение тестов, гонки и полная приёмка не установлены.

Отдельная существующая граница: `TestPrepareInvocationNormalizedDebugBareStaysInline` предполагает Windows-нормализацию, тогда как [вариант для других платформ](https://github.com/applicate2628/mcp-local-hub/blob/32b3c4679a6f323c82529a07df9a3a87fc17d6f7/internal/cli/windows_console_policy_other.go) возвращает неизменённые аргументы. Этот кандидат не меняет платформенную применимость теста и не заявляет успешность на Linux/macOS.

| Файл | SHA-256 |
|---|---|
| `baseline/main_test.go` | `3a0f226691721cc109417042937285d4223db94f6688bb4cd6293e8b9e197d09` |
| `candidate/cmd/mcphub/main_test.go` | `2c10343ad035722c3e03af41546d2a2f95118daae5bbf25eddb68f0ac6b3ae42` |
| `main_test.go.diff` | `e239e787ffb51732838e2639342345a14a881a40492889ed55079b1b92312e5a` |

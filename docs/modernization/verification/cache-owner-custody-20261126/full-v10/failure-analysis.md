# Full-v10: принятые причины FAIL и границы доказательств

Полный прогон закрыт с ошибкой. `go build` и `go vet` завершились успешно; обе обязательные регрессионные команды имеют exit 1. Лимит тестов остался 5 минут. Источник тестов: `aca81a1743ccfbf3e10902769a9b071eb911919e`; источник продукта и Serena: `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`.

## Содержание

1. [Четыре неуспешных набора](#четыре-неуспешных-набора)
2. [Накопленное время](#накопленное-время)
3. [Покрытие и сохранность](#покрытие-и-сохранность)
4. [Доказательства](#доказательства)

## Четыре неуспешных набора

Таблица 1. Ошибки относятся к разным стадиям.

| Набор | Подготовка | Наблюдаемый исход |
|---|---:|---|
| default api | 40.893095 s | Тесты не начаты: превышен бюджет подготовки 30 s. Все 6 служебных команд завершены. |
| default cli | 48.929169 s | Тесты не начаты: превышен бюджет подготовки 30 s. Все 6 служебных команд завершены. |
| state api | 12.103594 s | Общий timeout 5m; 266 PASS, 1 SKIP, 3261 ещё не начаты, 1 начаты без завершения. |
| state cli | 18.686209 s | Общий timeout 5m; 980 PASS, 11 SKIP, 985 ещё не начаты, 15 начаты без завершения. |

Default API: до рождения дочернего inventory-процесса прошло 27.9665243 s; default CLI — 21.8155718 s. Это измеренные промежутки владельцев процесса; они не доказывают причину задержки внутри CreateProcess или других операций.

Tagged API остановлен при `TestBuildAdoptPlanWithExplicitPortIgnoresExhaustedLegacyDraftRange (0s)`; tagged CLI — при `TestRunSupervise_IPCShutdownJoinsAcceptExitOnTestExit (0s)`. Приписывать этим тестам предшествующие пять минут нельзя. В стеке API виден разбор YAML; в CLI — ожидание события и `FlushFileBuffers` записи supervisor lock. Длительность этих конкретных операций журнал не измеряет.

## Накопленное время

Суммы ниже включают только завершённые тесты верхнего уровня. Время подтестов повторно не складывается. Источник — текстовые Go-строки, округлённые до 0.01 s; полей Time/Elapsed в test2json нет.

### mcp-local-hub/internal/api

Все завершённые: **299.76 s**. Двадцать самых долгих: **214.82 s** (71.61% от 300 s). Теоретическая погрешность суммы из-за округления не более ±1.335 s.

Таблица 2. Двадцать наиболее долгих завершённых тестов; все PASS.

| Тест | Время, s |
|---|---:|
| `TestAdoptReceiverRejectsIntentIdentityDrift` | 47.59 |
| `TestAdoptRepeatRejectsLegacyEmptyIntentHash` | 40.30 |
| `TestProbeProviderProcessWorkingDirectoryReadsOwnedWOW64Helper` | 26.70 |
| `TestProviderWorkingDirectoryIdentityOwnsOnlySelectedLiveInstance` | 15.64 |
| `TestAdoptSecretRoutingNamespacesVaultKeysByManifest` | 10.30 |
| `TestAdoptRepeatRejectsMalformedProvenance` | 9.04 |
| `TestProviderManagedInstallStateAdvancesBeforeInstall` | 7.67 |
| `TestAdoptSecretPrefixedExistingVaultKeyRoutesAsForeignLiteral` | 6.76 |
| `TestAdoptNonCodexCompatibilityMatrixDoesNotEmitHSettlement` | 6.02 |
| `TestCaptureAdoptProvenanceUpsertReapsOrphan` | 5.90 |
| `TestProviderWorkingDirectoryPEBDOSPathNamedPipeDoesNotConnect` | 5.01 |
| `TestExecuteProviderAdoptCommitsDisabledProviderAndRepeatIsNoop` | 4.64 |
| `TestExecuteAdopt_PreservedRowReclaimableAfterOperatorReversal` | 4.51 |
| `TestAdoptCodexToolTimeoutRoundTripsBindingAndDeAdopt` | 4.08 |
| `TestAdoptReceiverRejectsParseableManifestHashDrift` | 3.98 |
| `TestAdoptCaptureRefusesMutatedPriorCrashRow` | 3.70 |
| `TestExecuteProviderAdoptRestoresActivationOnPostCASMetadataDrift` | 3.38 |
| `TestExecuteAdoptPersistsProvenanceAcrossFreshAPI` | 3.37 |
| `TestAdoptFreshIntentHashMatchesManifestAndProvenance` | 3.28 |
| `TestExecuteAdopt_ForwardCommittedPreservesPartialPrefixForRecovery` | 2.95 |

### mcp-local-hub/internal/cli

Все завершённые: **299.70 s**. Двадцать самых долгих: **167.39 s** (55.80% от 300 s). Теоретическая погрешность суммы из-за округления не более ±4.955 s.

Таблица 3. Двадцать наиболее долгих завершённых тестов; все PASS.

| Тест | Время, s |
|---|---:|
| `TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade` | 26.21 |
| `TestMCPFrontReview_ForwardRefusesToMergeIntoARetiredGeneration` | 25.56 |
| `TestGUIOwnerUnknownConfirmationMarkerWorker_HeldLockBoundsAllWriteSites` | 15.45 |
| `TestReliability_VersionCommandExitsZero` | 12.28 |
| `TestMCPFrontV3_SerenaConflictClassifierMutationTable` | 8.86 |
| `TestMigrateSerena_RealWriterConsentReceivingWindows` | 7.73 |
| `TestRunReconcileMCPFront_Rollback_InvalidDurableTargetFailsClosed` | 6.77 |
| `TestRunReconcileMCPFront_ForwardThenRollback_RoundTrip` | 6.28 |
| `TestMCPFrontOwnership_ForwardAcceptsASupervisedRouteDaemon` | 6.17 |
| `TestForce_KillRaceLost` | 6.16 |
| `TestAcceptLoop_BudgetBreakerExitsOnFlood` | 5.74 |
| `TestForce_KillRewritesPidportWithCurrentPID` | 5.31 |
| `TestGUIOwnerUnknownConfirmationMarkerWorker_HeldLockTimesOutAndReaps` | 5.05 |
| `TestDaemonEnvWithOverlayPathCaseMatrix` | 4.93 |
| `TestLoadSupervisorCurrentRunning_SkipsLiveNonMcphubPID` | 4.69 |
| `TestPidMatchesMcphub_RejectsForeignPathSameBasename` | 4.68 |
| `TestMCPFrontR2_SecondInvocationRefusesWhileTheTransactionLockIsHeld` | 4.07 |
| `TestRestartV3_SamePortStandbyBindWaitsForParentClose` | 3.94 |
| `TestMCPFrontR2_RollbackRefusesAnOperatorEditedSerenaEntry` | 3.83 |
| `TestMCPFrontV3_ClassifierRejectsPinMutationAcrossAcceptedShapes` | 3.68 |

## Покрытие и сохранность

Из 38 изменённых портовых тестов tagged-набор завершил 35 с PASS: 30 API и 5 CLI. Три API-теста не были начаты: `TestAdoptPlanOutputReportsOmittedSameNameClients`, `TestExecuteAdoptInstallFailureRemovesAdoptCreatedManifestAndSaysRerun`, `TestExecuteAdoptRefusesUnavailableVaultBeforeManifestWrite`. Default API/CLI не подтверждают эти исправления, поскольку не начали тесты.

Четырнадцать приватных миграционных тестов Serena были поставлены на паузу через `t.Parallel`; ни один не получил событие CONT. Это отсутствие покрытия в данном прогоне, а не опровержение отдельно закрытой КТ-1. Обязательный CLI-тест unreadable-intent прошёл; оставшиеся три обязательных CLI-теста и все пять обязательных API-тестов не были достигнуты.

Независимо приняты четыре failed-adapter receipt, 28 служебных команд и шесть оригинальных native-owner receipt; проверены завершение процессов, нулевой остаток дерева, EOF и закрытие каналов/выходных файлов перед чтением captures. Остальные 62 успешных адаптера здесь представлены точной оригинальной агрегированной квитанцией и phase settlement; 124 остальных оригинальных native receipts повторно не читались. Физическое закрытие не выдаёт PASS или разрешение на повторное использование/перенос ресурсов.

Оригинальная квитанция сообщает неизменность исходников, продукта, первоначальных входов и экспортов миграции. Чужие и удерживаемые области v7/x6d/source-v1 не читались. Проверки не запускались повторно.

## Доказательства

Таблица 4. Главные оригинальные и производные файлы.

| Файл | SHA-256 |
|---|---|
| `parent.json` | `3812d74a5a4b9d02eabae0a05a67ba7bb3dd79a729b40d91d29e05d0fbf6e7cb` |
| `receipt.json` | `555263b0add9ceec75e35967bc62e84c68c5e820e2e9bf5ba48be8bf551a225b` |
| `failure-scopes-closure.json` | `7ef88825eb92af05870e0d36285e535c858fc210fb331a76b8979a8ff4666095` |
| `capture-receiving.json` | `a6d3b83791f8a3a6a89342becb4f8d2788a9147b7093f5bc9219d6ca983b5ccf` |
| `event-timing-profile.json` | `115ff437f5e780095d1d35c7d17e64cc3bf4c7bfaf20d48cce9e69ec6c4b9229` |
| `completed-test-ledger.json` | `31d183a866145f07ab645cbf7b034949d68987b6b67e962f56af67ab63cdde8e` |
| `changed-test-status.json` | `76bba0ce646626fccd1ca9e620a99d5df6d0afea71452686f275dd9c973dd854` |
| `failure-analysis.json` | `2a934c2946e7fa644a38372d134878e60a70506261d72da6cd1eb514b12b3f88` |

Native bytes сохранены отдельно от транспортного текста. CRLF восстановлен только там, где получившийся SHA-256 совпал с известным native SHA; JSON-идентификаторы uint64 не пропускались через числовую сериализацию JavaScript.

# Контракт безопасного восстановления текущей scheduler-задачи

Дата проверки: 20 ноября 2026 года, UTC. Исходный коммит: `c55d9ca619f6f9301ba1113615370fc6218d608f`, дерево `06a85c6efe53f5a4a8babbcadb7818752bb7acee`.

**Рекомендация: подключить к одной ветви ошибки Create отдельную возможность `CreateXMLIfAbsent`, заранее проверить её наличие и сохранить подтверждённый защищённый снимок до Delete.** При любой ошибке восстановления сохранить снимок и обе причины; никакого повторного Delete, обычного ImportXML или вывода о владельце задачи из совпадения XML. Общий интерфейс Scheduler расширять для этого не требуется.

Это статический обзор контракта, исходников и затем подготовленного кандидата R21. Продукт, тесты и удалённая машина в этой работе не запускались; файлы продукта и тестов не изменялись. Результат независимого ревью кандидата приведён в разделах 10–11; он не заменяет нативную квалификацию и не является подтверждением готовности полного запроса на включение изменений (PR) #660.

## Содержание

1. [Исходное поведение и подтверждённые риски](#baseline)
2. [Минимальный контракт платформенной операции](#backend)
3. [Контракт потребителя install](#consumer)
4. [Защищённый сохраняемый снимок](#snapshot)
5. [Различающая матрица проверок](#tests)
6. [Совместимость, простота и открытые границы](#limits)
7. [Проведённая проверка и прочитанные навыки](#verification)
8. [Источники](#sources)
9. [Термины и сокращения](#terms)
10. [Независимое ревью кандидата API](#candidate-api)
11. [Независимое ревью кандидата Windows Scheduler](#candidate-scheduler)
12. [Независимое ревью runner и frozen source manifest](#candidate-runner)
13. [Ревью ограниченного нативного harness](#native-harness)
14. [Ревью native build/run wrapper](#native-wrapper)

<a id="baseline"></a>
## 1. Исходное поведение и подтверждённые риски

В `executeInstallToWithSymlinkConsents` снимок получен в `install.go:3821`, успешный Delete завершается на `3830–3832`, затем Create вызывается на `3834`. Его ошибка немедленно возвращается через `failAfterRollback`; компенсация текущей задачи добавляется лишь на `3839–3848`. Поэтому при ошибке Create до применения и свободном имени старое определение теряется: сохранённый `priorXML` текущего шага не используется для восстановления. Это существовавшая граница, не дефект предыдущего допуска снимка/удаления [1].

Перенос имеющейся компенсации перед Create не исправляет требуемый контракт: компенсация безусловно вызывает Delete, а затем ImportXML. Windows ImportXML использует `/F`, то есть разрешает замещение существующего определения. Если между исходным Delete и отказом Create появилась внешняя задача, либо Create сам успел создать определение до возврата ошибки, такой откат удалит или заменит присутствующую задачу без доказанного владения [1, 3].

**Таблица 1 — подтверждённые места и минимальные действия.**

| Место | Условие и последствие | Действие в выбранном срезе |
|---|---|---|
| `install.go:3834–3839` | Delete успешен; Create отказал; текущая компенсация ещё не зарегистрирована. Старое определение не восстанавливается. | Отдельный вызов безопасного восстановления в этой ветви ошибки. |
| `install.go:3840–3842`, `scheduler_windows.go:274` | Перенос старой closure перед Create удаляет/перезаписывает появившуюся задачу. | Не использовать эту closure для текущего неуспешного Create. |
| `register_test.go:3686–3703,3746–3753` | Fake Create и ImportXML записывают map без проверки занятого имени. Такой fake может сделать опасную реализацию зелёной. | Для новой матрицы использовать модель с реальным отказом при занятом имени и отдельными исходами до/после применения. |
| `readiness_command.go:247–255`, `cli/install.go:399–402` | Координатор сохраняет исходную ошибку, но ошибка печати settlement в CLI заменяет её. Только память typed error не обеспечивает доступность снимка после завершения процесса. | Снимок существует на диске до Delete. Не заявлять полную доставку причин по всем внешним каналам без их проверки. |

Локальные blob-идентификаторы `install.go`, нового snapshot-теста и `register_test.go` сверены командой `git hash-object` без записи объектов и совпали с точными blob исходного дерева: `18d246b0288620d1e65e81e5a4ee87e6a68431a5`, `99c1b93f70a57d8bca1f797484de154debb0ce4d`, `30737f11000a3bca8c709378320c091a1be0199a`. Следовательно, локально разобранные ветви относятся именно к указанному коммиту [1, 4, 5].

<a id="backend"></a>
## 2. Минимальный контракт платформенной операции

Предлагаемый контракт программного интерфейса (API), не существующее в исходном коммите объявление:

```go
type XMLCreator interface {
    CreateXMLIfAbsent(name string, xml []byte) error
}
```

Это дополнительная возможность конкретной реализации Scheduler. Тип разумно объявить рядом с существующим optional-интерфейсом `ContextLister`: там находится общий контракт платформенных реализаций. Windows реализация обязуется обеспечить условие на стороне Task Scheduler; install проверяет поддержку перед разрушительным шагом. Простая проверка ExportXML перед принудительным импортом контракту не соответствует: между двумя вызовами имя может занять другой участник [2, 6].

Метод должен иметь следующие точные свойства:

- **Вход:** точное имя и сохранённый XML. Вызов не должен модифицировать переданный срез байтов; потребитель сохраняет собственную копию снимка. Поддерживаемая кодировка и способ передачи XML должны быть определены платформенной реализацией.
- **Свободное имя:** зарегистрировать определение из XML. Возврат `nil` означает подтверждённое принятие этой операции платформенным владельцем.
- **Занятое имя:** вернуть различимую ошибку, например `ErrTaskAlreadyExists`; существующее определение не удалять и не обновлять. Это верно также при побайтовом совпадении XML: совпадение не даёт права замещения.
- **Неопределённый исход:** любой транспортный, протокольный или иной отказ после возможной отправки не означает «не применено». Запрещены последующая слепая очистка, принудительный повтор и превращение такого отказа в успех по одному чтению совпавших байтов.
- **Прочие эффекты:** не выполнять явные Run/Stop/Delete. Полный запрет выполнения самой задачи шире этого условия: её собственные временные триггеры могут сработать независимо.

Microsoft различает регистрацию новой задачи флагом `TASK_CREATE` и обновление существующей через `TASK_UPDATE`/`TASK_CREATE_OR_UPDATE`. Для платформенного пути уместен явный режим создания; конкретное отображение нативного конфликта в Go sentinel и правильность передачи principal/logon-параметров должны проверяться отдельно. `TASK_IGNORE_REGISTRATION_TRIGGERS` подавляет именно регистрационные триггеры, а не все возможные запуски. Документация — основание выбора примитива; модельный тест и отсутствие `/F` сами по себе не доказывают нативную гарантию нового пути [16].

**Таблица 2 — выбор поверхности API.**

| Вариант | Совместимость и сложность | Вывод |
|---|---|---|
| Дополнительный `XMLCreator` | Существующие реализации Scheduler компилируются; нужен один type assertion и отказ до Delete при отсутствии поддержки. Есть аналог ContextLister. | Рекомендуется для одного потребителя. |
| Новый обязательный метод Scheduler | Все fake/stub/wrapper-реализации должны обновить method set; единственный потребитель восстановления расширяет контракт для всех. | Не даёт дополнительной безопасности по сравнению с обязательным admission перед Delete. |
| Изменить ImportXML на create-only | Меняет намеренные сценарии замещения, существующий откат и смежные потребители. | Не соответствует узкому срезу. |
| ExportXML → Delete/ImportXML либо ExportXML → force-import | Проверка и запись разделены; внешний участник может занять имя после проверки. | Не соответствует инварианту. |

Не требуется вводить глобальную классификацию исходов Create, общий автомат транзакции или новый тип TaskSpec. Один безопасный conditional-create можно попробовать после любого Create error при наличии старого снимка: если имя свободно — вернуть старое определение; если занято — сохранить присутствующее и вернуть конфликт.

<a id="consumer"></a>
## 3. Контракт потребителя install

Рекомендуемая последовательность для одной задачи:

1. Выполнить существующий ExportXML admission. Ошибка, пустой успешный снимок и противоречие «not found плюс байты» по-прежнему останавливают шаг.
2. При подтверждённом отсутствии выполнить обычный Create без Delete и без recovery-снимка. При его ошибке ничего по текущему имени не очищать.
3. При непустом успешном снимке проверить `XMLCreator`. Если его нет, вернуть отказ до создания recovery-файла, Delete и Create; никаких запасных путей через ImportXML.
4. Скопировать `priorXML`, записать защищённый recovery-артефакт и подтвердить точный readback. Любая ошибка этого этапа останавливает замену до Delete.
5. Выполнить единственный исходный Delete. При ошибке сохранить артефакт, вернуть ошибку Delete и его путь; Create и восстановление не вызывать. Ошибка Delete может иметь неопределённый эффект — сообщение не должно утверждать, что старая задача сохранилась.
6. Выполнить обычный Create. При успехе продолжить существующий сценарий; отдельный recovery-вызов не нужен.
7. При Create error вызвать `CreateXMLIfAbsent` ровно один раз с сохранёнными байтами. Не вызывать дополнительный Delete, ImportXML, Run либо Stop текущей задачи.
8. Затем выполнить существующий откат ранее завершённых шагов, сохраняя причину текущей ошибки. Успешное восстановление текущей задачи не превращает install в успех.

Для ошибки восстановления достаточно локального typed error с именем задачи, ссылкой на сохранённый артефакт, исходной Create error и restore error. `Error()` должен ясно различать «старое определение восстановлено» и «восстановление не подтверждено»; он не должен печатать XML. `errors.Is` и `errors.As` должны находить обе причины. Подход уже есть в `LivenessTaskPostImportError`: поля `Cause`/`Rollback` и `Unwrap() []error`. Можно использовать тот же приём или `errors.Join` с отдельными контекстами; новый общий error framework не нужен [7, 17].

Если restore вернул `nil`, сохраняется исходный Create error; допустимо сообщение о восстановлении на момент подтверждения. Если restore вернул ошибку после фактического применения, результат остаётся неподтверждённым, обе ошибки и артефакт сохраняются. Появление старого XML при последующем чтении не доказывает, кто его создал, и не разрешает очистку.

Контракт этой ветви не меняет существующие компенсации после успешного Create. Поэтому он не доказывает безопасность всего последующего отката и не должен сопровождаться сообщением «вся установка атомарна».

<a id="snapshot"></a>
## 4. Защищённый сохраняемый снимок

### 4.1. Применимое минимальное решение

Предложенная ведущим исполнителем форма — **новый JSON-файл под `DaemonStateDir()` с фиксированным префиксом `scheduler-recovery-` и случайным суффиксом** — подходит для этого среза. Один документ содержит версию схемы, точное имя задачи, время сохранения в UTC, исходные XML-байты и SHA-256 этих байтов. Для Go-поля `[]byte` JSON-кодирование сохраняет байты посредством Base64; кодировка XML не должна проходить через произвольное строковое перекодирование. Файл является снимком до операции, а не журналом доказанного конечного результата.

Согласованная для реализации политика «сохранять при всех исходах» избегает небезопасного удаления файла по повторно разрешённому пути. На успешном заменяющем шаге путь следует показать как ссылку на резервную копию; при Delete/Create/restore error включить в диагностику. На неизвестном исходе никакой `defer os.Remove(snapshotPath)` не допускается. Такая политика создаёт намеренно сохраняемые резервные копии: автоматическая очистка, ограничение их общего количества и UI-индекс не реализованы и не должны подразумеваться.

### 4.2. Уже имеющиеся примитивы

`SecureCreateOwnerOnlyFile` не требует JSON-схемы и создаёт произвольные байты в новом защищённом файле. Windows реализация устанавливает ограничивающий список доступа до записи, выполняет FlushFileBuffers, публикует через rename без замещения, закрывает файл и повторно проверяет DACL. POSIX-реализация создаёт файл с режимом `0600`, выполняет fsync файла и публикует через Linkat без замещения [8–10].

**Семантика результата обязательна:** `created=true, err=nil` — данный вызов создал файл; `created=false, err=nil` — регулярный файл уже существовал и новые байты не записаны. Для recovery-снимка второй исход является конфликтом, а не успехом. Ошибка возможна после фактической публикации, например при close/reopen/verification: replacement нужно остановить; путь считается кандидатом с неподтверждённым сохранением, а не подтверждённым снимком.

Есть важное различие политик: `SecureCreateOwnerOnlyFile` **намеренно обходит parent owner/DACL gate**. `DaemonStateDir()` в Windows сам по себе только создаёт каталог; он не доказывает защищённость родительской DACL. Обычный state writer сначала применяет строгий gate, не relax-ит чужого владельца и не relax-ит родителя в `operatorRequiresSingleUserHome()` [8, 11, 12]. Поэтому выбор прямого OwnerOnlyFile должен быть явным:

- Если recovery-файл обязан соблюдать строгий родительский state-file контракт, использовать `SecureCreateClientConfigIfMissing` — тот же create-new pipeline с включённым parent gate — либо уже существующий соответствующий policy wrapper.
- Если используется generic OwnerOnlyFile, ограничить утверждение приватностью созданного файла и не заявлять соблюдение строгой parent-policy без отдельной проверки. Не делать непрозрачный fallback к обычному os.WriteFile.

Это конкретное различие текущих контрактов, а не требование переписать весь файловый слой. Для нового файла со случайным именем `SecureWriteClientConfig` менее подходит: он намеренно заменяет конечный файл. Новый framework хранения не нужен.

### 4.3. Readback и пределы доказательства

`ReadStateFileBeneathRootNoFollow` читает регулярный файл через цепочку удерживаемых дескрипторов без следования ссылкам, ограничивает размер и сверяет SHA-256. Это доказательство выбранных байтов и способа чтения; его реализация **не проверяет DACL/UID/mode**. Приватность нового файла обеспечивает secure-create, а не наличие слова State в имени reader [13, 14].

Обычный state reader ограничен 1 MiB; backup-reader использует 16 MiB. Лимит следует проверить на **сериализованном JSON**, а не только на исходном XML: Base64 увеличивает размер. Превышение лимита должно останавливать шаг до записи и Delete. Для exact-readback сравнить полный сериализованный документ; отдельно зафиксировать digest исходного XML в самом документе [15].

Успешный fsync/FlushFileBuffers, завершение публикации и точный readback подтверждают сохранённый файл в проверенном сценарии. Этот обзор не подтверждает сохранность после внезапной потери питания; в прочитанном POSIX secure-create нет fsync каталога. Нельзя превращать «снимок доступен после возврата/завершения процесса» в более широкую гарантию аппаратной crash durability.

### 4.4. Рассмотренное повторное использование

В дереве уже есть `RetainedTaskXMLMaterialStore`: физический Windows-владелец XML, созданный для Product V3. Он сохраняет native identity родителя/корня, выполняет write/readback, возвращает RootPath, Load и явный Close; Close означает выбранное удаление, а не простое освобождение idle handles. При сохранении неразрешённого восстановления Close вызывать нельзя [18].

Это подходящий кандидат, если потребуется полноценный управляемый lifecycle XML-каталога. Для текущей политики «один свежий immutable JSON, сохраняемый при всех исходах» generic secure-create проще: не требует Windows-only адаптера, Product V3 namespace, дополнительного .lock и переноса существующего формата refs. Store сам по себе также не создаёт install-специфичный индекс task→artifact. Его строки не следует копировать в новый владельческий слой. Оба варианта оценены; выбранный ограниченный file-примитив уже существует.

<a id="tests"></a>
## 5. Различающая матрица проверок

**Таблица 3 — минимальные модельные критерии. Это требования к тестам, не выполненные результаты.**

| № | Сценарий | Решающий критерий |
|---|---|---|
| M1 | Create отказал до применения; имя свободно | Старые байты восстановлены; один исходный Delete и один recovery-call; original Create error доступен через errors.Is; install не успешен. |
| M2 | После Delete внешняя задача заняла имя; Create отказал | Внешние байты сохранены; recovery вернул типизированный конфликт; дополнительного Delete/ImportXML нет; обе причины и файл сохранены. |
| M3 | Внешняя задача появляется непосредственно перед записью восстановления | Backend conditional-create отказывает на стороне владельца; предварительное чтение отсутствия не позволяет затереть задачу. |
| M4 | Create записал новое определение и затем вернул ошибку | Присутствующие байты сохранены; recovery конфликтует; ошибка не классифицирована как «ничего не произошло». |
| M5 | Restore отказал до применения | Старое определение пока отсутствует; snapshot остаётся доступным; Create и restore errors различимы; нет success/deleted log. |
| M6 | Restore применил старое определение и затем вернул ошибку | Возвращены обе причины; файл сохранён; нет заявления о подтверждённом восстановлении и нет cleanup/retry по имени. |
| M7 | Scheduler без XMLCreator, но старый снимок есть | Ноль Delete/Create/recovery/файловых сохранений; ясная unsupported-capability причина. |
| M8 | Артефакт: create=false,nil; write error до/после публикации; readback mismatch/error | Ни Delete, ни Create не начинаются; конфликт/неопределённость не маскируются успехом; существующий чужой файл не изменён. |
| M9 | Snapshot сериализованного JSON превышает выбранный лимит | Отказ до writer/Delete/Create; существенен именно размер JSON. |
| M10 | Старой задачи не было; Create отказал или конфликтует | Ноль Delete/recovery/snapshot-файлов; исходная ошибка сохранена. |
| M11 | Новый заменяющий Create успешен | Recovery не вызывается; выбранная retain-all политика оставляет корректный JSON и сообщает путь; обычный последующий поток сохранён. |
| M12 | Delete вернул ошибку | Create/recovery не вызываются; snapshot остаётся; ошибка не утверждает неизменность задачи. |
| M13 | Предыдущая задача того же плана успешно завершена, текущая не создалась | Текущий snapshot/ошибки сохраняются при откате предыдущих; новая ветвь не регистрирует текущую опасную closure. |
| M14 | Небезопасный parent при включённой строгой политике, если она входит в выбранный writer-контракт | Отказ до Delete; нет обхода через relax-путь. |
| M15 | Два элемента SchedulerTasks относятся к одному нативному имени | Отказ до scheduler acquisition, файловых сохранений и мутаций; как минимум exact/case/root-prefix варианты различаются тестом. Иначе прежняя closure может удалить текущего внешнего occupant после корректного отказа восстановления. |

Для M1–M6 нужна специализированная модель с операционным журналом и hook-ами на различимые фазы. Existing `fakeScheduler.failCreateForName` моделирует только induced failure, а не разрешение конфликта реальным владельцем. Полезно оставить общий fake нетронутым и добавить локальную обёртку, где и Create, и CreateXMLIfAbsent отказывают при уже существующем имени, а сценарий «применил, затем error» задан отдельно [4, 5].

**Таблица 4 — что доказывает каждый уровень.**

| Уровень | Допустимый вывод | Недопустимый вывод |
|---|---|---|
| Consumer model | Порядок вызовов, сохранённые ошибки/байты, отсутствие Delete/Import в новой ветви. | Атомарность нативного Task Scheduler. |
| Bridge/command fixture | Сериализация, выбранные flags, typed mapping, отсутствие force/interactive path. | Что сама Windows гарантирует те же результаты. |
| Разрешённая нативная проверка | Отказ при реально занятом имени, сохранение нативного определения, импорт поддерживаемого principal/XML в точной среде. | Универсальная восстановимость всех экспортов, credentials, security descriptor и runtime-состояния. |

Снимок в JSON проверяется после возврата из install: схема, имя, точные исходные XML-байты, digest, существующий путь. Проверка только `len(snapshot)>0` недостаточна. Для ошибки восстановления проверяются оба `errors.Is`, а не только фрагмент сообщения. Для неопределённого исхода необходимо проверять отсутствие ложной успешной строки.

<a id="limits"></a>
## 6. Совместимость, простота и открытые границы

Новая операция не должна менять обычные Create и force ImportXML. Linux/macOS backend сейчас возвращают ErrNotImplemented при создании Scheduler; noop backend отвергает мутации. Optional capability сохраняет их текущий method set и не допускает ложного восстановления через no-op [2, 19].

Сохранённый XML не гарантирует нативную восстановимость: экспорт не является доказательством наличия пароля, полного security descriptor или возможности зарегистрировать прежнего principal. Нативная регистрация может нормализовать XML. Модельное побайтовое восстановление подтверждает поток исходных байтов, а не побайтовый вид нового нативного экспорта.

Остаются отдельно открытыми: внешний обмен определения между Export и Delete; неопределённый результат самого Delete; последующие старые rollback-closure с Delete+ImportXML; отказы других компенсаций; полная атомарность install; долговременная индексация и очистка накопленных recovery-файлов; нативные runtime/liveness-свойства задачи. Новая ветвь не закрывает эти вопросы автоматически.

CommandCoordinator сохраняет объект ошибки, но CLI при render error способен заменить install error, а bulk install печатает частные ошибки и возвращает общий счётчик. Сохраняемый JSON делает восстановительные данные независимыми от времени жизни error-объекта; это не означает, что все каналы уже передают обе typed причины. Отдельную CLI-правку ради этого обзора не предлагаю [6].

<a id="verification"></a>
## 7. Проведённая проверка и прочитанные навыки

Проверены исходная ветвь install, соседние error/rollback контракты, реальные method sets Scheduler/stub/noop, реализация Windows Create/ImportXML/COM, существующий fake, snapshot-тесты, фактические secure-create/readback операции и варианты сохранения XML. Чтение GitHub выполнялось по точному commit ref; публикаций и внешних сообщений не было. Запускались только локальные read-only команды просмотра/хеширования; продуктовые тесты, сборка и go vet не запускались.

Полностью прочитаны и применены к обзору:

- `go-project-and-api-design/SKILL.md` и `references/api-evolution.md` — отдельная capability, совместимость method set, отсутствие лишнего framework.
- `review-go-engineering-change/SKILL.md` и `references/finding-standard.md` — конкретные места, условия, последствия и отличие дефекта от неиспытанной гарантии.
- `code-simplification/SKILL.md` из Ponytail — узкий срез, повторное использование существующих файловых примитивов, отсутствие несвязанного refactoring.
- `go-data-consistency/SKILL.md` и `references/isolation-and-ambiguity.md` — применены только общие правила условной записи и неизвестного исхода; новых заявлений о транзакциях баз данных здесь нет.

Репозиторные AGENTS.md прочитаны; в актуальном каноне Orchestrarium дополнительно рассмотрены разделы поведения/контрактов, доказательств, повторного использования, сохранения результатов и ограничений делегирования. Полного формального воспроизведения всех ролей не выполнялось. Самопроверка отчёта: существенные выводы привязаны к исходникам; методы, предлагаемые для новой дельты, не названы уже реализованными; результаты модели и нативной среды не смешаны.

<a id="sources"></a>
## 8. Источники

Ссылки на исходники mcp-local-hub в GitHub закреплены за `c55d9ca619f6f9301ba1113615370fc6218d608f`. Отдельно обозначенные локальные ссылки относятся к проверенному кандидату R21.

1. [internal/api/install.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/install.go), `3740–3785`, `3815–3848`: rollback, snapshot/Delete/Create, текущая compensation closure.
2. [internal/scheduler/scheduler.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/scheduler/scheduler.go): Scheduler, error sentinels, ContextLister.
3. [internal/scheduler/scheduler_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/scheduler/scheduler_windows.go), `204–301`: Create, Delete, ExportXML, force ImportXML, encoding preparation.
4. [internal/api/register_test.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/register_test.go), `3600–3616`, `3686–3753`: исходная модель Scheduler.
5. [internal/api/install_scheduler_snapshot_test.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/install_scheduler_snapshot_test.go): уже выполненный предыдущим срезом snapshot/Delete admission и границы его модели.
6. [internal/api/readiness_command.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/readiness_command.go), `110–119`, `241–255`, `280–294`; [internal/cli/install.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/cli/install.go), `341–354`, `388–402`: error propagation.
7. [internal/api/liveness_task.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/liveness_task.go), `123–151`: existing multi-cause error precedent.
8. [internal/api/secure_create_client_config.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/secure_create_client_config.go), `60–100`: strict-parent и owner-only create-new варианты.
9. [internal/api/secure_create_client_config_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/secure_create_client_config_windows.go), `49–194`: создание, flush, no-replace publication, post-check.
10. [internal/api/secure_create_client_config_posix.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/secure_create_client_config_posix.go), `40–157`: mode, fsync, Linkat, post-check.
11. [internal/api/state_paths_prod.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/state_paths_prod.go), `38–50`; [internal/api/state_paths_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/state_paths_windows.go), `56–72`: canonical state root.
12. [internal/api/state_file_helper.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/state_file_helper.go), `80–164`; [internal/api/secure_write_client_config.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/secure_write_client_config.go), `74–97`: state write policy и atomic replacement.
13. [internal/api/state_read_beneath_root_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/state_read_beneath_root_windows.go), `19–180`: Windows no-follow/checksum readback.
14. [internal/api/state_read_beneath_root_posix.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/state_read_beneath_root_posix.go), `15–166`: POSIX no-follow/checksum readback.
15. [internal/api/state_read_caps.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/state_read_caps.go): 1 MiB / 16 MiB limits.
16. Microsoft Learn: [TASK_CREATION](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/ne-taskschd-task_creation) и [TaskFolder.RegisterTask](https://learn.microsoft.com/en-us/windows/win32/taskschd/taskfolder-registertask), просмотрены 20.11.2026: режимы create/update и регистрационные триггеры.
17. Go: [package errors](https://pkg.go.dev/errors), просмотрен 20.11.2026: errors.Join, errors.Is/As, Unwrap с несколькими причинами.
18. [internal/api/retained_task_material_windows.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/retained_task_material_windows.go), `20–37`, `78–158`, `181–332`: physical XML owner, Retain/Load/Close.
19. [scheduler_linux.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/scheduler/scheduler_linux.go), [scheduler_darwin.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/scheduler/scheduler_darwin.go), [scheduler_noop.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/scheduler/scheduler_noop.go): совместимость платформ и test seam.
20. [Orchestrarium shared/AGENTS.shared.md](https://github.com/applicate2628/Orchestrarium/blob/main/shared/AGENTS.shared.md), актуальный read-only просмотр 20.11.2026; локальный `r20-continuation/public/SCOPE-NEXT.md`: граница переданного среза.
21. [internal/config/manifest.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/config/manifest.go), `1180–1468`; [internal/api/install_parsed_manifest.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/install_parsed_manifest.go), `734–744`; [internal/api/daemon_intent.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/daemon_intent.go), `285–299`: уникальность имён, выбор public execution path и существующая нормализация ведущего разделителя.
22. [API install candidate](https://github.com/applicate2628/mcp-local-hub/blob/ba17867125b3728fb734e90b383639a3ff9175ee/internal/api/install.go), [recovery candidate](https://github.com/applicate2628/mcp-local-hub/blob/ba17867125b3728fb734e90b383639a3ff9175ee/internal/api/install_task_recovery.go), [consumer tests](https://github.com/applicate2628/mcp-local-hub/blob/ba17867125b3728fb734e90b383639a3ff9175ee/internal/api/install_task_recovery_test.go), [storage tests](https://github.com/applicate2628/mcp-local-hub/blob/ba17867125b3728fb734e90b383639a3ff9175ee/internal/api/install_task_recovery_storage_test.go), [snapshot tests](https://github.com/applicate2628/mcp-local-hub/blob/ba17867125b3728fb734e90b383639a3ff9175ee/internal/api/install_scheduler_snapshot_test.go): проверенная локальная дельта API.
23. Microsoft Learn, [MS-TSCH §3.2.5.4.2 SchRpcRegisterTask](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-tsch/849c131a-64e4-46ef-b015-9d4c599c5167), просмотрен независимо 20.11.2026: TASK_LOGON_NONE, выбор principal, TASK_CREATE и 0x800700B7.
24. [Scheduler interface candidate](https://github.com/applicate2628/mcp-local-hub/blob/ba17867125b3728fb734e90b383639a3ff9175ee/internal/scheduler/scheduler.go), [Windows XML candidate](https://github.com/applicate2628/mcp-local-hub/blob/ba17867125b3728fb734e90b383639a3ff9175ee/internal/scheduler/scheduler_xml_create_windows.go), [COM bridge candidate](https://github.com/applicate2628/mcp-local-hub/blob/ba17867125b3728fb734e90b383639a3ff9175ee/internal/scheduler/scheduler_com_windows.go), [Windows XML tests](https://github.com/applicate2628/mcp-local-hub/blob/ba17867125b3728fb734e90b383639a3ff9175ee/internal/scheduler/scheduler_xml_create_windows_test.go): проверенная локальная дельта платформенной реализации.
25. [hardened_tempdir_windows_test.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/hardened_tempdir_windows_test.go), `32–43`; [hardened_tempdir_posix_test.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/hardened_tempdir_posix_test.go); [daemon_intent_test.go](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/api/daemon_intent_test.go), `31–44`: существующий строгий test fixture parent.
26. [check_recovery.py](check_recovery.py), [prepare_source.py](prepare_source.py), [checkout_published.py](checkout_published.py), [source-plan.json](source-plan.json), [source-manifest.json](source-manifest.json): проверенный план из девяти файлов, preparation/execution/checkout scripts и frozen identities.
27. [native-harness/main.go](native-original.go): отдельная ограниченная проверочная программа, не продуктовая дельта. Рассмотренная начальная версия: SHA-256 `1f28a5bfd2a43a1df2edc829085b5ea48732f77c1fc44c8485ca83f2a6ac78ab`; повторно проверенная исправленная версия: `7691f5c760e0b024eb1d45c145d2f729efac9fd3e54daae29d3cc88fa4913c27`.
28. [initial test input](initial-install_task_recovery_test.go), [initial source manifest](initial-source-manifest.json), [initial product patch](initial-product.patch): сохранённые исходные материалы первого запуска и база для независимой проверки поправки assertions.
29. [native_run.py](native_run.py): отдельный wrapper сборки и запуска рассмотренного harness. Повторно проверенный SHA-256 `00410026f1d0191c4d614f6e2b92ffd0c228d15815589f9f92ce4b7318b61c9d`.

<a id="terms"></a>
## 9. Термины и сокращения

- **API — application programming interface:** программный интерфейс между install и платформенным планировщиком.
- **Backend:** реализация операций конкретной операционной системой.
- **Conditional create / create-if-absent:** создание, которое сам владелец состояния отклоняет, если имя уже занято.
- **Capability:** дополнительный контракт конкретной реализации, обнаруживаемый через Go interface assertion.
- **XML — Extensible Markup Language:** формат определения задачи Windows Task Scheduler.
- **JSON — JavaScript Object Notation:** формат предлагаемого recovery-документа.
- **Base64:** обратимое представление произвольных байтов печатными символами; не шифрование.
- **SHA-256 — Secure Hash Algorithm, 256 бит:** контрольная сумма сохранённых байтов; не подтверждение владельца задачи.
- **DACL — discretionary access control list:** список разрешений доступа к объекту Windows.
- **UID — user identifier:** числовой идентификатор владельца в POSIX-системах.
- **UTC — Coordinated Universal Time:** временная шкала для отметки сохранения снимка.
- **MiB — mebibyte:** 1 048 576 байт.
- **Typed error:** значение ошибки с программно доступными полями и цепочкой причин.
- **Unknown outcome:** вызов вернул ошибку, а факт применения эффекта не установлен.
- **Readback:** повторное чтение сохранённого результата для проверки фактических байтов.
- **CLI — command-line interface:** интерфейс командной строки.
- **PR — pull request:** запрос на включение изменений.
- **COM — Component Object Model:** нативный компонентный интерфейс Windows, используемый для Task Scheduler.
- **HRESULT:** 32-битный код результата нативного вызова Windows; сообщение исключения не заменяет этот код.
- **VARIANT:** типизированное значение, передаваемое через COM Automation; отображение PowerShell `$null` проверяется отдельно.
- **UTF-8 / UTF-16LE:** способы кодирования Unicode; LE означает little endian, порядок младшего байта первым.
- **AST — abstract syntax tree:** абстрактное синтаксическое дерево; его построение проверяет синтаксис без выполнения проверяемого скрипта.

<a id="candidate-api"></a>
## 10. Независимое ревью кандидата API

**Вывод статического ревью: подтверждённых новых блокирующих дефектов в выбранной ветви не найдено после добавления проверки повторяющихся имён.** Это утверждение относится к файлам [22] с хешами из таблицы 5. Ревьюер не запускал их тесты и не присваивает результатам статус PASS.

### 10.1. Исправленная граница общего rollback

До дополнительной проверки имён оставался конкретный опасный вход: два элемента `p.SchedulerTasks` могут обозначать одну задачу. Первый успешный Create добавляет прежнюю closure с Delete+ImportXML; ошибка второго Create вызывает безопасный CreateXMLIfAbsent, а затем `failAfterRollback` выполняет первую closure. Если имя к этому моменту занял внешний участник, эта closure удаляет его. Следовательно, безопасность новой текущей ветви зависела также от уникальности всех предыдущих целей.

В `ServerManifest.Validate` нет общего запрета одинаковых daemon names, case aliases либо daemon с именем `weekly-refresh`. `BuildPlanWithOpts` конкатенирует имя manifest и daemon, а weekly-refresh добавляет отдельным элементом. Таким образом, некорректный для native executor план построить возможно. При этом обычный public install в текущем коммите через `selectGlobalInstallExecutionPhase` для `!dryRun` устанавливает `SkipSchedulerTasks=true`. Поэтому обнаруженное нарушение доказано на контракте native executor; массовую достижимость из текущего обычного CLI этим обзором не утверждаю [1, 21].

В текущем кандидате `install.go:3695–3706` проверка выполняется при `actions.createEligible`, до notices, `newScheduler`, разрешения runtime path и любых rollback-эффектов. Сравниваются имена после удаления одного начального `\`, с `strings.EqualFold`. Это закрывает exact, обычные case и root-prefix aliases, заданные в `TestExecuteInstallToRejectsAliasedRecoveryNames:246–271`. При `skipTasks=true` новый отказ не вводится. Парное сравнение подходит небольшому явному плану и не требует нового реестра или неподтверждённого Unicode canonicalizer.

Существующий `canonicalIntentTaskKey:291–298` добавляет только начальный `\`; он не реализует сравнение без учёта регистра и не заменяет данный guard. Полное совпадение Go Unicode case folding с нативными Windows правилами для всех Unicode имён в этой работе не квалифицировано. Отдельные старые rollback closures других задач по-прежнему обладают собственной известной опасной семантикой; новый guard не делает их условными [21, 22].

### 10.2. Порядок, ошибки и сохранённый материал

`install.go:3839–3869` сначала проверяет optional capability и подтверждает сохранение снимка. Только затем выполняется Delete. При Create error текущая задача не включается в прежний rollback: выполняется ровно один `CreateXMLIfAbsent` с экспортированными байтами, а `InstallTaskRecoveryError` передаётся через существующий `failAfterRollback`. Старая задача, отсутствовавшая при ExportXML, остаётся в отдельной ветви без Delete, восстановления и recovery-файла [22].

`InstallTaskRecoveryError:19–38` сохраняет исходную Create error при подтверждённом восстановлении; при отказе восстановления `Unwrap() []error` содержит обе причины. Сообщение раскрывает путь и не печатает XML. Ошибка восстановления не сопровождается фразой о подтверждённой регистрации. Структура создаётся production-ветвью с ненулевой `Cause`; nil-Cause zero value не является наблюдавшимся исполнением этого сценария.

`saveInstallTaskSnapshot:62–95` ограничивает как вход, так и сериализованный JSON, использует `SecureCreateClientConfigIfMissing`, требует `created=true`, затем выполняет no-follow readback с SHA-256 всего документа. Родительская проверка сознательно не обходится. Ошибка после возможной публикации возвращает путь, прекращает допуск Delete и не удаляет файл; занятый файл не принимается как собственный снимок. Выбранная политика retain-all соблюдена и после успешной замены [8–15, 22].

Новые тесты используют `hardenedTempDir(t)` и `SetDaemonStateRootForTest`. Это минимальный существующий корректный fixture: Windows helper применяет защищённый DACL, POSIX helper выставляет 0700. `daemonIntentTestHelper` уже использует этот helper; обычный `t.TempDir` нельзя самостоятельно считать доказательством прохождения Windows parent gate [25].

Отдельная копия `priorXML` в executor не добавлена. Реальный Windows ExportXML в данном исходнике возвращает свежий `[]byte` из COM-строки, поэтому мутация чужого внутреннего буфера здесь не является подтверждённым production-дефектом. Если контракт потребуется распространять на backend с заимствованным изменяемым буфером, потребителю нужна собственная копия до Delete. Сериализованный recovery-файл уже независим от последующего времени жизни среза [3, 22].

### 10.3. Что различают написанные тесты

Матрица consumer-теста явно содержит: отказ Create до применения; занятого внешнего участника; применившийся Create с потерянным ответом; отказ восстановления; применившееся восстановление с потерянным ответом. Проверяются сохранённые байты/наличие задачи, обе `errors.Is`, один исходный Delete, один recovery-вызов, отсутствие ImportXML/Run и ложного install success. Для occupied-состояния используется отдельный create-only метод модели, не исходный replacing ImportXML [22].

Сценарии внешнего участника и применившегося Create различаются записываемыми значениями, но используют один hook `createWrites`. Это доказывает сохранность occupant при любом его происхождении; реальную межпроцессную гонку этот consumer-тест не моделирует. Отдельно проверены отсутствие capability, существующий снимок до Delete, отказ Delete, непригодное хранилище, отсутствие старой задачи, коллизия recovery-файла, UTF-8/UTF-16LE байты и лимит сериализованного документа. Новый consumer-тест не инъецирует ошибку именно после публикации recovery-файла или на readback; эти ветви прочитаны статически, дополнительный PASS им не приписывается.

**Таблица 5 — SHA-256 рассмотренного API-кандидата.**

| Файл внутри `r21-recovery/candidate/internal/api` | SHA-256 |
|---|---|
| install.go | `b9a8d59d56691c8d5edf5b96816cd54087ab1b10db592ddb9ef42367ca541bce` |
| install_task_recovery.go | `edfd3bf0428534f931019a0b75b101b60ccce2ac257c8ab11e0947ebba444510` |
| install_task_recovery_test.go | `7f7afc5ff0f497d2384a9378f287e9930f65468650cb6d91f933664d41ad3b37` |
| install_task_recovery_storage_test.go | `8899bf5ccb5eacef5c66bebdeeaf091aa26b91fffe0ab9b49aa3edb8cfecd78f` |
| install_scheduler_snapshot_test.go | `e61b2b0e7e41a39148d779326d6d1c193eb061ffa379a6a72185e581d0d30177` |

<a id="candidate-scheduler"></a>
## 11. Независимое ревью кандидата Windows Scheduler

**Вывод статического ревью: подтверждённых блокирующих дефектов в рассмотренном платформенном кандидате не найдено.** Проверены новый метод целиком, финальная phase-gated классификация COM-ошибок, interface addition, существующее преобразование кодировок и все новые platform tests. Нативная регистрация и эти tests ревьюером не запускались. Идентичность рассмотренных файлов приведена в таблице 6.

### 11.1. Нативный контракт и данные

Ветка `create_xml_if_absent` размещена в COM script до прежнего GetTasks/absence gate. Она вызывает только `RegisterTask` с флагами 34: `TASK_CREATE` и `TASK_IGNORE_REGISTRATION_TRIGGERS`. Ни UPDATE, ни force-import, ни Delete/Run/Enable в ней нет. Следовательно, допуск создания делегирован владельцу task store, а не предварительному списку имён [24].

Независимо просмотренная спецификация MS-TSCH предписывает отказ 0x800700B7 при занятом пути и TASK_CREATE без UPDATE. TASK_LOGON_NONE выбирает тип входа из XML; явные credentials имеют приоритет перед XML principal. Эта документация обосновывает выбранные аргументы, но не подменяет проверку того, как фактический PowerShell marshals `$null` в COM VARIANT [23].

Сохранение raw XML в API и транспорт в backend имеют разные проверяемые свойства. API хранит исходные байты. Backend принимает UTF-8 с необязательным маркером либо валидный UTF-16LE с маркером, повторно использует `prepareTaskXMLForImport`, затем передаёт UTF-16LE через Base64 в JSON stdin фиксированного скрипта. Strict decoder создаёт COM-строку без пересборки TaskSpec и без удаления неизвестных XML-узлов. UTF-8 → UTF-16LE меняет представление байтов, сохраняя Unicode-текст; native re-export может дополнительно нормализовать определение. Инвариант не следует называть «побайтово неизменный native XML» [3, 24].

Проверка literal root name ограничивает форму входа нового метода. Имя и XML не интерполируются в PowerShell-код. Invalid UTF-8, odd-length UTF-16LE, broken surrogate pairs, unsupported UTF-16BE и пустое содержимое отклоняются до dispatch. Старые Create/ImportXML не изменены.

### 11.2. Ошибки и неопределённость

`Get-NativeHRESULT` проходит цепочку InnerException, выбирает COMException HRESULT и не классифицирует ошибки по локализованному тексту. Финальный catch относит 0x800700B7 к конфликту только в `xml_register`; преждевременный аналогичный код при соединении остаётся unavailable. Access denied выделяется отдельно, XML decode и известные schema-коды помечаются corrupt. Прочие HRESULT сохраняются в диагностике с фазой и возвращаются как unavailable [24].

`schedulerCOM` задаёт ограничение времени. Transport/process/protocol failure и отсутствие объекта результата после RegisterTask не превращаются в успех. Новый Go helper явно сообщает uncertain outcome для ErrUnavailable; повторной регистрации, удаления или импорта не выполняет. Это согласовано с потребителем, который при любом recovery error сохраняет артефакт и обе причины. Положительный ответ подтверждает регистрацию, но не исполнение и не готовность задачи.

Новые bridge tests заменяют всю строку создания/соединения COM объекта до запуска PowerShell. Они проверяют fixed-script transport, flags, исходный Unicode, вложенную COMException, native codes, malformed encoding и null registration result, не соединяясь с Task Scheduler. У финальной защиты от conflict classification на connect-phase нет отдельного нового regression-теста; guard проверен чтением исходника, этот пробел не назван runtime-дефектом.

### 11.3. Оставшаяся квалификация и границы утверждения

Требуются отдельные разрешённые наблюдения на реальном Windows владельце: registration при отсутствии, refusal и сохранение visible/hidden occupant, обычный case alias, фактический current-user InteractiveToken round trip и `$null` marshalling, Unicode transport и lost-reply handling. До них модельный успех и успешная компиляция не доказывают весь native integration path.

Произвольные service/group/other-user principals, исходные пароли, точный security descriptor, все варианты Unicode alias, enabled time triggers и runtime readiness не квалифицированы. Снимок остаётся полезным при отказе восстановления, однако не делает XML достаточным для восстановления этих дополнительных состояний. Новая возможность является best effort восстановлением определения с гарантией create-only выбранного нативного примитива; полной транзакцией установки она не является.

**Таблица 6 — SHA-256 рассмотренного Scheduler-кандидата.**

| Файл внутри `r21-recovery/scheduler-candidate/internal/scheduler` | SHA-256 |
|---|---|
| scheduler.go | `907449ab2be563539dd29179a00a7b68fa5cead9f89376169e521bd9df9123c5` |
| scheduler_com_windows.go | `bd3cc13ea5fd4152597e8e69963bfcc58553820245f31113b51d731954532efd` |
| scheduler_xml_create_windows.go | `eafbae884b9516385206004c2255fd63b23a0a7e07f0179c93a66218817f1ba1` |
| scheduler_xml_create_windows_test.go | `062b4671cd846bc4d06ec684f6d6b7bc3f4b05a8265840f4950dba20f14704ed` |

`scheduler_windows.go` в этом кандидате не отличается от baseline; его SHA-256: `c812e1b25e1b2f80c1d95f94bcae36cb6752a6e3320d0e856f8193036d1f2409`. Сопоставление выполнено локальным `git diff --no-index` и SHA-256 чтением, без изменения git branch/index или файлов продукта.

<a id="candidate-runner"></a>
## 12. Независимое ревью runner и frozen source manifest

**Вывод: подтверждённых блокирующих дефектов в рассмотренном конкретном плане запуска не найдено.** Все три Python-скрипта и JSON-план [26] прочитаны полностью. Выполнены только `ast.parse` и независимые вычисления над прочитанными файлами; ни один reviewed script не импортировался и не исполнялся. Все три файла синтаксически допустимы. Проверки продукта и нативные операции ревьюером не запускались.

### 12.1. Идентичность исходников и переходов

`prepare_source.py` генерирует manifests и patches из явного списка девяти путей, отвергает повторения, неподходящие пути, неизменённые файлы и существующие выходы. Контрольная дельта состоит только из `install_task_recovery_test.go` и `install_scheduler_snapshot_test.go`; её тесты не ссылаются на новые product symbols. Остальные изменения приходят отдельным product patch. Поэтому negative control можно скомпилировать на точном c55d9ca baseline, а последующий candidate проверяет реальную полную дельту [26].

`Runner.begin` проверяет исходный коммит/дерево, SHA-256 runner/generator и frozen patches. `admit` проверяет HEAD, отсутствие staged изменений, точный набор tracked/untracked/ignored статусов и байты всех девяти путей. Candidate допускается после retained negative-control receipt, сверки его исходных stdout/stderr и повторной проверки точного oracle. Не требуется снова запускать уже подтверждённый negative control. Конечный source state проверяется до признания фазы complete и повторно в finish.

Независимая реконструкция unified diff из прочитанных base/candidate bytes подтвердила все три patch-файла. Проверены SHA-256 и Git blob каждого source entry, совпадение перенесённых Scheduler-файлов с рассмотренной отдельной копией, generator/runner/source-plan pins. Полученный список расхождений пуст. Это проверка данных и pin-цепочки, не исполнение scripts.

`checkout_published.py` сначала допускает candidate, staged tree всех девяти файлов сравнивает с ожидаемым published tree, затем читает из фиксированного репозитория точный commit. Перед detached checkout проверяет единственного родителя c55d9ca, tree и полный список изменённых путей. После checkout повторно допускает published состояние. Merge, rebase, cherry-pick и интеграции ветвей в коде этого wrapper нет. Published execution дополнительно сверяет publication-input с его исходным tree и source-manifest; соответствие всей публикации ещё не наблюдалось ревьюером [26].

### 12.2. Точный oracle результатов

Runner принимает результат test command только при settled exit с требуемым кодом, отсутствии timeout/capture/source errors и точном совпадении multiset terminal events. Незапланированные тесты, пропущенные случаи, дубликаты/недостаточные повторы, другая package и skip отвергаются. Для negative control дополнительно нужны конкретные behavioral markers из выбранного теста: сам по себе exit=1 не считается доказательством старого дефекта. Syntax/build failure не может заменить ожидаемые тестовые FAIL [26].

**Таблица 7 — ожидаемые события плана, не результаты запуска.**

| Фаза | Ожидаемые test terminal events без package | Отдельное package event |
|---|---:|---|
| control / API | 15 FAIL, 0 PASS, 0 SKIP | 1 FAIL |
| candidate / API | 42 PASS, 0 FAIL, 0 SKIP | 1 PASS |
| candidate / Scheduler | 42 PASS, 0 FAIL, 0 SKIP | 1 PASS |
| adjacent / API | 38 PASS, 0 FAIL, 0 SKIP | 1 PASS |
| published / API | 42 PASS, 0 FAIL, 0 SKIP | 1 PASS |
| published / Scheduler | 42 PASS, 0 FAIL, 0 SKIP | 1 PASS |

Положительные количества учитывают два повтора и родительские test events вместе с subtests. JSON-структуры candidate/published suites совпали точно. Числа не означают количество независимых сценариев и до фактического выполнения не являются PASS-утверждением.

### 12.3. Побочные эффекты и завершение

Fixed Windows task root расположен под `<WORK_ROOT>`; используются существующие toolchain/cache, package download и Go telemetry выключены. Runtime directories направлены в выделенную область; Git hooks/interactive credentials отключены. Reviewed unit suites используют fake Scheduler/подменённое COM connection statement. Флаг `native_scheduler_operations=false` и переменная окружения не названы системным запретом native calls: основание — именно прочитанный bounded suite, а не мнимая sandbox-гарантия.

Каждая evidence directory создаётся только заново; JSON receipts и log-файлы открываются в exclusive-create режиме. Скрипты не перезаписывают результаты прежнего запуска и не очищают их автоматически. При deadline retained `Popen` handle используется только для прямого дочернего процесса. Нет PID re-resolution, taskkill, prefix/process-name termination либо вывода о завершении всего дерева процессов. Потомки и возможные поздние log writers явно остаются unresolved; такая фаза не получает complete. Следует сохранять runtime/evidence до разрешения этой неопределённости.

**Таблица 8 — проверенные SHA-256 runner artifacts.**

| Артефакт | SHA-256 |
|---|---|
| source-manifest.json | `f3e12a0bed9f4024e4e00734eb35da859c23defc86c8b0963ac9469d80890534` |
| check_recovery.py | `6f69fc636f8ec47b725f07238f7ea2180cee9fb8efbd8e8a7275f4665ea87e61` |
| prepare_source.py | `bdbfc30cc3554d792e5b09df5fd9912cbe62a6a46b5651e9b55fdefb37f1831f` |
| checkout_published.py | `7dd36a8c3daaf38729bd39a47f5ee2feafca259561a79d58d9ec0c2461a35e98` |
| source-plan.json | `9d40c8e4ecf23d11653acf13a677fb147d75291d25ca5aac8677b413175e8238` |
| control.patch | `38af6db6234bad8eff83283b4044614027cc490aa8e6d5537615c5a3ecd1448b` |
| product.patch | `13a064fecffb545388a9fdbaa054283f59d4b3cb5bc74c430b2b81c2c908e7e0` |
| candidate.patch | `c0d88845076887b04d4a55da35c57dc6df49511f68575fad776f0a2dbfed4c2d` |

### 12.4. Поправка двух assertions после первого запуска

Первый Windows-запуск candidate/API не прошёл. По сообщению ведущего исполнителя, было 26 test PASS, 16 test FAIL и отдельный package FAIL; 12 ошибочных assertion outputs имели только markers `RECOVERY-DIAGNOSTIC` и `RECOVERY-DELETE`. Ведущий не наблюдал сообщений об ошибках проверок состояния, причин, снимка или количества операций. Эти числа переданы ведущим; данный независимый этап проверял поправку исходников и pins, без повторного запуска suite и без самостоятельного просмотра удалённого raw log. Первый запуск не обозначен как PASS.

Причина подтверждается сравнением исходников: продукт печатает путь в error через `fmt` с `%q`, то есть в кавычках с экранированием обратных слешей. На Windows такая строка не содержит исходный неэкранированный путь как непрерывную подстроку. В `install_task_recovery_test.go` изменены ровно три места: добавлен import `strconv`; два error-path assertions теперь ищут `strconv.Quote(path)`. Plain-path assertion для успешного stdout, проверки обеих причин, содержимого snapshot, состояния scheduler, количества операций и отсутствие ложного success не изменены [22, 28]. Это исправляет представление ожидаемого пути и не ослабляет поведенческий oracle.

Сохранённый исходный тест имеет SHA-256 `44c7ab0aa8b266b0a9c0e2e5b5590620fdc7ce83c769265bc7a92f31648a9583`; исправленный — указан в таблице 5. В manifests изменился только source entry этого test-файла. Base commit/tree, набор девяти путей, source plan и все suites остались прежними. Product patch побайтово совпал с сохранённым исходным; изменились только control patch, полный candidate patch и связанные pins. Независимая повторная реконструкция всех source SHA-256/Git blobs и трёх patches, а также проверка generator/runner/source-plan pins дали пустой список расхождений.

Таблица 8 обновлена до исправленного frozen manifest `f3e12a0b…`. Исходный manifest `058dc446…` и его inputs оставлены в diagnostics [28]; прежние результаты не переименованы в результаты новой дельты. Ведущий повторил квалификацию с исправленными assertions и при передаче следующего этапа сообщил о сохранённых проверенных receipts: candidate 84 PASS, adjacent 38 PASS, build/vet завершены. Ревьюер данного отчёта не запускал эти проверки и не проводил отдельную проверку удалённых raw logs; это атрибутированный результат ведущего, а не независимо полученный runtime PASS.

<a id="native-harness"></a>
## 13. Ревью ограниченного нативного harness

Ведущий исполнитель отдельно задал проверку двух свежих отключённых fixture names и необязательную очистку `--cleanup-owned` после всех успешных проверок. Принято явное условие отсутствия параллельных внешних записей именно в эти тестовые имена. Оно ограничивает допустимый вывод и не превращает чтение перед Delete в атомарное условное удаление. Ревьюер прочитал 337 строк версии [27] и не запускал эту программу.

### 13.1. Граница действий

Заданы ровно два полных имени `\MCPHub-R21-Native-e8b3694319384cb5af9cf33f6b1396ef-U8` и `\MCPHub-R21-Native-e8b3694319384cb5af9cf33f6b1396ef-U16`. Для каждого отдельно объявлен lowercase alias, используемый только в create-only conflict attempt. Программа не перебирает задачи по префиксу для удаления. Output directory фиксирован под `<WORK_ROOT>`, существующая директория не используется повторно; action path имеет значение `NEVER-EXECUTE-...exe`, его отсутствие проверяется, payload-файл программой не создаётся [27].

Оба XML определяют текущий SID, InteractiveToken, LeastPrivilege, `Enabled=false`, `AllowStartOnDemand=false`, пустые Triggers и Hidden=true. В harness отсутствуют Run, Stop и Enable. После создания проверяются principal, связка Action Context, команда, аргументы с Unicode, WorkingDirectory, marker, URI, отсутствие триггеров и отключённое runtime state. Это ограниченная квалификация регистрации; проверка LastRunTime/списка instances в этой программе отсутствует, так что отдельного наблюдения «никогда не было попытки исполнения» она не выдаёт.

### 13.2. Намерения, неопределённость и очистка

Manifest с обоими именами записывается эксклюзивно и Sync выполняется до вызовов, способных зарегистрировать задачу. Create/conflict/delete attempts добавляются в JSONL с Sync до соответствующей операции. Invalid-byte вызовы используют два конкретных входа, для которых рассмотренный Go helper возвращает ошибку до COM dispatch. Начальные имена проверяются на отсутствие, но решение о занятости всё равно принимает TASK_CREATE на стороне планировщика.

Ошибка первоначальной регистрации, ExportXML, верификации или записи evidence возвращает управление до cleanup. При unknown outcome нет Delete и повтора. Expected occupied-name refusals разрешают продолжение лишь при `ErrTaskAlreadyExists` и побайтовом совпадении полного native re-export до/после. Для cleanup требуется ровно два записанных успешных создания; допускаются только перечисленные имена, повторный полный export hash, marker/URI/principal/settings, отключённый runtime state и SID из Status. После Delete выполняется наблюдение отсутствия; ошибка прекращает дальнейшую очистку. Это соответствует заданной ограниченной политике при её явном предположении о внешних writers [27].

### 13.3. Замечания по рассмотренной начальной версии

**Таблица 9 — findings и границы harness evidence.**

| Класс | Место и условие | Следствие и минимальное действие |
|---|---|---|
| Подтверждённая неточность receipt, P2; исправлена | Начальная `main.go:158–165`: CreateXMLIfAbsent вернул nil, следующий emit завершился ошибкой; `state.Created++` ещё не выполнен. | Final `created_tasks` мог не учесть подтверждённое создание. В повторно проверенной версии счётчик увеличивается сразу после nil Create result, до emit (`158–166`); emit error по-прежнему возвращает отказ до cleanup. |
| Ограничение подтверждения записи; helper исправлен | Начальная `writeNewSynced:309–318`: Sync errors учитывались, Close errors игнорировались. | В повторно проверенной версии named return + defer возвращает Close error, если более раннего Write/Sync error нет (`311–325`). JSONL каждый раз синхронизируется через Sync; deferred log.Close не является отдельным подтверждённым lifecycle receipt. |
| Ограничение сценария | Оба положительных входа конструируются через taskXML; native ExportXML используется для сравнения/сохранения, но не повторно передаётся на свободное имя. | Можно квалифицировать native registration, null-principal marshalling, encoding, hidden occupant и обычный case conflict. Нельзя назвать это испытанием untouched ExportXML → CreateXMLIfAbsent round trip. Для такого утверждения требуется отдельный явно спроектированный случай; расширение имён/удалений этим ревью не предлагается. |
| Требование к внешней привязке evidence | Manifest записывает baseline commit, но не хеш кандидата, harness source и исполняемого файла. | Внешний build/run receipt должен до запуска связать frozen nine-file manifest, harness source hash, exact compiler invocation и executable hash, затем сохранить stdout/stderr/exit. Сам native manifest этого доказательства не содержит. |

Повторное ревью исправлений выполнено по SHA-256 `7691f5c760e0b024eb1d45c145d2f729efac9fd3e54daae29d3cc88fa4913c27`. Независимое обратное применение только двух описанных изменений в памяти воспроизвело исходный хеш `1f28a5bf…`, что подтверждает отсутствие иной дельты между этими просмотренными версиями. Контроль мутаций, два имени и синтетические входы не изменены.

Подтверждённого выхода за заданную destructive scope и оставшихся блокирующих замечаний в исправленной версии не найдено при явном fixture-exclusivity предположении. Native runtime outcomes и внешняя source/executable binding ревьюером не наблюдались; runtime PASS этим разделом не заявляется. При дальнейших изменениях harness нужна сверка новой дельты и её хеша перед исполнением.

<a id="native-wrapper"></a>
## 14. Ревью native build/run wrapper

**Вывод статического ревью: оставшихся блокирующих замечаний в wrapper [29] с SHA-256 `00410026f1d0191c4d614f6e2b92ffd0c228d15815589f9f92ce4b7318b61c9d` не найдено.** Все 138 строк первоначально переданного варианта прочитаны и сопоставлены с фактическим JSON-производителем — harness `7691f5c…` из раздела 13. Затем прочитана финальная дельта учёта запуска. Выполнен AST-only parse, без импорта и исполнения wrapper.

### 14.1. Сборка и привязка источников

Wrapper повторно использует рассмотренный Runner и допускает точный candidate с девятью файлами. Хеш оригинального harness задан явно; форматируется только новая эксклюзивно созданная копия вне SOURCE. Go overlay связывает виртуальный `cmd/r21-native-qualification/main.go` с этой копией. Если каталог назначения, бинарный файл или каталог результатов уже существует, либо build materializes виртуальный каталог, native launch отклоняется. Fallback с записью harness в source tree отсутствует [26, 29].

После offline build снова проверяется candidate. До запуска сохраняется `native-launch-inputs.json`, связывающий SHA-256 девяти source files, frozen source manifest, исходный и отформатированный harness, overlay, executable и wrapper. После завершения повторно сверяются входы и source state. Таким образом, описанный в таблице 9 пробел внешней привязки закрывается механизмом wrapper; фактические построенные executable bytes и его runtime receipt этим статическим ревью ещё не наблюдались.

Окружение сборки наследует ограничения Runner: существующий local toolchain, downloads выключены, module mode readonly, отдельные runtime paths под D:. Для единственного вызова harness `MCPHUB_E2E_SCHEDULER` меняется с `none` на пустую строку; прежнее значение возвращается в finally. Native операция явно отделена от unit suites, у которых native flag остаётся false.

### 14.2. Совпадение с фактической JSON-схемой

В harness `errorText(nil)` возвращает пустую строку. Две проверки wrapper для `create_result.error` и `conflict.export_error` корректно требуют `""`, а не JSON null. Это устраняет producer/schema mismatch до native execution. Final object сверяется целиком, включая точные типы bool/int, два acknowledged создания, два подтверждённых удаления и успешную cleanup.

Данные JSONL должны совпасть с stdout до final object. Wrapper проверяет manifest identity, два имени и их lowercase aliases, planned disabled/hidden/no-trigger policy, точные operation counts, успешные исходы, отсутствие после preflight/cleanup, пары exact/case conflicts, хеши исходных входов и полного XML. Сохранённый UTF-16 файл декодируется обратно в UTF-8 и сверяется с хешем original COM text, что связывает два сохранённых представления [27, 29].

**Таблица 10 — ожидаемый успешный native transcript, не наблюдённый результат.**

| Операция | Число JSONL events |
|---|---:|
| preflight | 2 |
| invalid_bytes | 2 |
| create_attempt | 2 |
| create_result | 2 |
| export_observation | 2 |
| conflict_attempt | 4 |
| conflict | 4 |
| cleanup_attempt | 2 |
| cleanup_complete | 2 |
| complete | 1 |
| Всего | 23 |

После этих 23 записей ожидается один final object только в stdout. Counts относятся к наблюдениям pinned harness, не к трассировке системных вызовов. Unknown/retained/unconfirmed операции, stderr, иной count, ошибка exit или несовпадение evidence исключают complete.

### 14.3. Исправленный учёт неопределённого запуска

В исходном wrapper `9fae0e9f…`, строка 114, запуск считался наблюдённым только через возвращённую переменную row или существующий `native-harness.launched.json`. Подтверждённый control-flow gap: Runner.run добавляет row с PID в `runner.receipt["commands"]` до сохранения последующего command receipt. Если postlaunch evidence write завершается ошибкой и файл launched не удалось создать, присваивание row в wrapper не выполняется. Старое выражение тогда могло записать false, хотя процесс уже запускался и native outcome мог быть неопределённым.

В окончательной версии проверяется также PID сохранённой в памяти command row с точным label `native-harness`. Если надёжного PID/launch receipt нет после попытки запуска, оба флага получают null и явное unresolved explanation, а не false. Process lookup и повторное разрешение PID не добавлены. Обратная замена только этого блока в памяти воспроизвела исходный просмотренный хеш `9fae0e9f4311915ead5f7b49862da03e6a7b73e5ca4c62418164b6d869c314c1`, подтвердив, что иной дельты между рассмотренными вариантами нет.

Прямой дочерний процесс имеет 600-секундный deadline и наследует рассмотренный механизм retained-handle termination. Потомки не объявляются завершёнными. Wrapper не выполняет отдельный task cleanup при ошибке, не повторяет native запуск и не расширяет два разрешённых fixture names. Все ограничения синтетических входов и неатомарной cleanup при отсутствии внешних writers из раздела 13 сохраняются.

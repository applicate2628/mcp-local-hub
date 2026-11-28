# Исправление DATA-parser: имена Go tests в UTF-8

## Наблюдавшийся отказ

Root выполнил исходный pinned parser `4d5422763fc3a3f55fd52bc14506aa10fb669c8b3eb491661e593696e7db6414` над точными восстановленными historical CLOSED CLI inputs. DATA replay завершился `UnicodeDecodeError` в строке 108: ASCII decoder встретил byte `e2` в позиции 68. Папка результата не была создана. Go/native tests не запускались. Исходные восемь файлов `old-cli-budget-analysis-v1` сохранены без изменений.

[original-data-failure.json](original-data-failure.json) сохраняет безопасную фактическую проекцию сообщения root. Полный traceback не выдуман и в этот supplement не включён; отдельной native custody receipt для DATA replay не создаётся.

Причина подтверждена исходным pinned stdout: frame 1981, физическая строка 2425, subtest `TestSupervisorPIDIsLiveMcphubSupervisor_OwnerSIDGate/same_owner_SID_→_kill_proceeds`. Стрелка `→` — Unicode U+2192, bytes `e2 86 92`. Это корректное имя вложенного Go subtest, а не повреждение capture.

## Точная правка

[Два изменённых выражения](ascii-to-utf8.patch) декодируют test name как **строгий UTF-8**. Event labels RUN/PAUSE/CONT/PASS/FAIL/SKIP и числовые durations остаются ASCII. Регулярные выражения SYN frames, root/subtest separation, original closure и все input SHA predicates побайтно не изменены. Невалидный UTF-8 не игнорируется.

[Parser v2](parse_closed_cli_durations_v2.py): 9156 bytes, SHA-256 `7f623cc124d2aa04432c694d94007d39ebf6563a13a3944f2efc30a125bb13c9`. Применение diff к exact v1 даёт эти же bytes; новое имя файла не меняет код.

## Ожидаемый DATA readback

В момент подготовки supplement Python v2 ещё не запускался. Ожидаемые значения взяты из ранее выполненного V8-разбора:

- 7642 SYN frames; 1599 root terminals = 1585 PASS + 14 SKIP;
- 299,30 напечатанных секунд; 14 PAUSE, 0 CONT; 374 roots без RUN;
- `top-level-terminal-ledger.json`: **456870 bytes**, SHA-256 **`6a687a7e45efcf069e46e0bbafd36edcf09247e8f09e49467c59278925c34ea8`**.

Все 1599 root names в ledger состоят из ASCII; исправление nested UTF-8 имени не меняет сериализацию этого ledger. Root должен сравнить фактический результат v2 с указанным точным SHA.

```text
python parse_closed_cli_durations_v2.py --root-receiving old-root-receiving.json --receipt old-cli-receipt.json --stdout old-cli-stdout.txt --output replay-v2-new
```

Имена входов произвольны; обязательны исходные pinned bytes из v1. Выходная папка новая. Этот supplement исправляет DATA-инструмент и сохраняет его исходный отказ. Исторический Go результат остаётся FAIL/CLOSED; текущий UNKNOWN и его captures/Source/cache не читались и не изменялись.

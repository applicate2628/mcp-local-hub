# Воспроизведение модели и чтение Windows-результата

Модель использует обычный Python и существующие файлы репозитория. Она не запускает
дочерний OS-процесс, native owner, Go, Rust или прежнюю попытку. Runtime adapter
остаётся отключённым. Windows-результат ниже является уже сохранённым отдельным
результатом; команды модели не выдают новый native-допуск.

[Подготовка](#1-подготовка-отдельной-модели) ·
[Model tests](#2-два-model-test-файла) ·
[Windows Go-result](#3-сохранённый-windows-go-result) ·
[Evidence](#4-идентичности-evidence)

## 1. Подготовка отдельной модели

Выберите существующий checkout, содержащий этот пакет #664, и новую пустую
временную папку. На Windows используйте разрешённую область `D:\dev\ChatGPT`.
Новая ВМ, WSL-среда и загрузка зависимостей не нужны.

В примере `repo` — путь checkout, `replay` — новая папка модели. Это только
копирование и проверка DATA-файлов; исходный checkout не изменяется.

```python
from pathlib import Path
import hashlib
import json

repo = Path(r"<repository>")
replay = Path(r"<new-empty-model-directory>")
bundle = repo / "docs/modernization/verification/failed-owner-receiving-20261126"
pins = json.loads((bundle / "artifact-manifest.json").read_bytes())["files"]

def read_pinned(relative):
    raw = (bundle / relative).read_bytes()
    assert len(raw) == pins[relative]["bytes"]
    assert hashlib.sha256(raw).hexdigest() == pins[relative]["sha256"]
    return raw

assert not replay.exists()
work = replay / "external-owner-candidate"
inputs = replay / "external-owner-inputs"
(work / "checks").mkdir(parents=True)
inputs.mkdir()
for relative in sorted(pins):
    if relative.startswith("sources/candidate/"):
        name = relative.removeprefix("sources/candidate/").removesuffix(".txt")
        target = work / "candidate" / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(read_pinned(relative))
for name in ("test_failed_receiving.py", "probe_external_timeout.py"):
    (work / name).write_bytes(read_pinned("sources/" + name + ".txt"))
(inputs / "named-v2-guard-custody.py").write_bytes(
    read_pinned("sources/named-v2-guard-custody.py.txt"))

public_core = repo / (
    "docs/modernization/verification/cache-owner-custody-20261126/"
    "sources/full-v13/qualified_windows_products_consumer_v1.py.txt")
raw = public_core.read_bytes()
assert hashlib.sha256(raw).hexdigest() == (
    "e11118b22a6d805fd9984c885fb646e29638777b46b2227c788864c8d6d7953d")
(inputs / "public-v13-qualified_windows_products_consumer_v1.py").write_bytes(raw)
```

Полный core уже опубликован в #664, commit
`0c255490461fed0a3ee3d43acdd5216e2e064a53`; этот пакет не дублирует его.
Тест проверяет полный hash public v13, извлекает только
`Run.retain_failed_full_workers` через Python AST и проверяет hash этого метода.
Модуль core целиком не импортируется.

AST метода равен
`1d32f9a1b4488234e6eff022998334aecf407b0e5a3acbf333fe98f95a2aa8cc`.
Он и текст метода совпали с actual v14 core
`b679aadd8455cb70c8a445e3abf99484edaf76208fffc0f8347100d12c5976c8`.
Это не утверждение о равенстве остальных частей core.
[Точные данные](reviews/core-method-provenance.json).

## 2. Два model test файла

Из новой папки `external-owner-candidate`:

```text
python -B test_failed_receiving.py -v
python -B probe_external_timeout.py --guard ../external-owner-inputs/named-v2-guard-custody.py --out checks/replay-baseline
python -B probe_external_timeout.py --guard candidate/guard-custody.py --out checks/replay-candidate
```

Ожидания различаются намеренно:

| Команда | Ожидаемый исход |
| --- | --- |
| DATA/adapter suite | Exit 0, 9 tests PASS, включая 23 отрицательных subtests |
| named-v2 baseline | Exit 1, модель FAIL; 11 из 15 инвариантов соблюдены, late receiving отсутствует |
| candidate guard | Exit 0, 15 из 15 инвариантов PASS |

У baseline ложны только `receiver_bound_before_launch`,
`same_original_row_received`, `late_physical_closure_received`,
`capture_handles_closed_after_proof`. Сохранение original failure и запретов
должно выполняться у обеих версий. Baseline FAIL нельзя переписать в PASS.

Имена каталогов `replay-baseline` и `replay-candidate` должны быть новыми; probe
отказывается повторно использовать существующий output root. Model fixtures
сохраняются после тестов; подтверждённо ненужные собственные копии можно переместить
в trash после сохранения отчёта, без очистки trash.

[Новый public-core PASS](checks/public-core-python-tests.log) и
[исторический actual-core PASS](checks/final-python-tests.log) имеют отдельную
область. Source `test_failed_receiving.py` изменён только в input filename,
full-file SHA и дополнительной AST assertion.
[Точный diff fixture](reviews/public-core-fixture-change.patch).

## 3. Сохранённый Windows Go-result

Существующая изолированная P02-композиция:
`b6f82b3f531bb3eec197049167cdc5971b44e2b0`. Нативный stage:
`.scratch/failed-owner-review-20261126-v1`.

Десять Go-файлов из `sources/candidate` материализованы с двумя правилами:

- `controller/*` → `<stage>/controller/*`;
- `owner/canonicalcaseowner/*` → `<stage>/canonicalcaseowner/*`.

Единственная подстановка в bytes:
`__CANONICAL_EXTERNAL_STAGE__` → `failed-owner-review-20261126-v1`.
После неё все десять SHA-256 совпали с inputs исходного `go test`.
[Карта и три дополнительные dependency pins](native-go-v1/source-binding.json).

Записанная команда, выполненная root через существующий original-owner guard:

```text
go test -json -count=1 -timeout 5m -run ^TestFailedPhysicalOuterOriginalReturnAndFrozenIO$ ./.scratch/failed-owner-review-20261126-v1/controller
```

Это Windows compilation и unit-проверка controller с настоящими OS pipes.
Она не запускает debug-owner native loop и не квалифицирует late native
`Owner.Run`. 1 root, 2 subtests и 1 package получили PASS. Сборка выполнена
внутри `go test`; отдельных `go build`, race и полного suite не было.

Standalone head #664 не объявляется эквивалентом использованной P02-композиции.
Новый Windows запуск требует собственной привязки точной композиции, guard и
входов. Сохранённая команда, source pins и receipts не являются таким допуском.

## 4. Идентичности evidence

`native-go-v1/receipt.json` содержит точные исходные CRLF bytes:
36 604 байта, SHA-256
`8b8c8a5172331b8b258c7765ef4d52f8c9cbfb8ad68512e8ee474c74ea480216`.
Импорт с LF имел 35 694 байта и SHA-256
`2d5be6fa0240cf19808e3a8256fcc6553b5fedf955ddfd0f27adf3ae478f644b`;
замена только окончаний строк проверенно восстановила native hash.

При чтении raw hashes используйте `read_bytes()`; текстовый newline conversion
меняет hash. Два непустых captures сохранены точно. Шесть пустых файлов восстановлены
по нулевой длине и SHA-256 пустых bytes, указанным в CLOSED receipts.
[Полный разбор](native-go-v1/receiving-review.json) подтверждает 4 original command
closures, 8 reader joins и отдельное получение завершения родителя.

`artifact-manifest.json` закрепляет все остальные файлы пакета и не включает
сам себя. Исторический manifest до получения Go-result сохранён отдельно.
Ни одна из этих DATA-записей не меняет старые FAIL/UNKNOWN и не активирует adapter.

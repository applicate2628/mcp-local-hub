"""Analyze preserved Go output bytes; never run Go, Cargo, tests, or subprocesses."""
import argparse
import hashlib
import json
import re
import zipfile
from collections import Counter
from datetime import datetime, timedelta, timezone
from decimal import Decimal
from pathlib import Path

COMMIT = "cf5e71ffd88abe53c611f8ee06b536b3228e60e7"
BASE_URL = "https://github.com/applicate2628/mcp-local-hub/blob/" + COMMIT + "/docs/modernization/verification/go-recovery-qualified-inputs-2026-11-20/"
ARCHIVE_SHA = "d12ca2b81b815d18e4a49c659516e8f29c05709542aefb86919b9b5101b178ab"
ARCHIVE_GIT_BLOB = "ab3794334d3ff2087515444acdcda770de877466"
METADATA_GIT_BLOB = "fb6c8a4019346b3f33daef091032e29f27961296"
PACKAGE = re.compile(rb"^(ok|FAIL|\?)\s+(\S+)\s+(?:(\d+(?:\.\d+)?)s|\[([^]]+)\])")
ROOT = re.compile(rb"^--- (PASS|FAIL|SKIP): (\S+) \((\d+(?:\.\d+)?)s\)")
RUNNING = re.compile(rb"^\s{2,}(\S+) \((\d+(?:\.\d+)?(?:ns|us|ms|s|m|h))\)$")

def sha(raw):
    return hashlib.sha256(raw).hexdigest()

def blob(raw):
    return hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest()

def total(values):
    return str(sum((Decimal(v) for v in values), Decimal(0)))

def msk(value):
    return datetime.fromisoformat(value).astimezone(timezone(timedelta(hours=3))).isoformat()

def parse_block(lines, first, end, package):
    roots, timeouts, stacks = [], [], []
    for index in range(first, end):
        line = lines[index]
        match = ROOT.match(line)
        if match:
            roots.append({"package": package, "test": match[2].decode("ascii"),
                          "status": match[1].decode(), "reported_seconds": match[3].decode(),
                          "line": index + 1, "start_event_time": None, "terminal_event_time": None})
        if line.startswith(b"panic: test timed out after "):
            current = []
            position = index + 1
            while position < end and (lines[position].startswith(b"\t") or lines[position] == b"running tests:"):
                running = RUNNING.match(lines[position])
                if running:
                    current.append({"test": running[1].decode("ascii"), "printed_age": running[2].decode(),
                                    "is_root": b"/" not in running[1], "line": position + 1})
                position += 1
            timeouts.append({"package": package, "line": index + 1,
                             "printed_timeout": line.split(b"after ", 1)[1].decode(),
                             "running_tests_at_snapshot": current,
                             "snapshot_time": None, "complete_started_test_inventory": False})
        if line.startswith(b"goroutine "):
            position = index + 1
            while position < end and lines[position] and not lines[position].startswith(b"goroutine "):
                position += 1
            group = lines[index:position]
            joined = b"\n".join(group)
            interesting = [term for term in (b"FlushFileBuffers", b"readSupervisorIPCLine") if term in joined]
            if interesting:
                selected = []
                for offset, frame in enumerate(group):
                    if (frame.startswith(b"mcp-local-hub/") or b"FlushFileBuffers" in frame or
                            frame.startswith(b"\t<WORK_ROOT>/source/")):
                        selected.append({"line": index + offset + 1,
                                         "frame": frame.decode("ascii", errors="backslashreplace")})
                stacks.append({"package": package, "first_line": index + 1, "last_line": position,
                               "observations": [term.decode() for term in interesting], "selected_frames": selected})
    return roots, timeouts, stacks

def analyze_run(zipped, receipt, manifest, name):
    member = "evidence/gates/" + name + ".stdout.log"
    raw = zipped.read(member)
    mapping = manifest[member]
    command_index = next(i for i, item in enumerate(receipt["commands"]) if item["name"] == name)
    command = receipt["commands"][command_index]
    following = receipt["commands"][command_index + 1]
    assert sha(raw) == mapping["saved_sha256"] and len(raw) == mapping["saved_bytes"]
    assert command["stdout_sha256"] == mapping["source_sha256"]
    assert command["stdout_bytes"] == mapping["source_bytes"]
    lines = raw.splitlines()
    undecodable = sum(0xDC80 <= ord(c) <= 0xDCFF for c in raw.decode("utf-8", errors="surrogateescape"))
    assert undecodable == mapping.get("undecodable_utf8_bytes", 0)
    event_count = 0
    for line in lines:
        try:
            event = json.loads(line)
        except (ValueError, UnicodeError):
            continue
        if isinstance(event, dict) and all(key in event for key in ("Time", "Action", "Package")):
            event_count += 1
    packages, roots, timeouts, stacks = [], [], [], []
    start = 0
    for index, line in enumerate(lines):
        match = PACKAGE.match(line)
        if not match:
            continue
        package = match[2].decode("ascii")
        packages.append({"package": package, "status": match[1].decode(),
                         "reported_seconds": match[3].decode() if match[3] else None,
                         "note": match[4].decode() if match[4] else None,
                         "summary_line": index + 1, "block_first_line": start + 1,
                         "start_time": None, "finish_time": None})
        block_roots, block_timeouts, block_stacks = parse_block(lines, start, index, package)
        roots.extend(block_roots)
        timeouts.extend(block_timeouts)
        stacks.extend(block_stacks)
        start = index + 1
    timed = [item for item in packages if item["reported_seconds"] is not None]
    maximum = max(Decimal(item["reported_seconds"]) for item in timed)
    by_package = []
    for package in sorted({item["package"] for item in roots}):
        selected = [item for item in roots if item["package"] == package]
        by_package.append({"package": package, "printed_completed_roots": len(selected),
                           "status_counts": dict(Counter(item["status"] for item in selected)),
                           "sum_printed_root_seconds_not_walltime": total(item["reported_seconds"] for item in selected)})
    started = datetime.fromisoformat(command["started_utc"])
    nominal = started + timedelta(seconds=float(command["seconds"]))
    return {"run": name, "member": member, "saved_bytes": len(raw), "saved_sha256": sha(raw),
            "source_private_hash_binding": {key: mapping[key] for key in ("source_bytes", "source_sha256")},
            "private_original_rehashed": False, "source_mapping_matches_receipt": True,
            "line_count": len(lines), "undecodable_utf8_bytes": undecodable,
            "format": "plain go test output, not JSON events", "go_json_event_count": event_count,
            "last_json_event_time": None, "last_text_line_time": None,
            "run_to_terminal_intervals": None, "dangling_from_start_events": None,
            "command": {key: command[key] for key in ("arguments", "started_utc", "seconds", "exit_code", "settled", "direct_child_deadline_seconds")},
            "started_msk": msk(command["started_utc"]), "observed_process_exit_timestamp": None,
            "nominal_start_plus_elapsed_utc_not_observed_exit": nominal.isoformat(),
            "next_command": {"name": following["name"], "started_utc": following["started_utc"], "started_msk": msk(following["started_utc"])},
            "package_status_counts": dict(Counter(item["status"] for item in packages)),
            "packages": packages, "sum_package_seconds_not_walltime": total(item["reported_seconds"] for item in timed),
            "largest_package_seconds": str(maximum),
            "outer_minus_largest_package_not_compile_time": str(Decimal(str(command["seconds"])) - maximum),
            "slowest_ten_packages": sorted(timed, key=lambda item: Decimal(item["reported_seconds"]), reverse=True)[:10],
            "printed_completed_roots": roots, "root_durations_by_package": by_package,
            "slowest_ten_printed_roots_only": sorted(roots, key=lambda item: Decimal(item["reported_seconds"]), reverse=True)[:10],
            "timeouts": timeouts, "timeout_stack_observations": stacks}

def report(data):
    rows = ["# Разбор сохранённых общих Go-проверок", "",
            "**Результат:** архив доступен и проверен, но оба целевых журнала — обычный текст Go, а не JSON-события. Полная разбивка времени по корневым тестам из этих байтов не восстанавливается. Ни одна проверка Go, Cargo или конвейер непрерывной интеграции не запускалась.", "",
            "## Содержание", "", "1. [Источник и метод](#источник-и-метод)",
            "2. [Известные времена](#известные-времена)", "3. [Самые долгие пакеты](#самые-долгие-пакеты)",
            "4. [Видимые длительности корневых тестов](#видимые-длительности-корневых-тестов)",
            "5. [Снимки тайм-аутов](#снимки-тайм-аутов)", "6. [Следующий проверяемый вопрос](#следующий-проверяемый-вопрос)",
            "7. [Пределы и воспроизведение](#пределы-и-воспроизведение)", "", "## Источник и метод", "",
            f"[Архив на точном опубликованном коммите]({BASE_URL}evidence-redacted.zip): **298 833 байта**, SHA-256 `{ARCHIVE_SHA}`. Проверены Git blob, контроль целостности ZIP, состав и размеры/хеши всех **274 членов** по [метаданным]({BASE_URL}archive-metadata.json). Исходный ZIP сохранён в рабочем каталоге вне trash; в каталоге публикации этого отчёта архив и метаданные повторно не размещаются.", "",
            "Обозначения: ROOT — корневой тест, без пути вложенного подслучая; API — программный интерфейс; CLI — интерфейс командной строки; IPC — межпроцессное взаимодействие; CST — название интегрируемого продукта электромагнитного моделирования; JSON — текстовый формат структурированных данных JavaScript Object Notation; UTF-8 — 8-битная форма кодирования Unicode. Wall time — фактически прошедшее время команды; elapsed в строке Go — напечатанная длительность указанного теста или пакета.", "",
            "Исторический вход квитанции: `f755d374fab317e1037deecd5dac8ffabbcdde36` плюс незакоммиченная правка `internal/cli/gui_force_test.go`. Это не проверка текущего #658. `source_sha256/source_bytes` относятся к приватным оригиналам; опубликованные `saved_sha256/saved_bytes` проверены непосредственно. Исходные приватные байты заново не получались.", "",
            "Квитанция задаёт `go test -count=1 -timeout 5m ./...` и tagged-команду для API/CLI; опций `-json` и `-v` нет. В каждой stdout-копии найдено **0 событий Go JSON**. Full содержит 86 байт, не декодируемых как UTF-8; tagged корректен как UTF-8. Анализ использует побайтные ASCII-шаблоны и не перекодирует исходники. Номера строк относятся к опубликованным членам ZIP.", "",
            "Корневой тест — имя без `/`; вложенные подслучаи отдельно не суммируются. Напечатанные завершения отнесены к блоку, закрывающемуся строкой результата соответствующего пакета. Этот порядок текстовых блоков не используется как хронология параллельного выполнения.", "",
            "## Известные времена", "", "**Таблица 1 — внешние команды и пакеты.**", "",
            "| Запуск | Внешняя команда, с | Самый долгий пакет, с | Сумма длительностей пакетов, с; не wall time | Начало МСК, 20.11.2026 | Следующая команда МСК |",
            "|---|---:|---:|---:|---|---|"]
    for run in data["runs"]:
        rows.append(f'| {run["run"]} | {run["command"]["seconds"]} | {run["largest_package_seconds"]} | {run["sum_package_seconds_not_walltime"]} | {run["started_msk"]} | {run["next_command"]["started_msk"]} |')
    rows += ["", "Обе команды завершились с кодом 1 и `settled=true`. Квитанция не хранит точный UTC-момент выхода процесса. Следующая команда начинается после его завершения. Выражение «начало + seconds» — вычисленная оценка, не наблюдённое событие: runner записывает UTC до сохранения квитанции, а монотонный отсчёт начинает после него. Время последнего JSON/текстового события и интервалы run→pass/fail отсутствуют.", "",
             "Пакетные и тестовые времена могут перекрываться. Суммы в таблице 1 нельзя считать длительностью команды. Разности внешнего времени и максимума пакета — 102.117887 с и 13.01549 с — также не измеряют отдельно компиляцию или простой; начала пакетов неизвестны.", "", "## Самые долгие пакеты", "",
             "**Таблица 2 — десять самых долгих пакетов full; tagged содержит только API 300.069 с и CLI 300.063 с, оба FAIL.**", "",
             "| Пакет | Статус | Напечатано, с | Строка full |", "|---|---|---:|---:|"]
    for item in data["runs"][0]["slowest_ten_packages"]:
        rows.append(f'| `{item["package"]}` | {item["status"]} | {item["reported_seconds"]} | {item["summary_line"]} |')
    rows += ["", "Full содержит 73 результата пакетов: 58 PASS, 4 FAIL, 11 без тестовых файлов. Даже успешный GUI-пакет занял 240.305 с; анализ только двух тайм-аутов не объясняет весь внешний запуск.", "", "## Видимые длительности корневых тестов", "",
             "**Таблица 3 — полнота напечатанных корневых завершений.**", "",
             "| Запуск / пакет | Напечатанные ROOT FAIL | Сумма напечатанных длительностей, с |", "|---|---:|---:|"]
    for run in data["runs"]:
        for item in run["root_durations_by_package"]:
            rows.append(f'| {run["run"]} / `{item["package"]}` | {item["printed_completed_roots"]} | {item["sum_printed_root_seconds_not_walltime"]} |')
    rows += ["", "Full показывает только четыре завершившихся ROOT FAIL: CST 0.31 с, RunV5 и два Job-теста по напечатанным 0.00 с. Tagged показывает RunV5 0.00 с и 14 Serena-отказов, всего 8.03 с. Значение 0.00 — округлённая запись, не доказательство нулевой длительности. PASS-корни не перечислены; полные slowest-10 и распределение 300 с между корнями неизвестны. Эти частичные суммы не вычитаются из времени пакета как неперекрывающиеся интервалы.", "",
             "**Таблица 4 — десять самых долгих только среди напечатанных tagged ROOT FAIL; это не десять самых долгих тестов пакета.**", "",
             "| Тест | Напечатано, с | Строка tagged |", "|---|---:|---:|"]
    for item in data["runs"][1]["slowest_ten_printed_roots_only"]:
        rows.append(f'| `{item["test"]}` | {item["reported_seconds"]} | {item["line"]} |')
    rows += ["", "## Снимки тайм-аутов", "", "**Таблица 5 — только имена из `running tests` при панике.**", "",
             "| Запуск / пакет | Имя | Напечатанный возраст | Строка |", "|---|---|---:|---:|"]
    for run in data["runs"]:
        for timeout in run["timeouts"]:
            for item in timeout["running_tests_at_snapshot"]:
                rows.append(f'| {run["run"]} / `{timeout["package"]}` | `{item["test"]}` | {item["printed_age"]} | {item["line"]} |')
    rows += ["", "Это снимки выполнявшихся тестов, не восстановленный список всех начатых тестов без завершения. Возраст текущих корней — 0/3 с и 1/4 с; он не равен 300-секундному бюджету пакета. Последние имена не устанавливают причину тайм-аута.", "",
             "## Следующий проверяемый вопрос", "",
             "В трёх из четырёх снимков текущий поток находится в `FlushFileBuffers` через `secureWriteClientConfigToResolvedParentObserved`, `secure_write_windows.go:330`: full CLI сохраняет состояние; tagged API создаёт vault-фикстуру; tagged CLI сохраняет registry в `seedSerenaWorkspaceWithPort`. Full API вместо этого ожидает сообщение приветствия локального supervisor IPC в `readSupervisorIPCLine`. Точные строки и прикладные кадры сохранены в JSON.", "",
             "**Гипотеза для следующего адресного измерения:** повторяющиеся синхронные сохранения при подготовке фикстур могут создавать заметную накопленную стоимость. Нужны число вызовов и их интервалы на одной проверенной группе тестов, с разделением подготовки и проверяемого действия. Эти снимки не дают длительность отдельных `FlushFileBuffers`, не доказывают неисправность носителя и не объясняют весь бюджет. Повтор последнего теста без такой гипотезы не обоснован.", "",
             "Предшествующий доступный шаг — новая инвентаризация компилируемых Test/Example/Fuzz-корней на точном #658 и план частей. Она даст состав, но не длительности. Последующее исполнение выбранной части требует отдельной проверки её тел и входов. Общий предел 5m и обязательные полные команды остаются прежними.", "",
             "## Пределы и воспроизведение", "",
             "`analyze_go_budgets.py` читает ZIP и метаданные, проверяет их и создаёт этот Markdown и JSON. Он не импортирует код проекта, не запускает дочерние процессы и не изменяет исходный архив. Для повторения используйте новый выходной каталог:", "",
             "```text", "python analyze_go_budgets.py --archive <путь-к-старому-evidence-redacted.zip> --metadata <путь-к-старому-archive-metadata.json> --output <новый-каталог>", "```", "",
             f"Входы для воспроизведения: [старый evidence-redacted.zip]({BASE_URL}evidence-redacted.zip) и [старый archive-metadata.json]({BASE_URL}archive-metadata.json). Оба закреплены на том же неизменяемом коммите.", "",
             f"[Опубликованное описание исходного среза]({BASE_URL}RESULTS.md) и `evidence/gates/receipt.json` в архиве задают источник и команды. Честно закрыто чтение и разложение доступных наблюдений; полная ROOT-телеметрия исторических запусков отсутствует, а общие FAIL сохраняются.", ""]
    return "\n".join(rows)

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--archive", type=Path, default=Path(__file__).parent / "evidence-redacted.zip")
    parser.add_argument("--metadata", type=Path, default=Path(__file__).parent / "archive-metadata.json")
    parser.add_argument("--output", type=Path, default=Path(__file__).parent)
    args = parser.parse_args()
    archive = args.archive.read_bytes()
    metadata_bytes = args.metadata.read_bytes()
    assert sha(archive) == ARCHIVE_SHA and blob(archive) == ARCHIVE_GIT_BLOB and len(archive) == 298833
    assert blob(metadata_bytes) == METADATA_GIT_BLOB
    metadata = json.loads(metadata_bytes)
    assert metadata["archive_sha256"] == sha(archive) and metadata["archive_bytes"] == len(archive)
    with zipfile.ZipFile(args.archive) as zipped:
        assert zipped.testzip() is None
        assert len(zipped.infolist()) == len(metadata["members"]) == len(set(zipped.namelist())) == 274
        assert set(zipped.namelist()) == {item["path"] for item in metadata["members"]}
        for item in metadata["members"]:
            raw = zipped.read(item["path"])
            assert sha(raw) == item["sha256"] and len(raw) == item["bytes"]
        manifest = json.loads(zipped.read("manifest.json"))
        receipt = json.loads(zipped.read("evidence/gates/receipt.json"))
        data = {"schema": 1, "source_commit": COMMIT, "source_url": BASE_URL,
                "archive_bytes": len(archive), "archive_sha256": sha(archive),
                "archive_git_blob": blob(archive), "archive_crc_verified": True,
                "archive_member_hashes_verified": 274, "source_before": receipt["source_before"],
                "runner_member": "runners/run_candidate.py", "runner_source_sha256": receipt["runner_sha256"],
                "receipt_saved_sha256": sha(zipped.read("evidence/gates/receipt.json")),
                "new_tests_run": False, "original_bytes_modified": False,
                "runs": [analyze_run(zipped, receipt, manifest, name) for name in ("full-test", "tagged-api-cli")]}
    assert [run["go_json_event_count"] for run in data["runs"]] == [0, 0]
    assert [len(run["printed_completed_roots"]) for run in data["runs"]] == [4, 15]
    args.output.mkdir(parents=True, exist_ok=True)
    for name, text in (("budget-audit.json", json.dumps(data, indent=2, ensure_ascii=False) + "\n"),
                       ("budget-audit.md", report(data))):
        with (args.output / name).open("x", encoding="utf-8") as stream:
            stream.write(text)
    print(json.dumps({"verified_archive_members": 274, "new_tests_run": False,
                      "runs": [{"name": run["run"], "packages": len(run["packages"]),
                                "printed_roots": len(run["printed_completed_roots"]),
                                "json_events": run["go_json_event_count"],
                                "timeout_snapshots": len(run["timeouts"])} for run in data["runs"]]}))

if __name__ == "__main__":
    main()

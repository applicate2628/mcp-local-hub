#!/usr/bin/env python3
"""Render the narrowly scoped R20 documentation amendment from actual evidence.

This program performs local reads/writes only. It neither fetches nor publishes.
No completed result is rendered from the unfilled input template.
"""
from __future__ import annotations

import argparse
import difflib
import hashlib
import json
import math
import re
import sys
from datetime import datetime
from pathlib import Path, PurePosixPath


ROOT = Path(__file__).resolve().parents[2]
ANCHOR = "install-snapshot-guard-20261121"
BASE_CODE = "11c286fb634cc63d2c6019a790ff376a1d834af9"
EVIDENCE_DIR = "docs/modernization/verification/install-snapshot-guard-2026-11-21"
REPO = "https://github.com/applicate2628/mcp-local-hub"
SOURCE = {
    "plan": {
        "local": ROOT / "r19-cancellation/plan.candidate.md",
        "repo_path": ".plans/2026-11/open-pr-integration-20261117.md",
        "commit": "6cc44839556653b2e8265c39143d36914c7e25e5",
        "git_blob": "ece1e9263364168579142480a44f5cfdbbc17acc",
        "sha256": "8761771c795f75c0fd034d7f7c91a1fdd2d10d790d0d40f7b32ec0b8a3ec8527",
        "bytes": 339963,
    },
    "board": {
        "local": ROOT / "r19-cancellation/board.candidate.md",
        "repo_path": "docs/modernization/plans/rust-independent-execution-plan-2026-11-12.md",
        "commit": "203209df3a11b66ccba59cc7a308d3aa8e4cc7f8",
        "git_blob": "8c43010e08f7dc725ab53aa547088e2a523b334f",
        "sha256": "9f2ca30c4f9aa85a815d6706bf1396a4c89795d6aebd2df14db0e335bb84a1db",
        "bytes": 160106,
    },
}
TOC_PREVIOUS = (
    "    - [Причины отмены Scheduler и отдельная CLI-фикстура, 20.11.2026]"
    "(#scheduler-cancellation-20261120)\n"
)
TOC_NEW = f"    - [Допуск к общей замене задачи, 21.11.2026](#{ANCHOR})\n"
PLAN_MARKER = '<a id="remaining-after-r17-export-20261119"></a>\n'
BOARD_MARKER = "### 0.1. Как обновлять статус без раздувания плана\n"
OLD_ITEM2 = (
    "Общий install replacement по-прежнему подавляет Export/Delete и регистрирует откат после Create; "
    "register/upgrade/weekly требуют собственных правил владения определением."
)
OLD_ROW4_SUMMARY = (
    "командный контракт Create, guard снимка перед prune, сохранение причин отмены "
    "и указанные Go-тестовые исправления выполнены."
)
RUN_SPEC = {
    "negative_control": ("FAIL", 0, 15, 0, 1),
    "candidate_target": ("PASS", 30, 0, 1, 0),
    "candidate_neighbor": ("PASS", 8, 0, 1, 0),
    "candidate_build": ("PASS", None, None, None, None),
    "candidate_vet": ("PASS", None, None, None, None),
    "published_target": ("PASS", 30, 0, 1, 0),
}
RUN_LABELS = {
    "negative_control": "Отрицательный контроль на исходном production и новых тестах",
    "candidate_target": "Целевая выборка кандидата",
    "candidate_neighbor": "Соседний контракт Pass A / Pass B",
    "candidate_build": "Сборка кандидата",
    "candidate_vet": "Статический анализ go vet кандидата",
    "published_target": "Повтор целевой выборки точного опубликованного коммита",
}
COUNT_KEYS = ("test_pass", "test_fail", "test_skip", "package_pass", "package_fail", "package_skip")


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def git_blob(data: bytes) -> str:
    header = b"blob " + str(len(data)).encode("ascii") + b"\0"
    return hashlib.sha1(header + data).hexdigest()


def q(text: object) -> str:
    return chr(96) + str(text) + chr(96)


def replace_once(text: str, old: str, new: str) -> str:
    require(text.count(old) == 1, f"Expected one exact replacement target: {old[:100]!r}")
    return text.replace(old, new, 1)


def active_items(text: str) -> dict[str, str]:
    require(text.count(PLAN_MARKER) == 1, "Active-remainder marker is missing or duplicated")
    section = text.split(PLAN_MARKER, 1)[1]
    rows = re.findall(r"^([1-8])\. [^\n]*(?:\n|$)", section, re.MULTILINE)
    require(rows == [str(n) for n in range(1, 9)], "The active plan must retain exactly eight directions")
    return {
        line[0]: line
        for line in section.splitlines(keepends=True)
        if re.match(r"^[1-8]\. ", line)
    }


def board_rows(text: str) -> dict[str, str]:
    pairs = []
    for line in text.splitlines(keepends=True):
        match = re.match(r"^\| \*\*([1-6])\*\* \|", line)
        if match:
            pairs.append((match.group(1), line))
    require([n for n, _ in pairs] == [str(n) for n in range(1, 7)], "Board must retain six numbered rows")
    return dict(pairs)


def load_bases() -> tuple[dict[str, str], dict]:
    texts, manifest = {}, {}
    for name, spec in SOURCE.items():
        raw = spec["local"].read_bytes()
        require(len(raw) == spec["bytes"], f"{name}: source length drift")
        require(sha256(raw) == spec["sha256"], f"{name}: source SHA-256 drift")
        require(git_blob(raw) == spec["git_blob"], f"{name}: source Git blob drift")
        text = raw.decode("utf-8")
        require(ANCHOR not in text, f"{name}: checkpoint already exists")
        require(text.count(TOC_PREVIOUS) == 1, f"{name}: navigation anchor drift")
        texts[name] = text
        manifest[name] = {
            **{k: v for k, v in spec.items() if k != "local"},
            "local": str(spec["local"]),
            "protected_units": {
                number: sha256(line.encode("utf-8"))
                for number, line in (active_items(text) if name == "plan" else board_rows(text)).items()
            },
        }
    return texts, manifest


def check_sha(value: object, field: str, length: int = 40) -> str:
    require(isinstance(value, str) and bool(re.fullmatch(rf"[0-9a-f]{{{length}}}", value)), f"{field}: expected {length}-digit lowercase hexadecimal hash")
    require(len(set(value)) > 1, f"{field}: placeholder hashes are not accepted")
    return value


def check_count(value: object, field: str, positive: bool = False) -> int:
    require(type(value) is int and value >= (1 if positive else 0), f"{field}: expected a {'positive' if positive else 'nonnegative'} integer")
    return value


def check_relative_file(value: object, field: str) -> str:
    require(isinstance(value, str) and bool(value), f"{field}: evidence filename is required")
    path = PurePosixPath(value)
    require(not path.is_absolute() and ".." not in path.parts and "\\" not in value, f"{field}: expected a relative POSIX path")
    require(bool(re.fullmatch(r"[A-Za-z0-9._/-]+", value)), f"{field}: unsupported filename characters")
    return value


def validate_input(data: dict, target: str) -> None:
    require(data.get("schema_version") == 1, "schema_version must be 1")
    code = check_sha(data.get("code_commit"), "code_commit")
    require(code != BASE_CODE, "code_commit must identify the new published change, not its parent")
    require(data.get("code_parent") == BASE_CODE, "code_parent must be the reviewed #660 parent")
    evidence = data.get("evidence_commit")
    if target in ("plan", "both"):
        check_sha(evidence, "evidence_commit")
        require(evidence not in (SOURCE["board"]["commit"], code), "evidence_commit must identify the new separate documentation publication")
    elif evidence is not None:
        check_sha(evidence, "evidence_commit")
    for field in ("code_readback_equal", "candidate_matches_published", "published_checkout_clean"):
        require(data.get(field) is True, f"{field}: actual verification is required")
    require(type(data.get("update_item6")) is bool, "update_item6 must be a JSON boolean")
    observed = data.get("observed_at_utc")
    require(isinstance(observed, str) and observed.endswith("Z"), "observed_at_utc must be an actual UTC ISO timestamp ending in Z")
    datetime.fromisoformat(observed.replace("Z", "+00:00"))
    evidence_files = data.get("evidence_files", {})
    for field in ("results", "summary", "review"):
        check_relative_file(evidence_files.get(field), f"evidence_files.{field}")
    runs = data.get("runs")
    require(isinstance(runs, dict) and set(runs) == set(RUN_SPEC), "runs must contain exactly the six documented checks")
    for name, expected in RUN_SPEC.items():
        run = runs[name]
        require(isinstance(run, dict), f"runs.{name}: expected an object")
        require(run.get("status") == expected[0], f"runs.{name}: no completed document until actual {expected[0]} is established")
        exit_code = run.get("exit_code")
        require(type(exit_code) is int, f"runs.{name}.exit_code: integer required")
        require((exit_code == 0) == (expected[0] == "PASS"), f"runs.{name}: exit code contradicts status")
        duration = run.get("duration_seconds")
        require(type(duration) in (int, float) and math.isfinite(duration) and duration >= 0, f"runs.{name}.duration_seconds: actual finite duration required")
        require(isinstance(run.get("command"), str) and bool(run["command"].strip()), f"runs.{name}.command: actual command required")
        if name in ("candidate_target", "candidate_neighbor", "published_target"):
            command = run["command"]
            require(bool(re.search(r"(?:^|\s)-race(?:\s|$)", command)), f"runs.{name}: command must include the documented -race flag")
            require(bool(re.search(r"(?:^|\s)-count(?:=|\s+)2(?:\s|$)", command)), f"runs.{name}: command must include the documented two repeats")
            require("test_state_path_env" in command, f"runs.{name}: command must include the qualified isolation tag")
        check_relative_file(run.get("evidence_path"), f"runs.{name}.evidence_path")
        if expected[1] is not None:
            counts = run.get("counts")
            require(isinstance(counts, dict) and set(counts) == set(COUNT_KEYS), f"runs.{name}.counts: all six counters required")
            for key in COUNT_KEYS:
                check_count(counts[key], f"runs.{name}.counts.{key}")
            want = dict(zip(COUNT_KEYS, (expected[1], expected[2], 0, expected[3], expected[4], 0)))
            require(counts == want, f"runs.{name}: observed counters differ from the scoped acceptance set; review the source/selection before rendering")
        else:
            require(run.get("counts") is None, f"runs.{name}: build/vet do not have test-event counters")
    require(runs["negative_control"].get("failure_reasons_verified") is True, "negative_control: verify the intended failure causes, not only a nonzero exit code")
    broad = data.get("broad_checks", {})
    require(set(broad) == {"default", "tagged_api_cli"}, "Both broad-check dispositions are required")
    for name, entry in broad.items():
        require(entry.get("status") == "NOT_RUN", f"broad_checks.{name}: this renderer preserves the agreed unrun scope; handle a changed execution scope separately")
        reason = entry.get("reason")
        require(isinstance(reason, str) and len(reason.strip()) >= 10 and "\n" not in reason, f"broad_checks.{name}.reason: one factual reason is required")
    timing = data.get("timing_observation")
    if timing is not None:
        require(isinstance(timing, dict) and set(timing) == {"summary", "evidence_path"}, "timing_observation: expected summary and evidence_path")
        summary = timing["summary"]
        require(isinstance(summary, str) and 10 <= len(summary.strip()) <= 3000 and "\n" not in summary, "timing_observation.summary: one factual paragraph is required")
        check_relative_file(timing["evidence_path"], "timing_observation.evidence_path")
        require(data["update_item6"], "timing_observation requires update_item6=true so the observation is not silently omitted")
    counters = data.get("artifact_counters", {})
    allowed = {"evidence_file_count", "archive_bytes", "archive_entries", "manifest_file_hashes", "archive_sha256", "source_streams"}
    require(set(counters) == allowed, "artifact_counters: expected the six documented optional fields")
    for key in allowed - {"archive_sha256"}:
        if counters[key] is not None:
            check_count(counters[key], f"artifact_counters.{key}", positive=True)
    archive_fields = ("archive_bytes", "archive_entries", "manifest_file_hashes", "archive_sha256")
    if any(counters[key] is not None for key in archive_fields):
        require(all(counters[key] is not None for key in archive_fields), "Archive counters must be supplied together")
        check_sha(counters["archive_sha256"], "artifact_counters.archive_sha256", 64)
        require(counters["manifest_file_hashes"] <= counters["archive_entries"], "Archive manifest count cannot exceed entry count")


def seconds(value: int | float) -> str:
    return format(value, ".9f").rstrip("0").rstrip(".").replace(".", ",")


def evidence_base(data: dict, document: str) -> str:
    if document == "board":
        return "../verification/install-snapshot-guard-2026-11-21"
    return f"{REPO}/blob/{data['evidence_commit']}/{EVIDENCE_DIR}"


def link(data: dict, document: str, label: str, key: str) -> str:
    return f"[{label}]({evidence_base(data, document)}/{data['evidence_files'][key]})"


def result_lines(data: dict, document: str) -> str:
    lines = []
    for key, label in RUN_LABELS.items():
        run = data["runs"][key]
        counts = run["counts"]
        detail = ""
        if counts is not None:
            if run["status"] == "FAIL":
                detail = f"; {counts['test_fail']} тестовых/подтестовых FAIL и {counts['package_fail']} пакетный FAIL; ожидаемые причины проверены"
            else:
                detail = f"; {counts['test_pass']} тестовых/подтестовых и {counts['package_pass']} пакетный PASS, без FAIL/SKIP"
        path = f"{evidence_base(data, document)}/{run['evidence_path']}"
        lines.append(f"- [{label}]({path}) — **{run['status']}**, {seconds(run['duration_seconds'])} с{detail}.")
    return "\n".join(lines)


def artifact_summary(data: dict) -> str:
    c = data["artifact_counters"]
    parts = []
    if c["evidence_file_count"] is not None:
        parts.append(f"Файлов свидетельств: **{c['evidence_file_count']}**.")
    if c["archive_bytes"] is not None:
        parts.append(
            f"Архив: **{c['archive_bytes']} байт, {c['archive_entries']} элементов, "
            f"{c['manifest_file_hashes']} файловых хешей**; SHA-256 {q(c['archive_sha256'])}."
        )
    if c["source_streams"] is not None:
        parts.append(f"Учтено исходных потоков: **{c['source_streams']}**.")
    return " ".join(parts)


def checkpoint(data: dict, document: str) -> str:
    heading = (
        "#### Допуск к общей замене задачи, 21 ноября 2026 года"
        if document == "plan"
        else "**Допуск к общей замене задачи, 21 ноября 2026 года [36].**"
    )
    pinned = f" Свидетельства закреплены в #623 {q(data['evidence_commit'])}." if document == "plan" else ""
    artifacts = artifact_summary(data)
    artifact_paragraph = f"\n\n{artifacts}" if artifacts else ""
    return (
        f'<a id="{ANCHOR}"></a>\n{heading}\n\n'
        f"В существующем #660 опубликован {q(data['code_commit'])}: общий цикл "
        f"{q('executeInstallToWithSymlinkConsents')} прекращает замену при ошибочном, пустом или противоречивом "
        f"результате {q('ExportXML')}; прямой или обёрнутый {q('ErrTaskNotFound')} без байтов разрешает "
        f"{q('Create')} без предварительного {q('Delete')}. Ошибка {q('Delete')} прекращает следующий "
        f"{q('Create')} и сохраняет исходную причину через {q('errors.Is')}. Выполняется существующий откат ранее "
        "завершённых шагов. Изменены только блок допуска в install.go и отдельный тестовый файл; "
        f"интерфейсы и платформенные команды сохранены.{pinned}\n\n"
        f"{result_lines(data, document)}\n\n"
        "Целевая и соседняя положительные выборки используют детектор гонок "
        f"{q('-race')} и два повтора {q('-count=2')} с {q('test_state_path_env')}. "
        "Повтор опубликованного кода относится к чистому checkout его точного коммита; совпадение "
        "прочитанных исходников и проверенного кандидата подтверждено. Планировщик в тестах подставной: "
        "проверены сохранение байтов и записи, отсутствие запрещённых вызовов, модель конфликта после "
        "отсутствующего снимка и восстановление предыдущего завершённого шага. Длительности команд не являются "
        "измерением ускорения продукта.\n\n"
        f"Обычный широкий набор — **NOT_RUN**: {data['broad_checks']['default']['reason']}. "
        f"Полный тегированный API/CLI — **NOT_RUN**: {data['broad_checks']['tagged_api_cli']['reason']}. "
        "Прежние общие FAIL #658 и #660 сохраняются; адресные результаты не закрывают эти критерии. "
        "Открыты восстановление текущей задачи после успешного Delete и неуспешного Create, неопределённый "
        "нативный исход Delete, внешняя замена между Export/Delete и при откате, принудительный ImportXML "
        "с /F, пригодность XML для нативного импорта и остальные операции R17. Полная установка и нативные "
        "изменяющие действия этой модельной серией не квалифицированы. Ветки не интегрировались."
        f"{artifact_paragraph}\n\n"
        f"{link(data, document, 'Результаты и границы', 'results')} · "
        f"{link(data, document, 'сводка исполнения', 'summary')} · "
        f"{link(data, document, 'независимое ревью кандидата', 'review')}. "
        "Независимое статическое чтение не является формальным внешним одобрением. "
        "XML — формат определения задачи; API/CLI — программный интерфейс/командная строка; "
        "PASS/FAIL/NOT_RUN — успех, отказ и отсутствие запуска конкретной проверки.\n\n"
    )


def render_plan(data: dict, original: str) -> tuple[str, dict]:
    before = active_items(original)
    cp = checkpoint(data, "plan")
    replacement = (
        f"[Допуск к общей замене](#{ANCHOR}) исправлен и проверен в #660 {q(data['code_commit'][:8])}: "
        "ошибочный, пустой или противоречивый снимок останавливает шаг до Delete/Create; подтверждённое "
        "отсутствие разрешает Create без Delete; ошибка Delete не подавляется. Эту завершённую локальную "
        "защиту не возвращать в невыполненную разработку. Восстановление текущей задачи после "
        "Delete-success/Create-failure, неопределённый нативный исход Delete, внешняя замена между "
        "Export/Delete и при откате, а также принудительный ImportXML с /F остаются открытыми; "
        "register/upgrade/weekly требуют собственных правил владения определением."
    )
    item2 = replace_once(before["2"], OLD_ITEM2, replacement)
    item6 = before["6"]
    if data["update_item6"]:
        item6 = item6.rstrip("\n") + (
            f" Новый [адресный срез допуска к замене](#{ANCHOR}) на #660 "
            f"{q(data['code_commit'][:8])} проверен отдельно; его модельные PASS не заменяют "
            "прежние общие FAIL и не переносят исправления #658 в #660.\n"
        )
        if data.get("timing_observation") is not None:
            timing = data["timing_observation"]
            timing_url = f"{evidence_base(data, 'plan')}/{timing['evidence_path']}"
            item6 = item6.rstrip("\n") + " " + timing["summary"] + f" [Квитанции наблюдения]({timing_url}).\n"
    result = replace_once(original, before["2"], item2)
    if data["update_item6"]:
        result = replace_once(result, before["6"], item6)
    result = replace_once(result, TOC_PREVIOUS, TOC_PREVIOUS + TOC_NEW)
    result = replace_once(result, PLAN_MARKER, cp + PLAN_MARKER)
    after = active_items(result)
    protected = ["1", "3", "4", "5", "7", "8"] + ([] if data["update_item6"] else ["6"])
    for number in protected:
        require(before[number] == after[number], f"Protected plan item {number} changed")
    # Undo exactly our allowed amendments and compare the entire prior text.
    restored = replace_once(result, cp, "")
    restored = replace_once(restored, TOC_PREVIOUS + TOC_NEW, TOC_PREVIOUS)
    restored = replace_once(restored, item2, before["2"])
    if data["update_item6"]:
        restored = replace_once(restored, item6, before["6"])
    require(restored == original, "Plan history changed outside the approved regions")
    require(before["6"].rstrip("\n") in after["6"], "Historical Go results in item 6 were rewritten")
    return result, audit_units("plan", before, after, protected, restored)


def render_board(data: dict, original: str) -> tuple[str, dict]:
    before = board_rows(original)
    row4 = before["4"]
    require(row4.endswith(" |\n"), "Stage-4 row ending drift")
    split = row4[:-3].split(" | ")
    require(len(split) == 5, "Stage-4 table layout drift")
    completed = (
        f" [Допуск к общей замене](#{ANCHOR}) исправлен в #660 {q(data['code_commit'][:8])} [36]: "
        "недостоверный снимок останавливает удаление, отсутствие допускает Create без Delete, "
        "ошибка Delete прекращает шаг."
    )
    split[2] += completed
    split[4] = replace_once(
        split[4],
        OLD_ROW4_SUMMARY,
        "командный контракт Create, guard снимка перед prune и общей заменой, сохранение причин отмены "
        "и указанные Go-тестовые исправления выполнены.",
    )
    # The existing next-actions cell already preserves Delete-success/Create-failure.
    split[4] += (
        f" Новый [срез допуска к замене](#{ANCHOR}) [36] имеет отдельные адресные результаты "
        "и повтор опубликованного коммита; прежние общие FAIL не заменены. Неопределённый Delete, "
        "внешняя замена и принудительный ImportXML с /F остаются."
    )
    new_row4 = " | ".join(split) + " |\n"
    cp = checkpoint(data, "board")
    reference = (
        f"36. {link(data, 'board', 'Допуск к общей замене задачи, 21.11.2026', 'results')}. "
        f"Код #660 {q(data['code_commit'])}; "
        f"{link(data, 'board', 'сводка исполнения', 'summary')}, "
        f"{link(data, 'board', 'независимое ревью кандидата', 'review')}. "
        "Модельные проверки допуска отделены от нативного восстановления, внешней замены и общих Go-критериев.\n"
    )
    reference35 = re.findall(r"^35\. [^\n]*\n", original, re.MULTILINE)
    require(len(reference35) == 1 and not re.search(r"^36\. ", original, re.MULTILINE), "Board bibliography numbering drift")
    result = replace_once(original, before["4"], new_row4)
    result = replace_once(result, TOC_PREVIOUS, TOC_PREVIOUS + TOC_NEW)
    result = replace_once(result, BOARD_MARKER, cp + BOARD_MARKER)
    result = replace_once(result, reference35[0], reference35[0] + reference)
    after = board_rows(result)
    protected = ["1", "2", "3", "5", "6"]
    for number in protected:
        require(before[number] == after[number], f"Protected board row {number} changed")
    restored = replace_once(result, new_row4, before["4"])
    restored = replace_once(restored, cp, "")
    restored = replace_once(restored, TOC_PREVIOUS + TOC_NEW, TOC_PREVIOUS)
    restored = replace_once(restored, reference35[0] + reference, reference35[0])
    require(restored == original, "Board history changed outside stage 4, navigation, checkpoint and reference")
    return result, audit_units("board", before, after, protected, restored)


def audit_units(name: str, before: dict, after: dict, protected: list[str], restored: str) -> dict:
    return {
        "protected_units": {
            n: {
                "before_sha256": sha256(before[n].encode("utf-8")),
                "after_sha256": sha256(after[n].encode("utf-8")),
                "byte_equal": before[n] == after[n],
            }
            for n in sorted(protected, key=int)
        },
        "all_unit_count_before": len(before),
        "all_unit_count_after": len(after),
        "prior_text_reconstructed_byte_equal": True,
        "reconstructed_prior_sha256": sha256(restored.encode("utf-8")),
        "source_sha256": SOURCE[name]["sha256"],
    }


def write_new_or_equal(path: Path, content: bytes) -> None:
    if path.exists():
        require(path.is_file() and path.read_bytes() == content, f"Refusing to overwrite a differing file: {path}. Use a new output directory; review/retain the prior result.")
        return
    path.write_bytes(content)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, help="JSON with actual results; see input.template.json")
    parser.add_argument("--target", choices=("board", "plan", "both"), default="both")
    parser.add_argument("--output-dir", type=Path, help="A new local result directory")
    parser.add_argument("--verify-bases", action="store_true", help="Verify source identities and print their protected hashes; no result document is created")
    args = parser.parse_args()
    try:
        bases, source_manifest = load_bases()
        if args.verify_bases:
            require(args.input is None and args.output_dir is None, "--verify-bases accepts no input/output path")
            print(json.dumps(source_manifest, ensure_ascii=False, indent=2))
            return 0
        require(args.input is not None and args.output_dir is not None, "--input and --output-dir are required")
        raw_input = args.input.read_bytes()
        data = json.loads(raw_input.decode("utf-8"))
        require(isinstance(data, dict), "Input must be a JSON object")
        validate_input(data, args.target)
        targets = ("board", "plan") if args.target == "both" else (args.target,)
        output = args.output_dir.resolve()
        require(output != args.input.parent.resolve(), "Use a separate output directory")
        payloads, audits = {}, {}
        for name in targets:
            rendered, audit = (render_board if name == "board" else render_plan)(data, bases[name])
            raw = rendered.encode("utf-8")
            audits[name] = {
                **audit,
                "output_sha256": sha256(raw),
                "output_git_blob": git_blob(raw),
                "output_bytes": len(raw),
                "repo_path": SOURCE[name]["repo_path"],
                "source_commit": SOURCE[name]["commit"],
            }
            payloads[f"{name}.candidate.md"] = raw
            diff = "".join(difflib.unified_diff(
                bases[name].splitlines(keepends=True), rendered.splitlines(keepends=True),
                fromfile="a/" + SOURCE[name]["repo_path"], tofile="b/" + SOURCE[name]["repo_path"],
            ))
            payloads[f"{name}.patch"] = diff.encode("utf-8")
        report = {
            "schema_version": 1, "checkpoint": ANCHOR, "input_sha256": sha256(raw_input),
            "observed_at_utc": data["observed_at_utc"], "target": args.target,
            "code_commit": data["code_commit"],
            "evidence_commit": data["evidence_commit"] if args.target != "board" else None,
            "board_uses_relative_evidence_links": True,
            "timing_observation_in_plan": args.target != "board" and data.get("timing_observation") is not None,
            "documents": audits, "sources": source_manifest,
            "local_only": True, "github_mutations": False,
        }
        payloads["amendment-audit.json"] = (json.dumps(report, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
        # Check every destination before creating/writing any result.
        for filename, content in payloads.items():
            path = output / filename
            require(not path.exists() or (path.is_file() and path.read_bytes() == content), f"Differing destination already exists: {path}; choose a new output directory")
        output.mkdir(parents=True, exist_ok=True)
        for filename, content in payloads.items():
            write_new_or_equal(output / filename, content)
        print(json.dumps({"output_dir": str(output), "files": list(payloads), "prior_history_preserved": True}, ensure_ascii=False))
        return 0
    except (ValueError, OSError, KeyError, TypeError) as exc:
        print(f"amend_docs: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

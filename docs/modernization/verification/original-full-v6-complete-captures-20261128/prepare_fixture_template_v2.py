"""Prepare an explicit fixture-template revision from frozen, closed DATA.

No artifact execution/import, network, subprocess, Git, policy, cache access,
receipt reclassification, deletion, overwrite, or change to the v1 capsule.
All paths are explicit operands. Fixture values come from pinned public sources.
"""
import argparse
import hashlib
import json
import re
from pathlib import Path, PurePosixPath

PLAN_SHA256 = "756f745a528329663ba06d41cba24f228b5b9a83e460749fb7079874ddf6575f"


def require(ok, message):
    if not ok:
        raise ValueError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def json_bytes(value):
    return (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode("utf-8")


def relative(value):
    require(isinstance(value, str) and value and "\\" not in value and ":" not in value,
            "plain relative artifact path required")
    path = PurePosixPath(value)
    require(not path.is_absolute() and ".." not in path.parts and "." not in path.parts
            and str(path) == value, "unclean artifact path")
    return Path(*path.parts)


def beneath(path, root):
    try:
        path.resolve().relative_to(root.resolve())
        return True
    except ValueError:
        return False


def ordinary(path, root):
    require(beneath(path, root) and path.is_file() and not path.is_symlink()
            and not getattr(path, "is_junction", lambda: False)(),
            "ordinary bounded input required")
    return path.read_bytes()


def pin(raw, row, label):
    require(len(raw) == row["bytes"] and digest(raw) == row["sha256"],
            "pinned bytes differ: " + label)


def fixture_value(source, selector):
    lines = source.decode("utf-8").splitlines()
    require(0 < selector["line"] <= len(lines), "fixture line missing")
    line = lines[selector["line"] - 1]
    kind = selector["kind"]
    if kind.startswith("raw_"):
        delimiter = chr(96)
        literals = re.findall(delimiter + "([^" + delimiter + "]*)" + delimiter, line)
        require(len(literals) == 1, "expected one raw fixture literal")
        value = literals[0].encode("utf-8")
    else:
        literals = re.findall(r'"(?:\\.|[^"\\])*"', line)
        require(len(literals) == (2 if kind == "quoted_concat_suffix" else 1),
                "fixed quoted fixture expression differs")
        if kind == "quoted_concat_suffix":
            expression = line.split("=", 1)[1].strip()
            require(re.fullmatch(r'"(?:\\.|[^"\\])*"\s*\+\s*"(?:\\.|[^"\\])*"',
                                 expression), "only two literal operands are admitted")
        value = "".join(json.loads(token) for token in literals).encode("utf-8")
    count = selector["count"]
    if kind in ("raw_prefix", "quoted_prefix"):
        start = selector["start"]
        require(start >= 0 and start + count <= len(value), "fixture prefix outside literal")
        return value[start:start + count]
    require(kind in ("raw_suffix", "quoted_concat_suffix") and len(value) >= count,
            "unsupported fixture selector")
    return value[-count:]


def replacement(roles, substitution):
    text = roles[substitution["role"]]
    transform = substitution["transform"]
    if transform == "double_backslashes":
        text = text.replace("\\", "\\\\")
    elif transform == "quadruple_backslashes":
        text = text.replace("\\", "\\\\\\\\")
    else:
        require(transform == "literal", "unsupported role transform")
    return text.encode("utf-8")


def reconstruct(payload, item, roles):
    raw = payload
    for substitution in item["substitutions"]:
        token = substitution["marker"].encode("ascii")
        require(raw.count(token) == substitution["occurrences"], "marker count differs")
        raw = raw.replace(token, replacement(roles, substitution))
    require(b"@@RECOVERY_" not in raw, "unresolved reconstruction marker")
    if item.get("data_transform"):
        require(item["data_transform"] == "nul_paths_as_lf_lines"
                and item["path_count"] == 3509 and raw.count(b"\n") == 3509
                and b"\0" not in raw and b"\r" not in raw, "path-list projection differs")
        raw = raw.replace(b"\n", b"\0")
    require(len(raw) == item["original_bytes"]
            and digest(raw) == item["original_sha256"], "historical original differs")
    return raw


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("source-capsule", "source-fixtures", "operator-roles",
                 "canonical-locations", "output-capsule", "output-private", "work-area"):
        parser.add_argument("--" + name, required=True, type=Path)
    args = parser.parse_args()
    here = Path(__file__).resolve().parent
    plan_raw = ordinary(here / "fixture-template-v2-plan.json", here)
    require(digest(plan_raw) == PLAN_SHA256, "reviewed transformation plan changed")
    plan = json.loads(plan_raw)
    work = args.work_area.resolve(strict=True)
    source = args.source_capsule.resolve(strict=True)
    fixtures = args.source_fixtures.resolve(strict=True)
    require(work.is_dir() and source.is_dir() and fixtures.is_dir()
            and beneath(source, work) and beneath(fixtures, work), "bounded input roots required")
    outputs = (args.output_capsule, args.output_private)
    for target in outputs:
        require(target.parent.is_dir() and beneath(target, work)
                and not target.exists() and not target.is_symlink()
                and not beneath(target, source) and not beneath(target, fixtures)
                and not beneath(target, here), "fresh separate output under approved work area required")
    require(not beneath(outputs[0], outputs[1]) and not beneath(outputs[1], outputs[0]),
            "public and private outputs must be separate")

    old_manifest_raw = ordinary(source / "artifact-manifest.json", source)
    require(digest(old_manifest_raw) == plan["original_capsule_manifest_sha256"],
            "frozen v1 artifact manifest changed")
    old_manifest = json.loads(old_manifest_raw)
    rows = old_manifest["files"]
    require(len(rows) + 1 == plan["expected_original_public_file_count"]
            and len({row["path"] for row in rows}) == len(rows), "v1 artifact inventory differs")
    old = {"artifact-manifest.json": old_manifest_raw}
    for row in rows:
        name = row["path"]
        raw = ordinary(source / relative(name), source)
        pin(raw, row, name)
        old[name] = raw
    binding_raw = old["source-bindings.json"]
    require(digest(binding_raw) == plan["original_binding_sha256"], "v1 bindings changed")
    old_bindings = json.loads(binding_raw)
    require(old_bindings["source_file_count"] == plan["expected_original_file_count"]
            and len(old_bindings["files"]) == 333
            and len(old_bindings["required_roles"]) == plan["old_role_count"], "v1 role scope differs")
    roles_raw = ordinary(args.operator_roles, work)
    require(digest(roles_raw) == plan["original_role_file_sha256"], "original operator role file differs")
    roles = json.loads(roles_raw)
    require(set(roles) == set(old_bindings["required_roles"]), "original role keys differ")
    for value in roles.values():
        require(isinstance(value, str) and value and
                not any(part in value for part in ("\0", "\n", "\r", "@@RECOVERY_")),
                "invalid original role value")

    public_sources = {}
    for name, row in plan["sources"].items():
        raw = ordinary(fixtures / relative(name), fixtures)
        pin(raw, row, name)
        public_sources[name] = raw
    additions = {}
    for role, spec in plan["roles"].items():
        values = [fixture_value(public_sources[s["source"]], s) for s in spec["selectors"]]
        require(values and all(value == values[0] for value in values), "independent fixture sources disagree")
        pin(values[0], spec, role)
        require(role not in roles and b"@@RECOVERY_" not in values[0],
                "new fixture role overlaps original role")
        additions[role] = values[0]
        roles[role] = values[0].decode("utf-8")

    canonical_raw = ordinary(args.canonical_locations, work)
    require(digest(canonical_raw) == plan["canonical_locations_sha256"], "canonical diagnostic changed")
    canonical = json.loads(canonical_raw)
    require(canonical["status"] == "reviewable" and canonical["unsupported"] == []
            and canonical["source_commit"] == plan["publication_candidate_commit"]
            and canonical["rule_counts"] == {"machine-path-prefix-v1": 4, "value-password-v1": 5}
            and canonical["publication_authority"] is False
            and canonical["review_policy_written"] is False, "canonical finding scope differs")
    actual_rows = {row["occurrence_id"]: row for row in canonical["occurrences"]}
    require(len(actual_rows) == len(plan["regions"]) == 9, "nine exact occurrences required")
    grouped = {}
    for region in plan["regions"]:
        observed = actual_rows[region["occurrence_id"]]
        for field, value in observed.items():
            require(region.get(field) == value, "canonical region metadata differs")
        path = region["capsule_path"]
        raw = old[path]
        require(digest(raw) == region["blob_sha256"] and len(raw) == region["blob_bytes"],
                "scanner-reviewed blob differs")
        start, end = region["region_start_byte"], region["region_end_byte_exclusive"]
        value = raw[start:end]
        require(0 <= start < end <= len(raw) and digest(value) == region["region_sha256"]
                and value == additions[region["role"]], "finding is not the pinned source fixture")
        require(raw[:start].count(b"\n") + 1 == region["line"], "finding line differs")
        grouped.setdefault(path, []).append(region)

    new = dict(old)
    for path, regions in grouped.items():
        raw = old[path]
        ordered = sorted(regions, key=lambda row: row["region_start_byte"], reverse=True)
        previous = len(raw)
        for region in ordered:
            start, end = region["region_start_byte"], region["region_end_byte_exclusive"]
            require(end <= previous, "overlapping diagnostic regions")
            marker = plan["roles"][region["role"]]["marker"].encode("ascii")
            require(marker not in old[path], "fixture marker already present")
            raw = raw[:start] + marker + raw[end:]
            previous = start
        restored = raw
        for role, spec in plan["roles"].items():
            restored = restored.replace(spec["marker"].encode("ascii"), additions[role])
        require(restored == old[path], "exact v1 blob reversal failed")
        new[path] = raw

    bindings = json.loads(binding_raw)
    for role, spec in plan["roles"].items():
        bindings["required_roles"][role] = {
            "description": spec["description"], "value_kind": "exact source-defined fixture bytes",
            "public_source_recovery": "fixture-template-v2-plan.json"
        }
    changed_payloads = []
    for item in bindings["files"]:
        path = item["published_path"]
        if path in grouped:
            changed_payloads.append(path)
            for role, spec in plan["roles"].items():
                count = sum(row["role"] == role for row in grouped[path])
                if count:
                    item["substitutions"].append({
                        "marker": spec["marker"], "role": role,
                        "transform": "literal", "occurrences": count
                    })
            item["published_bytes"] = len(new[path])
            item["published_sha256"] = digest(new[path])
    require(len(changed_payloads) == 2 and len(bindings["required_roles"]) == plan["new_role_count"],
            "only two runtime payloads and two new roles admitted")
    bindings["total_role_substitutions"] = sum(
        row["occurrences"] for item in bindings["files"] for row in item["substitutions"])
    require(bindings["total_role_substitutions"] == old_bindings["total_role_substitutions"] + 8,
            "runtime payload substitution count differs")
    old_originals = [(r["original_role_path"], r["original_bytes"], r["original_sha256"])
                     for r in old_bindings["files"]]
    require(old_originals == [(r["original_role_path"], r["original_bytes"], r["original_sha256"])
                             for r in bindings["files"]], "original inventory changed")
    for item in bindings["files"]:
        reconstruct(new[item["published_path"]], item, roles)
    new["source-bindings.json"] = json_bytes(bindings)
    new_binding_sha = digest(new["source-bindings.json"])
    for name in ("reconstruct_sources.py", "materialize_from_closed_files.py"):
        previous = plan["original_binding_sha256"].encode("ascii")
        require(new[name].count(previous) == 1, "one binding pin literal required")
        new[name] = new[name].replace(previous, new_binding_sha.encode("ascii"))

    # Replace the explanatory paragraph, not other diagnostics or test data.
    readme = new["README.md"].decode("utf-8")
    paragraphs = readme.split("\n\n")
    indexes = [i for i, value in enumerate(paragraphs) if value.startswith("Часть ролей обозначает")]
    require(len(indexes) == 1, "one legacy fixture-provenance paragraph required")
    paragraphs[indexes[0]] = (
        "Часть ролей обозначает **synthetic test fixtures**. В версии v2 два дополнительных "
        "явных маркера заменяют исходный GUI home-prefix fixture и парольный diagnostic sentinel. "
        "Их значения не опубликованы в этом пакете: они восстанавливаются из точных публичных "
        "Git blobs по [плану и source provenance](fixture-template-v2-plan.json). "
        "Это источник фикстур, а не разрешение scanner. Native PID и handles не заменяются ролями."
    )
    readme = "\n\n".join(paragraphs)
    supplement = (
        "\n## Ревизия представления v2\n\n"
        "Canonical diagnostics для отклонённого commit 264a0601b7d027652c12ec88ae4debd6c3bfb69f "
        "выделили 9 точных областей в трёх файлах: 4 префикса тестового пути и 5 парольных "
        "test-sentinel assignments. Все совпали с byte-pinned исходными фикстурами. "
        "Два runtime templates теперь содержат 8 дополнительных role markers; девятый marker "
        "находится в source-fixture-provenance.json. Активный renderer требует **24 роли** "
        "(22 прежние и 2 fixture roles). Original333 bytes/SHA, capture-index, источники FAIL/CLOSED "
        "и все runtime/native numeric records неизменны.\n\n"
        "См. [точную безопасную диагностику](canonical-finding-locations.json), "
        "[план и публичные source pins](fixture-template-v2-plan.json), "
        "[DATA-преобразователь](prepare_fixture_template_v2.py) и "
        "[его квитанцию](fixture-template-v2-receipt.json). "
        "Source727 commit ref отсутствует на GitHub; каждый указанный exact source Git blob "
        "публично получен и проверен отдельно. Для исторического toggle требуется старый blob, "
        "а не изменённый текущий файл. Значения извлекаются как DATA из литералов закреплённых "
        "строк, без исполнения Go-кода. Новая private roles.json формируется отдельным "
        "explicit output; её нельзя публиковать. Для повторного получения двух fixture roles "
        "достаточны опубликованные blob pins и selector rules, исходные 22 operator roles "
        "по-прежнему предоставляются оператором.\n\n"
        "Ранее замороженный v1 не изменялся. Его authored source-fixture-provenance.json также "
        "восстанавливается побайтно одной обратной заменой нового fixture marker; исходный "
        "SHA/размер и region закреплены в плане. Исторические данные проверок v1 далее сохранены "
        "как история. Новый scanner PASS не заявлен; native publication gate остаётся обязательным. "
        "Никакие код, policy, approval records, тестовые таймауты или критерии closure не изменены.\n"
    )
    first, rest = readme.split("\n", 1)
    new["README.md"] = (first + "\n" + supplement + "\n" + rest).encode("utf-8")
    disclosure = json.loads(new["disclosure-review.json"])
    require("synthetic_password_sentinel" in disclosure, "legacy fixture disclosure missing")
    del disclosure["synthetic_password_sentinel"]
    disclosure["schema"] = "closed-full-v6-all-captures-disclosure-review-v2"
    disclosure["role_count"] = plan["new_role_count"]
    disclosure["synthetic_fixture_roles"] = sorted(plan["roles"])
    disclosure["canonical_v1_findings"] = canonical["rule_counts"]
    disclosure["canonical_locations_sha256"] = plan["canonical_locations_sha256"]
    disclosure["source_provenance"] = "fixture-template-v2-plan.json"
    disclosure["new_scanner_qualification_claimed"] = False
    disclosure["prior_pattern_checks_scope"] = "Historical author checks; canonical v1 findings are preserved separately."
    new["disclosure-review.json"] = json_bytes(disclosure)

    new["fixture-template-v2-plan.json"] = plan_raw
    new["canonical-finding-locations.json"] = canonical_raw
    new["prepare_fixture_template_v2.py"] = Path(__file__).read_bytes()
    report = {
        "schema": "closed-full-v6-fixture-template-transformation-v2",
        "status": "EXACT_333_ORIGINALS_RECONSTRUCTED",
        "historical_result": "FAIL/CLOSED",
        "original_capsule_manifest_sha256": plan["original_capsule_manifest_sha256"],
        "plan_sha256": PLAN_SHA256,
        "canonical_locations_sha256": plan["canonical_locations_sha256"],
        "source_binding_sha256": new_binding_sha,
        "original_files": 333, "changed_runtime_payloads": len(changed_payloads),
        "new_fixture_roles": sorted(plan["roles"]), "all_role_count": len(roles),
        "canonical_regions_transformed": 9,
        "all_333_original_size_and_sha256_unchanged_and_checked": True,
        "v1_author_provenance_exact_marker_reversal_checked": True,
        "public_fixture_sources": plan["sources"],
        "old_capsule_or_source_modified": False,
        "new_native_or_test_commands": False, "runtime_code_imported": False,
        "scanner_or_policy_changed": False, "publication_authority": False,
        "role_values_included": False,
        "changes": [
            {"path": name, "old_bytes": len(old[name]), "old_sha256": digest(old[name]),
             "new_bytes": len(raw), "new_sha256": digest(raw)}
            for name, raw in sorted(new.items()) if name in old and raw != old[name]
        ],
    }
    new["fixture-template-v2-receipt.json"] = json_bytes(report)
    manifest = dict(old_manifest)
    manifest["schema"] = "closed-full-v6-all-captures-artifact-manifest-v2"
    manifest["description"] = "Explicit source-fixture template revision; all runtime originals and historical FAIL/CLOSED are unchanged."
    manifest["previous_manifest_sha256"] = plan["original_capsule_manifest_sha256"]
    manifest["fixture_template_plan_sha256"] = PLAN_SHA256
    manifest["files"] = [
        {"path": name, "bytes": len(raw), "sha256": digest(raw)}
        for name, raw in sorted(new.items()) if name != "artifact-manifest.json"
    ]
    new["artifact-manifest.json"] = json_bytes(manifest)
    roles_output = json_bytes(roles)
    require(len(roles) == 24, "exact output role count differs")

    # Every original and all future public bytes have been checked before first write.
    for target in outputs:
        target.mkdir()
    for name, raw in sorted(new.items()):
        target = args.output_capsule / relative(name)
        target.parent.mkdir(parents=True, exist_ok=True)
        with target.open("xb") as stream:
            stream.write(raw)
        require(target.read_bytes() == raw, "new public artifact readback differs")
    roles_path = args.output_private / "roles.json"
    with roles_path.open("xb") as stream:
        stream.write(roles_output)
    require(roles_path.read_bytes() == roles_output, "private role readback differs")
    result = {
        "schema": "closed-full-v6-fixture-v2-preparation-result",
        "status": report["status"], "public_files": len(new), "original_files": 333,
        "artifact_manifest_sha256": digest(new["artifact-manifest.json"]),
        "source_bindings_sha256": new_binding_sha,
        "private_roles_sha256": digest(roles_output),
        "values_emitted": False, "v1_modified": False, "publication_authority": False,
    }
    with (args.output_private / "result.json").open("xb") as stream:
        stream.write(json_bytes(result))
    print(json.dumps(result))


if __name__ == "__main__":
    main()

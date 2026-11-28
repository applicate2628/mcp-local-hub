"""Create the private R21 packaging-input.json once, after publication checks.

Run unoptimized, without arguments, from the approved Windows task root. This
preparer reads only the named receipts, scripts and bounded artifact directories;
it performs no subprocess, network, scheduler, cleanup or publication operation.
Its only new file is private packaging-input.json. Output is counts and its hash.
"""
import sys
sys.dont_write_bytecode = True

import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import types

ROOT = Path(r"<RECOVERY_ROOT>")
BASE = "c55d9ca619f6f9301ba1113615370fc6218d608f"
PACKER_SHA256 = "ebb43aca4d8d899572ef3923b994abcf99aac6b23f6319c6f4b7eaa9f50ba5f5"
MANIFEST_SHA256 = "f3e12a0bed9f4024e4e00734eb35da859c23defc86c8b0963ac9469d80890534"
RUNNER_SHA256 = "6f69fc636f8ec47b725f07238f7ea2180cee9fb8efbd8e8a7275f4665ea87e61"
CHECKOUT_SHA256 = "7dd36a8c3daaf38729bd39a47f5ee2feafca259561a79d58d9ec0c2461a35e98"
OLD_MANIFEST_SHA256 = "058dc4461898c2b6301b372cf630778e3e57fa63dddd125f2f801b271a7da95b"
NATIVE_PHASES = ("native-qualification", "native-observer", "native-qualification-final", "native-retained-cleanup")
NATIVE_FILES = {"manifest.json", "observations.jsonl", "export-1.xml", "export-2.xml"}
OBSERVER_FILES = {"intent.json", "observation.json", "comparison.json", "retained-task.xml"}
SETUP_RECEIPT = "diagnostics/native-preflight/evidence/receipt.json"
IDENTITY_ENV = ("USERNAME", "USERDOMAIN", "COMPUTERNAME", "USERPROFILE")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def load_packer():
    path = ROOT / "pack_public.py"
    info = path.lstat()
    require(stat.S_ISREG(info.st_mode) and not getattr(info, "st_file_attributes", 0) & 0x400 and info.st_size <= 1024 * 1024, "Expected the ordinary reviewed packer file")
    raw = path.read_bytes()
    require(digest(raw) == PACKER_SHA256, "Packer bytes differ from the reviewed version")
    # Execute only the exact hash-verified module bytes, with a non-main name.
    # Its reviewed import body defines helpers; it never invokes packer.main().
    module = types.ModuleType("r21_packer_for_private_input")
    module.__file__ = str(path)
    exec(compile(raw, str(path), "exec"), module.__dict__)
    require(module.ROOT == ROOT and module.BASE == BASE, "Packer task identity differs")
    return module


def children(packer, name):
    path, info = packer.checked_path(name)
    require(stat.S_ISDIR(info.st_mode), "Expected an ordinary artifact directory")
    result = sorted(child.name for child in path.iterdir())
    require(len(result) <= 2048 and all(re.fullmatch(r"[A-Za-z0-9_.-]+", leaf) for leaf in result), "Artifact directory inventory is outside the reviewed bound")
    packer.inventory(name, result)
    return result


def native_root_names():
    # Only this one directory level is listed; unrelated children are not read.
    result = sorted(path.name for path in ROOT.iterdir() if re.fullmatch(r"native-r21-[0-9a-f]{32}", path.name))
    require(1 <= len(result) <= 16, "Expected a bounded retained native-run inventory")
    return result


def redaction_mappings(manifests, environment):
    rows, seen, counters = [], set(), {}
    builtins = {str(ROOT).casefold(), r"<WORK_ROOT>", r"<PREVIOUS_RECOVERY_ROOT>"}

    def add(value, label):
        if value is None or value == "":
            return
        require(isinstance(value, str), "Private identity must be a string")
        if value.casefold() in {"system", "workgroup", "nt authority"} or value.casefold() in builtins:
            return
        require(value == value.strip() and 3 <= len(value) <= 512 and not any(c in value for c in "\r\n\0"), "Private identity is not a bounded concrete value")
        if value.casefold() in seen:
            return
        seen.add(value.casefold())
        counters[label] = counters.get(label, 0) + 1
        rows.append({"literal": value, "replacement": "<" + label + "_%02d>" % counters[label]})

    for manifest in manifests:
        require(isinstance(manifest.get("account"), str) and manifest["account"] and isinstance(manifest.get("sid"), str) and re.fullmatch(r"S-\d+(?:-\d+)+", manifest["sid"]), "Native manifest lacks its concrete account/SID")
        add(manifest["account"], "NATIVE_PRINCIPAL")
        add(manifest["sid"], "NATIVE_SID")
    for key, label in (("USERNAME", "USER_NAME"), ("USERDOMAIN", "USER_DOMAIN"), ("COMPUTERNAME", "HOST_NAME"), ("USERPROFILE", "USER_PROFILE")):
        add(environment.get(key), label)
    require(len(rows) <= 32, "Too many private identity mappings")
    return rows


def prepare(packer, environment):
    known = {"source-manifest.json": MANIFEST_SHA256, "check_recovery.py": RUNNER_SHA256, "checkout_published.py": CHECKOUT_SHA256}
    for name, expected in known.items():
        require(digest(packer.read(name)) == expected, "Qualification input changed after freeze")
    manifest = packer.parse(packer.inputs["source-manifest.json"])
    publication_raw = packer.read("publication-input.json")
    publication = packer.parse(publication_raw)
    require(set(publication) == {"schema", "base", "commit", "tree", "parents", "checkout_runner_sha256", "execution_runner_sha256", "source_manifest_sha256"} and publication["schema"] == 1 and publication["base"] == BASE and publication["parents"] == [BASE] and publication["commit"] != BASE, "Publication is not the reviewed single-parent child")
    for key in ("commit", "tree"):
        packer.sha(publication[key], 40)
    require(publication["source_manifest_sha256"] == MANIFEST_SHA256 and publication["execution_runner_sha256"] == RUNNER_SHA256 and publication["checkout_runner_sha256"] == CHECKOUT_SHA256, "Publication source/runner pins differ")
    finished = []
    for phase in packer.PHASES:
        receipt = packer.parse(packer.read("evidence/" + phase + "/receipt.json"))
        require(receipt.get("schema") == 2 and receipt.get("phase") == phase and receipt.get("base") == BASE and receipt.get("completed") is True and receipt.get("state") == "complete" and not receipt.get("error") and not receipt.get("process_tree_unresolved"), "All six qualification phases must have completed")
        require(receipt.get("manifest_sha256") == MANIFEST_SHA256 and receipt.get("runner_sha256") == RUNNER_SHA256, "Qualification receipt pins differ")
        state = "control" if phase == "control" else "published" if phase in {"published-checkout", "published"} else "candidate"
        packer.source(receipt["source_after"], state, manifest, publication)
        require(receipt.get("source_final") == receipt["source_after"], "Qualification final source observation differs")
        if phase in {"published-checkout", "published"}:
            require(receipt.get("publication_input_sha256") == digest(publication_raw), "Published receipt is bound to another publication input")
        finished.append(packer.timestamp(receipt["finished_utc"]))
    old_raw = packer.read("diagnostics/initial/inputs/source-manifest.json")
    require(digest(old_raw) == OLD_MANIFEST_SHA256, "Initial source manifest changed")
    old_receipts = {}
    for phase in ("control", "candidate"):
        raw = packer.read("diagnostics/initial/evidence/" + phase + "/receipt.json")
        receipt = packer.parse(raw)
        require(receipt.get("phase") == phase and receipt.get("manifest_sha256") == OLD_MANIFEST_SHA256 and receipt.get("runner_sha256") == RUNNER_SHA256 and receipt.get("completed") is (phase == "control") and receipt.get("state") == ("complete" if phase == "control" else "failed"), "Initial attempted receipt identity/state differs")
        old_receipts[phase] = digest(raw)
    amendment_raw = packer.read("test-assertion-amendment.json")
    amendment = packer.parse(amendment_raw)
    require(amendment.get("old_manifest_sha256") == OLD_MANIFEST_SHA256 and amendment.get("new_manifest_sha256") == MANIFEST_SHA256 and amendment.get("product_changes") is False and amendment.get("changed_files") == ["internal/api/install_task_recovery_test.go"], "Initial test-assertion amendment differs")
    public, private = [], []

    def add(name):
        raw = packer.read(name)
        if name.endswith(".xml"):
            private.append({"path": name, "sha256": digest(raw), "bytes": len(raw)})
        else:
            format_name = "jsonl" if name.endswith(".jsonl") else "json" if name.endswith(".json") else "text"
            require(format_name != "text" or name.endswith(".log"), "Unexpected supplemental file extension")
            public.append({"path": name, "sha256": digest(raw), "format": format_name})

    # This historical failure is mandatory. Its duplicate private log directory
    # is not enumerated; the packer records that those streams are not reverified.
    add(SETUP_RECEIPT)
    present_phases = []
    for phase in NATIVE_PHASES:
        directory = "evidence/" + phase
        if not (ROOT / directory).exists():
            continue
        names = children(packer, directory)
        require("receipt.json" in names and all(name.endswith((".json", ".log")) for name in names), "Native phase lacks its retained receipt or contains unexpected files")
        for name in names:
            add(directory + "/" + name)
        # A failed phase is valid retained evidence. No PASS/cleanup inference.
        present_phases.append(phase)
    require("native-qualification" in present_phases, "Original native attempt must be retained")
    packer.inventory("evidence", {*packer.PHASES, *present_phases})
    roots, native_manifests = native_root_names(), []
    for directory in roots:
        names = children(packer, directory)
        require(set(names) <= NATIVE_FILES and {"manifest.json", "observations.jsonl"} <= set(names), "Native run directory has unknown files or lacks its retained manifest/events")
        for name in names:
            add(directory + "/" + name)
        native_manifests.append(packer.parse(packer.inputs[directory + "/manifest.json"]))
    observation_dir = "diagnostics/native-observation"
    if (ROOT / observation_dir).exists():
        names = children(packer, observation_dir)
        require(set(names) <= OBSERVER_FILES, "Observer directory contains an unreviewed file")
        for name in names:
            add(observation_dir + "/" + name)
    public.sort(key=lambda entry: entry["path"])
    private.sort(key=lambda entry: entry["path"])
    mappings = redaction_mappings(native_manifests, environment)
    packer.supplemental(public, private)
    concrete = [(str(ROOT), "<RECOVERY_ROOT>"), (str(packer.OLD.parent), "<INSTALL_SNAPSHOT_ROOT>"), (r"<WORK_ROOT>", "<WORK_ROOT>")]
    concrete += [(row["literal"], row["replacement"]) for row in mappings]
    for entry in public:
        redacted = packer.redact(packer.inputs[entry["path"]], concrete)
        if entry["format"] == "json":
            packer.parse(redacted)
        elif entry["format"] == "jsonl":
            for line in redacted.splitlines():
                packer.parse(line)
    # The complete selected byte inventory and directory inventories are checked
    # again immediately before the one permitted new private file is created.
    for name in tuple(packer.inputs):
        packer.read(name)
    for name, names in tuple(packer.inventories.items()):
        packer.inventory(name, names)
    require(native_root_names() == roots, "Native run inventory changed during preparation")
    observed = dt.datetime.now(dt.timezone.utc)
    require(all(value <= observed for value in finished), "Observation predates a qualification completion")
    config = {"schema": 1, "observed_at_utc": observed.isoformat(), "pins": {"source_manifest_sha256": MANIFEST_SHA256, "execution_runner_sha256": RUNNER_SHA256, "checkout_runner_sha256": CHECKOUT_SHA256, "publication_input_sha256": digest(publication_raw)}, "redactions": mappings, "diagnostics": {"initial": {"manifest_sha256": OLD_MANIFEST_SHA256, "receipt_sha256": old_receipts, "amendment_sha256": digest(amendment_raw)}}, "extra_public_files": public, "private_artifacts": private}
    counts = {"qualification_phases": len(packer.PHASES), "native_phase_directories": len(present_phases), "native_run_directories": len(roots), "public_supplemental_files": len(public), "private_artifacts": len(private), "private_redaction_mappings": len(mappings)}
    return packer.encode(config), counts


def main():
    require(os.name == "nt" and not sys.flags.optimize and len(sys.argv) == 1 and Path(__file__).resolve() == ROOT / "prepare_pack_input.py", "Use the reviewed preparer unoptimized from the approved Windows root")
    output = ROOT / "packaging-input.json"
    require(not output.exists(), "Retain the existing private packaging input; overwrite is prohibited")
    packer = load_packer()
    raw, counts = prepare(packer, {key: os.environ.get(key, "") for key in IDENTITY_ENV})
    with output.open("xb") as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    require(output.read_bytes() == raw, "Saved private input differs from verified memory")
    print(json.dumps({"counts": counts, "packaging_input_sha256": digest(raw)}, sort_keys=True), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Do not print private config, principal values or an environment dump.
        print(json.dumps({"preparation_failed": True, "error_type": type(error).__name__}), file=sys.stderr, flush=True)
        raise SystemExit(1)

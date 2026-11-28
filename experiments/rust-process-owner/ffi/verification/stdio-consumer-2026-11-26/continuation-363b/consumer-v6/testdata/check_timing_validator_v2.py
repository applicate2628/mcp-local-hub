"""Pure parser checks. No project imports, process launch, native call or writes.

Arguments: old runner, candidate runner, original v5 stdout capture.
Only named pure AST definitions are evaluated. Synthetic values exist only in
temporary test dictionaries; the original capture and v5 FAIL remain unchanged.
"""
import ast
import copy
import hashlib
import json
from pathlib import Path
import sys
from types import SimpleNamespace

FIELDS = ("start_ns", "rpc_ns", "stop_ns", "case_ns")
SPEC = SimpleNamespace(
    COMMIT="3ebe335a2f666a5279d7fafc31a6f4dc3feedff4",
    DLL_SHA="dfa87a10a39e9110cda735ae6a987bfa64b03446529072710723e9b6ea0dca56",
    CASES=(("healthy-after-canceled-observers", 64), ("final-reply", 1), ("lifetime-cancel", 1)),
)

def functions(path, names):
    tree = ast.parse(Path(path).read_text(encoding="utf-8"))
    definitions = [node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name in names]
    if "require" in names and not any(node.name == "require" for node in definitions):
        lifecycle_raw = Path(path).with_name("focused_runner.py").read_bytes()
        assert hashlib.sha256(lifecycle_raw).hexdigest() == "7c509aefed299c536587455c4ebe146b0f1666e9e3f16381a62491c7f271f517"
        lifecycle_tree = ast.parse(lifecycle_raw.decode("utf-8"))
        definitions = [node for node in lifecycle_tree.body
                       if isinstance(node, ast.FunctionDef) and node.name == "require"] + definitions
    assert {node.name for node in definitions} == set(names)
    namespace = {"json": json, "spec": SPEC}
    exec(compile(ast.Module(body=definitions, type_ignores=[]), "pure-parser-slice", "exec"), namespace)
    return namespace

old = functions(sys.argv[1], ("require", "receive_consumer"))
new = functions(sys.argv[2], ("require", "receive_consumer", "elapsed_timing_metadata"))
raw = Path(sys.argv[3]).read_bytes()
assert hashlib.sha256(raw).hexdigest() == "1400dc786b109bbad1004808e83f090675a1bce5c3b48133ebaa6c20aacb70aa"
prefix = b"RUST_ADOPTION_RECEIPT "
original = json.loads(next(line[len(prefix):] for line in raw.splitlines() if line.startswith(prefix)))
helper = original["helper_sha256"]
checks = []

def reject(name, fn, data, expected=None):
    try:
        fn(data, "go", helper)
    except (RuntimeError, KeyError) as error:
        if expected is not None:
            assert str(error) == expected
    else:
        raise AssertionError(name + " unexpectedly accepted")
    checks.append(name)

def variant(record):
    return b"".join(prefix + json.dumps(record, separators=(",", ":")).encode() + b"\n" if line.startswith(prefix) else line for line in raw.splitlines(keepends=True))

reject("original-v5-parser-rejects-actual-zero-samples", old["receive_consumer"], raw, "Actual positive timings required")
actual = new["receive_consumer"](raw, "go", helper)
assert actual == original
metadata = new["elapsed_timing_metadata"](actual)
assert actual == original
assert metadata["zero_samples"] == [
    {"case": "final-reply", "metric": "rpc_ns", "raw_value": 0},
    {"case": "lifetime-cancel", "metric": "rpc_ns", "raw_value": 0},
]
assert metadata["zero_interpretation"] == "resolution_unresolved"
assert metadata["clock_quantum_measured"] is False and metadata["zero_latency_claimed"] is False
assert metadata["speed_ratio_from_zero_permitted"] is False
checks.append("candidate-retains-actual-zeros-and-distinct-controller-metadata")

synthetic_positive = copy.deepcopy(original)
for row in synthetic_positive["cases"]:
    for name in FIELDS:
        if row[name] == 0:
            row[name] = 1000000  # Explicit synthetic parser fixture, never a measured result.
positive = new["receive_consumer"](variant(synthetic_positive), "go", helper)
assert new["elapsed_timing_metadata"](positive)["zero_samples"] == []
checks.append("synthetic-positive-integers-accepted")

synthetic_zero = copy.deepcopy(original)
for row in synthetic_zero["cases"]:
    for name in FIELDS:
        row[name] = 0
zero = new["receive_consumer"](variant(synthetic_zero), "go", helper)
assert len(new["elapsed_timing_metadata"](zero)["zero_samples"]) == 12
checks.append("synthetic-all-zero-fields-retained-and-classified")

for label, value in (("negative", -1), ("float", 0.0), ("boolean", False), ("string", "0"), ("null", None)):
    record = copy.deepcopy(original)
    record["cases"][0]["rpc_ns"] = value
    reject("reject-" + label + "-elapsed", new["receive_consumer"], variant(record))
for name in FIELDS:
    record = copy.deepcopy(original)
    del record["cases"][0][name]
    reject("reject-missing-" + name, new["receive_consumer"], variant(record))
for name in ("handles_closed", "same_stop_deadline", "received_before_stop_deadline"):
    record = copy.deepcopy(original)
    record["cases"][0][name] = False
    reject("preserve-" + name + "-guard", new["receive_consumer"], variant(record))
record = copy.deepcopy(original)
record["source_commit"] = "0" * 40
reject("preserve-source-pin-guard", new["receive_consumer"], variant(record))
record = copy.deepcopy(original)
record["cases"][1]["original_exit_code"] = 1
reject("preserve-natural-exit-guard", new["receive_consumer"], variant(record))
record = copy.deepcopy(original)
record["cases"][1]["original_identity"] = copy.deepcopy(record["cases"][0]["original_identity"])
reject("preserve-three-original-identities-guard", new["receive_consumer"], variant(record))
assert hashlib.sha256(Path(sys.argv[3]).read_bytes()).hexdigest() == "1400dc786b109bbad1004808e83f090675a1bce5c3b48133ebaa6c20aacb70aa"
print(json.dumps({"schema": "consumer-timing-parser-checks-v1", "status": "PASS", "checks": checks, "checks_count": len(checks), "native_processes_started": False, "original_v5_fail_reclassified": False, "synthetic_values_published_as_measurements": False}))

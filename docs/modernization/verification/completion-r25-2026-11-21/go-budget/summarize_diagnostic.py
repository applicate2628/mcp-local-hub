#!/usr/bin/env python3
"""Reduce one R25 instrumented Go root; never execute repository code."""

import argparse
import collections
import hashlib
import json
import re
import statistics
from pathlib import Path


MARKER = re.compile(r"R25_GO_BUDGET phase=([a-z-]+) elapsed_ns=(\d+)")


def summarize(raw, selection, exit_code):
    root = selection["root"]
    package = selection["package"]
    expected = {root: selection["phases"]["root"]}
    for case in selection["cases"]:
        phases = selection["phases"]["all_cases"][:]
        if case["durable"]:
            phases += selection["phases"]["invalid_cases"]
        expected[root + "/" + case["name"]] = phases

    problems = []
    runs = collections.Counter()
    terminals = collections.defaultdict(list)
    measured = collections.defaultdict(dict)
    event_times = {}
    package_terminals = []
    for line_number, line in enumerate(raw.splitlines(), 1):
        try:
            event = json.loads(line)
            if not isinstance(event, dict):
                raise ValueError("event is not an object")
        except (ValueError, UnicodeDecodeError) as exc:
            problems.append(f"line {line_number}: invalid JSON event: {exc}")
            continue
        action, test = event.get("Action"), event.get("Test", "")
        output = event.get("Output", "")
        if not isinstance(action, str) or not isinstance(test, str) or not isinstance(output, str):
            problems.append(f"line {line_number}: invalid event field types")
            continue
        if action in ("build-fail", "fail", "skip"):
            problems.append(f"line {line_number}: {action}: {test or event.get('Package', '')}")
        if test:
            if event.get("Package") != package or test not in expected:
                problems.append(f"line {line_number}: unexpected test or package: {test}")
                continue
            if action == "run":
                runs[test] += 1
                event_times.setdefault(test, {})["run"] = event.get("Time")
            elif action in ("pass", "fail", "skip"):
                terminals[test].append(action)
                event_times.setdefault(test, {}).update(
                    terminal=event.get("Time"), go_elapsed_seconds=event.get("Elapsed")
                )
        elif action in ("pass", "fail", "skip"):
            if event.get("Package") != package:
                problems.append(f"line {line_number}: unexpected package terminal")
            package_terminals.append(event)
        if "R25_GO_BUDGET" not in output:
            continue
        matches = MARKER.findall(output)
        if action != "output" or len(matches) != 1 or test not in expected:
            problems.append(f"line {line_number}: malformed phase output")
            continue
        phase, nanoseconds = matches[0]
        if phase not in expected[test] or phase in measured[test]:
            problems.append(f"line {line_number}: unexpected or duplicate phase: {test}/{phase}")
            continue
        measured[test][phase] = int(nanoseconds)

    for test, phases in expected.items():
        if runs[test] != 1 or terminals[test] != ["pass"]:
            problems.append(f"incomplete test lifecycle: {test}: runs={runs[test]}, terminals={terminals[test]}")
        missing = set(phases) - measured[test].keys()
        if missing:
            problems.append(f"missing phases: {test}: {sorted(missing)}")
    if len(package_terminals) != 1 or package_terminals[0].get("Action") != "pass":
        problems.append("exactly one passing package terminal is required")
    if exit_code != 0:
        problems.append(f"command exit code is {exit_code}")

    by_phase = collections.defaultdict(list)
    records = []
    for test in expected:
        for phase, nanoseconds in measured[test].items():
            by_phase[phase].append(nanoseconds)
            records.append({"test": test, "phase": phase, "elapsed_ns": nanoseconds})
    aggregates = {}
    for phase, values in by_phase.items():
        aggregates[phase] = {
            "count": len(values), "sum_ns": sum(values),
            "minimum_ns": min(values), "median_ns": statistics.median(values),
            "maximum_ns": max(values),
        }
    return {
        "schema": "r25-go-budget-diagnostic-result-v1",
        "complete_selection": not problems,
        "problems": problems, "exit_code": exit_code,
        "log_bytes": len(raw), "log_sha256": hashlib.sha256(raw).hexdigest(),
        "declared_source_ref": selection["source_ref"],
        "declared_overlay": selection["overlay"],
        "source_execution_binding_verified_by_this_reducer": False,
        "expected_roots": 1, "expected_cases": selection["case_count"],
        "expected_durable_cases": selection["durable_case_count"],
        "expected_phase_records": sum(map(len, expected.values())),
        "observed_phase_records": len(records),
        "phase_aggregates": aggregates, "test_events": event_times,
        "package_terminals": package_terminals, "phase_records": records,
        "limits": [
            "Fixture-write includes the complete WriteStateFileAtomic call, not FlushFileBuffers alone.",
            "Logging, assertions, uninstrumented setup, test cleanup and TestMain are outside these stage timers.",
            "Do not subtract stage sums from rounded Go Elapsed to infer CPU time or idle time.",
            "A single instrumented sample is not a speedup, stable budget or full-suite acceptance result.",
            "Collector source, environment, command and settlement receipts remain separately required.",
        ],
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--events", type=Path, required=True)
    parser.add_argument("--selection", type=Path, required=True)
    parser.add_argument("--exit-code", type=int, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.events.stat().st_size > 64 << 20:
        parser.error("event stream exceeds the 64 MiB diagnostic bound; preserve the original")
    raw = args.events.read_bytes()
    selection = json.loads(args.selection.read_text(encoding="utf-8"))
    result = summarize(raw, selection, args.exit_code)
    with args.output.open("x", encoding="utf-8") as stream:
        json.dump(result, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
    print(json.dumps({key: result[key] for key in (
        "complete_selection", "expected_cases", "expected_phase_records", "observed_phase_records"
    )}))
    return 0 if result["complete_selection"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

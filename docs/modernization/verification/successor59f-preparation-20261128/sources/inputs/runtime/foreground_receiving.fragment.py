# Typed DATA receiving for the pinned Source foreground owner; no native action.
FOREGROUND_CLEANUP_TESTS = (
    "TestCanonicalGUIRuntimeRecoveryNativeIndependentReplay",
    "TestCanonicalGUIRuntimeRecoveryNativeIndependentReplay/cleanup-uses-current-operation-context",
    "TestCanonicalSuccessorNativeControlRetainsOriginalFiles",
    "TestCanonicalSuccessorNativeControlRetainsOriginalFiles/absent",
    "TestCanonicalSuccessorNativeControlRetainsOriginalFiles/identical",
    "TestCanonicalSuccessorNativeControlRetainsOriginalFiles/different-identity",
    "TestStartSupervisorDetachedBreakaway_AddsFlagOnSuccess",
)

def validate_foreground_terminal(proof, binding):
    """Receive original closure only; snapshots convey no process authority."""
    def check(ok, message):
        if not ok:
            raise RuntimeError("original foreground " + message)
    keys = {"request_sha256", "request_nonce", "binary", "expected_sha256", "source_dir",
            "run_root", "args", "controller_argv"}
    check(isinstance(proof, dict) and isinstance(binding, dict) and set(binding) == keys,
          "terminal binding is incomplete")
    check(all(isinstance(binding[key], str) and binding[key]
              for key in keys - {"args", "controller_argv"})
          and re.fullmatch("[0-9a-f]{64}", binding["request_sha256"])
          and re.fullmatch("[0-9a-f]{32}", binding["request_nonce"])
          and re.fullmatch("[0-9a-f]{64}", binding["expected_sha256"])
          and isinstance(binding["args"], list) and flags_are_literal(binding["args"])
          and isinstance(binding["controller_argv"], list) and binding["controller_argv"],
          "request identity or exact Go arguments differ")
    expected_argv = [binding["controller_argv"][0], "--test-binary", binding["binary"],
                     "--expected-sha256", binding["expected_sha256"], "--source-dir", binding["source_dir"],
                     "--run-root", binding["run_root"], "--timeout", "5m", "--", *binding["args"]]
    check(all(isinstance(value, str) and value for value in expected_argv)
          and binding["controller_argv"] == expected_argv, "controller arguments differ")
    check(proof.get("schema") == "literal-foreground-cli-run-v1"
          and proof.get("mode") == "incoming-go-cli-foreground"
          and proof.get("native_scope") == "original-root-and-original-job-members"
          and proof.get("escaped_cleanup_scope") == "source-pinned-test-owned-original-wait-and-release-path"
          and proof.get("nested_close_success_receipt") is False
          and proof.get("binary") == binding["binary"]
          and proof.get("expected_sha256") == binding["expected_sha256"]
          and proof.get("binary_sha256") == binding["expected_sha256"]
          and proof.get("source_dir") == binding["source_dir"]
          and proof.get("run_root") == binding["run_root"] and proof.get("args") == binding["args"]
          and proof.get("test_timeout") == "5m0s" and proof.get("bootstrap_budget") == "5s",
          "receipt scope or immutable invocation differs")
    check(all(proof.get(key) is True for key in
              ("native_complete", "io_complete", "sinks_finalized", "settlement_complete",
               "physical_complete", "capture_access", "path_pins_closed"))
          and proof.get("custody_retained") is False and proof.get("cancelled") is False
          and proof.get("qualification") in ("PASS", "FAIL")
          and type(proof.get("root_exit_code")) is int,
          "native, IO, or retained custody remains incomplete")
    from datetime import datetime
    times = {}
    for key in ("started_utc", "finished_utc", "work_deadline", "cleanup_deadline"):
        value = proof.get(key)
        check(isinstance(value, str) and value.endswith("Z"), "deadline metadata is not UTC")
        times[key] = datetime.fromisoformat(value[:-1] + "+00:00")
    check((times["work_deadline"] - times["started_utc"]).total_seconds() == 305
          and (times["cleanup_deadline"] - times["work_deadline"]).total_seconds() == 10
          and times["finished_utc"] >= times["started_utc"],
          "original fixed work or cleanup deadline differs")

    native, root, outcome = proof.get("native"), proof.get("root"), proof.get("outcome")
    check(isinstance(native, dict) and isinstance(root, dict) and isinstance(outcome, dict),
          "native owner snapshots are absent")
    check(type(native.get("NoChildKnown")) is bool and native.get("HandlesClosed") is True
          and native.get("Error") is None and outcome.get("Pending") in (None, [])
          and outcome.get("AcquisitionEntered") is True and outcome.get("JobAcquired") is True
          and outcome.get("OriginalAcquired") is True and outcome.get("ChildObservationKnown") is True
          and outcome.get("Identity") == root
          and outcome.get("AdmissionPresent") is False
          and outcome.get("AdmissionCloseAttempted") is False
          and outcome.get("AdmissionCloseError") is None,
          "original foreground owner or release snapshot differs")
    no_child = native["NoChildKnown"]
    if no_child:
        check(root.get("Known") is False and proof.get("root_wait_known") is False
              and proof.get("qualification") == "FAIL", "no-child closure cannot qualify a suite")
    else:
        check(root.get("Known") is True and type(root.get("PID")) is int and root["PID"] > 0
              and isinstance(root.get("StartedAt"), str) and root["StartedAt"]
              and type(root.get("NativeBirthFILETIME")) is int and root["NativeBirthFILETIME"] > 0
              and native.get("RootIdentity") == root and native.get("RootWaitKnown") is True
              and proof.get("root_wait_known") is True
              and native.get("RootExitCode") == proof["root_exit_code"]
              and native.get("MembersExited") is True and native.get("AccountingKnown") is True
              and type(native.get("FinalActive")) is int and native["FinalActive"] == 0
              and outcome.get("IdentityError") is None,
              "original root wait, birth, members or accounting remains unproved")

    for name in ("JobClose", "ProcessClose", "ThreadClose", "WrapperClose"):
        slot = outcome.get(name)
        check(isinstance(slot, dict) and slot.get("AcquiredKnown") is True
              and type(slot.get("Acquired")) is bool and slot.get("Error") is None,
              "native close observation is unknown: " + name)
        if slot["Acquired"]:
            check(all(slot.get(key) is True for key in ("CloseAttempted", "ClosedKnown", "Closed")),
                  "native close is unreceived: " + name)
    inherited = outcome.get("Inherited")
    check(inherited is None or isinstance(inherited, list), "inherited-handle inventory is invalid")
    for slot in inherited or []:
        check(isinstance(slot, dict) and slot.get("CloseAttempted") is True
              and slot.get("ClosedKnown") is True and type(slot.get("OwnedKnown")) is bool
              and slot.get("NativeError") is None and slot.get("ReportedError") is None,
              "original inherited-handle close remains unknown")
    check(type(outcome.get("AttributesPresent")) is bool, "attribute ownership is unknown")
    if outcome["AttributesPresent"]:
        attrs = outcome.get("Attributes")
        check(isinstance(attrs, dict) and attrs.get("Returned") is True
              and attrs.get("AllocationKnown") is True and attrs.get("Error") is None
              and type(attrs.get("Allocated")) is bool and type(attrs.get("Initialized")) is bool,
              "original attribute-list close is unknown")
        if attrs["Allocated"]:
            check(attrs.get("FreeAttempted") is True and attrs.get("Freed") is True
                  and (attrs["Initialized"] is False
                       or attrs.get("DeleteAttempted") is True and attrs.get("DeleteReturned") is True),
                  "original attribute-list resource remains retained")
    check(type(outcome.get("MemberObservationPresent")) is bool, "member observation ownership is unknown")
    if outcome["MemberObservationPresent"]:
        members = outcome.get("Members")
        check(isinstance(members, dict) and outcome.get("MemberPolicyError") is None
              and "members" in members and (members["members"] is None or isinstance(members["members"], list))
              and all(isinstance(row, dict) and row.get("handle_closed") is True
                      for row in members["members"] or []), "original member handle remains unclosed")

    io_record = proof.get("io")
    check(isinstance(io_record, dict), "IO receipt is absent")
    for name in ("Stdin", "StdoutRead", "StdoutWrite", "StderrRead", "StderrWrite"):
        slot = io_record.get(name)
        check(isinstance(slot, dict) and all(slot.get(key) is True for key in
              ("Acquired", "CloseAttempted", "Closed")) and slot.get("CloseError") is None,
              "original standard-file close is unreceived: " + name)
    for name in ("Stdout", "Stderr"):
        stream = io_record.get(name)
        check(isinstance(stream, dict) and all(stream.get(key) is True for key in
              ("WorkerStarted", "WorkerComplete", "EOF")) and stream.get("Error") is None
              and stream.get("LimitExceeded") is False and stream.get("Limit") == 16 << 20
              and type(stream.get("Bytes")) is int and 0 <= stream["Bytes"] <= 16 << 20,
              "original reader, EOF or capture bound remains unreceived: " + name)
    for name in ("stdout_sink", "stderr_sink"):
        slot = proof.get(name)
        check(isinstance(slot, dict) and all(slot.get(key) is True for key in
              ("Acquired", "CloseAttempted", "Closed")) and slot.get("CloseError") is None,
              "original sink finalization is unreceived: " + name)

    pins = proof.get("path_pins")
    expected_pins = [(binding["binary"], False), (binding["source_dir"], True),
                     (str(Path(binding["run_root"]).parent), True), (binding["run_root"], True)]
    check(isinstance(pins, list) and len(pins) == len(expected_pins),
          "original path/image pin inventory is incomplete")
    for pin, (path, directory) in zip(pins, expected_pins):
        acquired = len(Path(path).parents) + 1
        check(isinstance(pin, dict) and set(pin) == {"path", "directory", "acquired", "closes"}
              and pin["path"] == path and pin["directory"] is directory
              and type(pin["acquired"]) is int and pin["acquired"] == acquired
              and isinstance(pin["closes"], list) and len(pin["closes"]) == acquired,
              "original ancestor or image pin inventory differs")
        for slot, expected_slot in zip(pin["closes"], range(acquired - 1, -1, -1)):
            check(isinstance(slot, dict) and set(slot) in
                  ({"slot", "attempted", "closed"}, {"slot", "attempted", "closed", "error"})
                  and type(slot["slot"]) is int and slot["slot"] == expected_slot
                  and slot["attempted"] is True and slot["closed"] is True
                  and slot.get("error", "") == "", "original path/image pin close remains unreceived")

    counts = proof.get("cleanup_tests")
    check(isinstance(counts, dict) and set(counts) == set(FOREGROUND_CLEANUP_TESTS)
          and all(isinstance(row, dict) and set(row) == {"Runs", "Passes", "Fails", "Skips"}
                  and all(type(value) is int and value >= 0 for value in row.values())
                  for row in counts.values()), "escaped-cleanup test observations are incomplete")
    cleanup_passed = all(row == {"Runs": 1, "Passes": 1, "Fails": 0, "Skips": 0}
                         for row in counts.values())
    check(type(proof.get("escaped_cleanup_proved")) is bool
          and proof["escaped_cleanup_proved"] is cleanup_passed
          and (no_child or cleanup_passed), "escaped-actor cleanup is unproved")
    return "no-child" if no_child else "suite"

def foreground_qualification(proof, binding):
    return (validate_foreground_terminal(proof, binding) == "suite"
            and proof.get("qualification") == "PASS" and proof.get("root_exit_code") == 0
            and proof.get("binary_after_measured") is True and proof.get("binary_unchanged") is True
            and not proof.get("native_error") and not proof.get("failure"))

def foreground_failed_receiver(argv, cwd, proof_path, binding):
    """Freeze one original callback and share its control reader with normal receiving."""
    expected_argv, expected_cwd = [str(value) for value in argv], str(cwd)
    bound = json.loads(json.dumps(binding))
    proof_path = Path(proof_path)
    if (expected_argv != bound["controller_argv"] or expected_cwd != bound["source_dir"]
            or proof_path != Path(bound["run_root"]) / "result.json"):
        raise RuntimeError("foreground receiving requires the prebound original invocation")
    metadata_files, failure, received = [], None, None
    unread = object()
    original_result, original_bytes = unread, None
    def read_original_result():
        nonlocal failure, original_result, original_bytes
        if failure is not None:
            raise RuntimeError("original foreground reader retains its first error") from failure
        if original_result is not unread:
            return original_result
        try:
            handle = proof_path.open("rb")
            metadata_files.append(handle)
            try:
                original_bytes = handle.read((16 << 20) + 1)
            finally:
                handle.close()
            if not handle.closed or len(original_bytes) > 16 << 20:
                raise RuntimeError("original foreground control read/close is incomplete")
            original_result = json.loads(original_bytes, object_pairs_hook=core.unique_object)
            return original_result
        except BaseException as error:
            failure = error
            raise
    def original_result_sha256():
        read_original_result()
        return hashlib.sha256(original_bytes).hexdigest()
    def receive(row, direct_exit_code):
        nonlocal failure, received
        if failure is not None:
            raise RuntimeError("original failed foreground receiver retains its first error") from failure
        if received is not None:
            return received
        try:
            if (row.get("argv") != expected_argv or row.get("cwd") != expected_cwd
                    or type(row.get("original_pid")) is not int or row["original_pid"] <= 0
                    or type(direct_exit_code) is not int):
                raise RuntimeError("late foreground receiving differs from its prebound invocation")
            proof = read_original_result()
            terminal = validate_foreground_terminal(proof, bound)
            if terminal == "no-child" and direct_exit_code == 0:
                raise RuntimeError("closed foreground creation failure requires its original nonzero exit")
            if not all(handle.closed for handle in metadata_files):
                raise RuntimeError("original foreground metadata reader remains open")
            received = {"schema": "foreground-original-physical-closure-v1", "status": "CLOSED",
                "physical_complete": True, "terminal_kind": terminal,
                "original_controller_pid": row["original_pid"], "original_direct_exit_code": direct_exit_code,
                "original_receipt": {"path": str(proof_path), "sha256": original_result_sha256()},
                "proof": proof, "metadata_handles_closed": True, "qualification_passed": False,
                "capture_access_issued": False, "source_or_cache_access_issued": False,
                "new_commands_permitted": False, "qualification_deadlines_extended": False}
            return received
        except BaseException as error:
            failure = error
            raise
    receive.read_original_result = read_original_result
    receive.original_result_sha256 = original_result_sha256
    return receive

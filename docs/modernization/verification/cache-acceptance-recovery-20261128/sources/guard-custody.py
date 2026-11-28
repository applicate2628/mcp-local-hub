"""Ordinary tools retain their original native owner and stream workers.

Source candidate. Named/deferred owners stay outside a Job.
Their actual Popen and original files survive timeout/receiving failures. An
unresolved owner remains UNKNOWN. Late physical receiving preserves that failure
and requires a callback bound to the original invocation before launch.
"""
import sys
sys.dont_write_bytecode = True

import ctypes
import dataclasses
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import threading
import time

REVIEW_COMPLETE = True
JOB_MODULE = Path(__file__).with_name("windows_job.py")
JOB_MODULE_SHA256 = "d94583001a33ef34adb67feec34b4f4b0bf3626df9f63da3c7a7c85bf3e0b552"
CAPTURE_LIMIT = None
CLEANUP_SECONDS = 10
RETAINED_PROCESS_OWNERS = []
RETAINED_READERS = []
RETAINED_INPUT_WRITERS = []
RETAINED_COMMANDS = []
OWNER_MODULE = None
COORDINATOR = None


def load_original_owner():
    global OWNER_MODULE, COORDINATOR
    if OWNER_MODULE is not None:
        return
    require(os.name == "nt", "ordinary native command owner requires Windows")
    raw = JOB_MODULE.read_bytes()
    require(hashlib.sha256(raw).hexdigest() == JOB_MODULE_SHA256, "installed native owner differs")
    spec = importlib.util.spec_from_file_location("guarded_ordinary_installed_windows_job", JOB_MODULE)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    # Execute the bytes whose hash was checked; do not re-open or modify the installed file.
    exec(compile(raw, str(JOB_MODULE), "exec"), module.__dict__)
    require(module.WINDOWS_JOB_MODULE_CONTRACT_V1 == "orchestrarium.windows-job.module.v1",
            "installed native owner contract differs")
    OWNER_MODULE = module
    COORDINATOR = module.WindowsInheritanceCoordinatorV1()


def feed_stdin(fd, path, state, custody):
    # The installed owner accepts pipe/null input, not a caller-owned file handle.
    # One retained worker transfers the existing closed input file without buffering it in memory.
    reader = writer = None
    state["worker_started"] = True
    try:
        writer = os.fdopen(fd, "wb", buffering=0)
        custody["writer"] = writer
        reader = path.open("rb", buffering=0)
        custody["reader"] = reader
        while True:
            data = reader.read(65536)
            if not data:
                state["input_eof"] = True
                break
            state["bytes_read"] += len(data)
            offset = 0
            while offset < len(data):
                count = writer.write(data[offset:])
                require(count is not None and count > 0, "short original stdin write")
                offset += count
                state["bytes_written"] += count
    except BaseException as error:
        state["error"] = type(error).__name__ + ": " + str(error)
    finally:
        try:
            writer.close() if writer is not None else os.close(fd)
            state["pipe_close_returned"] = True
        except BaseException as error:
            state["error"] = type(error).__name__ + ": " + str(error)
        try:
            if reader is not None:
                reader.close()
                state["input_close_returned"] = True
        except BaseException as error:
            state["error"] = type(error).__name__ + ": " + str(error)
        state["worker_complete_monotonic"] = time.monotonic()
        state["worker_complete"] = True


def start_original_worker(fd, path, state, name, target, command_workers, retained_workers, io_custody=None):
    worker = None
    try:
        args = (fd, path, state) if io_custody is None else (fd, path, state, io_custody)
        worker = threading.Thread(target=target, args=args, daemon=True, name=name)
        retained_workers.append(worker)
        command_workers.append(worker)
        worker.start()
    except BaseException:
        if worker is None or worker.ident is None:
            os.close(fd)
            state["pipe_close_returned"] = True
        raise

def require(condition, message):
    if not condition:
        raise RuntimeError(message)

def hidden_console():
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    user = ctypes.WinDLL("user32", use_last_error=True)
    kernel.GetConsoleWindow.restype = ctypes.c_void_p
    user.IsWindowVisible.argtypes = [ctypes.c_void_p]
    window = kernel.GetConsoleWindow()
    facts = {"attached": bool(window), "visible": bool(window and user.IsWindowVisible(window))}
    require(facts == {"attached": True, "visible": False}, "runner requires an attached hidden console")
    return facts

def drain(fd, path, state, custody):
    reader = writer = None
    state["worker_started"] = True
    try:
        reader = os.fdopen(fd, "rb", buffering=0)
        custody["reader"] = reader
        writer = path.open("xb", buffering=0)
        custody["writer"] = writer
        while True:
            data = reader.read(65536)
            if not data:
                state["eof"] = True
                break
            state["bytes_seen"] += len(data)
            limit = state["limit_bytes"]
            keep = len(data) if limit is None else min(len(data), max(0, limit - state["bytes_stored"]))
            state["limit_exceeded"] = limit is not None and state["bytes_seen"] > limit
            offset = 0
            while offset < keep:
                count = writer.write(data[offset:keep])
                require(count is not None and count > 0, "short capture write")
                offset += count
                state["bytes_stored"] += count
    except BaseException as error:
        state["error"] = type(error).__name__ + ": " + str(error)
    finally:
        try:
            reader.close() if reader is not None else os.close(fd)
            state["pipe_close_returned"] = True
        except BaseException as error:
            state["error"] = type(error).__name__ + ": " + str(error)
        try:
            if writer is not None:
                writer.close()
                state["capture_close_returned"] = True
        except BaseException as error:
            state["error"] = type(error).__name__ + ": " + str(error)
        state["worker_complete_monotonic"] = time.monotonic()
        state["worker_complete"] = True


def join_original_workers(readers, writers, cleanup_end, row, started):
    """Join original workers by zero-time polling under the unchanged deadline."""
    observations, errors, pending = [], [], []
    row["worker_join_observations"] = observations

    def remember_error(worker, observation, operation, error):
        # Keep original exception objects, including the first one for re-raise.
        errors.append((worker, error))
        observation["join_error_count"] += 1
        if "join_error_type" not in observation:
            observation.update(join_error_type=type(error).__name__,
                               join_error=str(error), join_error_operation=operation)

    for worker in readers + writers:
        side = worker.name.rsplit("-", 1)[-1]
        state = row.get("stdin", {}) if side == "stdin" else row["io"].get(side, {})
        keys = ("worker_complete", "eof", "input_eof",
                "pipe_close_returned", "capture_close_returned", "input_close_returned")
        observation = {"name": worker.name, "ident": worker.ident,
                       "native_id": worker.native_id, "alive_before": None, "alive_after": None,
                       "stream_before": {key: state[key] for key in keys if key in state},
                       "join_attempted": False, "join_attempt_count": 0,
                       "join_error_count": 0, "join_timeout_seconds": 0, "joined": False}
        observations.append(observation)
        if worker.ident is not None:
            pending.append((worker, state, observation))

    # Every started worker gets one attempt even if the shared deadline has
    # already elapsed, as in the former join(max(0, remaining)) implementation.
    while pending:
        still_pending = []
        for worker, state, observation in pending:
            if not observation["join_attempted"]:
                try:
                    # On the pinned CPython, is_alive() can itself join natively.
                    observation["alive_before"] = worker.is_alive()
                except BaseException as error:
                    remember_error(worker, observation, "alive_before", error)
            entered = time.monotonic()
            remaining = max(0, cleanup_end - entered)
            if not observation["join_attempted"]:
                observation.update(entered_seconds=entered-started, remaining_seconds=remaining)
            observation.update(join_attempted=True, last_entered_seconds=entered-started,
                               last_remaining_seconds=remaining)
            observation["join_attempt_count"] += 1
            try:
                worker.join(0)
            except BaseException as error:
                remember_error(worker, observation, "join", error)
            try:
                observation["alive_after"] = worker.is_alive()
            except BaseException as error:
                observation["alive_after"] = None
                remember_error(worker, observation, "alive_after", error)
            observation.update(returned_seconds=time.monotonic()-started,
                               stream_after={key: state[key] for key in keys if key in state},
                               worker_complete_seconds=(state["worker_complete_monotonic"] - started
                                                        if "worker_complete_monotonic" in state else None))
            observation["joined"] = (observation["join_error_count"] == 0
                                      and observation["alive_after"] is False)
            if not observation["joined"] and observation["join_error_count"] == 0:
                still_pending.append((worker, state, observation))
        pending = still_pending
        remaining = cleanup_end - time.monotonic()
        if not pending or remaining <= 0:
            break
        try:
            time.sleep(min(0.005, remaining))
        except BaseException as error:
            # A polling-wait interruption also must reach UNKNOWN publication.
            # None denotes the coordinator wait, never an invented worker.
            errors.append((None, error))
            row["worker_join_poll_error"] = {"type": type(error).__name__, "message": str(error),
                                              "seconds": time.monotonic()-started}
            break
        if time.monotonic() >= cleanup_end:
            break
    return errors


def make_guarded_run_class(core):
    require(REVIEW_COMPLETE is True, "disabled ordinary native guard: root review and qualification pending")
    load_original_owner()
    base = core.Run

    class GuardedRun(base):
        def __init__(self, root, env):
            self.command_guards = {}
            self._command_receiving = {}
            super().__init__(root, env)

        def commands_quiescent(self):
            return all(item["state"] == "complete" for item in self._command_receiving.values())

        def quiescence_snapshot(self):
            return {
                "complete": self.commands_quiescent(),
                "commands": [
                    {"name": item["name"], "state": item["state"],
                     "reason": item.get("reason", ""), "direct_exit_known": "exit_code" in item["row"]}
                    for item in self._command_receiving.values()
                ],
            }


        def receive_failed_worker_closure(self, deadline):
            """Receive physical closure only; keep the failed command and gate unchanged."""
            require(isinstance(deadline, (int, float)) and not isinstance(deadline, bool)
                    and math.isfinite(deadline), "finite receiving deadline required")
            started = time.monotonic()
            failed = [item for item in self._command_receiving.values() if item["state"] != "complete"]
            require(bool(failed), "failed original commands are required")
            states_before = self.quiescence_snapshot()
            records, pending = [], []
            for item in failed:
                row = item["row"]
                matches = [custody for custody in RETAINED_COMMANDS
                           if custody.get("run") is self and custody["row"] is row]
                record = {"name": item["name"], "state": "UNKNOWN", "original_state": item["state"],
                          "workers": [], "capture_access_issued": False}
                records.append(record)
                if len(matches) != 1:
                    record["reason"] = "no one original ordinary custody for this Run"
                    continue
                custody = matches[0]
                if custody.get("kind") == "named-controller":
                    self._receive_failed_named(custody, item, record, deadline)
                    continue
                original, closure = custody["original"], custody["closure"]
                if original is None or not any(value is original for value in RETAINED_PROCESS_OWNERS):
                    record["reason"] = "original native owner is not retained"
                    continue
                before = dataclasses.asdict(closure) if isinstance(closure, OWNER_MODULE.WindowsJobClosureV1) else {}
                if before:
                    before["issues"] = list(before["issues"])
                record["native_before"] = before
                try:
                    binding = custody.get("native_physical_binding")
                    if binding is None:
                        binding = original.physical_binding(closure)
                        custody["native_physical_binding"] = binding
                    require(binding["original_owner_binding"] == row.get("original_owner_binding")
                            and original.original_binding() == binding["original_owner_binding"],
                            "late receiving must retain the same original owner/handles")
                    physical = original.receive_failed_closure(closure, deadline)
                    custody["physical_closure"] = physical
                    native = dataclasses.asdict(physical)
                    native["issues"] = list(native["issues"])
                    record.update(binding, same_original_owner_retained=True, native=native)
                except BaseException as error:
                    record["reason"] = "original physical receiving remains unknown: " + type(error).__name__ + ": " + str(error)
                    continue
                if not (native.get("direct_reaped") is True and type(native.get("direct_exit_code")) is int
                        and native.get("active_zero") is True and native.get("handles_closed") is True
                        and native.get("issues") == []):
                    record["reason"] = "original native physical closure remains incomplete"
                    continue
                readers, writers = custody["readers"], custody["input_writers"]
                workers = readers + writers
                if not (len(readers) == 2 and len(writers) == (1 if row["stdin_mode"] == "pipe" else 0)
                        and all(any(value is worker for value in RETAINED_READERS) for worker in readers)
                        and all(any(value is worker for value in RETAINED_INPUT_WRITERS) for worker in writers)
                        and all(worker.ident is not None for worker in workers)):
                    record["reason"] = "original worker inventory is incomplete"
                    continue
                for worker in workers:
                    record["workers"].append({"name": worker.name, "ident": worker.ident,
                                              "native_id": worker.native_id, "join_calls": 0,
                                              "joined": False, "alive_after": None,
                                              "join_error_count": 0, "first_join_error": None})
                pending.append((row, workers, record))
            receiving_error = None
            while pending:
                for row, workers, record in tuple(pending):
                    for worker, observation in zip(workers, record["workers"]):
                        if observation["joined"]:
                            continue
                        observation["join_calls"] += 1
                        try:
                            # No positive timed join: keep the original object and poll it.
                            worker.join(0)
                            observation["alive_after"] = worker.is_alive()
                            observation["joined"] = not observation["alive_after"]
                        except BaseException as error:
                            observation["join_error_count"] += 1
                            if observation["first_join_error"] is None:
                                observation["first_join_error"] = type(error).__name__ + ": " + str(error)
                    if all(observation["joined"] for observation in record["workers"]):
                        io = {side: dict(state) for side, state in row["io"].items()}
                        stdin = dict(row.get("stdin", {}))
                        outputs_closed = (set(io) == {"stdout", "stderr"} and all(
                            state.get("error") is None and all(state.get(key) is True for key in (
                                "acquired", "worker_started", "worker_complete", "eof",
                                "pipe_close_returned", "capture_close_returned")) for state in io.values()))
                        input_closed = (row["stdin_mode"] == "null" or
                            all(stdin.get(key) is True for key in (
                                "acquired", "worker_started", "worker_complete",
                                "pipe_close_returned", "input_close_returned")))
                        record.update(io=io, stdin=stdin,
                                      state="CLOSED" if outputs_closed and input_closed else "UNKNOWN",
                                      seconds=time.monotonic() - started)
                        if record["state"] != "CLOSED":
                            record["reason"] = "joined workers did not establish all original EOF/close facts"
                        pending.remove((row, workers, record))
                remaining = deadline - time.monotonic()
                if not pending or remaining <= 0:
                    break
                try:
                    time.sleep(min(0.005, remaining))
                except BaseException as error:
                    receiving_error = type(error).__name__ + ": " + str(error)
                    break
            for _, _, record in pending:
                record["reason"] = receiving_error or "original worker joins remain unresolved at the receiving deadline"
            require(self.quiescence_snapshot() == states_before,
                    "physical receiving must not reclassify the failed command")
            return {"schema": "failed-original-worker-physical-closure-v1",
                    "status": "CLOSED" if all(row["state"] == "CLOSED" for row in records) else "UNKNOWN",
                    "commands": records, "seconds": time.monotonic() - started,
                    "receiving_error": receiving_error,
                    "deadline_exceeded": time.monotonic() > deadline,
                    "original_command_states_unchanged": True, "qualification_passed": False,
                    "capture_access_issued": False, "new_commands_permitted": False}

        def _receive_failed_named(self, custody, item, record, deadline):
            """Receive the same direct controller and its prebound nested physical proof."""
            if custody.get("physical_closure") is not None:
                record.update(custody["physical_closure"])
                return
            original, receive = custody["original"], custody["physical_receiver"]
            if original is None or not any(value is original for value in RETAINED_PROCESS_OWNERS):
                record["reason"] = "original named process is not retained"
                return
            if receive is None:
                record["reason"] = "original named physical receiver was not bound before launch"
                return
            while True:
                try:
                    code = original.wait(timeout=0)
                    custody["wait_returned"] = True
                    break
                except core.subprocess.TimeoutExpired:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        record["reason"] = "original named process exit remains unreceived"
                        return
                    try:
                        time.sleep(min(0.005, remaining))
                    except BaseException as error:
                        record["reason"] = type(error).__name__ + ": " + str(error)
                        return
                except BaseException as error:
                    record["reason"] = type(error).__name__ + ": " + str(error)
                    return
            try:
                require(type(code) is int, "original direct exit code is unreceived")
                proof = receive(custody["row"], code)
                require(isinstance(proof, dict) and proof.get("physical_complete") is True,
                        "original named physical closure is unverified")
                json.dumps(proof)
                if not custody["proof_and_files_received"]:
                    self._close_named_files(item)
                require(bool(custody["files"]) and all(
                    held["close_attempted"] and held["closed"] and held["error"] is None
                    and held["file"].closed for held in custody["files"]),
                    "original named file close remains unreceived")
            except BaseException as error:
                record["reason"] = type(error).__name__ + ": " + str(error)
                return
            closed = {"state": "CLOSED", "original_external_process_retained": True,
                      "original_direct_exit_code": code, "parent_file_handles_closed": True,
                      "external_physical_receiving": proof}
            custody["physical_closure"] = closed
            record.update(closed)

        def _publish_state(self, item, state, reason=""):
            item["state"], item["reason"] = state, reason
            item["row"]["quiescence_state"] = state
            if reason:
                item["row"]["quiescence_reason"] = reason
            else:
                item["row"].pop("quiescence_reason", None)
            self.receipt["command_quiescence"] = self.quiescence_snapshot()
            self.save()

        def _close_named_files(self, item):
            # Only the exact successful native/IO proof receiver reaches here.
            # A direct Popen exit is necessary but never sufficient.
            matches = [custody for custody in RETAINED_COMMANDS
                       if custody.get("run") is self and custody["row"] is item["row"]
                       and custody.get("kind") == "named-controller"]
            require(len(matches) == 1, "one original named-controller custody required")
            custody = matches[0]
            original = custody["original"]
            require(custody["wait_returned"] is True and original is not None
                    and any(value is original for value in RETAINED_PROCESS_OWNERS)
                    and type(original.returncode) is int,
                    "original named controller exit must be received before closing its files")
            for held in custody["files"]:
                require(not held["close_attempted"], "original named file close is one-shot")
                held["close_attempted"] = True
                try:
                    held["file"].close()
                    require(held["file"].closed is True, "original named file close did not complete")
                    held["closed"] = True
                except BaseException as error:
                    held["error"] = error
                    raise
            custody["proof_and_files_received"] = True

        def _receive_completion(self, item, receive):
            try:
                evidence = receive(item["row"])
                if not (isinstance(evidence, dict) and evidence or
                        isinstance(evidence, list) and evidence and all(isinstance(v, dict) and v for v in evidence)):
                    raise RuntimeError("completion receiver must return original proof data, not a verdict")
                # Validate serializability before permitting any capture access.
                json.dumps(evidence)
                if item.get("named_controller") is True:
                    self._close_named_files(item)
            except BaseException as error:
                self._publish_state(item, "unknown", type(error).__name__ + ": " + str(error))
                raise
            item["row"]["completion_receiving"] = evidence
            self._publish_state(item, "complete")

        def _finalize_captures(self, item, decode):
            row = item["row"]
            if item["state"] != "complete" or not self.commands_quiescent():
                raise RuntimeError("capture measurement requires positive global command quiescence")
            try:
                for stream in ("stdout", "stderr"):
                    row[stream + "_sha256"] = core.sha(row[stream])
                row["captures_measured"] = True
                result = Path(row["stdout"]).read_text(encoding="utf-8-sig", errors="strict") if decode else None
            except BaseException as error:
                row["capture_error"] = type(error).__name__ + ": " + str(error)
                raise
            finally:
                self.save()
            if item["check"] and row["exit_code"]:
                raise RuntimeError(item["name"] + " exited " + str(row["exit_code"]) + "; see " + row["stderr"])
            return result

        def command(self, argv, name, cwd=None, timeout=600, check=True, stdin_path=None, *, defer_output=False, capture_limit=None, physical_receiver=None):
            if not self.commands_quiescent():
                raise RuntimeError("new command refused: original command quiescence is unresolved")
            guard = self.command_guards.get(name)
            if guard is not None and not callable(guard):
                raise TypeError("named completion receiver must be callable")
            if defer_output and guard is not None:
                raise ValueError("choose named receiving or deferred receiving, not both")
            if physical_receiver is not None and not callable(physical_receiver):
                raise TypeError("external physical receiver must be callable")
            # Existing named/deferred owners must remain outside this new Job boundary.
            if guard is None and not defer_output:
                require(physical_receiver is None, "external physical receiver requires an external command")
                return self._ordinary_command(argv, name, cwd, timeout, check, stdin_path, capture_limit)
            require(capture_limit is None, "capture limits are supported only by the original ordinary owner")
            row = {
                "argv": [str(x) for x in argv], "cwd": str(core.REPO if cwd is None else cwd),
                "stdout": str(self.root / (name + ".stdout.txt")),
                "stderr": str(self.root / (name + ".stderr.txt")),
                "captures_measured": False,
            }
            item = {"row": row, "name": name, "state": "running", "check": check,
                    "named_controller": True}
            custody = {"kind": "named-controller", "run": self, "row": row,
                       "original": None, "files": [], "wait_returned": False,
                       "proof_and_files_received": False, "failure": None,
                       "physical_receiver": physical_receiver}
            # The root owns this holder before publication, opening files or Popen.
            RETAINED_COMMANDS.append(custody)
            self._command_receiving[id(row)] = item
            self.receipt["commands"].append(row)
            original = None
            start = time.monotonic()
            phases = row["phases_elapsed_seconds"] = {}
            try:
                self._publish_state(item, "running")
                out = Path(row["stdout"]).open("xb")
                custody["files"].append({"role": "stdout", "file": out,
                    "close_attempted": False, "closed": False, "error": None})
                err = Path(row["stderr"]).open("xb")
                custody["files"].append({"role": "stderr", "file": err,
                    "close_attempted": False, "closed": False, "error": None})
                stdin = Path(stdin_path).open("rb") if stdin_path else core.subprocess.DEVNULL
                if stdin_path:
                    custody["files"].append({"role": "stdin", "file": stdin,
                        "close_attempted": False, "closed": False, "error": None})
                startupinfo = None
                creationflags = core.subprocess.CREATE_NO_WINDOW
                tools = self.receipt.get("read_only_tool_aliases", {})
                if row["argv"][0] in tuple(tools.get(key, {}).get("resolved") for key in ("go", "pwsh")):
                    startupinfo = core.subprocess.STARTUPINFO()
                    startupinfo.dwFlags = core.subprocess.STARTF_USESHOWWINDOW
                    startupinfo.wShowWindow = core.subprocess.SW_HIDE
                    creationflags = core.subprocess.CREATE_NEW_CONSOLE
                phases["launch_entered"] = time.monotonic() - start
                original = core.subprocess.Popen(
                    row["argv"], cwd=row["cwd"], env=self.env, stdin=stdin, stdout=out, stderr=err,
                    creationflags=creationflags, startupinfo=startupinfo,
                )
                phases["launch_returned"] = time.monotonic() - start
                custody["original"] = original
                RETAINED_PROCESS_OWNERS.append(original)
                row["original_pid"] = original.pid
                row["original_pid_is_authority"] = False
                # wait(timeout) never kills the original controller. Its original
                # native work/cleanup deadlines and caller timeout are unchanged.
                phases["wait_entered"] = time.monotonic() - start
                code = original.wait(timeout=timeout)
                phases["wait_returned"] = time.monotonic() - start
                custody["wait_returned"] = True
                row["exit_code"] = code
            except BaseException as error:
                if "wait_entered" in phases and "wait_returned" not in phases:
                    phases["wait_exception"] = time.monotonic() - start
                elif "launch_entered" in phases and "launch_returned" not in phases:
                    phases["launch_exception"] = time.monotonic() - start
                # Retain the returned actual Popen even if registration/publication
                # was interrupted. Never infer nested closure from direct exit.
                if original is not None:
                    custody["original"] = original
                    if not any(value is original for value in RETAINED_PROCESS_OWNERS):
                        RETAINED_PROCESS_OWNERS.append(original)
                custody["failure"] = error
                row["seconds"] = round(time.monotonic() - start, 3)
                self._publish_state(item, "unknown", type(error).__name__ + ": " + str(error))
                raise
            row["seconds"] = round(time.monotonic() - start, 3)
            if defer_output:
                self._publish_state(item, "awaiting-original-receipt")
                return None
            if guard is not None:
                self._receive_completion(item, guard)
            elif row["exit_code"] == 0:
                self._publish_state(item, "complete")
            else:
                self._publish_state(item, "unknown", "nonzero ordinary tool exit has no received completion proof")
                raise RuntimeError(name + " exited " + str(row["exit_code"]) + "; captures retained unmeasured")
            return self._finalize_captures(item, decode=True)


        def _ordinary_command(self, argv, name, cwd, timeout, check, stdin_path, capture_limit=None):
            require(isinstance(timeout, (int, float)) and not isinstance(timeout, bool)
                    and math.isfinite(timeout) and timeout > 0, "ordinary command requires its finite original work timeout")
            require(capture_limit is None or type(capture_limit) is int and capture_limit > 0,
                    "capture limit must be a positive integer byte count")
            console = hidden_console()
            argv = [str(value) for value in argv]
            require(bool(argv) and os.path.isabs(argv[0]), "ordinary native command requires an absolute executable")
            started = time.monotonic()
            work_end = started + timeout
            cleanup_end = work_end + CLEANUP_SECONDS
            row = {
                "argv": argv, "cwd": str(core.REPO if cwd is None else cwd),
                "stdout": str(self.root / (name + ".stdout.txt")),
                "stderr": str(self.root / (name + ".stderr.txt")),
                "captures_measured": False, "command_success": False, "io": {},
                "work_seconds": timeout, "cleanup_seconds": CLEANUP_SECONDS,
                "native_owner": {"path": str(JOB_MODULE), "sha256": JOB_MODULE_SHA256},
                "console": console, "stdin_mode": "pipe" if stdin_path else "null",
                "before_resume_entered": False, "before_resume_returned": False,
                "before_resume_deadline_exceeded": False,
                "phases_elapsed_seconds": {key: None for key in (
                    "initial_save_started", "initial_save_finished", "launch_entered",
                    "before_resume_entered", "before_resume_returned", "launch_returned",
                    "launch_exception", "external_settle_end")},
            }
            item = {"row": row, "name": name, "state": "running", "check": check}
            self._command_receiving[id(row)] = item
            self.receipt["commands"].append(row)
            original = closure = None
            code = None
            readers, writers = [], []
            custody = {"run": self, "original": None, "closure": None, "readers": readers,
                       "input_writers": writers, "row": row, "exception": None,
                       "stdin_objects": {"fd": None, "reader": None, "writer": None},
                       "output_objects": {side: {"fd": None, "reader": None, "writer": None}
                                          for side in ("stdout", "stderr")}}
            RETAINED_COMMANDS.append(custody)
            row["phases_elapsed_seconds"]["initial_save_started"] = time.monotonic() - started
            self._publish_state(item, "running")
            row["phases_elapsed_seconds"]["initial_save_finished"] = time.monotonic() - started

            def before_resume():
                entered = time.monotonic()
                row["before_resume_entered"] = True
                row["phases_elapsed_seconds"]["before_resume_entered"] = entered - started
                row["before_resume_deadline_exceeded"] = entered >= work_end
                require(not row["before_resume_deadline_exceeded"], "work deadline elapsed before original resume")
                row["phases_elapsed_seconds"]["before_resume_returned"] = time.monotonic() - started
                row["before_resume_returned"] = True

            try:
                row["phases_elapsed_seconds"]["launch_entered"] = time.monotonic() - started
                original = OWNER_MODULE.WindowsJobOwnerV1.launch(
                    executable=argv[0], argv=argv, cwd=row["cwd"], environment=self.env,
                    stdin=row["stdin_mode"], stdout="pipe", stderr="pipe", coordinator=COORDINATOR,
                    before_resume=before_resume)
                row["phases_elapsed_seconds"]["launch_returned"] = time.monotonic() - started
                custody["original"] = original
                RETAINED_PROCESS_OWNERS.append(original)
                row["original_pid"] = original.pid
                row["original_owner_binding"] = original.original_binding()
                for side, take in (("stdout", original.take_stdout_fd), ("stderr", original.take_stderr_fd)):
                    fd = take()
                    require(type(fd) is int, "original output descriptor was not transferred")
                    custody["output_objects"][side]["fd"] = fd
                    state = {"acquired": True, "worker_started": False, "worker_complete": False,
                             "eof": False, "pipe_close_returned": False, "capture_close_returned": False,
                              "error": None, "bytes_seen": 0, "bytes_stored": 0,
                              "limit_bytes": capture_limit, "limit_exceeded": False}
                    row["io"][side] = state
                    start_original_worker(fd, Path(row[side]), state, name + "-" + side,
                                          drain, readers, RETAINED_READERS, custody["output_objects"][side])
                if stdin_path:
                    fd = original.take_stdin_fd()
                    require(type(fd) is int, "original input descriptor was not transferred")
                    custody["stdin_objects"]["fd"] = fd
                    state = {"acquired": True, "worker_started": False, "worker_complete": False,
                             "input_eof": False, "pipe_close_returned": False, "input_close_returned": False,
                             "error": None, "bytes_read": 0, "bytes_written": 0}
                    row["stdin"] = state
                    start_original_worker(fd, Path(stdin_path), state, name + "-stdin",
                                          feed_stdin, writers, RETAINED_INPUT_WRITERS, custody["stdin_objects"])
                code = original.wait(work_end)
                row["timed_out"] = code is None
            except BaseException as error:
                custody["exception"] = error
                if row["phases_elapsed_seconds"]["launch_returned"] is None:
                    row["phases_elapsed_seconds"]["launch_exception"] = time.monotonic() - started
                row["error"] = type(error).__name__ + ": " + str(error)
                closure = getattr(error, "windows_job_closure", None)
                creation = getattr(error, "windows_job_creation", None)
                if isinstance(creation, OWNER_MODULE.WindowsJobCreateProcessV1):
                    row["process_creation"] = dataclasses.asdict(creation)
                pending_owner = getattr(error, "windows_job_owner", None)
                if original is None and isinstance(pending_owner, OWNER_MODULE.WindowsJobProcessV1):
                    original = custody["original"] = pending_owner
                    RETAINED_PROCESS_OWNERS.append(original)
                    row["original_pid"] = original.pid
                    row["original_owner_binding"] = original.original_binding()
            finally:
                work_finished = time.monotonic()
                row["work_deadline_exceeded"] = work_finished > work_end
                row["work_elapsed_seconds"] = work_finished - started
                cleanup_end = min(work_finished, work_end) + CLEANUP_SECONDS
                row["cleanup_interval_seconds"] = max(0, cleanup_end - work_finished)
                if original is not None:
                    try:
                        closure = original.settle(cleanup_end, terminate=code is None or "error" in row)
                    except BaseException as error:
                        custody["settlement_exception"] = error
                        row["settlement_error"] = type(error).__name__ + ": " + str(error)
                    finally:
                        row["phases_elapsed_seconds"]["external_settle_end"] = time.monotonic() - started
                custody["closure"] = closure
                if closure is not None:
                    row["native"] = dataclasses.asdict(closure)
                    row["native"]["issues"] = list(row["native"]["issues"])
                    if type(closure.direct_exit_code) is int:
                        row["exit_code"] = closure.direct_exit_code
                join_errors = join_original_workers(readers, writers, cleanup_end, row, started)
                custody["join_errors"] = join_errors
            joined_workers = {worker for worker, observation in zip(
                readers + writers, row["worker_join_observations"]) if observation["joined"]}
            native = row.get("native", {})
            pipe_keys = ("acquired", "worker_started", "worker_complete", "eof",
                         "pipe_close_returned", "capture_close_returned")
            readers_joined = len(readers) == 2 and all(worker in joined_workers for worker in readers)
            stdin_state = row.get("stdin", {})
            stdin_closed = (not stdin_path and not writers or
                bool(stdin_path) and len(writers) == 1 and writers[0].ident is not None
                and writers[0] in joined_workers
                and all(stdin_state.get(key) is True for key in (
                    "acquired", "worker_started", "worker_complete",
                    "pipe_close_returned", "input_close_returned")))
            stdin_delivery_ok = (not stdin_path or
                stdin_state.get("error") is None and stdin_state.get("input_eof") is True
                and stdin_state.get("bytes_read") == stdin_state.get("bytes_written"))
            quiet = (isinstance(closure, OWNER_MODULE.WindowsJobClosureV1)
                     and native.get("direct_reaped") is True and type(native.get("direct_exit_code")) is int
                     and native.get("active_zero") is True and native.get("handles_closed") is True
                     and native.get("issues") == [] and not join_errors and readers_joined and stdin_closed
                     and set(row["io"]) == {"stdout", "stderr"}
                     and all(state["error"] is None and all(state[key] is True for key in pipe_keys)
                             for state in row["io"].values()))
            creation = row.get("process_creation", {})
            closed_prestart = (
                original is None and not readers and not writers and not join_errors
                and row["io"] == {} and stdin_state == {} and row["stdin_mode"] == "null"
                and row["before_resume_entered"] is False and row["before_resume_returned"] is False
                and row["before_resume_deadline_exceeded"] is False
                and row["phases_elapsed_seconds"]["launch_returned"] is None
                and row.get("error") == "WindowsJobError: WJOB-PROCESS-CREATE:process-create"
                and creation.get("attempted") is True and creation.get("created") is False
                and type(creation.get("win32_last_error")) is int
                and 0 <= creation["win32_last_error"] <= 0xffffffff
                and isinstance(closure, OWNER_MODULE.WindowsJobClosureV1)
                and native == {"direct_exit_code": None, "direct_reaped": True, "active_zero": True,
                               "job_terminated": True, "handles_closed": True, "issues": []})
            if closed_prestart:
                quiet = True
                row.update(lifecycle_state="CLOSED_PRESTART_FAIL", captures="NOT_CREATED",
                           readers="NOT_STARTED", command_executed=False,
                           termination_code_is_command_result=False, command_success=False)
            closed_pre_resume = (
                original is None and not readers and not writers and row["io"] == {}
                and row["before_resume_entered"] is True and row["before_resume_returned"] is False
                and row["before_resume_deadline_exceeded"] is True
                and row["phases_elapsed_seconds"]["launch_returned"] is None
                and row.get("error") == "RuntimeError: work deadline elapsed before original resume"
                and isinstance(closure, OWNER_MODULE.WindowsJobClosureV1)
                and native == {"direct_exit_code": 1, "direct_reaped": True, "active_zero": True,
                               "job_terminated": True, "handles_closed": True, "issues": []})
            if closed_pre_resume:
                quiet = True
                row.update(lifecycle_state="CLOSED_PRE_RESUME_FAIL", captures="NOT_CREATED",
                           readers="NOT_STARTED", command_executed=False,
                           termination_code_is_command_result=False, command_success=False)
            row.update(seconds=time.monotonic() - started,
                       cleanup_deadline_exceeded=time.monotonic() > cleanup_end,
                       original_owner_retained=original is not None
                       and any(value is original for value in RETAINED_PROCESS_OWNERS),
                       original_readers_retained=all(any(value is worker for value in RETAINED_READERS)
                                                    for worker in readers),
                       original_readers_joined=readers_joined,
                       original_input_writers_retained=all(
                           any(value is worker for value in RETAINED_INPUT_WRITERS) for worker in writers),
                       original_input_writer_joined=bool(stdin_path) and len(writers) == 1
                       and writers[0].ident is not None and writers[0] in joined_workers)
            if not quiet:
                reason = ("original worker join raised; custody unresolved" if join_errors
                          else "original ordinary native/EOF/close remains unresolved")
                self._publish_state(item, "unknown", reason)
                if join_errors:
                    raise join_errors[0][1]
                raise RuntimeError("original ordinary custody unresolved; captures retained unmeasured: " + name)
            proof = {"schema": "ordinary-original-native-io-completion-v1", "native": native,
                     "io": row["io"], "stdin_mode": row["stdin_mode"], "stdin": stdin_state,
                     "original_owner_retained": row["original_owner_retained"],
                     "original_readers_joined": readers_joined,
                     "original_input_writer_joined": row["original_input_writer_joined"],
                     "lifecycle_state": row.get("lifecycle_state", "CLOSED")}
            if closed_prestart:
                proof["process_creation"] = creation
            self._receive_completion(item, lambda unused: proof)
            if closed_prestart:
                # No process, exit code, reader or capture exists. Keep the
                # original launch failure after its positive resource closure.
                raise custody["exception"]
            if closed_pre_resume:
                # Pinned owner closed every native/pipe handle before any descriptor transfer.
                # No capture or reader existed, and the work deadline still failed.
                raise RuntimeError("work deadline elapsed before original resume")
            execution_ok = (row.get("timed_out") is False and not row["work_deadline_exceeded"]
                            and not row["cleanup_deadline_exceeded"] and not native["job_terminated"]
                            and "error" not in row and "settlement_error" not in row and stdin_delivery_ok
                            and not any(state["limit_exceeded"] for state in row["io"].values()))
            try:
                for side in ("stdout", "stderr"):
                    require(Path(row[side]).stat().st_size == row["io"][side]["bytes_stored"],
                            "closed original capture size differs")
            except BaseException as error:
                row["capture_error"] = type(error).__name__ + ": " + str(error)
                self.save()
                raise
            # A timeout or late exit cannot become success, including check=False.
            # Completion is published before hashes or decoding, as in the existing guard.
            output = self._finalize_captures(item, decode=execution_ok)
            row["command_success"] = execution_ok and row["exit_code"] == 0
            self.save()
            if not execution_ok:
                raise RuntimeError("ordinary command failed after original native/IO closure: " + name)
            return output


        def finalize_deferred(self, row, completion_reader, decode=False):
            item = self._command_receiving.get(id(row))
            if item is None or item["row"] is not row or item["state"] != "awaiting-original-receipt":
                raise RuntimeError("one original pending command row is required")
            if not callable(completion_reader):
                raise TypeError("original completion receiver must be callable")
            self._receive_completion(item, completion_reader)
            return self._finalize_captures(item, decode=decode)

    return GuardedRun

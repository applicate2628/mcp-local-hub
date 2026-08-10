from __future__ import annotations

import threading
import time
import uuid
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from contextlib import suppress
from dataclasses import dataclass, field
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

Runner = Callable[["JobContext"], dict[str, Any]]


def utc_now() -> str:
    return datetime.now(UTC).isoformat().replace("+00:00", "Z")


@dataclass
class JobRecord:
    job_id: str
    solver: str
    project_path: Path
    output_dir: Path
    settings: dict[str, Any]
    timeout_s: float
    state: str = "queued"
    stage: str = "queued"
    progress: float | None = None
    created_utc: str = field(default_factory=utc_now)
    started_utc: str | None = None
    finished_utc: str | None = None
    result: dict[str, Any] | None = None
    error: dict[str, Any] | None = None
    cancel_event: threading.Event = field(default_factory=threading.Event, repr=False)
    timeout_event: threading.Event = field(default_factory=threading.Event, repr=False)
    cancel_callback: Callable[[], None] | None = field(default=None, repr=False)
    lock: threading.RLock = field(default_factory=threading.RLock, repr=False)


class JobContext:
    def __init__(self, record: JobRecord) -> None:
        self.record = record
        self.deadline = time.monotonic() + record.timeout_s

    @property
    def output_dir(self) -> Path:
        return self.record.output_dir

    def remaining(self) -> float:
        return max(0.0, self.deadline - time.monotonic())

    def cancelled(self) -> bool:
        return self.record.cancel_event.is_set()

    def check(self) -> None:
        if self.record.timeout_event.is_set() or self.remaining() <= 0:
            raise JobTimedOut(f"job exceeded timeout of {self.record.timeout_s:.17g} seconds")
        if self.cancelled():
            raise JobCancelled("job cancellation requested")

    def update(self, stage: str, progress: float | None = None) -> None:
        if progress is not None and not 0.0 <= progress <= 1.0:
            raise ValueError("progress must be within [0,1]")
        with self.record.lock:
            self.record.stage = stage
            self.record.progress = progress
        self.check()

    def install_cancel(self, callback: Callable[[], None]) -> None:
        call_now = False
        with self.record.lock:
            self.record.cancel_callback = callback
            call_now = self.record.cancel_event.is_set()
        if call_now:
            callback()


class JobCancelled(RuntimeError):
    pass


class JobTimedOut(TimeoutError):
    pass


class JobManager:
    """One serialized, bounded solver queue with retained terminal records."""

    def __init__(self, solver: str) -> None:
        self.solver = solver
        self._executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix=f"{solver}-job")
        self._jobs: dict[str, JobRecord] = {}
        self._lock = threading.RLock()

    def start(
        self,
        *,
        project_path: Path,
        output_root: Path,
        settings: dict[str, Any],
        timeout_s: float,
        runner: Runner,
    ) -> dict[str, Any]:
        if not 1.0 <= timeout_s <= 7 * 24 * 3600:
            raise ValueError("timeout_s must be within [1,604800]")
        job_id = str(uuid.uuid4())
        output_dir = output_root / job_id
        output_dir.mkdir(mode=0o700)
        record = JobRecord(job_id, self.solver, project_path, output_dir, settings, timeout_s)
        with self._lock:
            self._jobs[job_id] = record
        self._executor.submit(self._run, record, runner)
        return self.snapshot(job_id)

    def _run(self, record: JobRecord, runner: Runner) -> None:
        with record.lock:
            if record.cancel_event.is_set():
                record.state = "cancelled"
                record.stage = "cancelled_before_start"
                record.finished_utc = utc_now()
                return
            record.state = "running"
            record.stage = "starting"
            record.started_utc = utc_now()
        watchdog = threading.Timer(record.timeout_s, self._expire, args=(record,))
        watchdog.daemon = True
        watchdog.start()
        try:
            result = runner(JobContext(record))
            if record.timeout_event.is_set():
                raise JobTimedOut(f"job exceeded timeout of {record.timeout_s:.17g} seconds")
            if record.cancel_event.is_set():
                raise JobCancelled("job cancellation requested")
            with record.lock:
                record.result = result
                record.progress = 1.0
                record.stage = "completed"
                record.state = "succeeded"
        except JobCancelled as exc:
            self._terminal_error(record, "cancelled", "cancelled", exc)
        except JobTimedOut as exc:
            self._terminal_error(record, "timed_out", "timeout", exc)
        except Exception as exc:
            self._terminal_error(record, "failed", "solver_error", exc)
        finally:
            watchdog.cancel()
            with record.lock:
                record.cancel_callback = None
                record.finished_utc = utc_now()

    @staticmethod
    def _expire(record: JobRecord) -> None:
        callback: Callable[[], None] | None
        with record.lock:
            if record.state != "running":
                return
            record.timeout_event.set()
            record.cancel_event.set()
            record.stage = "timeout_requested"
            callback = record.cancel_callback
        if callback is not None:
            with suppress(Exception):
                callback()

    @staticmethod
    def _terminal_error(record: JobRecord, state: str, code: str, exc: BaseException) -> None:
        with record.lock:
            record.state = state
            record.stage = state
            record.error = {
                "code": code,
                "message": str(exc),
                "exception": type(exc).__name__,
                "stage": record.stage,
            }

    def _record(self, job_id: str) -> JobRecord:
        with self._lock:
            try:
                return self._jobs[job_id]
            except KeyError as exc:
                raise KeyError("unknown job_id") from exc

    def snapshot(self, job_id: str, *, include_result: bool = False) -> dict[str, Any]:
        record = self._record(job_id)
        with record.lock:
            payload: dict[str, Any] = {
                "job_id": record.job_id,
                "solver": record.solver,
                "state": record.state,
                "stage": record.stage,
                "progress": record.progress,
                "created_utc": record.created_utc,
                "started_utc": record.started_utc,
                "finished_utc": record.finished_utc,
                "error": record.error,
            }
            if include_result:
                if record.state not in {"succeeded", "failed", "cancelled", "timed_out"}:
                    raise RuntimeError("job result is not terminal")
                payload["result"] = record.result
            return payload

    def cancel(self, job_id: str) -> dict[str, Any]:
        record = self._record(job_id)
        callback: Callable[[], None] | None
        with record.lock:
            if record.state in {"succeeded", "failed", "cancelled", "timed_out"}:
                return self.snapshot(job_id)
            record.cancel_event.set()
            record.stage = "cancellation_requested"
            callback = record.cancel_callback
        if callback is not None:
            callback()
        return self.snapshot(job_id)

    def successful(self, job_id: str) -> JobRecord:
        record = self._record(job_id)
        with record.lock:
            if record.state != "succeeded":
                raise RuntimeError(f"job is not successful: {record.state}")
        return record


def solve_action(
    manager: JobManager,
    action: str,
    *,
    job_id: str | None,
) -> dict[str, Any] | None:
    normalized = action.strip().lower()
    if normalized == "start":
        return None
    if not job_id:
        raise ValueError(f"job_id is required for action={normalized}")
    if normalized == "status":
        return manager.snapshot(job_id)
    if normalized == "result":
        return manager.snapshot(job_id, include_result=True)
    if normalized == "cancel":
        return manager.cancel(job_id)
    raise ValueError("action must be start, status, result, or cancel")

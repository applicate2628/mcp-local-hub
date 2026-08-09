from __future__ import annotations

import time
from pathlib import Path

from mcphub_em_mcp.jobs import JobManager


def _wait_terminal(manager: JobManager, job_id: str, timeout: float = 3) -> dict:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        state = manager.snapshot(job_id, include_result=False)
        if state["state"] in {"succeeded", "failed", "cancelled", "timed_out"}:
            return state
        time.sleep(0.01)
    raise AssertionError("job did not become terminal")


def test_job_success_and_result(tmp_path: Path) -> None:
    manager = JobManager("fixture")
    project = tmp_path / "model.dat"
    project.write_text("input", encoding="utf-8")

    def runner(ctx):
        ctx.update("solving", 0.5)
        return {"answer": 42}

    started = manager.start(
        project_path=project,
        output_root=tmp_path,
        settings={"x": 1.0},
        timeout_s=2,
        runner=runner,
    )
    terminal = _wait_terminal(manager, started["job_id"])
    assert terminal["state"] == "succeeded"
    assert manager.snapshot(started["job_id"], include_result=True)["result"] == {"answer": 42}


def test_job_cancel_calls_installed_solver_stop(tmp_path: Path) -> None:
    manager = JobManager("fixture-cancel")
    project = tmp_path / "model.dat"
    project.write_text("input", encoding="utf-8")
    stopped = []

    def runner(ctx):
        ctx.install_cancel(lambda: stopped.append(True))
        while not ctx.cancelled():
            time.sleep(0.01)
        ctx.check()
        raise AssertionError("unreachable")

    started = manager.start(
        project_path=project,
        output_root=tmp_path,
        settings={},
        timeout_s=2,
        runner=runner,
    )
    deadline = time.monotonic() + 1
    while manager.snapshot(started["job_id"])["stage"] == "queued" and time.monotonic() < deadline:
        time.sleep(0.01)
    manager.cancel(started["job_id"])
    assert _wait_terminal(manager, started["job_id"])["state"] == "cancelled"
    assert stopped == [True]


def test_job_timeout_calls_stop_and_is_distinct(tmp_path: Path) -> None:
    manager = JobManager("fixture-timeout")
    project = tmp_path / "model.dat"
    project.write_text("input", encoding="utf-8")
    stopped = []

    def runner(ctx):
        ctx.install_cancel(lambda: stopped.append(True))
        while not ctx.cancelled():
            time.sleep(0.01)
        ctx.check()
        raise AssertionError("unreachable")

    started = manager.start(
        project_path=project,
        output_root=tmp_path,
        settings={},
        timeout_s=1,
        runner=runner,
    )
    terminal = _wait_terminal(manager, started["job_id"], timeout=2)
    assert terminal["state"] == "timed_out"
    assert terminal["error"]["code"] == "timeout"
    assert stopped == [True]

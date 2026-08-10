from __future__ import annotations

import json
import re
import shutil
import time
from pathlib import Path
from typing import Any

from .aedt_batch import (
    CONFIGURE_SCRIPT,
    EXPORT_SCRIPT,
    aedt_command,
    batch_failure_code,
    find_ansysedt,
    run_aedt_process,
)
from .contracts import (
    ExclusiveUnitFloat,
    HFSSBasisOrder,
    LicensePollSeconds,
    LicenseWaitSeconds,
    PassCount,
    PositiveTimeoutSeconds,
    SolveAction,
)
from .hfss_mesh_vendor.extract_hfss_solver_mesh import extract_solver_mesh
from .jobs import JobContext, JobManager, solve_action, utc_now
from .provenance import artifact_record, sha256_file, write_json
from .safety import existing_output_root, existing_project_file, require_confirmation, require_windows
from .strict_fastmcp import strict_fastmcp

mcp = strict_fastmcp(
    "mcphub-hfss",
    instructions=(
        "Noninteractive HFSS jobs. hfss_solve owns start/status/result/cancel/preflight; "
        "exporters consume one successful job_id and never solve again."
    ),
)
_jobs = JobManager("hfss")


def _batch_log_path(project: Path, operation: str) -> Path:
    if operation == "batch_save":
        return project.with_name(project.stem + "-batchsave.log")
    if operation == "batch_solve":
        return project.with_name(project.name + ".batchinfo") / (project.stem + ".log")
    raise ValueError(operation)


def _diagnostic_text(*paths: Path) -> str:
    chunks: list[str] = []
    for path in paths:
        if path.is_file():
            chunks.append(path.read_text(encoding="utf-8", errors="replace"))
    return "\n".join(chunks)


def _raise_aedt_failure(stage: str, return_code: int, diagnostic: str) -> None:
    code = batch_failure_code(diagnostic)
    if code == "hfss_project_kernel_migration_required":
        raise RuntimeError(
            "hfss_project_kernel_migration_required: this legacy ACIS project must be "
            "converted with AEDT 2023 R1 through 2024 R2 before AEDT 2025 R1 can solve it"
        )
    if code == "hfss_license_unavailable":
        raise RuntimeError("hfss_license_unavailable: AEDT did not obtain an HFSS license")
    if code is not None or return_code != 0:
        raise RuntimeError(f"hfss_batch_failed: AEDT stage {stage} exited with code {return_code}")


def _run_script(
    ctx: JobContext,
    executable: Path,
    *,
    name: str,
    source: str,
    config: dict[str, Any],
    stage: str,
) -> dict[str, Any]:
    runtime = ctx.output_dir / "_runtime"
    runtime.mkdir(exist_ok=True)
    script = runtime / f"{name}.py"
    config_path = runtime / f"{name}.json"
    result_path = runtime / f"{name}-result.json"
    process_log = runtime / f"{name}-process.log"
    script.write_text(source, encoding="utf-8", newline="\n")
    write_json(config_path, config)
    result_path.unlink(missing_ok=True)
    return_code = run_aedt_process(
        ctx,
        aedt_command(executable, "run_script", script),
        stage=stage,
        output_log=process_log,
        environ={
            "MCPHUB_HFSS_PROJECT": str(config["project"]),
            "MCPHUB_HFSS_CONFIG": str(config_path),
            "MCPHUB_HFSS_RESULT": str(result_path),
        },
    )
    if not result_path.is_file():
        _raise_aedt_failure(stage, return_code, _diagnostic_text(process_log))
        raise RuntimeError(f"hfss_batch_failed: AEDT stage {stage} returned no result record")
    result = json.loads(result_path.read_text(encoding="utf-8"))
    if result.get("status") != "ok":
        detail = str(result.get("traceback", "AEDT script failed")).splitlines()[-1]
        code = batch_failure_code(detail)
        if code:
            _raise_aedt_failure(stage, return_code, detail)
        raise RuntimeError(f"hfss_batch_failed: AEDT stage {stage}: {detail}")
    _raise_aedt_failure(stage, return_code, _diagnostic_text(process_log))
    return result


def _run_batch_save(ctx: JobContext, executable: Path, project: Path) -> None:
    runtime = ctx.output_dir / "_runtime"
    runtime.mkdir(exist_ok=True)
    process_log = runtime / "batch-save-process.log"
    vendor_log = _batch_log_path(project, "batch_save")
    return_code = run_aedt_process(
        ctx,
        aedt_command(executable, "batch_save", project),
        stage="upgrading_project_copy",
        output_log=process_log,
    )
    _raise_aedt_failure("upgrading_project_copy", return_code, _diagnostic_text(process_log, vendor_log))


def _run_batch_solve(
    ctx: JobContext,
    executable: Path,
    project: Path,
    license_wait_timeout_s: float,
    license_poll_s: float,
) -> None:
    runtime = ctx.output_dir / "_runtime"
    runtime.mkdir(exist_ok=True)
    vendor_log = _batch_log_path(project, "batch_solve")
    deadline = time.monotonic() + license_wait_timeout_s
    attempt = 0
    while True:
        ctx.check()
        attempt += 1
        vendor_log.unlink(missing_ok=True)
        process_log = runtime / f"batch-solve-process-{attempt}.log"
        return_code = run_aedt_process(
            ctx,
            aedt_command(executable, "batch_solve", project),
            stage="solving",
            output_log=process_log,
        )
        diagnostic = _diagnostic_text(process_log, vendor_log)
        if batch_failure_code(diagnostic) != "hfss_license_unavailable":
            _raise_aedt_failure("solving", return_code, diagnostic)
            return
        if license_wait_timeout_s <= 0 or time.monotonic() >= deadline:
            _raise_aedt_failure("solving", return_code, diagnostic)
        ctx.update("waiting_for_license", None)
        ctx.record.cancel_event.wait(
            min(license_poll_s, ctx.remaining(), max(0.0, deadline - time.monotonic()))
        )


def _runner(
    project: Path,
    design: str | None,
    setup_name: str,
    sweep_name: str,
    settings: dict[str, Any],
    license_wait_timeout_s: float,
    license_poll_s: float,
) -> Any:
    def run(ctx: JobContext) -> dict[str, Any]:
        require_windows()
        executable = find_ansysedt()
        ctx.update("copying_project", 0.05)
        project_copy = ctx.output_dir / project.name
        shutil.copy2(project, project_copy)
        input_sha = sha256_file(project)

        _run_batch_save(ctx, executable, project_copy)
        configure = _run_script(
            ctx,
            executable,
            name="configure",
            source=CONFIGURE_SCRIPT,
            config={"project": str(project_copy), **settings},
            stage="configuring_setup",
        )
        _run_batch_solve(
            ctx,
            executable,
            project_copy,
            license_wait_timeout_s,
            license_poll_s,
        )
        ctx.check()
        convergence = ctx.output_dir / "hfss_convergence.conv"
        mesh_stats = ctx.output_dir / "hfss_mesh_stats.ms"
        _run_script(
            ctx,
            executable,
            name="export-solve-artifacts",
            source=EXPORT_SCRIPT,
            config={
                "project": str(project_copy),
                "design": design,
                "setup": setup_name,
                "sweep": sweep_name,
                "convergence": str(convergence),
                "mesh_stats": str(mesh_stats),
                "generalized": None,
                "renormalized": None,
                "reference_ohm": 50.0,
            },
            stage="exporting_convergence",
        )
        manifest = {
            "schema": "mcphub.hfss.solve.v1",
            "solver": {"product": "Ansys HFSS", "version": str(configure["version"])},
            "created_utc": ctx.record.created_utc,
            "completed_utc": utc_now(),
            "input": {"name": project.name, "sha256": input_sha},
            "project": {"path": project_copy.name, "sha256": sha256_file(project_copy)},
            "design": design,
            "setup": setup_name,
            "sweep": sweep_name,
            "settings": settings,
            "determinism": {
                "declared": False,
                "reason": "HFSS adaptive meshing does not expose a server-controlled deterministic seed",
            },
            "artifacts": [],
        }
        for path, media in ((convergence, "text/plain"), (mesh_stats, "text/plain")):
            if path.is_file():
                manifest["artifacts"].append(artifact_record(path, ctx.output_dir, media_type=media))
        manifest_path = ctx.output_dir / "manifest.json"
        write_json(manifest_path, manifest)
        return {
            "manifest": artifact_record(manifest_path, ctx.output_dir, media_type="application/json"),
            "artifacts": manifest["artifacts"],
            "capabilities": {"sparameters": True, "volume_mesh": True},
        }

    return run


@mcp.tool()
def hfss_solve(
    action: SolveAction = "start",
    job_id: str | None = None,
    project_path: str | None = None,
    output_root: str | None = None,
    design: str | None = None,
    setup: str = "Setup1",
    sweep: str = "Sweep1",
    basis_order: HFSSBasisOrder = 1,
    maximum_delta_s: ExclusiveUnitFloat = 0.01,
    maximum_passes: PassCount = 10,
    minimum_passes: PassCount = 2,
    minimum_converged_passes: PassCount = 1,
    do_material_lambda: bool = True,
    adaptation_frequency: str = "5GHz",
    timeout_s: PositiveTimeoutSeconds = 21600,
    license_wait_timeout_s: LicenseWaitSeconds = 0,
    license_poll_s: LicensePollSeconds = 30,
    confirm: bool = False,
) -> dict[str, Any]:
    """Use start, status, result, cancel, or preflight for one isolated HFSS solve job."""
    normalized_action = action.strip().lower()
    if normalized_action not in {"start", "preflight"}:
        routed = solve_action(_jobs, normalized_action, job_id=job_id)
        if routed is not None:
            return routed
    if project_path is None or output_root is None:
        raise ValueError(f"project_path and output_root are required for action={normalized_action}")
    project = existing_project_file(project_path, (".aedt",))
    root = existing_output_root(output_root)
    if basis_order not in {-1, 0, 1, 2}:
        raise ValueError("basis_order must be -1, 0, 1, or 2")
    if not 0 < maximum_delta_s < 1:
        raise ValueError("maximum_delta_s must be within (0,1)")
    if not 1 <= minimum_passes <= maximum_passes <= 100:
        raise ValueError("passes must satisfy 1 <= minimum_passes <= maximum_passes <= 100")
    if not 1 <= minimum_converged_passes <= maximum_passes:
        raise ValueError("minimum_converged_passes must be within [1,maximum_passes]")
    if not 0 <= license_wait_timeout_s <= 86400 or not 0.1 <= license_poll_s <= 3600:
        raise ValueError("invalid license wait configuration")
    frequency_match = re.fullmatch(
        r"([0-9]+(?:\.[0-9]+)?(?:[eE][-+]?[0-9]+)?)\s*([A-Za-z]+)",
        adaptation_frequency,
    )
    if frequency_match is None or float(frequency_match.group(1)) <= 0:
        raise ValueError("adaptation_frequency must be a positive number with an explicit unit")
    settings = {
        "design": design,
        "setup": setup,
        "sweep": sweep,
        "basis_order": basis_order,
        "maximum_delta_s": maximum_delta_s,
        "maximum_passes": maximum_passes,
        "minimum_passes": minimum_passes,
        "minimum_converged_passes": minimum_converged_passes,
        "do_material_lambda": do_material_lambda,
        "adaptation_frequency": adaptation_frequency,
    }
    if normalized_action == "preflight":
        return {
            "valid": True,
            "action": "preflight",
            "solver": "hfss",
            "project": {"name": project.name, "suffix": project.suffix.lower()},
            "settings": settings,
        }
    require_confirmation(confirm, "starting an HFSS solve")
    return _jobs.start(
        project_path=project,
        output_root=root,
        settings=settings,
        timeout_s=timeout_s,
        runner=_runner(project, design, setup, sweep, settings, license_wait_timeout_s, license_poll_s),
    )


def _scrub_touchstone(path: Path, forbidden_roots: list[Path]) -> None:
    lines: list[str] = []
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        if line.lstrip().startswith("!") and any(
            str(root).lower() in line.lower() for root in forbidden_roots
        ):
            continue
        if re.search(r"(?i)[a-z]:[\\/]", line):
            if line.lstrip().startswith("!"):
                continue
            raise RuntimeError("HFSS Touchstone data contains an absolute machine path")
        lines.append(line)
    path.write_text("\n".join(lines) + "\n", encoding="utf-8", newline="\n")


@mcp.tool()
def hfss_export_sparams(job_id: str, reference_ohm: float = 50.0) -> dict[str, Any]:
    """Export generalized and reference-renormalized Touchstone from one solved job."""
    record = _jobs.successful(job_id)
    if reference_ohm <= 0:
        raise ValueError("reference_ohm must be positive")
    project = record.output_dir / record.project_path.name
    raw = record.output_dir / "hfss_generalized.s2p"
    renorm = record.output_dir / "hfss_renormalized.s2p"
    ctx = JobContext(record)
    executable = find_ansysedt()
    try:
        _run_script(
            ctx,
            executable,
            name="export-sparameters",
            source=EXPORT_SCRIPT,
            config={
                "project": str(project),
                "design": record.settings.get("design"),
                "setup": str(record.settings.get("setup", "Setup1")),
                "sweep": str(record.settings.get("sweep", "Sweep1")),
                "convergence": None,
                "mesh_stats": None,
                "generalized": str(raw),
                "renormalized": str(renorm),
                "reference_ohm": reference_ohm,
            },
            stage="exporting_sparameters",
        )
    finally:
        with record.lock:
            record.stage = "completed"
            record.progress = 1.0
    _scrub_touchstone(raw, [project.parent, record.project_path.parent])
    _scrub_touchstone(renorm, [project.parent, record.project_path.parent])
    base = json.loads((record.output_dir / "manifest.json").read_text(encoding="utf-8"))
    raw_record = artifact_record(raw, record.output_dir, media_type="application/touchstone")
    renorm_record = artifact_record(renorm, record.output_dir, media_type="application/touchstone")
    export_manifest = record.output_dir / "hfss_sparams_manifest.json"
    write_json(
        export_manifest,
        {
            "schema": "mcphub.hfss.sparams.v1",
            "job_id": job_id,
            "created_utc": utc_now(),
            "solver": base["solver"],
            "project": base["project"],
            "settings": base["settings"],
            "determinism": base["determinism"],
            "numeric_precision": "%.17g",
            "generalized": raw_record,
            "renormalized": renorm_record,
            "renormalized_reference_ohm": reference_ohm,
        },
    )
    return {
        "job_id": job_id,
        "generalized": raw_record,
        "renormalized": renorm_record,
        "reference_ohm": reference_ohm,
        "numeric_precision": "%.17g",
        "manifest": artifact_record(export_manifest, record.output_dir, media_type="application/json"),
    }


@mcp.tool()
def hfss_export_mesh(job_id: str, adaptive_pass: int = -1, exclude_background: bool = True) -> dict[str, Any]:
    """Export the solved HFSS volume mesh for the requested adaptive pass."""
    record = _jobs.successful(job_id)
    if adaptive_pass < -1:
        raise ValueError("adaptive_pass must be -1 (latest) or a non-negative pass index")
    if adaptive_pass != -1:
        raise RuntimeError(
            "hfss_adaptive_pass_cache_unavailable: AEDT results expose a validated latest "
            "solver cache but no pass-indexed volume caches on this installation"
        )
    project = record.output_dir / record.project_path.name
    results_dir = project.with_name(project.name + "results")
    if not results_dir.is_dir():
        raise RuntimeError("hfss_results_missing: solved project has no .aedtresults directory")
    extracted = extract_solver_mesh(
        results_dir,
        out_dir=record.output_dir,
        prefix="hfss_latest",
        no_sections=True,
        allow_unvalidated=False,
        exclude_background=exclude_background,
    )
    mesh_path = Path(extracted["mesh_path"])
    validation_path = Path(extracted["validation_path"])
    Path(extracted["summary_path"]).unlink(missing_ok=True)
    mesh_record = artifact_record(mesh_path, record.output_dir, media_type="application/vnd.gmsh")
    validation_record = artifact_record(validation_path, record.output_dir, media_type="text/csv")
    manifest_path = record.output_dir / "hfss_mesh_manifest.json"
    write_json(
        manifest_path,
        {
            "schema": "mcphub.hfss.mesh.v1",
            "job_id": job_id,
            "created_utc": utc_now(),
            "adaptive_pass": "latest",
            "exclude_background": exclude_background,
            "source_format": extracted["source_format"],
            "vertices": extracted["vertices"],
            "tetrahedra": extracted["elements"],
            "mesh_hash": extracted["mesh_hash"],
            "mesh": mesh_record,
            "validation": validation_record,
        },
    )
    return {
        "job_id": job_id,
        "adaptive_pass": "latest",
        "mesh_hash": extracted["mesh_hash"],
        "mesh": mesh_record,
        "validation": validation_record,
        "manifest": artifact_record(manifest_path, record.output_dir, media_type="application/json"),
    }


def main() -> None:
    mcp.run(transport="stdio")


if __name__ == "__main__":
    main()

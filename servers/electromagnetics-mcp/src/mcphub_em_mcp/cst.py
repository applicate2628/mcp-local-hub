from __future__ import annotations

import importlib
import json
import math
import os
import re
import shutil
import sys
import time
from contextlib import suppress
from pathlib import Path
from typing import Any

from mcp.server.fastmcp import FastMCP

from .cst_results import export_result_tree
from .jobs import JobContext, JobManager, solve_action, utc_now
from .provenance import artifact_record, sha256_file, write_json
from .safety import existing_output_root, existing_project_file, require_confirmation, require_windows
from .slim import read_slim, write_surface_gmsh, write_volume_gmsh

mcp = FastMCP(
    "mcphub-cst",
    instructions=(
        "Noninteractive CST jobs. cst_solve owns start/status/result/cancel; "
        "exporters consume one successful job_id and never solve again."
    ),
)
_jobs = JobManager("cst")


def _version_key(path: Path) -> tuple[int, ...]:
    return tuple(int(value) for value in re.findall(r"\d+", path.name)) or (0,)


def _cst_library_root() -> Path:
    require_windows()
    candidates: list[Path] = []
    configured = os.environ.get("CST_INSTALL_ROOT", "").strip()
    if configured:
        candidates.append(Path(configured))
    program_files_value = os.environ.get("PROGRAMFILES", "").strip()
    if not program_files_value:
        raise RuntimeError("PROGRAMFILES is not defined; set CST_INSTALL_ROOT explicitly")
    program_files = Path(program_files_value)
    candidates.extend(sorted(program_files.glob("CST Studio Suite *"), key=_version_key, reverse=True))
    for root in candidates:
        library = root / "AMD64" / "python_cst_libraries"
        if library.is_dir():
            return library.resolve(strict=True)
    raise RuntimeError("CST Studio Suite Python libraries were not found")


def _cst_module(name: str) -> Any:
    library = str(_cst_library_root())
    if library not in sys.path:
        sys.path.insert(0, library)
    return importlib.import_module(name)


def _is_license_error(exc: BaseException) -> bool:
    text = str(exc).lower()
    return "license" in text or "licensed" in text


def _cst_version(environment: Any) -> str:
    for line in str(environment.version()).splitlines():
        clean = line.strip()
        if clean.lower().startswith("this is cst design environment"):
            return clean.removeprefix("This is ")
    raise RuntimeError("CST version response did not contain a product release line")


def _with_license_wait(ctx: JobContext, wait_s: float, poll_s: float, factory: Any) -> Any:
    deadline = time.monotonic() + wait_s
    while True:
        ctx.check()
        try:
            return factory()
        except Exception as exc:
            if not _is_license_error(exc):
                raise
            if wait_s <= 0 or time.monotonic() >= deadline:
                raise RuntimeError("cst_license_unavailable: configured license wait exhausted") from exc
            ctx.update("waiting_for_license", None)
            ctx.record.cancel_event.wait(min(poll_s, ctx.remaining(), max(0.0, deadline - time.monotonic())))


def _bool(value: bool) -> str:
    return "True" if value else "False"


def _settings_history(settings: dict[str, Any]) -> str:
    """Generate only typed, fixed-shape CST history; caller text is never executable."""

    samples = settings["frequency_samples_ghz"]
    frequency_range = settings["frequency_range_ghz"]
    range_min = format(frequency_range[0], ".17g")
    range_max = format(frequency_range[1], ".17g")
    lines = [
        f"Solver.FrequencyRange {range_min}, {range_max}",
        "With MeshSettings",
        '    .SetMeshType "Tet"',
        "End With",
        "With FDSolver",
        "    .Reset",
        '    .SetMethod "Tetrahedral", "Discrete samples only"',
        '    .Type "Direct"',
        f"    .AccuracyTet {settings['accuracy_tet']:.17g}",
        f'    .OrderTet "{settings["order_tet"]}"',
        "    .MixedOrderTet False",
        '    .Stimulation "All", "All"',
        f"    .SetPortMeshMatches3DMeshTet {_bool(settings['port_mesh_matches_3d'])}",
        "    .ForceRecalculateOldSamples True",
        "End With",
        "With MeshAdaption3D",
        '    .SetType "HighFrequencyTet"',
        '    .SetAdaptionStrategy "ExpertSystem"',
        f"    .MinPasses {settings['minimum_passes']}",
        f"    .MaxPasses {settings['maximum_passes']}",
        f"    .MaxDeltaS {settings['maximum_delta_s']:.17g}",
        "    .NumberOfDeltaSChecks 1",
        "    .EnableInnerSParameterAdaptation True",
        f"    .PropagationConstantAccuracy {settings['propagation_constant_accuracy']:.17g}",
        f"    .NumberOfPropConstChecks {settings['propagation_constant_checks']}",
        "    .EnablePortPropagationConstantAdaptation True",
        "End With",
        "With FDSolver",
        "    .MeshAdaptionTet True",
        '    .ResetSampleIntervals "all"',
    ]
    for sample in samples:
        text = format(sample, ".17g")
        adaptation = _bool(sample == settings["adaptation_frequency_ghz"])
        lines.append(f'    .AddSampleInterval {text}, {text}, 1, "Single", {adaptation}')
    lines.extend(["    .ForceRecalculateOldSamples True", "End With"])
    return "\n".join(lines)


def _copy_cst_bundle(project: Path, destination: Path) -> Path:
    project_copy = destination / project.name
    shutil.copy2(project, project_copy)
    data = project.with_suffix("")
    if data.is_dir():
        shutil.copytree(data, destination / data.name)
    return project_copy


def _open_owned_project(interface: Any, project_path: Path) -> tuple[Any, Any]:
    """Open through CST's reliable Project API and bind only its new DE process."""

    before = {int(pid) for pid in interface.running_design_environments()}
    project: Any = None
    try:
        project = interface.Project.open(project_path)
        after = {int(pid) for pid in interface.running_design_environments()}
        created = after - before
        if len(created) != 1:
            raise RuntimeError("CST Project.open did not create exactly one isolated Design Environment")
        environment = interface.DesignEnvironment.connect(created.pop())
        return project, environment
    except Exception:
        if project is not None:
            with suppress(Exception):
                project.close()
        after = {int(pid) for pid in interface.running_design_environments()}
        for pid in after - before:
            with suppress(Exception):
                interface.DesignEnvironment.connect(pid).close()
        raise


def _runner(
    project: Path,
    settings: dict[str, Any],
    license_wait_timeout_s: float,
    license_poll_s: float,
) -> Any:
    def run(ctx: JobContext) -> dict[str, Any]:
        ctx.update("copying_project", 0.05)
        project_copy = _copy_cst_bundle(project, ctx.output_dir)
        input_sha = sha256_file(project)
        environment: Any = None
        cst_project: Any = None
        try:
            interface = _cst_module("cst.interface")
            cst_project, environment = _with_license_wait(
                ctx,
                license_wait_timeout_s,
                license_poll_s,
                lambda: _open_owned_project(interface, project_copy),
            )
            ctx.install_cancel(environment.close)
            ctx.update("opening_project", 0.1)
            if cst_project.model3d is None:
                raise RuntimeError("CST project has no 3D model")
            ctx.update("configuring_solver", 0.15)
            cst_project.model3d.add_to_history(
                "mcphub: explicit solver and adaptation settings",
                _settings_history(settings),
            )
            cst_project.save(include_results=True)
            ctx.update("solving", None)
            cst_project.model3d.run_solver()
            ctx.check()
            ctx.update("saving_results", 0.9)
            cst_project.save(include_results=True)
            manifest = {
                "schema": "mcphub.cst.solve.v1",
                "solver": {"product": "CST Studio Suite", "version": _cst_version(environment)},
                "created_utc": ctx.record.created_utc,
                "completed_utc": utc_now(),
                "input": {"name": project.name, "sha256": input_sha},
                "project": {"path": project_copy.name, "sha256": sha256_file(project_copy)},
                "settings": settings,
                "determinism": {
                    "declared": False,
                    "reason": (
                        "CST tetrahedral meshing is order-dependent and exposes no server-controlled seed"
                    ),
                    "settings_command_order": "fixed by mcphub.cst.solve.v1",
                },
                "artifacts": [],
            }
            manifest_path = ctx.output_dir / "manifest.json"
            write_json(manifest_path, manifest)
            return {
                "manifest": artifact_record(manifest_path, ctx.output_dir, media_type="application/json"),
                "capabilities": {
                    "results": True,
                    "mesh": (project_copy.with_suffix("") / "Result" / "3d.slim").is_file(),
                    "mesh_source_format": "CST SLIM 1.4 verified converter",
                },
            }
        finally:
            if cst_project is not None:
                with suppress(Exception):
                    cst_project.close()
            if environment is not None:
                with suppress(Exception):
                    environment.close()

    return run


@mcp.tool()
def cst_solve(
    action: str = "start",
    job_id: str | None = None,
    project_path: str | None = None,
    output_root: str | None = None,
    propagation_constant_accuracy: float = 1e-5,
    propagation_constant_checks: int = 2,
    accuracy_tet: float = 1e-10,
    order_tet: str = "First",
    port_mesh_matches_3d: bool = True,
    maximum_passes: int = 12,
    minimum_passes: int = 3,
    maximum_delta_s: float = 0.005,
    adaptation_frequency_ghz: float = 5.0,
    frequency_range_ghz: list[float] | None = None,
    frequency_samples_ghz: list[float] | None = None,
    timeout_s: float = 21600,
    license_wait_timeout_s: float = 0,
    license_poll_s: float = 30,
    confirm: bool = False,
) -> dict[str, Any]:
    """Start, inspect, retrieve, or cancel one isolated CST solve job."""
    routed = solve_action(_jobs, action, job_id=job_id)
    if routed is not None:
        return routed
    require_confirmation(confirm, "starting a CST solve")
    if project_path is None or output_root is None:
        raise ValueError("project_path and output_root are required for action=start")
    project = existing_project_file(project_path, (".cst",))
    root = existing_output_root(output_root)
    if not 0 < propagation_constant_accuracy < 1:
        raise ValueError(
            "propagation_constant_accuracy must be within (0,1); "
            "CST's historical 0.005 default is not silently used"
        )
    if not 1 <= propagation_constant_checks <= 20:
        raise ValueError("propagation_constant_checks must be within [1,20]")
    if not 0 < accuracy_tet < 1:
        raise ValueError("accuracy_tet must be within (0,1)")
    if order_tet not in {"First", "Second"}:
        raise ValueError("order_tet must be First or Second")
    if not 1 <= minimum_passes <= maximum_passes <= 100:
        raise ValueError("passes must satisfy 1 <= minimum_passes <= maximum_passes <= 100")
    if (
        not 0 < maximum_delta_s < 1
        or not math.isfinite(adaptation_frequency_ghz)
        or adaptation_frequency_ghz <= 0
    ):
        raise ValueError("invalid adaptation settings")
    frequency_range = frequency_range_ghz or []
    if (
        len(frequency_range) != 2
        or any(not math.isfinite(value) or value <= 0 for value in frequency_range)
        or frequency_range[0] >= frequency_range[1]
    ):
        raise ValueError("frequency_range_ghz must contain two positive increasing values")
    samples = sorted(frequency_samples_ghz or [])
    if (
        not samples
        or len(samples) > 10000
        or len(set(samples)) != len(samples)
        or any(not math.isfinite(value) or value <= 0 for value in samples)
    ):
        raise ValueError("frequency_samples_ghz must contain 1..10000 positive values")
    if samples[0] < frequency_range[0] or samples[-1] > frequency_range[1]:
        raise ValueError("frequency_samples_ghz values must be inside frequency_range_ghz")
    if adaptation_frequency_ghz not in samples:
        raise ValueError("frequency_samples_ghz must include adaptation_frequency_ghz")
    if not 0 <= license_wait_timeout_s <= 86400 or not 0.1 <= license_poll_s <= 3600:
        raise ValueError("invalid license wait configuration")
    settings = {
        "propagation_constant_accuracy": propagation_constant_accuracy,
        "propagation_constant_checks": propagation_constant_checks,
        "accuracy_tet": accuracy_tet,
        "order_tet": order_tet,
        "port_mesh_matches_3d": port_mesh_matches_3d,
        "maximum_passes": maximum_passes,
        "minimum_passes": minimum_passes,
        "maximum_delta_s": maximum_delta_s,
        "adaptation_frequency_ghz": adaptation_frequency_ghz,
        "frequency_range_ghz": frequency_range,
        "frequency_samples_ghz": samples,
    }
    return _jobs.start(
        project_path=project,
        output_root=root,
        settings=settings,
        timeout_s=timeout_s,
        runner=_runner(project, settings, license_wait_timeout_s, license_poll_s),
    )


@mcp.tool()
def cst_export_mesh(job_id: str, coordinate_unit: str = "mm", adaptive_pass: int = -1) -> dict[str, Any]:
    """Export the completed CST volume and port meshes as machine-neutral Gmsh 2.2."""
    record = _jobs.successful(job_id)
    if adaptive_pass != -1:
        raise RuntimeError("cst_mesh_pass_unavailable: CST 2026 exposes only the retained final SLIM mesh")
    result_dir = (record.output_dir / record.project_path.stem / "Result").resolve()
    source = result_dir / "3d.slim"
    if not source.is_file():
        raise RuntimeError("cst_mesh_source_unavailable: solved project has no Result/3d.slim")
    mesh = read_slim(source)
    volume = record.output_dir / "cst_volume_mesh.msh"
    write_volume_gmsh(volume, mesh, coordinate_unit)
    artifacts = [artifact_record(volume, record.output_dir, media_type="application/x-gmsh")]
    for index in range(1, 257):
        port_source = result_dir / f"Port{index}.slim"
        if not port_source.is_file():
            continue
        port_mesh = read_slim(port_source)
        port_output = record.output_dir / f"cst_port_{index}_mesh.msh"
        write_surface_gmsh(port_output, port_mesh, coordinate_unit, f"port_{index}")
        artifacts.append(artifact_record(port_output, record.output_dir, media_type="application/x-gmsh"))
    manifest_path = record.output_dir / "cst_mesh_manifest.json"
    base = json.loads((record.output_dir / "manifest.json").read_text(encoding="utf-8"))
    write_json(
        manifest_path,
        {
            "schema": "mcphub.cst.mesh.v1",
            "job_id": job_id,
            "created_utc": utc_now(),
            "solver": base["solver"],
            "project": base["project"],
            "settings": base["settings"],
            "mesh_hash": mesh.mesh_hash,
            "coordinate_unit": coordinate_unit,
            "numeric_precision": "%.17g",
            "source_format": (
                "CST SLIM 1.4 binary_little_endian, verified fixed node/edge/triangle/tetrahedron schema"
            ),
            "source_sha256": sha256_file(source),
            "determinism": base["determinism"],
            "artifacts": artifacts,
        },
    )
    return {
        "job_id": job_id,
        "mesh_hash": mesh.mesh_hash,
        "artifacts": artifacts,
        "manifest": artifact_record(manifest_path, record.output_dir, media_type="application/json"),
    }


@mcp.tool()
def cst_export_results(job_id: str) -> dict[str, Any]:
    """Export full-precision S, port, and convergence result-tree data."""
    record = _jobs.successful(job_id)
    results = _cst_module("cst.results")
    project = record.output_dir / record.project_path.name
    module = results.ProjectFile(project).get_3d()
    paths = export_result_tree(
        module,
        record.output_dir,
        propagation_constant_accuracy=float(record.settings["propagation_constant_accuracy"]),
        propagation_constant_checks=int(record.settings["propagation_constant_checks"]),
    )
    artifacts = [artifact_record(path, record.output_dir, media_type="text/csv") for path in paths]
    manifest_path = record.output_dir / "cst_results_manifest.json"
    base = json.loads((record.output_dir / "manifest.json").read_text(encoding="utf-8"))
    write_json(
        manifest_path,
        {
            "schema": "mcphub.cst.results.v1",
            "job_id": job_id,
            "created_utc": utc_now(),
            "solver": base["solver"],
            "project": base["project"],
            "settings": base["settings"],
            "determinism": base["determinism"],
            "numeric_precision": "%.17g",
            "complex_convention": "separate real and imaginary columns",
            "gamma_unit": "rad_per_m",
            "renormalized_reference_ohm": 50.0,
            "artifacts": artifacts,
        },
    )
    return {
        "job_id": job_id,
        "artifacts": artifacts,
        "manifest": artifact_record(manifest_path, record.output_dir, media_type="application/json"),
    }


def main() -> None:
    mcp.run(transport="stdio")


if __name__ == "__main__":
    main()

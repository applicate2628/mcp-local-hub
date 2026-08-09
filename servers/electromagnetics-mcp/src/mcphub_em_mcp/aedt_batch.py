from __future__ import annotations

import os
import re
import subprocess
import time
from collections.abc import Mapping, Sequence
from contextlib import suppress
from pathlib import Path

from .jobs import JobContext
from .safety import require_windows

CONFIGURE_SCRIPT = r"""# -*- coding: utf-8 -*-
import json
import os
import traceback
import ScriptEnv

project_path = os.environ["MCPHUB_HFSS_PROJECT"]
config_path = os.environ["MCPHUB_HFSS_CONFIG"]
result_path = os.environ["MCPHUB_HFSS_RESULT"]
result = {"status": "error"}
try:
    cfg = json.load(open(config_path, "r"))
    ScriptEnv.Initialize("Ansoft.ElectronicsDesktop")
    project = oDesktop.OpenProject(project_path)
    design = project.SetActiveDesign(cfg["design"]) if cfg.get("design") else project.GetActiveDesign()
    if design is None:
        raise RuntimeError("HFSS project has no active design")
    analysis = design.GetModule("AnalysisSetup")
    analysis.EditSetup(cfg["setup"], [
        "NAME:" + cfg["setup"],
        "Frequency:=", cfg["adaptation_frequency"],
        "MaxDeltaS:=", cfg["maximum_delta_s"],
        "MaximumPasses:=", cfg["maximum_passes"],
        "MinimumPasses:=", cfg["minimum_passes"],
        "MinimumConvergedPasses:=", cfg["minimum_converged_passes"],
        "BasisOrder:=", cfg["basis_order"],
        "DoMaterialLambda:=", cfg["do_material_lambda"],
    ])
    project.Save()
    result = {
        "status": "ok",
        "version": str(oDesktop.GetVersion()),
        "project": str(project.GetName()),
        "design": str(design.GetName()),
    }
except Exception:
    result = {"status": "error", "traceback": traceback.format_exc()}
    raise
finally:
    with open(result_path, "w") as handle:
        json.dump(result, handle, sort_keys=True)
    try:
        if "project" in globals() and project is not None:
            oDesktop.CloseProject(project.GetName())
    except Exception:
        pass
    try:
        oDesktop.QuitApplication()
    except Exception:
        pass
"""


EXPORT_SCRIPT = r"""# -*- coding: utf-8 -*-
import json
import os
import traceback
import ScriptEnv

project_path = os.environ["MCPHUB_HFSS_PROJECT"]
config_path = os.environ["MCPHUB_HFSS_CONFIG"]
result_path = os.environ["MCPHUB_HFSS_RESULT"]
result = {"status": "error"}
try:
    cfg = json.load(open(config_path, "r"))
    ScriptEnv.Initialize("Ansoft.ElectronicsDesktop")
    project = oDesktop.OpenProject(project_path)
    design = project.SetActiveDesign(cfg["design"]) if cfg.get("design") else project.GetActiveDesign()
    if design is None:
        raise RuntimeError("HFSS project has no active design")
    if cfg.get("convergence"):
        design.ExportConvergence(cfg["setup"], "", cfg["convergence"], True)
    if cfg.get("mesh_stats"):
        design.ExportMeshStats(cfg["setup"], "", cfg["mesh_stats"], True)
    if cfg.get("generalized") or cfg.get("renormalized"):
        solutions = design.GetModule("Solutions")
        solution = cfg["setup"] + ":" + cfg["sweep"]
        if cfg.get("generalized"):
            solutions.ExportNetworkData(
                "", [solution], 3, cfg["generalized"], ["All"], False,
                cfg["reference_ohm"], "S", -1, 1, 17, True, True, True)
        if cfg.get("renormalized"):
            solutions.ExportNetworkData(
                "", [solution], 3, cfg["renormalized"], ["All"], True,
                cfg["reference_ohm"], "S", -1, 1, 17, True, True, True)
    result = {
        "status": "ok",
        "version": str(oDesktop.GetVersion()),
        "project": str(project.GetName()),
        "design": str(design.GetName()),
    }
except Exception:
    result = {"status": "error", "traceback": traceback.format_exc()}
    raise
finally:
    with open(result_path, "w") as handle:
        json.dump(result, handle, sort_keys=True)
    try:
        if "project" in globals() and project is not None:
            oDesktop.CloseProject(project.GetName())
    except Exception:
        pass
    try:
        oDesktop.QuitApplication()
    except Exception:
        pass
"""


def find_ansysedt(environ: Mapping[str, str] | None = None) -> Path:
    require_windows()
    env = os.environ if environ is None else environ
    override = env.get("MCPHUB_ANSYSEDT_EXE", "").strip()
    if override:
        candidate = Path(override).resolve()
        if candidate.is_file() and candidate.name.lower() == "ansysedt.exe":
            return candidate
        raise FileNotFoundError("MCPHUB_ANSYSEDT_EXE is not a regular ansysedt.exe")

    roots = [Path(env[name]) for name in ("ProgramFiles", "ProgramW6432") if env.get(name)]
    candidates: set[Path] = set()
    for root in roots:
        candidates.update(root.glob("ANSYS Inc/v*/AnsysEM/ansysedt.exe"))
        candidates.update(root.glob("AnsysEM/AnsysEM*/Win64/ansysedt.exe"))
    regular = sorted((path.resolve() for path in candidates if path.is_file()), reverse=True)
    if not regular:
        raise FileNotFoundError("Ansys Electronics Desktop executable was not found")
    return regular[0]


def aedt_command(
    executable: Path,
    operation: str,
    target: Path,
    *,
    wait_for_license: bool = False,
) -> list[str]:
    if operation == "run_script":
        return [str(executable), "-features=beta", "-ng", "-RunScriptAndExit", str(target)]
    if operation == "batch_save":
        return [str(executable), "-ng", "-BatchSave", str(target)]
    if operation == "batch_solve":
        command = [str(executable), "-ng"]
        if wait_for_license:
            command.append("-WaitForLicense")
        return [*command, "-BatchSolve", str(target)]
    raise ValueError(f"unsupported AEDT operation: {operation}")


def batch_failure_code(text: str) -> str | None:
    lowered = text.lower()
    if "requires parasolid migration" in lowered or "created with acis geometry kernel" in lowered:
        return "hfss_project_kernel_migration_required"
    if "license" in lowered and any(word in lowered for word in ("unavailable", "not available", "failed")):
        return "hfss_license_unavailable"
    if re.search(r"(?im)^\s*(?:error|fatal)\b|call failed|batch solve failed", text):
        return "hfss_batch_failed"
    return None


def terminate_process_tree(process: subprocess.Popen[bytes]) -> None:
    if process.poll() is not None:
        return
    system_root = Path(os.environ.get("SYSTEMROOT", r"C:\Windows"))
    taskkill = system_root / "System32" / "taskkill.exe"
    if taskkill.is_file():
        with suppress(Exception):
            subprocess.run(
                [str(taskkill), "/PID", str(process.pid), "/T", "/F"],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                timeout=15,
                check=False,
                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
            )
    if process.poll() is None:
        with suppress(Exception):
            process.kill()
    with suppress(Exception):
        process.wait(timeout=15)


def run_aedt_process(
    ctx: JobContext,
    command: Sequence[str],
    *,
    stage: str,
    output_log: Path,
    environ: Mapping[str, str] | None = None,
) -> int:
    ctx.update(stage, None)
    creationflags = getattr(subprocess, "CREATE_NO_WINDOW", 0) | getattr(
        subprocess, "CREATE_NEW_PROCESS_GROUP", 0
    )
    merged_env = os.environ.copy()
    if environ is not None:
        merged_env.update(environ)
    with output_log.open("wb") as stream:
        process = subprocess.Popen(
            list(command),
            cwd=ctx.output_dir,
            env=merged_env,
            stdin=subprocess.DEVNULL,
            stdout=stream,
            stderr=subprocess.STDOUT,
            creationflags=creationflags,
        )
        ctx.install_cancel(lambda: terminate_process_tree(process))
        try:
            while process.poll() is None:
                ctx.check()
                time.sleep(min(0.2, max(0.01, ctx.remaining())))
            return int(process.returncode or 0)
        finally:
            if process.poll() is None:
                terminate_process_tree(process)
            else:
                with suppress(Exception):
                    process.wait(timeout=5)

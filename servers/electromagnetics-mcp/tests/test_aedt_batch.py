from pathlib import Path

import numpy as np

from mcphub_em_mcp.aedt_batch import (
    CONFIGURE_SCRIPT,
    EXPORT_SCRIPT,
    aedt_command,
    batch_failure_code,
)
from mcphub_em_mcp.hfss_mesh_vendor.extract_hfss_solver_mesh import canonical_mesh_hash


def test_aedt_commands_are_argument_safe_and_explicit() -> None:
    exe = Path("C:/Program Files/ANSYS Inc/v251/AnsysEM/ansysedt.exe")
    target = Path("C:/workspace/job/model.aedt")
    script = Path("C:/workspace/job/configure.py")

    assert aedt_command(exe, "batch_save", target) == [
        str(exe),
        "-ng",
        "-BatchSave",
        str(target),
    ]
    assert aedt_command(exe, "batch_solve", target, wait_for_license=True) == [
        str(exe),
        "-ng",
        "-WaitForLicense",
        "-BatchSolve",
        str(target),
    ]
    assert aedt_command(exe, "run_script", script) == [
        str(exe),
        "-features=beta",
        "-ng",
        "-RunScriptAndExit",
        str(script),
    ]


def test_batch_log_failure_classes_are_typed() -> None:
    assert batch_failure_code("requires Parasolid migration") == "hfss_project_kernel_migration_required"
    assert batch_failure_code("created with ACIS geometry kernel. It cannot be accessed") == (
        "hfss_project_kernel_migration_required"
    )
    assert batch_failure_code("Licensed feature HFSS is not available") == "hfss_license_unavailable"
    assert batch_failure_code("ERROR: setup failed") == "hfss_batch_failed"
    assert batch_failure_code("Stopping Batch Run") is None


def test_embedded_scripts_use_only_job_local_environment_inputs() -> None:
    for script in (CONFIGURE_SCRIPT, EXPORT_SCRIPT):
        assert "MCPHUB_HFSS_PROJECT" in script
        assert "D:\\" not in script
        assert "C:\\Users" not in script
        assert "ScriptEnv.Initialize" in script

    for setting in (
        "Frequency:=",
        "MaxDeltaS:=",
        "MaximumPasses:=",
        "MinimumPasses:=",
        "MinimumConvergedPasses:=",
        "BasisOrder:=",
        "DoMaterialLambda:=",
    ):
        assert setting in CONFIGURE_SCRIPT
    assert "ExportConvergence" in EXPORT_SCRIPT
    assert "ExportMeshStats" in EXPORT_SCRIPT
    assert "ExportNetworkData" in EXPORT_SCRIPT


def test_mesh_hash_covers_ordered_coordinates_connectivity_and_materials() -> None:
    coords = np.array([[0.0, 0.0, 0.0], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0], [0.0, 0.0, 1.0]])
    tets = np.array([[0, 1, 2, 3]])
    bodies = np.array([7])
    baseline = canonical_mesh_hash(coords, tets, bodies)

    assert baseline == canonical_mesh_hash(coords.copy(), tets.copy(), bodies.copy())
    assert baseline != canonical_mesh_hash(coords, tets, np.array([8]))
    assert baseline != canonical_mesh_hash(coords[[1, 0, 2, 3]], tets, bodies)

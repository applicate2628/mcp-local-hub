from __future__ import annotations

import csv

import pytest

from mcphub_em_mcp.cst import _cst_version, _settings_history, cst_solve
from mcphub_em_mcp.cst_results import (
    CONVERGENCE_PATHS,
    PORT_PATHS,
    PORTMODE_PROGRESSION_PATH,
    REFERENCE_PATHS,
    S_PATHS,
    export_result_tree,
)


class _Environment:
    def version(self) -> str:
        return (
            "This is CST DESIGN ENVIRONMENT 2026.2 Release\n"
            "Using install path: X:/machine/local/\nAccess is denied.\n"
        )


def test_cst_version_excludes_machine_path_and_noise() -> None:
    assert _cst_version(_Environment()) == "CST DESIGN ENVIRONMENT 2026.2 Release"


def test_cst_history_uses_explicit_precision_and_fixed_command_shape() -> None:
    history = _settings_history(
        {
            "adaptation_frequency_ghz": 5.0,
            "frequency_range_ghz": [1.0, 20.0],
            "frequency_samples_ghz": [1.0, 20.0],
            "minimum_passes": 3,
            "maximum_passes": 12,
            "maximum_delta_s": 0.005,
            "propagation_constant_accuracy": 1e-5,
            "propagation_constant_checks": 2,
            "accuracy_tet": 1e-10,
            "order_tet": "First",
            "port_mesh_matches_3d": True,
        }
    )
    assert ".PropagationConstantAccuracy 1.0000000000000001e-05" in history
    assert ".MaxDeltaS 0.0050000000000000001" in history
    assert ".SetPortMeshMatches3DMeshTet True" in history
    assert "Solver.FrequencyRange 1, 20" in history
    assert history.index("Solver.FrequencyRange") < history.index("With FDSolver")
    assert '.SetMethod "Tetrahedral", "Discrete samples only"' in history
    assert '.AddSampleInterval 1, 1, 1, "Single", False' in history
    assert '.AddSampleInterval 20, 20, 1, "Single", False' in history
    assert "@" not in history


def test_cst_history_marks_only_explicit_adaptation_frequency() -> None:
    history = _settings_history(
        {
            "adaptation_frequency_ghz": 5.0,
            "frequency_range_ghz": [1.0, 20.0],
            "frequency_samples_ghz": [1.0, 5.0, 20.0],
            "minimum_passes": 3,
            "maximum_passes": 12,
            "maximum_delta_s": 0.005,
            "propagation_constant_accuracy": 1e-5,
            "propagation_constant_checks": 2,
            "accuracy_tet": 1e-10,
            "order_tet": "First",
            "port_mesh_matches_3d": True,
        }
    )
    assert history.count(', "Single", True') == 1
    assert '.AddSampleInterval 5, 5, 1, "Single", True' in history


def test_cst_solve_rejects_missing_frequency_range(tmp_path) -> None:
    project = tmp_path / "model.cst"
    project.write_bytes(b"fixture")
    with pytest.raises(ValueError, match="frequency_range_ghz"):
        cst_solve(
            project_path=str(project),
            output_root=str(tmp_path),
            adaptation_frequency_ghz=5.0,
            frequency_samples_ghz=[5.0],
            confirm=True,
        )


def test_cst_solve_rejects_samples_outside_frequency_range(tmp_path) -> None:
    project = tmp_path / "model.cst"
    project.write_bytes(b"fixture")
    with pytest.raises(ValueError, match="inside frequency_range_ghz"):
        cst_solve(
            project_path=str(project),
            output_root=str(tmp_path),
            adaptation_frequency_ghz=5.0,
            frequency_range_ghz=[1.0, 10.0],
            frequency_samples_ghz=[5.0, 20.0],
            confirm=True,
        )


class _ResultItem:
    def __init__(self, rows):
        self._rows = rows

    def get_data(self):
        return self._rows


class _ResultModule:
    def __init__(self) -> None:
        self._rows = {
            **{path: [(1.0, 0.1 + 0.01j)] for path in S_PATHS.values()},
            **{path: [(1.0, 50.0 + 0j)] for path in REFERENCE_PATHS.values()},
            **{path: [(1.0, 1.0 + 0j)] for path in PORT_PATHS.values()},
            **{path: [(1.0, 0.0 + 0j)] for path in CONVERGENCE_PATHS.values()},
            PORTMODE_PROGRESSION_PATH: [(1.0, 0.1), (2.0, 0.004), (3.0, 0.003)],
        }

    def get_result_item(self, path):
        return _ResultItem(self._rows[path])


def test_cst_results_exports_portmode_refinement_progression(tmp_path) -> None:
    paths = export_result_tree(
        _ResultModule(),
        tmp_path,
        propagation_constant_accuracy=0.005,
        propagation_constant_checks=2,
    )
    progression = tmp_path / "cst_portmode_refinement_progression.csv"
    assert progression in paths
    with progression.open(encoding="utf-8", newline="") as stream:
        rows = list(csv.DictReader(stream))
    assert [row["port_mesh_refinement_pass"] for row in rows] == ["1", "2", "3"]
    assert [row["consecutive_met"] for row in rows] == ["0", "1", "2"]
    assert [row["terminates_loop"] for row in rows] == ["False", "False", "True"]

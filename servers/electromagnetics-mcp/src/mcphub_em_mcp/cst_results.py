from __future__ import annotations

import cmath
import csv
from collections.abc import Iterable
from pathlib import Path
from typing import Any

S_PATHS = {
    "s11": r"1D Results\S-Parameters\S1,1",
    "s21": r"1D Results\S-Parameters\S2,1",
    "s12": r"1D Results\S-Parameters\S1,2",
    "s22": r"1D Results\S-Parameters\S2,2",
}
REFERENCE_PATHS = {
    "zref1": r"1D Results\Reference Impedance\ZRef 1(1)",
    "zref2": r"1D Results\Reference Impedance\ZRef 2(1)",
}
PORT_PATHS = {
    "gamma1_per_m": r"1D Results\Port Information\Gamma\1(1)",
    "gamma2_per_m": r"1D Results\Port Information\Gamma\2(1)",
    "eps_eff1": r"1D Results\Port Information\Effective Dielectric Constant\1(1)",
    "eps_eff2": r"1D Results\Port Information\Effective Dielectric Constant\2(1)",
    "line_impedance1_ohm": r"1D Results\Port Information\Line Impedance\1(1)",
    "line_impedance2_ohm": r"1D Results\Port Information\Line Impedance\2(1)",
    "wave_impedance1_ohm": r"1D Results\Port Information\Wave Impedance\1(1)",
    "wave_impedance2_ohm": r"1D Results\Port Information\Wave Impedance\2(1)",
}
CONVERGENCE_PATHS = {
    "relative_residual_log10_excitation_1": (
        r"1D Results\Convergence\Equation System Solver\Relative Residual\[1]"
    ),
    "relative_residual_log10_excitation_2": (
        r"1D Results\Convergence\Equation System Solver\Relative Residual\[2]"
    ),
    "iterations_excitation_1": r"1D Results\Convergence\Equation System Solver\Number of Iterations\[1]",
    "iterations_excitation_2": r"1D Results\Convergence\Equation System Solver\Number of Iterations\[2]",
}
PORTMODE_PROGRESSION_PATH = r"1D Results\Convergence\Portmodes\Progression"


def read_series(module: Any, tree_path: str) -> tuple[tuple[float, complex], ...]:
    values: list[tuple[float, complex]] = []
    for record in module.get_result_item(tree_path).get_data():
        if len(record) < 2:
            raise ValueError(f"unexpected CST result tuple at {tree_path}: {record!r}")
        values.append((float(record[0]), complex(record[1])))
    if not values:
        raise ValueError(f"CST result tree item is empty: {tree_path}")
    return tuple(values)


def _frequency_union(series: Iterable[tuple[tuple[float, complex], ...]]) -> list[float]:
    values = sorted({frequency for rows in series for frequency, _ in rows})
    return values


def _write_portmode_progression(
    result_module: Any,
    output_dir: Path,
    propagation_constant_accuracy: float,
    propagation_constant_checks: int,
) -> Path:
    records = result_module.get_result_item(PORTMODE_PROGRESSION_PATH).get_data()
    if not records:
        raise ValueError(f"CST result tree item is empty: {PORTMODE_PROGRESSION_PATH}")
    output = output_dir / "cst_portmode_refinement_progression.csv"
    consecutive = 0
    with output.open("w", encoding="utf-8", newline="") as stream:
        fields = [
            "port_mesh_refinement_pass",
            "delta_kz_over_k0",
            "criterion",
            "meets_criterion",
            "consecutive_met",
            "terminates_loop",
        ]
        writer = csv.DictWriter(stream, fieldnames=fields, lineterminator="\n")
        writer.writeheader()
        for record in records:
            if len(record) < 2:
                raise ValueError(f"unexpected CST result tuple at {PORTMODE_PROGRESSION_PATH}: {record!r}")
            pass_index = float(record[0])
            increment = float(complex(record[1]).real)
            if not pass_index.is_integer() or pass_index < 1 or increment < 0:
                raise ValueError(f"invalid CST port-mode progression tuple: {record!r}")
            meets = increment < propagation_constant_accuracy
            consecutive = consecutive + 1 if meets else 0
            writer.writerow(
                {
                    "port_mesh_refinement_pass": str(int(pass_index)),
                    "delta_kz_over_k0": format(increment, ".17g"),
                    "criterion": format(propagation_constant_accuracy, ".17g"),
                    "meets_criterion": str(meets),
                    "consecutive_met": str(consecutive),
                    "terminates_loop": str(meets and consecutive >= propagation_constant_checks),
                }
            )
    return output


def export_result_tree(
    result_module: Any,
    output_dir: Path,
    propagation_constant_accuracy: float,
    propagation_constant_checks: int,
) -> list[Path]:
    all_paths = {**S_PATHS, **REFERENCE_PATHS, **PORT_PATHS, **CONVERGENCE_PATHS}
    series = {name: read_series(result_module, path) for name, path in all_paths.items()}
    frequencies = _frequency_union(series.values())
    output = output_dir / "cst_results.csv"
    fields = ["frequency"]
    for name in all_paths:
        fields.extend((f"{name}_re", f"{name}_im"))
    maps = {name: {freq: value for freq, value in rows} for name, rows in series.items()}
    with output.open("w", encoding="utf-8", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=fields, lineterminator="\n")
        writer.writeheader()
        for frequency in frequencies:
            row: dict[str, str] = {"frequency": format(frequency, ".17g")}
            for name in all_paths:
                value = maps[name].get(frequency)
                row[f"{name}_re"] = "" if value is None else format(value.real, ".17g")
                row[f"{name}_im"] = "" if value is None else format(value.imag, ".17g")
            writer.writerow(row)
    renormalized = output_dir / "cst_sparams_50ohm.csv"
    _write_renormalized(renormalized, series, 50.0)
    progression = _write_portmode_progression(
        result_module,
        output_dir,
        propagation_constant_accuracy,
        propagation_constant_checks,
    )
    return [output, renormalized, progression]


def _mul(a: list[list[complex]], b: list[list[complex]]) -> list[list[complex]]:
    return [[sum(a[i][k] * b[k][j] for k in range(2)) for j in range(2)] for i in range(2)]


def _inv(a: list[list[complex]]) -> list[list[complex]]:
    determinant = a[0][0] * a[1][1] - a[0][1] * a[1][0]
    if determinant == 0:
        raise ValueError("singular two-port matrix during CST renormalization")
    return [[a[1][1] / determinant, -a[0][1] / determinant], [-a[1][0] / determinant, a[0][0] / determinant]]


def _write_renormalized(
    path: Path, series: dict[str, tuple[tuple[float, complex], ...]], reference: float
) -> None:
    maps = {name: {frequency: value for frequency, value in rows} for name, rows in series.items()}
    frequencies = sorted(
        set(maps["s11"])
        & set(maps["s21"])
        & set(maps["s12"])
        & set(maps["s22"])
        & set(maps["zref1"])
        & set(maps["zref2"])
    )
    with path.open("w", encoding="utf-8", newline="") as stream:
        fields = ["frequency", "s11_re", "s11_im", "s21_re", "s21_im", "s12_re", "s12_im", "s22_re", "s22_im"]
        writer = csv.DictWriter(stream, fieldnames=fields, lineterminator="\n")
        writer.writeheader()
        identity = [[1 + 0j, 0j], [0j, 1 + 0j]]
        for frequency in frequencies:
            s = [
                [maps["s11"][frequency], maps["s12"][frequency]],
                [maps["s21"][frequency], maps["s22"][frequency]],
            ]
            d = [[cmath.sqrt(maps["zref1"][frequency]), 0j], [0j, cmath.sqrt(maps["zref2"][frequency])]]
            plus = [[identity[i][j] + s[i][j] for j in range(2)] for i in range(2)]
            minus = [[identity[i][j] - s[i][j] for j in range(2)] for i in range(2)]
            z = _mul(_mul(d, _mul(plus, _inv(minus))), d)
            ref = [[reference + 0j, 0j], [0j, reference + 0j]]
            renorm = _mul(
                [[z[i][j] - ref[i][j] for j in range(2)] for i in range(2)],
                _inv([[z[i][j] + ref[i][j] for j in range(2)] for i in range(2)]),
            )
            row = {"frequency": format(frequency, ".17g")}
            for name, value in (
                ("s11", renorm[0][0]),
                ("s21", renorm[1][0]),
                ("s12", renorm[0][1]),
                ("s22", renorm[1][1]),
            ):
                row[f"{name}_re"] = format(value.real, ".17g")
                row[f"{name}_im"] = format(value.imag, ".17g")
            writer.writerow(row)

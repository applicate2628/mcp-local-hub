from __future__ import annotations

import ast
import sys
from pathlib import Path

PACKAGE = Path(__file__).parents[1] / "src" / "mcphub_em_mcp"
TESTS = Path(__file__).parent


def _obsolete_topology_relations() -> tuple[str, ...]:
    needles = (
        "cst_saved_field_" + "helper",
        "helper" + "_protocol",
        "test_cst_saved_field_" + "helper",
        "ParentCall" + "Settlement",
        "helper" + " workspace",
        "helper" + " result",
        "parent" + "-owned",
        "class " + "NonceLedger",
        "SavedField" + "BrokerService",
        "InProcess" + "BrokerTransport",
    )
    matches: list[str] = []
    for root in (PACKAGE, TESTS):
        for path in sorted(root.glob("*.py")):
            if path.name == Path(__file__).name:
                continue
            text = path.read_text(encoding="utf-8").lower()
            for needle in needles:
                if needle.lower() in text:
                    matches.append(f"{path.name}:{needle}")
    return tuple(matches)


def test_p10_old_topology_inventory_is_zero() -> None:
    residue = _obsolete_topology_relations()
    assert residue == (), f"P10-OLD-TOPOLOGY-RESIDUE:{len(residue)}:{residue[:8]}"


def test_t12_residue_oracle_detects_reintroduced_parallel_route(tmp_path, monkeypatch) -> None:
    stale = tmp_path / "stale_route.py"
    stale.write_text("class " + "SavedField" + "BrokerService: pass\n", encoding="utf-8")
    tests_root = tmp_path / "tests"
    tests_root.mkdir()
    module = sys.modules[__name__]
    monkeypatch.setattr(module, "PACKAGE", tmp_path)
    monkeypatch.setattr(module, "TESTS", tests_root)
    expected = "stale_route.py:" + "SavedField" + "BrokerService"
    assert _obsolete_topology_relations() == (expected,)


def test_daemon_is_only_broker_client_and_has_no_worker_or_source_edge() -> None:
    frontend_tree = ast.parse((PACKAGE / "cst.py").read_text(encoding="utf-8"))
    frontend_imports = {
        node.module or "" for node in ast.walk(frontend_tree) if isinstance(node, ast.ImportFrom)
    }
    assert any(
        module.rsplit(".", 1)[-1] == "cst_saved_field_daemon_client_windows" for module in frontend_imports
    ), "P10-FRONTEND-DAEMON-CLIENT-MISSING"
    assert not any("broker" in module.rsplit(".", 1)[-1] for module in frontend_imports), (
        "P10-FRONTEND-DIRECT-BROKER-EDGE"
    )

    tree = ast.parse((PACKAGE / "cst_saved_field_daemon_service_windows.py").read_text(encoding="utf-8"))
    imported = {node.module or "" for node in ast.walk(tree) if isinstance(node, ast.ImportFrom)}
    forbidden = {
        "cst_saved_field_containment_windows",
        "cst_saved_field_" + "helper",
        "cst_saved_field_" + "helper" + "_protocol",
        "cst_saved_field_transfer",
        "cst_saved_field_vendor",
        "cst_saved_field_broker_worker",
    }
    hits = tuple(sorted(module for module in imported if module.rsplit(".", 1)[-1] in forbidden))
    assert hits == (), f"P10-DAEMON-AUTHORITY-EDGE:{hits}"
    assert any(module.rsplit(".", 1)[-1] == "cst_saved_field_broker_client_windows" for module in imported), (
        "P10-DAEMON-BROKER-CLIENT-MISSING"
    )


def test_one_broker_worker_route_is_declared() -> None:
    required = (
        "cst_saved_field_broker_protocol.py",
        "cst_saved_field_broker_client_windows.py",
        "cst_saved_field_broker_worker_protocol.py",
        "cst_saved_field_broker_worker.py",
        "cst_saved_field_vendor_isolation_windows.py",
    )
    missing = tuple(name for name in required if not (PACKAGE / name).is_file())
    assert missing == (), f"P10-BROKER-ROUTE-MISSING:{missing}"

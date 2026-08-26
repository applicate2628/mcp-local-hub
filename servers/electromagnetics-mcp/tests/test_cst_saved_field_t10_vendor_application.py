from __future__ import annotations

import ast
from dataclasses import dataclass
from pathlib import Path

import pytest


def test_t10_vendor_depends_only_on_neutral_port_and_has_no_filesystem_fallback() -> None:
    source = Path("src/mcphub_em_mcp/cst_saved_field_vendor.py").read_text(encoding="utf-8")
    tree = ast.parse(source)
    relative_imports = {
        node.module
        for node in ast.walk(tree)
        if isinstance(node, ast.ImportFrom) and node.level and node.module is not None
    }
    assert relative_imports == {"cst_saved_field_port"}
    assert not any(
        isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id == "Path"
        for node in ast.walk(tree)
    )
    assert "._path(" not in source
    assert ".open(" not in source


def test_t10_isolation_requires_fixed_principal_and_retries_one_failed_close() -> None:
    from mcphub_em_mcp.cst_saved_field_vendor_isolation_windows import (
        BROKER_SERVICE,
        IsolatedVendorPathLease,
        VendorIsolationProofV1,
    )

    class Platform:
        def __init__(self) -> None:
            self.handles = {1, 2}
            self.failed_once = False

        def hold_ancestor(self, relative, **shares):
            return 1, f"opaque:{relative}"

        def hold_read_input(self, relative, **shares):
            return 2, f"opaque:{relative}"

        def prepare_output(self, relative):  # pragma: no cover - not needed by this oracle
            raise AssertionError

        def seal_output(self, relative, **shares):  # pragma: no cover - not needed
            raise AssertionError

        def create_clean_input(self, *args, **kwargs):  # pragma: no cover - not needed
            raise AssertionError

        def revalidate(self, handle):
            return handle in self.handles

        def close(self, handle):
            if handle == 2 and not self.failed_once:
                self.failed_once = True
                return False
            self.handles.discard(handle)
            return True

    proof = VendorIsolationProofV1(
        service_name=BROKER_SERVICE,
        token_user=rf"NT SERVICE\{BROKER_SERVICE}",
        workspace_owner=rf"NT SERVICE\{BROKER_SERVICE}",
        protected_dacl=True,
        daemon_access_denied=True,
        session_id=0,
    )
    lease = IsolatedVendorPathLease(Platform(), proof=proof)
    lease.hold_ancestor("model")
    lease.hold_read_input("model/input.sct")
    first = lease.settle()
    assert first.close_succeeded is False and first.owned_remaining == 1
    second = lease.settle()
    assert second.complete and second.close_attempts == 3


@dataclass
class _LeaseReceipt:
    handles_received: int = 1
    close_attempts: int = 1
    close_succeeded: bool = True
    owned_remaining: int = 0

    @property
    def complete(self) -> bool:
        return (
            self.close_attempts >= self.handles_received and self.close_succeeded and not self.owned_remaining
        )


def test_t10_sampler_session_owns_one_lease_and_exact_settlement_order() -> None:
    from mcphub_em_mcp.cst_saved_field_application import SamplerSession

    trace: list[str] = []

    class Lease:
        def settle(self):
            trace.append("lease:settle")
            return _LeaseReceipt()

    class Snapshot:
        def __init__(self) -> None:
            self.calls = 0

        def create_vendor_path_lease(self):
            self.calls += 1
            trace.append("lease:create")
            return Lease()

        def settle(self):
            trace.append("workspace:settle")

    class Owned:
        def clear_geometry_data_cache(self):
            trace.append("session:cache")

        def close_without_save(self):
            trace.append("session:close")

        def is_absent(self):
            trace.append("session:absent")
            return True

    snapshot = Snapshot()
    session = SamplerSession(snapshot)
    borrowed = session.borrow_vendor_path_lease()
    assert borrowed is session.borrow_vendor_path_lease()
    session.adopt_owned(Owned())
    receipt = session.settle(source_changed_role=None)
    assert receipt.complete
    assert snapshot.calls == 1
    assert trace == [
        "lease:create",
        "session:cache",
        "session:close",
        "session:absent",
        "lease:settle",
        "workspace:settle",
    ]


def test_t10_success_cannot_outrun_incomplete_lease_or_workspace_cleanup() -> None:
    from mcphub_em_mcp.cst_saved_field_application import SamplerSession
    from mcphub_em_mcp.cst_saved_field_port import VendorFailure

    class Lease:
        def __init__(self) -> None:
            self.calls = 0

        def settle(self):
            self.calls += 1
            return _LeaseReceipt(close_attempts=self.calls, close_succeeded=False, owned_remaining=1)

    class Snapshot:
        def __init__(self) -> None:
            self.lease = Lease()
            self.workspace_settle_calls = 0

        def create_vendor_path_lease(self):
            return self.lease

        def settle(self):
            self.workspace_settle_calls += 1

    snapshot = Snapshot()
    session = SamplerSession(snapshot)
    session.borrow_vendor_path_lease()
    with pytest.raises(VendorFailure) as raised:
        session.settle(source_changed_role=None)
    assert raised.value.failure_id == "cst_saved_field.session_settle_failed"
    assert snapshot.lease.calls == 2
    assert snapshot.workspace_settle_calls == 0


def test_t10_source_drift_returns_settled_resources_for_application_mapping() -> None:
    from mcphub_em_mcp.cst_saved_field_application import SamplerSession

    class Snapshot:
        def settle(self):
            return None

    receipt = SamplerSession(Snapshot()).settle(source_changed_role="field")
    assert receipt.workspace_settled and receipt.session_settled
    assert receipt.owned_remaining == 0
    assert receipt.source_unchanged is False and receipt.source_changed_role == "field"

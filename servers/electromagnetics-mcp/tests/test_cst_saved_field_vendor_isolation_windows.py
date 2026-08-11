from __future__ import annotations

import pytest


class _Platform:
    def __init__(self) -> None:
        self.calls: list[tuple[object, ...]] = []
        self.handles: dict[int, tuple[str, str]] = {}
        self.next_handle = 1
        self.swap = False

    def hold_ancestor(self, relative: str, *, share_read: bool, share_write: bool, share_delete: bool):
        return self._hold("ancestor", relative, share_read, share_write, share_delete)

    def hold_read_input(self, relative: str, *, share_read: bool, share_write: bool, share_delete: bool):
        return self._hold("read", relative, share_read, share_write, share_delete)

    def prepare_output(self, relative: str):
        return self._hold("output", relative, True, True, False)

    def seal_output(self, relative: str, *, share_read: bool, share_write: bool, share_delete: bool):
        handle, locator = self._hold("seal", relative, share_read, share_write, share_delete)
        return handle, locator, "a" * 64

    def revalidate(self, handle: int) -> bool:
        self.calls.append(("revalidate", handle))
        return not self.swap and handle in self.handles

    def close(self, handle: int) -> bool:
        self.calls.append(("close", handle))
        return self.handles.pop(handle, None) is not None

    def _hold(self, role, relative, read, write, delete):
        handle = self.next_handle
        self.next_handle += 1
        self.handles[handle] = (role, relative)
        self.calls.append((role, relative, read, write, delete))
        return handle, f"opaque:{role}:{relative}"


def test_sr_c5_01_role_shares_output_seal_and_single_owner_settlement() -> None:
    from mcphub_em_mcp.cst_saved_field_vendor_isolation_windows import IsolatedVendorPathLease

    platform = _Platform()
    lease = IsolatedVendorPathLease(platform)
    assert lease.hold_ancestor("model/Result") == "opaque:ancestor:model/Result"
    assert lease.hold_read_input("model/Result/saved/e1.sct") == "opaque:read:model/Result/saved/e1.sct"
    assert lease.prepare_output("activation/vendor_generated.rex") == (
        "opaque:output:activation/vendor_generated.rex"
    )
    sealed = lease.seal_output("activation/vendor_generated.rex")
    assert sealed.locator == "opaque:seal:activation/vendor_generated.rex"
    assert sealed.sha256 == "a" * 64
    assert ("ancestor", "model/Result", True, True, False) in platform.calls
    assert ("read", "model/Result/saved/e1.sct", True, False, False) in platform.calls
    assert ("seal", "activation/vendor_generated.rex", False, False, False) in platform.calls
    receipt = lease.settle()
    assert receipt.handles_received == 4
    assert receipt.close_attempts == 4
    assert receipt.close_succeeded and receipt.owned_remaining == 0


def test_sr_c5_01_leaf_or_ancestor_swap_fails_without_path_fallback() -> None:
    from mcphub_em_mcp.cst_saved_field_vendor_isolation_windows import (
        BrokerIsolationFailure,
        IsolatedVendorPathLease,
    )

    platform = _Platform()
    lease = IsolatedVendorPathLease(platform)
    lease.hold_ancestor("model")
    lease.hold_read_input("model/input.sct")
    platform.swap = True
    with pytest.raises(BrokerIsolationFailure) as raised:
        lease.revalidate_all()
    assert raised.value.failure_id == "cst_saved_field.source_changed"
    assert all(call[0] != "reopen" for call in platform.calls)
    assert lease.settle().owned_remaining == 0

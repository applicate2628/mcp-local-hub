"""Concrete per-invocation owner for saved-field vendor and workspace settlement."""

from __future__ import annotations

from typing import Any

from .cst_saved_field_port import ApplicationSettlement, VendorFailure


class SamplerSession:
    """Own one snapshot, one borrowed vendor lease, and one adopted CST session."""

    def __init__(self, snapshot: Any) -> None:
        self._snapshot = snapshot
        self._lease: Any | None = None
        self._owned: Any | None = None
        self._settled = False

    def borrow_vendor_path_lease(self) -> Any:
        if self._settled:
            raise self._failure("vendor_lease")
        if self._lease is None:
            self._lease = self._snapshot.create_vendor_path_lease()
        return self._lease

    def adopt_owned(self, owned: Any) -> None:
        if self._settled or self._owned is not None:
            raise VendorFailure(
                "cst_saved_field.session_ownership_ambiguous",
                "session_adopt",
                "the sampler session ownership transfer is ambiguous",
            )
        self._owned = owned

    @staticmethod
    def _failure(stage: str) -> VendorFailure:
        return VendorFailure(
            "cst_saved_field.session_settle_failed",
            stage,
            "the saved-field application did not settle every owned resource",
        )

    def settle(self, *, source_changed_role: str | None) -> ApplicationSettlement:
        if self._settled:
            raise self._failure("session_settle")
        cache_cleared = self._owned is None
        closed_without_save = self._owned is None
        session_absent = self._owned is None
        if self._owned is not None:
            try:
                self._owned.clear_geometry_data_cache()
                cache_cleared = True
            except Exception:
                cache_cleared = False
            try:
                self._owned.close_without_save()
                closed_without_save = True
            except Exception:
                closed_without_save = False
            try:
                session_absent = bool(self._owned.is_absent())
            except Exception:
                session_absent = False

        session_settled = session_absent and cache_cleared and closed_without_save
        lease_settled = self._lease is None
        if self._lease is not None:
            for _attempt in range(2):
                try:
                    lease_receipt = self._lease.settle()
                    lease_settled = lease_receipt.complete
                except Exception:
                    lease_settled = False
                if lease_settled:
                    break

        workspace_settled = False
        if session_settled and lease_settled:
            try:
                self._snapshot.settle()
                workspace_settled = True
            except Exception:
                workspace_settled = False

        source_unchanged = source_changed_role is None
        receipt = ApplicationSettlement(
            workspace_settled=workspace_settled,
            session_settled=session_settled and lease_settled,
            source_unchanged=source_unchanged,
            owned_remaining=(int(not session_settled) + int(not lease_settled) + int(not workspace_settled)),
            cache_cleared=cache_cleared,
            closed_without_save=closed_without_save,
            acquisition=None,
            source_changed_role=source_changed_role,  # type: ignore[arg-type]
        )
        if not (receipt.workspace_settled and receipt.session_settled and receipt.owned_remaining == 0):
            raise self._failure("session_settle")
        self._settled = True
        return receipt

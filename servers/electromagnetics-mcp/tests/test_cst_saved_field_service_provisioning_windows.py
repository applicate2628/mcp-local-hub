from __future__ import annotations


def test_fixed_credential_free_service_contract_and_no_logon_path() -> None:
    from mcphub_em_mcp.cst_saved_field_vendor_isolation_windows import service_contract

    contract = service_contract(
        daemon_image=r"C:\Program Files\McpLocalHub\cst-daemon.exe",
        broker_image=r"C:\Program Files\McpLocalHub\cst-broker.exe",
    )
    assert tuple(item.name for item in contract.services) == (
        "McpLocalHubCstDaemon",
        "McpLocalHubCstVendorBroker",
    )
    assert tuple(item.account for item in contract.services) == (
        r"NT SERVICE\McpLocalHubCstDaemon",
        r"NT SERVICE\McpLocalHubCstVendorBroker",
    )
    assert all(item.credential_free and item.session_id == 0 for item in contract.services)
    assert all(item.service_sid_type == "SERVICE_SID_TYPE_UNRESTRICTED" for item in contract.services)
    assert all(item.protected_dacl for item in contract.services)
    assert "password" not in repr(contract).lower()
    assert "logonuser" not in repr(contract).lower()


def test_dry_run_provisioning_and_rollback_are_complete_and_side_effect_free() -> None:
    from mcphub_em_mcp.cst_saved_field_vendor_isolation_windows import (
        dry_run_provisioning,
        dry_run_rollback,
        service_contract,
    )

    contract = service_contract(daemon_image=r"C:\pinned\daemon.exe", broker_image=r"C:\pinned\broker.exe")
    provision = dry_run_provisioning(contract)
    rollback = dry_run_rollback(contract)
    assert provision.dry_run is True and provision.live_scm_calls == 0
    assert rollback.dry_run is True and rollback.live_scm_calls == 0
    assert provision.actions == (
        "create:McpLocalHubCstDaemon",
        "configure_sid:McpLocalHubCstDaemon",
        "protect_dacl:McpLocalHubCstDaemon",
        "create:McpLocalHubCstVendorBroker",
        "configure_sid:McpLocalHubCstVendorBroker",
        "protect_dacl:McpLocalHubCstVendorBroker",
        "restart_required",
    )
    assert rollback.actions == (
        "stop:McpLocalHubCstDaemon",
        "request_broker_settlement",
        "stop:McpLocalHubCstVendorBroker",
        "prove_service_handles_signaled",
        "prove_job_worker_pipe_workspace_absent",
        "disable_policy",
        "delete:McpLocalHubCstDaemon",
        "delete:McpLocalHubCstVendorBroker",
        "revoke_service_acl_state",
        "restart_required",
    )
    assert all("pid" not in action.lower() for action in rollback.actions)

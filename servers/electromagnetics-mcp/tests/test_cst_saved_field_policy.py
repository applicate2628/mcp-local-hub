from __future__ import annotations

import hashlib
import importlib.util
import json
from pathlib import Path

import pytest


def _policy():
    name = "mcphub_em_mcp.cst_saved_field_policy"
    assert importlib.util.find_spec(name) is not None, "typed authority policy module is missing"
    return __import__(name, fromlist=["*"])


class _Platform:
    def __init__(self, module, *, restricted: bool = True, local: bool = True) -> None:
        self.module = module
        self.restricted = restricted
        self.local = local
        self.calls: list[tuple[str, str]] = []

    def prove_file(self, path: Path):
        self.calls.append(("file", str(path)))
        return self.module.WindowsPathIdentityV1(
            canonical_path=str(path),
            volume_serial=11,
            file_id="1" * 32,
            link_count=1,
            reparse=False,
            streams=("::$DATA",),
            long_name_exact=True,
            short_name_present=False,
        )

    def read_verified_file(self, path: Path, *, maximum: int):
        self.calls.append(("held_read", str(path)))
        raw = path.read_bytes()
        if len(raw) > maximum:
            raise OSError("too large")
        return self.prove_file(path), raw, self.restricted

    def prove_directory(self, path: Path):
        self.calls.append(("directory", str(path)))
        return self.module.WindowsPathIdentityV1(
            canonical_path=str(path),
            volume_serial=22,
            file_id="2" * 32,
            link_count=1,
            reparse=False,
            streams=(),
            long_name_exact=True,
            short_name_present=False,
        )

    def prove_restricted_directory(self, path: Path):
        return self.prove_directory(path), self.restricted

    def access_is_restricted(self, path: Path) -> bool:
        self.calls.append(("access", str(path)))
        return self.restricted

    def drive_is_local(self, drive: str) -> bool:
        self.calls.append(("drive", drive))
        return self.local


def _entry() -> dict[str, object]:
    return {
        "entry_id": "line10-e",
        "root": "C:\\allowed",
        "root_identity": {"volume_serial": 22, "file_id": "2" * 32},
        "project_relative": "model.cst",
        "project_sha256": "a" * 64,
        "mesh_sha256": "b" * 64,
        "bundle_manifest_sha256": "c" * 64,
    }


def _write_policy(path: Path, *, enabled: bool = True, entries: list[dict[str, object]] | None = None):
    module = _policy()
    path.write_text(
        json.dumps(
            {
                "schema": "mcphub.cst.saved_field_authority.v1",
                "enabled": enabled,
                "endpoints": dict(module.ENDPOINT_DESCRIPTOR_V1),
                "entries": entries if entries is not None else [_entry()],
                "manifest_schema": "sha256-canonical-file-list-v2",
            },
            sort_keys=True,
            separators=(",", ":"),
        ),
        encoding="utf-8",
    )


@pytest.mark.parametrize("raw", [None, ""])
def test_authority_policy_absent_is_default_off(raw: str | None) -> None:
    module = _policy()
    result = module.load_authority_snapshot(raw, _Platform(module))
    assert result.enabled is False
    assert result.snapshot is None
    assert result.failure_id == "cst_saved_field.policy_disabled"


def test_windows_policy_platform_is_explicit_and_import_safe() -> None:
    module = _policy()
    assert hasattr(module, "WindowsPolicyPlatform")


def test_saved_field_authority_policy_v2(tmp_path: Path) -> None:
    module = _policy()
    policy = tmp_path / "authority.json"
    _write_policy(policy)
    platform = _Platform(module)
    first = module.load_authority_snapshot(str(policy), platform)
    assert first.enabled is True
    assert first.snapshot is not None
    original_revision = first.snapshot.revision
    assert original_revision == hashlib.sha256(policy.read_bytes()).hexdigest()
    assert first.snapshot.entries[0].entry_id == "line10-e"

    _write_policy(policy, enabled=False, entries=[])
    assert first.snapshot.revision == original_revision
    assert first.snapshot.entries[0].entry_id == "line10-e"
    second = module.load_authority_snapshot(str(policy), platform)
    assert second.enabled is False
    assert second.snapshot is None


@pytest.mark.parametrize(
    "mutation",
    [
        lambda doc: {**doc, "extra": True},
        lambda doc: {**doc, "schema": "mcphub.cst.saved_field_authority.v0"},
        lambda doc: {**doc, "enabled": 1},
        lambda doc: {**doc, "entries": [dict(_entry(), project_sha256="A" * 64)]},
        lambda doc: {**doc, "entries": [_entry(), _entry()]},
    ],
)
def test_authority_policy_closed_bounds_and_duplicates_fail_closed(tmp_path: Path, mutation) -> None:
    module = _policy()
    policy = tmp_path / "authority.json"
    base = {
        "schema": "mcphub.cst.saved_field_authority.v1",
        "enabled": True,
        "endpoints": dict(module.ENDPOINT_DESCRIPTOR_V1),
        "entries": [_entry()],
        "manifest_schema": "sha256-canonical-file-list-v2",
    }
    policy.write_text(json.dumps(mutation(base)), encoding="utf-8")
    result = module.load_authority_snapshot(str(policy), _Platform(module))
    assert result.enabled is False
    assert result.snapshot is None
    assert result.failure_id == "cst_saved_field.policy_invalid"


def test_authority_policy_owner_access_and_locality_are_mandatory(tmp_path: Path) -> None:
    module = _policy()
    policy = tmp_path / "authority.json"
    _write_policy(policy)
    for platform in (_Platform(module, restricted=False), _Platform(module, local=False)):
        result = module.load_authority_snapshot(str(policy), platform)
        assert result.enabled is False
        assert result.snapshot is None
        assert result.failure_id == "cst_saved_field.policy_invalid"


def test_authority_match_is_lexical_and_exact_without_platform_calls(tmp_path: Path) -> None:
    module = _policy()
    policy = tmp_path / "authority.json"
    _write_policy(policy)
    platform = _Platform(module)
    snapshot = module.load_authority_snapshot(str(policy), platform).snapshot
    assert snapshot is not None
    platform.calls.clear()

    descriptor = snapshot.authorize("C:\\allowed\\model.cst")
    assert descriptor.entry_id == "line10-e"
    assert descriptor.policy_revision == snapshot.revision
    assert platform.calls == []
    with pytest.raises(module.PolicyFailure) as denied:
        snapshot.authorize("C:\\allowed\\other.cst")
    assert denied.value.failure_id == "cst_saved_field.not_authorized"
    assert platform.calls == []


def test_operator_policy_generation_is_disabled_and_side_effect_free() -> None:
    module = _policy()
    raw = module.canonical_disabled_policy([_entry()])
    assert isinstance(raw, bytes)
    document = json.loads(raw)
    assert document["enabled"] is False
    assert document["schema"] == "mcphub.cst.saved_field_authority.v1"
    assert document["manifest_schema"] == "sha256-canonical-file-list-v2"
    assert document["endpoints"] == dict(module.ENDPOINT_DESCRIPTOR_V1)
    assert document["entries"] == [_entry()]
    assert b"restart" not in raw and b"daemon" not in raw


def test_policy_file_limit_is_exact(tmp_path: Path) -> None:
    module = _policy()
    policy = tmp_path / "authority.json"
    policy.write_bytes(b" " * (1_048_576 + 1))
    result = module.load_authority_snapshot(str(policy), _Platform(module))
    assert result.enabled is False
    assert result.failure_id == "cst_saved_field.policy_invalid"


def test_policy_snapshot_reads_bytes_and_acl_from_one_verified_handle(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    module = _policy()
    policy = tmp_path / "authority.json"
    _write_policy(policy)
    platform = _Platform(module)
    original = platform.read_verified_file
    captured = policy.read_bytes()

    def held_read(path: Path, *, maximum: int):
        evidence, _raw, restricted = original(path, maximum=maximum)
        monkeypatch.setattr(Path, "read_bytes", lambda _self: (_ for _ in ()).throw(AssertionError()))
        monkeypatch.setattr(Path, "stat", lambda _self: (_ for _ in ()).throw(AssertionError()))
        return evidence, captured, restricted

    platform.read_verified_file = held_read  # type: ignore[method-assign]
    result = module.load_authority_snapshot(str(policy), platform)
    assert result.enabled is True
    assert [call[0] for call in platform.calls].count("held_read") == 1


def test_policy_acl_is_exact_allowlist_not_foreign_sid_denylist() -> None:
    module = _policy()
    owner = "S-1-5-21-1-2-3-1001"
    assert module._sddl_access_is_restricted(  # noqa: SLF001
        f"O:{owner}D:(A;;FA;;;{owner})(A;;FA;;;SY)(A;;FA;;;BA)", owner
    )
    assert not module._sddl_access_is_restricted(  # noqa: SLF001
        f"O:{owner}D:(A;;FA;;;{owner})(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;S-1-5-21-9)", owner
    )

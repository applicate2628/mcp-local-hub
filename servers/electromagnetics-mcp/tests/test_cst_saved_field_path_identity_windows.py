from __future__ import annotations

import importlib.util
from dataclasses import replace
from pathlib import Path

import pytest


def _policy():
    name = "mcphub_em_mcp.cst_saved_field_policy"
    assert importlib.util.find_spec(name) is not None, "Windows path identity owner is missing"
    return __import__(name, fromlist=["*"])


ROLES = (
    "policy",
    "root",
    "project",
    "source_row",
    "destination_row",
    "vendor_payload",
    "clean_payload",
    "generated_header",
    "registration",
)


@pytest.mark.parametrize("role", ROLES)
@pytest.mark.parametrize(
    "component",
    [
        *(f"COM{digit}" for digit in range(1, 10)),
        *(f"LPT{digit}" for digit in range(1, 10)),
        "COM¹",
        "COM²",
        "COM³",
        "LPT¹",
        "LPT²",
        "LPT³",
        "com1.txt",
        "LpT².payload.bin",
        "NUL.txt",
        "CONIN$",
    ],
)
def test_saved_field_reserved_device_alias_properties(role: str, component: str) -> None:
    module = _policy()
    calls: list[str] = []

    class Provider:
        def prove(self, *_args, **_kwargs):
            calls.append("filesystem")
            raise AssertionError("lexically invalid path reached provider")

    with pytest.raises(module.PolicyFailure) as raised:
        module.prove_windows_path(f"C:\\allowed\\{component}", provider=Provider(), kind="file", role=role)
    assert raised.value.failure_id == "cst_saved_field.path_namespace_invalid"
    assert calls == []


@pytest.mark.parametrize(
    "raw",
    [
        r"\\server\share\model.cst",
        r"\\?\C:\allowed\model.cst",
        r"\\.\C:\allowed\model.cst",
        r"C:\allowed\model.cst:secret",
        r"C:\allowed\name. ",
        r"C:\allowed\PROGRA~1\model.cst",
        "C:\\allowed\\e\u0301.cst",
        r"C:\allowed\*.cst",
    ],
)
def test_saved_field_windows_path_identity_v1(raw: str) -> None:
    module = _policy()
    calls: list[str] = []

    class Provider:
        def prove(self, *_args, **_kwargs):
            calls.append("filesystem")

    for role in (
        "root",
        "project",
        "manifest",
        "ancillary",
        "source",
        "destination",
        "workspace",
        "vendor",
        "header",
        "payload",
        "registration",
    ):
        with pytest.raises(module.PolicyFailure) as raised:
            module.prove_windows_path(raw, provider=Provider(), kind="file", role=role)
        assert raised.value.failure_id == "cst_saved_field.path_namespace_invalid"
    assert calls == []


def test_saved_field_local_nofollow_boundary() -> None:
    module = _policy()

    class Provider:
        def __init__(self, evidence) -> None:
            self.evidence = evidence
            self.calls = 0

        def prove(self, path: Path, *, kind: str):
            self.calls += 1
            assert str(path) == "C:\\allowed\\project.cst"
            assert kind == "file"
            return self.evidence

    good = module.ObjectIdentityEvidence(
        canonical_path="C:\\allowed\\project.cst",
        volume_serial=7,
        file_id="f" * 32,
        link_count=1,
        reparse=False,
        streams=("::$DATA",),
        long_name_exact=True,
        short_name_present=False,
    )
    provider = Provider(good)
    proof = module.prove_windows_path(
        "C:\\allowed\\project.cst", provider=provider, kind="file", role="project"
    )
    assert proof == good
    assert provider.calls == 1

    mutations = (
        {"canonical_path": "C:\\allowed\\PROJECT.cst"},
        {"file_id": ""},
        {"link_count": 2},
        {"reparse": True},
        {"streams": ("::$DATA", ":secret:$DATA")},
        {"long_name_exact": False},
        {"short_name_present": True},
    )
    for role in ("project", "manifest", "source", "destination", "vendor", "payload"):
        for changes in mutations:
            invalid = replace(good, **changes)
            with pytest.raises(module.PolicyFailure) as raised:
                module.prove_windows_path(
                    "C:\\allowed\\project.cst",
                    provider=Provider(invalid),
                    kind="file",
                    role=role,
                )
            assert raised.value.failure_id == "cst_saved_field.path_identity_ambiguous"

from __future__ import annotations

import argparse
import hashlib
import json
import struct
from pathlib import Path

AMD64 = 0x8664
PE32_PLUS = 0x20B
KERNEL32 = "KERNEL32.dll"
ENTRY_SYMBOL = "mcphub_cst_entry"
DEPENDENT_LOAD_SYSTEM32 = 0x0800
CETCOMPAT = "CETCOMPAT"
HIGHENTROPYVA = 0x0020
DYNAMICBASE = 0x0040
NXCOMPAT = 0x0100
IMAGE_DEBUG_TYPE_EX_DLLCHARACTERISTICS = 20
IMAGE_DLLCHARACTERISTICS_EX_CET_COMPAT = 0x01


class PEError(ValueError):
    pass


def _u16(data: bytes, offset: int) -> int:
    return struct.unpack_from("<H", data, offset)[0]


def _u32(data: bytes, offset: int) -> int:
    return struct.unpack_from("<I", data, offset)[0]


def inspect_image(path: Path) -> dict[str, object]:
    data = path.read_bytes()
    if len(data) < 0x100 or data[:2] != b"MZ":
        raise PEError("not an MZ image")
    pe = _u32(data, 0x3C)
    if data[pe : pe + 4] != b"PE\0\0":
        raise PEError("not a PE image")
    coff = pe + 4
    machine, sections, timestamp, _, _, optional_size, _ = struct.unpack_from("<HHIIIHH", data, coff)
    optional = coff + 20
    if machine != AMD64 or _u16(data, optional) != PE32_PLUS:
        raise PEError("image is not AMD64 PE32+")
    entry_rva = _u32(data, optional + 16)
    dll_characteristics = _u16(data, optional + 70)
    directory_count = _u32(data, optional + 108)
    directory_base = optional + 112
    section_base = optional + optional_size
    parsed_sections: list[dict[str, int | str]] = []
    for index in range(sections):
        off = section_base + index * 40
        name = data[off : off + 8].split(b"\0", 1)[0].decode("ascii", "strict")
        virtual_size, virtual_address, raw_size, raw_offset = struct.unpack_from("<IIII", data, off + 8)
        characteristics = _u32(data, off + 36)
        if characteristics & 0x20000000 and characteristics & 0x80000000:
            raise PEError(f"writable/executable section: {name}")
        parsed_sections.append(
            {
                "name": name,
                "rva": virtual_address,
                "vsize": virtual_size,
                "raw": raw_offset,
                "raw_size": raw_size,
                "characteristics": characteristics,
            }
        )

    def directory(index: int) -> tuple[int, int]:
        if index >= directory_count:
            return (0, 0)
        return struct.unpack_from("<II", data, directory_base + index * 8)

    def rva_offset(rva: int) -> int:
        for section in parsed_sections:
            start = int(section["rva"])
            span = max(int(section["vsize"]), int(section["raw_size"]))
            if start <= rva < start + span:
                return int(section["raw"]) + rva - start
        raise PEError(f"unmapped RVA 0x{rva:x}")

    import_rva, import_size = directory(1)
    if not import_rva or not import_size:
        raise PEError("missing import directory")
    imports: dict[str, list[str]] = {}
    cursor = rva_offset(import_rva)
    while True:
        original, _, _, name_rva, first = struct.unpack_from("<IIIII", data, cursor)
        cursor += 20
        if not any((original, name_rva, first)):
            break
        name_off = rva_offset(name_rva)
        end = data.index(0, name_off)
        dll = data[name_off:end].decode("ascii", "strict")
        thunk = rva_offset(original or first)
        names: list[str] = []
        while True:
            value = struct.unpack_from("<Q", data, thunk)[0]
            thunk += 8
            if value == 0:
                break
            if value & (1 << 63):
                raise PEError("ordinal import forbidden")
            hint_name = rva_offset(value & 0x7FFF_FFFF_FFFF_FFFF) + 2
            end = data.index(0, hint_name)
            names.append(data[hint_name:end].decode("ascii", "strict"))
        imports[dll] = sorted(names)
    if [name.casefold() for name in imports] != [KERNEL32.casefold()]:
        raise PEError(f"direct imports are not exactly {KERNEL32}: {sorted(imports)}")

    forbidden = {"delay_import": 13, "tls": 9, "clr": 14, "bound_import": 11}
    for label, index in forbidden.items():
        if directory(index) != (0, 0):
            raise PEError(f"{label} directory present")
    relocation_rva, relocation_size = directory(5)
    if not relocation_rva or not relocation_size:
        raise PEError("base relocation directory absent")
    debug_rva, debug_size = directory(6)
    cet_compatible = False
    if debug_rva and debug_size and debug_size % 28 == 0:
        debug = rva_offset(debug_rva)
        for item in range(debug_size // 28):
            item_offset = debug + item * 28
            debug_type = _u32(data, item_offset + 12)
            value_size = _u32(data, item_offset + 16)
            value_offset = _u32(data, item_offset + 24)
            if debug_type == IMAGE_DEBUG_TYPE_EX_DLLCHARACTERISTICS and value_size >= 4:
                cet_compatible = bool(_u32(data, value_offset) & IMAGE_DLLCHARACTERISTICS_EX_CET_COMPAT)
    if not cet_compatible:
        raise PEError("CETCOMPAT extended DLL characteristic absent")
    load_rva, load_size = directory(10)
    if not load_rva or load_size < 80:
        raise PEError("load configuration absent or too small")
    dependent_load_flags = _u16(data, rva_offset(load_rva) + 78)
    if dependent_load_flags != DEPENDENT_LOAD_SYSTEM32:
        raise PEError(f"dependent load flags are 0x{dependent_load_flags:x}")
    required = HIGHENTROPYVA | DYNAMICBASE | NXCOMPAT
    if dll_characteristics & required != required:
        raise PEError("HIGHENTROPYVA/DYNAMICBASE/NXCOMPAT incomplete")
    return {
        "machine": "AMD64",
        "entry_rva": entry_rva,
        "entry_symbol": ENTRY_SYMBOL,
        "timestamp": timestamp,
        "direct_imports": list(imports),
        "import_functions": imports[KERNEL32],
        "delay_import": False,
        "tls": False,
        "clr": False,
        "bound_import": False,
        "dependent_load_flags": dependent_load_flags,
        "relocations": True,
        "mitigations": [CETCOMPAT, "HIGHENTROPYVA", "DYNAMICBASE", "NXCOMPAT"],
        "sections": parsed_sections,
        "sha256": hashlib.sha256(data).hexdigest(),
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("image", type=Path)
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()
    try:
        facts = inspect_image(args.image)
    except (OSError, PEError, struct.error, UnicodeError) as exc:
        print(f"native_loader_invalid: {exc}")
        return 78
    print(
        json.dumps(facts, sort_keys=True, separators=(",", ":"))
        if args.json
        else "native PE verification: PASS"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

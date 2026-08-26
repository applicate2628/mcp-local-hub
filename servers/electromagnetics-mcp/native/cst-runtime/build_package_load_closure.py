from __future__ import annotations

import argparse
import hashlib
import json
import struct
from pathlib import Path


class ClosureError(ValueError):
    pass


def _u16(data: bytes, offset: int) -> int:
    return struct.unpack_from("<H", data, offset)[0]


def _u32(data: bytes, offset: int) -> int:
    return struct.unpack_from("<I", data, offset)[0]


def dependency_edges(path: Path) -> tuple[tuple[str, ...], tuple[str, ...]]:
    data = path.read_bytes()
    if len(data) < 0x100 or data[:2] != b"MZ":
        raise ClosureError(f"not a PE image: {path.name}")
    pe = _u32(data, 0x3C)
    if data[pe : pe + 4] != b"PE\0\0" or _u16(data, pe + 24) != 0x20B:
        raise ClosureError(f"not an AMD64 PE32+ image: {path.name}")
    section_count = _u16(data, pe + 6)
    optional_size = _u16(data, pe + 20)
    optional = pe + 24
    directory_count = _u32(data, optional + 108)
    sections = []
    for index in range(section_count):
        off = optional + optional_size + index * 40
        vsize, rva, raw_size, raw = struct.unpack_from("<IIII", data, off + 8)
        sections.append((rva, max(vsize, raw_size), raw))

    def rva_offset(rva: int) -> int:
        for start, size, raw in sections:
            if start <= rva < start + size:
                return raw + rva - start
        raise ClosureError(f"unmapped dependency RVA in {path.name}: 0x{rva:x}")

    def directory(index: int) -> tuple[int, int]:
        if index >= directory_count:
            return (0, 0)
        return struct.unpack_from("<II", data, optional + 112 + index * 8)

    def ascii_name(rva: int) -> str:
        off = rva_offset(rva)
        end = data.index(0, off)
        return data[off:end].decode("ascii", "strict")

    normal: list[str] = []
    import_rva, import_size = directory(1)
    if import_rva and import_size:
        cursor = rva_offset(import_rva)
        while True:
            descriptor = struct.unpack_from("<IIIII", data, cursor)
            cursor += 20
            if not any(descriptor):
                break
            normal.append(ascii_name(descriptor[3]))

    delayed: list[str] = []
    delay_rva, delay_size = directory(13)
    if delay_rva and delay_size:
        cursor = rva_offset(delay_rva)
        while True:
            descriptor = struct.unpack_from("<IIIIIIII", data, cursor)
            cursor += 32
            if not any(descriptor):
                break
            attributes, name_value = descriptor[0], descriptor[1]
            name_rva = (
                name_value
                if attributes & 1
                else name_value - struct.unpack_from("<Q", data, optional + 24)[0]
            )
            delayed.append(ascii_name(name_rva))
    return tuple(normal), tuple(delayed)


def _row(path: Path, relative: str, kind: str, normal: tuple[str, ...], delayed: tuple[str, ...]):
    data = path.read_bytes()
    return {
        "class": kind,
        "name": relative,
        "size": len(data),
        "sha256": hashlib.sha256(data).hexdigest(),
        "normal_dependencies": list(normal),
        "delay_dependencies": list(delayed),
    }


def build_closure(
    package_root: Path,
    roots: tuple[str, ...],
    system_rows: dict[str, Path],
) -> dict[str, object]:
    package_root = package_root.resolve(strict=True)
    package_index: dict[str, list[Path]] = {}
    for path in sorted(package_root.rglob("*"), key=lambda item: item.as_posix().encode("utf-8")):
        if path.is_file() and path.suffix.casefold() in {".exe", ".dll", ".pyd"}:
            package_index.setdefault(path.name.casefold(), []).append(path)
    system_index = {name.casefold(): path.resolve(strict=True) for name, path in system_rows.items()}
    if len(system_index) != len(system_rows):
        raise ClosureError("ambiguous System32 basename")
    pending: list[Path] = []
    for root in roots:
        candidates = package_index.get(Path(root).name.casefold(), [])
        exact = [
            candidate for candidate in candidates if candidate.relative_to(package_root).as_posix() == root
        ]
        if len(exact) != 1:
            raise ClosureError(f"missing or ambiguous package root: {root}")
        pending.append(exact[0])
    package_rows: dict[str, dict[str, object]] = {}
    used_system: set[str] = set()
    while pending:
        current = pending.pop()
        relative = current.relative_to(package_root).as_posix()
        if relative in package_rows:
            continue
        normal, delayed = dependency_edges(current)
        package_rows[relative] = _row(current, relative, "package", normal, delayed)
        for dependency in normal + delayed:
            key = dependency.casefold()
            candidates = package_index.get(key, [])
            if len(candidates) > 1:
                raise ClosureError(f"ambiguous package dependency: {dependency}")
            if len(candidates) == 1:
                pending.append(candidates[0])
            elif key in system_index:
                used_system.add(key)
            else:
                raise ClosureError(f"unresolved dependency: {dependency}")
    ordered_package = [
        package_rows[key] for key in sorted(package_rows, key=lambda item: item.encode("utf-8"))
    ]
    ordered_system = []
    for key in sorted(used_system, key=lambda item: item.encode("utf-8")):
        path = system_index[key]
        normal, delayed = dependency_edges(path)
        ordered_system.append(_row(path, path.name, "system32", normal, delayed))
    return {
        "schema": "mcphub.cst.package-load-closure.v1",
        "package_rows": ordered_package,
        "system32_rows": ordered_system,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--package-root", type=Path, required=True)
    parser.add_argument("--root", action="append", required=True)
    parser.add_argument("--system-row", action="append", default=[])
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    try:
        system_rows = {}
        for value in args.system_row:
            name, separator, path = value.partition("=")
            if (
                not separator
                or not name
                or not path
                or name.casefold() in {key.casefold() for key in system_rows}
            ):
                raise ClosureError("invalid or duplicate --system-row")
            system_rows[name] = Path(path)
        result = build_closure(args.package_root, tuple(args.root), system_rows)
    except (OSError, ClosureError, struct.error, UnicodeError, ValueError) as exc:
        print(f"native_loader_invalid: {exc}")
        return 78
    encoded = json.dumps(result, sort_keys=True, separators=(",", ":")) + "\n"
    if args.output:
        args.output.write_text(encoded, encoding="utf-8", newline="\n")
    else:
        print(encoded, end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

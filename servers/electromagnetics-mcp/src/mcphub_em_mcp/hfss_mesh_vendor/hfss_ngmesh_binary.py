from __future__ import annotations

import math
import re
import struct
from collections import OrderedDict
from pathlib import Path

TOKEN_BIAS = 4_000_000
ID_TOKEN_XOR_MASK = TOKEN_BIAS
ZERO_TOKEN_PAIR = bytes.fromhex("00093d0000093d00")
NGMESH_COORD_XOR_MASK = int.from_bytes(ZERO_TOKEN_PAIR, "little")
NUM_RE = re.compile(
    r"[-+]?\d+(?:\.\d*)?(?:e[-+]?\d+)?|[-+]?\.\d+(?:e[-+]?\d+)?",
    re.IGNORECASE,
)


def token_value(data: bytes, offset: int) -> int | None:
    if offset + 4 > len(data):
        return None
    raw = struct.unpack_from("<i", data, offset)[0]
    if data[offset + 2] == 0x3D and data[offset + 3] == 0x00:
        return raw - TOKEN_BIAS
    if -4_100_000 <= raw <= -3_900_000:
        return raw + TOKEN_BIAS
    return None


def id_token_value(data: bytes, offset: int) -> int | None:
    if offset + 4 > len(data) or data[offset + 3] != 0x00:
        return None
    encoded = struct.unpack_from("<I", data, offset)[0]
    return encoded ^ ID_TOKEN_XOR_MASK


def read_ascii_string(data: bytes, offset: int) -> tuple[str, int] | None:
    length = token_value(data, offset)
    if length is None or length <= 0 or length > 512:
        return None
    start = offset + 4
    end = start + length
    if end > len(data):
        return None
    payload = data[start:end]
    if not all(byte in (9, 10, 13) or 32 <= byte < 127 for byte in payload):
        return None
    return payload.decode("ascii", errors="replace"), end


def read_string_table(data: bytes, offset: int) -> tuple[list[str], int] | None:
    count = token_value(data, offset)
    if count is None or count <= 0 or count > 512:
        return None
    cursor = offset + 4
    names: list[str] = []
    for _ in range(count):
        item = read_ascii_string(data, cursor)
        if item is None:
            return None
        text, cursor = item
        names.append(text)
    return names, cursor


def score_body_name_table(names: list[str]) -> int:
    if len(names) < 2:
        return -1
    lowered = [name.lower() for name in names]
    score = len(names)
    if any(name in {"air", "airbox", "background"} for name in lowered):
        score += 100
    if any("trace" in name for name in lowered):
        score += 50
    if any("dielectric" in name for name in lowered):
        score += 50
    if "no comment" in lowered or any("setup" in name for name in lowered):
        score -= 100
    return score


def find_body_name_table(data: bytes) -> dict[str, object] | None:
    best: tuple[int, int, list[str], int] | None = None
    for offset in range(len(data) - 8):
        table = read_string_table(data, offset)
        if table is None:
            continue
        names, end = table
        score = score_body_name_table(names)
        if score < 0:
            continue
        if best is None or score > best[0]:
            best = (score, offset, names, end)
    if best is None:
        return None
    _score, offset, names, end = best
    return {
        "offset": offset,
        "end_offset": end,
        "count": len(names),
        "names": names,
    }


def decode_header_hints(data: bytes) -> OrderedDict[str, int | None]:
    fields = OrderedDict()
    fields["major"] = struct.unpack_from("<i", data, 0x10)[0] if len(data) >= 0x14 else None
    fields["minor"] = struct.unpack_from("<i", data, 0x14)[0] if len(data) >= 0x18 else None
    fields["point"] = struct.unpack_from("<i", data, 0x18)[0] if len(data) >= 0x1C else None
    fields["token_0x1c"] = token_value(data, 0x1C)
    fields["token_0x20"] = token_value(data, 0x20)
    fields["token_0x24"] = token_value(data, 0x24)
    fields["token_0x28"] = token_value(data, 0x28)
    fields["nbodies_hint_0x2c"] = token_value(data, 0x2C)
    fields["token_0x30"] = id_token_value(data, 0x30)
    fields["npoints_hint_0x34"] = id_token_value(data, 0x34)
    fields["nsurf_hint_0x38"] = id_token_value(data, 0x38)
    fields["nvol_hint_0x3c"] = id_token_value(data, 0x3C)
    return fields


def read_header_tokens(data: bytes) -> OrderedDict[str, int | None]:
    return decode_header_hints(data)


def parse_stats(path: Path | str | None) -> OrderedDict[int, int]:
    result: OrderedDict[int, int] = OrderedDict()
    if path is None:
        return result
    stats_path = Path(path)
    for line in stats_path.read_text(errors="ignore").splitlines():
        values = NUM_RE.findall(line)
        if len(values) < 2:
            continue
        try:
            body_id = int(values[0])
            count = int(values[1])
        except ValueError:
            continue
        result[body_id] = count
    return result


def find_volume_record_table(
    data: bytes,
    stats_counts: OrderedDict[int, int],
) -> dict[str, object] | None:
    positive_total = sum(count for count in stats_counts.values() if count > 0)
    if positive_total <= 0:
        return None

    bodies = set(stats_counts)
    id_one = bytes([1, 9, 0x3D, 0])
    best: dict[str, object] | None = None
    start = 0
    stride = 7 * 4
    while True:
        offset = data.find(id_one, start)
        if offset < 0:
            break
        start = offset + 1
        cursor = offset
        previous = 0
        counts: OrderedDict[int, int] = OrderedDict()
        max_node_id = 0
        records = 0
        while cursor + stride <= len(data):
            element_id = id_token_value(data, cursor)
            body_id = id_token_value(data, cursor + 4)
            nodes = [id_token_value(data, cursor + 4 * index) for index in range(2, 6)]
            locked = token_value(data, cursor + 24)
            if (
                element_id is None
                or body_id not in bodies
                or locked not in (0, 1)
                or any(node is None or node <= 0 for node in nodes)
                or element_id != previous + 1
            ):
                break
            counts[body_id] = counts.get(body_id, 0) + 1
            max_node_id = max(max_node_id, *(int(node) for node in nodes if node is not None))
            previous = element_id
            records += 1
            cursor += stride
            if records >= positive_total:
                break
        if best is None or records > int(best["record_count"]):
            best = {
                "offset": offset,
                "end_offset": cursor,
                "record_count": records,
                "max_node_id": max_node_id,
                "body_counts": {str(body_id): count for body_id, count in counts.items()},
                "matches_stats_counts": records == positive_total
                and all(counts.get(body_id, 0) == count for body_id, count in stats_counts.items()),
            }
        if best and bool(best["matches_stats_counts"]):
            return best
    return best


def read_fixed_f64_or_token_zero(data: bytes, offset: int, count: int) -> list[float]:
    values: list[float] = []
    cursor = offset
    for _ in range(count):
        chunk = data[cursor : cursor + 8]
        if len(chunk) != 8:
            break
        if chunk == ZERO_TOKEN_PAIR:
            values.append(0.0)
        else:
            values.append(struct.unpack_from("<d", data, cursor)[0])
        cursor += 8
    return values


def read_xor_f64_values(data: bytes, offset: int, count: int) -> list[float]:
    values: list[float] = []
    cursor = offset
    for _ in range(count):
        if cursor + 8 > len(data):
            break
        encoded = struct.unpack_from("<Q", data, cursor)[0]
        decoded = encoded ^ NGMESH_COORD_XOR_MASK
        values.append(struct.unpack("<d", struct.pack("<Q", decoded))[0])
        cursor += 8
    return values


def find_point_scalar_block(data: bytes, header: OrderedDict[str, int | None]) -> dict[str, object] | None:
    npoints = header.get("npoints_hint_0x34")
    if npoints is None or npoints <= 0:
        return None
    for offset in range(0, len(data) - 4):
        count = id_token_value(data, offset)
        if count != npoints:
            continue
        cursor = offset + 4
        matched = 0
        for expected in range(1, int(npoints) + 1):
            if id_token_value(data, cursor) != expected:
                break
            matched += 1
            cursor += 4
        if matched != npoints:
            continue
        scalar_count = id_token_value(data, cursor)
        if scalar_count != npoints * 3:
            continue
        data_offset = cursor + 4
        end_offset = data_offset + int(scalar_count) * 8
        payload: dict[str, object] = {
            "point_id_table_offset": offset,
            "point_id_table_end_offset": cursor,
            "point_count": npoints,
            "scalar_count_offset": cursor,
            "scalar_data_offset": data_offset,
            "scalar_count": scalar_count,
            "fixed_8byte_end_offset": end_offset if end_offset <= len(data) else None,
        }
        if end_offset <= len(data):
            values = read_fixed_f64_or_token_zero(data, data_offset, int(scalar_count))
            if len(values) == scalar_count:
                triples = [values[index : index + 3] for index in range(0, len(values), 3)]
                mins = [min(triple[axis] for triple in triples) for axis in range(3)]
                maxs = [max(triple[axis] for triple in triples) for axis in range(3)]
                zero_ratios = [
                    sum(1 for triple in triples if triple[axis] == 0.0) / len(triples) for axis in range(3)
                ]
                payload["fixed_8byte_f64_or_zero_bbox"] = {"min": mins, "max": maxs}
                payload["fixed_8byte_zero_ratios"] = zero_ratios
            decoded = read_xor_f64_values(data, data_offset, int(scalar_count))
            finite = [value for value in decoded if math.isfinite(value)]
            payload["xor_mask_hex"] = ZERO_TOKEN_PAIR.hex(" ")
            payload["xor_decoded_finite_count"] = len(finite)
            if len(decoded) == scalar_count and len(finite) == scalar_count:
                triples = [decoded[index : index + 3] for index in range(0, len(decoded), 3)]
                mins_m = [min(triple[axis] for triple in triples) for axis in range(3)]
                maxs_m = [max(triple[axis] for triple in triples) for axis in range(3)]
                payload["xor_decoded_bbox_m"] = {"min": mins_m, "max": maxs_m}
                payload["xor_decoded_bbox_mm"] = {
                    "min": [value * 1000.0 for value in mins_m],
                    "max": [value * 1000.0 for value in maxs_m],
                }
        return payload
    return None


def build_ngmesh_diagnostics(
    header: OrderedDict[str, int | None],
    stats_counts: OrderedDict[int, int],
    body_table: dict[str, object] | None,
    point_block: dict[str, object] | None,
    volume_table: dict[str, object] | None,
) -> OrderedDict[str, object]:
    stats_total = sum(count for count in stats_counts.values() if count > 0)
    scalar_match = False
    point_count = header.get("npoints_hint_0x34")
    scalar_count = None
    coordinate_bbox_mm = None
    if point_block is not None:
        scalar_count = point_block.get("scalar_count")
        scalar_match = scalar_count == int(point_block.get("point_count", 0)) * 3
        coordinate_bbox_mm = point_block.get("xor_decoded_bbox_mm")
    volume_matches = bool(volume_table and volume_table.get("matches_stats_counts"))
    export_eligible = bool(point_block is not None and scalar_match and volume_matches)
    if export_eligible:
        confidence = "high"
    elif point_block is not None:
        confidence = "medium"
    else:
        confidence = "low"
    return OrderedDict(
        [
            ("point_count_hint", point_count),
            ("volume_count_hint", header.get("nvol_hint_0x3c")),
            ("body_count_hint", header.get("nbodies_hint_0x2c")),
            ("body_name_count", int(body_table.get("count", 0)) if body_table else 0),
            ("stats_body_count_total", stats_total),
            ("point_block_found", point_block is not None),
            ("point_scalar_count", scalar_count),
            ("point_scalar_count_matches_3n", scalar_match),
            ("coordinate_xor_mask_hex", ZERO_TOKEN_PAIR.hex(" ")),
            ("coordinate_bbox_mm", coordinate_bbox_mm),
            ("volume_record_table_found", volume_table is not None),
            ("volume_record_count", int(volume_table.get("record_count", 0)) if volume_table else 0),
            ("volume_record_count_matches_stats", volume_matches),
            ("export_eligible", export_eligible),
            ("confidence", confidence),
        ]
    )


def inspect_binary_ngmesh(
    path: Path | str,
    stats_path: Path | str | None = None,
) -> dict[str, object]:
    ngmesh_path = Path(path)
    stats_file = Path(stats_path) if stats_path is not None else None
    data = ngmesh_path.read_bytes()

    strings = []
    cursor = 0
    while cursor < min(len(data), 4096):
        item = read_ascii_string(data, cursor)
        if item is not None:
            text, end = item
            strings.append({"offset": cursor, "text": text})
            cursor = end
            continue
        cursor += 1

    header = decode_header_hints(data)
    stats_counts = parse_stats(stats_file)
    body_table = find_body_name_table(data)
    point_block = find_point_scalar_block(data, header)
    volume_table = find_volume_record_table(data, stats_counts)
    return {
        "path": str(ngmesh_path),
        "size_bytes": len(data),
        "magic": data[:8].hex(" "),
        "header": header,
        "strings_near_header": strings,
        "body_name_table": body_table,
        "point_scalar_block": point_block,
        "volume_record_table": volume_table,
        "stats_path": str(stats_file) if stats_file else None,
        "stats_body_counts": {str(body_id): count for body_id, count in stats_counts.items()},
        "diagnostics": build_ngmesh_diagnostics(header, stats_counts, body_table, point_block, volume_table),
        "trusted_volume_decode": False,
        "note": (
            "Binary HFSS ngmesh metadata, point scalar blocks, and volume record counts are "
            "inspectable here. This inspector deliberately does not export meshes; use "
            "extract_hfss_solver_mesh.py for coordinate decoding and current.stats validation."
        ),
    }


def inspect_ngmesh(path: Path | str, stats_path: Path | str | None = None) -> dict[str, object]:
    return inspect_binary_ngmesh(path, stats_path)

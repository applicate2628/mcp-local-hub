#!/usr/bin/env python3
"""Read declared evidence as data only. Never extract or execute archive members.

Usage: python inspect_nested.py inputs.json NEW_OUTPUT_DIRECTORY
Exit 0 means reports were written; inspect complete_for_declared_scope separately.
"""
import base64
import binascii
import gzip
import hashlib
import io
import json
import lzma
import os
from pathlib import Path
import re
import stat
import struct
import sys
import tarfile
import time
import zipfile

CHUNK = 65536
MIB = 1024 * 1024
HARD = {"input_bytes": 64 * MIB, "member_bytes": 64 * MIB,
        "expanded_total_bytes": 256 * MIB, "archive_depth": 6,
        "items": 20000, "seconds": 45, "json_depth": 12}
DEFAULT = dict(HARD, member_bytes=32 * MIB)
SHA = re.compile(r"[0-9a-f]{64}\Z")
CUES = ("b-full-route-successor-candidate-v1", "canonical-source-manifest-v5",
        "GO_CONTROLLER_EXTENSION")
STRING_MARK = re.compile(rb'["\\]')
SCALAR_END = re.compile(rb'[\s,\]}:]')


class Limit(Exception):
    pass


class Pairs(list):
    """Keep every JSON object value, including duplicate keys."""


def digest(data):
    return hashlib.sha256(data).hexdigest()


def label(value):
    value = str(value)
    return value if len(value) <= 1024 else value[:768] + "...[sha256:" + digest(value.encode("utf-8", "surrogatepass")) + "]"


def snapshot(st, include_ctime=True):
    return (st.st_dev, st.st_ino, st.st_mode, st.st_size, st.st_mtime_ns,
            st.st_ctime_ns if include_ctime else None, getattr(st, "st_file_attributes", 0))


def safe_stat(path, regular=False):
    st = os.lstat(path)
    if stat.S_ISLNK(st.st_mode) or getattr(st, "st_file_attributes", 0) & 0x400:
        raise ValueError("symlink or reparse point rejected")
    if regular and not stat.S_ISREG(st.st_mode):
        raise ValueError("nonregular input rejected")
    return st


def safe_ancestors(path):
    for parent in reversed(path.parents):
        safe_stat(parent)


def read_native(path, cap, tick=lambda: None, expected=None):
    """Pin path/fd metadata before and after a chunked, exact-length read."""
    path = Path(path).absolute()
    before = safe_stat(path, regular=True)
    if before.st_size > cap:
        raise Limit("input_bytes")
    if expected is not None and before.st_size != expected:
        raise ValueError("pinned input size mismatch")
    flags = os.O_RDONLY | getattr(os, "O_BINARY", 0) | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(path, flags)
    try:
        before_fd = os.fstat(fd)
        # Windows path-stat and fd-stat may report different ctime semantics.
        if snapshot(before_fd, include_ctime=False) != snapshot(before, include_ctime=False):
            raise ValueError("input changed before read")
        chunks, remaining = [], before.st_size
        while remaining:
            tick()
            chunk = os.read(fd, min(CHUNK, remaining))
            if not chunk:
                raise ValueError("input ended before pinned size")
            chunks.append(chunk)
            remaining -= len(chunk)
        after_fd = os.fstat(fd)
        after_path = safe_stat(path, regular=True)
        if snapshot(after_fd) != snapshot(before_fd) or snapshot(after_path) != snapshot(before):
            raise ValueError("input changed during read")
        return b"".join(chunks)
    finally:
        os.close(fd)


def write_exclusive(path, data):
    safe_ancestors(path)
    with open(path, "xb") as stream:
        for offset in range(0, len(data), CHUNK):
            stream.write(data[offset:offset + CHUNK])
        stream.flush()
        os.fsync(stream.fileno())
    reread = read_native(path, len(data), expected=len(data))
    if digest(reread) != digest(data):
        raise OSError("artifact readback SHA-256 mismatch")
    return {"file": path.name, "bytes": len(data), "sha256": digest(data),
            "fsynced_and_readback_verified": True}


def classify(data, name):
    low = name.lower()
    if data.startswith((b"GIF87a", b"GIF89a")):
        return "gif"
    if data.startswith((b"PK\x03\x04", b"PK\x05\x06", b"PK\x07\x08")):
        return "zip"
    if data.startswith(b"\x1f\x8b"):
        return "gzip"
    if data.startswith(b"\xfd7zXZ\x00"):
        return "xz"
    if len(data) >= 262 and data[257:262] == b"ustar":
        return "tar"
    if data.startswith((b"7z\xbc\xaf\x27\x1c", b"Rar!", b"BZh")):
        return "unsupported_archive"
    if low.endswith(".zip"):
        return "zip_by_name"
    if low.endswith((".gz", ".tgz")):
        return "gzip_by_name"
    if low.endswith(".xz"):
        return "xz_by_name"
    if low.endswith(".tar"):
        return "tar_by_name"
    if low.endswith((".jsonl", ".ndjson")):
        return "jsonl"
    if low.endswith(".json"):
        return "json"
    if data.lstrip(b" \t\r\n\xef\xbb\xbf").startswith((b"{", b"[")):
        return "possible_json"
    return "text_or_binary"


class Inspector:
    def __init__(self, spec, output):
        self.spec, self.output = spec, output
        self.limits = dict(DEFAULT)
        for key, value in spec.get("limits", {}).items():
            if key not in HARD or isinstance(value, bool) or not isinstance(value, (int, float)) or not 0 < value <= HARD[key]:
                raise ValueError("invalid or unsupported limit: " + key)
            if key != "seconds" and int(value) != value:
                raise ValueError("nonintegral limit: " + key)
            self.limits[key] = value if key == "seconds" else int(value)
        self.targets = spec["targets"]
        if not isinstance(self.targets, dict) or not self.targets or any(not SHA.fullmatch(v) for v in self.targets.values()):
            raise ValueError("targets must contain full lowercase SHA-256 values")
        self.prior = set(spec.get("seen_archive_sha256", []))
        if any(not SHA.fullmatch(v) for v in self.prior):
            raise ValueError("invalid prior archive SHA-256")
        self.start = time.monotonic()
        self.items = self.expanded = 0
        self.readset, self.archives, self.hits, self.cues = [], [], [], []
        self.errors, self.limitations, self.saved, self.seen = [], [], {}, {}
        self.inputs = []
        self.json_details = []

    def tick(self):
        if time.monotonic() - self.start > self.limits["seconds"]:
            raise Limit("soft_time_budget")

    def item(self):
        self.tick()
        if self.items >= self.limits["items"]:
            raise Limit("items")
        self.items += 1

    def issue(self, source, reason, error=False):
        text = str(reason)
        for token, native in self.spec["roots"].items():
            text = text.replace(native, token)
        row = {"source": label(source), "reason": label(text)}
        (self.errors if error else self.limitations).append(row)

    def failed(self, source, exc):
        if isinstance(exc, Limit):
            self.issue(source, str(exc))
        else:
            message = getattr(exc, "strerror", None) or str(exc)
            self.issue(source, type(exc).__name__ + ": " + message, error=True)

    def read_stream(self, stream, source, cap, expected=None):
        if expected is not None and expected > cap:
            raise Limit("member_bytes: declared " + str(expected) + ", cap " + str(cap))
        chunks, total = [], 0
        while expected is None or total < expected:
            self.tick()
            room = min(cap - total, self.limits["expanded_total_bytes"] - self.expanded)
            if room <= 0:
                raise Limit("expanded_total_bytes" if self.expanded >= self.limits["expanded_total_bytes"] else "member_bytes: reached cap before EOF was established")
            count = min(CHUNK, room, expected - total if expected is not None else CHUNK)
            chunk = stream.read(count)
            if not chunk:
                if expected is not None and total != expected:
                    raise ValueError("member ended before declared size")
                break
            chunks.append(chunk)
            total += len(chunk)
            self.expanded += len(chunk)
        return b"".join(chunks)

    def record(self, data, source, name):
        self.item()
        sha = digest(data)
        roles = sorted(role for role, target in self.targets.items() if sha == target)
        prefix = sha.startswith("9315")
        row = {"source": label(source), "bytes": len(data), "sha256": sha,
               "targets": roles, "prefix9315": prefix, "whole_bytes_checked": True,
               "classification": classify(data, name), "header_hex": data[:16].hex()}
        self.readset.append(row)
        cue_names = list(CUES) + list(self.targets.values())
        content_cues = [cue for cue in cue_names if cue.encode("ascii") in data or
                        (cue in self.targets.values() and cue.upper().encode("ascii") in data)]
        path_cues = [cue for cue in cue_names if cue.lower() in source.lower()]
        if content_cues or path_cues:
            self.cues.append({"source": label(source), "content_cues": content_cues,
                              "path_cues": path_cues, "content_scan": "full_bytes"})
        if row["classification"] == "gif":
            row["gif_header"] = data[:6].decode("ascii")
            if len(data) >= 10:
                row["gif_logical_screen_pixels"] = list(struct.unpack_from("<HH", data, 6))
            row["gif_scope"] = "whole-byte hash and header only; no image decoding or generation"
        if roles or prefix:
            if sha not in self.saved:
                filename = "candidate-sha256-" + sha + ".bin"
                self.saved[sha] = write_exclusive(self.output / filename, data)
            self.hits.append(dict(row, artifact=self.saved[sha]["file"],
                                  qualification="exact_target" if roles else "unqualified_hash_prefix_only"))
        return row

    def inspect(self, data, source, name, depth=0, force=False, recorded=None):
        try:
            row = recorded or self.record(data, source, name)
            kind = row["classification"]
            if kind in ("json", "jsonl", "possible_json"):
                if depth > self.limits["archive_depth"]:
                    raise Limit("archive_depth (container/decoded-value transforms)")
                self.inspect_json(data, source, name, depth, kind)
            elif kind == "unsupported_archive":
                raise Limit("unsupported_archive_format")
            elif kind.replace("_by_name", "") in ("zip", "tar", "gzip", "xz"):
                self.archive(data, source, name, depth, row, force=force)
        except Exception as exc:
            self.failed(source, exc)

    def zip_open(self, data):
        # Bound central-directory allocation before zipfile constructs its index.
        start = max(0, len(data) - 65557)
        pos = data.rfind(b"PK\x05\x06", start)
        if pos >= 0 and pos + 22 <= len(data):
            count = struct.unpack_from("<H", data, pos + 10)[0]
            if count == 65535:
                raise Limit("ZIP64 central-directory count is outside this bounded parser")
            if count > self.limits["items"]:
                raise Limit("ZIP directory items")
        return zipfile.ZipFile(io.BytesIO(data))

    def archive(self, data, source, name, depth, row, force=False, requested=None):
        kind = row["classification"].replace("_by_name", "")
        entry = {"source": label(source), "sha256": row["sha256"], "kind": kind,
                 "depth": depth, "scope": "requested_members" if requested is not None else "all_members",
                 "status": "started"}
        self.archives.append(entry)
        before = len(self.errors) + len(self.limitations)
        try:
            if depth >= self.limits["archive_depth"]:
                raise Limit("archive_depth")
            if requested is None and not force:
                if row["sha256"] in self.prior:
                    entry.update(status="deduplicated_prior_run", provenance="supplied seen_archive_sha256")
                    self.issue(source, "prior archive SHA-256 deduplicated; prior content check not repeated")
                    return
                if row["sha256"] in self.seen:
                    entry.update(status="deduplicated_this_run", duplicate_of=self.seen[row["sha256"]])
                    self.issue(source, "archive SHA-256 already encountered in this run; see duplicate_of")
                    return
                self.seen[row["sha256"]] = label(source)
            if force and row["sha256"] in self.prior:
                entry["prior_deduplication_override"] = "explicit inspect_root"
            if requested is not None and row["sha256"] in self.prior:
                entry["prior_seen_container_index_reopened"] = True
            if kind in ("gzip", "xz"):
                cap = HARD["input_bytes"] if requested is not None else self.limits["member_bytes"]
                stream = gzip.GzipFile(fileobj=io.BytesIO(data), mode="rb") if kind == "gzip" else lzma.LZMAFile(io.BytesIO(data), "rb")
                with stream:
                    unpacked = self.read_stream(stream, source, cap)
                inner_name = name[:-3] if name.lower().endswith((".gz", ".xz")) else name + ".decoded"
                if name.lower().endswith(".tgz"):
                    inner_name = name[:-4] + ".tar"
                inner_source = source + "::" + kind + "-decoded"
                inner = self.record(unpacked, inner_source, inner_name)
                if requested is not None:
                    if inner["classification"].replace("_by_name", "") not in ("zip", "tar", "gzip", "xz"):
                        raise ValueError("requested-member container did not decode to a supported archive")
                    self.archive(unpacked, inner_source, inner_name, depth + 1, inner, requested=requested)
                else:
                    self.inspect(unpacked, inner_source, inner_name, depth + 1, recorded=inner)
            elif kind == "zip":
                with self.zip_open(data) as archive:
                    infos = archive.infolist()
                    entry["directory_entries"] = len(infos)
                    if len(infos) > self.limits["items"]:
                        raise Limit("ZIP directory items")
                    self.members(archive, infos, source, depth, requested, "zip", entry)
            elif kind == "tar":
                with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as archive:
                    self.members(archive, archive, source, depth, requested, "tar", entry)
            else:
                raise ValueError("declared container has no supported archive signature or name")
            entry["status"] = "checked_declared_scope" if before == len(self.errors) + len(self.limitations) else "partial"
        except Exception as exc:
            entry["status"] = "skipped" if isinstance(exc, Limit) else "error"
            entry["reason"] = label(type(exc).__name__ + ": " + str(exc))
            self.failed(source, exc)

    def members(self, archive, infos, source, depth, requested, kind, entry):
        wanted = {m["name"]: m for m in requested} if requested is not None else None
        if wanted is not None and len(wanted) != len(requested):
            raise ValueError("duplicate requested member names")
        found, names = set(), set()
        entry["members_read"] = 0
        for number, info in enumerate(infos, 1):
            self.tick()
            if number > self.limits["items"]:
                raise Limit("archive directory items")
            member_name = info.filename if kind == "zip" else info.name
            if wanted is not None and member_name not in wanted:
                continue
            member_source = label(source + "::" + member_name)
            request = wanted.get(member_name) if wanted is not None else None
            found.add(member_name)
            if member_name in names:
                self.issue(member_source, "duplicate archive member name; every occurrence is considered")
            names.add(member_name)
            try:
                self.item()
                if kind == "zip":
                    mode = info.external_attr >> 16
                    if info.is_dir():
                        if request is not None:
                            raise ValueError("requested member is a directory")
                        continue
                    if stat.S_ISLNK(mode) or (stat.S_IFMT(mode) and not stat.S_ISREG(mode)):
                        raise Limit("nonregular ZIP member")
                    size = info.file_size
                    opener = lambda: archive.open(info, "r")
                else:
                    if info.isdir():
                        if request is not None:
                            raise ValueError("requested member is a directory")
                        continue
                    if not info.isfile() or info.issparse():
                        raise Limit("nonregular or sparse TAR member")
                    size = info.size
                    opener = lambda: archive.extractfile(info)
                cap = HARD["input_bytes"] if request is not None else self.limits["member_bytes"]
                if request is not None and "bytes" in request and size != request["bytes"]:
                    raise ValueError("pinned member size mismatch")
                with opener() as stream:
                    payload = self.read_stream(stream, member_source, cap, expected=size)
                record = self.record(payload, member_source, member_name)
                entry["members_read"] += 1
                if request is not None and "sha256" in request and record["sha256"] != request["sha256"]:
                    raise ValueError("pinned member SHA-256 mismatch")
                self.inspect(payload, member_source, member_name, depth + 1, recorded=record)
            except Exception as exc:
                self.failed(member_source, exc)
                if isinstance(exc, Limit) and str(exc) in ("items", "soft_time_budget", "expanded_total_bytes"):
                    raise
        if wanted is not None:
            for missing in sorted(set(wanted) - found):
                self.issue(source + "::" + missing, "requested member missing", error=True)
            entry["requested_members"] = sorted(wanted)
            entry["found_requested_members"] = sorted(found)

    def json_preflight(self, data, node_budget):
        # Reject excessive nesting/node counts before json.loads allocates a tree.
        index = nodes = depth = 0
        while index < len(data):
            self.tick()
            char = data[index]
            if char in b" \t\r\n,:\xef\xbb\xbf":
                index += 1
                continue
            if char == 34:
                index += 1
                while True:
                    mark = STRING_MARK.search(data, index)
                    if mark is None:
                        raise ValueError("unterminated JSON string")
                    index = mark.end()
                    if mark[0] == b'"':
                        break
                    index += 1
                    self.tick()
                nodes += 1
            elif char in b"{[":
                depth += 1
                nodes += 1
                index += 1
                if depth > self.limits["json_depth"]:
                    raise Limit("json_depth")
            elif char in b"}]":
                depth -= 1
                index += 1
            else:
                match = SCALAR_END.search(data, index + 1)
                index = match.start() if match else len(data)
                nodes += 1
            if nodes > node_budget:
                raise Limit("JSON per-document node budget before parsing")
        return nodes

    def inspect_json(self, data, source, name, depth, kind):
        lines = io.BytesIO(data) if kind == "jsonl" else (data,)
        total_lines = data.count(b"\n") + (bool(data) and not data.endswith(b"\n")) if kind == "jsonl" else None
        parsed = 0
        detail = {"source": label(source), "parsed_records": 0, "nodes": 0,
                  "short_metadata_strings_hash_checked": 0, "value_candidates": 0}
        self.json_details.append(detail)
        for index, line in enumerate(lines, 1):
            if not line.strip():
                continue
            line_source = source + ("::line:" + str(index) if kind == "jsonl" else "")
            try:
                detail["nodes"] += self.json_preflight(line, self.limits["items"] - detail["nodes"])
                value = json.loads(line, object_pairs_hook=Pairs, parse_int=lambda _: None,
                                   parse_float=lambda _: None,
                                   parse_constant=lambda token: (_ for _ in ()).throw(ValueError("non-JSON constant " + token)))
            except Limit as exc:
                self.issue(line_source, str(exc))
                if total_lines is not None:
                    self.issue(source, "JSONL partial: parsed " + str(parsed) + " records; physical lines " + str(total_lines) + "; whole raw bytes were hash/cue checked")
                return
            except (ValueError, UnicodeError) as exc:
                if kind != "possible_json":
                    self.failed(line_source, exc)
                else:
                    self.readset[-1]["json_probe"] = "not valid JSON; retained as data"
                continue
            parsed += 1
            detail["parsed_records"] = parsed
            try:
                self.walk_json(value, line_source + "::$", depth, detail)
            except Limit as exc:
                self.issue(line_source, str(exc))
                if total_lines is not None:
                    self.issue(source, "JSONL partial: parsed " + str(parsed) + " records; physical lines " + str(total_lines) + "; whole raw bytes were hash/cue checked")
                return

    def walk_json(self, value, source, depth, detail, key=""):
        self.tick()
        if isinstance(value, Pairs):
            for index, (name, child) in enumerate(value):
                child_source = label(source + "[" + json.dumps(label(name), ensure_ascii=True) + ";pair=" + str(index) + "]")
                self.walk_json(child, child_source, depth, detail, key=name)
        elif isinstance(value, list):
            for index, child in enumerate(value):
                self.walk_json(child, label(source + "[" + str(index) + "]"), depth, detail)
        elif isinstance(value, str):
            encoded = value.encode("utf-8", "surrogatepass")
            if len(encoded) > self.limits["member_bytes"]:
                raise Limit("JSON decoded string member_bytes")
            if self.expanded + len(encoded) > self.limits["expanded_total_bytes"]:
                raise Limit("expanded_total_bytes")
            explicit = "base64" in key.lower() or "b64" in key.lower()
            candidate = encoded
            if candidate.startswith(b"data:") and b";base64," in candidate[:256]:
                candidate = candidate.split(b";base64,", 1)[1]
                explicit = True
            sha = digest(encoded)
            if explicit or len(encoded) >= 256 or encoded.lstrip().startswith((b"{", b"[")) or sha in self.targets.values() or sha.startswith("9315"):
                if detail["value_candidates"] >= 1000:
                    raise Limit("JSON per-document candidate budget (1000)")
                detail["value_candidates"] += 1
                self.expanded += len(encoded)
                self.inspect(encoded, source + "::utf8-string", key, depth + 1)
            else:
                detail["short_metadata_strings_hash_checked"] += 1
            compact = b"".join(candidate.split())
            if (explicit or len(compact) >= 256 or compact.startswith((b"/Td6WFo", b"H4sI", b"UEsDB", b"UEsFB"))) and len(compact) % 4 == 0 and re.fullmatch(rb"[A-Za-z0-9+/_-]*={0,2}", compact):
                size = len(compact) // 4 * 3 - (len(compact) - len(compact.rstrip(b"=")))
                if size > self.limits["member_bytes"] or self.expanded + size > self.limits["expanded_total_bytes"]:
                    raise Limit("base64 decoded byte budget")
                chunks = []
                try:
                    if detail["value_candidates"] >= 1000:
                        raise Limit("JSON per-document candidate budget (1000)")
                    detail["value_candidates"] += 1
                    for offset in range(0, len(compact), CHUNK):
                        self.tick()
                        part = base64.b64decode(compact[offset:offset + CHUNK], altchars=b"-_", validate=True)
                        chunks.append(part)
                        self.expanded += len(part)
                    self.inspect(b"".join(chunks), source + "::base64-decoded", "", depth + 1)
                except binascii.Error as exc:
                    if explicit:
                        self.failed(source, exc)
            elif explicit and compact:
                self.issue(source, "explicit base64 field has unsupported/invalid encoding", error=True)

    def resolve(self, public):
        matches = [(token, native) for token, native in self.spec["roots"].items() if public == token or public.startswith(token + "\\") or public.startswith(token + "/")]
        if len(matches) != 1:
            raise ValueError("input path must begin with exactly one declared root token")
        token, native = matches[0]
        parts = public[len(token):].replace("\\", "/").strip("/").split("/")
        if any(part in ("", ".", "..") or ":" in part for part in parts):
            raise ValueError("invalid relative input path")
        return Path(native).joinpath(*parts)

    def run(self):
        for declared in self.spec["inputs"]:
            source = declared["path"]
            state = {"source": source, "status": "started"}
            self.inputs.append(state)
            before = len(self.errors) + len(self.limitations)
            try:
                self.tick()
                if not isinstance(declared["bytes"], int) or not SHA.fullmatch(declared["sha256"]):
                    raise ValueError("outer input requires byte size and full SHA-256 pins")
                data = read_native(self.resolve(source), self.limits["input_bytes"], self.tick, declared["bytes"])
                state["resolved_path_sha256"] = digest(os.path.realpath(self.resolve(source)).encode("utf-8", "surrogatepass"))
                row = self.record(data, source, source)
                if row["sha256"] != declared["sha256"]:
                    raise ValueError("pinned outer SHA-256 mismatch")
                state["outer_pin_verified"] = True
                if declared.get("inspect_root"):
                    self.inspect(data, source, source, force=True, recorded=row)
                elif declared.get("members"):
                    self.archive(data, source, source, 0, row, requested=declared["members"])
                else:
                    raise ValueError("input has neither inspect_root nor requested members")
                state["status"] = "checked_declared_scope" if before == len(self.errors) + len(self.limitations) else "partial"
            except Exception as exc:
                state["status"] = "skipped" if isinstance(exc, Limit) else "error"
                self.failed(source, exc)
        return self.report()

    def report(self):
        complete = not self.errors and not self.limitations and len(self.inputs) == len(self.spec["inputs"])
        return {"schema": "receiver-nested-archive-report-v1", "targets": self.targets,
                "limits": self.limits, "complete_for_declared_scope": complete,
                "scope": "only declared inputs and requested members; no discovery, repair, extraction, or execution",
                "counting": "items include byte candidates and traversed selected/archive entries; JSON nodes use an independent per-document items-sized budget; expansion includes decompression, retained JSON strings, and base64 outputs; depth counts container/decoded-value transforms",
                "json_policy": "all decoded string values are SHA-256 checked; short metadata values without a hit are counted rather than individually listed; long/structured/explicit-base64 values and actual hits are candidates; at most 1000 value candidates per document",
                "requested_member_bytes_ceiling": HARD["input_bytes"],
                "budget_used": {"items": self.items, "expanded_bytes": self.expanded},
                "input_status": self.inputs, "readset": self.readset, "archives": self.archives,
                "hits": self.hits, "cues": self.cues, "errors": self.errors,
                "json_details": self.json_details,
                "limitations": self.limitations, "artifacts": sorted(self.saved.values(), key=lambda x: x["file"]),
                "prefix9315_policy": "unqualified candidate only; never proof of exact historical template"}


def main(argv=None):
    args = sys.argv[1:] if argv is None else argv
    if len(args) != 2:
        raise ValueError("usage: inspect_nested.py SPEC.json NEW_OUTPUT_DIRECTORY")
    raw = read_native(Path(args[0]), 2 * MIB)
    spec = json.loads(raw)
    output = Path(args[1]).absolute()
    safe_ancestors(output)
    inspector = Inspector(spec, output)
    output.mkdir(exist_ok=False)
    report = inspector.run()
    report["input_spec_sha256"] = digest(raw)
    report_info = write_exclusive(output / "report.json", (json.dumps(report, sort_keys=True, indent=2, ensure_ascii=True) + "\n").encode("utf-8"))
    summary = {"schema": "receiver-nested-archive-summary-v1",
               "complete_for_declared_scope": report["complete_for_declared_scope"],
               "input_count": len(inspector.inputs), "whole_byte_candidates": len(inspector.readset),
               "exact_target_hits": [h for h in inspector.hits if h["targets"]],
               "unqualified_prefix9315_hits": [h for h in inspector.hits if not h["targets"]],
               "archive_status_counts": {status: sum(a["status"] == status for a in inspector.archives) for status in sorted({a["status"] for a in inspector.archives})},
               "error_count": len(inspector.errors), "limitation_count": len(inspector.limitations),
               "budget_used": report["budget_used"], "report_artifact": report_info,
               "candidate_artifacts": report["artifacts"]}
    summary_info = write_exclusive(output / "summary.json", (json.dumps(summary, sort_keys=True, indent=2, ensure_ascii=True) + "\n").encode("utf-8"))
    print(json.dumps({"report": report_info, "summary": summary_info,
                      "complete_for_declared_scope": report["complete_for_declared_scope"]}, sort_keys=True))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:
        # No native path strings are needed in the external failure message.
        print(json.dumps({"status": "setup_or_artifact_failure", "error_type": type(error).__name__}), file=sys.stderr)
        sys.exit(2)

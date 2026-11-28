#!/usr/bin/env python3
"""Focused regression for Windows path-stat/fd-stat ctime differences."""
from contextlib import ExitStack
import json
import sys
from types import SimpleNamespace
from unittest.mock import patch

sys.dont_write_bytecode = True
import inspect_nested as inspector


def stat_record(ctime, **changes):
    fields = dict(st_dev=15424946169900087391, st_ino=2814749773396785,
                  st_mode=33206, st_size=3, st_mtime_ns=1795883803645445100,
                  st_ctime_ns=ctime, st_file_attributes=32)
    fields.update(changes)
    return SimpleNamespace(**fields)


def check(path_before, fd_before, path_after, fd_after, rejected=False):
    with ExitStack() as stack:
        stack.enter_context(patch.object(inspector, "safe_stat", side_effect=[path_before, path_after]))
        stack.enter_context(patch.object(inspector.os, "open", return_value=42))
        stack.enter_context(patch.object(inspector.os, "fstat", side_effect=[fd_before, fd_after]))
        stack.enter_context(patch.object(inspector.os, "read", return_value=b"abc"))
        close = stack.enter_context(patch.object(inspector.os, "close"))
        try:
            result = inspector.read_native("fixture.bin", cap=3, expected=3)
        except ValueError as exc:
            assert rejected and "input changed" in str(exc), str(exc)
        else:
            assert not rejected and result == b"abc"
        close.assert_called_once_with(42)


def main():
    path_ctime, fd_ctime = 1795883803644441500, 1795883803645445100
    path_before, fd_before = stat_record(path_ctime), stat_record(fd_ctime)
    check(path_before, fd_before, stat_record(path_ctime), stat_record(fd_ctime))
    for field, changed in (("st_mtime_ns", path_before.st_mtime_ns + 1), ("st_size", 4)):
        check(path_before, fd_before, stat_record(path_ctime, **{field: changed}), stat_record(fd_ctime), rejected=True)
        check(path_before, fd_before, stat_record(path_ctime), stat_record(fd_ctime, **{field: changed}), rejected=True)
        check(path_before, stat_record(fd_ctime, **{field: changed}), stat_record(path_ctime), stat_record(fd_ctime), rejected=True)
    check(path_before, fd_before, stat_record(path_ctime + 1), stat_record(fd_ctime), rejected=True)
    check(path_before, fd_before, stat_record(path_ctime), stat_record(fd_ctime + 1), rejected=True)
    print(json.dumps({"status": "PASS", "regression": "path/fd ctime differences accepted only when each method remains stable; size/mtime differences and within-method ctime changes rejected", "cases": 9}))


if __name__ == "__main__":
    main()

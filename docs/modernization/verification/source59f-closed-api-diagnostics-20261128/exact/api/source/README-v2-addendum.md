# API receiver v2: exact inventory record shape

The root executed v1 after reviewing it. It stopped while receiving metadata with `KeyError: 'kind'`; no API raw payload was read. The original v1 source and its UNKNOWN receiving result remain unchanged.

Actual inventory control metadata has exactly `receipt`, `proof`, and `sha256`. The suite owner separately has `kind: namespace`. The full receiver already treats the inventory role as contained implicitly.

The sole v2 change replaces the inventory record's missing `kind` lookup with an exact three-key shape check. All existing contained proof schema, image, arguments, native settlement, pipe and reader checks remain unchanged, as do the API-only path boundary and FAIL handling.

Use `receive_closed_api59f_v2.py` with the same receipt SHA and a new output filename. Source: 32,505 bytes / 479 lines, SHA256 `9ffd6631bfd2d6cb274634906ad754c3957a8f9e770e593b0c7395a4bd6bdf9f`. Patch: 745 bytes, SHA256 `86bc84d8078894a78bb260b8d0c435d57f91f623cc029d70360ccda39632f395`.

A static DATA checker proved that the bytes contain exactly that one replacement and parsed their Python syntax. The author has not executed either receiver. Root execution and actual scoped results are separate evidence.

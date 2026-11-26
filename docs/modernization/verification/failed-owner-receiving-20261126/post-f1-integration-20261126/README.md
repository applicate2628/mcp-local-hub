# Post-F1 integration and native CBuild search

## Selected API tests on composed Test Source

PR 665 at bec9a4b61aed39b99073cb22fce6ffbcf1be8a95 contributed only its three test-file changes. A direct upstream patch did not apply without context reconciliation because the composed Test Source already had independent port-probe and helper changes. The resulting patch preserves them and changes no production file.

Starting from b05d0e838af9da7ede6fbf537c84c4a4a5697037, the exact received commit is f49e7ea000db8a8a186da737eac5aa6aed326416 (tree bf2c64429082f04de2197b1e5845dd70d13b4c2b). Decode [the forward/reverse-checked patch](api-receiving.patch.base64) to recover its bytes. SHA-256: 9f46b601c9c88e683cf059887a75937e7ed6deb140635bdb02b824ed337ff0e9.

The first selected API run had six passing roots and two fixture failures: the Windows-mounted D: path exposed mode 0777 to Linux, and the vault correctly refused the key before the intended assertions. A TMPDIR-only correction was insufficient: Go also selected GOTMPDIR for its test process. With both temporary paths in a private 0700 native tmpfs workspace, all eight selected API roots (30 named tests and subtests) passed together in one default run on WSL2 Ubuntu, Go 1.26.4 linux/amd64. No skip or failure; exit 0. Canonical-helper dispatch, canonical path behavior, and required-secret admission assertions remained unchanged. SHA-256 of the final JSONL: 69e4e7d51a81d3323b1547f663aa32c8601cf9bb672df901ffc3aa269bb8c08e. This is not full API qualification or Windows acceptance.

## Native Windows CBuild package search

The verified Windows x64 Product from b3014dbe, SHA-256 fef99a8289eb382aa16e2d1c2c7f145e429f9335ed8fb6f1ccbedc7e020bb0cf, served its explicit CBuild stdio command in an empty, owned project directory. The existing local vcpkg catalog returned 50 matches for zlib, including exact zlib, and an empty successful result for the absent package. Initialize, tools/list and both calls returned four MCP responses; the original process exited 0 and stderr was empty. The project, download, and binary cache directories remained empty. SHA-256 of the response JSONL: ae0dc5f61fc7d896e0d08cf450018b3097fdf8a1ebdc98bdd4e99d1f4e1a7de3.

This search was through a direct Product child, not the npm launcher or the working Hub. It does not qualify package installation, the live Hub route, isolated Windows profile admission, migration, or release.

## Tagged Linux follow-up and Windows compile observation

The same f49e7ea0 Test Source and eight selected API roots passed with test_state_path_env on WSL2 Ubuntu, Go 1.26.4 linux/amd64. There were 30 named tests and subtests, no failures or skips, empty stderr and a confirmed original exit 0. Both temporary paths were in a private native Linux tmpfs workspace; the test tag and the regular vault checks stayed enabled. SHA-256 of the tagged JSONL: 304189229ccf25d4b4372b8bd378839758d0e8241360e929cc3a6006c7375162.

On Windows Go 1.27.4 the comparison remained in build/vet after the initial collector's 470-second wait. The original Go process and scoped compiler/vet children were verified after the collector stopped. Its event and stderr files were empty at that observation. This is not evidence that tests started, failed, or passed, and is not a replacement for the distinct isolated Windows profile required by Product acceptance. No duplicate Windows command or test involving the working Hub was started.

# Accepted-master synchronization

Prior FFI head: a7a4c2d19c5a572ae771ab9b9a1511ae2cf9a718.
Accepted master: 17bc78fc602efa204a74596409c939a3bf1ddc42.
Native parent synchronized non-rewriting commit: 87bf2fe6d612de296d4f30a8a5087adb8ee5cdbc c5672f49f65713c7555e8841e46de75b045da5f8 06d8706a446b456642a13f23cfa58e10c6e3c28f 2c13114eabc7ad6cab416788b6e32faaa8c99935 c028db723295f02909969b57f9d1ee8837a22344.
FFI merge commit: bb8e31648581f603c1a35e65b02eb9c02f1345c2.

All native/FFI source bytes in experiments/rust-process-owner are unchanged by
the synchronization. Parent and child histories are preserved, not cherry-picked
or force-pushed; the child follows its actual parent. Root product changes are
those already accepted in master, not a second copy of the active Serena work.

All22 recorded steps passed expected exits:49 top-level Go tests,63 nested cases,
0 skips; debug/release DLL race x3, cgocheck2, vet;14 Rust ABI tests and53 native
parent checks in debug/release, Clippy/fmt. See steps.json and source-hashes.json.
These are scoped experiment gates, not whole-Hub/installed/native Unix acceptance.
No new behavioral feature or performance benefit is claimed by this merge.

The coordinated branch audit and convergence decisions are in existing PR623,
under docs/modernization/verification/branch-audit-2026-11-14. Active session
PR632-641 and verify/serena-postmerge-20261114 remain separate and untouched.
No merge into master, installation, paid/model/provider call or explicit CI/bot
dispatch was performed. All necessary evidence is committed with the PR.

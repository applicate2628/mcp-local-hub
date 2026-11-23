# GUI settings use the existing application owner

Qualification date: 24 November 2026 (Moscow time). Owner: #663, exact input `67f89401c31736399c84d866828241db8c12d8cb` plus the four changed files below. The general architecture gate remains **FAIL**.

## Contents

1. [Ownership correction](#ownership-correction)
2. [Executed checks](#executed-checks)
3. [Exact source and review](#exact-source-and-review)
4. [Evidence and limits](#evidence-and-limits)

## Ownership correction

`realSettingsAPI.List` and `Set` created a fresh application programming interface (API) owner for every settings call. The graphical user interface (GUI) server already owns an API instance. The adapter now retains that instance and `NewServer` binds it through `realSettingsAPI{api: s.api}`. Construction order, server lifetime and release order are unchanged. The narrow adapter remains because the GUI also requires path/open actions and its own method names. The settings methods continue to read and write their real backing state; this does not introduce a cached settings value.

Only the two now-obsolete `api_construction` baseline entries are removed: fingerprints `94c67958d68ac8aa3eb628d7d51b6351dbb006d023d302b6ba8a39d6454ca4e5` and `98e8c2d7078d9d2879eaafea91ee2edf1e35f1ccf20fdff1382e3cb5e39d2522`. Parsed baseline comparison confirms 2,397 entries become 2,395 with every retained entry and every other top-level value unchanged. No limit, metric allowance or policy is raised.

The shared settings-test factory now places its fake preferences path under `t.TempDir()`. Its former fallback `/tmp/test/gui-preferences.yaml` could cause the open-path scenario to create a directory outside the authorized Windows task root. The test still exercises the same handler and fake open operation; it no longer relies on a shared absolute path.

## Executed checks

Table 1 reports Go event counts, including subtests and package events. All selected tests used `-race -count=1`, private task state, the existing Go 1.27.4 Windows AMD64 toolchain and offline dependencies.

| Table 1 — command scope | Result | Evidence |
|---|---|---|
| Existing settings and owner/lifecycle roots, standard selection | PASS | 44 PASS, 0 FAIL, 0 SKIP; 45.721 s |
| Same roots with `test_state_path_env` | PASS | 44 PASS, 0 FAIL, 0 SKIP; 36.749 s |
| `go build ./...` | PASS | 21.985 s |
| `go vet ./...` | PASS | 27.143 s |
| `go run ./tools/archcheck verify ...` | FAIL, exit 1 | 54.278 s; `expired=0 grown=42 new=387 stale=128 unowned=0 workers=72` |

The architecture report no longer contains either `realSettingsAPI.List` or `realSettingsAPI.Set`. Remaining violations keep the gate red. Its totals and comparison categories describe this exact owner input and must not be subtracted from the separate composed Source report to imply an improvement of that size.

The settings qualification plan SHA-256 is `69bca4a9ee1db330842c0a0d6ff2b160dce1cf06b0400f76d33155f1374b44b2`. The architecture plan SHA-256 is `90f9749311540d31c6130aff0e3517608f77366958b8b59fe6f69a3b4d06eea0`. Both verify the same unchanged source inventory: 3,287 files / 59,826,371 bytes, SHA-256 `bbf49311b7114ca75bcc37f77ecec065a41b0cf3e93deeed6c900db5292e51b9`.

## Exact source and review

| Table 2 — file | SHA-256 |
|---|---|
| `internal/gui/server.go` | `2d385c5e5ff2ab354c61797763ea37b1011b1bab99cfedfef05e62d5a6c28e7a` |
| `internal/gui/settings.go` | `75fb9e4dce147baca8a85818c7f94e81e9db7aa6493692ac19aafd5247e5831d` |
| `internal/gui/settings_test.go` | `5ea03e2f94999577501a416af6250b5755ea6cd25231fdb9d509be749e431c69` |
| `architecture/baseline.yaml` | `190fdd72cf1e103aacaecb49f8388657e57b17fa166876f23d3754e4cf43206d` |

Own review traced the actual adapter constructor and API settings methods, checked the sole production `realSettingsAPI` binding, unchanged construction/release order, settings disk-read behavior, test isolation and the exact parsed baseline delta. No additional actionable defect was found in this small slice. No new dependency, alternate owner or forwarding layer was introduced.

## Evidence and limits

[Evidence archive](settings-qualification-20261124-57604a3a.zip): 980,337 bytes, SHA-256 `f5090531bb172ed2d8626c2fed5ea9adc1bae7c1081236d060011add8dd63212`. [The manifest](evidence-manifest.json) maps original and published bytes/hashes. The archive contains the successful settings phase and failed architecture phase, including the actual architecture report, command streams, exact files, frozen plans, preparation and runner. Local path prefixes are redacted in the published view; original logs and a separate raw archive remain outside trash. Recorded input hashes refer to originals, not to redacted script text.

Full standard/tagged test suites, installed lifecycle, platform execution and exact-new-commit external review remain open. The separately issued Source `b426508361858ff2d63a5b994633d4cc367f55bf` does not contain this later settings change. No merge, manual continuous integration dispatch or installed-state mutation occurred.

# Backlog: Zed demigrate refuses fetch/drmemory — live entry ≠ manifest shape

Date filed: 2026-06-16 (user: "в бэклог")
Status: fixed

## Symptom
`mcphub` demigrate (Apply changes, incl retry 2) fails 2 rows for Zed:
- fetch/demigrate/zed + drmemory/demigrate/zed: "no backup contains <entry>
  in pre-hub form; marker confirms mcphub-managed, but live entry no longer
  matches manifest-managed shape — refusing to RemoveEntry (entry may be
  user-modified); edit Zed settings.json manually or re-run migrate".
- Newest backup: %APPDATA%\Zed\settings.json.bak-mcp-local-hub-20260616-171402.

## Root-cause hypothesis (UNVERIFIED)
The Zed live entry for fetch/drmemory drifted from the manifest-managed shape
(URL host migrated localhost->127.0.0.1 this session, OR a user edit), AND no
backup holds the pre-hub form, so the demigrate safety gate (liveEntryMatches
ManifestBinding) refuses RemoveEntry to avoid clobbering a possibly user-modified
entry. Likely interacts with the 127.0.0.1 migration (the live URL is now
127.0.0.1 while the recognition matcher / backup is localhost).

## Next steps
- Diagnose: compare the Zed live fetch/drmemory entries vs the manifest binding
  shape + the newest backup; confirm whether the 127.0.0.1 migration is why the
  shape no longer matches.
- Likely the matcher needs the same all-loopback-form recognition fix applied to
  liveEntryMatchesManifestBinding (commit 9f0f6be) — verify it covers the Zed
  relay/url path too.
- Workaround for now: edit %APPDATA%\Zed\settings.json manually or re-run migrate.

## Resolution (fixed 2026-06-17 — repo-audit review)

Root cause CONFIRMED: relay-stdio clients (Zed) write `mcphub relay --url http://<loopback>:<port>/mcp` for stable-port globals (GetEntry maps --url -> RelayURL, URL=""), but liveEntryMatchesManifestBinding (managed_entries.go) had only a `url` exact-match branch + an Antigravity `--server/--daemon` branch — NEITHER covered the relay-URL form, so demigrate refused fetch/drmemory. Fix: added a relay-URL recognition branch (RelayURL matches an expected loopback endpoint + IsMcphubBinary(RelayExePath)). Both localhost (fetch:9133) and 127.0.0.1 (drmemory:9135) now match. Test: TestLiveEntryMatchesManifestBinding_RecognizesRelayURLForm.

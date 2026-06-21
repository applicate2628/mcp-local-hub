# Groups/namespaces v1 — deferred follow-ups

Source: PR #372 review-loop (Codex bot R3 + sonnet + opus-arch + consultant lanes, 2026-06-19).
These were triaged as fast-follow (NOT v1-merge blockers); the in-PR batch (commit `dcfa26f`) fixed the bot findings + cheap high-value items. Tracked here so they are not left implicit (consultant ask).

## Items

### C3 — `_meta.mcphub.emptyReason` diagnostic (highest-value)
Empty group, all-members-unresolved, and all-tools-hidden ALL render as a silent `tools/list` → `tools=[]` (`internal/api/hub_mcp_aggregator.go:566`, success envelope at `:1468` only carries `partialFailures` + `instance_id`). An operator pointing a client at `/g/<group>/mcp` and seeing zero tools cannot tell which of the three it is → turns a 30-second fix into a multi-hour daemon chase. Add a structured `_meta.mcphub.emptyReason` (or a structured event) on the empty path naming the cause.

### C4 — structured warn on silent member-skip
`BuildResolverSnapshotFromManifestsAndGroups` hits a bare `continue` for an unresolved member server (`internal/api/hub_mcp_resolver.go:~278`) with no event. A direct `groups.yaml` edit naming a since-removed server yields a known-but-empty group with zero breadcrumb. Emit a structured warn with group name + skipped-server count. (Pairs with C3 — the "groups fail quietly" theme.)

### C5-case — group-name case-sensitivity decision — RESOLVED (case-fold uniqueness)
Decision: case-fold group-name uniqueness — `Frontend` and `frontend` collide and the second is rejected, killing the operator-confusion footgun. Stored CASING is preserved (display + routing keep the operator's exact name); only the uniqueness COMPARISON case-folds, so the `/g/<group>/mcp` route, the `g:<name>` scope key, and the token lookup stay case-sensitive on the stored name. Single owner: new `checkGroupNamesUnique` in `internal/api/hub_mcp_groups.go` keyed on the lowercased name, called from BOTH dedup sites (parse/Load + create/write); the GUI upsert find-to-replace (`internal/gui/groups.go`) is now `strings.EqualFold` so a case-variant POST updates in place rather than 500-ing. (The length cap — 64 chars — was already folded into the in-PR batch.)

### B4-full — reconcile path writing group endpoints into client configs — DESIGNED, awaiting go-ahead
The in-PR batch surfaced the group connection triple (url + token + instance_id) in `/api/groups` + the GUI so an operator can hand-configure a client. The FULL fix mirrors `mcphub install --reconcile-hub-mode` for clients: a path that WRITES group endpoints directly into client config files so a group is plug-and-play (no hand-copy). Bigger; design + a new reconcile surface.

**2026-06-21:** architect design complete (PASS) + operator decisions made — **Model B** (separate `group-client-bindings` store + GUI matrix, NOT a `clients:` list in `groups.yaml`); **full feature, phased, await operator go-ahead** to plan/implement. Phase 1 = single-client wire + the C7 port-reset safety gate + reserved-name guard (~90% value); Phase 2 = full matrix + scan-classify + CLI verb. ~1,200-2,000 LOC. Full design: `work-items/decisions/2026-06-21-groups-client-reconcile-b4.md`.

## Notes
- All four are additive / non-breaking; none blocks v1.
- `tools_hidden` is documented (in-PR) as a UX filter, NOT an access-control boundary — the §D multi-tenant/auth paid tier is the eventual security layer built ON this seam, tracked in the roadmap, not here.

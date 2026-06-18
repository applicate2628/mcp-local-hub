# Groups/namespaces v1 — deferred follow-ups

Source: PR #372 review-loop (Codex bot R3 + sonnet + opus-arch + consultant lanes, 2026-06-19).
These were triaged as fast-follow (NOT v1-merge blockers); the in-PR batch (commit `dcfa26f`) fixed the bot findings + cheap high-value items. Tracked here so they are not left implicit (consultant ask).

## Items

### C3 — `_meta.mcphub.emptyReason` diagnostic (highest-value)
Empty group, all-members-unresolved, and all-tools-hidden ALL render as a silent `tools/list` → `tools=[]` (`internal/api/hub_mcp_aggregator.go:566`, success envelope at `:1468` only carries `partialFailures` + `instance_id`). An operator pointing a client at `/g/<group>/mcp` and seeing zero tools cannot tell which of the three it is → turns a 30-second fix into a multi-hour daemon chase. Add a structured `_meta.mcphub.emptyReason` (or a structured event) on the empty path naming the cause.

### C4 — structured warn on silent member-skip
`BuildResolverSnapshotFromManifestsAndGroups` hits a bare `continue` for an unresolved member server (`internal/api/hub_mcp_resolver.go:~278`) with no event. A direct `groups.yaml` edit naming a since-removed server yields a known-but-empty group with zero breadcrumb. Emit a structured warn with group name + skipped-server count. (Pairs with C3 — the "groups fail quietly" theme.)

### C5-case — group-name case-sensitivity decision
Duplicate detection is exact-name only (`hub_mcp_groups.go:~190`), so `Frontend` and `frontend` are distinct groups with distinct scope keys + token rows — an operator-confusion footgun. Needs a PRODUCT decision: case-fold group names (reject case-collisions) or keep them distinct + document. (The length cap — 64 chars — was already folded into the in-PR batch; only the case decision is deferred.)

### B4-full — reconcile path writing group endpoints into client configs
The in-PR batch surfaced the group connection triple (url + token + instance_id) in `/api/groups` + the GUI so an operator can hand-configure a client. The FULL fix mirrors `mcphub install --reconcile-hub-mode` for clients: a path that WRITES group endpoints directly into client config files so a group is plug-and-play (no hand-copy). Bigger; design + a new reconcile surface.

## Notes
- All four are additive / non-breaking; none blocks v1.
- `tools_hidden` is documented (in-PR) as a UX filter, NOT an access-control boundary — the §D multi-tenant/auth paid tier is the eventual security layer built ON this seam, tracked in the roadmap, not here.

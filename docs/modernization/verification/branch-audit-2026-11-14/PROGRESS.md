# Branch audit and convergence, 2026-11-14

Ruling: user requests all remote branches/open PRs, duplication review and safe
convergence while another session is working. Protect PR632-641 and the
verify/serena-postmerge-20261114 integration branch; inspect only. This session
owns PR623-631. No deletion, force push, active-main edits, installed changes,
new models or CI/bot dispatch. All required evidence must be in existing PRs.

Use exact git ancestry/tree/content evidence, never branch names or matching
subjects alone. Retain historical closed branches without automatic deletion.
Separate truly redundant changes from deliberately stacked dependency diffs.
A harmless merge prediction is not test/release readiness. Record live-head drift
and recheck before any write. First gh read command combined unsupported --slurp
and --jq; it failed before snapshots. Corrected with paginated JSON lines; no
source or remote mutation resulted from that command error.
Full history recovered before ancestry decisions. Initial analyzer stopped on a
new active-session SHA not yet fetched; a read-only fetch supplied that exact
object. Snapshot then completed:93 refs,19 open PRs, no same-head/same-change
pairs, no per-PR changed-file intersections, only8 ancestor relations from
three deliberate stacks. Own623-631 each lag accepted master by53 commits.

Relay convergence has exactly one conflict in cmd/mcphub/main.go. Resolution
starts from exact master blob and adds bootstrap's explicit stdin grant to the
canonical-entry-bound root; neither authority is replaced. Initial shell text
capture damaged Unicode comment bytes; corrected using raw git output decoded
as UTF-8, with exact round-trip assertion. No semantic code was changed for that
encoding correction and no damaged version was committed.
Publication whitespace check rejected a trailing space in the fetch log header.
Whitespace-only normalization applied; original SHA256=a9a3fe87e66767dc126f6910e9c8cdf04e874211dd8ac5f96c3852700be48581.
The fetched history was independently checked non-shallow; the log's imperfect
header is not substituted for ancestry/command-success checks.

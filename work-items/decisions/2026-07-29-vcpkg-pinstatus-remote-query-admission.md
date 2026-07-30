# Vcpkg pin-status remote-query admission

Status: proposed
Date: 2026-07-29
Owner: Architect
Relates-to: `2026-07-25-vcpkg-mcp-tool-contracts.md`
Required acceptance: independent architecture gate, then explicit Lead acceptance

## Decision requested

Define the single admission boundary for a remote URL before `vcpkg_pin_status`
passes that URL to the Git child process. This record is deliberately narrow: it
does not change remote selection, Git invocation, ancestry classification, or
public redaction.

No implementation is authorized while this record remains `proposed`.

## Proposed decision

The URL classifier owns three mutually exclusive outcomes, in this precedence
order:

| Outcome | Input evidence | Admission | Stable public reason |
|---|---|---|---|
| Positive credential | Parsed user information is present, crude user-information syntax is present in an otherwise unparsable URL, or a query segment has a non-empty value and its raw key positively matches the existing credential-key predicate | Reject before any child-process call | `remote_url_credential_bearing` |
| Unclassified non-empty query | No positive credential evidence exists, but at least one query segment has a non-empty value | Reject before any child-process call | `remote_url_query_unclassified` |
| No value-bearing query | There is no query, every segment has no `=`, or every value after `=` is empty | Admit, subject to all other existing validation | none |

The positive credential outcome wins for mixed queries. Percent-encoded,
malformed, or otherwise unfamiliar keys with non-empty values are not promoted
to “credential”; they remain unclassified and are still rejected. Parsed and
fallback parsing paths apply the same query-value and key predicates.

The existing positive credential-key vocabulary remains owned by
`internal/vcpkgmcp/pinstatus/redact.go`; this decision does not broaden or rename
that vocabulary.

## Approved-URL type boundary

Successful classification produces a package-private `approvedRemoteURL`
value. Raw strings cannot cross the remote-reference execution seam:

- the classifier is the only constructor;
- `remoteRefsFn` and its test doubles accept `approvedRemoteURL`, not `string`;
- `defaultRemoteRefs` unwraps the value only at the final `exec.CommandContext`
  argument construction boundary;
- no parse failure or retry path bypasses the constructor.

The type is an admission proof, not a sanitizer and not a public contract.

## Redaction independence

Public emission redaction remains unconditional and independent of admission.
Classification must never infer safety from “redaction changed nothing”, and a
rejected URL must still be redacted before any diagnostic or result field can
observe it. Admission reads the raw candidate only inside the classifier;
emission continues to use the existing redaction owner.

## Compatibility and rollback

`remote_url_credential_bearing` retains its existing spelling and meaning:
positive credential evidence only. `remote_url_query_unclassified` is one
additive closed-enum value with status `unknown`; it must not be reused for
positive credentials or empty/no-value queries.

Before publication, rollback is atomic removal of the classifier/type seam,
the additive reason, its tests, and its documentation. After publication, the
new reason spelling remains reserved even if a future accepted allow-list
admits selected value-bearing queries; such a change requires a superseding
decision and must not silently recategorize historical positive credentials.

## Enforcement and falsifying probes

Single owner: the package-private classifier in
`internal/vcpkgmcp/pinstatus/redact.go`, called by
`internal/vcpkgmcp/pinstatus/pinstatus.go` before `remoteRefsFn`.

The decision is falsified if any of these probes fails:

1. Table tests cover parsed and fallback URLs, user information, every positive
   key, unknown non-empty keys, mixed queries, no-`=` segments, empty values,
   percent encoding, and malformed input.
2. Recorder tests prove zero `remoteRefsFn` calls for both rejection outcomes.
3. Child-process probes prove zero secret bytes in argv, error text, result
   fields, and captured output for both rejection outcomes.
4. A compile-time signature probe proves that raw `string` cannot be passed to
   `remoteRefsFn`.
5. Contract tests pin both stable reason spellings and `unknown` tri-state.

## Alternatives rejected

- Allow all non-positive queries: a new credential spelling can reach argv
  before the positive vocabulary learns it.
- Reject every query including empty/no-value segments: this expands behavior
  beyond the reviewed safety boundary without evidence that those forms carry
  a secret value.
- Use redaction as admission: emission safety and execution authority have
  different failure modes and must remain independent.
- Keep `remoteRefsFn(ctx, string)`: it permits future callers to bypass the
  classifier without a type error.

## Acceptance state

This is a `proposed` decision. It requires an independent architecture `PASS`
and explicit Lead acceptance before its status may become `accepted` and before
backend implementation is authorized. Security review remains a later,
separate required gate.

## Terms and Abbreviations

- URL: Uniform Resource Locator.
- argv: operating-system child-process argument vector.
- Git: the version-control executable used to read remote references.


# Brief — CST saved-field point sampler

Primary task: implement and verify the P0 `cst_sample_saved_field` capability from the user-supplied requirements document.

Current stage: Research accepted; architecture is next.

## Scope

- New read-only CST tool over retained solved bundles.
- Exact input/output/provenance/no-solve contract from the requirements document.
- Disposable-copy `Result3D` / CST-generated header / `ResultTree` / `GetFieldVector` flow.
- Deterministic Line10 acceptance and independent native export comparison.
- Focused documentation and tests; safe catalog/publish/deploy only after independent gates.

## Out of scope

- Reimplementation of existing solve, mesh export, or general results export.
- HFSS/CST P1/P2 capabilities.
- VFEM mathematics or fitting/scaling of sampled results.
- Mutation of source retained bundles or termination of pre-existing CST processes.

## Constraints

- Authoritative contract: `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md`.
- No implicit solve, remesh, fallback frame, cached prior-call state, or machine-local output path.
- Existing six tool contracts and manifests are must-not-break surfaces.
- Real solver work requires explicit bounded acceptance ownership and process/hash oracles.
- User-approved authority contract: default-off sampler; operator-managed allowlist of permitted local roots and exact bundle identities is enforced inside the CST server, not trusted from a caller-provided flag.
- User-approved duration contract: one isolated Windows helper/Job Object per call, 60-second hard bound, no foreign process authority, and no release claim until target containment evidence passes.

## Risks and owners

- CST API/result-frame semantics and zero ambiguity: `$computational-scientist` / `$architect`.
- Process and source-bundle safety: `$security-engineer` and `$reliability-engineer` as needed by design.
- Implementation/integration: one `$backend-engineer` owner after accepted design/plan.
- Independent verification: `$qa-engineer`; architecture/security review if the accepted design requires them.

## Acceptance criteria

The mechanical P0 criteria and delivery outputs at requirements lines 44-177 and 205-211 are binding. A tool merely appearing in `tools/list` is not acceptance.

## Open obligations

- Persist and accept the factual research matrix.
- Produce and review a falsifiable design for the sampler and native comparison seam.
- Replace the blocked synchronous candidate with the approved authorization and per-invocation containment design; re-review and replan all invalidated phases.
- Plan, implement, verify, publish, deploy, and live-smoke without solver or bundle collateral damage.

# Bug: CST saved-field deadline and public error channel are not end-to-end bounded

- id: 2026-08-12-cst-saved-field-deadline-and-public-error-unbounded
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: high
- area: helper budget, native streams, and public protocol validation
- found-by: security-engineer
- candidate: c7654794c79ee4d8eed5378117e2ce84f7608040
- fix-class: implementation

## Reproduction

1. `AbsoluteInvocationBudget` is decoded but never validated for finiteness,
   ordering, or equality with the parent-issued budget
   (`cst_saved_field_helper_protocol.py:79-121`).
2. `run_helper` invokes the application even when the received deadline is already
   expired. A current-session smoke probe supplied an expired budget and observed
   the application callback execute (`cst_saved_field_helper.py:387-410`).
3. Hash loops, source enumeration/copy, vendor inventory/activation, and the point
   loop do not consume the request deadline; `_sha256_handle` has no per-block check
   (`cst_saved_field_transfer.py:74-83`) and `activate_and_sample` has no budget input
   (`cst_saved_field_vendor.py:405-499`).
4. Parent `WriteFile` is synchronous and occurs before the timed process wait, with
   no writer thread, cancellation handle, or deadline (`cst_saved_field_containment_windows.py:598-613`).
   The declared five-second launch and 58-second helper-frame cutoffs are unused.
5. Helper failure identifiers and stages are accepted as arbitrary strings and the
   parent publishes the identifier directly (`cst_saved_field_helper_protocol.py:194-216`;
   `cst.py:591-597`). A current-session smoke probe published
   `CANARY_PUBLIC_FAILURE` verbatim.

## Expected versus actual

- Expected: one parent-issued monotonic budget is validated and enforced before
  every source/vendor/allocation/output unit, all three streams are cancellable and
  bounded, and public failures are selected from a fixed allowlist.
- Actual: timeout is mainly a later process wait, a blocked input write can precede
  it, helper work ignores the wire deadline, and a compromised/malformed helper can
  inject arbitrary text into the agent-facing error channel.

## Security impact

The call can exceed its declared wall-clock contract, perform source/vendor work
after expiry, wedge the parent on input, or leak/inject helper-controlled text into
MCP output. This falsifies SEC-C12, SEC-C13, and SEC-C14.

## Required correction and falsifying probe

Validate the exact issued budget, reject expiry before application entry, thread the
budget through inventory/hash/copy/vendor/sample/encoding, and use deadline-aware or
cancellable bounded input/output owners. Replace raw helper failure strings with a
closed enum mapped by the parent. Block each stage past each cutoff and inject
canaries into failure ID/stage/protocol/stderr/environment; prove bounded settlement,
no post-deadline work, and no canary in MCP text or logs.

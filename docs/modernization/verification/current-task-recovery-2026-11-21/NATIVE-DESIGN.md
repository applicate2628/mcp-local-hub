# R21 — Windows restore-if-absent mechanism qualification

> **Исторический проект до выполнения.** Ниже сохранены исходные статическое заключение и запланированная область стенда. Его отметка `native behavior NOT_RUN` относится к моменту подготовки. Фактически выполненная окончательная проверка использовала два синтетических определения, а не неизменённый ExportXML → регистрацию. Актуальные результаты, первоначальный отказ и отдельная очистка приведены в [нативной диагностике](NATIVE-DIAGNOSIS.md) и [проверке фактического архива](EVIDENCE-REVIEW.md).

## Contents

1. [Verdict and scope](#1-verdict-and-scope)
2. [Exact-head implementation evidence](#2-exact-head-implementation-evidence)
3. [Minimum implementable mechanism](#3-minimum-implementable-mechanism)
4. [Fidelity, identity, and execution boundaries](#4-fidelity-identity-and-execution-boundaries)
5. [Errors and uncertain outcomes](#5-errors-and-uncertain-outcomes)
6. [Bounded native qualification plan](#6-bounded-native-qualification-plan)
7. [Cleanup invariant](#7-cleanup-invariant)
8. [Blocking risks and remaining assumptions](#8-blocking-risks-and-remaining-assumptions)
9. [Sources and skills read](#9-sources-and-skills-read)

## 1. Verdict and scope

**The agreed candidate uses a separate XML restoration operation backed by Task Scheduler COM `RegisterTask` with `TASK_CREATE | TASK_IGNORE_REGISTRATION_TRIGGERS` (`0x22`, decimal `34`), never `TASK_UPDATE` or `TASK_CREATE_OR_UPDATE`.** Its contract is “register the supplied definition if the native name is free; otherwise leave the occupant untouched and return a conflict.” The exclusion guarantee comes from the create-only bit `0x2`: the Microsoft protocol specification requires `0x800700B7` (`ERROR_ALREADY_EXISTS`) for an existing path when creation, rather than update, was requested. An earlier absence lookup is not its substitute. [1]

**Design qualified by sources; native behavior NOT_RUN.** No Windows command, product command, remote command, installation, publication, review request, task mutation, task execution, or external message was performed. After the initial research, the root agent authorized an isolated implementation under `r21-recovery/scheduler-candidate/internal/scheduler` and a planned native harness. These local candidate files and this report are the only written deliverables from this agent.

Repository: `applicate2628/mcp-local-hub`, pull request (PR) #660. GitHub read at research time confirmed draft head `c55d9ca619f6f9301ba1113615370fc6218d608f`, base `a7d92d2852c4f6eb8afb0569ef76a545e38aacf7`, unmerged. All implementation conclusions below use the explicit head, not the default branch. Module declares Go `1.26.2`.

The first native qualification scope is an exported, explicitly identified current-user task using `InteractiveToken`. The generic capability does not reject other logon types locally: it offers best-effort XML-only registration, with no credential or security-descriptor recovery promise. Native failure must preserve the durable backup. Silently changing identity or logon type is unacceptable.

## 2. Exact-head implementation evidence

**Table 1 — Relevant source positions at the reviewed head.**

| File and lines | Observed behavior | Consequence |
|---|---|---|
| `scheduler_windows.go:205–228` | `Create` rebuilds XML from `TaskSpec`; invokes `schtasks /Create /TN … /XML …` without `/F`; standard input is nil. | Suitable evidence for its current command construction, not a raw XML restoration API. |
| `scheduler_windows.go:243–251` | `ExportXML` calls COM operation `export` and returns `[]byte(response.Task.XML)`. | Actual output is Go UTF-8 bytes of COM text; the nearby comment still says `schtasks /Query`. |
| `scheduler_windows.go:259–280` | `ImportXML` prepares bytes, writes a temporary file, and invokes `schtasks … /F`. | It explicitly permits replacement. It cannot satisfy the new non-overwrite restoration contract. |
| `scheduler_windows.go:287–302,304–325` | Encoding helper accepts valid UTF-16 little-endian with byte-order mark, or valid UTF-8 with optional byte-order mark; rejects UTF-16 big-endian and invalid surrogates/bytes. | Useful strict-decoding policy to reuse; it is not XML schema validation. |
| `xml_encoding.go:7–26` | Encoder writes UTF-16 little-endian code units, including surrogate pairs; it does not rewrite the XML declaration. | Do not infer that conversion also corrects a contradictory encoding declaration. |
| `scheduler_com_windows.go:18,20–27,49,123–142` | Fixed PowerShell source receives JSON through standard input, with explicit UTF-8 console encoding; request has no XML field today. | Extend the request as data; do not interpolate XML or task names into PowerShell source. |
| `scheduler_com_windows.go:97–101` | Connects to the root folder, enumerates hidden tasks for named operations, then returns `task_absent` before dispatching later operations. | An operation that creates an absent task must branch before this absence guard, preferably before enumeration. |
| `scheduler_com_windows.go:99` | Name matching uses case-sensitive `-ceq` after trimming leading backslashes. | Such a lookup must not define whether native registration may replace a task. Let the native name namespace decide conflicts. |
| `scheduler_com_windows.go:121,145–171` | Catch reports top-level exception `HResult`; dispatcher maps known kinds but has no occupied-name kind; native code is not used to distinguish conflict. | New operation needs deliberate native-error extraction and a typed conflict result. |
| `scheduler_com_windows.go:16,128–140,149–153` | Fifteen-second context deadline; timeout, child error, and invalid JSON become `ErrUnavailable`. | An unavailable response after a mutation is not proof that no mutation occurred. |
| `scheduler.go:78–82,112–145` | No conflict sentinel; `Scheduler` offers `Create`, `ExportXML`, and replacing `ImportXML`, but no restoration-without-replacement contract. | Add an explicit narrow capability/contract; do not silently change existing replacement callers. |

Exact source links are [12]–[16]. Line numbers identify the inspected text; no implementation change is included in this report.

The command-line documentation describes `/F` as the force/suppress-warning switch and `/XML` as the definition input. It does not establish that an entire no-`/F` interactive flow is an atomic conditional write. Removing `/F` from a new raw-XML invocation is a smaller textual change, but does not by itself supply the required backend proof. [2] The separate COM operation avoids depending on that undocumented internal command-line sequence.

## 3. Minimum implementable mechanism

Agreed capability: optional `XMLCreator` interface with `CreateXMLIfAbsent(name string, rawXML []byte) error`. The optional interface avoids forcing unrelated platform implementations and mocks to acquire a feature they do not support. If it is unavailable, callers must fail closed; falling back to `ImportXML` would remove the guarantee.

Implement within the existing Windows bridge:

1. Validate a literal, nonempty task name within the supported root-folder scope. Reject ambiguous/path-expanding input; do not convert an empty value into an automatically generated task name.
2. Reuse strict preparation of `rawXML` to UTF-16 little-endian with a byte-order mark, preserving the exported definition. Reject empty or malformed encoded input before sending the mutation request. Do not rebuild a `TaskSpec` or serialize through a smaller XML structure that can discard fields.
3. Add `XMLUTF16LE []byte` to the JSON request; Go serializes the bytes as base64. The fixed script receives data through standard input and strictly decodes it with a throwing Unicode decoder.
4. Dispatch `restore_if_absent` immediately after connecting/getting the root folder and before `GetTasks`/`task_absent`. Make one registration call; any preceding read is diagnostic only.
5. Invocation: `RegisterTask(exactName, xmlText, 34, null, null, 0, null)`. Here `34` means create only plus suppression of registration triggers; `0` is `TASK_LOGON_NONE`, requesting the logon type from the XML; null user/password provide no credential override. The protocol also establishes precedence for an XML principal when no explicit credential principal is supplied. [1]
6. Require a non-null registered-task result and report creation separately from transport failure. Qualify PowerShell's actual null-to-COM `VARIANT` conversion and returned object on the host before treating this invocation as proven. The COM signature takes Unicode `BSTR` text and variant credential arguments. [3][10]
7. Map confirmed native occupied-name error to a typed conflict, preserving the existing occupant. Never add an update flag, call replacing import, delete the occupant, or run the task to recover from that conflict.

Keep existing `ImportXML` replacement semantics for callers that deliberately need them. This report recommends no broad scheduler refactor, new dependency, direct task-store file manipulation, credential prompt, or access-control change.

## 4. Fidelity, identity, and execution boundaries

### 4.1 Preserve the definition, define the claim precisely

`IRegisteredTask.Xml` exposes XML registration information. [4] Sending the full exported text retains fields that `TaskSpec` does not model: hidden/disabled settings, triggers, principal identity, logon type, action context, exact arguments, working directory, and other supported XML fields. This is a preservation strategy, not proof of byte-identical re-export: the service may normalize the registered representation. Use native re-export to compare the complete definition and explicitly report any allowed normalization.

For the same-name production path, require the expected task path and the exported registration URI to agree under the application's chosen name rules. A qualification that restores into a second fresh name must record the deliberate URI/path difference rather than silently ignore it.

The Task Scheduler schema has fields for `Hidden`, `Enabled`, principal settings, and an optional registration security descriptor. It has no password value in the principal definition. [5] Therefore “preserves XML” does not imply “recovers all original credentials or every piece of scheduler state.”

### 4.2 Principal versus task-object owner

Current bridge `Owner` means `Definition.Principal.UserId`, the account used to run the task. It is not the owner of the task's security descriptor, and `RegistrationInfo.Author` is not ownership proof.

Microsoft documents that credential parameters can override the XML principal. A current-user interactive task normally needs no supplied password; other security contexts can require credentials or privileges. [6] Do not hard-code the current username or logon type `3` into a generic restoration call: that would alter a different exported principal/type.

Exact security-descriptor preservation is a separate feature. `GetSecurityDescriptor` obtains the task object's security information; current `ExportXML` never invokes it. [7] An optional XML `SecurityDescriptor` field should be retained if present, but this implementation has no independent snapshot proving equality of the original object's owner and discretionary access-control list (DACL). A requirement to restore those exactly is blocked until separately captured, authorized, reapplied, and verified. Do not silently widen the R21 change to solve that different contract.

For `Password` or `InteractiveTokenOrPassword`, registration requires a supplied password; raw export is insufficient. [3] The generic method deliberately leaves this to native registration, returns failure without substitution or credential prompts, and makes no guarantee of success for such an export. Group, service-account, other-user, and privilege-sensitive cases remain unqualified by the limited fixture. Current ownership checks do not establish recoverability: a caller must preserve durable backup and explicit failed/uncertain state, and must not present XML-only backup as guaranteed restoration of every task.

### 4.3 Registration can execute an enabled task

Microsoft's registration-trigger example explicitly starts an action on registration. [8] Plain creation is therefore not a general “write metadata only” operation.

`TASK_IGNORE_REGISTRATION_TRIGGERS` (`0x20`) is combined with create in the candidate, producing `0x22`, to suppress registration-trigger activation. It does not suppress all time-based activation. `TASK_DISABLE` would change enabled behavior and is not added. [9] The candidate therefore does not promise no execution for an arbitrary enabled definition.

The bounded native plan below uses XML with global `Enabled=false`, no runnable payload, and no `Run`, `RunEx`, `/Run`, enable, or stop call. It qualifies registration and conflict behavior only. It cannot establish operational readiness or enabled-task trigger behavior.

### 4.4 Encoding and transport

COM `BSTR` is a length-bearing wide-character string, not an XML file whose bytes are decoded by `schtasks`. [10] A Go `[]byte` export containing UTF-8 text can still carry an XML declaration that says UTF-16, because it originated as COM text. Decode bytes under an explicit accepted-input policy, then send the original Unicode text; do not treat `string(utf16Bytes)` as decoding.

Qualify Cyrillic, a supplementary-plane character, quotation marks, ampersands, and backslashes end to end. Preserve the existing fixed-source/JSON-data separation. Invalid UTF-8 must be rejected before conversion to a JSON string; Go's `encoding/json` can substitute replacement characters. [18] A raw-byte/base64 transport is an alternative. The implementation subsequently requested by the root agent uses prepared UTF-16 little-endian bytes encoded as base64 JSON, with strict decoding in the fixed bridge script.

## 5. Errors and uncertain outcomes

**Table 2 — Proposed outcome handling.**

| Observed outcome | Safe classification | Required caller behavior |
|---|---|---|
| Successful native registration and valid bridge reply | Created | Continue only with separately authorized recovery steps; creation is not readiness. |
| Confirmed native `0x800700B7` | Occupied/conflict | Preserve occupant; return a typed conflict; no delete/import/run fallback. |
| Invalid byte encoding before dispatch | Input rejected; no dispatch | Return error; do not contact scheduler. |
| Native XML/credential/access rejection | Rejected, with native code and phase | Report exact failure; do not mislabel it absent or conflict. |
| Timeout, killed bridge, malformed/lost JSON, or failed verification after registration | Outcome uncertain | Read-only reconciliation is permitted; no compensating delete, replacing import, or inferred success. |
| Existing task observed after an uncertain outcome | Occupied, provenance unproven | Its presence alone does not identify which writer created it. Preserve it. |

The COM registration API also lists success-status warnings in which registration occurred despite a possible trigger or batch-logon problem. [3] Do not equate “registered” with “all triggers will run.” PowerShell automation may not expose every positive HRESULT directly; scope qualification accordingly.

PowerShell method-invocation exceptions can wrap another exception; COM exceptions expose a native HRESULT. [11] The current top-level `.Exception.HResult` catch is insufficient evidence that the occupied-name code reaches Go unchanged. Native qualification must capture the bounded exception chain and establish which member is the COM result; production mapping must not depend on localized error text.

A retry restricted to create-only does preserve the no-overwrite property, but a resulting conflict cannot prove that the original timed-out attempt failed. Prefer one mutation followed by read-only reconciliation and an explicit uncertain result. Never erase a task merely because a local success receipt was lost.

## 6. Bounded native qualification plan

**Status: NOT_RUN.** The root agent selected the standalone Go harness at `r21-recovery/native-harness/main.go`; it must be copied beneath `cmd/r21-native-qualification` in the temporary source overlay so Go's internal-package import boundary is satisfied. Its flags do not authorize execution by this agent.

### 6.1 Fixed boundary and durable records

The harness has one literal Windows output directory:

`<RECOVERY_ROOT>\native-r21-e8b3694319384cb5af9cf33f6b1396ef`

It refuses an existing output directory and a present action executable. Before any scheduler mutation it writes and synchronizes `manifest.json`, containing the two names, their explicit lowercase case aliases, current account/SID, input hashes, disabled/hidden/no-trigger policy, nonexistent command, and cleanup assumption. It appends and synchronizes JSON observations before creation, conflict attempts, and deletion. A missing or uncertain receipt never authorizes replacement or cleanup. All records and exported definitions remain outside trash.

**Table 3 — Two proposed fixture names; no prefix operations.**

| Encoding | Exact name |
|---|---|
| UTF-8 text input | `\MCPHub-R21-Native-e8b3694319384cb5af9cf33f6b1396ef-U8` |
| UTF-16 little-endian with byte-order mark | `\MCPHub-R21-Native-e8b3694319384cb5af9cf33f6b1396ef-U16` |

Case-alias attempts use the explicitly recorded lowercase spelling of the corresponding name. Both are globally disabled and hidden, have no triggers, use the current SID with `InteractiveToken` and `LeastPrivilege`, and disallow on-demand start. The action points to a confirmed nonexistent executable beneath the output directory. Arguments include Cyrillic, Han characters, a supplementary-plane character, quotes, and native Windows paths. The harness never calls `Run`, `Stop`, enable, or a fixture payload.

### 6.2 Assertions

**Table 4 — Selected two-name native matrix.**

| Row | Action | Required evidence |
|---|---|---|
| Q0 | Read-only preflight of both exact names and nonexistent action path | Both names observed absent; durable name/hash manifest before mutation; native create-only remains authoritative for occupancy. |
| Q1 | Invalid UTF-8 at the first name, invalid UTF-16 surrogate at the second, each while absent | Typed corrupt-input refusal and the exact name still absent; no third fixture name. |
| Q2 | Create first task from UTF-8 text | Successful registration; native XML retains current SID, interactive logon, hidden/disabled state, no triggers, exact action/arguments/directory; runtime state observed disabled. |
| Q3 | Create second task from UTF-16 little-endian bytes | The same property checks; input and full native export hashes recorded, exported definitions retained. |
| Q4 | For each owned task, attempt distinct XML at the exact same name and its lowercase alias | Typed occupied-name result and byte-identical complete `ExportXML` before/after each attempt. Every attempted spelling is durably tracked before the call. |
| Q5 | Optional `--cleanup-owned`, only after all Q0–Q4 checks pass | Immediate exact-name re-export, complete XML digest equality, current principal/marker/definition check, disabled state and status owner check; then exact-name deletion and observed absence. Any mismatch or uncertainty retains the task and reports it. |

Two successful fixture creations are expected. Unexpected registration of a case alias is a failure, is logged against a predeclared alias, and never enters automatic cleanup as an owned fixture. There is no retry loop, real password test, access-control mutation, payload execution, or registration/time-trigger activation test. Fake-COM tests cover transport and exception branches separately; they do not replace Q0–Q5.

The native plan does not independently prove all concurrency schedules. The selected service contract supplies create-only exclusion; the fixture proves the candidate reaches it and preserves the observed occupant. Cleanup has the separate assumption stated in §7.

## 7. Cleanup invariant

**Do not use unconditional deletion in `defer`/`finally`, prefix cleanup, or deleting every name that appeared in the manifest.** The native `DeleteTask` API accepts a name and unsupported flags; it has no expected-definition or object-version argument. [17]

A check of name, SID, marker, XML hash, and disabled state followed by `DeleteTask(name)` has a time-of-check/time-of-use race: an external writer can replace the task after the check. Neither a UUID-like name nor the application's cooperative lease excludes that external writer. This is an architectural inference from the API shape, not a claim that such a race was observed.

The strict production guarantee against arbitrary external writers therefore cannot be supplied by this read/delete sequence. For the controlled native qualification only, the root agent authorized optional `--cleanup-owned`: two confirmed creations, all checks passed, fresh full-XML digest and ownership/marker/disabled-state verification, exact-name deletion, then observed absence. This qualification explicitly assumes no concurrent external writes to the isolated random fixture names. It does not prove atomic compare-and-delete, and this cleanup policy is not a production rollback primitive.

An initial creation conflict or uncertain result never enters cleanup. Expected conflict-test refusals retain eligibility only when the original creation was confirmed and the complete XML remained unchanged. No cleanup runs after a failed qualification. Any mismatched or unconfirmed task is retained with the durable manifest and reported; that outcome must not be described as cleaned up.

For local files under the manifest directory, move only confirmed unused and unnecessary fixture material into its `trash` subdirectory. Keep the report, raw observations, exact-name manifest, unresolved recovery material, and only needed copies outside trash. Do not empty trash automatically. A scheduler task is not a file that can safely be moved to trash through these APIs.

## 8. Blocking risks and remaining assumptions

**Table 5 — Concrete risks versus qualification assumptions.**

| Status | Location/condition | Consequence and minimum treatment |
|---|---|---|
| Confirmed incompatible implementation | `scheduler_windows.go:274`, restoration calls `ImportXML` after another actor occupies the name | `/F` permits replacing the new occupant. Add the separate create-only operation; do not modify unrelated replacement flows. |
| Confirmed dispatch incompatibility | `scheduler_com_windows.go:101`, new create operation added below existing absence guard | Absent task returns before registration. Branch before that guard. |
| Required native qualification | `scheduler_com_windows.go:121`, wrapped COM error | Top-level exception can conceal the occupied-name HRESULT. Qualify and extract native result deliberately. |
| Confirmed uncertainty boundary | `scheduler_com_windows.go:132–140`, reply lost after service acceptance | Error can coexist with a newly registered task. Return uncertainty; no compensating delete/replace. |
| Scope limitation | XML-only snapshot, original task requires a password or exact original object security descriptor | Snapshot is insufficient for that broader restoration promise. Restrict supported contract or design explicit additional state separately. |
| Execution-policy decision | Arbitrary enabled exported definition with due/registration triggers | Registration itself can activate work. Decide suppression policy; disabled native fixtures do not prove enabled-task safety. |
| Cleanup blocker under external writers | Any read-then-`DeleteTask(name)` cleanup | External replacement can be deleted. Retain uncertain state or establish real exclusion before deleting. |
| Assumption to qualify | PowerShell null arguments, `TASK_LOGON_NONE`, current-user interactive exported XML | Must preserve principal/logon type in actual host re-export. No runtime evidence obtained in this task. |
| Assumption to qualify | XML Unicode transport, hidden conflict, native name aliases | Must pass Q2–Q8 on the real target implementation; source inspection alone is insufficient. |

No broader claim is made about another Windows release, elevated or service identities, arbitrary task folders, enabled triggers, successful task execution, readiness, or all security-descriptor variants. This report supports a concrete mechanism choice and a bounded qualification design.

## 9. Sources and skills read

Primary documentation was retrieved from Microsoft Learn and Go package documentation. Web reference identifiers are included so the root agent can independently reopen the sources before citing them. No secondary technical source was used.

**Table 6 — Numbered sources.**

| No. | Source / exact URL | Retrieved reference |
|---|---|---|
| 1 | [MS-TSCH, SchRpcRegisterTask](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-tsch/849c131a-64e4-46ef-b015-9d4c599c5167), especially native flag semantics, principal/logon precedence, and occupied path result | первичный источник прочитан |
| 2 | [schtasks create](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/schtasks-create) | первичный источник прочитан |
| 3 | [ITaskFolder::RegisterTask](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-itaskfolder-registertask) | первичный источник прочитан |
| 4 | [IRegisteredTask::get_Xml](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-iregisteredtask-get_xml) | первичный источник прочитан |
| 5 | [Task Scheduler Schema](https://learn.microsoft.com/en-us/windows/win32/taskschd/task-scheduler-schema) | первичный источник прочитан |
| 6 | [Security Contexts for Tasks](https://learn.microsoft.com/en-us/windows/win32/taskschd/security-contexts-for-running-tasks) | первичный источник прочитан |
| 7 | [IRegisteredTask::GetSecurityDescriptor](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-iregisteredtask-getsecuritydescriptor) | первичный источник прочитан |
| 8 | [Registration Trigger Example, XML](https://learn.microsoft.com/en-us/windows/win32/taskschd/registration-trigger-example--xml-) | первичный источник прочитан |
| 9 | [TASK_CREATION enumeration](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/ne-taskschd-task_creation) | первичный источник прочитан |
| 10 | [MS-DTYP, BSTR](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/692a42a9-06ce-4394-b9bc-5d2a50440168); [Windows Unicode strings](https://learn.microsoft.com/en-us/windows/desktop/learnwin32/working-with-strings) | первичный источник прочитан |
| 11 | [MethodInvocationException](https://learn.microsoft.com/en-us/dotnet/api/system.management.automation.methodinvocationexception); [COMException](https://learn.microsoft.com/en-us/dotnet/api/system.runtime.interopservices.comexception?view=net-10.0) | первичный источник прочитан |
| 12 | [scheduler_windows.go at reviewed head](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/scheduler/scheduler_windows.go) | GitHub connector; blob `7d19a7cbd928e61a82b1bc58d0d5bdf3b6a5070c` |
| 13 | [scheduler_com_windows.go at reviewed head](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/scheduler/scheduler_com_windows.go) | GitHub connector; blob `632cfddcab608646eb9e3af0a6c4702c1a924c8d` |
| 14 | [scheduler.go at reviewed head](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/scheduler/scheduler.go) | GitHub connector; blob `198e6b7cb3cef3b234a346b1c8b92f9001dac2d4` |
| 15 | [xml_encoding.go at reviewed head](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/internal/scheduler/xml_encoding.go) | GitHub connector; blob `9b68455ae4d16648ac76602fa1e94e136931ee82` |
| 16 | [go.mod at reviewed head](https://github.com/applicate2628/mcp-local-hub/blob/c55d9ca619f6f9301ba1113615370fc6218d608f/go.mod); [PR #660 metadata](https://api.github.com/repos/applicate2628/mcp-local-hub/pulls/660) | GitHub connector; module blob `b9b488ddf9ed0f2ac78e8bdcab90f77a57f2b7d3` |
| 17 | [ITaskFolder::DeleteTask](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-itaskfolder-deletetask) | первичный источник прочитан |
| 18 | [Go encoding/json](https://pkg.go.dev/encoding/json#Marshal) | первичный источник прочитан |
| 19 | [Task Scheduler error and success constants](https://learn.microsoft.com/en-us/windows/win32/taskschd/task-scheduler-error-and-success-constants) | первичный источник прочитан |

Skills read in full and applied: `agent-skills:source-driven-development` (`c38/source-driven-development`), and `software-architect:architecture-review` (`e40/skills/architecture-review`). Their use was limited to exact-source verification, explicit trust/identity/failure boundaries, and targeted qualification; no installation or permission expansion followed from them.

Implementation-stage skills also read: `go-project-and-api-design` and its `references/api-evolution.md`, plus `go-testing-and-verification`. The candidate preserves the existing broad `Scheduler` interface and replacing `ImportXML` behavior, introduces one optional capability, and adds focused transport/error tests. Tests that run PowerShell substitute the complete COM connection statement before launch, so they do not touch Task Scheduler. Those tests have not been run by this agent. Local `/usr/bin/go` is not the Go compiler (`go version` reports an unknown option), and `gofmt` is unavailable; the root agent owns compilation and Windows qualification.

Terminology: COM — Component Object Model; XML — Extensible Markup Language; JSON — JavaScript Object Notation; SID — Windows security identifier; HRESULT — native Windows result code; DACL — discretionary access-control list; SDDL — Security Descriptor Definition Language; UTF-8/UTF-16 — Unicode Transformation Formats; byte-order mark — leading encoding marker; URI — Uniform Resource Identifier; NOT_RUN — not executed.

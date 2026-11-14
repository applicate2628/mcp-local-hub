#ifndef MCPHUB_IDENTITY_V1_H
#define MCPHUB_IDENTITY_V1_H
#include "owner_v1.h"

/* Additive Windows x64 identity schema. Existing ABI1 exports/layouts and
 * mh_version1() remain unchanged. Absence of mh_admit_identity1 is unsupported,
 * never permission to fall back to PID/image-only admission.
 * All pointer validity, exclusive owner, loaded-DLL and callback obligations
 * from owner_v1.h also apply; this is not a hostile-memory sandbox. */
enum { MH_ID_VERSION=1, MH_ID_SID_CAPACITY=68 };
enum { MH_ID_STAGE_NONE=0, MH_ID_STAGE_PID=1, MH_ID_STAGE_IMAGE=2,
       MH_ID_STAGE_CREATION=3, MH_ID_STAGE_JOB=4, MH_ID_STAGE_WAIT=5,
       MH_ID_STAGE_TOKEN=6, MH_ID_STAGE_COMPLETE=7 };
enum { MH_ID_ERROR_NONE=0, MH_ID_ERROR_NATIVE=1, MH_ID_ERROR_INVALID_DATA=2,
       MH_ID_ERROR_NO_MEMORY=3, MH_ID_ERROR_OTHER=4 };
enum { MH_ID_JOB_MEMBER=1, MH_ID_JOB_NOT_MEMBER=2 };
enum { MH_ID_WAIT_NOT_SIGNALED=1, MH_ID_WAIT_SIGNALED=2 };

typedef struct {
    uint32_t size, version, pid, job_membership;
    uint64_t creation_time_100ns;
    uint32_t wait_state, sid_len;
    mh_u16 image;
    uint8_t sid[MH_ID_SID_CAPACITY];
    uint32_t reserved;
} mh_identity;
typedef struct {
    uint32_t size, version, stage, query_error_kind;
    int32_t query_native_error, token_close_error;
} mh_identity_report;

/* Successful observations are emitted only inside the original NativeOwner's
 * at-create Job-bound admission, before ResumeThread. Every query uses retained
 * objects; no process is reconstructed by PID. The values are separate
 * observations, not an atomic snapshot, ownership or a product capability.
 * pid/creation_time are nonzero; creation_time uses 100 ns since 1601-01-01 UTC.
 * Job/wait values are the known enums above: zero is unknown and is invalid in
 * a complete observation. NotSignaled includes suspended, not a promise of
 * future execution. image has 1..32768 UTF16 units without NUL. Preserve raw
 * units, including unpaired surrogates. sid_len is 8..68, containing only the
 * validated binary TokenUser SID; unused SID bytes and reserved are zero.
 * Success includes successful close of the temporary token. No raw native
 * handles or formatting/free receipt is exported.
 *
 * The identity pointer, its inline bytes and image view are read-only and live
 * ONLY until this synchronous callback returns. Copy before retaining. The
 * token is an opaque call-scoped integer, never a dereferenced pointer here.
 * Return exactly 1=allow or 0=deny. Other decisions reject and consume admission.
 * No unwind/longjmp/Goexit, native pointer retention, owner reentry or waiting
 * for another thread to enter the same owner is allowed. */
typedef uint32_t (*mh_identity_check_fn)(const mh_identity*, uint32_t, uintptr_t);

/* Query or token-close failure consumes admission and does not invoke policy.
 * Return is MH_STATE; the separate report preserves both failures. No SID is
 * exposed on cleanup failure, even if its query/copy succeeded.
 * NONE means observation collection did not start, not general retry authority.
 * COMPLETE means observations completed, not policy approval/resume success;
 * it remains COMPLETE after policy denial or later native resume/wait failure.
 * Report invariants:
 * - NONE/COMPLETE: all three error fields zero.
 * - PID..TOKEN: a query error is present, or TOKEN has a close-only failure.
 * - NATIVE iff query_native_error is nonzero; other kinds require zero there.
 * - token_close_error is nonzero only at TOKEN; never uses mh_out.cleanup_mask.
 *
 * report/out are valid aligned exclusive DISJOINT writable buffers. A valid
 * report is initialized before owner/admission processing. Exact version/byte
 * sizes and non-null callback are required; rejected preflight never consumes
 * admission. After any actual admission attempt, do not automatically replay
 * through another export. Existing mh_out settlement meanings are unchanged. */
typedef uint32_t (*mh_admit_identity_fn)(mh_owner*, uint32_t,
    mh_identity_check_fn, uintptr_t, mh_identity_report*, uint32_t,
    mh_out*, uint32_t);

#if UINTPTR_MAX == UINT64_MAX
_Static_assert(sizeof(mh_identity)==120, "identity layout");
_Static_assert(offsetof(mh_identity,creation_time_100ns)==16 &&
    offsetof(mh_identity,image)==32 && offsetof(mh_identity,sid)==48 &&
    offsetof(mh_identity,reserved)==116, "identity offsets");
_Static_assert(sizeof(mh_identity_report)==24 &&
    offsetof(mh_identity_report,query_native_error)==16 &&
    offsetof(mh_identity_report,token_close_error)==20, "identity report layout");
#endif
#endif

#ifndef MCPHUB_OWNER_V1_H
#define MCPHUB_OWNER_V1_H
#include <stdint.h>
#include <stddef.h>

/* Experimental Windows x64 C ABI. Not a product API or hostile-memory sandbox.
 * All non-null pointers must be valid/aligned for their stated lengths. Inputs
 * stay immutable through the call and may be released immediately afterwards.
 * Owner calls/release require exclusive access; no stale/copied owner pointer,
 * overlapping buffers or reentrant owner calls. DLL stays loaded until all owners release.
 * Expected errors are values; an unexpected Rust panic/abort terminates host. */
typedef struct mh_owner mh_owner;
typedef struct { const uint16_t *data; size_t len; } mh_u16;
typedef struct {
    uint32_t version, reserved;
    mh_u16 program, cwd;
    const mh_u16 *args;
    size_t argc;
} mh_create;
typedef struct {
    uint32_t size, version, state, flags, pid, root_exit, active;
    int32_t native_error;
    uint32_t cleanup_mask;
    int32_t cleanup_codes[3]; /* thread, process, job; valid iff mask bit set */
} mh_out;
enum { MH_OK=0, MH_ARGUMENT=1, MH_ABI=2, MH_NATIVE=3,
       MH_DENIED=4, MH_STATE=5, MH_NOT_SETTLED=6 };
enum { MH_UNKNOWN=0, MH_PENDING=1, MH_COMPLETE=2, MH_CLEANUP_FAILED=3 };
enum { MH_ROOT_KNOWN=1, MH_ACTIVE_KNOWN=2 };
/* Version 1: explicit absolute program/cwd, <=64 args, <=32766 UTF-16 units
 * per string and <=32766 aggregate units; embedded NUL rejected. Complete
 * EMPTY environment, no redirected stdio in this first calling-side slice.
 * out_owner must point to a NULL slot; failure never overwrites a live owner.
 * The numeric PID is diagnostic only, never a substitute for owner authority. */
typedef uint32_t (*mh_version_fn)(void);
typedef uint32_t (*mh_create_fn)(const mh_create*, uint32_t, mh_owner**, mh_out*, uint32_t);
/* Allow/deny is the caller's decision, not authorization or image validation.
 * A valid deny consumes the same one-shot NativeOwner admission as allow. */
typedef uint32_t (*mh_admit_fn)(mh_owner*, uint32_t, mh_out*, uint32_t);
/* Optional additive export mh_admit_checked1, ABI1 layout unchanged.
 * Called synchronously inside the existing one-shot native admission, BEFORE
 * ResumeThread. pid/image are queried through the ORIGINAL process handle.
 * image is a read-only UTF-16 view valid ONLY until the callback returns.
 * Copy before retention. Return exactly 1=allow or 0=deny; any other value
 * rejects and consumes admission. NULL callback rejects without consuming.
 * The token is an opaque integer, never a pointer the DLL dereferences.
 * Callback MUST return normally: no unwind/longjmp, Goexit, reentry on the
 * same owner, or retained native pointers. No callback is stored after return.
 * PID/image name is evidence, NOT file digest, signature, token or sandbox proof. */
typedef uint32_t (*mh_check_fn)(uint32_t, const uint16_t*, size_t, uintptr_t);
typedef uint32_t (*mh_admit_checked_fn)(mh_owner*, mh_check_fn, uintptr_t, mh_out*, uint32_t);
typedef uint32_t (*mh_stop_fn)(mh_owner*, uint32_t, mh_out*, uint32_t);
/* Timeout 0..60000ms. Pending retains the same pointer. State and native errors
 * are separate: MH_OK does not turn MH_CLEANUP_FAILED into successful cleanup. */
typedef uint32_t (*mh_finish_fn)(mh_owner*, uint32_t, mh_out*, uint32_t);
/* Release only after terminal finish; clears caller slot. NULL slot idempotent.
 * No implied stop/retry/finalizer, no free() of Rust-allocated memory by caller. */
typedef uint32_t (*mh_release_fn)(mh_owner**, mh_out*, uint32_t);
#if UINTPTR_MAX == UINT64_MAX
_Static_assert(sizeof(mh_u16)==16, "UTF16 view layout");
_Static_assert(sizeof(mh_create)==56 && offsetof(mh_create,args)==40, "create layout");
_Static_assert(sizeof(mh_out)==48 && offsetof(mh_out,cleanup_codes)==36, "outcome layout");
#endif
#endif

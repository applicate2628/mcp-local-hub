//! Isolated Windows C ABI over the existing owner. No second process engine.
//! Foreign memory validity/exclusivity is a caller contract, not discoverable by
//! checking a numeric address. extern C never unwinds; panic/abort kills host.
#![cfg(windows)]
#![deny(unsafe_op_in_unsafe_fn)]
use mcphub_process_owner_spike::{LaunchContext, NativeOwner, Settlement};
use std::{
    ffi::OsString,
    io,
    mem::size_of,
    os::windows::ffi::{OsStrExt, OsStringExt},
    path::PathBuf,
    ptr, slice,
    time::Duration,
};

const OK: u32 = 0;
const ARGUMENT: u32 = 1;
const ABI: u32 = 2;
const NATIVE: u32 = 3;
const DENIED: u32 = 4;
const STATE: u32 = 5;
const NOT_SETTLED: u32 = 6;
const ROOT_KNOWN: u32 = 1;
const ACTIVE_KNOWN: u32 = 2;
const PENDING: u32 = 1;
const COMPLETE: u32 = 2;
const CLEANUP_FAILED: u32 = 3;
const MAX_UNITS: usize = 32766;

#[repr(C)]
#[derive(Clone, Copy)]
pub struct U16 {
    data: *const u16,
    len: usize,
}
#[repr(C)]
#[derive(Clone, Copy)]
pub struct EnvironmentEntry {
    name: U16,
    value: U16,
}
#[repr(C)]
pub struct Environment {
    entries: *const EnvironmentEntry,
    count: usize,
}
#[repr(C)]
pub struct Create {
    version: u32,
    reserved: u32,
    program: U16,
    cwd: U16,
    args: *const U16,
    argc: usize,
}
#[repr(C)]
#[derive(Default, Clone, Copy, Debug, PartialEq, Eq)]
pub struct Outcome {
    size: u32,
    version: u32,
    state: u32,
    flags: u32,
    pid: u32,
    root_exit: u32,
    active: u32,
    native_error: i32,
    cleanup_mask: u32,
    cleanup_codes: [i32; 3],
}
// Opaque to C. Caller has exactly one owning slot; never a HANDLE or a PID.
pub struct Owner {
    inner: NativeOwner,
    terminal: bool,
}

#[no_mangle]
pub extern "C" fn mh_version1() -> u32 {
    1
}

// SAFETY for all helpers is inherited from each exported function: buffers are
// valid, initialized, aligned, non-overlapping and exclusive for this call.
unsafe fn output<'a>(raw: *mut Outcome, bytes: u32) -> Result<&'a mut Outcome, u32> {
    if raw.is_null() || !raw.is_aligned() || bytes as usize != size_of::<Outcome>() {
        return Err(ARGUMENT);
    }
    // SAFETY: valid caller output, checked alignment/size, exclusive/no alias.
    unsafe {
        raw.write(Outcome {
            size: size_of::<Outcome>() as u32,
            version: 1,
            ..Outcome::default()
        })
    };
    // SAFETY: all output fields were initialized above before forming a reference.
    Ok(unsafe { &mut *raw })
}
unsafe fn copy_string(value: U16) -> Result<OsString, u32> {
    if value.len > MAX_UNITS {
        return Err(ARGUMENT);
    }
    if value.len == 0 {
        return Ok(OsString::new());
    }
    if value.data.is_null() || !value.data.is_aligned() {
        return Err(ARGUMENT);
    }
    // SAFETY: caller promises a live immutable initialized UTF-16 view for len.
    let units = unsafe { slice::from_raw_parts(value.data, value.len) };
    if units.contains(&0) {
        return Err(ARGUMENT);
    }
    Ok(OsString::from_wide(units))
}
// This boundary's budget is deliberately smaller than the native core's general
// block budget. Measure separators before copying, reject overflow/count/null
// structurally, then let the one native LaunchContext validate names/order.
unsafe fn copy_environment(view: &Environment) -> Result<Vec<(OsString, OsString)>, u32> {
    if view.count > 256 {
        return Err(ARGUMENT);
    }
    if view.count == 0 {
        return Ok(Vec::new());
    }
    if view.entries.is_null() || !view.entries.is_aligned() {
        return Err(ARGUMENT);
    }
    // SAFETY: caller promises this immutable initialized view for count entries.
    let entries = unsafe { slice::from_raw_parts(view.entries, view.count) };
    let units = entries.iter().try_fold(1usize, |size, entry| {
        size.checked_add(entry.name.len)?
            .checked_add(entry.value.len)?
            .checked_add(2)
    });
    if units.is_none_or(|size| size > MAX_UNITS) {
        return Err(ARGUMENT);
    }
    // SAFETY: same copied-input contract as program/argv; no native pointer is
    // retained. NUL/individual bounds are checked by the shared decoder.
    unsafe {
        entries
            .iter()
            .map(|entry| Ok((copy_string(entry.name)?, copy_string(entry.value)?)))
            .collect()
    }
}
fn failure(out: &mut Outcome, e: io::Error) -> u32 {
    if let Some(code) = e.raw_os_error() {
        out.native_error = code;
        return NATIVE;
    }
    match e.kind() {
        io::ErrorKind::PermissionDenied => DENIED,
        _ => STATE,
    }
}
// Create has input-validation failures as well as native failures. Keep this
// phase-specific classification separate from state errors of an existing owner.
fn create_failure(out: &mut Outcome, error: io::Error) -> u32 {
    if error.raw_os_error().is_none() && error.kind() == io::ErrorKind::InvalidInput {
        ARGUMENT
    } else {
        failure(out, error)
    }
}
fn snapshot(owner: &Owner, out: &mut Outcome) {
    out.pid = owner.inner.root_pid();
    if let Some(code) = owner.inner.root_exit() {
        out.flags |= ROOT_KNOWN;
        out.root_exit = code;
    }
}
unsafe fn call(
    raw: *mut Owner,
    out: *mut Outcome,
    bytes: u32,
    action: impl FnOnce(&mut Owner, &mut Outcome) -> u32,
) -> u32 {
    // SAFETY: each caller supplies exclusive valid output by exported contract.
    let out = match unsafe { output(out, bytes) } {
        Ok(o) => o,
        Err(e) => return e,
    };
    if raw.is_null() || !raw.is_aligned() {
        return ARGUMENT;
    }
    // SAFETY: raw must be a live unique owner from create, no concurrent calls.
    let owner = unsafe { &mut *raw };
    let status = action(owner, out);
    snapshot(owner, out);
    status
}
fn encode_settlement(value: Settlement, out: &mut Outcome) {
    match value {
        Settlement::Pending {
            root_exit,
            active_processes,
        } => {
            out.state = PENDING;
            if let Some(code) = root_exit {
                out.flags |= ROOT_KNOWN;
                out.root_exit = code;
            }
            if let Some(count) = active_processes {
                out.flags |= ACTIVE_KNOWN;
                out.active = count;
            }
        }
        Settlement::Complete { root_exit } => {
            out.state = COMPLETE;
            out.flags |= ROOT_KNOWN;
            out.root_exit = root_exit;
        }
        Settlement::CleanupFailed {
            root_exit,
            failures,
        } => {
            out.state = CLEANUP_FAILED;
            out.flags |= ROOT_KNOWN;
            out.root_exit = root_exit;
            for failure in failures {
                let index = match failure.operation {
                    "CloseThread" => 0,
                    "CloseProcess" => 1,
                    "CloseJob" => 2,
                    _ => unreachable!("native owner changed the fixed close contract"),
                };
                out.cleanup_mask |= 1 << index;
                out.cleanup_codes[index] = failure.code;
            }
        }
    }
}

/// Create one suspended owner, copying all input before return.
/// # Safety
/// Follow include/owner_v1.h: req points to a full initialized Create; all input
/// views are readable for their lengths. slot/out are disjoint exclusive valid
/// storage; *slot is NULL. No pointer may be retained/reused after release.
#[no_mangle]
pub unsafe extern "C" fn mh_create1(
    req: *const Create,
    req_bytes: u32,
    slot: *mut *mut Owner,
    out: *mut Outcome,
    out_bytes: u32,
) -> u32 {
    // SAFETY: forward the unchanged ABI1 creation contract, with empty environment.
    unsafe { create_impl(req, req_bytes, slot, out, out_bytes, None) }
}

/// Create with a complete explicit environment, sharing the original creator.
/// # Safety
/// In addition to mh_create1, env must point to a full initialized Environment
/// with env_bytes equal to its exact layout size. All entries and UTF16 views
/// are valid, immutable, non-overlapping with outputs, and live through return.
#[no_mangle]
pub unsafe extern "C" fn mh_create_env1(
    req: *const Create,
    req_bytes: u32,
    env: *const Environment,
    env_bytes: u32,
    slot: *mut *mut Owner,
    out: *mut Outcome,
    out_bytes: u32,
) -> u32 {
    // SAFETY: forward the caller contract; one common path owns validation/create.
    unsafe { create_impl(req, req_bytes, slot, out, out_bytes, Some((env, env_bytes))) }
}

unsafe fn create_impl(
    req: *const Create,
    req_bytes: u32,
    slot: *mut *mut Owner,
    out: *mut Outcome,
    out_bytes: u32,
    environment: Option<(*const Environment, u32)>,
) -> u32 {
    // SAFETY: valid exclusive caller output, or structural rejection.
    let out = match unsafe { output(out, out_bytes) } {
        Ok(o) => o,
        Err(e) => return e,
    };
    if req.is_null() || !req.is_aligned() || slot.is_null() || !slot.is_aligned() {
        return ARGUMENT;
    }
    if req_bytes as usize != size_of::<Create>() {
        return ABI;
    }
    // SAFETY: caller owns complete initialized request and separate owner slot.
    let req = unsafe { &*req };
    if req.version != 1 || req.reserved != 0 {
        return ABI;
    }
    if unsafe { !(*slot).is_null() } {
        return STATE;
    }
    if req.argc > 64 {
        return ARGUMENT;
    }
    let views = if req.argc == 0 {
        &[][..]
    } else {
        if req.args.is_null() || !req.args.is_aligned() {
            return ARGUMENT;
        }
        // SAFETY: validated count, caller promises full array lifetime.
        unsafe { slice::from_raw_parts(req.args, req.argc) }
    };
    let total = views
        .iter()
        .map(|v| v.len)
        .chain([req.program.len, req.cwd.len])
        .try_fold(0usize, |a, b| a.checked_add(b));
    if total.is_none_or(|units| units > MAX_UNITS) {
        return ARGUMENT;
    }
    // SAFETY: inputs validated structurally, underlying readable memory promised.
    let prepared = unsafe {
        (|| {
            let program = PathBuf::from(copy_string(req.program)?);
            let cwd = PathBuf::from(copy_string(req.cwd)?);
            let args = views
                .iter()
                .map(|v| copy_string(*v))
                .collect::<Result<Vec<_>, _>>()?;
            if !program.is_absolute() || !cwd.is_absolute() {
                return Err(ARGUMENT);
            }
            Ok((program, cwd, args))
        })()
    };
    let (program, cwd, args) = match prepared {
        Ok(p) => p,
        Err(e) => return e,
    };
    let environment = match environment {
        None => Vec::new(),
        Some((view, bytes)) => {
            if bytes as usize != size_of::<Environment>() {
                return ABI;
            }
            if view.is_null() || !view.is_aligned() {
                return ARGUMENT;
            }
            // SAFETY: full caller environment object; nested view checked above.
            match unsafe { copy_environment(&*view) } {
                Ok(entries) => entries,
                Err(code) => return code,
            }
        }
    };
    match NativeOwner::create_suspended_with_context(
        &program,
        &args,
        LaunchContext {
            current_dir: &cwd,
            environment: &environment,
        },
        None,
    ) {
        Ok(inner) => {
            let owner = Box::new(Owner {
                inner,
                terminal: false,
            });
            snapshot(&owner, out);
            // SAFETY: publish exactly one Rust allocation into empty caller slot.
            unsafe { *slot = Box::into_raw(owner) };
            OK
        }
        Err(e) => create_failure(out, e),
    }
}

/// Apply a one-shot caller decision. This boolean is not identity authorization.
/// # Safety
/// raw is a live exclusive owner from this DLL; out follows the header contract.
#[no_mangle]
pub unsafe extern "C" fn mh_admit1(
    raw: *mut Owner,
    allow: u32,
    out: *mut Outcome,
    bytes: u32,
) -> u32 {
    // SAFETY: forwarded exclusive owner/output contract, no pointers retained.
    unsafe {
        call(raw, out, bytes, |owner, out| {
            if allow > 1 {
                return ARGUMENT;
            }
            match owner.inner.admit_and_resume(|_| {
                if allow == 1 {
                    Ok(())
                } else {
                    Err(io::ErrorKind::PermissionDenied.into())
                }
            }) {
                Ok(()) => OK,
                Err(e) => failure(out, e),
            }
        })
    }
}
/// Call-scoped native observation callback. Values are not native handles.
pub type AdmissionCheck = unsafe extern "C" fn(u32, *const u16, usize, usize) -> u32;

/// Run the foreign decision inside the ORIGINAL retained-process admission.
/// # Safety
/// Follow the live owner/output contract and header callback obligations.
/// The callback must use the C ABI, return normally, never mutate/retain the
/// read-only image slice or reenter this owner, and keep its token state live.
/// Raw foreign callers, unlike the supplied Go adapter, must contain their own
/// exceptions/panics before returning across this non-unwinding boundary.
#[no_mangle]
pub unsafe extern "C" fn mh_admit_checked1(
    raw: *mut Owner,
    check: Option<AdmissionCheck>,
    token: usize,
    out: *mut Outcome,
    bytes: u32,
) -> u32 {
    // SAFETY: same exclusive owner/output contract as the original entry point.
    unsafe {
        call(raw, out, bytes, |owner, out| {
            let Some(check) = check else {
                return ARGUMENT;
            };
            let mut invalid_decision = false;
            let result = owner.inner.admit_and_resume(|view| {
                let pid = view.process_id()?;
                let path = view.image_path()?;
                let image: Vec<u16> = path.as_os_str().encode_wide().collect();
                // SAFETY: synchronous function pointer and caller token valid by
                // contract; immutable image stays alive throughout this call. Parent
                // admission retains its original process, no PID re-open occurs.
                match check(pid, image.as_ptr(), image.len(), token) {
                    1 => Ok(()),
                    0 => Err(io::ErrorKind::PermissionDenied.into()),
                    _ => {
                        invalid_decision = true;
                        Err(io::ErrorKind::InvalidInput.into())
                    }
                }
            });
            if invalid_decision {
                return ARGUMENT;
            }
            match result {
                Ok(()) => OK,
                Err(error) => failure(out, error),
            }
        })
    }
}
/// Stop only the retained owner's Job; never reopen by PID.
/// # Safety
/// Same live owner/output requirements as mh_admit1.
#[no_mangle]
pub unsafe extern "C" fn mh_stop1(
    raw: *mut Owner,
    code: u32,
    out: *mut Outcome,
    bytes: u32,
) -> u32 {
    // SAFETY: forwarded exclusive owner/output contract.
    unsafe {
        call(raw, out, bytes, |owner, out| {
            match owner.inner.terminate(code) {
                Ok(()) => OK,
                Err(e) => failure(out, e),
            }
        })
    }
}
/// Wait within finite budget. Pending and failures keep this owner live.
/// # Safety
/// Same live owner/output requirements as mh_admit1.
#[no_mangle]
pub unsafe extern "C" fn mh_finish1(
    raw: *mut Owner,
    ms: u32,
    out: *mut Outcome,
    bytes: u32,
) -> u32 {
    // SAFETY: forwarded exclusive owner/output contract.
    unsafe {
        call(raw, out, bytes, |owner, out| {
            if ms > 60000 {
                return ARGUMENT;
            }
            match owner.inner.finish(Duration::from_millis(ms.into())) {
                Ok(value) => {
                    owner.terminal = !matches!(value, Settlement::Pending { .. });
                    encode_settlement(value, out);
                    OK
                }
                Err(e) => failure(out, e),
            }
        })
    }
}
/// Release only a terminal owner. No stop, retry, implicit finish or finalizer.
/// # Safety
/// slot owns the sole live pointer, or NULL; out is disjoint and valid. No aliases
/// may call/release after this; slot is cleared only after terminal deallocation.
#[no_mangle]
pub unsafe extern "C" fn mh_release1(slot: *mut *mut Owner, out: *mut Outcome, bytes: u32) -> u32 {
    // SAFETY: exported exclusive buffer contract.
    let out = match unsafe { output(out, bytes) } {
        Ok(o) => o,
        Err(e) => return e,
    };
    if slot.is_null() || !slot.is_aligned() {
        return ARGUMENT;
    }
    // SAFETY: caller slot valid; its nonnull value must be original live owner.
    let raw = unsafe { *slot };
    if raw.is_null() {
        return OK;
    }
    if !raw.is_aligned() {
        return ARGUMENT;
    }
    let owner = unsafe { &mut *raw };
    snapshot(owner, out);
    if !owner.terminal {
        return NOT_SETTLED;
    }
    // Re-read the cached terminal outcome, not another cleanup attempt.
    match owner.inner.finish(Duration::ZERO) {
        Ok(value) => encode_settlement(value, out),
        Err(e) => return failure(out, e),
    }
    // SAFETY: consume exactly one Box allocated by create, then invalidate slot.
    unsafe {
        drop(Box::from_raw(raw));
        *slot = ptr::null_mut()
    };
    OK
}

#[cfg(test)]
mod tests;

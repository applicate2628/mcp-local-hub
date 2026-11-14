//! Versioned identity observations over the existing one-shot owner admission.
use super::{call, failure, Outcome, Owner, ABI, ARGUMENT, OK, STATE, U16};
use mcphub_process_owner_spike::{ProcessView, RootWait, TokenUserError};
use std::{io, mem::size_of, os::windows::ffi::OsStrExt};

const VERSION: u32 = 1;
const SID_CAPACITY: usize = 68;
const STAGE_NONE: u32 = 0;
const STAGE_PID: u32 = 1;
const STAGE_IMAGE: u32 = 2;
const STAGE_CREATION: u32 = 3;
const STAGE_JOB: u32 = 4;
const STAGE_WAIT: u32 = 5;
const STAGE_TOKEN: u32 = 6;
const STAGE_COMPLETE: u32 = 7;
const ERROR_NONE: u32 = 0;
const ERROR_NATIVE: u32 = 1;
const ERROR_INVALID_DATA: u32 = 2;
const ERROR_NO_MEMORY: u32 = 3;
const ERROR_OTHER: u32 = 4;
const JOB_MEMBER: u32 = 1;
const JOB_NOT_MEMBER: u32 = 2;
const WAIT_NOT_SIGNALED: u32 = 1;
const WAIT_SIGNALED: u32 = 2;

#[repr(C)]
pub struct Identity {
    size: u32,
    version: u32,
    pid: u32,
    job_membership: u32,
    creation_time_100ns: u64,
    wait_state: u32,
    sid_len: u32,
    image: U16,
    sid: [u8; SID_CAPACITY],
    reserved: u32,
}

#[repr(C)]
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub struct IdentityReport {
    size: u32,
    version: u32,
    stage: u32,
    query_error_kind: u32,
    query_native_error: i32,
    token_close_error: i32,
}

pub type IdentityCheck = unsafe extern "C" fn(*const Identity, u32, usize) -> u32;

// Keep the image allocation alive alongside the raw view throughout the callback.
struct OwnedObservation {
    value: Identity,
    _image: Vec<u16>,
}

unsafe fn report_output<'a>(
    raw: *mut IdentityReport,
    bytes: u32,
) -> Result<&'a mut IdentityReport, u32> {
    if raw.is_null() || !raw.is_aligned() || bytes as usize != size_of::<IdentityReport>() {
        return Err(ARGUMENT);
    }
    // SAFETY: caller supplies exclusive initialized/writable report storage,
    // disjoint from owner, Outcome and all callback inputs; size/alignment checked.
    unsafe {
        raw.write(IdentityReport {
            size: size_of::<IdentityReport>() as u32,
            version: VERSION,
            stage: STAGE_NONE,
            query_error_kind: ERROR_NONE,
            ..IdentityReport::default()
        })
    };
    // SAFETY: the complete report was initialized above and remains exclusive.
    Ok(unsafe { &mut *raw })
}

fn record_query_error(report: &mut IdentityReport, stage: u32, error: &io::Error) {
    report.stage = stage;
    report.query_native_error = error.raw_os_error().unwrap_or_default();
    report.query_error_kind = if error.raw_os_error().is_some() {
        ERROR_NATIVE
    } else {
        match error.kind() {
            io::ErrorKind::InvalidData => ERROR_INVALID_DATA,
            io::ErrorKind::OutOfMemory => ERROR_NO_MEMORY,
            _ => ERROR_OTHER,
        }
    };
}

fn query<T>(
    report: &mut IdentityReport,
    stage: u32,
    action: impl FnOnce() -> io::Result<T>,
) -> Result<T, ()> {
    report.stage = stage;
    action().map_err(|error| record_query_error(report, stage, &error))
}

fn record_token_error(report: &mut IdentityReport, error: TokenUserError) {
    report.stage = STAGE_TOKEN;
    match error {
        TokenUserError::Observation(error) => record_query_error(report, STAGE_TOKEN, &error),
        TokenUserError::Cleanup {
            observation,
            failure,
        } => {
            if let Some(error) = observation {
                record_query_error(report, STAGE_TOKEN, &error);
            }
            // The native TokenUserError::Cleanup contract names this operation
            // CloseToken. It is separate from the owner's three cleanup slots.
            report.token_close_error = failure.code;
        }
    }
}

fn observe(view: ProcessView<'_>, report: &mut IdentityReport) -> Result<OwnedObservation, ()> {
    let pid = query(report, STAGE_PID, || view.process_id())?;
    let image = query(report, STAGE_IMAGE, || {
        let path = view.image_path()?;
        let count = path.as_os_str().encode_wide().count();
        if count == 0 || count > 32768 {
            return Err(io::ErrorKind::InvalidData.into());
        }
        let mut image = Vec::new();
        image
            .try_reserve_exact(count)
            .map_err(|_| io::Error::from(io::ErrorKind::OutOfMemory))?;
        image.extend(path.as_os_str().encode_wide());
        if image.contains(&0) {
            return Err(io::ErrorKind::InvalidData.into());
        }
        Ok(image)
    })?;
    let creation_time_100ns = query(report, STAGE_CREATION, || view.creation_time_100ns())?;
    let job_membership = if query(report, STAGE_JOB, || view.is_in_original_job())? {
        JOB_MEMBER
    } else {
        JOB_NOT_MEMBER
    };
    let wait_state = match query(report, STAGE_WAIT, || view.wait_state())? {
        RootWait::NotSignaled => WAIT_NOT_SIGNALED,
        RootWait::Signaled => WAIT_SIGNALED,
    };
    report.stage = STAGE_TOKEN;
    let sid = view
        .token_user_sid()
        .map_err(|error| record_token_error(report, error))?;
    if !(8..=SID_CAPACITY).contains(&sid.len()) {
        record_query_error(
            report,
            STAGE_TOKEN,
            &io::Error::from(io::ErrorKind::InvalidData),
        );
        return Err(());
    }
    let mut value = Identity {
        size: size_of::<Identity>() as u32,
        version: VERSION,
        pid,
        job_membership,
        creation_time_100ns,
        wait_state,
        sid_len: sid.len() as u32,
        image: U16 {
            data: image.as_ptr(),
            len: image.len(),
        },
        sid: [0; SID_CAPACITY],
        reserved: 0,
    };
    value.sid[..sid.len()].copy_from_slice(&sid);
    report.stage = STAGE_COMPLETE;
    Ok(OwnedObservation {
        value,
        _image: image,
    })
}

/// Collect all identity observations and then request one foreign decision.
/// The existing owner still controls admission, resume and explicit settlement.
/// # Safety
/// Follow include/identity_v1.h: owner is live/exclusive, output buffers are
/// valid, aligned and mutually disjoint. The callback uses this exact C ABI,
/// returns normally, does not retain/mutate native views or reenter the owner,
/// and contains its own language exceptions/panics. Its token stays live until
/// return and is never dereferenced here. No native pointer escapes the callback.
#[no_mangle]
pub unsafe extern "C" fn mh_admit_identity1(
    raw: *mut Owner,
    version: u32,
    check: Option<IdentityCheck>,
    token: usize,
    report: *mut IdentityReport,
    report_bytes: u32,
    out: *mut Outcome,
    out_bytes: u32,
) -> u32 {
    // SAFETY: valid caller report storage by this export's disjoint-output
    // contract. A malformed size/null/alignment is rejected without dereference.
    let report = unsafe { report_output(report, report_bytes) };
    // SAFETY: reuse the existing Outcome/owner validation and final snapshot.
    // Request validation below precedes the only native admission invocation.
    unsafe {
        call(raw, out, out_bytes, |owner, out| {
            let report = match report {
                Ok(report) => report,
                Err(status) => return status,
            };
            if version != VERSION {
                return ABI;
            }
            let Some(check) = check else {
                return ARGUMENT;
            };
            let mut invalid_decision = false;
            let result = owner.inner.admit_and_resume(|view| {
                let observation = observe(view, report).map_err(|()| io::ErrorKind::Other)?;
                // SAFETY: complete initialized identity and its owned image stay
                // live/immutable until this synchronous callback returns. The
                // callback/token obligations are imposed on the foreign caller.
                match check(&observation.value, size_of::<Identity>() as u32, token) {
                    1 => Ok(()),
                    0 => Err(io::ErrorKind::PermissionDenied.into()),
                    _ => {
                        invalid_decision = true;
                        Err(io::ErrorKind::InvalidInput.into())
                    }
                }
            });
            if report.stage != STAGE_NONE && report.stage != STAGE_COMPLETE {
                // The typed report retains query and token-close failures;
                // ABI1 Outcome has no token cleanup slot and stays unchanged.
                return STATE;
            }
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::DENIED;
    use mcphub_process_owner_spike::{NativeFailure, NativeOwner, Settlement};
    use std::{
        mem::{align_of, offset_of},
        time::Duration,
    };

    #[test]
    fn identity_layout_is_separate_from_abi1_outcome() {
        assert_eq!(size_of::<Identity>(), 120);
        assert_eq!(align_of::<Identity>(), 8);
        assert_eq!(offset_of!(Identity, creation_time_100ns), 16);
        assert_eq!(offset_of!(Identity, image), 32);
        assert_eq!(offset_of!(Identity, sid), 48);
        assert_eq!(offset_of!(Identity, reserved), 116);
        assert_eq!(size_of::<IdentityReport>(), 24);
        assert_eq!(offset_of!(IdentityReport, query_native_error), 16);
        assert_eq!(offset_of!(IdentityReport, token_close_error), 20);
        assert_eq!(size_of::<Outcome>(), 48);
    }

    #[test]
    fn report_storage_is_initialized_and_bad_metadata_is_not_dereferenced() {
        let mut report = IdentityReport {
            stage: 99,
            query_error_kind: 99,
            query_native_error: 87,
            token_close_error: 6,
            ..IdentityReport::default()
        };
        // SAFETY: exclusive complete report buffer for the exact supplied size.
        let initialized =
            unsafe { report_output(&mut report, size_of::<IdentityReport>() as u32).unwrap() };
        assert_eq!(
            *initialized,
            IdentityReport {
                size: 24,
                version: 1,
                ..IdentityReport::default()
            }
        );
        let mut storage = [0x5555_5555u32; 7];
        let before = storage;
        // SAFETY: deliberately malformed metadata must be rejected before any
        // pointer access. The backing array remains live for these calls.
        unsafe {
            assert_eq!(
                report_output(std::ptr::null_mut(), 24).unwrap_err(),
                ARGUMENT
            );
            assert_eq!(
                report_output(storage.as_mut_ptr().cast(), 23).unwrap_err(),
                ARGUMENT
            );
            assert_eq!(
                report_output(storage.as_mut_ptr().cast(), 25).unwrap_err(),
                ARGUMENT
            );
            assert_eq!(
                report_output(storage.as_mut_ptr().cast::<u8>().add(1).cast(), 24).unwrap_err(),
                ARGUMENT
            );
        }
        assert_eq!(storage, before);
    }

    #[test]
    fn query_error_classification_preserves_stage_native_code_and_non_native_kind() {
        for (stage, error, kind, native) in [
            (STAGE_PID, io::Error::from_raw_os_error(5), ERROR_NATIVE, 5),
            (
                STAGE_CREATION,
                io::ErrorKind::InvalidData.into(),
                ERROR_INVALID_DATA,
                0,
            ),
            (
                STAGE_IMAGE,
                io::ErrorKind::OutOfMemory.into(),
                ERROR_NO_MEMORY,
                0,
            ),
            (STAGE_WAIT, io::ErrorKind::Other.into(), ERROR_OTHER, 0),
        ] {
            let mut report = IdentityReport::default();
            let result: Result<(), ()> = query(&mut report, stage, || Err(error));
            assert!(result.is_err());
            assert_eq!(report.stage, stage);
            assert_eq!(report.query_error_kind, kind);
            assert_eq!(report.query_native_error, native);
            assert_eq!(report.token_close_error, 0);
        }
    }

    #[test]
    fn token_query_and_close_failures_remain_independent() {
        let cases = [
            (
                TokenUserError::Observation(io::Error::from_raw_os_error(5)),
                ERROR_NATIVE,
                5,
                0,
            ),
            (
                TokenUserError::Cleanup {
                    observation: None,
                    failure: NativeFailure {
                        operation: "CloseToken",
                        code: 6,
                    },
                },
                ERROR_NONE,
                0,
                6,
            ),
            (
                TokenUserError::Cleanup {
                    observation: Some(io::Error::from_raw_os_error(5)),
                    failure: NativeFailure {
                        operation: "CloseToken",
                        code: 6,
                    },
                },
                ERROR_NATIVE,
                5,
                6,
            ),
            (
                TokenUserError::Cleanup {
                    observation: Some(io::ErrorKind::InvalidData.into()),
                    failure: NativeFailure {
                        operation: "CloseToken",
                        code: 6,
                    },
                },
                ERROR_INVALID_DATA,
                0,
                6,
            ),
        ];
        for (error, kind, native, close) in cases {
            let mut report = IdentityReport::default();
            record_token_error(&mut report, error);
            assert_eq!(report.stage, STAGE_TOKEN);
            assert_eq!(report.query_error_kind, kind);
            assert_eq!(report.query_native_error, native);
            assert_eq!(report.token_close_error, close);
        }
    }
    #[test]
    fn native_preflight_preserves_admission_then_deny_is_single_use() {
        unsafe extern "C" fn deny(_identity: *const Identity, _bytes: u32, token: usize) -> u32 {
            // SAFETY: this test supplies its exclusive live call counter for
            // the synchronous callback; the producer never dereferences it.
            unsafe { *(token as *mut usize) += 1 };
            0
        }

        let executable = std::env::current_exe().unwrap();
        let inner = NativeOwner::create_suspended(&executable, &["--list".into()]).unwrap();
        // The Box remains owned by this test. The export only borrows its
        // pointer; mh_release1 is not used on this caller-owned allocation.
        let mut owner = Box::new(Owner {
            inner,
            terminal: false,
        });
        let raw = &mut *owner as *mut Owner;
        let mut calls = 0usize;
        let token = &mut calls as *mut usize as usize;
        let check: Option<IdentityCheck> = Some(deny);
        let report_bytes = size_of::<IdentityReport>() as u32;
        let out_bytes = size_of::<Outcome>() as u32;
        let mut report = IdentityReport::default();
        let mut out = Outcome::default();
        for (version, callback, report_size, out_size, expected) in [
            (2, check, report_bytes, out_bytes, ABI),
            (1, check, report_bytes - 1, out_bytes, ARGUMENT),
            (1, None, report_bytes, out_bytes, ARGUMENT),
            (1, check, report_bytes, out_bytes - 1, ARGUMENT),
        ] {
            // SAFETY: live caller-owned Owner and disjoint full output storage.
            // Deliberately bad metadata must reject before native admission.
            let status = unsafe {
                mh_admit_identity1(
                    raw,
                    version,
                    callback,
                    token,
                    &mut report,
                    report_size,
                    &mut out,
                    out_size,
                )
            };
            assert_eq!(status, expected);
            assert_eq!(calls, 0, "preflight invoked the identity callback");
        }

        // SAFETY: the same complete owner/outputs/token remain live and exclusive.
        let status = unsafe {
            mh_admit_identity1(
                raw,
                VERSION,
                check,
                token,
                &mut report,
                report_bytes,
                &mut out,
                out_bytes,
            )
        };
        assert_eq!(status, DENIED);
        assert_eq!(calls, 1, "preflight consumed the original admission");
        assert_eq!(report.stage, STAGE_COMPLETE);
        assert_eq!(report.query_error_kind, ERROR_NONE);
        assert_eq!(report.query_native_error, 0);
        assert_eq!(report.token_close_error, 0);

        // SAFETY: same retained owner; the consumed admission must reject.
        let status = unsafe {
            mh_admit_identity1(
                raw,
                VERSION,
                check,
                token,
                &mut report,
                report_bytes,
                &mut out,
                out_bytes,
            )
        };
        assert_eq!(status, STATE);
        assert_eq!(calls, 1, "identity policy was replayed");
        assert_eq!(report.stage, STAGE_NONE);
        owner.inner.terminate(81).unwrap();
        assert_eq!(
            owner.inner.finish(Duration::from_secs(3)).unwrap(),
            Settlement::Complete { root_exit: 81 }
        );
        drop(owner);
    }
}

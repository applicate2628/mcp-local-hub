//! Native failures on owned test resources; no runtime syscall substitution.
use super::*;

#[test]
fn attribute_query_failure_preserves_native_error_code() {
    let mut bytes = 0;
    // SAFETY: documented size-query shape; intentionally invalid attribute count.
    let result = unsafe { InitializeProcThreadAttributeList(null_mut(), u32::MAX, 0, &mut bytes) };
    let native = io::Error::last_os_error();
    assert_eq!(
        result, 0,
        "invalid native attribute count unexpectedly accepted"
    );
    assert_ne!(
        native.raw_os_error(),
        Some(ERROR_INSUFFICIENT_BUFFER as i32)
    );
    assert!(native.raw_os_error().is_some_and(|code| code != 0));
    let failure = match AttributeList::new(u32::MAX) {
        Ok(_) => panic!("invalid attribute count produced an initialized owner"),
        Err(failure) => failure,
    };
    assert_eq!(
        failure.raw_os_error(),
        native.raw_os_error(),
        "formatting the native failure must not discard its machine-readable code"
    );
}

use windows_sys::Win32::{
    Foundation::{
        DuplicateHandle, SetHandleInformation, ERROR_ACCESS_DENIED, HANDLE_FLAG_PROTECT_FROM_CLOSE,
    },
    System::Threading::{
        GetCurrentProcess, GetProcessHandleCount, SuspendThread, PROCESS_QUERY_LIMITED_INFORMATION,
        PROCESS_SYNCHRONIZE, THREAD_QUERY_LIMITED_INFORMATION,
    },
};

const WAIT: Duration = Duration::from_secs(3);
// WinNT.h Job access rights; windows-sys does not export these macros here.
const JOB_QUERY: u32 = 0x0004;
const JOB_TERMINATE: u32 = 0x0008;

fn query_token(owner: &NativeOwner) -> OwnedHandle {
    let mut raw = null_mut();
    // SAFETY: the original process remains owned and the output is writable.
    assert_ne!(
        unsafe {
            windows_sys::Win32::System::Threading::OpenProcessToken(
                owner.process.as_ref().unwrap().as_raw_handle(),
                windows_sys::Win32::Security::TOKEN_QUERY,
                &mut raw,
            )
        },
        0
    );
    // SAFETY: successful acquisition grants exactly one owned token handle.
    unsafe { OwnedHandle::from_raw_handle(raw) }
}

#[test]
fn token_user_open_failure_uses_original_process_and_keeps_owner() {
    let mut owner = suspended();
    let original = restrict(&mut owner.process, PROCESS_SYNCHRONIZE);
    let failure = owner
        .admit_and_resume(|view| {
            let failure = match view.token_user_sid() {
                Err(failure) => failure,
                Ok(_) => panic!("token lookup ignored retained process handle"),
            };
            match failure {
                crate::TokenUserError::Observation(error) => {
                    denied(&error);
                    Err(error)
                }
                other => panic!("open failure claimed token cleanup: {other:?}"),
            }
        })
        .unwrap_err();
    denied(&failure);
    drop(owner.process.replace(original));
    assert_eq!(owner.wait_root(Duration::ZERO).unwrap(), None);
    assert_eq!(owner.active_processes().unwrap(), 1);
    stop_and_finish(&mut owner);
}

#[test]
fn token_user_query_failure_preserves_native_error_and_closes_token() {
    let mut owner = suspended();
    let source = query_token(&owner);
    let before = handle_count();
    let restricted = duplicate_with_access(&source, 0);
    match crate::token_user::owned_token_user_sid(restricted) {
        Err(crate::TokenUserError::Observation(error)) => denied(&error),
        _ => panic!("denied token query lost its outcome"),
    }
    assert_eq!(
        handle_count(),
        before,
        "failed token query leaked its handle"
    );
    drop(source);
    stop_and_finish(&mut owner);
}

// Fixture-only recovery authority: protection proves this exact owned handle
// cannot have closed/recycled. There is never a second OwnedHandle alias.
struct ProtectedToken {
    token: Option<OwnedHandle>,
    raw: HANDLE,
}
impl ProtectedToken {
    fn new(token: OwnedHandle) -> Self {
        let raw = token.as_raw_handle();
        // SAFETY: this fixture controls the live owned token handle exclusively.
        assert_ne!(
            unsafe {
                SetHandleInformation(
                    raw,
                    HANDLE_FLAG_PROTECT_FROM_CLOSE,
                    HANDLE_FLAG_PROTECT_FROM_CLOSE,
                )
            },
            0
        );
        Self {
            token: Some(token),
            raw,
        }
    }
    fn take(&mut self) -> OwnedHandle {
        self.token.take().unwrap()
    }
}
impl Drop for ProtectedToken {
    fn drop(&mut self) {
        let mut flags = 0;
        // SAFETY: this is the protected fixture handle, not a PID or reopened resource.
        if unsafe { GetHandleInformation(self.raw, &mut flags) } == 0
            || flags & HANDLE_FLAG_PROTECT_FROM_CLOSE == 0
        {
            eprintln!("token fixture protection lost; refusing numeric-handle cleanup");
            return;
        }
        // SAFETY: protection still proves identity; revoke only this fixture's bit.
        if unsafe { SetHandleInformation(self.raw, HANDLE_FLAG_PROTECT_FROM_CLOSE, 0) } == 0 {
            eprintln!("token fixture could not remove close protection");
            return;
        }
        if self.token.is_none() {
            // SAFETY: the observing helper consumed its sole owner and failed to
            // close the protected handle; only the fixture can now reclaim it.
            if unsafe { CloseHandle(self.raw) } == 0 {
                eprintln!("token fixture final close failed");
            }
        }
    }
}

#[test]
fn token_user_protected_close_preserves_both_outcomes() {
    for deny_query in [false, true] {
        let mut owner = suspended();
        let before = handle_count();
        let source = query_token(&owner);
        let token = if deny_query {
            let restricted = duplicate_with_access(&source, 0);
            drop(source);
            restricted
        } else {
            source
        };
        let mut fixture = ProtectedToken::new(token);
        // SAFETY: fixture-only native reference failure. Protection guarantees
        // this attempt cannot close/recycle the handle; it is not a runtime retry.
        let rejected = unsafe { CloseHandle(fixture.raw) };
        let expected = io::Error::last_os_error();
        assert_eq!(rejected, 0);
        let expected_code = expected.raw_os_error().unwrap();
        match crate::token_user::owned_token_user_sid(fixture.take()) {
            Err(crate::TokenUserError::Cleanup {
                observation,
                failure,
            }) => {
                assert_eq!(failure.operation, "CloseToken");
                assert_eq!(
                    failure.code, expected_code,
                    "token close changed native error"
                );
                if deny_query {
                    denied(
                        observation
                            .as_ref()
                            .expect("token cleanup erased the query error"),
                    );
                } else {
                    assert!(
                        observation.is_none(),
                        "successful query was replaced by an error"
                    );
                }
            }
            _ => panic!("protected token close became success or lost its receipt"),
        }
        assert_eq!(owner.wait_root(Duration::ZERO).unwrap(), None);
        drop(fixture);
        assert_eq!(
            handle_count(),
            before,
            "protected token fixture leaked a handle"
        );
        stop_and_finish(&mut owner);
    }
}

#[test]
fn token_user_repeated_observation_closes_handles_and_copy_survives() {
    let mut owner = suspended();
    let mut observed = Vec::new();
    owner
        .admit_and_resume(|view| {
            observed = view.token_user_sid().map_err(io::Error::other)?;
            assert!(!observed.is_empty());
            let before = handle_count();
            for _ in 0..64 {
                let next = view.token_user_sid().map_err(io::Error::other)?;
                assert!(
                    observed == next,
                    "token user changed during fixture observation"
                );
                assert_eq!(handle_count(), before, "token observation leaked a handle");
            }
            Ok(())
        })
        .unwrap();
    let snapshot = observed.clone();
    assert_eq!(
        owner.finish(WAIT).unwrap(),
        Settlement::Complete { root_exit: 0 }
    );
    drop(owner);
    assert!(observed == snapshot, "copied SID changed after settlement");
}

#[test]
fn identity_initial_capture_error_is_not_repaired_by_later_success() {
    let mut owner = suspended();
    let restricted = duplicate_with_access(owner.process.as_ref().unwrap(), PROCESS_SYNCHRONIZE);
    // A real denied query supplies the stored failure. This tests retention of
    // capture failure, not a claim that normal creator handles lose their rights.
    let capture = creation_time_100ns(restricted.as_handle());
    assert_eq!(capture, Err(ERROR_ACCESS_DENIED as i32));
    owner.original_creation = capture;
    assert!(creation_time_100ns(owner.process.as_ref().unwrap().as_handle()).is_ok());
    let failure = owner
        .admit_and_resume(|view| view.creation_time_100ns().map(|_| ()))
        .unwrap_err();
    denied(&failure);
    assert_eq!(owner.wait_root(Duration::ZERO).unwrap(), None);
    let mut repeated = false;
    assert!(owner
        .admit_and_resume(|_| {
            repeated = true;
            Ok(())
        })
        .is_err());
    assert!(!repeated);
    stop_and_finish(&mut owner);
}

#[test]
fn identity_zero_or_changed_creation_binding_is_rejected() {
    for zero in [false, true] {
        let mut owner = suspended();
        let original = owner.original_creation.unwrap();
        owner.original_creation = Ok(if zero { 0 } else { original + 1 });
        let failure = owner
            .admit_and_resume(|view| view.creation_time_100ns().map(|_| ()))
            .unwrap_err();
        assert_eq!(failure.kind(), io::ErrorKind::InvalidData);
        assert_eq!(owner.wait_root(Duration::ZERO).unwrap(), None);
        stop_and_finish(&mut owner);
    }
}

#[test]
fn identity_process_query_failure_is_not_an_empty_observation() {
    let mut owner = suspended();
    let original = restrict(&mut owner.process, PROCESS_SYNCHRONIZE);
    let failure = owner
        .admit_and_resume(|view| {
            denied(&view.is_in_original_job().unwrap_err());
            view.creation_time_100ns().map(|_| ())
        })
        .unwrap_err();
    denied(&failure);
    drop(owner.process.replace(original));
    assert_eq!(owner.wait_root(Duration::ZERO).unwrap(), None);
    stop_and_finish(&mut owner);
}

#[test]
fn identity_job_query_failure_is_not_false_membership() {
    let mut owner = suspended();
    let original = restrict(&mut owner.job, JOB_TERMINATE);
    let failure = owner
        .admit_and_resume(|view| {
            assert_ne!(view.creation_time_100ns()?, 0);
            view.is_in_original_job().map(|_| ())
        })
        .unwrap_err();
    denied(&failure);
    drop(owner.job.replace(original));
    assert_eq!(owner.wait_root(Duration::ZERO).unwrap(), None);
    stop_and_finish(&mut owner);
}

#[test]
fn identity_membership_checks_exact_job_instead_of_any_job() {
    let mut owner = suspended();
    // SAFETY: a new unnamed, exclusively owned test Job with default security.
    let unrelated = unsafe { CreateJobObjectW(null(), null()) };
    assert!(!unrelated.is_null());
    // SAFETY: successful acquisition grants exactly this one owned handle.
    let original = owner
        .job
        .replace(unsafe { OwnedHandle::from_raw_handle(unrelated) })
        .unwrap();
    let failure = owner
        .admit_and_resume(|view| {
            assert!(
                !view.is_in_original_job()?,
                "membership in another Job was accepted"
            );
            assert_eq!(view.wait_state()?, RootWait::NotSignaled);
            Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "test policy rejects wrong Job",
            ))
        })
        .unwrap_err();
    assert_eq!(failure.kind(), io::ErrorKind::PermissionDenied);
    drop(owner.job.replace(original));
    stop_and_finish(&mut owner);
}

#[test]
fn identity_wait_failure_is_not_a_liveness_result() {
    let mut owner = suspended();
    let original = restrict(&mut owner.process, PROCESS_QUERY_LIMITED_INFORMATION);
    let failure = owner
        .admit_and_resume(|view| {
            assert_ne!(view.creation_time_100ns()?, 0);
            assert!(view.is_in_original_job()?);
            let failure = view
                .wait_state()
                .expect_err("failed observation became a liveness value");
            denied(&failure);
            Err(failure)
        })
        .unwrap_err();
    denied(&failure);
    drop(owner.process.replace(original));
    stop_and_finish(&mut owner);
}

#[test]
fn identity_signaled_process_with_exit_259_is_not_alive_or_resumed() {
    use windows_sys::Win32::System::Threading::TerminateProcess;
    let mut owner = suspended();
    let birth = owner.original_creation.unwrap();
    // SAFETY: terminate only the original process owned by this test fixture.
    assert_ne!(
        unsafe { TerminateProcess(owner.process.as_ref().unwrap().as_raw_handle(), 259) },
        0
    );
    assert_eq!(owner.wait_root(WAIT).unwrap(), Some(259));
    let mut observed = false;
    let failure = owner
        .admit_and_resume(|view| {
            observed = true;
            assert_eq!(view.creation_time_100ns()?, birth);
            assert_eq!(view.wait_state()?, RootWait::Signaled);
            Ok(())
        })
        .unwrap_err();
    assert!(observed);
    assert_eq!(failure.kind(), io::ErrorKind::InvalidInput);
    assert_eq!(
        owner.finish(WAIT).unwrap(),
        Settlement::Complete { root_exit: 259 }
    );
}

#[test]
fn repeated_identity_observations_do_not_accumulate_handles() {
    let mut owner = suspended();
    owner
        .admit_and_resume(|view| {
            let birth = view.creation_time_100ns()?;
            assert!(view.is_in_original_job()?);
            assert_eq!(view.wait_state()?, RootWait::NotSignaled);
            let baseline = handle_count();
            for _ in 0..64 {
                assert_eq!(view.creation_time_100ns()?, birth);
                assert!(view.is_in_original_job()?);
                assert_eq!(view.wait_state()?, RootWait::NotSignaled);
            }
            assert_eq!(handle_count(), baseline);
            Ok(())
        })
        .unwrap();
    assert_eq!(
        owner.finish(WAIT).unwrap(),
        Settlement::Complete { root_exit: 0 }
    );
}

fn suspended() -> NativeOwner {
    // This unit-test executable only lists its tests if resumed: no recursive
    // test execution, external program, UI, network or indefinite work.
    NativeOwner::create_suspended(&std::env::current_exe().unwrap(), &["--list".into()]).unwrap()
}
fn duplicate_with_access(source: &OwnedHandle, rights: u32) -> OwnedHandle {
    let mut duplicate = null_mut();
    // SAFETY: source stays owned and live; both process pseudo-handles identify
    // this process. No CLOSE_SOURCE, inheritance or SAME_ACCESS flag is used.
    let ok = unsafe {
        DuplicateHandle(
            GetCurrentProcess(),
            source.as_raw_handle(),
            GetCurrentProcess(),
            &mut duplicate,
            rights,
            0,
            0,
        )
    };
    assert_ne!(
        ok,
        0,
        "reduce own test handle rights: {}",
        io::Error::last_os_error()
    );
    // SAFETY: successful native duplication returned exactly one owned handle.
    unsafe { OwnedHandle::from_raw_handle(duplicate) }
}
fn restrict(slot: &mut Option<OwnedHandle>, rights: u32) -> OwnedHandle {
    let restricted = duplicate_with_access(slot.as_ref().unwrap(), rights);
    slot.replace(restricted).unwrap()
}
fn denied(error: &io::Error) {
    assert_eq!(error.raw_os_error(), Some(ERROR_ACCESS_DENIED as i32));
}
fn stop_and_finish(owner: &mut NativeOwner) {
    owner.terminate(83).unwrap();
    assert_eq!(
        owner.finish(WAIT).unwrap(),
        Settlement::Complete { root_exit: 83 }
    );
}
fn exited_owner() -> NativeOwner {
    let mut owner = suspended();
    owner.admit_and_resume(|_| Ok(())).unwrap();
    assert_eq!(owner.wait_root(WAIT).unwrap(), Some(0));
    let until = Instant::now() + WAIT;
    while owner.active_processes().unwrap() != 0 {
        assert!(Instant::now() < until, "owned Job did not settle");
        thread::sleep(Duration::from_millis(2));
    }
    owner
}
fn handle_count() -> u32 {
    let mut count = 0;
    // SAFETY: current-process pseudo handle and writable DWORD, no close.
    assert_ne!(
        unsafe { GetProcessHandleCount(GetCurrentProcess(), &mut count) },
        0
    );
    count
}

#[test]
fn native_create_failure_preserves_error_without_repeated_handle_growth() {
    let root = PathBuf::from(std::env::var_os("MCPHUB_RX02_FIXTURES").expect("owned fixture root"));
    let missing = root.join("never-created-native-failure.exe");
    assert!(!missing.exists());
    // Allow one-time runtime/OS initialization before testing accumulation.
    // This does not resolve or hide the separately documented first-call delta.
    let first = NativeOwner::create_suspended(&missing, &[]).err().unwrap();
    let code = first.raw_os_error().expect("native missing-image result");
    assert_ne!(code, 0);
    let count = handle_count();
    for _ in 0..24 {
        let result = NativeOwner::create_suspended_with_context(
            &missing,
            &[],
            LaunchContext {
                current_dir: &root,
                environment: &[],
            },
            None,
        );
        let error = result.err().expect("missing image cannot produce an owner");
        assert_eq!(error.raw_os_error(), Some(code));
        assert!(!missing.exists());
    }
    assert_eq!(
        handle_count(),
        count,
        "partial create leaks repeated Job ownership"
    );
}

#[test]
fn resume_access_denied_retains_original_suspended_owner_and_consumes_admission() {
    let mut owner = suspended();
    let original_thread = restrict(&mut owner.thread, THREAD_QUERY_LIMITED_INFORMATION);
    let pid = owner.root_pid();
    let failure = owner
        .admit_and_resume(|view| {
            assert_eq!(view.process_id()?, pid);
            Ok(())
        })
        .unwrap_err();
    denied(&failure);
    assert_eq!(owner.root_pid(), pid);
    assert_eq!(owner.wait_root(Duration::ZERO).unwrap(), None);
    assert_eq!(owner.active_processes().unwrap(), 1);
    drop(owner.thread.replace(original_thread));
    let mut called_again = false;
    assert!(owner
        .admit_and_resume(|_| {
            called_again = true;
            Ok(())
        })
        .is_err());
    assert!(
        !called_again,
        "failed native resume reopened the one-shot admission"
    );
    stop_and_finish(&mut owner);
}

#[test]
fn unexpected_suspend_count_keeps_owner_without_claiming_execution() {
    let mut owner = suspended();
    // SAFETY: this test exclusively retains the child's original primary thread.
    assert_eq!(
        unsafe { SuspendThread(owner.thread.as_ref().unwrap().as_raw_handle()) },
        1
    );
    let failure = owner.admit_and_resume(|_| Ok(())).unwrap_err();
    assert!(failure.to_string().contains("suspend count"));
    assert_eq!(owner.wait_root(Duration::ZERO).unwrap(), None);
    assert!(owner.admit_and_resume(|_| Ok(())).is_err());
    stop_and_finish(&mut owner);
}

#[test]
fn wait_access_denied_is_not_timeout_or_terminal_completion() {
    let mut owner = suspended();
    let original = restrict(&mut owner.process, PROCESS_QUERY_LIMITED_INFORMATION);
    denied(&owner.finish(Duration::ZERO).unwrap_err());
    assert_eq!(owner.root_exit(), None);
    assert!(owner.terminal.is_none());
    assert!(owner.process.is_some() && owner.thread.is_some() && owner.job.is_some());
    drop(owner.process.replace(original));
    assert!(matches!(
        owner.finish(Duration::ZERO).unwrap(),
        Settlement::Pending {
            root_exit: None,
            ..
        }
    ));
    stop_and_finish(&mut owner);
}

#[test]
fn exit_code_query_failure_does_not_fabricate_root_exit() {
    let mut owner = suspended();
    let original = restrict(&mut owner.process, PROCESS_SYNCHRONIZE);
    owner.admit_and_resume(|_| Ok(())).unwrap();
    denied(&owner.wait_root(WAIT).unwrap_err());
    assert_eq!(owner.root_exit(), None);
    assert!(owner.terminal.is_none());
    drop(owner.process.replace(original));
    assert_eq!(owner.wait_root(WAIT).unwrap(), Some(0));
    assert_eq!(
        owner.finish(WAIT).unwrap(),
        Settlement::Complete { root_exit: 0 }
    );
}

#[test]
fn job_query_failure_keeps_observed_root_and_live_owner_slots() {
    let mut owner = exited_owner();
    let original = restrict(&mut owner.job, JOB_TERMINATE);
    denied(&owner.finish(WAIT).unwrap_err());
    assert_eq!(owner.root_exit(), Some(0));
    assert!(owner.terminal.is_none());
    assert!(owner.process.is_some() && owner.thread.is_some() && owner.job.is_some());
    drop(owner.job.replace(original));
    assert_eq!(
        owner.finish(WAIT).unwrap(),
        Settlement::Complete { root_exit: 0 }
    );
}

#[test]
fn failed_termination_is_not_reissued_after_rights_are_restored() {
    let mut owner = suspended();
    let original = restrict(&mut owner.job, JOB_QUERY);
    denied(&owner.terminate(83).unwrap_err());
    drop(owner.job.replace(original));
    denied(&owner.terminate(83).unwrap_err()); // still the original failed attempt
    assert!(owner.terminate(84).is_err());
    assert_eq!(owner.wait_root(Duration::ZERO).unwrap(), None);
    assert_eq!(owner.active_processes().unwrap(), 1);
    // SAFETY: explicit test recovery using its retained original Job only,
    // not a retry in the library or a lookup of an unrelated process.
    assert_ne!(
        unsafe { TerminateJobObject(owner.job.as_ref().unwrap().as_raw_handle(), 83) },
        0
    );
    assert_eq!(
        owner.finish(WAIT).unwrap(),
        Settlement::Complete { root_exit: 83 }
    );
    denied(&owner.terminate(83).unwrap_err()); // execution and failed stop outcomes differ
}

// Fixture only: protected handles cannot be closed/recycled while protected.
// The guard removes the protection and reclaims only handles detached by finish.
// It never adopts a second OwnedHandle for a slot the owner still holds.
struct ProtectedOwner {
    owner: NativeOwner,
    handles: Vec<HANDLE>,
}
impl ProtectedOwner {
    fn new(mask: u8) -> Self {
        let mut fixture = Self {
            owner: exited_owner(),
            handles: Vec::new(),
        };
        for (index, slot) in [
            &fixture.owner.thread,
            &fixture.owner.process,
            &fixture.owner.job,
        ]
        .into_iter()
        .enumerate()
        {
            if mask & (1 << index) == 0 {
                continue;
            }
            let raw = slot.as_ref().unwrap().as_raw_handle();
            // SAFETY: this fixture exclusively controls the live owned handle.
            assert_ne!(
                unsafe {
                    SetHandleInformation(
                        raw,
                        HANDLE_FLAG_PROTECT_FROM_CLOSE,
                        HANDLE_FLAG_PROTECT_FROM_CLOSE,
                    )
                },
                0
            );
            fixture.handles.push(raw);
        }
        fixture
    }
}
impl Drop for ProtectedOwner {
    fn drop(&mut self) {
        for &raw in &self.handles {
            let mut flags = 0;
            // SAFETY: protection remained set since acquisition; the handle
            // therefore cannot have been successfully closed and recycled.
            let live = unsafe { GetHandleInformation(raw, &mut flags) } != 0
                && flags & HANDLE_FLAG_PROTECT_FROM_CLOSE != 0;
            if !live {
                eprintln!("fixture protection lost; refusing numeric-handle cleanup");
                continue;
            }
            let retained = [&self.owner.thread, &self.owner.process, &self.owner.job]
                .into_iter()
                .any(|slot| slot.as_ref().is_some_and(|h| h.as_raw_handle() == raw));
            // SAFETY: still the same protected fixture handle; clear only our bit.
            if unsafe { SetHandleInformation(raw, HANDLE_FLAG_PROTECT_FROM_CLOSE, 0) } == 0 {
                eprintln!("could not remove owned fixture protection");
                continue;
            }
            if !retained {
                // SAFETY: owner detached this handle after its failed close;
                // protection proved it remained live. Fixture now releases it.
                if unsafe { CloseHandle(raw) } == 0 {
                    eprintln!("owned fixture final close failed");
                }
            }
        }
    }
}

#[test]
fn protected_close_failures_preserve_each_error_and_original_root_result() {
    for mask in 1..8 {
        let mut fixture = ProtectedOwner::new(mask);
        let with_owner = handle_count(); // exactly Job + process + primary thread
        let first = fixture.owner.finish(WAIT).unwrap();
        match &first {
            Settlement::CleanupFailed {
                root_exit,
                failures,
            } => {
                assert_eq!(*root_exit, 0);
                let names = ["CloseThread", "CloseProcess", "CloseJob"]
                    .into_iter()
                    .enumerate()
                    .filter_map(|(i, name)| (mask & (1 << i) != 0).then_some(name))
                    .collect::<Vec<_>>();
                assert_eq!(
                    failures.iter().map(|f| f.operation).collect::<Vec<_>>(),
                    names
                );
                assert!(failures.iter().all(|f| f.code != 0));
            }
            other => panic!("native close failures became success or timeout: {other:?}"),
        }
        assert_eq!(fixture.owner.root_exit(), Some(0));
        assert!(
            fixture.owner.thread.is_none()
                && fixture.owner.process.is_none()
                && fixture.owner.job.is_none()
        );
        assert_eq!(fixture.owner.finish(Duration::ZERO).unwrap(), first);
        for &raw in &fixture.handles {
            let mut flags = 0;
            // SAFETY: this fixture still guards the protected native handle.
            assert_ne!(unsafe { GetHandleInformation(raw, &mut flags) }, 0);
            assert_ne!(flags & HANDLE_FLAG_PROTECT_FROM_CLOSE, 0);
        }
        // The library must retain failure; only this fixture may revoke its own
        // artificial protection. A real ambiguous failed close is not retryable.
        drop(fixture);
        assert_eq!(
            handle_count(),
            with_owner - 3,
            "owned failure fixture leaked native handles"
        );
    }
}

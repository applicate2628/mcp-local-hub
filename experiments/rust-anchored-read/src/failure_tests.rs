use super::*;
use std::{cell::RefCell, collections::BTreeMap};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Role {
    Root,
    First,
    Parent,
    File,
}

// Deliberately synthetic codes: these test propagation, not OS error induction.
const METADATA_CODE: NativeCode = NativeCode::Win32(0x5a01);
fn close_code(role: Role) -> NativeCode {
    NativeCode::Win32(0x5b00 + role as i32)
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Input {
    Valid,
    WrongDigest,
    CancelAfterMetadata,
    TooSmall,
    NativeReadFailure,
}

#[derive(Default)]
struct OperationsLog {
    acquired: Vec<Role>,
    closed: Vec<Role>,
    live: BTreeMap<usize, Role>,
    max_live: usize,
    native_metadata_succeeded: Vec<Role>,
    native_close_succeeded: Vec<Role>,
    read_requests: Vec<usize>,
}

const PAYLOAD: &[u8] = b"known verified bytes";

fn exercise(
    input: Input,
    metadata_failure: Option<Role>,
    close_failures: &[Role],
) -> (ReadResult, OperationsLog) {
    let fixture = Fixture::new();
    fs::create_dir(fixture.root.join("first")).unwrap();
    fs::create_dir(fixture.root.join("first/parent")).unwrap();
    let payload = if input == Input::NativeReadFailure {
        vec![b'x'; 4097]
    } else {
        PAYLOAD.to_vec()
    };
    let data = fixture.root.join("first/parent/data");
    fs::write(&data, &payload).unwrap();
    let mut locked = (input == Input::NativeReadFailure).then(|| LockedByte {
        file: fs::File::open(&data).unwrap(),
        locked: false,
    });
    let canceled = AtomicBool::new(false);
    let log = RefCell::new(OperationsLog::default());
    let expected = if input == Input::WrongDigest {
        digest(b"different bytes")
    } else {
        digest(&payload)
    };
    let cap = if input == Input::TooSmall {
        payload.len() - 1
    } else {
        payload.len()
    };

    let result = read_with_operations(
        &fixture.root,
        &["first".into(), "parent".into(), "data".into()],
        cap,
        &expected,
        &canceled,
        ReadOperations {
            step: |step| {
                if let Step::BeforeRead { requested } = step {
                    log.borrow_mut().read_requests.push(requested);
                    if log.borrow().read_requests.len() == 2 {
                        if let Some(locked) = locked.as_mut() {
                            locked.lock().expect("lock original fixture byte");
                        }
                    }
                }
                if input == Input::CancelAfterMetadata && matches!(step, Step::BeforeRead { .. }) {
                    canceled.store(true, Ordering::Release);
                }
                Ok(())
            },
            inspect: |handle: &OwnedHandle, directory, operation| {
                let role = {
                    let mut log = log.borrow_mut();
                    let role = match log.acquired.len() {
                        0 => Role::Root,
                        1 => Role::First,
                        2 => Role::Parent,
                        3 => Role::File,
                        _ => panic!("unexpected additional acquisition"),
                    };
                    // Numeric reuse after a completed close is valid. Only two
                    // simultaneously live acquisitions may not share a handle.
                    assert!(log
                        .live
                        .insert(handle.as_raw_handle() as usize, role)
                        .is_none());
                    log.acquired.push(role);
                    log.max_live = log.max_live.max(log.live.len());
                    role
                };
                let info = inspect(handle, directory, operation)?;
                log.borrow_mut().native_metadata_succeeded.push(role);
                if metadata_failure == Some(role) {
                    Err(ReadError::new(
                        Category::UnsafeObjectOrIo,
                        operation,
                        Some(METADATA_CODE),
                    ))
                } else {
                    Ok(info)
                }
            },
            close: |handle: OwnedHandle| {
                let role = {
                    let mut log = log.borrow_mut();
                    let role = log
                        .live
                        .remove(&(handle.as_raw_handle() as usize))
                        .expect("close consumes one currently owned acquisition");
                    log.closed.push(role);
                    role
                };
                // Consume the actual OwnedHandle exactly once. A substituted
                // failure never leaves a real handle open or closes a stale
                // number: it follows a successful native close on this handle.
                close_once(handle)?;
                log.borrow_mut().native_close_succeeded.push(role);
                if close_failures.contains(&role) {
                    Err(close_code(role))
                } else {
                    Ok(())
                }
            },
        },
    );
    let unlock_result = locked.as_mut().map(LockedByte::unlock);
    drop(locked);
    if let Some(result) = unlock_result {
        result.expect("unlock original fixture byte");
    }
    (result, log.into_inner())
}

fn assert_operations(log: &OperationsLog, acquired: &[Role], closed: &[Role]) {
    assert_eq!(log.acquired.as_slice(), acquired);
    assert_eq!(log.closed.as_slice(), closed);
    assert!(
        log.live.is_empty(),
        "an acquired handle missed explicit cleanup"
    );
    assert!(
        log.max_live <= 2,
        "more than two live handles were retained"
    );
    assert_eq!(log.native_metadata_succeeded.as_slice(), acquired);
    assert_eq!(log.native_close_succeeded.as_slice(), closed);
}

fn assert_error(
    error: &ReadError,
    category: Category,
    operation: &str,
    native: Option<NativeCode>,
    cleanup: &[Role],
) {
    assert_eq!(error.category, category);
    assert_eq!(error.operation, operation);
    assert_eq!(error.native, native);
    assert_eq!(
        error.cleanup_failures,
        cleanup.iter().copied().map(close_code).collect::<Vec<_>>()
    );
}

#[test]
fn native_success_keeps_original_handles_and_verified_bytes() {
    let (result, log) = exercise(Input::Valid, None, &[]);
    assert_eq!(result.unwrap().as_slice(), PAYLOAD);
    assert_operations(
        &log,
        &[Role::Root, Role::First, Role::Parent, Role::File],
        &[Role::Root, Role::First, Role::File, Role::Parent],
    );
}

#[test]
fn metadata_failure_keeps_primary_and_each_cleanup_combination() {
    struct MetadataCase {
        target: Role,
        operation: &'static str,
        acquired: &'static [Role],
        closed_before_failure: &'static [Role],
        cleanup: &'static [Role],
    }
    let cases = [
        MetadataCase {
            target: Role::Root,
            operation: "stat-root",
            acquired: &[Role::Root],
            closed_before_failure: &[],
            cleanup: &[Role::Root],
        },
        MetadataCase {
            target: Role::First,
            operation: "stat-component",
            acquired: &[Role::Root, Role::First],
            closed_before_failure: &[],
            cleanup: &[Role::First, Role::Root],
        },
        MetadataCase {
            target: Role::Parent,
            operation: "stat-component",
            acquired: &[Role::Root, Role::First, Role::Parent],
            closed_before_failure: &[Role::Root],
            cleanup: &[Role::Parent, Role::First],
        },
        MetadataCase {
            target: Role::File,
            operation: "stat-final",
            acquired: &[Role::Root, Role::First, Role::Parent, Role::File],
            closed_before_failure: &[Role::Root, Role::First],
            cleanup: &[Role::File, Role::Parent],
        },
    ];
    for case in cases {
        // Exhaust precisely the one/two retained cleanup slots, not unrelated
        // input permutations. This distinguishes first, later and joint errors.
        for mask in 0..(1usize << case.cleanup.len()) {
            let failures: Vec<Role> = case
                .cleanup
                .iter()
                .enumerate()
                .filter_map(|(index, role)| (mask & (1 << index) != 0).then_some(*role))
                .collect();
            let (result, log) = exercise(Input::Valid, Some(case.target), &failures);
            let closed: Vec<Role> = case
                .closed_before_failure
                .iter()
                .chain(case.cleanup)
                .copied()
                .collect();
            assert_operations(&log, case.acquired, &closed);
            assert_error(
                &result.unwrap_err(),
                Category::UnsafeObjectOrIo,
                case.operation,
                Some(METADATA_CODE),
                &failures,
            );
        }
    }
}

#[test]
fn close_failure_never_returns_bytes_and_retains_later_cleanup_errors() {
    struct CloseCase {
        failures: &'static [Role],
        primary: Role,
        operation: &'static str,
        acquired: &'static [Role],
        closed: &'static [Role],
        cleanup: &'static [Role],
    }
    let cases = [
        CloseCase {
            failures: &[Role::Root],
            primary: Role::Root,
            operation: "close-parent",
            acquired: &[Role::Root, Role::First],
            closed: &[Role::Root, Role::First],
            cleanup: &[],
        },
        CloseCase {
            failures: &[Role::Root, Role::First],
            primary: Role::Root,
            operation: "close-parent",
            acquired: &[Role::Root, Role::First],
            closed: &[Role::Root, Role::First],
            cleanup: &[Role::First],
        },
        CloseCase {
            failures: &[Role::First],
            primary: Role::First,
            operation: "close-parent",
            acquired: &[Role::Root, Role::First, Role::Parent],
            closed: &[Role::Root, Role::First, Role::Parent],
            cleanup: &[],
        },
        CloseCase {
            failures: &[Role::First, Role::Parent],
            primary: Role::First,
            operation: "close-parent",
            acquired: &[Role::Root, Role::First, Role::Parent],
            closed: &[Role::Root, Role::First, Role::Parent],
            cleanup: &[Role::Parent],
        },
        CloseCase {
            failures: &[Role::File],
            primary: Role::File,
            operation: "close",
            acquired: &[Role::Root, Role::First, Role::Parent, Role::File],
            closed: &[Role::Root, Role::First, Role::File, Role::Parent],
            cleanup: &[Role::File],
        },
        CloseCase {
            failures: &[Role::Parent],
            primary: Role::Parent,
            operation: "close",
            acquired: &[Role::Root, Role::First, Role::Parent, Role::File],
            closed: &[Role::Root, Role::First, Role::File, Role::Parent],
            cleanup: &[Role::Parent],
        },
        CloseCase {
            failures: &[Role::File, Role::Parent],
            primary: Role::File,
            operation: "close",
            acquired: &[Role::Root, Role::First, Role::Parent, Role::File],
            closed: &[Role::Root, Role::First, Role::File, Role::Parent],
            cleanup: &[Role::File, Role::Parent],
        },
    ];
    for case in cases {
        let (result, log) = exercise(Input::Valid, None, case.failures);
        assert_operations(&log, case.acquired, case.closed);
        assert_error(
            &result.unwrap_err(),
            Category::UnsafeObjectOrIo,
            case.operation,
            Some(close_code(case.primary)),
            case.cleanup,
        );
    }
}

#[test]
fn non_io_primary_failures_survive_both_cleanup_failures() {
    for (input, category, operation) in [
        (
            Input::WrongDigest,
            Category::ChecksumMismatch,
            "verify-checksum",
        ),
        (
            Input::CancelAfterMetadata,
            Category::Canceled,
            "after-read-hook",
        ),
        (Input::TooSmall, Category::TooLarge, "stat-final"),
    ] {
        let failures = &[Role::File, Role::Parent];
        let (result, log) = exercise(input, None, failures);
        assert_operations(
            &log,
            &[Role::Root, Role::First, Role::Parent, Role::File],
            &[Role::Root, Role::First, Role::File, Role::Parent],
        );
        assert_error(&result.unwrap_err(), category, operation, None, failures);
    }
}

#[test]
fn native_partial_read_failure_survives_each_cleanup_failure_combination() {
    use windows_sys::Win32::Foundation::ERROR_LOCK_VIOLATION;

    // The zero-cleanup-error control already has its own native test. Only the
    // three missing combinations are exercised here. ReadFile really fails;
    // the close statuses still follow successful native closes of the originals.
    for failures in [
        &[Role::File][..],
        &[Role::Parent][..],
        &[Role::File, Role::Parent][..],
    ] {
        let (result, log) = exercise(Input::NativeReadFailure, None, failures);
        assert_eq!(log.read_requests, vec![4096, 2]);
        assert_operations(
            &log,
            &[Role::Root, Role::First, Role::Parent, Role::File],
            &[Role::Root, Role::First, Role::File, Role::Parent],
        );
        assert_error(
            &result.unwrap_err(),
            Category::UnsafeObjectOrIo,
            "read-final",
            Some(NativeCode::Win32(ERROR_LOCK_VIOLATION as i32)),
            failures,
        );
    }
}

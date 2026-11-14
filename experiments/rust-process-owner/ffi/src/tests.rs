use super::*;
use std::mem::{offset_of, MaybeUninit};

#[test]
fn layout_matches_the_compiled_c_header() {
    assert_eq!(size_of::<U16>(), 16);
    assert_eq!(size_of::<Create>(), 56);
    assert_eq!(offset_of!(Create, args), 40);
    assert_eq!(size_of::<Outcome>(), 48);
    assert_eq!(offset_of!(Outcome, cleanup_codes), 36);
}
#[test]
fn output_is_initialized_before_exposure_and_short_storage_is_untouched() {
    let mut storage = MaybeUninit::<Outcome>::uninit();
    // SAFETY: exclusive suitably aligned uninitialized storage for a full output.
    let out = unsafe { output(storage.as_mut_ptr(), 48) }.unwrap();
    assert_eq!(
        *out,
        Outcome {
            size: 48,
            version: 1,
            ..Outcome::default()
        }
    );
    let mut sentinel = [0xabab_ababu32; 12];
    let old = sentinel;
    // SAFETY: valid storage; intentionally short declared size must be rejected.
    assert_eq!(
        unsafe { mh_release1(&mut ptr::null_mut(), sentinel.as_mut_ptr().cast(), 44) },
        ARGUMENT
    );
    assert_eq!(sentinel, old);
}
#[test]
fn wrong_version_or_reserved_field_cannot_create_an_owner() {
    let empty = U16 {
        data: ptr::null(),
        len: 0,
    };
    let mut out = Outcome::default();
    for (version, reserved) in [(0, 0), (2, 0), (1, 1)] {
        let req = Create {
            version,
            reserved,
            program: empty,
            cwd: empty,
            args: ptr::null(),
            argc: 0,
        };
        let mut owner = ptr::null_mut();
        // SAFETY: complete valid request, empty input views, separate output slot.
        assert_eq!(
            unsafe { mh_create1(&req, 56, &mut owner, &mut out, 48) },
            ABI
        );
        assert!(owner.is_null());
        assert_eq!(out.state, 0);
        assert_eq!(out.native_error, 0);
    }
}
#[test]
fn structural_input_rejection_does_not_dereference_null_or_oversized_views() {
    for value in [
        U16 {
            data: ptr::null(),
            len: 1,
        },
        U16 {
            data: ptr::null(),
            len: usize::MAX,
        },
    ] {
        // SAFETY: invalid structural shape is rejected before dereference.
        assert_eq!(unsafe { copy_string(value) }.unwrap_err(), ARGUMENT);
    }
    let embedded = [65u16, 0, 66];
    // SAFETY: valid initialized view with forbidden embedded terminator.
    assert_eq!(
        unsafe {
            copy_string(U16 {
                data: embedded.as_ptr(),
                len: 3,
            })
        }
        .unwrap_err(),
        ARGUMENT
    );
    let mut out = Outcome::default();
    let mut owner = ptr::null_mut();
    // SAFETY: valid output; null request/owner is a defined argument error.
    assert_eq!(
        unsafe { mh_create1(ptr::null(), 56, &mut owner, &mut out, 48) },
        ARGUMENT
    );
    assert_eq!(
        unsafe { mh_finish1(ptr::null_mut(), 0, &mut out, 48) },
        ARGUMENT
    );
    assert_eq!(unsafe { mh_release1(&mut owner, &mut out, 48) }, OK);
    assert!(owner.is_null());
}
#[test]
fn bounded_cleanup_encoding_preserves_each_error_and_root_exit() {
    for mask in 1u32..8 {
        let failures = ["CloseThread", "CloseProcess", "CloseJob"]
            .iter()
            .enumerate()
            .filter(|(i, _)| mask & (1 << i) != 0)
            .map(|(i, op)| mcphub_process_owner_spike::NativeFailure {
                operation: op,
                code: 5 + i as i32,
            })
            .collect();
        let mut out = Outcome::default();
        encode_settlement(
            Settlement::CleanupFailed {
                root_exit: 259,
                failures,
            },
            &mut out,
        );
        assert_eq!(out.state, CLEANUP_FAILED);
        assert_eq!(out.root_exit, 259);
        assert_eq!(out.flags, ROOT_KNOWN);
        assert_eq!(out.cleanup_mask, mask);
        for i in 0..3 {
            assert_eq!(
                out.cleanup_codes[i],
                if mask & (1 << i) != 0 {
                    5 + i as i32
                } else {
                    0
                }
            );
        }
    }
}
#[test]
fn pending_and_error_encodings_do_not_fabricate_success() {
    let mut out = Outcome::default();
    encode_settlement(
        Settlement::Pending {
            root_exit: Some(7),
            active_processes: Some(1),
        },
        &mut out,
    );
    assert_eq!(out.state, PENDING);
    assert_eq!(out.flags, ROOT_KNOWN | ACTIVE_KNOWN);
    assert_eq!(out.root_exit, 7);
    assert_eq!(out.active, 1);
    assert_eq!(failure(&mut out, io::Error::from_raw_os_error(87)), NATIVE);
    assert_eq!(out.native_error, 87);
    assert_eq!(out.state, PENDING);
    let mut denied_out = Outcome::default();
    assert_eq!(
        failure(&mut denied_out, io::ErrorKind::PermissionDenied.into()),
        DENIED
    );
    assert_eq!(denied_out.native_error, 0);
}

#[test]
fn native_invalid_parameter_code_survives_create_classification() {
    let mut out = Outcome::default();
    assert_eq!(
        create_failure(&mut out, io::Error::from_raw_os_error(87)),
        NATIVE
    );
    assert_eq!(out.native_error, 87);
    let mut local = Outcome::default();
    assert_eq!(
        create_failure(&mut local, io::ErrorKind::InvalidInput.into()),
        ARGUMENT
    );
    assert_eq!(local.native_error, 0);
}

#[test]
fn environment_layout_and_empty_view_match_the_c_contract() {
    assert_eq!(size_of::<EnvironmentEntry>(), 32);
    assert_eq!(offset_of!(EnvironmentEntry, value), 16);
    assert_eq!(size_of::<Environment>(), 16);
    assert_eq!(offset_of!(Environment, count), 8);
    // SAFETY: count zero deliberately needs no array; empty never dereferences.
    let copied = unsafe {
        copy_environment(&Environment {
            entries: ptr::null(),
            count: 0,
        })
    }
    .unwrap();
    assert!(copied.is_empty());
}

#[test]
fn environment_bounds_reject_before_dereferencing_entry_strings() {
    // SAFETY: each structural error is rejected before touching the absent data.
    for view in [
        Environment {
            entries: ptr::null(),
            count: 1,
        },
        Environment {
            entries: ptr::null(),
            count: 257,
        },
    ] {
        assert_eq!(unsafe { copy_environment(&view) }.unwrap_err(), ARGUMENT);
    }
    let overflow = EnvironmentEntry {
        name: U16 {
            data: ptr::null(),
            len: usize::MAX,
        },
        value: U16 {
            data: ptr::null(),
            len: 1,
        },
    };
    // SAFETY: valid initialized entry array; size arithmetic rejects before strings.
    assert_eq!(
        unsafe {
            copy_environment(&Environment {
                entries: &overflow,
                count: 1,
            })
        }
        .unwrap_err(),
        ARGUMENT
    );
    let too_large = EnvironmentEntry {
        name: U16 {
            data: ptr::null(),
            len: 1,
        },
        value: U16 {
            data: ptr::null(),
            len: MAX_UNITS - 3,
        },
    };
    // 1 final NUL + name1 + value(MAX-3) + '='/'NUL'2 = MAX+1.
    assert_eq!(
        unsafe {
            copy_environment(&Environment {
                entries: &too_large,
                count: 1,
            })
        }
        .unwrap_err(),
        ARGUMENT
    );
}

#[test]
fn environment_values_are_owned_and_preserve_raw_windows_units() {
    let name = [65u16];
    let mut value = [0xd800u16, 61, 66];
    let entry = EnvironmentEntry {
        name: U16 {
            data: name.as_ptr(),
            len: 1,
        },
        value: U16 {
            data: value.as_ptr(),
            len: 3,
        },
    };
    // SAFETY: immutable valid input through copy only; afterwards caller may mutate.
    let owned = unsafe {
        copy_environment(&Environment {
            entries: &entry,
            count: 1,
        })
    }
    .unwrap();
    value[0] = 88;
    assert_eq!(value[0], 88, "caller mutation must be real");
    assert_eq!(
        owned[0].1.encode_wide().collect::<Vec<_>>(),
        vec![0xd800, 61, 66]
    );
    let bad = [65u16, 0];
    let entry = EnvironmentEntry {
        name: U16 {
            data: name.as_ptr(),
            len: 1,
        },
        value: U16 {
            data: bad.as_ptr(),
            len: 2,
        },
    };
    assert_eq!(
        unsafe {
            copy_environment(&Environment {
                entries: &entry,
                count: 1,
            })
        }
        .unwrap_err(),
        ARGUMENT
    );
}

#[test]
fn extended_environment_request_rejects_wrong_size_without_creation() {
    let program = std::ffi::OsStr::new(r"D:\owned\fixture.exe")
        .encode_wide()
        .collect::<Vec<_>>();
    let cwd = std::ffi::OsStr::new(r"D:\owned")
        .encode_wide()
        .collect::<Vec<_>>();
    let req = Create {
        version: 1,
        reserved: 0,
        program: U16 {
            data: program.as_ptr(),
            len: program.len(),
        },
        cwd: U16 {
            data: cwd.as_ptr(),
            len: cwd.len(),
        },
        args: ptr::null(),
        argc: 0,
    };
    let env = Environment {
        entries: ptr::null(),
        count: 0,
    };
    let mut owner = ptr::null_mut();
    let mut out = Outcome::default();
    // SAFETY: real request/output allocations; only declared environment size is wrong.
    assert_eq!(
        unsafe { mh_create_env1(&req, 56, &env, 15, &mut owner, &mut out, 48) },
        ABI
    );
    assert!(owner.is_null());
    assert_eq!(out.pid, 0);
    // SAFETY: null environment is structurally rejected with no native acquisition.
    assert_eq!(
        unsafe { mh_create_env1(&req, 56, ptr::null(), 16, &mut owner, &mut out, 48) },
        ARGUMENT
    );
    assert!(owner.is_null());
    assert_eq!(out.pid, 0);
}

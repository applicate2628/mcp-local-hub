use super::*;
use crate::read_verified;
use std::{
    fs::{self, OpenOptions},
    io::Write,
    os::windows::fs::{symlink_dir, symlink_file},
    path::PathBuf,
    sync::atomic::AtomicU64,
};
static NEXT: AtomicU64 = AtomicU64::new(1);
struct Fixture {
    root: PathBuf,
}
impl Fixture {
    fn new() -> Self {
        let base =
            PathBuf::from(std::env::var_os("MCPHUB_RX04_FIXTURES").expect("owned fixture root"));
        let root = base.join(format!(
            "read-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        fs::create_dir(&root).unwrap();
        Self { root }
    }
    fn file(&self, name: &str, data: &[u8]) {
        fs::write(self.root.join(name), data).unwrap()
    }
}
// Fixtures remain available for evidence and trash-first disposition.
fn digest(value: &[u8]) -> [u8; 32] {
    Sha256::digest(value).into()
}
fn check_category(result: ReadResult, want: Category) {
    assert_eq!(result.unwrap_err().category, want)
}
fn read(f: &Fixture, names: &[&str], cap: usize, expected: &[u8]) -> ReadResult {
    read_verified(
        &f.root,
        &names.iter().map(OsString::from).collect::<Vec<_>>(),
        cap,
        &digest(expected),
        &AtomicBool::new(false),
    )
}

#[test]
fn reads_exact_verified_bytes_empty_and_unicode() {
    let f = Fixture::new();
    fs::create_dir(f.root.join("nested λ")).unwrap();
    for (name, data) in [
        ("empty", b"".as_slice()),
        ("данные.json", b"known verified bytes".as_slice()),
    ] {
        fs::write(f.root.join("nested λ").join(name), data).unwrap();
        let bytes = read(&f, &["nested λ", name], 1024, data).unwrap();
        assert_eq!(bytes.as_slice(), data)
    }
}
#[test]
fn rejects_invalid_input_before_open() {
    let f = Fixture::new();
    for name in ["", ".", "..", "a/b", "a\\b", "nul\0suffix"] {
        check_category(read(&f, &[name], 32, b""), Category::InvalidInput)
    }
    check_category(read(&f, &[], 32, b""), Category::InvalidInput);
    check_category(read(&f, &["missing"], 0, b""), Category::InvalidInput);
    check_category(
        read(&f, &["missing"], usize::MAX, b""),
        Category::InvalidInput,
    );
    check_category(
        read_verified(
            Path::new("relative"),
            &["data".into()],
            32,
            &digest(b""),
            &AtomicBool::new(false),
        ),
        Category::InvalidInput,
    )
}
#[test]
fn canceled_before_open_has_typed_result() {
    let f = Fixture::new();
    let canceled = AtomicBool::new(true);
    check_category(
        read_verified(
            &f.root.join("missing-root"),
            &["missing".into()],
            1024,
            &digest(b""),
            &canceled,
        ),
        Category::Canceled,
    )
}
#[test]
fn checksum_mismatch_and_initial_size_limit_return_no_data() {
    let f = Fixture::new();
    f.file("data", b"payload");
    check_category(read(&f, &["data"], 8, b"other"), Category::ChecksumMismatch);
    check_category(read(&f, &["data"], 6, b"payload"), Category::TooLarge)
}
#[test]
fn final_directory_and_missing_file_preserve_native_failure() {
    let f = Fixture::new();
    fs::create_dir(f.root.join("dir")).unwrap();
    check_category(read(&f, &["dir"], 32, b""), Category::UnsafeObjectOrIo);
    let error = read(&f, &["missing"], 32, b"").unwrap_err();
    assert_eq!(error.category, Category::UnsafeObjectOrIo);
    assert!(matches!(error.native, Some(NativeCode::Nt(_))));
    assert_eq!(error.operation, "open-final")
}
#[test]
fn root_and_intermediate_and_final_reparse_are_rejected() {
    let f = Fixture::new();
    let outside = Fixture::new();
    outside.file("data", b"outside");
    symlink_dir(&outside.root, f.root.join("root-link"))
        .expect("native symlink privilege must be present; do not elevate");
    check_category(
        read_verified(
            &f.root.join("root-link"),
            &["data".into()],
            32,
            &digest(b"outside"),
            &AtomicBool::new(false),
        ),
        Category::UnsafeObjectOrIo,
    );
    check_category(
        read(&f, &["root-link", "data"], 32, b"outside"),
        Category::UnsafeObjectOrIo,
    );
    symlink_file(outside.root.join("data"), f.root.join("file-link"))
        .expect("native symlink capability");
    check_category(
        read(&f, &["file-link"], 32, b"outside"),
        Category::UnsafeObjectOrIo,
    )
}
#[test]
fn component_swapped_to_link_before_open_is_rejected() {
    let f = Fixture::new();
    let outside = Fixture::new();
    fs::create_dir(f.root.join("pins")).unwrap();
    outside.file("data", b"outside");
    let mut swapped = false;
    let result = read_with_hook(
        &f.root,
        &["pins".into(), "data".into()],
        32,
        &digest(b"outside"),
        &AtomicBool::new(false),
        |event| {
            if matches!(event, Step::BeforeOpen(0)) && !swapped {
                fs::rename(f.root.join("pins"), f.root.join("pins-old")).unwrap();
                symlink_dir(&outside.root, f.root.join("pins")).unwrap();
                swapped = true
            }
            Ok(())
        },
    );
    assert!(swapped);
    check_category(result, Category::UnsafeObjectOrIo)
}
#[test]
fn retained_parent_does_not_reopen_changed_path() {
    let f = Fixture::new();
    fs::create_dir(f.root.join("pins")).unwrap();
    fs::write(f.root.join("pins/data"), b"original").unwrap();
    let mut swapped = false;
    let result = read_with_hook(
        &f.root,
        &["pins".into(), "data".into()],
        32,
        &digest(b"original"),
        &AtomicBool::new(false),
        |event| {
            if matches!(event, Step::BeforeOpen(1)) && !swapped {
                fs::rename(f.root.join("pins"), f.root.join("pins-old")).unwrap();
                fs::create_dir(f.root.join("pins")).unwrap();
                fs::write(f.root.join("pins/data"), b"replaced").unwrap();
                swapped = true
            }
            Ok(())
        },
    )
    .unwrap();
    assert!(swapped);
    assert_eq!(result.as_slice(), b"original")
}
#[test]
fn retained_final_handle_reads_original_after_path_replacement() {
    let f = Fixture::new();
    f.file("data", b"original");
    let mut replaced = false;
    let result = read_with_hook(
        &f.root,
        &["data".into()],
        32,
        &digest(b"original"),
        &AtomicBool::new(false),
        |event| {
            if matches!(event, Step::BeforeRead { .. }) && !replaced {
                fs::rename(f.root.join("data"), f.root.join("old-data")).unwrap();
                f.file("data", b"replaced");
                replaced = true
            }
            Ok(())
        },
    )
    .unwrap();
    assert!(replaced);
    assert_eq!(result.as_slice(), b"original")
}
#[test]
fn growth_after_stat_hits_one_byte_sentinel_without_returning_data() {
    let f = Fixture::new();
    let payload = vec![b'x'; 4096];
    f.file("data", &payload);
    let mut grew = false;
    let mut requests = Vec::new();
    let result = read_with_hook(
        &f.root,
        &["data".into()],
        4096,
        &digest(&payload),
        &AtomicBool::new(false),
        |event| {
            if let Step::BeforeRead { requested } = event {
                requests.push(requested);
                if !grew {
                    OpenOptions::new()
                        .append(true)
                        .open(f.root.join("data"))
                        .unwrap()
                        .write_all(b"y")
                        .unwrap();
                    grew = true
                }
            }
            Ok(())
        },
    );
    assert!(grew);
    assert_eq!(requests, vec![4096, 1]);
    check_category(result, Category::TooLarge)
}
#[test]
fn mid_read_cancellation_returns_no_partial_result() {
    let f = Fixture::new();
    let payload = vec![b'x'; 8192];
    f.file("data", &payload);
    let canceled = AtomicBool::new(false);
    let mut reads = 0;
    let result = read_with_hook(
        &f.root,
        &["data".into()],
        8192,
        &digest(&payload),
        &canceled,
        |event| {
            if matches!(event, Step::BeforeRead { .. }) {
                reads += 1;
                if reads == 2 {
                    canceled.store(true, Ordering::Release)
                }
            }
            Ok(())
        },
    );
    assert_eq!(reads, 2);
    check_category(result, Category::Canceled)
}
#[test]
fn injected_native_phase_error_preserves_category_and_closes_handles() {
    let f = Fixture::new();
    f.file("data", b"test");
    let result = read_with_hook(
        &f.root,
        &["data".into()],
        32,
        &digest(b"test"),
        &AtomicBool::new(false),
        |_| {
            Err(ReadError::new(
                Category::UnsafeObjectOrIo,
                "injected",
                Some(NativeCode::Win32(123)),
            ))
        },
    );
    let err = result.unwrap_err();
    assert_eq!(err.operation, "injected");
    assert_eq!(err.native, Some(NativeCode::Win32(123)));
    assert!(err.cleanup_failures.is_empty())
}
#[test]
fn native_read_failure_after_partial_read_preserves_error_and_releases_lock() {
    use windows_sys::Win32::{
        Foundation::ERROR_LOCK_VIOLATION,
        Storage::FileSystem::{LockFile, UnlockFile},
    };

    // This guard owns only the fixture's independent handle and one byte lock.
    // File closes the handle on every path, including a panic during the hook.
    struct LockedByte {
        file: fs::File,
        locked: bool,
    }
    impl LockedByte {
        fn unlock(&mut self) -> std::io::Result<()> {
            if self.locked {
                // SAFETY: the same live fixture handle and exact locked range.
                if unsafe { UnlockFile(self.file.as_raw_handle(), 4096, 0, 1, 0) } == 0 {
                    return Err(std::io::Error::last_os_error());
                }
                self.locked = false;
            }
            Ok(())
        }
    }
    impl Drop for LockedByte {
        fn drop(&mut self) {
            // Never replace the reader's result or a panic with a cleanup panic.
            // Closing the owned File also releases any remaining byte-range lock.
            let _ = self.unlock();
        }
    }

    let f = Fixture::new();
    let payload = vec![b'x'; 4097];
    f.file("data", &payload);
    let mut locked = LockedByte {
        file: fs::File::open(f.root.join("data")).unwrap(),
        locked: false,
    };
    let mut requests = Vec::new();
    let result = read_with_hook(
        &f.root,
        &["data".into()],
        payload.len(),
        &digest(&payload),
        &AtomicBool::new(false),
        |event| {
            if let Step::BeforeRead { requested } = event {
                requests.push(requested);
                if requests.len() == 2 {
                    // LockFile is nonblocking. Its exclusive byte lock denies
                    // the reader's separate handle even in this same process.
                    // https://learn.microsoft.com/windows/win32/api/fileapi/nf-fileapi-lockfile
                    // SAFETY: live read-capable fixture handle, fixed valid range.
                    let ok = unsafe { LockFile(locked.file.as_raw_handle(), 4096, 0, 1, 0) };
                    locked.locked = ok != 0;
                    assert_ne!(
                        ok,
                        0,
                        "lock fixture byte: {}",
                        std::io::Error::last_os_error()
                    );
                }
            }
            Ok(())
        },
    );
    // Restore the fixture before any result assertion; preserve both outcomes.
    let unlock_result = locked.unlock();
    drop(locked);
    let err = result.unwrap_err();
    // The second request proves 4096 bytes were already buffered: 4097-4096+1.
    assert_eq!(requests, vec![4096, 2]);
    assert_eq!(err.category, Category::UnsafeObjectOrIo);
    assert_eq!(err.operation, "read-final");
    assert_eq!(
        err.native,
        Some(NativeCode::Win32(ERROR_LOCK_VIOLATION as i32))
    );
    assert!(err.cleanup_failures.is_empty());
    unlock_result.expect("unlock fixture byte");
    assert_eq!(
        read(&f, &["data"], payload.len(), &payload)
            .unwrap()
            .as_slice(),
        payload
    );
}
#[test]
fn repeated_success_and_failure_do_not_leak_handles() {
    use windows_sys::Win32::System::Threading::{GetCurrentProcess, GetProcessHandleCount};
    fn handles() -> u32 {
        let mut n = 0; /* SAFETY: valid pseudo-handle and writable count. */
        assert_ne!(
            unsafe { GetProcessHandleCount(GetCurrentProcess(), &mut n) },
            0
        );
        n
    }
    let f = Fixture::new();
    f.file("data", b"test");
    drop(read(&f, &["data"], 32, b"test").unwrap());
    let before = handles();
    for _ in 0..32 {
        drop(read(&f, &["data"], 32, b"test").unwrap());
        check_category(read(&f, &["data"], 32, b"bad"), Category::ChecksumMismatch);
        check_category(
            read(&f, &["missing"], 32, b"test"),
            Category::UnsafeObjectOrIo,
        );
        check_category(read(&f, &["data"], 1, b"test"), Category::TooLarge)
    }
    assert_eq!(
        handles(),
        before,
        "native read handles grew across settled calls"
    )
}

#[test]
fn root_ancestor_reparse_is_not_silently_followed() {
    let f = Fixture::new();
    let outside = Fixture::new();
    fs::create_dir(outside.root.join("nested")).unwrap();
    fs::write(outside.root.join("nested/data"), b"outside").unwrap();
    symlink_dir(&outside.root, f.root.join("linked")).expect("native symlink capability");
    let result = read_verified(
        &f.root.join("linked/nested"),
        &["data".into()],
        32,
        &digest(b"outside"),
        &AtomicBool::new(false),
    );
    check_category(result, Category::UnsafeObjectOrIo);
}

#[test]
fn exact_cap_still_checks_eof_and_returns_complete_payload() {
    let f = Fixture::new();
    let payload = vec![b'x'; 4096];
    f.file("data", &payload);
    let mut requests = Vec::new();
    let result = read_with_hook(
        &f.root,
        &["data".into()],
        4096,
        &digest(&payload),
        &AtomicBool::new(false),
        |event| {
            if let Step::BeforeRead { requested } = event {
                requests.push(requested)
            }
            Ok(())
        },
    )
    .unwrap();
    assert_eq!(requests, vec![4096, 1]);
    assert_eq!(result.as_slice(), payload);
}

#[path = "failure_tests.rs"]
mod failures;

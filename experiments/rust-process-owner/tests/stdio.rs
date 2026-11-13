#![cfg(windows)]
use mcphub_process_owner_spike::{BorrowedStdio, NativeOwner, Settlement};
use std::{
    ffi::OsString,
    fs::{self, File},
    io::{self, Read, Write},
    os::windows::io::{AsHandle, AsRawHandle, BorrowedHandle, FromRawHandle, OwnedHandle},
    path::{Path, PathBuf},
    ptr::{null, null_mut},
    sync::atomic::{AtomicU64, Ordering},
    thread,
    time::{Duration, Instant},
};
use windows_sys::Win32::{
    Foundation::{
        GetHandleInformation, SetHandleInformation, ERROR_BROKEN_PIPE, HANDLE_FLAG_INHERIT,
        WAIT_TIMEOUT,
    },
    Security::SECURITY_ATTRIBUTES,
    System::{
        Pipes::{CreatePipe, PeekNamedPipe},
        Threading::{CreateEventW, GetCurrentProcess, GetProcessHandleCount, WaitForSingleObject},
    },
};

const DEADLINE: Duration = Duration::from_secs(3);
const BYTES: &[u8] = b"\0MCP\r\n\xff\x80binary\n";
const STDERR: &[u8] = b"native-stderr\0\xff";
static NEXT: AtomicU64 = AtomicU64::new(1);

struct Fixture {
    root: PathBuf,
    input: File,
    output: File,
    error: File,
}
impl Fixture {
    fn new() -> Self {
        let root =
            PathBuf::from(std::env::var_os("MCPHUB_RX02_FIXTURES").expect("owned fixture root"))
                .join(format!(
                    "stdio-{}-{}",
                    std::process::id(),
                    NEXT.fetch_add(1, Ordering::Relaxed)
                ));
        fs::create_dir(&root).unwrap();
        fs::write(root.join("input"), BYTES).unwrap();
        let input = File::open(root.join("input")).unwrap();
        let output = File::create(root.join("output")).unwrap();
        let error = File::create(root.join("error")).unwrap();
        for file in [&input, &output, &error] {
            inheritable(file.as_handle(), true);
        }
        Self {
            root,
            input,
            output,
            error,
        }
    }
    fn args(&self, mode: &str) -> Vec<OsString> {
        vec![mode.into(), self.root.join("marker").into_os_string()]
    }
    fn stdio(&self) -> BorrowedStdio<'_> {
        BorrowedStdio {
            stdin: self.input.as_handle(),
            stdout: self.output.as_handle(),
            stderr: self.error.as_handle(),
        }
    }
    fn launch(&self) -> NativeOwner {
        NativeOwner::create_suspended_with_stdio(child(), &self.args("stdio"), self.stdio())
            .unwrap()
    }
    fn flags(&self) -> [u32; 3] {
        [
            flags(self.input.as_handle()),
            flags(self.output.as_handle()),
            flags(self.error.as_handle()),
        ]
    }
}
fn child() -> &'static Path {
    Path::new(env!("CARGO_BIN_EXE_owner_child"))
}
fn flags(handle: BorrowedHandle<'_>) -> u32 {
    let mut flags = 0;
    // SAFETY: the test keeps the borrowed owner open; output is writable.
    assert_ne!(
        unsafe { GetHandleInformation(handle.as_raw_handle(), &mut flags) },
        0
    );
    flags
}
fn inheritable(handle: BorrowedHandle<'_>, yes: bool) {
    // SAFETY: only this fixture's handle is changed before concurrent use.
    assert_ne!(
        unsafe {
            SetHandleInformation(
                handle.as_raw_handle(),
                HANDLE_FLAG_INHERIT,
                if yes { HANDLE_FLAG_INHERIT } else { 0 },
            )
        },
        0
    );
}
fn handles() -> u32 {
    let mut count = 0;
    // SAFETY: current-process pseudo handle and writable count.
    assert_ne!(
        unsafe { GetProcessHandleCount(GetCurrentProcess(), &mut count) },
        0
    );
    count
}
fn pipe() -> (File, File) {
    let mut read = null_mut();
    let mut write = null_mut();
    let security = SECURITY_ATTRIBUTES {
        nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
        lpSecurityDescriptor: null_mut(),
        bInheritHandle: 1,
    };
    // SAFETY: valid POD security and exclusive output handle slots.
    assert_ne!(
        unsafe { CreatePipe(&mut read, &mut write, &security, 4096) },
        0
    );
    // SAFETY: successful CreatePipe transfers two distinct owned handles.
    unsafe { (File::from_raw_handle(read), File::from_raw_handle(write)) }
}
fn wait_marker(path: &Path) {
    let until = Instant::now() + DEADLINE;
    while !path.exists() {
        assert!(Instant::now() < until, "child readiness missing");
        thread::sleep(Duration::from_millis(2));
    }
}
fn finish(owner: &mut NativeOwner) {
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 0 }
    );
}

#[test]
fn exact_stdio_bytes_and_parent_ownership_survive_settlement() {
    let mut f = Fixture::new();
    let before = f.flags();
    let mut owner = f.launch();
    assert_eq!(owner.active_processes().unwrap(), 1);
    assert!(!f.root.join("marker").exists());
    assert_eq!(fs::metadata(f.root.join("output")).unwrap().len(), 0);
    owner.admit_and_resume(|_| Ok(())).unwrap();
    finish(&mut owner);
    assert_eq!(fs::read(f.root.join("output")).unwrap(), BYTES);
    assert_eq!(fs::read(f.root.join("error")).unwrap(), STDERR);
    assert_eq!(f.flags(), before);
    f.output.write_all(b"parent-still-owns-output").unwrap();
    f.error.write_all(b"parent-still-owns-error").unwrap();
    assert_eq!(f.flags(), before);
}

#[test]
fn stdout_and_stderr_may_share_one_borrowed_handle() {
    let f = Fixture::new();
    let io = BorrowedStdio {
        stdin: f.input.as_handle(),
        stdout: f.output.as_handle(),
        stderr: f.output.as_handle(),
    };
    let mut owner =
        NativeOwner::create_suspended_with_stdio(child(), &f.args("stdio"), io).unwrap();
    owner.admit_and_resume(|_| Ok(())).unwrap();
    finish(&mut owner);
    let mut expected = BYTES.to_vec();
    expected.extend_from_slice(STDERR);
    assert_eq!(fs::read(f.root.join("output")).unwrap(), expected);
    assert_eq!(fs::metadata(f.root.join("error")).unwrap().len(), 0);
}

#[test]
fn child_keeps_os_copies_after_parent_releases_its_files() {
    let f = Fixture::new();
    let root = f.root.clone();
    let mut owner = f.launch();
    drop(f); // borrow ends after synchronous CreateProcess; child owns its copies
    owner.admit_and_resume(|_| Ok(())).unwrap();
    finish(&mut owner);
    assert_eq!(fs::read(root.join("output")).unwrap(), BYTES);
    assert_eq!(fs::read(root.join("error")).unwrap(), STDERR);
}

#[test]
fn denied_admission_does_not_consume_or_close_borrowed_streams() {
    let f = Fixture::new();
    let before = f.flags();
    let mut owner = f.launch();
    let err = owner
        .admit_and_resume(|_| {
            Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "fixture denies",
            ))
        })
        .unwrap_err();
    assert_eq!(err.kind(), io::ErrorKind::PermissionDenied);
    owner.terminate(73).unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 73 }
    );
    assert!(!f.root.join("marker").exists());
    assert_eq!(fs::metadata(f.root.join("output")).unwrap().len(), 0);
    assert_eq!(f.flags(), before);
}

#[test]
fn noninheritable_stdio_is_rejected_without_changing_flags() {
    for slot in 0..3 {
        let f = Fixture::new();
        let files = [&f.input, &f.output, &f.error];
        inheritable(files[slot].as_handle(), false);
        let before = f.flags();
        let result = NativeOwner::create_suspended_with_stdio(child(), &f.args("stdio"), f.stdio());
        assert!(
            matches!(result, Err(ref e) if e.kind() == io::ErrorKind::InvalidInput),
            "slot {slot} must be rejected before create"
        );
        assert_eq!(f.flags(), before);
        assert!(!f.root.join("marker").exists());
    }
}

#[test]
fn missing_image_preserves_borrowed_handles() {
    let f = Fixture::new();
    let before = f.flags();
    // A first failed CreateProcess can change process-global native bookkeeping;
    // the existing no-stdio path showed the same one-time rise. Do not label
    // that as per-attempt leakage: verify the borrowed sources on the first
    // attempt and stable count across repeated failures after it.
    assert!(
        NativeOwner::create_suspended_with_stdio(&f.root.join("absent.exe"), &[], f.stdio())
            .is_err()
    );
    assert_eq!(f.flags(), before);
    let count = handles();
    for _ in 0..24 {
        assert!(NativeOwner::create_suspended_with_stdio(
            &f.root.join("absent.exe"),
            &[],
            f.stdio()
        )
        .is_err());
        assert_eq!(f.flags(), before);
        assert_eq!(handles(), count);
    }
}

#[test]
fn unrelated_inheritable_event_does_not_cross_create_boundary() {
    let f = Fixture::new();
    let security = SECURITY_ATTRIBUTES {
        nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
        lpSecurityDescriptor: null_mut(),
        bInheritHandle: 1,
    };
    // SAFETY: unnamed private manual-reset event; its handle is retained until child exit.
    let raw = unsafe { CreateEventW(&security, 1, 0, null()) };
    assert!(!raw.is_null());
    // SAFETY: the successful native create transfers ownership exactly once.
    let extra = unsafe { OwnedHandle::from_raw_handle(raw) };
    let mut args = f.args("stdio-probe");
    args.push((raw as usize).to_string().into());
    let mut owner = NativeOwner::create_suspended_with_stdio(child(), &args, f.stdio()).unwrap();
    owner.admit_and_resume(|_| Ok(())).unwrap();
    finish(&mut owner);
    // The actual event, not merely a reused numeric handle, must remain unsignaled.
    assert_eq!(
        unsafe { WaitForSingleObject(extra.as_raw_handle(), 0) },
        WAIT_TIMEOUT,
        "foreign inheritable capability leaked"
    );
    assert_eq!(fs::read(f.root.join("output")).unwrap(), BYTES);
    assert_eq!(
        flags(extra.as_handle()) & HANDLE_FLAG_INHERIT,
        HANDLE_FLAG_INHERIT
    );
}

#[test]
fn pipe_eof_and_remaining_bytes_are_not_held_by_parent_owner() {
    let f = Fixture::new();
    let (child_input, mut parent_write) = pipe();
    let (mut parent_read, child_output) = pipe();
    inheritable(parent_write.as_handle(), false);
    inheritable(parent_read.as_handle(), false);
    let io = BorrowedStdio {
        stdin: child_input.as_handle(),
        stdout: child_output.as_handle(),
        stderr: f.error.as_handle(),
    };
    let mut owner =
        NativeOwner::create_suspended_with_stdio(child(), &f.args("stdio"), io).unwrap();
    drop(child_input);
    drop(child_output);
    owner.admit_and_resume(|_| Ok(())).unwrap();
    wait_marker(&f.root.join("marker"));
    parent_write.write_all(BYTES).unwrap();
    assert!(
        matches!(
            owner.finish(Duration::from_millis(20)).unwrap(),
            Settlement::Pending {
                root_exit: None,
                ..
            }
        ),
        "EOF must wait for the actual writer owner"
    );
    drop(parent_write);
    finish(&mut owner);
    let mut available = 0;
    // SAFETY: owned read end, no competing reader, only availability is requested.
    assert_ne!(
        unsafe {
            PeekNamedPipe(
                parent_read.as_raw_handle(),
                null_mut(),
                0,
                null_mut(),
                &mut available,
                null_mut(),
            )
        },
        0
    );
    assert_eq!(available as usize, BYTES.len());
    let mut result = vec![0; BYTES.len()];
    parent_read.read_exact(&mut result).unwrap();
    assert_eq!(result, BYTES);
    // Never perform an unbounded read_to_end in a regression: an extra parent-side
    // writer would suppress EOF. Peek detects that error without hanging the test.
    assert_eq!(
        unsafe {
            PeekNamedPipe(
                parent_read.as_raw_handle(),
                null_mut(),
                0,
                null_mut(),
                &mut available,
                null_mut(),
            )
        },
        0
    );
    assert_eq!(
        io::Error::last_os_error().raw_os_error(),
        Some(ERROR_BROKEN_PIPE as i32)
    );
}

#[test]
fn repeated_stdio_lifetimes_do_not_accumulate_handles() {
    let baseline = handles();
    for _ in 0..16 {
        let f = Fixture::new();
        let mut owner = f.launch();
        owner.admit_and_resume(|_| Ok(())).unwrap();
        finish(&mut owner);
    }
    assert_eq!(handles(), baseline);
}

#[test]
fn pseudo_null_and_nonfile_sources_are_rejected_before_create() {
    let f = Fixture::new();
    let before = f.flags();
    let security = SECURITY_ATTRIBUTES {
        nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
        lpSecurityDescriptor: null_mut(),
        bInheritHandle: 1,
    };
    // SAFETY: private event returned as a unique live owned handle.
    let raw = unsafe { CreateEventW(&security, 1, 0, null()) };
    assert!(!raw.is_null());
    let event = unsafe { OwnedHandle::from_raw_handle(raw) };
    // SAFETY: BorrowedHandle explicitly admits NULL and pseudo process handles;
    // neither will be owned or closed here. The public stdio contract rejects them.
    let pseudo = unsafe { BorrowedHandle::borrow_raw(GetCurrentProcess()) };
    let null_handle = unsafe { BorrowedHandle::borrow_raw(null_mut()) };
    for input in [pseudo, null_handle, event.as_handle()] {
        let io = BorrowedStdio {
            stdin: input,
            stdout: f.output.as_handle(),
            stderr: f.error.as_handle(),
        };
        assert!(NativeOwner::create_suspended_with_stdio(child(), &f.args("stdio"), io).is_err());
        assert_eq!(f.flags(), before);
        assert!(!f.root.join("marker").exists());
    }
    assert_eq!(
        flags(event.as_handle()) & HANDLE_FLAG_INHERIT,
        HANDLE_FLAG_INHERIT
    );
}

#[test]
fn explicit_context_composes_with_borrowed_stdio_owner() {
    let f = Fixture::new();
    let before = f.flags();
    let mut owner = NativeOwner::create_suspended_with_context(
        child(),
        &f.args("stdio"),
        mcphub_process_owner_spike::LaunchContext {
            current_dir: &f.root,
            environment: &[],
        },
        Some(f.stdio()),
    )
    .unwrap();
    assert!(!f.root.join("marker").exists());
    owner.admit_and_resume(|_| Ok(())).unwrap();
    finish(&mut owner);
    assert_eq!(fs::read(f.root.join("output")).unwrap(), BYTES);
    assert_eq!(
        fs::read(f.root.join("error")).unwrap(),
        b"native-stderr\0\xff"
    );
    assert_eq!(f.flags(), before);
}

#![cfg(windows)]
//! Qualification of the existing Job lifetime, not a new process backend.
use std::{
    ffi::OsString,
    fs, io,
    os::windows::{
        ffi::OsStringExt,
        io::{AsRawHandle, FromRawHandle, OwnedHandle},
        process::CommandExt,
    },
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    sync::atomic::{AtomicU64, Ordering},
    thread,
    time::{Duration, Instant},
};
use windows_sys::Win32::{
    Foundation::{FILETIME, WAIT_OBJECT_0, WAIT_TIMEOUT},
    System::Threading::{
        GetExitCodeProcess, GetProcessTimes, OpenProcess, QueryFullProcessImageNameW,
        TerminateProcess, WaitForSingleObject, PROCESS_QUERY_LIMITED_INFORMATION,
        PROCESS_SYNCHRONIZE, PROCESS_TERMINATE,
    },
};
const WAIT: Duration = Duration::from_secs(4);
static NEXT: AtomicU64 = AtomicU64::new(1);
fn fixture() -> &'static Path {
    Path::new(env!("CARGO_BIN_EXE_owner_crash_fixture"))
}

// Only directly created fixture children are controlled through std::Child.
// Observation never retains a Job handle: that would prevent last-handle-close.
struct Created(Child);
impl Created {
    fn launch(role: &str, dir: &Path, phase: &str) -> Self {
        let stderr = fs::File::create(dir.join(format!("{role}.stderr"))).unwrap();
        let child = Command::new(fixture())
            .arg(role)
            .arg(dir)
            .arg(phase)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(stderr)
            .creation_flags(0x08000000)
            .spawn()
            .unwrap();
        Self(child)
    }
    fn alive(&mut self) -> bool {
        self.0.try_wait().unwrap().is_none()
    }
    fn stop(&mut self) -> io::Result<()> {
        if self.0.try_wait()?.is_none() {
            self.0.kill()?;
        }
        let until = Instant::now() + WAIT;
        loop {
            if self.0.try_wait()?.is_some() {
                return Ok(());
            }
            if Instant::now() >= until {
                return Err(io::ErrorKind::TimedOut.into());
            }
            thread::sleep(Duration::from_millis(2));
        }
    }
}
impl Drop for Created {
    fn drop(&mut self) {
        if let Err(e) = self.stop() {
            eprintln!("owned fixture cleanup failed: {e}");
        }
    }
}

// A process PID is used once to acquire observation of our own fixture. Its
// creation time AND executable identity must match before it becomes an owned
// cleanup target. Afterwards only this retained handle is used, never PID kill.
struct Observed(OwnedHandle);
impl Observed {
    fn acquire(record: &Path) -> Self {
        let bytes = fs::read(record).unwrap();
        assert!(bytes.len() < 128);
        let text = std::str::from_utf8(&bytes).unwrap();
        let fields = text.split_whitespace().collect::<Vec<_>>();
        assert_eq!(fields.len(), 2);
        let pid = fields[0].parse::<u32>().unwrap();
        let expected_creation = fields[1].parse::<u64>().unwrap();
        // SAFETY: only PID from this test's completed, owned fixture record.
        // Native identity is checked before any termination capability is used.
        let raw = unsafe {
            OpenProcess(
                PROCESS_QUERY_LIMITED_INFORMATION | PROCESS_SYNCHRONIZE | PROCESS_TERMINATE,
                0,
                pid,
            )
        };
        assert!(
            !raw.is_null(),
            "open owned fixture: {}",
            io::Error::last_os_error()
        );
        // SAFETY: successful OpenProcess returned one new owned handle.
        let handle = unsafe { OwnedHandle::from_raw_handle(raw) };
        let mut times = [FILETIME {
            dwLowDateTime: 0,
            dwHighDateTime: 0,
        }; 4];
        // SAFETY: retained process handle and four writable FILETIME values.
        assert_ne!(
            unsafe {
                GetProcessTimes(
                    raw,
                    &mut times[0],
                    &mut times[1],
                    &mut times[2],
                    &mut times[3],
                )
            },
            0
        );
        let actual_creation =
            (u64::from(times[0].dwHighDateTime) << 32) | u64::from(times[0].dwLowDateTime);
        assert_eq!(
            actual_creation, expected_creation,
            "fixture PID was replaced"
        );
        let mut image = vec![0u16; 32768];
        let mut len = image.len() as u32;
        // SAFETY: live handle, correctly sized writable UTF-16 image buffer.
        assert_ne!(
            unsafe { QueryFullProcessImageNameW(raw, 0, image.as_mut_ptr(), &mut len) },
            0
        );
        let image = PathBuf::from(OsString::from_wide(&image[..len as usize]));
        assert_eq!(
            fs::canonicalize(image).unwrap(),
            fs::canonicalize(fixture()).unwrap()
        );
        Self(handle)
    }
    fn exited(&self, timeout: Duration) -> io::Result<bool> {
        // SAFETY: the original observation handle remains open throughout wait.
        match unsafe {
            WaitForSingleObject(
                self.0.as_raw_handle(),
                timeout.as_millis().try_into().unwrap(),
            )
        } {
            WAIT_OBJECT_0 => Ok(true),
            WAIT_TIMEOUT => Ok(false),
            _ => Err(io::Error::last_os_error()),
        }
    }
    fn exit_code(&self) -> u32 {
        assert!(self.exited(Duration::ZERO).unwrap());
        let mut code = 0;
        // SAFETY: retained terminated process handle and writable DWORD.
        assert_ne!(
            unsafe { GetExitCodeProcess(self.0.as_raw_handle(), &mut code) },
            0
        );
        code
    }
    fn cleanup(&self) -> io::Result<()> {
        if !self.exited(Duration::ZERO)? {
            // SAFETY: only our identity-verified, retained test-fixture handle.
            if unsafe { TerminateProcess(self.0.as_raw_handle(), 121) } == 0 {
                let failure = io::Error::last_os_error();
                if !self.exited(Duration::ZERO)? {
                    return Err(failure);
                }
            }
        }
        if !self.exited(WAIT)? {
            return Err(io::ErrorKind::TimedOut.into());
        }
        Ok(())
    }
}
impl Drop for Observed {
    fn drop(&mut self) {
        if let Err(e) = self.cleanup() {
            eprintln!("retained fixture cleanup failed: {e}");
        }
    }
}
fn wait_file(path: &Path, owner: &mut Created) {
    let until = Instant::now() + WAIT;
    while !path.exists() {
        assert!(
            owner.alive(),
            "fixture owner exited before readiness: {}",
            path.display()
        );
        assert!(
            Instant::now() < until,
            "missing fixture barrier: {}",
            path.display()
        );
        thread::sleep(Duration::from_millis(2));
    }
}

fn scenario(phase: &str, abrupt: bool) {
    let base = PathBuf::from(std::env::var_os("MCPHUB_RX02_FIXTURES").expect("owned fixture root"));
    let dir = base.join(format!(
        "crash-{}-{}-{phase}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ));
    fs::create_dir(&dir).unwrap();
    let control_dir = dir.join("unrelated");
    fs::create_dir(&control_dir).unwrap();
    let mut control = Created::launch("control", &control_dir, "");
    wait_file(&control_dir.join("control.process"), &mut control);
    let mut owner = Created::launch("owner", &dir, phase);
    wait_file(&dir.join("root.process"), &mut owner);
    let root = Observed::acquire(&dir.join("root.process"));
    assert!(!root.exited(Duration::ZERO).unwrap());
    assert!(
        !dir.join("root.started").exists(),
        "child executed before test admission"
    );
    let leaf = if phase == "suspended" {
        None
    } else {
        fs::write(dir.join("resume"), b"permit-test-admission").unwrap();
        wait_file(&dir.join("leaf.process"), &mut owner);
        let leaf = Observed::acquire(&dir.join("leaf.process"));
        assert!(!leaf.exited(Duration::ZERO).unwrap());
        if phase == "root-exited" {
            fs::write(dir.join("exit-root"), b"exit-root-only").unwrap();
        }
        Some(leaf)
    };
    wait_file(&dir.join("owner.ready"), &mut owner);
    if phase == "root-exited" {
        assert!(root.exited(WAIT).unwrap());
        assert_eq!(root.exit_code(), 7);
    } else {
        assert!(!root.exited(Duration::ZERO).unwrap());
    }
    if let Some(ref leaf) = leaf {
        assert!(!leaf.exited(Duration::ZERO).unwrap());
    }
    assert!(owner.alive());
    assert!(control.alive());
    assert!(!dir.join("owner.drop").exists());
    if abrupt {
        // Windows Child::kill targets only this retained creator handle. It is
        // not TerminateJobObject and does not explicitly kill the descendants.
        owner.stop().unwrap();
    } else {
        fs::write(dir.join("finish"), b"ordinary-explicit-finish").unwrap();
        let until = Instant::now() + WAIT;
        while owner.alive() {
            assert!(
                Instant::now() < until,
                "normal owner finish did not complete"
            );
            thread::sleep(Duration::from_millis(2));
        }
        assert!(owner.0.try_wait().unwrap().unwrap().success());
    }
    // Capture the actual outcome BEFORE test cleanup. Cleanup is not evidence
    // of the library's crash semantics and cannot turn a timeout into a PASS.
    let root_exited = root.exited(WAIT).unwrap();
    let leaf_exited = leaf
        .as_ref()
        .map(|p| p.exited(WAIT).unwrap())
        .unwrap_or(true);
    let control_survived = control.alive();
    let destructor_ran = dir.join("owner.drop").exists();
    let code = if root_exited {
        Some(root.exit_code())
    } else {
        None
    };
    root.cleanup().unwrap();
    if let Some(ref leaf) = leaf {
        leaf.cleanup().unwrap();
    }
    control.stop().unwrap();
    assert!(root_exited && leaf_exited, "creator died but original root/descendant remained live: root={root_exited}, leaf={leaf_exited}");
    assert!(control_survived, "unrelated owned control was terminated");
    assert_eq!(
        destructor_ran, !abrupt,
        "crash scenario was replaced by Rust unwinding/normal cleanup"
    );
    if phase == "suspended" {
        assert!(!dir.join("root.started").exists());
    }
    if phase == "root-exited" {
        assert_eq!(code, Some(7), "original root result was overwritten");
    }
    if !abrupt {
        assert_eq!(code, Some(61));
    }
}
#[test]
fn creator_death_before_admission_stops_suspended_child() {
    scenario("suspended", true);
}
#[test]
fn creator_death_stops_live_root_and_descendant_only() {
    scenario("running", true);
}
#[test]
fn creator_death_after_root_exit_stops_remaining_descendant() {
    scenario("root-exited", true);
}
#[test]
fn ordinary_finish_is_distinct_from_creator_crash() {
    scenario("running", false);
}

#[test]
fn creator_death_with_explicit_context_and_stdio_stops_descendants() {
    scenario("running-stdio", true);
}

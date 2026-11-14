#![cfg(windows)]
use mcphub_process_owner_spike::{NativeOwner, RootWait, Settlement};
use std::{
    ffi::OsString,
    fs, io,
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
    thread,
    time::{Duration, Instant},
};
const DEADLINE: Duration = Duration::from_secs(3);
static NEXT: AtomicU64 = AtomicU64::new(1);
struct Fixture {
    root: PathBuf,
}
impl Fixture {
    fn new() -> Self {
        let root =
            PathBuf::from(std::env::var_os("MCPHUB_RX02_FIXTURES").expect("owned fixture root"))
                .join(format!(
                    "mcphub-native-owner-{}-{}",
                    std::process::id(),
                    NEXT.fetch_add(1, Ordering::Relaxed)
                ));
        fs::create_dir(&root).unwrap();
        Self { root }
    }
    fn marker(&self) -> PathBuf {
        self.root.join("marker with spaces λ.txt")
    }
    fn launch(&self, mode: &str, code: u32, payload: &str) -> NativeOwner {
        NativeOwner::create_suspended(
            Path::new(env!("CARGO_BIN_EXE_owner_child")),
            &[
                mode.into(),
                self.marker().into_os_string(),
                code.to_string().into(),
                payload.into(),
            ],
        )
        .unwrap()
    }
}
// Fixtures are retained for evidence and later trash-first disposition; no deletion.
fn wait_marker(path: &Path) {
    let until = Instant::now() + DEADLINE;
    while !path.exists() {
        assert!(
            Instant::now() < until,
            "test child did not publish readiness"
        );
        thread::sleep(Duration::from_millis(5));
    }
}

#[test]
fn token_user_observation_matches_child_and_survives_settlement() {
    let f = Fixture::new();
    let mut owner = f.launch("token-user", 0, "");
    let mut observed = Vec::new();
    owner
        .admit_and_resume(|view| {
            observed = view.token_user_sid().map_err(io::Error::other)?;
            assert!(!observed.is_empty(), "token observation returned no SID");
            assert!(!f.marker().exists(), "token observation resumed the child");
            assert_eq!(view.wait_state()?, RootWait::NotSignaled);
            Ok(())
        })
        .unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 0 }
    );
    drop(owner);
    let report = fs::read(f.marker()).unwrap();
    // Compare privately: never include the machine's actual SID in test logs.
    assert!(
        observed == report,
        "original-process SID differs from child observation"
    );
}

#[test]
fn identity_observation_matches_child_and_does_not_resume_it() {
    let f = Fixture::new();
    let mut owner = f.launch("identity", 0, "");
    let pid = owner.root_pid();
    let mut birth = 0;
    owner
        .admit_and_resume(|view| {
            assert_eq!(view.process_id()?, pid);
            birth = view.creation_time_100ns()?;
            assert_ne!(birth, 0);
            assert_eq!(view.creation_time_100ns()?, birth);
            assert!(view.is_in_original_job()?);
            assert_eq!(view.wait_state()?, RootWait::NotSignaled);
            assert!(!f.marker().exists(), "observation resumed the child");
            Ok(())
        })
        .unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 0 }
    );
    let report = fs::read(f.marker()).unwrap();
    assert_eq!(report.len(), 12);
    assert_eq!(u32::from_le_bytes(report[..4].try_into().unwrap()), pid);
    assert_eq!(u64::from_le_bytes(report[4..].try_into().unwrap()), birth);
}

#[test]
fn original_process_is_suspended_and_job_member_before_admission() {
    let f = Fixture::new();
    let mut owner = f.launch("exit", 7, "hello");
    let pid = owner.root_pid();
    assert!(pid > 0);
    assert_eq!(owner.active_processes().unwrap(), 1);
    assert!(!f.marker().exists());
    assert_eq!(owner.wait_root(Duration::ZERO).unwrap(), None);
    owner
        .admit_and_resume(|view| {
            assert_eq!(view.process_id()?, pid);
            assert_eq!(
                view.image_path()?.file_name(),
                Path::new(env!("CARGO_BIN_EXE_owner_child")).file_name()
            );
            assert!(!f.marker().exists());
            Ok(())
        })
        .unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 7 }
    );
    assert_eq!(fs::read_to_string(f.marker()).unwrap(), "hello");
    assert_eq!(
        owner.finish(Duration::ZERO).unwrap(),
        Settlement::Complete { root_exit: 7 }
    );
}

#[test]
fn rejected_admission_retains_original_owner_without_execution() {
    let f = Fixture::new();
    let mut owner = f.launch("exit", 0, "must not appear");
    let pid = owner.root_pid();
    let err = owner
        .admit_and_resume(|_| {
            Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "fixture rejects",
            ))
        })
        .unwrap_err();
    assert_eq!(err.kind(), io::ErrorKind::PermissionDenied);
    assert_eq!(owner.root_pid(), pid);
    assert!(!f.marker().exists());
    assert!(matches!(
        owner.finish(Duration::ZERO).unwrap(),
        Settlement::Pending {
            root_exit: None,
            ..
        }
    ));
    owner.terminate(73).unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 73 }
    );
    assert!(!f.marker().exists());
}

#[test]
fn running_timeout_keeps_owner_and_can_be_settled() {
    let f = Fixture::new();
    let mut owner = f.launch("block", 0, "");
    owner.admit_and_resume(|_| Ok(())).unwrap();
    wait_marker(&f.marker());
    let pid = owner.root_pid();
    assert!(matches!(
        owner.finish(Duration::from_millis(5)).unwrap(),
        Settlement::Pending {
            root_exit: None,
            ..
        }
    ));
    assert_eq!(owner.root_pid(), pid);
    owner.terminate(74).unwrap();
    owner.terminate(74).unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 74 }
    );
    assert_eq!(owner.root_exit(), Some(74));
}

#[test]
fn one_owner_settles_root_and_descendant() {
    let f = Fixture::new();
    let mut owner = f.launch("tree", 0, "");
    owner.admit_and_resume(|_| Ok(())).unwrap();
    wait_marker(&f.marker());
    wait_marker(&f.marker().with_extension("grandchild"));
    assert!(owner.active_processes().unwrap() >= 2);
    owner.terminate(75).unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 75 }
    );
}

#[test]
fn exit_code_still_active_is_not_mistaken_for_running() {
    let f = Fixture::new();
    let mut owner = f.launch("exit", 259, "finished");
    owner.admit_and_resume(|_| Ok(())).unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 259 }
    );
}

#[test]
fn wide_argv_preserves_unicode_quotes_backslashes_and_empty() {
    for payload in [
        "",
        "a b λ",
        "quote\"inside",
        "trail\\",
        "slash\\\"quote\\\\",
    ] {
        let f = Fixture::new();
        let mut owner = f.launch("exit", 0, payload);
        owner.admit_and_resume(|_| Ok(())).unwrap();
        assert_eq!(
            owner.finish(DEADLINE).unwrap(),
            Settlement::Complete { root_exit: 0 }
        );
        assert_eq!(fs::read_to_string(f.marker()).unwrap(), payload);
    }
}

#[test]
fn repeated_resume_is_rejected() {
    let f = Fixture::new();
    let mut owner = f.launch("block", 0, "");
    owner.admit_and_resume(|_| Ok(())).unwrap();
    assert_eq!(
        owner.admit_and_resume(|_| Ok(())).unwrap_err().kind(),
        io::ErrorKind::InvalidInput
    );
    owner.terminate(76).unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 76 }
    );
}

#[test]
fn invalid_argument_fails_before_creation() {
    let args = [OsString::from("bad\0argument")];
    let result = NativeOwner::create_suspended(Path::new(env!("CARGO_BIN_EXE_owner_child")), &args);
    assert!(matches!(result,Err(ref e) if e.kind()==io::ErrorKind::InvalidInput));
}

#[test]
fn missing_image_returns_error_without_started_owner() {
    let f = Fixture::new();
    assert!(NativeOwner::create_suspended(&f.root.join("missing.exe"), &[]).is_err());
}

#[test]
fn repeated_create_settle_does_not_grow_handle_count() {
    use windows_sys::Win32::System::Threading::{GetCurrentProcess, GetProcessHandleCount};
    fn handles() -> u32 {
        let mut n = 0; /* SAFETY: valid pseudo-handle and writable count. */
        assert_ne!(
            unsafe { GetProcessHandleCount(GetCurrentProcess(), &mut n) },
            0
        );
        n
    }
    let warm = Fixture::new();
    let mut owner = warm.launch("exit", 0, "");
    owner.admit_and_resume(|_| Ok(())).unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 0 }
    );
    drop(owner);
    let before = handles();
    for _ in 0..24 {
        let f = Fixture::new();
        let mut owner = f.launch("exit", 0, "");
        owner.admit_and_resume(|_| Ok(())).unwrap();
        assert_eq!(
            owner.finish(DEADLINE).unwrap(),
            Settlement::Complete { root_exit: 0 }
        );
    }
    assert_eq!(
        handles(),
        before,
        "native handles leaked across complete owner lifetimes"
    );
}

#[test]
fn completed_root_does_not_mean_completed_tree() {
    let f = Fixture::new();
    let mut owner = f.launch("detach", 0, "");
    owner.admit_and_resume(|_| Ok(())).unwrap();
    wait_marker(&f.marker().with_extension("grandchild"));
    assert_eq!(owner.wait_root(DEADLINE).unwrap(), Some(7));
    assert!(
        matches!(owner.finish(Duration::ZERO).unwrap(),Settlement::Pending{root_exit:Some(7),active_processes:Some(n)} if n>0)
    );
    owner.terminate(82).unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 7 }
    );
}

#[test]
fn denied_admission_cannot_be_retried_as_approval() {
    let f = Fixture::new();
    let mut owner = f.launch("exit", 0, "forbidden");
    assert!(owner
        .admit_and_resume(|_| Err(io::ErrorKind::PermissionDenied.into()))
        .is_err());
    assert_eq!(
        owner.admit_and_resume(|_| Ok(())).unwrap_err().kind(),
        io::ErrorKind::InvalidInput
    );
    owner.terminate(83).unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 83 }
    );
    assert!(!f.marker().exists());
}

#[test]
fn root_result_survives_late_stop_request() {
    let f = Fixture::new();
    let mut owner = f.launch("exit", 7, "completed");
    owner.admit_and_resume(|_| Ok(())).unwrap();
    assert_eq!(owner.wait_root(DEADLINE).unwrap(), Some(7));
    owner.terminate(84).unwrap();
    assert_eq!(
        owner.finish(DEADLINE).unwrap(),
        Settlement::Complete { root_exit: 7 }
    );
}

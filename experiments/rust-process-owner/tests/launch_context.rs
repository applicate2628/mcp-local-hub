#![cfg(windows)]
use mcphub_process_owner_spike::{LaunchContext, NativeOwner, Settlement};
use std::{
    env,
    ffi::{OsStr, OsString},
    fs, io,
    os::windows::ffi::{OsStrExt, OsStringExt},
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
    time::Duration,
};

const WAIT: Duration = Duration::from_secs(3);
const PARENT_KEY: &str = "MCPHUB_RX02_PARENT_SENTINEL";
static NEXT: AtomicU64 = AtomicU64::new(1);

struct Fixture {
    root: PathBuf,
    cwd: PathBuf,
    report: PathBuf,
}
impl Fixture {
    fn new() -> Self {
        let root = PathBuf::from(env::var_os("MCPHUB_RX02_FIXTURES").expect("owned fixtures root"))
            .join(format!(
                "launch-context-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
        fs::create_dir(&root).unwrap();
        let cwd = root.join("working folder λ");
        fs::create_dir(&cwd).unwrap();
        let report = root.join("report.bin");
        Self { root, cwd, report }
    }
    fn args(&self, keys: &[&str]) -> Vec<OsString> {
        let mut args = vec!["context".into(), self.report.clone().into_os_string()];
        args.extend(keys.iter().map(OsString::from));
        args
    }
    fn create(&self, entries: &[(OsString, OsString)], keys: &[&str]) -> io::Result<NativeOwner> {
        NativeOwner::create_suspended_with_context(
            child(),
            &self.args(keys),
            LaunchContext {
                current_dir: &self.cwd,
                environment: entries,
            },
            None,
        )
    }
    fn read(&self) -> Vec<Option<OsString>> {
        let bytes = fs::read(&self.report).unwrap();
        let mut remaining = bytes.as_slice();
        let mut fields = Vec::new();
        while !remaining.is_empty() {
            assert!(remaining.len() >= 4);
            let n = u32::from_le_bytes(remaining[..4].try_into().unwrap());
            remaining = &remaining[4..];
            if n == u32::MAX {
                fields.push(None);
                continue;
            }
            let n = n as usize;
            assert!(n <= 32768);
            assert!(remaining.len() >= 2 * n);
            let wide = remaining[..2 * n]
                .as_chunks::<2>()
                .0
                .iter()
                .map(|p| u16::from_le_bytes(*p))
                .collect::<Vec<_>>();
            fields.push(Some(OsString::from_wide(&wide)));
            remaining = &remaining[2 * n..];
        }
        fields
    }
}
fn child() -> &'static Path {
    Path::new(env!("CARGO_BIN_EXE_owner_child"))
}
fn complete(owner: &mut NativeOwner) {
    assert_eq!(
        owner.finish(WAIT).unwrap(),
        Settlement::Complete { root_exit: 0 }
    );
}
fn parent_snapshot() -> (PathBuf, Option<OsString>) {
    (env::current_dir().unwrap(), env::var_os(PARENT_KEY))
}
fn assert_parent(snapshot: &(PathBuf, Option<OsString>)) {
    assert_eq!(env::current_dir().unwrap(), snapshot.0);
    assert_eq!(env::var_os(PARENT_KEY), snapshot.1)
}
fn assert_cwd(observed: &Option<OsString>, expected: &Path) {
    assert_eq!(
        fs::canonicalize(Path::new(observed.as_ref().unwrap())).unwrap(),
        fs::canonicalize(expected).unwrap()
    );
}

#[test]
fn explicit_context_preserves_unicode_empty_values_and_excludes_parent() {
    let f = Fixture::new();
    let snapshot = parent_snapshot();
    assert_eq!(
        snapshot.1.as_deref(),
        Some(OsStr::new("owned-test-parent-only-20261113")),
        "test driver must seed only its fake sentinel"
    );
    let entries = vec![
        ("MCPHUB_A".into(), "λ=value=with=equals".into()),
        ("MCPHUB_EMPTY".into(), OsString::new()),
        ("MCPHUB_ТЕСТ".into(), "значение 🦀".into()),
    ];
    let before = entries.clone();
    let mut args = f.args(&["mcphub_a", "MCPHUB_EMPTY", "mcphub_тест", PARENT_KEY]);
    args[0] = "context-relative".into();
    let mut owner = NativeOwner::create_suspended_with_context(
        child(),
        &args,
        LaunchContext {
            current_dir: &f.cwd,
            environment: &entries,
        },
        None,
    )
    .unwrap();
    assert!(!f.report.exists());
    assert!(!f.cwd.join("relative-proof.bin").exists());
    assert_parent(&snapshot);
    owner
        .admit_and_resume(|_| {
            assert!(!f.report.exists());
            Ok(())
        })
        .unwrap();
    complete(&mut owner);
    let fields = f.read();
    assert_eq!(fields.len(), 5);
    assert_cwd(&fields[0], &f.cwd);
    assert_eq!(
        &fields[1..],
        &[
            Some(entries[0].1.clone()),
            Some(OsString::new()),
            Some(entries[2].1.clone()),
            None
        ]
    );
    assert_eq!(
        fs::read(f.cwd.join("relative-proof.bin")).unwrap(),
        b"owned-context"
    );
    assert_eq!(entries, before);
    assert_parent(&snapshot);
}

#[test]
fn empty_explicit_environment_is_not_inherit() {
    let f = Fixture::new();
    let snapshot = parent_snapshot();
    assert!(snapshot.1.is_some());
    let mut owner = f.create(&[], &[PARENT_KEY]).unwrap();
    owner.admit_and_resume(|_| Ok(())).unwrap();
    complete(&mut owner);
    let fields = f.read();
    assert_eq!(fields.len(), 2);
    assert_cwd(&fields[0], &f.cwd);
    assert_eq!(fields[1], None);
    assert_parent(&snapshot);
}

#[test]
fn old_create_still_inherits_parent_context() {
    let f = Fixture::new();
    let snapshot = parent_snapshot();
    // The child fixture writes its relative proof only when an explicit test key requests it.
    let mut owner = NativeOwner::create_suspended(child(), &f.args(&[PARENT_KEY])).unwrap();
    owner.admit_and_resume(|_| Ok(())).unwrap();
    complete(&mut owner);
    let fields = f.read();
    assert_cwd(&fields[0], &snapshot.0);
    assert_eq!(fields[1], snapshot.1);
    assert_parent(&snapshot);
}

#[test]
fn encoded_context_outlives_source_strings_until_child_runs() {
    let f = Fixture::new();
    let mut entries = vec![("MCPHUB_VALUE".into(), "before".into())];
    let mut owner = f.create(&entries, &["MCPHUB_VALUE"]).unwrap();
    entries[0].1 = "after-create".into();
    drop(entries);
    owner.admit_and_resume(|_| Ok(())).unwrap();
    complete(&mut owner);
    assert_eq!(f.read()[1], Some("before".into()));
}

#[test]
fn denied_admission_keeps_context_unexecuted_and_parent_unchanged() {
    let f = Fixture::new();
    let snapshot = parent_snapshot();
    let entries = vec![("MCPHUB_VALUE".into(), "unused".into())];
    let mut owner = f.create(&entries, &["MCPHUB_VALUE"]).unwrap();
    let err = owner
        .admit_and_resume(|_| {
            Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "owned deny",
            ))
        })
        .unwrap_err();
    assert_eq!(err.kind(), io::ErrorKind::PermissionDenied);
    assert!(!f.report.exists());
    assert_parent(&snapshot);
    owner.terminate(77).unwrap();
    assert_eq!(
        owner.finish(WAIT).unwrap(),
        Settlement::Complete { root_exit: 77 }
    );
    assert!(!f.report.exists());
    assert_parent(&snapshot);
}

#[test]
fn invalid_environment_is_rejected_before_process_creation() {
    let cases = vec![
        vec![(OsString::new(), "v".into())],
        vec![("BAD=NAME".into(), "v".into())],
        vec![("=D:".into(), "D:\\hidden".into())],
        vec![(OsString::from_wide(&[65, 0, 66]), "v".into())],
        vec![("A".into(), OsString::from_wide(&[65, 0, 66]))],
        vec![("A".into(), "first".into()), ("a".into(), "second".into())],
        vec![
            ("ТЕСТ".into(), "first".into()),
            ("тест".into(), "second".into()),
        ],
        vec![("Z".into(), "last".into()), ("A".into(), "first".into())],
        vec![(
            "MCPHUB_VALUE".into(),
            OsString::from("x".repeat((1 << 20) + 1)),
        )],
    ];
    let snapshot = parent_snapshot();
    for entries in cases {
        let f = Fixture::new();
        let result = f.create(&entries, &[]);
        if let Ok(mut owner) = result {
            let _ = owner.terminate(78);
            let _ = owner.finish(WAIT);
            panic!("invalid context was accepted")
        }
        let err = result.err().unwrap();
        assert_eq!(err.kind(), io::ErrorKind::InvalidInput);
        assert!(!err.to_string().contains("first"));
        assert!(!err.to_string().contains("second"));
        assert!(!f.report.exists());
        assert_parent(&snapshot);
    }
}

#[test]
fn relative_empty_and_nul_directory_are_rejected_without_parent_mutation() {
    let f = Fixture::new();
    let snapshot = parent_snapshot();
    let mut with_nul = f.cwd.as_os_str().encode_wide().collect::<Vec<_>>();
    with_nul.push(0);
    for path in [
        PathBuf::new(),
        PathBuf::from("relative"),
        PathBuf::from("D:relative"),
        PathBuf::from(OsString::from_wide(&with_nul)),
    ] {
        let result = NativeOwner::create_suspended_with_context(
            child(),
            &f.args(&[]),
            LaunchContext {
                current_dir: &path,
                environment: &[],
            },
            None,
        );
        if let Ok(mut owner) = result {
            let _ = owner.terminate(79);
            let _ = owner.finish(WAIT);
            panic!("invalid cwd accepted")
        }
        assert_eq!(result.err().unwrap().kind(), io::ErrorKind::InvalidInput);
        assert_parent(&snapshot);
    }
    assert!(!f.report.exists());
}

#[test]
fn native_missing_directory_error_is_not_success_or_parent_change() {
    let f = Fixture::new();
    let snapshot = parent_snapshot();
    let missing = f.root.join("absent");
    let result = NativeOwner::create_suspended_with_context(
        child(),
        &f.args(&[]),
        LaunchContext {
            current_dir: &missing,
            environment: &[],
        },
        None,
    );
    if let Ok(mut owner) = result {
        let _ = owner.terminate(80);
        let _ = owner.finish(WAIT);
        panic!("missing cwd accepted")
    }
    assert!(result.err().unwrap().raw_os_error().is_some());
    assert!(!f.report.exists());
    assert_parent(&snapshot);
}

#[test]
fn context_value_round_trips_non_unicode_windows_units() {
    let f = Fixture::new();
    let raw = OsString::from_wide(&[b'x' as u16, 0xD800, b'y' as u16]);
    let entries = vec![("MCPHUB_RAW".into(), raw.clone())];
    let mut owner = f.create(&entries, &["MCPHUB_RAW"]).unwrap();
    owner.admit_and_resume(|_| Ok(())).unwrap();
    complete(&mut owner);
    assert_eq!(f.read()[1], Some(raw));
}

#[test]
fn two_live_children_have_separate_contexts_without_parent_switches() {
    let left = Fixture::new();
    let right = Fixture::new();
    let snapshot = parent_snapshot();
    let mut a = left
        .create(&[("MCPHUB_ID".into(), "left".into())], &["MCPHUB_ID"])
        .unwrap();
    let mut b = right
        .create(&[("MCPHUB_ID".into(), "right".into())], &["MCPHUB_ID"])
        .unwrap();
    a.admit_and_resume(|_| Ok(())).unwrap();
    b.admit_and_resume(|_| Ok(())).unwrap();
    complete(&mut a);
    complete(&mut b);
    assert_cwd(&left.read()[0], &left.cwd);
    assert_eq!(left.read()[1], Some("left".into()));
    assert_cwd(&right.read()[0], &right.cwd);
    assert_eq!(right.read()[1], Some("right".into()));
    assert_parent(&snapshot);
}

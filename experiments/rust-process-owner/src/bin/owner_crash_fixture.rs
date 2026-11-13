//! Finite, owned native test fixture. Not a service, recovery worker or backend.
#[cfg(not(windows))]
fn main() {
    std::process::exit(93);
}

#[cfg(windows)]
fn main() {
    windows_fixture::run();
}

#[cfg(windows)]
mod windows_fixture {
    use mcphub_process_owner_spike::{BorrowedStdio, LaunchContext, NativeOwner, Settlement};
    use std::{
        env, fs, io,
        os::windows::{
            io::{AsHandle, AsRawHandle, FromRawHandle, OwnedHandle},
            process::CommandExt,
        },
        path::{Path, PathBuf},
        process::{self, Command, Stdio},
        thread,
        time::{Duration, Instant},
    };
    use windows_sys::Win32::{
        Foundation::{SetHandleInformation, FILETIME, HANDLE, HANDLE_FLAG_INHERIT},
        System::Threading::{
            GetCurrentProcess, GetProcessTimes, OpenProcess, PROCESS_QUERY_LIMITED_INFORMATION,
        },
    };

    fn publish(path: &Path, content: impl AsRef<[u8]>) {
        let temporary = path.with_extension("pending");
        fs::write(&temporary, content).expect("owned fixture record");
        fs::rename(&temporary, path).expect("publish complete fixture record");
    }
    fn identity(handle: HANDLE) -> io::Result<u64> {
        let mut times = [FILETIME {
            dwLowDateTime: 0,
            dwHighDateTime: 0,
        }; 4];
        // SAFETY: caller retains the process handle (or current-process pseudo
        // handle); each output is a writable correctly sized FILETIME.
        if unsafe {
            GetProcessTimes(
                handle,
                &mut times[0],
                &mut times[1],
                &mut times[2],
                &mut times[3],
            )
        } == 0
        {
            return Err(io::Error::last_os_error());
        }
        Ok((u64::from(times[0].dwHighDateTime) << 32) | u64::from(times[0].dwLowDateTime))
    }
    fn self_record(path: &Path) {
        // SAFETY: pseudo handle is borrowed only, never turned into an owner.
        let creation = identity(unsafe { GetCurrentProcess() }).unwrap();
        publish(path, format!("{} {creation}\n", process::id()));
    }
    fn wait_for(path: &Path) {
        let until = Instant::now() + Duration::from_secs(20);
        while !path.exists() {
            assert!(
                Instant::now() < until,
                "fixture barrier timeout: {}",
                path.display()
            );
            thread::sleep(Duration::from_millis(2));
        }
    }
    struct DropWitness(PathBuf);
    impl Drop for DropWitness {
        fn drop(&mut self) {
            let _ = fs::write(&self.0, b"Rust stack destruction occurred");
        }
    }
    pub(super) fn run() {
        let args = env::args_os().collect::<Vec<_>>();
        assert_eq!(args.len(), 4);
        let role = args[1].to_str().expect("fixture role");
        let dir = PathBuf::from(&args[2]);
        assert!(
            dir.is_absolute() && dir.is_dir(),
            "existing owned fixture directory required"
        );
        let phase = args[3].to_str().expect("fixture phase");
        match role {
            "control" | "leaf" => {
                self_record(&dir.join(format!("{role}.process")));
                // A failing test cannot leave a normally running fixture forever.
                // Successful crash qualification finishes before this watchdog.
                thread::sleep(Duration::from_secs(30));
                process::exit(92);
            }
            "root" => {
                publish(&dir.join("root.started"), b"admitted");
                let mut child = Command::new(env::current_exe().unwrap())
                    .arg("leaf")
                    .arg(&dir)
                    .arg(phase)
                    .stdin(Stdio::null())
                    .stdout(Stdio::null())
                    .stderr(Stdio::null())
                    .creation_flags(0x08000000)
                    .spawn()
                    .expect("owned descendant");
                wait_for(&dir.join("leaf.process"));
                if phase == "root-exited" {
                    wait_for(&dir.join("exit-root"));
                    process::exit(7); // leaves its Job-contained child running
                }
                let _ = child.wait();
                process::exit(92);
            }
            "owner" => {
                assert!(matches!(
                    phase,
                    "suspended" | "running" | "root-exited" | "running-stdio"
                ));
                let _witness = DropWitness(dir.join("owner.drop"));
                let exe = env::current_exe().unwrap();
                let child_args = vec!["root".into(), dir.clone().into_os_string(), phase.into()];
                let streams = if phase == "running-stdio" {
                    let input_path = dir.join("input");
                    fs::write(&input_path, b"").unwrap();
                    let files = [
                        fs::File::open(input_path).unwrap(),
                        fs::File::create(dir.join("output")).unwrap(),
                        fs::File::create(dir.join("error")).unwrap(),
                    ];
                    for file in &files {
                        // SAFETY: these newly created fixture files are owned
                        // exclusively here; no concurrent flag mutation/create.
                        assert_ne!(
                            unsafe {
                                SetHandleInformation(
                                    file.as_raw_handle(),
                                    HANDLE_FLAG_INHERIT,
                                    HANDLE_FLAG_INHERIT,
                                )
                            },
                            0
                        );
                    }
                    Some(files)
                } else {
                    None
                };
                let mut owner = if let Some(ref files) = streams {
                    NativeOwner::create_suspended_with_context(
                        &exe,
                        &child_args,
                        LaunchContext {
                            current_dir: &dir,
                            environment: &[],
                        },
                        Some(BorrowedStdio {
                            stdin: files[0].as_handle(),
                            stdout: files[1].as_handle(),
                            stderr: files[2].as_handle(),
                        }),
                    )
                    .unwrap()
                } else {
                    NativeOwner::create_suspended(&exe, &child_args).unwrap()
                };
                drop(streams); // no fixture-owned stream extends the Job lifetime
                let pid = owner.root_pid();
                // SAFETY: the existing owner retains this original suspended
                // child, so its PID cannot identify a replacement here. This
                // query-only metadata handle is closed before publishing; it
                // never observes or retains the Job.
                let raw = unsafe { OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid) };
                assert!(
                    !raw.is_null(),
                    "metadata open: {}",
                    io::Error::last_os_error()
                );
                // SAFETY: successful OpenProcess returned one new owned handle.
                let observation = unsafe { OwnedHandle::from_raw_handle(raw) };
                let creation = identity(observation.as_raw_handle()).unwrap();
                drop(observation);
                publish(&dir.join("root.process"), format!("{pid} {creation}\n"));
                if phase != "suspended" {
                    wait_for(&dir.join("resume"));
                    owner
                        .admit_and_resume(|view| {
                            assert_eq!(view.process_id()?, pid);
                            Ok(())
                        })
                        .unwrap();
                    wait_for(&dir.join("leaf.process"));
                    if phase == "root-exited" {
                        assert_eq!(owner.wait_root(Duration::from_secs(10)).unwrap(), Some(7));
                        assert!(owner.active_processes().unwrap() >= 1);
                    } else {
                        assert!(owner.active_processes().unwrap() >= 2);
                    }
                }
                publish(
                    &dir.join("owner.ready"),
                    b"owner alive and original Job retained",
                );
                wait_for(&dir.join("finish"));
                owner.terminate(61).unwrap();
                assert_eq!(
                    owner.finish(Duration::from_secs(4)).unwrap(),
                    Settlement::Complete { root_exit: 61 }
                );
                // Only normal control reaches here and then runs DropWitness.
            }
            _ => panic!("unknown fixture role"),
        }
    }
}

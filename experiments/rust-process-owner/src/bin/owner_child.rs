#[cfg(windows)]
use std::os::windows::process::CommandExt;
use std::{
    env, fs,
    process::{self, Command},
    thread,
    time::Duration,
};
fn main() {
    let args: Vec<_> = env::args_os().collect();
    if args.len() < 3 {
        process::exit(90)
    }
    let mode = args[1].to_string_lossy();
    let marker = std::path::PathBuf::from(&args[2]);
    #[cfg(windows)]
    if mode == "token-user" {
        use std::{
            io,
            mem::size_of,
            os::windows::io::{AsRawHandle, FromRawHandle, IntoRawHandle, OwnedHandle},
            ptr::null_mut,
        };
        use windows_sys::Win32::{
            Foundation::{CloseHandle, ERROR_INSUFFICIENT_BUFFER},
            Security::{
                GetLengthSid, GetTokenInformation, IsValidSid, TokenUser, TOKEN_QUERY, TOKEN_USER,
            },
            System::Threading::{GetCurrentProcess, OpenProcessToken},
        };
        let mut raw = null_mut();
        // SAFETY: the child's own pseudo-process handle; writable token output.
        assert_ne!(
            unsafe { OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut raw) },
            0
        );
        // SAFETY: successful acquisition transfers exactly one token handle.
        let token = unsafe { OwnedHandle::from_raw_handle(raw) };
        let mut needed = 0;
        // Independent fixture uses the documented size query, not the library's
        // fixed buffer/parser. It never calls ProcessView::token_user_sid.
        // SAFETY: live owned token and documented null/zero size-query output.
        assert_eq!(
            unsafe {
                GetTokenInformation(token.as_raw_handle(), TokenUser, null_mut(), 0, &mut needed)
            },
            0
        );
        assert_eq!(
            io::Error::last_os_error().raw_os_error(),
            Some(ERROR_INSUFFICIENT_BUFFER as i32)
        );
        assert!((size_of::<TOKEN_USER>() as u32..=4096).contains(&needed));
        let mut storage = vec![0usize; (needed as usize).div_ceil(size_of::<usize>())];
        // SAFETY: initialized aligned storage has at least the requested size;
        // the token stays owned for the complete native observation and copy.
        assert_ne!(
            unsafe {
                GetTokenInformation(
                    token.as_raw_handle(),
                    TokenUser,
                    storage.as_mut_ptr().cast(),
                    needed,
                    &mut needed,
                )
            },
            0
        );
        // SAFETY: a successful TokenUser query initializes this header and its
        // SID in storage, which has not moved since the call and remains live.
        let user = unsafe { storage.as_ptr().cast::<TOKEN_USER>().read() };
        // SAFETY: native successful TokenUser result supplies a valid SID.
        assert_ne!(unsafe { IsValidSid(user.User.Sid) }, 0);
        // SAFETY: IsValidSid just accepted this live native result.
        let length = unsafe { GetLengthSid(user.User.Sid) } as usize;
        let start = (user.User.Sid as usize)
            .checked_sub(storage.as_ptr() as usize)
            .unwrap();
        assert!(
            start >= size_of::<TOKEN_USER>()
                && start
                    .checked_add(length)
                    .is_some_and(|end| end <= needed as usize)
        );
        // SAFETY: the exact validated SID extent is still inside owned storage.
        let report =
            unsafe { std::slice::from_raw_parts(user.User.Sid.cast::<u8>(), length) }.to_vec();
        let raw = token.into_raw_handle();
        // SAFETY: this consumes the sole token owner; no Drop repeats the close.
        assert_ne!(unsafe { CloseHandle(raw) }, 0);
        fs::write(marker, report).expect("owned private token-user report");
        return;
    }
    #[cfg(windows)]
    if mode == "identity" {
        use windows_sys::Win32::{
            Foundation::FILETIME,
            System::Threading::{GetCurrentProcess, GetProcessTimes},
        };
        let mut creation = FILETIME {
            dwLowDateTime: 0,
            dwHighDateTime: 0,
        };
        let mut exit = creation;
        let mut kernel = creation;
        let mut user = creation;
        // SAFETY: this process's pseudo-handle is valid and never closed; four
        // separate FILETIME outputs are writable. Only creation is consumed.
        assert_ne!(
            unsafe {
                GetProcessTimes(
                    GetCurrentProcess(),
                    &mut creation,
                    &mut exit,
                    &mut kernel,
                    &mut user,
                )
            },
            0
        );
        let ticks = (u64::from(creation.dwHighDateTime) << 32) | u64::from(creation.dwLowDateTime);
        let mut report = process::id().to_le_bytes().to_vec();
        report.extend_from_slice(&ticks.to_le_bytes());
        fs::write(marker, report).expect("owned identity report");
        return;
    }
    #[cfg(windows)]
    if mode == "context" || mode == "context-relative" {
        use std::os::windows::ffi::OsStrExt;
        // Observe only explicitly requested test keys, never dump inherited
        // environment or user secrets. UTF-16 framing preserves non-Unicode.
        assert!(args.len() <= 19);
        let mut report = Vec::new();
        let cwd = env::current_dir().expect("child cwd").into_os_string();
        let fields = std::iter::once(Some(cwd)).chain(args[3..].iter().map(env::var_os));
        for field in fields {
            if let Some(value) = field {
                let wide: Vec<_> = value.encode_wide().collect();
                assert!(wide.len() <= 32768);
                report.extend_from_slice(&(wide.len() as u32).to_le_bytes());
                for unit in wide {
                    report.extend_from_slice(&unit.to_le_bytes());
                }
            } else {
                report.extend_from_slice(&u32::MAX.to_le_bytes());
            }
        }
        if mode == "context-relative" {
            fs::write("relative-proof.bin", b"owned-context").expect("owned relative artifact");
        }
        fs::write(marker, report).expect("owned context report");
        return;
    }
    if mode == "stdio" || mode == "stdio-probe" {
        use std::io::{Read, Write};
        fs::write(&marker, b"reading").expect("owned readiness marker");
        #[cfg(windows)]
        if mode == "stdio-probe" {
            let raw = args[3]
                .to_string_lossy()
                .parse::<usize>()
                .expect("fixture handle");
            // SAFETY: fixture asks only SetEvent on a numeric handle. No handle is
            // borrowed/closed or fabricated into an OwnedHandle. Native failure
            // is expected when the capability did not cross process creation.
            unsafe { windows_sys::Win32::System::Threading::SetEvent(raw as _) };
        }
        let mut bytes = Vec::new();
        std::io::stdin()
            .take(4096)
            .read_to_end(&mut bytes)
            .expect("owned bounded stdin");
        std::io::stdout().write_all(&bytes).expect("stdout bytes");
        std::io::stdout().flush().unwrap();
        std::io::stderr()
            .write_all(b"native-stderr\0\xff")
            .expect("stderr bytes");
        std::io::stderr().flush().unwrap();
        fs::write(&marker, b"complete").expect("owned completion marker");
        return;
    }
    if mode == "exit" {
        if args.len() != 5 {
            process::exit(91)
        }
        fs::write(marker, args[4].to_string_lossy().as_bytes()).expect("owned fixture write");
        process::exit(args[3].to_string_lossy().parse::<i32>().expect("exit code"));
    }
    if mode == "tree" || mode == "detach" {
        let grand = marker.with_extension("grandchild");
        let mut cmd = Command::new(env::current_exe().unwrap());
        cmd.arg("block").arg(&grand);
        #[cfg(windows)]
        cmd.creation_flags(0x08000000);
        let mut child = cmd.spawn().expect("test descendant");
        fs::write(&marker, b"root-ready").unwrap();
        if mode == "detach" {
            process::exit(7);
        }
        let _ = child.wait();
        process::exit(0);
    }
    if mode == "block" {
        fs::write(marker, b"ready").expect("owned fixture write");
        thread::sleep(Duration::from_secs(10));
        process::exit(92);
    }
    process::exit(93)
}

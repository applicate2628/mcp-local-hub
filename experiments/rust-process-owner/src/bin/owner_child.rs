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

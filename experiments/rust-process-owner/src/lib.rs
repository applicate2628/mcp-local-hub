//! Isolated native-owner experiment, not a production Hub backend.
//! No public C ABI, inherited stdio, policy, retries, or implicit Unix fallback.
#![deny(unsafe_op_in_unsafe_fn)]

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NativeFailure {
    pub operation: &'static str,
    pub code: i32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
#[must_use = "a pending owner must remain available for explicit settlement"]
pub enum Settlement {
    Pending {
        root_exit: Option<u32>,
        active_processes: Option<u32>,
    },
    Complete {
        root_exit: u32,
    },
    CleanupFailed {
        root_exit: u32,
        failures: Vec<NativeFailure>,
    },
}

#[cfg(windows)]
mod windows;
#[cfg(windows)]
pub use windows::{NativeOwner, ProcessView};

#[cfg(not(windows))]
mod unsupported {
    use std::{ffi::OsString, io, path::Path};
    /// Strict suspended-at-create Job ownership has no qualified Unix backend.
    pub struct NativeOwner;
    impl NativeOwner {
        pub fn create_suspended(_: &Path, _: &[OsString]) -> io::Result<Self> {
            Err(io::Error::new(
                io::ErrorKind::Unsupported,
                "strict native owner not qualified on this platform",
            ))
        }
    }
}
#[cfg(not(windows))]
pub use unsupported::NativeOwner;

//! Isolated state-file reader experiment. No installation, mutation or Hub API.
#![deny(unsafe_op_in_unsafe_fn)]
use std::{ffi::OsString, path::Path, sync::atomic::AtomicBool};
use zeroize::Zeroizing;

pub const MAX_STATE_BYTES: usize = 1 << 20;
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Category {
    InvalidInput,
    Canceled,
    TooLarge,
    ChecksumMismatch,
    UnsafeObjectOrIo,
    Unsupported,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum NativeCode {
    Nt(i32),
    Win32(i32),
}
#[derive(Debug)]
pub struct ReadError {
    pub category: Category,
    pub operation: &'static str,
    pub native: Option<NativeCode>,
    pub cleanup_failures: Vec<NativeCode>,
}
impl ReadError {
    pub(crate) fn new(
        category: Category,
        operation: &'static str,
        native: Option<NativeCode>,
    ) -> Self {
        Self {
            category,
            operation,
            native,
            cleanup_failures: Vec::new(),
        }
    }
}

/// Read a regular file through retained no-follow handles and verify the exact
/// returned bytes. This first slice accepts a caller-supplied cap up to 1 MiB.
/// The caller must already be authorized for root; a digest is not authority.
/// No ordinary path is reopened after final handle acquisition.
pub fn read_verified(
    root: &Path,
    components: &[OsString],
    cap: usize,
    expected: &[u8; 32],
    canceled: &AtomicBool,
) -> Result<Zeroizing<Vec<u8>>, ReadError> {
    #[cfg(windows)]
    {
        windows::read_with_hook(root, components, cap, expected, canceled, |_| Ok(()))
    }
    #[cfg(not(windows))]
    {
        let _ = (root, components, cap, expected, canceled);
        Err(ReadError::new(
            Category::Unsupported,
            "platform-unqualified",
            None,
        ))
    }
}

#[cfg(windows)]
mod windows;

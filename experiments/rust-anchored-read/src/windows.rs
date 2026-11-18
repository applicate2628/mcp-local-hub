use crate::{Category, NativeCode, ReadError, MAX_STATE_BYTES};
use sha2::{Digest, Sha256};
use std::{
    ffi::{OsStr, OsString},
    mem::{size_of, zeroed},
    os::windows::{
        ffi::{OsStrExt, OsStringExt},
        io::{AsRawHandle, FromRawHandle, IntoRawHandle, OwnedHandle},
    },
    path::{Component, Path},
    ptr::{null, null_mut},
    sync::atomic::{AtomicBool, Ordering},
};
use windows_sys::Wdk::{
    Foundation::OBJECT_ATTRIBUTES,
    Storage::FileSystem::{
        NtCreateFile, FILE_DIRECTORY_FILE, FILE_NON_DIRECTORY_FILE, FILE_OPEN,
        FILE_OPEN_REPARSE_POINT, FILE_SYNCHRONOUS_IO_NONALERT,
    },
};
use windows_sys::Win32::{
    Foundation::{
        CloseHandle, ERROR_HANDLE_EOF, HANDLE, OBJ_CASE_INSENSITIVE, OBJ_DONT_REPARSE,
        UNICODE_STRING,
    },
    Storage::FileSystem::{
        GetFileInformationByHandle, ReadFile, BY_HANDLE_FILE_INFORMATION, FILE_ATTRIBUTE_DIRECTORY,
        FILE_ATTRIBUTE_NORMAL, FILE_ATTRIBUTE_REPARSE_POINT, FILE_READ_ATTRIBUTES, FILE_READ_DATA,
        FILE_SHARE_DELETE, FILE_SHARE_READ, FILE_SHARE_WRITE, FILE_TRAVERSE, SYNCHRONIZE,
    },
    System::IO::IO_STATUS_BLOCK,
};
use zeroize::Zeroizing;

#[derive(Clone, Copy, Debug)]
#[cfg_attr(not(test), allow(dead_code))]
pub(crate) enum Step {
    BeforeOpen(usize),
    BeforeRead { requested: usize },
}
type ReadResult = Result<Zeroizing<Vec<u8>>, ReadError>;

// The optional hook is per invocation, not a global callback or production API.
// It makes path-replacement/growth/cancellation schedules deterministic in tests.
pub(crate) fn read_with_hook(
    root: &Path,
    components: &[OsString],
    cap: usize,
    expected: &[u8; 32],
    canceled: &AtomicBool,
    mut hook: impl FnMut(Step) -> Result<(), ReadError>,
) -> ReadResult {
    validate(root, components, cap)?;
    let mut owned: Vec<OwnedHandle> = Vec::with_capacity(2);
    let result = read_inner(
        root, components, cap, expected, canceled, &mut hook, &mut owned,
    );
    let mut failures = Vec::new();
    while let Some(handle) = owned.pop() {
        if let Err(code) = close_once(handle) {
            failures.push(code)
        }
    }
    match result {
        Err(mut failure) => {
            failure.cleanup_failures.extend(failures);
            Err(failure)
        }
        Ok(bytes) if failures.is_empty() => Ok(bytes),
        Ok(bytes) => {
            drop(bytes);
            Err(ReadError {
                category: Category::UnsafeObjectOrIo,
                operation: "close",
                native: failures.first().copied(),
                cleanup_failures: failures,
            })
        }
    }
}

fn read_inner(
    root: &Path,
    components: &[OsString],
    cap: usize,
    expected: &[u8; 32],
    canceled: &AtomicBool,
    hook: &mut impl FnMut(Step) -> Result<(), ReadError>,
    owned: &mut Vec<OwnedHandle>,
) -> ReadResult {
    check_cancel(canceled, "before-root-open")?;
    let native_root = native_root_name(root)?;
    owned.push(open_native(null_mut(), &native_root, true, "open-root")?);
    inspect(owned.last().unwrap(), true, "stat-root")?;
    for (index, component) in components.iter().enumerate() {
        check_cancel(canceled, "before-component-open")?;
        hook(Step::BeforeOpen(index))?;
        check_cancel(canceled, "after-component-hook")?;
        let directory = index + 1 < components.len();
        let operation = if directory {
            "open-component"
        } else {
            "open-final"
        };
        let next = open_native(
            owned.last().unwrap().as_raw_handle(),
            component,
            directory,
            operation,
        )?;
        // The new handle is retained before releasing its parent. Only the next
        // parent is needed; the number of owned handles stays bounded by two.
        owned.push(next);
        if directory {
            inspect(owned.last().unwrap(), true, "stat-component")?;
            let previous = owned.remove(0);
            close_once(previous).map_err(|code| {
                ReadError::new(Category::UnsafeObjectOrIo, "close-parent", Some(code))
            })?;
        }
    }
    let final_handle = owned.last().unwrap();
    let info = inspect(final_handle, false, "stat-final")?;
    let size = ((info.nFileSizeHigh as u64) << 32) | info.nFileSizeLow as u64;
    if size > cap as u64 {
        return Err(ReadError::new(Category::TooLarge, "stat-final", None));
    }
    let mut bytes = Zeroizing::new(Vec::new());
    bytes
        .try_reserve_exact(size as usize)
        .map_err(|_| ReadError::new(Category::UnsafeObjectOrIo, "reserve", None))?;
    let mut chunk = Zeroizing::new([0u8; 4096]);
    loop {
        check_cancel(canceled, "before-read")?;
        // One sentinel byte, including at the cap, detects growth after stat.
        let requested = (cap - bytes.len() + 1).min(chunk.len());
        hook(Step::BeforeRead { requested })?;
        check_cancel(canceled, "after-read-hook")?;
        let mut count = 0;
        // SAFETY: original synchronous handle and writable requested-byte span;
        // the stack output/count remain alive, no OVERLAPPED ownership is needed.
        let ok = unsafe {
            ReadFile(
                final_handle.as_raw_handle(),
                chunk.as_mut_ptr(),
                requested as u32,
                &mut count,
                null_mut(),
            )
        };
        let error = if ok == 0 {
            Some(std::io::Error::last_os_error().raw_os_error().unwrap_or(0))
        } else {
            None
        };
        if count as usize > cap - bytes.len() {
            return Err(ReadError::new(Category::TooLarge, "read-final", None));
        }
        if let Some(code) = error {
            if code == ERROR_HANDLE_EOF as i32 {
                break;
            }
            return Err(ReadError::new(
                Category::UnsafeObjectOrIo,
                "read-final",
                Some(NativeCode::Win32(code)),
            ));
        }
        if count == 0 {
            break;
        }
        bytes.extend_from_slice(&chunk[..count as usize]);
    }
    let actual: [u8; 32] = Sha256::digest(&*bytes).into();
    if actual != *expected {
        return Err(ReadError::new(
            Category::ChecksumMismatch,
            "verify-checksum",
            None,
        ));
    }
    Ok(bytes)
}

fn validate(root: &Path, components: &[OsString], cap: usize) -> Result<(), ReadError> {
    if !root.is_absolute()
        || root.components().any(|c| matches!(c, Component::ParentDir))
        || components.is_empty()
        || cap == 0
        || cap > MAX_STATE_BYTES
    {
        return Err(ReadError::new(Category::InvalidInput, "validate", None));
    }
    for component in components {
        if component.is_empty()
            || component == "."
            || component == ".."
            || component
                .encode_wide()
                .any(|c| c == 0 || c == b'/' as u16 || c == b'\\' as u16)
        {
            return Err(ReadError::new(
                Category::InvalidInput,
                "validate-component",
                None,
            ));
        }
    }
    Ok(())
}

fn native_root_name(root: &Path) -> Result<OsString, ReadError> {
    let mut path: Vec<u16> = root.as_os_str().encode_wide().collect();
    if path.contains(&0) {
        return Err(ReadError::new(Category::InvalidInput, "root-name", None));
    }
    for unit in &mut path {
        if *unit == b'/' as u16 {
            *unit = b'\\' as u16
        }
    }
    // Narrow first slice: drive-absolute paths only. UNC/device roots require
    // their own contract/qualification; do not canonicalize and silently follow.
    if path.len() < 3
        || path[1] != b':' as u16
        || path[2] != b'\\' as u16
        || !((b'A' as u16..=b'Z' as u16).contains(&path[0])
            || (b'a' as u16..=b'z' as u16).contains(&path[0]))
    {
        return Err(ReadError::new(
            Category::Unsupported,
            "root-namespace",
            None,
        ));
    }
    let mut native: Vec<u16> = "\\??\\".encode_utf16().collect();
    native.extend(path);
    Ok(OsString::from_wide(&native))
}

fn open_native(
    parent: HANDLE,
    name: &OsStr,
    directory: bool,
    operation: &'static str,
) -> Result<OwnedHandle, ReadError> {
    let mut wide: Vec<u16> = name.encode_wide().collect();
    if wide.is_empty() || wide.contains(&0) || wide.len() > 32766 {
        return Err(ReadError::new(Category::InvalidInput, "native-name", None));
    }
    let length = (wide.len() * 2) as u16;
    wide.push(0);
    let unicode = UNICODE_STRING {
        Length: length,
        MaximumLength: length + 2,
        Buffer: wide.as_mut_ptr(),
    };
    // SAFETY: Windows POD zero state represents absent optional descriptors.
    let mut attributes: OBJECT_ATTRIBUTES = unsafe { zeroed() };
    attributes.Length = size_of::<OBJECT_ATTRIBUTES>() as u32;
    attributes.RootDirectory = parent;
    attributes.ObjectName = &unicode;
    attributes.Attributes = OBJ_CASE_INSENSITIVE | OBJ_DONT_REPARSE;
    let mut iosb: IO_STATUS_BLOCK = unsafe { zeroed() };
    let mut handle: HANDLE = null_mut();
    let access = if directory {
        FILE_TRAVERSE | FILE_READ_ATTRIBUTES
    } else {
        FILE_READ_DATA | FILE_READ_ATTRIBUTES
    };
    let options = FILE_SYNCHRONOUS_IO_NONALERT
        | FILE_OPEN_REPARSE_POINT
        | if directory {
            FILE_DIRECTORY_FILE
        } else {
            FILE_NON_DIRECTORY_FILE
        };
    // SAFETY: counted UTF-16, attributes and IO_STATUS_BLOCK remain live. The
    // parent is the retained directory handle, not a path later reopened.
    let status = unsafe {
        NtCreateFile(
            &mut handle,
            access | SYNCHRONIZE,
            &attributes,
            &mut iosb,
            null(),
            FILE_ATTRIBUTE_NORMAL,
            FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
            FILE_OPEN,
            options,
            null(),
            0,
        )
    };
    if status < 0 {
        return Err(ReadError::new(
            Category::UnsafeObjectOrIo,
            operation,
            Some(NativeCode::Nt(status)),
        ));
    }
    if handle.is_null() {
        return Err(ReadError::new(Category::UnsafeObjectOrIo, operation, None));
    }
    // SAFETY: successful NtCreateFile transferred one uniquely owned handle.
    Ok(unsafe { OwnedHandle::from_raw_handle(handle) })
}

fn inspect(
    handle: &OwnedHandle,
    directory: bool,
    operation: &'static str,
) -> Result<BY_HANDLE_FILE_INFORMATION, ReadError> {
    // SAFETY: initialized output for the exact Win32 information call.
    let mut info: BY_HANDLE_FILE_INFORMATION = unsafe { zeroed() };
    if unsafe { GetFileInformationByHandle(handle.as_raw_handle(), &mut info) } == 0 {
        return Err(ReadError::new(
            Category::UnsafeObjectOrIo,
            operation,
            Some(NativeCode::Win32(
                std::io::Error::last_os_error().raw_os_error().unwrap_or(0),
            )),
        ));
    }
    if info.dwFileAttributes & FILE_ATTRIBUTE_REPARSE_POINT != 0
        || (info.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY != 0) != directory
    {
        return Err(ReadError::new(Category::UnsafeObjectOrIo, operation, None));
    }
    Ok(info)
}

fn close_once(handle: OwnedHandle) -> Result<(), NativeCode> {
    let raw = handle.into_raw_handle();
    // SAFETY: unique ownership consumed. Even on failure the numeric handle is
    // not retried, because a failed close is not authority over a reused handle.
    if unsafe { CloseHandle(raw) } == 0 {
        Err(NativeCode::Win32(
            std::io::Error::last_os_error().raw_os_error().unwrap_or(0),
        ))
    } else {
        Ok(())
    }
}
fn check_cancel(canceled: &AtomicBool, operation: &'static str) -> Result<(), ReadError> {
    if canceled.load(Ordering::Acquire) {
        Err(ReadError::new(Category::Canceled, operation, None))
    } else {
        Ok(())
    }
}

#[cfg(test)]
#[path = "read_tests.rs"]
mod tests;

use crate::NativeFailure;
use std::{
    error::Error,
    fmt, io,
    mem::{align_of, size_of, size_of_val},
    os::windows::io::{
        AsHandle, AsRawHandle, BorrowedHandle, FromRawHandle, IntoRawHandle, OwnedHandle,
    },
    ptr::null_mut,
    slice,
};
use windows_sys::Win32::{
    Foundation::{CloseHandle, GetLastError},
    Security::{
        GetSidLengthRequired, GetTokenInformation, IsValidSid, TokenUser, SECURITY_MAX_SID_SIZE,
        SID, TOKEN_QUERY, TOKEN_USER,
    },
    System::Threading::OpenProcessToken,
};

/// A token-user observation and its explicit token-close outcome.
/// A cleanup failure never carries a successful SID value.
#[derive(Debug)]
pub enum TokenUserError {
    Observation(io::Error),
    Cleanup {
        observation: Option<io::Error>,
        failure: NativeFailure,
    },
}

impl fmt::Display for TokenUserError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Observation(error) => write!(formatter, "TokenUser observation failed: {error}"),
            Self::Cleanup {
                observation,
                failure,
            } => {
                if let Some(error) = observation {
                    write!(formatter, "TokenUser observation failed: {error}; ")?;
                }
                write!(
                    formatter,
                    "{} failed with Windows error {}",
                    failure.operation, failure.code
                )
            }
        }
    }
}

impl Error for TokenUserError {
    fn source(&self) -> Option<&(dyn Error + 'static)> {
        match self {
            Self::Observation(error)
            | Self::Cleanup {
                observation: Some(error),
                ..
            } => Some(error),
            Self::Cleanup {
                observation: None, ..
            } => None,
        }
    }
}

pub(crate) fn process_token_user_sid(
    process: BorrowedHandle<'_>,
) -> Result<Vec<u8>, TokenUserError> {
    let mut raw_token = null_mut();
    // SAFETY: the original process remains borrowed for this synchronous call;
    // the output is writable, and only query access to its primary token is asked.
    if unsafe { OpenProcessToken(process.as_raw_handle(), TOKEN_QUERY, &mut raw_token) } == 0 {
        return Err(TokenUserError::Observation(io::Error::last_os_error()));
    }
    // SAFETY: successful OpenProcessToken returned this one owned token handle.
    let token = unsafe { OwnedHandle::from_raw_handle(raw_token) };
    owned_token_user_sid(token)
}

pub(crate) fn owned_token_user_sid(token: OwnedHandle) -> Result<Vec<u8>, TokenUserError> {
    // Keep every ordinary observation error until explicit cleanup completes.
    // Drop remains only a best-effort fallback if unwinding interrupts the query.
    let observation = query_token_user_sid(token.as_handle());
    let raw_token = token.into_raw_handle();
    // SAFETY: ownership was consumed before this one close attempt. On failure
    // no OwnedHandle remains to retry a potentially invalid/recycled number.
    let close_failure = if unsafe { CloseHandle(raw_token) } == 0 {
        // SAFETY: capture the native failure immediately on the calling thread.
        let code = unsafe { GetLastError() } as i32;
        Some(NativeFailure {
            operation: "CloseToken",
            code,
        })
    } else {
        None
    };
    match (observation, close_failure) {
        (Ok(sid), None) => Ok(sid),
        (Err(error), None) => Err(TokenUserError::Observation(error)),
        (result, Some(failure)) => Err(TokenUserError::Cleanup {
            observation: result.err(),
            failure,
        }),
    }
}

// Microsoft's SE_TOKEN_USER documents TOKEN_USER plus SECURITY_MAX_SID_SIZE as
// enough for the maximum GetTokenInformation(TokenUser) result, suitable for
// stack allocation. Word storage supplies native alignment without heap growth.
// https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_se_token_user
const TOKEN_USER_WORDS: usize =
    (size_of::<TOKEN_USER>() + SECURITY_MAX_SID_SIZE as usize).div_ceil(size_of::<usize>());
type TokenUserBuffer = [usize; TOKEN_USER_WORDS];
const _: () = assert!(align_of::<usize>() >= align_of::<TOKEN_USER>());
const _: () = assert!(align_of::<usize>() >= align_of::<SID>());

fn query_token_user_sid(token: BorrowedHandle<'_>) -> io::Result<Vec<u8>> {
    let mut buffer: TokenUserBuffer = [0; TOKEN_USER_WORDS];
    let mut returned = 0;
    // SAFETY: the borrowed token stays live; storage is initialized, aligned,
    // fixed in place and writable for the supplied byte count; returned is writable.
    if unsafe {
        GetTokenInformation(
            token.as_raw_handle(),
            TokenUser,
            buffer.as_mut_ptr().cast(),
            size_of_val(&buffer) as u32,
            &mut returned,
        )
    } == 0
    {
        return Err(io::Error::last_os_error());
    }
    copy_token_user_sid(&buffer, returned as usize)
}

fn copy_token_user_sid(buffer: &TokenUserBuffer, returned: usize) -> io::Result<Vec<u8>> {
    let header_length = size_of::<TOKEN_USER>();
    if returned < header_length || returned > size_of_val(buffer) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "TokenUser returned length is outside its buffer",
        ));
    }
    // SAFETY: the complete initialized header is within aligned owned storage.
    // TOKEN_USER contains only a raw pointer and integer, with no invalid bit patterns.
    let user = unsafe { buffer.as_ptr().cast::<TOKEN_USER>().read() };
    let base = buffer.as_ptr().cast::<u8>();
    let sid_address = user.User.Sid as usize;
    let offset = sid_address.checked_sub(base as usize);
    let Some(offset) = offset.filter(|offset| {
        !user.User.Sid.is_null()
            && *offset >= header_length
            && *offset <= returned
            && sid_address.is_multiple_of(align_of::<SID>())
    }) else {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "TokenUser SID pointer is outside or misaligned in its buffer",
        ));
    };

    // SAFETY: this length-only native function cannot fail and dereferences no
    // pointer. Zero subauthorities gives the variable-length SID's header size,
    // unlike size_of::<SID>(), which includes one trailing subauthority slot.
    let minimum_length = unsafe { GetSidLengthRequired(0) } as usize;
    if offset
        .checked_add(minimum_length)
        .is_none_or(|end| end > returned)
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "TokenUser SID header is truncated",
        ));
    }
    // SAFETY: returned is within the initialized word buffer; the byte view is
    // read-only and remains borrowed until the independent SID copy is complete.
    let bytes = unsafe { slice::from_raw_parts(base, returned) };
    let count = bytes[offset + 1];
    // Every subauthority is one DWORD (u32). Derive the count limit from the SDK
    // maximum and native fixed header; windows-sys does not export the SDK's
    // SID_MAX_SUB_AUTHORITIES macro.
    let maximum_count = (SECURITY_MAX_SID_SIZE as usize - minimum_length) / size_of::<u32>();
    if usize::from(count) > maximum_count {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "TokenUser SID has too many subauthorities",
        ));
    }
    // SAFETY: count is within the SID's supported maximum; this computes a size.
    let sid_length = unsafe { GetSidLengthRequired(count) } as usize;
    let end = offset.checked_add(sid_length);
    let Some(end) = end.filter(|end| {
        sid_length >= minimum_length
            && sid_length <= SECURITY_MAX_SID_SIZE as usize
            && *end <= returned
    }) else {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "TokenUser SID extends beyond the returned buffer",
        ));
    };
    // SAFETY: derive the pointer from our allocation, not the untrusted header
    // pointer. Its alignment, fixed header, count and full extent were checked.
    // IsValidSid only reads this storage despite its mutable-pointer signature.
    let valid = unsafe { IsValidSid(base.add(offset).cast_mut().cast()) };
    if valid == 0 {
        // IsValidSid has no extended error information: never read GetLastError.
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "TokenUser SID is not valid",
        ));
    }
    let mut sid = Vec::new();
    sid.try_reserve_exact(sid_length)
        .map_err(|_| io::Error::from(io::ErrorKind::OutOfMemory))?;
    sid.extend_from_slice(&bytes[offset..end]);
    Ok(sid)
}

#[cfg(test)]
mod tests {
    use super::*;
    use windows_sys::Win32::Security::{InitializeSid, SID_IDENTIFIER_AUTHORITY};

    // Initialize through the caller's borrow. Returning a pointer-containing
    // buffer by value would leave its embedded SID pointer at the old address.
    fn initialize_fixture(buffer: &mut TokenUserBuffer, count: u8) -> usize {
        let header_length = size_of::<TOKEN_USER>();
        // SAFETY: a supported count; this native function only computes a size.
        let sid_length = unsafe { GetSidLengthRequired(count) } as usize;
        assert!(header_length + sid_length <= size_of_val(buffer));
        // SAFETY: the buffer is aligned and the complete SID extent fits above.
        let sid = unsafe { buffer.as_mut_ptr().cast::<u8>().add(header_length) };
        let authority = SID_IDENTIFIER_AUTHORITY {
            Value: [0, 0, 0, 0, 0, 5],
        };
        // SAFETY: initialized writable storage for this count and valid authority.
        assert_ne!(unsafe { InitializeSid(sid.cast(), &authority, count) }, 0);
        // SAFETY: the header is initialized, aligned and disjoint from its SID.
        unsafe { (*buffer.as_mut_ptr().cast::<TOKEN_USER>()).User.Sid = sid.cast() };
        header_length + sid_length
    }

    fn assert_invalid(buffer: &TokenUserBuffer, returned: usize) {
        let error = copy_token_user_sid(buffer, returned).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::InvalidData);
        assert_eq!(error.raw_os_error(), None);
    }

    #[test]
    fn copies_minimum_and_maximum_valid_sid_extents() {
        // SID's fixed header is 8 bytes; each of 15 maximum subauthorities adds
        // one DWORD, reaching the SDK's SECURITY_MAX_SID_SIZE of 68 bytes.
        for (count, expected_length) in [(0, 8), (15, 68)] {
            let mut buffer: TokenUserBuffer = [0; TOKEN_USER_WORDS];
            let returned = initialize_fixture(&mut buffer, count);
            let sid = copy_token_user_sid(&buffer, returned).unwrap();
            let mut expected = vec![0; expected_length];
            expected[0] = 1; // SID_REVISION from the Windows SID definition.
            expected[1] = count;
            expected[7] = 5; // SECURITY_NT_AUTHORITY used by the native fixture.
            assert_eq!(sid, expected);
        }
    }

    #[test]
    fn rejects_short_headers_and_lengths_outside_returned_storage() {
        let mut buffer: TokenUserBuffer = [0; TOKEN_USER_WORDS];
        let returned = initialize_fixture(&mut buffer, 1);
        let header_length = size_of::<TOKEN_USER>();
        for malformed_length in [
            0,
            header_length - 1,
            size_of_val(&buffer) + 1,
            header_length,
            header_length + 7,
            returned - 1,
        ] {
            assert_invalid(&buffer, malformed_length);
        }
    }

    #[test]
    fn rejects_null_foreign_overlapping_and_misaligned_sid_pointers() {
        for case in 0..7 {
            let mut buffer: TokenUserBuffer = [0; TOKEN_USER_WORDS];
            let returned = initialize_fixture(&mut buffer, 1);
            let base = buffer.as_mut_ptr().cast::<u8>();
            let pointer = match case {
                0 => null_mut(),
                1 => base.wrapping_sub(align_of::<SID>()),
                2 => base,
                3 => base.wrapping_add(returned),
                4 => base.wrapping_add(size_of_val(&buffer)),
                5 => base.wrapping_add(size_of::<TOKEN_USER>() + 1),
                _ => std::ptr::without_provenance_mut(usize::MAX),
            };
            // SAFETY: mutate only the initialized fixture header. The parser
            // must reject these raw values without dereferencing any of them.
            unsafe { (*buffer.as_mut_ptr().cast::<TOKEN_USER>()).User.Sid = pointer.cast() };
            assert_invalid(&buffer, returned);
        }
    }

    #[test]
    fn rejects_invalid_revision_excessive_count_and_truncated_subauthorities() {
        for (field, value) in [(0, 0), (1, 16), (1, u8::MAX), (1, 15)] {
            let mut buffer: TokenUserBuffer = [0; TOKEN_USER_WORDS];
            let returned = initialize_fixture(&mut buffer, 1);
            // SAFETY: both modified bytes are within the fixture's SID header.
            unsafe {
                *buffer
                    .as_mut_ptr()
                    .cast::<u8>()
                    .add(size_of::<TOKEN_USER>() + field) = value;
            }
            assert_invalid(&buffer, returned);
        }
    }
}

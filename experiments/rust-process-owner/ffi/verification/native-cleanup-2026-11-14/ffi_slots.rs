// Appended ONLY to a disposable FFI DLL source copy; never in the ordinary DLL.
// The test alone gets original slots to induce and recover its native failures.
/// # Safety
/// raw must be a live exclusively borrowed Owner from this DLL. slots points to
/// three writable aligned usize values disjoint from the owner. No pointers or
/// callbacks are retained. The fixture must obey each native slot's ownership.
#[no_mangle]
pub unsafe extern "C" fn mh_qualification_slots1(
    raw: *mut Owner, slots: *mut usize, count: usize,
) -> u32 {
    if raw.is_null() || !raw.is_aligned() || slots.is_null()
        || !slots.is_aligned() || count != 3 { return ARGUMENT; }
    // SAFETY: exclusively retained valid owner and pinned writable output from
    // the opt-in test; integer handles are copied without adopting or closing.
    unsafe {
        let values = (*raw).inner.qualification_original_slots();
        std::ptr::copy_nonoverlapping(values.as_ptr(), slots, 3);
    }
    OK
}

// Appended ONLY to a disposable copy of native src/windows.rs by run.ps1.
// No syscall, ownership transfer, close behavior or result mapping is changed.
impl NativeOwner {
    /// Qualification-only observation of original slots. Not a product API.
    /// The caller must not close handles still owned by this NativeOwner.
    pub fn qualification_original_slots(&self) -> [usize; 3] {
        [&self.thread, &self.process, &self.job]
            .map(|slot| slot.as_ref().map_or(0, |h| h.as_raw_handle() as usize))
    }
}

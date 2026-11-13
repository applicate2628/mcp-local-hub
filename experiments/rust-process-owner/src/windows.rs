use crate::{LaunchContext, NativeFailure, Settlement};
use std::{
    alloc::{alloc_zeroed, dealloc, Layout},
    ffi::{c_void, OsStr, OsString},
    io,
    mem::{size_of, zeroed},
    os::windows::{
        ffi::{OsStrExt, OsStringExt},
        io::{AsHandle, AsRawHandle, BorrowedHandle, FromRawHandle, IntoRawHandle, OwnedHandle},
    },
    path::{Path, PathBuf},
    ptr::{null, null_mut, NonNull},
    thread,
    time::{Duration, Instant},
};
use windows_sys::Win32::{
    Foundation::{
        CloseHandle, GetHandleInformation, ERROR_INSUFFICIENT_BUFFER, HANDLE, HANDLE_FLAG_INHERIT,
        INVALID_HANDLE_VALUE, WAIT_FAILED, WAIT_OBJECT_0, WAIT_TIMEOUT,
    },
    Storage::FileSystem::{GetFileType, FILE_TYPE_DISK, FILE_TYPE_PIPE},
    System::{
        JobObjects::{
            CreateJobObjectW, JobObjectBasicAccountingInformation,
            JobObjectExtendedLimitInformation, QueryInformationJobObject, SetInformationJobObject,
            TerminateJobObject, JOBOBJECT_BASIC_ACCOUNTING_INFORMATION,
            JOBOBJECT_EXTENDED_LIMIT_INFORMATION, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
        },
        Threading::{
            CreateProcessW, DeleteProcThreadAttributeList, GetExitCodeProcess, GetProcessId,
            InitializeProcThreadAttributeList, QueryFullProcessImageNameW, ResumeThread,
            UpdateProcThreadAttribute, WaitForSingleObject, CREATE_NO_WINDOW, CREATE_SUSPENDED,
            CREATE_UNICODE_ENVIRONMENT, EXTENDED_STARTUPINFO_PRESENT, LPPROC_THREAD_ATTRIBUTE_LIST,
            PROCESS_INFORMATION, PROC_THREAD_ATTRIBUTE_HANDLE_LIST, PROC_THREAD_ATTRIBUTE_JOB_LIST,
            STARTF_USESTDHANDLES, STARTUPINFOEXW,
        },
    },
};

// Every registered attribute value must outlive this list, including native
// deletion, not only CreateProcessW. Its backing allocation has native-heap-
// equivalent alignment and never moves while registered.
struct AttributeList {
    ptr: NonNull<c_void>,
    layout: Layout,
    initialized: bool,
}
impl AttributeList {
    fn new(count: u32) -> io::Result<Self> {
        let mut bytes = 0;
        // SAFETY: null is the documented size-query mode; bytes is writable.
        let queried =
            unsafe { InitializeProcThreadAttributeList(null_mut(), count, 0, &mut bytes) };
        let err = io::Error::last_os_error();
        if queried != 0
            || err.raw_os_error() != Some(ERROR_INSUFFICIENT_BUFFER as i32)
            || bytes == 0
        {
            return Err(io::Error::other(format!("attribute size query: {err}")));
        }
        let layout = Layout::from_size_align(bytes, 16).map_err(|_| io::ErrorKind::InvalidInput)?;
        // SAFETY: valid nonzero layout; allocation is released with this layout.
        let ptr = NonNull::new(unsafe { alloc_zeroed(layout) })
            .ok_or(io::ErrorKind::OutOfMemory)?
            .cast::<c_void>();
        let mut list = Self {
            ptr,
            layout,
            initialized: false,
        };
        // SAFETY: allocated/aligned bytes from the size query, exclusively owned.
        if unsafe { InitializeProcThreadAttributeList(list.raw(), count, 0, &mut bytes) } == 0 {
            return Err(io::Error::last_os_error());
        }
        list.initialized = true;
        Ok(list)
    }
    fn raw(&self) -> LPPROC_THREAD_ATTRIBUTE_LIST {
        self.ptr.as_ptr()
    }
}
impl Drop for AttributeList {
    fn drop(&mut self) {
        // SAFETY: native initialization succeeded exactly when initialized is set;
        // deletion does not free our allocation. No native call is still active.
        unsafe {
            if self.initialized {
                DeleteProcThreadAttributeList(self.raw());
            }
            dealloc(self.ptr.as_ptr().cast(), self.layout);
        }
    }
}

/// Observation only: no public raw handle, closing operation, or ownership grant.
/// Its borrow cannot outlive the admission invocation.
///
/// ```compile_fail,E0521
/// use mcphub_process_owner_spike::NativeOwner;
/// fn retain_observation(owner: &mut NativeOwner) {
///     let mut escaped = None;
///     owner.admit_and_resume(|view| {
///         escaped = Some(view);
///         Ok(())
///     }).unwrap();
///     let _ = escaped.unwrap().process_id();
/// }
/// ```
pub struct ProcessView<'a> {
    handle: BorrowedHandle<'a>,
}
impl ProcessView<'_> {
    pub fn process_id(&self) -> io::Result<u32> {
        // SAFETY: borrowed live original process handle, no concurrent close.
        let id = unsafe { GetProcessId(self.handle.as_raw_handle()) };
        if id == 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(id)
        }
    }
    pub fn image_path(&self) -> io::Result<PathBuf> {
        let mut buffer = vec![0u16; 32768];
        let mut length = buffer.len() as u32;
        // SAFETY: buffer length is provided in UTF-16 code units; handle is live.
        if unsafe {
            QueryFullProcessImageNameW(
                self.handle.as_raw_handle(),
                0,
                buffer.as_mut_ptr(),
                &mut length,
            )
        } == 0
        {
            return Err(io::Error::last_os_error());
        }
        Ok(PathBuf::from(OsString::from_wide(
            &buffer[..length as usize],
        )))
    }
}

/// One owner of the original process/thread and its at-create Job membership.
/// Drop is a best-effort kill-on-close fallback, NOT a completed finish result.
#[must_use = "retain the owner until explicit finish reports a terminal result"]
pub struct NativeOwner {
    job: Option<OwnedHandle>,
    process: Option<OwnedHandle>,
    thread: Option<OwnedHandle>,
    pid: u32,
    admission_attempted: bool,
    root_exit: Option<u32>,
    termination: Option<(u32, Option<NativeFailure>)>,
    terminal: Option<Settlement>,
}

/// Explicit borrowed standard streams for one synchronous native create.
/// Source owners retain their handles; no flag mutation or ownership transfer.
/// All three sources must already be inheritable disk/pipe handles. Stdout and
/// stderr may refer to the same source; the inheritance allowlist is deduplicated.
/// The borrow expires after creation, not after process exit: OS handle copies
/// keep the child's resources alive independently of the parent's file objects.
///
/// ```compile_fail,E0505
/// use mcphub_process_owner_spike::{BorrowedStdio, NativeOwner};
/// use std::{fs::File, os::windows::io::AsHandle, path::Path};
/// fn release_source_before_create(file: File, image: &Path) {
///     let streams = BorrowedStdio {
///         stdin: file.as_handle(), stdout: file.as_handle(), stderr: file.as_handle(),
///     };
///     drop(file);
///     let _ = NativeOwner::create_suspended_with_stdio(image, &[], streams);
/// }
/// ```
pub struct BorrowedStdio<'a> {
    pub stdin: BorrowedHandle<'a>,
    pub stdout: BorrowedHandle<'a>,
    pub stderr: BorrowedHandle<'a>,
}

impl BorrowedStdio<'_> {
    fn validated_handles(&self) -> io::Result<[HANDLE; 3]> {
        let handles = [
            self.stdin.as_raw_handle(),
            self.stdout.as_raw_handle(),
            self.stderr.as_raw_handle(),
        ];
        for handle in handles {
            if handle.is_null() || handle == INVALID_HANDLE_VALUE {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "stdio requires a real file or pipe handle",
                ));
            }
            let mut flags = 0;
            // SAFETY: the source owner is borrowed throughout this synchronous
            // create. This observes flags only and never closes or changes it.
            if unsafe { GetHandleInformation(handle, &mut flags) } == 0 {
                return Err(io::Error::last_os_error());
            }
            if flags & HANDLE_FLAG_INHERIT == 0 {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "stdio source is not explicitly inheritable",
                ));
            }
            // SAFETY: borrowed live handle; this queries its native category.
            let kind = unsafe { GetFileType(handle) };
            if kind != FILE_TYPE_DISK && kind != FILE_TYPE_PIPE {
                return Err(io::Error::new(
                    io::ErrorKind::Unsupported,
                    "only disk/pipe stdio is qualified",
                ));
            }
        }
        Ok(handles)
    }
}

impl NativeOwner {
    /// Use a complete explicit environment and absolute child directory.
    /// Optional stdio has exactly the same borrowed contract as the stdio API.
    /// Context is validated before acquiring native owner resources. Its buffers
    /// live through creation; nothing changes the parent's environment or cwd.
    pub fn create_suspended_with_context(
        program: &Path,
        args: &[OsString],
        context: LaunchContext<'_>,
        stdio: Option<BorrowedStdio<'_>>,
    ) -> io::Result<Self> {
        Self::create_suspended_impl(program, args, stdio, Some(context))
    }
    /// Create with exactly these borrowed, already-inheritable disk/pipe
    /// sources. This does not change flags, duplicate or close parent handles.
    /// Source owners must not change native flags while creation is in progress.
    /// On return the child owns its OS copies; the Rust owner retains no source.
    pub fn create_suspended_with_stdio(
        program: &Path,
        args: &[OsString],
        stdio: BorrowedStdio<'_>,
    ) -> io::Result<Self> {
        Self::create_suspended_impl(program, args, Some(stdio), None)
    }

    pub fn create_suspended(program: &Path, args: &[OsString]) -> io::Result<Self> {
        Self::create_suspended_impl(program, args, None, None)
    }

    fn create_suspended_impl(
        program: &Path,
        args: &[OsString],
        stdio: Option<BorrowedStdio<'_>>,
        context: Option<LaunchContext<'_>>,
    ) -> io::Result<Self> {
        if !program.is_absolute() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "absolute image path required",
            ));
        }
        let application = wide_z(program.as_os_str())?;
        let mut command = command_line(program.as_os_str(), args)?;
        let prepared = context.as_ref().map(LaunchContext::prepare).transpose()?;
        // Validate before acquiring any native owner or starting a child.
        let stdio_handles = stdio
            .as_ref()
            .map(BorrowedStdio::validated_handles)
            .transpose()?;
        let mut inherited = Vec::new();
        if let Some(handles) = stdio_handles {
            for handle in handles {
                if !inherited.contains(&handle) {
                    inherited.push(handle);
                }
            }
        }
        // SAFETY: unnamed Job with default security; successful handle is unique.
        let raw_job = unsafe { CreateJobObjectW(null(), null()) };
        if raw_job.is_null() {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: valid owned kernel handle returned by CreateJobObjectW.
        let job = unsafe { OwnedHandle::from_raw_handle(raw_job) };
        // SAFETY: Win32 POD structure has valid all-zero initial representation.
        let mut limits: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = unsafe { zeroed() };
        limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        // SAFETY: exact class/struct/size; job remains open throughout the call.
        if unsafe {
            SetInformationJobObject(
                raw_job,
                JobObjectExtendedLimitInformation,
                (&limits as *const JOBOBJECT_EXTENDED_LIMIT_INFORMATION).cast(),
                size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
            )
        } == 0
        {
            return Err(io::Error::last_os_error());
        }
        // Declare all attribute backing before the list so reverse local drop
        // order destroys the native list before either handle array expires.
        let jobs = [raw_job];
        let attributes = AttributeList::new(if stdio_handles.is_some() { 2 } else { 1 })?;
        // SAFETY: the one-element array and Job outlive the attribute list; the
        // attribute performs membership assignment at creation, not post-spawn.
        if unsafe {
            UpdateProcThreadAttribute(
                attributes.raw(),
                0,
                PROC_THREAD_ATTRIBUTE_JOB_LIST as usize,
                jobs.as_ptr().cast(),
                size_of::<HANDLE>(),
                null_mut(),
                null(),
            )
        } == 0
        {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: Win32 POD structures permit zeroed optional fields.
        let mut startup: STARTUPINFOEXW = unsafe { zeroed() };
        startup.StartupInfo.cb = size_of::<STARTUPINFOEXW>() as u32;
        startup.lpAttributeList = attributes.raw();
        if let Some([input, output, error]) = stdio_handles {
            // SAFETY: distinct handles remain borrowed and inheritable across
            // CreateProcessW. The backing list is not moved/reallocated while
            // registered. This restricts inheritance for THIS child only.
            if unsafe {
                UpdateProcThreadAttribute(
                    attributes.raw(),
                    0,
                    PROC_THREAD_ATTRIBUTE_HANDLE_LIST as usize,
                    inherited.as_ptr().cast(),
                    std::mem::size_of_val(inherited.as_slice()),
                    null_mut(),
                    null(),
                )
            } == 0
            {
                return Err(io::Error::last_os_error());
            }
            startup.StartupInfo.dwFlags |= STARTF_USESTDHANDLES;
            startup.StartupInfo.hStdInput = input;
            startup.StartupInfo.hStdOutput = output;
            startup.StartupInfo.hStdError = error;
        }
        let mut created: PROCESS_INFORMATION = unsafe { zeroed() };
        let mut flags = CREATE_SUSPENDED | CREATE_NO_WINDOW | EXTENDED_STARTUPINFO_PRESENT;
        let (environment, directory) = if let Some(ref prepared) = prepared {
            flags |= CREATE_UNICODE_ENVIRONMENT;
            (
                prepared.environment.as_ptr().cast::<c_void>(),
                prepared.directory.as_ptr(),
            )
        } else {
            (null(), null())
        };
        // SAFETY: all UTF-16 buffers are NUL-terminated and command is writable;
        // attribute backing, context buffers and Job are live; native flags match buffers.
        // Optional inheritance uses only the validated explicit HANDLE_LIST;
        // existing no-stdio creation keeps inheritance disabled.
        let ok = unsafe {
            CreateProcessW(
                application.as_ptr(),
                command.as_mut_ptr(),
                null(),
                null(),
                i32::from(stdio_handles.is_some()),
                flags,
                environment,
                directory,
                &startup.StartupInfo,
                &mut created,
            )
        };
        if ok == 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: on success CreateProcessW returns two distinct owned handles.
        let process = unsafe { OwnedHandle::from_raw_handle(created.hProcess) };
        let thread = unsafe { OwnedHandle::from_raw_handle(created.hThread) };
        Ok(Self {
            job: Some(job),
            process: Some(process),
            thread: Some(thread),
            pid: created.dwProcessId,
            admission_attempted: false,
            root_exit: None,
            termination: None,
            terminal: None,
        })
    }

    pub fn root_pid(&self) -> u32 {
        self.pid
    }
    pub fn root_exit(&self) -> Option<u32> {
        self.root_exit
    }

    pub fn active_processes(&self) -> io::Result<u32> {
        let job = self.job.as_ref().ok_or(io::ErrorKind::InvalidInput)?;
        // SAFETY: exact POD buffer/class; job borrowed while self stays alive.
        let mut info: JOBOBJECT_BASIC_ACCOUNTING_INFORMATION = unsafe { zeroed() };
        if unsafe {
            QueryInformationJobObject(
                job.as_raw_handle(),
                JobObjectBasicAccountingInformation,
                (&mut info as *mut JOBOBJECT_BASIC_ACCOUNTING_INFORMATION).cast(),
                size_of::<JOBOBJECT_BASIC_ACCOUNTING_INFORMATION>() as u32,
                null_mut(),
            )
        } == 0
        {
            return Err(io::Error::last_os_error());
        }
        Ok(info.ActiveProcesses)
    }

    // A rejected decision consumes the single admission attempt. The original
    // suspended owner remains available only for observation/stop/settlement.
    pub fn admit_and_resume(
        &mut self,
        check: impl FnOnce(ProcessView<'_>) -> io::Result<()>,
    ) -> io::Result<()> {
        if self.admission_attempted || self.termination.is_some() || self.terminal.is_some() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "admission is single-use",
            ));
        }
        self.admission_attempted = true;
        let process = self.process.as_ref().ok_or(io::ErrorKind::InvalidInput)?;
        check(ProcessView {
            handle: process.as_handle(),
        })?;
        if self.wait_root(Duration::ZERO)?.is_some() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "process exited before resume",
            ));
        }
        let thread = self.thread.as_ref().ok_or(io::ErrorKind::InvalidInput)?;
        // SAFETY: retained original primary-thread handle, exclusively borrowed.
        let previous = unsafe { ResumeThread(thread.as_raw_handle()) };
        if previous == u32::MAX {
            return Err(io::Error::last_os_error());
        }
        if previous != 1 {
            return Err(io::Error::other("unexpected original suspend count"));
        }
        Ok(())
    }

    pub fn wait_root(&mut self, timeout: Duration) -> io::Result<Option<u32>> {
        if self.root_exit.is_some() {
            return Ok(self.root_exit);
        }
        let process = self.process.as_ref().ok_or(io::ErrorKind::InvalidInput)?;
        // SAFETY: live original handle; finite timeout, never INFINITE by overflow.
        match unsafe { WaitForSingleObject(process.as_raw_handle(), finite_millis(timeout)) } {
            WAIT_TIMEOUT => Ok(None),
            WAIT_FAILED => Err(io::Error::last_os_error()),
            WAIT_OBJECT_0 => {
                let mut code = 0;
                // SAFETY: signaled process and valid output. Exit code 259 remains
                // a real exit code because the wait, not its numeric value, proved exit.
                if unsafe { GetExitCodeProcess(process.as_raw_handle(), &mut code) } == 0 {
                    return Err(io::Error::last_os_error());
                }
                self.root_exit = Some(code);
                Ok(Some(code))
            }
            _ => Err(io::Error::other("unexpected process wait result")),
        }
    }

    pub fn terminate(&mut self, code: u32) -> io::Result<()> {
        if let Some((previous, failure)) = &self.termination {
            if *previous != code {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "termination code cannot change",
                ));
            }
            return match failure {
                Some(e) => Err(io::Error::from_raw_os_error(e.code)),
                None => Ok(()),
            };
        }
        let job = self.job.as_ref().ok_or(io::ErrorKind::InvalidInput)?;
        // SAFETY: this owner's Job only. No PID lookup or foreign process action.
        let ok = unsafe { TerminateJobObject(job.as_raw_handle(), code) };
        let failure = if ok == 0 {
            Some(NativeFailure {
                operation: "TerminateJobObject",
                code: io::Error::last_os_error().raw_os_error().unwrap_or(0),
            })
        } else {
            None
        };
        self.termination = Some((code, failure.clone()));
        match failure {
            Some(e) => Err(io::Error::from_raw_os_error(e.code)),
            None => Ok(()),
        }
    }

    pub fn finish(&mut self, timeout: Duration) -> io::Result<Settlement> {
        if let Some(result) = &self.terminal {
            return Ok(result.clone());
        }
        let deadline = Instant::now()
            .checked_add(timeout)
            .ok_or(io::ErrorKind::InvalidInput)?;
        if self
            .wait_root(deadline.saturating_duration_since(Instant::now()))?
            .is_none()
        {
            return Ok(Settlement::Pending {
                root_exit: None,
                active_processes: None,
            });
        }
        loop {
            let active = self.active_processes()?;
            if active == 0 {
                break;
            }
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Ok(Settlement::Pending {
                    root_exit: self.root_exit,
                    active_processes: Some(active),
                });
            }
            thread::sleep(remaining.min(Duration::from_millis(2)));
        }
        let exit = self.root_exit.expect("root exit observed above");
        let mut failures = Vec::new();
        for (name, slot) in [
            ("CloseThread", &mut self.thread),
            ("CloseProcess", &mut self.process),
            ("CloseJob", &mut self.job),
        ] {
            if let Some(handle) = slot.take() {
                let raw = handle.into_raw_handle();
                // SAFETY: uniquely owned handle; this is its only close attempt.
                // On native failure, do NOT retry a numeric handle that might be
                // invalid/recycled. Preserve the ambiguous cleanup result instead.
                if unsafe { CloseHandle(raw) } == 0 {
                    failures.push(NativeFailure {
                        operation: name,
                        code: io::Error::last_os_error().raw_os_error().unwrap_or(0),
                    });
                }
            }
        }
        let result = if failures.is_empty() {
            Settlement::Complete { root_exit: exit }
        } else {
            Settlement::CleanupFailed {
                root_exit: exit,
                failures,
            }
        };
        self.terminal = Some(result.clone());
        Ok(result)
    }
}

impl Drop for NativeOwner {
    fn drop(&mut self) {
        // Deliberately no unbounded wait or assertion of completion in Drop.
        // Close the Job first: kill-on-close requests termination of remaining
        // members. Explicit finish is the only source of a Complete result.
        drop(self.job.take());
    }
}

fn wide_z(value: &OsStr) -> io::Result<Vec<u16>> {
    let mut data: Vec<_> = value.encode_wide().collect();
    if data.contains(&0) {
        return Err(io::Error::new(io::ErrorKind::InvalidInput, "embedded NUL"));
    }
    data.push(0);
    Ok(data)
}

// Quote every argument using the Windows CRT backslash/quote rules. Values
// remain UTF-16; no lossy UTF-8 conversion of paths or arguments is performed.
fn command_line(program: &OsStr, args: &[OsString]) -> io::Result<Vec<u16>> {
    let mut out = Vec::new();
    for (index, argument) in std::iter::once(program)
        .chain(args.iter().map(OsString::as_os_str))
        .enumerate()
    {
        if index != 0 {
            out.push(b' ' as u16);
        }
        out.push(b'"' as u16);
        let mut slashes = 0;
        for unit in argument.encode_wide() {
            if unit == 0 {
                return Err(io::Error::new(io::ErrorKind::InvalidInput, "embedded NUL"));
            }
            if unit == b'\\' as u16 {
                slashes += 1;
                continue;
            }
            if unit == b'"' as u16 {
                out.extend(std::iter::repeat_n(b'\\' as u16, slashes * 2 + 1));
            } else {
                out.extend(std::iter::repeat_n(b'\\' as u16, slashes));
            }
            slashes = 0;
            out.push(unit);
        }
        out.extend(std::iter::repeat_n(b'\\' as u16, slashes * 2));
        out.push(b'"' as u16);
    }
    if out.len() >= 32767 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "command line too long",
        ));
    }
    out.push(0);
    Ok(out)
}

fn finite_millis(duration: Duration) -> u32 {
    duration.as_millis().min(u32::MAX as u128 - 1) as u32
}

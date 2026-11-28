//! Per-child native arguments, never a mutation of the parent's process state.
use std::{
    ffi::{OsStr, OsString},
    io,
    os::windows::ffi::OsStrExt,
    path::Path,
};
use windows_sys::Win32::Globalization::{CompareStringOrdinal, CSTR_LESS_THAN};

// Experimental input budget, not a claimed Windows environment size limit.
const MAX_ENVIRONMENT_UNITS: usize = 1 << 20;
const MAX_DIRECTORY_UNITS: usize = 32767;

/// Complete explicit environment and absolute working directory for one create.
/// Empty environment means empty, never implicit inheritance or parent merge.
/// Entries must be strictly sorted by Windows case-insensitive ordinal name;
/// duplicates, NULs and '=' in ordinary names are rejected, not normalized.
/// Hidden per-drive '=X:' entries are not supported by this bounded first slice.
/// Values and paths preserve Windows UTF-16 units, including non-Unicode values.
/// No sandbox, directory-identity lock or removal of loader-injected variables
/// is implied. The caller remains responsible for child-specific required keys.
pub struct LaunchContext<'a> {
    pub current_dir: &'a Path,
    pub environment: &'a [(OsString, OsString)],
}

pub(crate) struct PreparedContext {
    pub(crate) directory: Vec<u16>,
    pub(crate) environment: Vec<u16>,
}

impl LaunchContext<'_> {
    pub(crate) fn prepare(&self) -> io::Result<PreparedContext> {
        if !self.current_dir.is_absolute() {
            return Err(invalid("absolute child working directory required"));
        }
        let mut directory = bounded_wide(self.current_dir.as_os_str(), MAX_DIRECTORY_UNITS - 1)?;
        directory
            .try_reserve(1)
            .map_err(|_| io::ErrorKind::OutOfMemory)?;
        directory.push(0);
        let mut environment = Vec::<u16>::new();
        let mut previous_name: Option<(usize, usize)> = None;
        for (name, value) in self.environment {
            let name = bounded_wide(name, MAX_ENVIRONMENT_UNITS)?;
            if name.is_empty() || name.contains(&(b'=' as u16)) {
                return Err(invalid(
                    "ordinary environment name must be nonempty and contain no '='",
                ));
            }
            let value = bounded_wide(value, MAX_ENVIRONMENT_UNITS)?;
            let additional = name.len() + value.len() + 2;
            if additional >= MAX_ENVIRONMENT_UNITS
                || environment.len() > MAX_ENVIRONMENT_UNITS - 1 - additional
            {
                return Err(invalid(
                    "explicit environment exceeds the experiment's input budget",
                ));
            }
            if let Some((start, length)) = previous_name {
                // SAFETY: nonempty live UTF-16 slices; both bounded well below
                // i32::MAX. Comparison uses the OS ordinal uppercase table,
                // not locale-dependent or Rust Unicode case conversion.
                let order = unsafe {
                    CompareStringOrdinal(
                        environment[start..start + length].as_ptr(),
                        length as i32,
                        name.as_ptr(),
                        name.len() as i32,
                        1,
                    )
                };
                if order == 0 {
                    return Err(io::Error::last_os_error());
                }
                if order != CSTR_LESS_THAN {
                    return Err(invalid(
                        "explicit environment names must be strictly ordinal sorted and unique",
                    ));
                }
            }
            environment
                .try_reserve(additional + 1)
                .map_err(|_| io::ErrorKind::OutOfMemory)?;
            previous_name = Some((environment.len(), name.len()));
            environment.extend_from_slice(&name);
            environment.push(b'=' as u16);
            environment.extend_from_slice(&value);
            environment.push(0);
        }
        // A non-null pointer to TWO zeros distinguishes explicit empty from
        // CreateProcessW's NULL (inherit) environment argument.
        environment
            .try_reserve(2)
            .map_err(|_| io::ErrorKind::OutOfMemory)?;
        if environment.is_empty() {
            environment.push(0);
        }
        environment.push(0);
        Ok(PreparedContext {
            directory,
            environment,
        })
    }
}

fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message)
}

fn bounded_wide(value: &OsStr, cap: usize) -> io::Result<Vec<u16>> {
    let length = value.encode_wide().take(cap + 1).count();
    if length > cap {
        return Err(invalid(
            "launch-context string exceeds the experiment's input budget",
        ));
    }
    let mut units = Vec::new();
    units
        .try_reserve_exact(length)
        .map_err(|_| io::ErrorKind::OutOfMemory)?;
    for unit in value.encode_wide() {
        if unit == 0 {
            return Err(invalid("launch-context string contains NUL"));
        }
        units.push(unit);
    }
    Ok(units)
}

#[cfg(test)]
mod tests {
    use super::*;
    fn context(entries: &[(OsString, OsString)]) -> LaunchContext<'_> {
        LaunchContext {
            current_dir: Path::new(r"D:\owned"),
            environment: entries,
        }
    }

    #[test]
    fn block_has_exact_utf16_entries_and_double_terminator() {
        let entries = vec![("A".into(), "".into()), ("AA".into(), "λ=🦀".into())];
        let prepared = context(&entries).prepare().unwrap();
        assert_eq!(
            prepared.environment,
            OsStr::new("A=\0AA=λ=🦀\0\0")
                .encode_wide()
                .collect::<Vec<_>>()
        );
        assert_eq!(
            prepared.directory,
            OsStr::new("D:\\owned\0").encode_wide().collect::<Vec<_>>()
        );
        assert_eq!(context(&[]).prepare().unwrap().environment, vec![0, 0]);
    }

    #[test]
    fn full_block_budget_counts_separators_and_terminators() {
        // One key unit + '=' + value + entry NUL + block NUL.
        let exact = vec![(
            "A".into(),
            OsString::from("v".repeat(MAX_ENVIRONMENT_UNITS - 4)),
        )];
        assert_eq!(
            context(&exact).prepare().unwrap().environment.len(),
            MAX_ENVIRONMENT_UNITS
        );
        let over = vec![(
            "A".into(),
            OsString::from("v".repeat(MAX_ENVIRONMENT_UNITS - 3)),
        )];
        assert_eq!(
            context(&over).prepare().err().unwrap().kind(),
            io::ErrorKind::InvalidInput
        );
        let combined = vec![
            (
                "A".into(),
                OsString::from("v".repeat(MAX_ENVIRONMENT_UNITS / 2)),
            ),
            (
                "B".into(),
                OsString::from("v".repeat(MAX_ENVIRONMENT_UNITS / 2)),
            ),
        ];
        assert_eq!(
            context(&combined).prepare().err().unwrap().kind(),
            io::ErrorKind::InvalidInput
        );
    }

    #[test]
    fn directory_budget_and_errors_do_not_echo_values() {
        let too_long = std::path::PathBuf::from(format!("D:\\{}", "x".repeat(MAX_DIRECTORY_UNITS)));
        let result = LaunchContext {
            current_dir: &too_long,
            environment: &[],
        }
        .prepare();
        assert_eq!(result.err().unwrap().kind(), io::ErrorKind::InvalidInput);
        let private = "not-a-real-secret\0must-not-appear";
        let entries = vec![("A".into(), private.into())];
        let err = context(&entries).prepare().err().unwrap();
        assert!(!err.to_string().contains("not-a-real-secret"));
    }
}

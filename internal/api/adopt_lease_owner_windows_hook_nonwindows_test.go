//go:build !windows

package api

// The common owner test declares a Windows-only cleanup matrix but the test
// file itself is not build-tagged. Keep its Windows fault seam test-only on
// POSIX so the independently selected POSIX owner proof can compile; the
// matrix remains a Windows-runtime test and is not run by the POSIX selection.
var adoptLeaseWindowsFailureHook func(string) error

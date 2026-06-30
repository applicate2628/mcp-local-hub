//go:build !windows

package api

import (
	"fmt"
	"runtime"
)

// maxUnixSocketPathLen is the platform sun_path buffer size (in bytes,
// including the mandatory NUL terminator the kernel appends) that bounds how
// long a unix-domain-socket path can be before bind(2)/connect(2) refuses it
// with a cryptic ENAMETOOLONG-derived Go error. The kernel buffer is a fixed
// C char[] in struct sockaddr_un — there is no portable syscall to query it
// at runtime, so the documented constants are the only source: Linux's
// <linux/un.h> UNIX_PATH_MAX is 108; Darwin's <sys/un.h> SUN_LEN bound is 104.
// Both reserve the LAST byte for the NUL terminator, so the actual usable
// path length is one less than the buffer size.
func maxUnixSocketPathLen() int {
	if runtime.GOOS == "darwin" {
		return 104
	}
	return 108 // Linux and other POSIX targets default to the Linux value.
}

// ValidateSupervisorIPCSocketPathLen pre-validates socketPath against the
// platform's sun_path limit BEFORE the caller attempts net.Listen("unix",
// socketPath). A state directory that is unusually long (deep nesting, a
// long username, a corporate-profile redirect) can push
// "<state-dir>/supervisor.sock" past the kernel's fixed sockaddr_un buffer;
// without this check, net.Listen fails with an opaque
// "invalid argument"/ENAMETOOLONG-derived error that names neither the limit
// nor the actual path length, leaving the operator to guess the cause. This
// returns a clear, actionable error instead: the limit, the observed length,
// and the path itself.
func ValidateSupervisorIPCSocketPathLen(socketPath string) error {
	limit := maxUnixSocketPathLen()
	// -1 reserves the kernel-appended NUL terminator (see maxUnixSocketPathLen
	// doc) — the usable path length is one less than the raw buffer size.
	usable := limit - 1
	if len(socketPath) > usable {
		return fmt.Errorf(
			"supervisor IPC socket path too long for this platform's unix-domain-socket limit: "+
				"path is %d bytes, limit is %d bytes (sun_path buffer is %d bytes including the NUL terminator); "+
				"path=%q — shorten the mcp-local-hub state directory (e.g. via a shorter profile/home path) so the derived socket path fits",
			len(socketPath), usable, limit, socketPath,
		)
	}
	return nil
}

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
// at runtime, so the documented constants are the only source:
//
//   - Linux <linux/un.h> UNIX_PATH_MAX = 108.
//   - Darwin <sys/un.h> sun_path = 104.
//   - The BSDs (FreeBSD/NetBSD/OpenBSD/DragonFly) also use 104 — golang.org/x/
//     sys/unix.RawSockaddrUnix.Path is [104]int8 on every BSD GOOS (bot PR
//     #474 P3); returning Linux's 108 for them would over-estimate the limit
//     by 4 bytes and let a too-long path through to a cryptic bind failure.
//   - AIX <sys/un.h> sun_path is char[1023] — x/sys/unix.RawSockaddrUnix.Path
//     is [1023]uint8 on GOOS=aix (bot PR #474 P3, other direction): returning
//     the 108 default there would UNDER-estimate the limit by 915 bytes and
//     wrongly reject a long-but-valid AIX socket path. mcphub does not
//     officially target AIX, but naming it keeps the catch-all honest rather
//     than silently misclaiming "any other POSIX target uses 108".
//
// Every platform reserves the LAST byte for the NUL terminator, so the actual
// usable path length is one less than the buffer size.
func maxUnixSocketPathLen() int {
	switch runtime.GOOS {
	case "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
		return 104
	case "aix":
		return 1023
	default:
		// Linux (UNIX_PATH_MAX = 108) and the other 108-byte-sun_path POSIX
		// targets (Solaris/illumos also use 108). AIX (1023) and the BSDs (104)
		// are handled explicitly above.
		return 108
	}
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

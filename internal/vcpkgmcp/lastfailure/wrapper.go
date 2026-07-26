package lastfailure

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

// WrapperInfo is whatever could be recovered from a build_failed.log-shaped
// wrapper file. Every field is independently optional — this parser is
// format-TOLERANT by design (see package doc): a wrapper file is an
// operator convention, not a vcpkg contract, so a partial or malformed
// parse degrades gracefully rather than failing the whole tool call.
//
// Real sample this shape is grounded on (read this session, NOT committed —
// see testdata/wrapper_ok.log for the synthetic fixture copy):
//
//	[2026-07-24 21:35:11] triplet=cl
//	command: C:\vcpkg\vcpkg.exe install <ports...> --overlay-ports=C:\vcpkg/vcpkg-builds/overlays/ports --overlay-ports=C:\vcpkg/vcpkg-builds/overlays/ports_upd --overlay-ports=C:\vcpkg/vcpkg-builds/overlays/ports_mkl --overlay-triplets=C:\vcpkg/vcpkg-builds/overlays/triplets --triplet=cl --x-buildtrees-root=r:/b/cl --x-packages-root=r:/vcpkg-cache/packages/cl --clean-buildtrees-after-build --clean-packages-after-build --no-binarycaching --keep-going --enforce-port-checks --recurse --x-install-root=q:/vcpkg-libs/cl --host-triplet=cl
//	exit_code: 1
//	build_failed_count: 5
//	failed_ports:
//	- hpx:cl
//	- libmesh:cl
//	- sqlite3:cl
//	- suitesparse-graphblas:cl
//	- tbb:cl
type WrapperInfo struct {
	Triplet          string
	Command          string
	OverlayPorts     []string // in command-line order == precedence order
	BuildtreesRoot   string
	InstallRoot      string
	ExitCode         *int
	BuildFailedCount *int
	// FailedPorts entries are exactly as written, "port:triplet".
	FailedPorts []string
}

var (
	wrapperTripletHeaderRE = regexp.MustCompile(`^\[.*\]\s+triplet=(\S+)\s*$`)
	wrapperCommandRE       = regexp.MustCompile(`^command:\s*(.+)$`)
	wrapperOverlayPortsRE  = regexp.MustCompile(`--overlay-ports=(\S+)`)
	wrapperTripletFlagRE   = regexp.MustCompile(`--triplet=(\S+)`)
	wrapperBuildtreesRE    = regexp.MustCompile(`--x-buildtrees-root=(\S+)`)
	wrapperInstallRootRE   = regexp.MustCompile(`--x-install-root=(\S+)`)
	wrapperExitCodeRE      = regexp.MustCompile(`^exit_code:\s*(-?\d+)\s*$`)
	wrapperFailedCountRE   = regexp.MustCompile(`^build_failed_count:\s*(\d+)\s*$`)
	wrapperFailedHeaderRE  = regexp.MustCompile(`^failed_ports:\s*$`)
	wrapperFailedEntryRE   = regexp.MustCompile(`^-\s*(\S+)\s*$`)
)

// ParseWrapperContent parses a build_failed.log-shaped byte blob.
// ok reports whether ANYTHING recognizable was recovered at all — false
// means the caller should treat the file as malformed/unusable and degrade
// to the buildtrees-native path (NoteWrapperMalformed), never fail.
func ParseWrapperContent(data []byte) (info WrapperInfo, ok bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	inFailedPorts := false
	for scanner.Scan() {
		line := scanner.Text()

		if m := wrapperTripletHeaderRE.FindStringSubmatch(line); m != nil {
			info.Triplet = m[1]
			ok = true
			inFailedPorts = false
			continue
		}
		if m := wrapperCommandRE.FindStringSubmatch(line); m != nil {
			info.Command = m[1]
			ok = true
			inFailedPorts = false
			for _, om := range wrapperOverlayPortsRE.FindAllStringSubmatch(m[1], -1) {
				info.OverlayPorts = append(info.OverlayPorts, om[1])
			}
			if tm := wrapperTripletFlagRE.FindStringSubmatch(m[1]); tm != nil && info.Triplet == "" {
				info.Triplet = tm[1]
			}
			if bm := wrapperBuildtreesRE.FindStringSubmatch(m[1]); bm != nil {
				info.BuildtreesRoot = bm[1]
			}
			if im := wrapperInstallRootRE.FindStringSubmatch(m[1]); im != nil {
				info.InstallRoot = im[1]
			}
			continue
		}
		if m := wrapperExitCodeRE.FindStringSubmatch(line); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				info.ExitCode = &v
				ok = true
			}
			inFailedPorts = false
			continue
		}
		if m := wrapperFailedCountRE.FindStringSubmatch(line); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				info.BuildFailedCount = &v
				ok = true
			}
			inFailedPorts = false
			continue
		}
		if wrapperFailedHeaderRE.MatchString(line) {
			inFailedPorts = true
			ok = true
			continue
		}
		if inFailedPorts {
			if m := wrapperFailedEntryRE.FindStringSubmatch(line); m != nil {
				info.FailedPorts = append(info.FailedPorts, m[1])
				continue
			}
			// A non-matching line (blank, or next section) ends the list.
			inFailedPorts = false
		}
	}
	return info, ok
}

// PortNameFromEntry splits a "port:triplet" wrapper entry into its parts.
// Tolerates an entry with no ':' (returns the whole string as port, empty
// triplet) rather than erroring — the wrapper format is not vcpkg's own.
func PortNameFromEntry(entry string) (port, triplet string) {
	if i := strings.LastIndex(entry, ":"); i >= 0 {
		return entry[:i], entry[i+1:]
	}
	return entry, ""
}

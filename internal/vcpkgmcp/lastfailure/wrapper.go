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
	// ScanComplete reports whether the whole file was read to EOF without a
	// scanner error. When false, FailedPorts is a PREFIX of an unknown-length
	// list, so its ABSENCE of an entry proves nothing — see
	// FailedPortsListIsComplete.
	ScanComplete bool
}

// FailedPortsListIsComplete reports whether this wrapper's failed_ports list
// can be trusted as EXHAUSTIVE, which is the only condition under which the
// list's silence about a port is evidence that the port did not fail.
//
// This guard exists because a truncated or partially-written wrapper file is
// otherwise indistinguishable from a complete one, and the negative
// conclusion it enables ("ok — this port did not fail") is the single most
// damaging wrong answer this tool can give: it tells an operator to stop
// looking at a port that DID fail. Three independent conditions must hold:
//
//   - the file was scanned to EOF without error (a bufio error, e.g. a line
//     exceeding the buffer, truncates the list silently);
//   - the wrapper declared build_failed_count at all (an older or different
//     wrapper that omits it gives no completeness signal whatsoever);
//   - the number of entries recovered EQUALS that declared count.
//
// A wrapper that says build_failed_count: 2 while listing one port is
// self-evidently incomplete — before this guard, querying the OMITTED port
// returned a confident ok.
func (w WrapperInfo) FailedPortsListIsComplete() bool {
	return w.ScanComplete &&
		w.BuildFailedCount != nil &&
		len(w.FailedPorts) == *w.BuildFailedCount
}

var (
	wrapperTripletHeaderRE = regexp.MustCompile(`^\[.*\]\s+triplet=(\S+)\s*$`)
	wrapperCommandRE       = regexp.MustCompile(`^command:\s*(.+)$`)
	wrapperExitCodeRE      = regexp.MustCompile(`^exit_code:\s*(-?\d+)\s*$`)
	wrapperFailedCountRE   = regexp.MustCompile(`^build_failed_count:\s*(\d+)\s*$`)
	wrapperFailedHeaderRE  = regexp.MustCompile(`^failed_ports:\s*$`)
	wrapperFailedEntryRE   = regexp.MustCompile(`^-\s*(\S+)\s*$`)
)

// SplitWindowsCommandLine splits a recorded Windows command line into
// arguments using the CommandLineToArgvW rules, so a quoted value containing
// spaces survives as ONE argument.
//
// A regex like `--x-buildtrees-root=(\S+)` cannot do this: against
// `--x-buildtrees-root="D:\vcpkg builds"` it stops at the space and yields
// `"D:\vcpkg` — a path that does not exist, which the buildtrees probe then
// reports as a cleaned tree. The parse must therefore be quote-aware.
//
// Rules implemented (documented by Microsoft for CommandLineToArgvW /
// "Parsing C++ command-line arguments", and verified empirically against the
// real shell32 CommandLineToArgvW on Windows 11 — see
// TestSplitWindowsCommandLine_MatchesRealCommandLineToArgvW for the captured
// ground-truth cases):
//
//   - Arguments are separated by runs of spaces/tabs outside quotes.
//   - 2n backslashes followed by `"` -> n backslashes, and the `"` is a
//     quote delimiter (it toggles quote mode).
//   - 2n+1 backslashes followed by `"` -> n backslashes and a LITERAL `"`.
//   - Backslashes NOT followed by `"` are always literal.
//   - Inside quotes, `""` emits one literal `"` and ENDS quote mode
//     (measured: `a.exe "x""y z"` yields [a.exe, `x"y`, z], not [a.exe,
//     `x"y z`]).
func SplitWindowsCommandLine(s string) []string {
	var args []string
	var cur strings.Builder
	started := false // distinguishes an empty quoted arg "" from no arg at all
	inQuotes := false

	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}

	for i := 0; i < len(s); {
		c := s[i]

		switch {
		case c == '\\':
			// Count the backslash run, then decide by what follows it.
			n := 0
			for i+n < len(s) && s[i+n] == '\\' {
				n++
			}
			if i+n < len(s) && s[i+n] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				started = true
				if n%2 == 1 {
					// Odd run: the quote is escaped, hence literal.
					cur.WriteByte('"')
					i += n + 1
				} else {
					// Even run: the quote is a delimiter; let the '"' case
					// below handle the toggle on the next iteration.
					i += n
				}
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
				started = true
				i += n
			}

		case c == '"':
			started = true
			if inQuotes && i+1 < len(s) && s[i+1] == '"' {
				// "" inside quotes: one literal quote, and quote mode ends.
				cur.WriteByte('"')
				inQuotes = false
				i += 2
				continue
			}
			inQuotes = !inQuotes
			i++

		case (c == ' ' || c == '\t') && !inQuotes:
			flush()
			i++

		default:
			cur.WriteByte(c)
			started = true
			i++
		}
	}
	flush()
	return args
}

// commandFlagValues extracts every value supplied for flag (e.g.
// "--overlay-ports") from an already-split argument list, supporting BOTH
// documented vcpkg spellings: `--key=value` (one argument) and `--key value`
// (two arguments). Values are returned in command-line order, which for the
// overlay flags IS precedence order.
func commandFlagValues(argv []string, flag string) []string {
	var out []string
	prefix := flag + "="
	for i := 0; i < len(argv); i++ {
		switch {
		case strings.HasPrefix(argv[i], prefix):
			out = append(out, argv[i][len(prefix):])
		case argv[i] == flag && i+1 < len(argv):
			// A following token that is itself a flag is NOT this flag's
			// value — that would silently absorb the next option.
			if !strings.HasPrefix(argv[i+1], "-") {
				out = append(out, argv[i+1])
				i++
			}
		}
	}
	return out
}

// commandFlagValue returns the LAST value supplied for a single-valued flag
// (vcpkg's own last-wins behaviour for repeated single-valued options), or
// "" when the flag is absent.
func commandFlagValue(argv []string, flag string) string {
	vals := commandFlagValues(argv, flag)
	if len(vals) == 0 {
		return ""
	}
	return vals[len(vals)-1]
}

// ParseWrapperContent parses a build_failed.log-shaped byte blob.
//
// ok reports whether ANYTHING recognizable was recovered at all — false
// means the caller should treat the file as malformed/unusable and degrade
// to the buildtrees-native path (NoteWrapperMalformed), never fail.
//
// A scan error (returned as err, and recorded as ScanComplete=false) does
// NOT by itself make the file unusable: the fields recovered before the
// error are still real. It only disqualifies the one conclusion that depends
// on having seen the WHOLE file — see FailedPortsListIsComplete.
func ParseWrapperContent(data []byte) (info WrapperInfo, ok bool, err error) {
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

			argv := SplitWindowsCommandLine(m[1])
			info.OverlayPorts = append(info.OverlayPorts, commandFlagValues(argv, "--overlay-ports")...)
			if t := commandFlagValue(argv, "--triplet"); t != "" && info.Triplet == "" {
				info.Triplet = t
			}
			if b := commandFlagValue(argv, "--x-buildtrees-root"); b != "" {
				info.BuildtreesRoot = b
			}
			if r := commandFlagValue(argv, "--x-install-root"); r != "" {
				info.InstallRoot = r
			}
			continue
		}
		if m := wrapperExitCodeRE.FindStringSubmatch(line); m != nil {
			if v, aerr := strconv.Atoi(m[1]); aerr == nil {
				info.ExitCode = &v
				ok = true
			}
			inFailedPorts = false
			continue
		}
		if m := wrapperFailedCountRE.FindStringSubmatch(line); m != nil {
			if v, aerr := strconv.Atoi(m[1]); aerr == nil {
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

	if err = scanner.Err(); err != nil {
		// Explicitly NOT ok=false: partial context is still usable, it just
		// cannot support the exhaustive-list conclusion.
		return info, ok, err
	}
	info.ScanComplete = true
	return info, ok, nil
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

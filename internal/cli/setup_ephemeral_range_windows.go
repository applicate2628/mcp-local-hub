//go:build windows

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/process"
)

// L2 — setup-time detection (and opt-in fix) of the OS TCP ephemeral (dynamic)
// range overlapping mcphub's daemon pools. On the affected hosts (WSL2/Hyper-V
// widened the range to e.g. 1024-15000) the OS hands pool ports to foreign apps,
// which is the root cause the L1 self-heal recovers from at runtime. This step
// makes the misconfiguration operator-visible at `mcphub setup` and offers a
// consent-gated, admin-only host fix behind --fix-ephemeral-range. Windows-only;
// the POSIX build has no-op stubs (setup_ephemeral_range_other.go).

// ephemeralRange is the parsed OS TCP dynamic (ephemeral) port range:
// [start, start+count-1].
type ephemeralRange struct {
	start int
	count int
}

func (r ephemeralRange) end() int { return r.start + r.count - 1 }

func (r ephemeralRange) contains(port int) bool {
	return r.count > 0 && port >= r.start && port <= r.end()
}

// queryEphemeralTCPRange is the netsh probe seam (tests swap it). It reads the
// OS dynamic port range WITHOUT admin rights.
//
// FIX-2 (NEW-2): the netsh exec is deadline-bounded (3s). osEphemeralTCPRange
// caches this behind a sync.Once, and although the range check is pre-warmed
// off-loop at supervisor startup (see runSupervise wiring), the FIRST L3 terminal
// emit could still be the one that trips the Once if it races the warm-up — an
// UNBOUNDED subprocess wait on the event loop. The deadline caps that worst case;
// a slow/wedged netsh returns an error (range stays "unknown") instead of freezing
// child-exit/IPC processing.
var queryEphemeralTCPRange = func() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	netsh, err := trustedNetshPath()
	if err != nil {
		return nil, err
	}
	cmd := newNetshCommandContext(ctx, netsh, "int", "ipv4", "show", "dynamicport", "tcp")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("netsh int ipv4 show dynamicport tcp: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

var (
	ephemeralRangeOnce    sync.Once
	ephemeralRangeCache   ephemeralRange
	ephemeralRangeCacheOK bool
)

// osEphemeralTCPRange returns the parsed OS dynamic range, cached (the range
// does not change under us within a supervisor lifetime; caching keeps the L3
// event's inside_ephemeral_range check cheap enough to call from the loop).
func osEphemeralTCPRange() (ephemeralRange, bool) {
	ephemeralRangeOnce.Do(func() {
		out, err := queryEphemeralTCPRange()
		if err != nil {
			return
		}
		if r, ok := parseEphemeralTCPRange(out); ok {
			ephemeralRangeCache = r
			ephemeralRangeCacheOK = true
		}
	})
	return ephemeralRangeCache, ephemeralRangeCacheOK
}

// parseEphemeralTCPRange extracts (start, count) from `netsh int ipv4 show
// dynamicport tcp` output. It is LOCALE-ROBUST: it takes the first two
// integer-valued "label : <int>" lines positionally (Start Port, then Number of
// Ports) rather than matching the English labels, so a localized Windows still
// parses. Bounds are validated so a garbled parse yields ok=false.
func parseEphemeralTCPRange(out []byte) (ephemeralRange, bool) {
	var nums []int
	for _, line := range strings.Split(string(out), "\n") {
		idx := strings.LastIndex(line, ":")
		if idx < 0 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(line[idx+1:]))
		if err != nil {
			continue
		}
		nums = append(nums, n)
		if len(nums) == 2 {
			break
		}
	}
	if len(nums) < 2 {
		return ephemeralRange{}, false
	}
	start, count := nums[0], nums[1]
	if start < 1 || start > 65535 || count < 1 || start+count-1 > 65535 {
		return ephemeralRange{}, false
	}
	return ephemeralRange{start: start, count: count}, true
}

// ephemeralRangePortContains reports (inRange, known) for a port against the OS
// dynamic range — the L3 event's inside_ephemeral_range source. known=false when
// the range could not be probed/parsed.
func ephemeralRangePortContains(port int) (bool, bool) {
	r, ok := osEphemeralTCPRange()
	if !ok {
		return false, false
	}
	return r.contains(port), true
}

// mcphubEffectivePools returns the mcphub daemon pools the ephemeral-range
// overlap check considers: the shipped global fixed band, the serena dynamic
// pool, and the mcp-language-server LSP manifest pool (best-effort — a missing
// manifest simply contributes no pool). Each is a [start,end] range.
func mcphubEffectivePools() []tcpRange {
	pools := []tcpRange{
		// Shipped global fixed band (configs/ports.yaml 9121-9149, with growth
		// room). Hand-assigned; a static band is the proportionate check here.
		{start: 9121, end: 9149},
	}
	// Serena dynamic pool (built-in default or the operator's daemon_template),
	// PLUS its native-http UPSTREAM (internal) band (P2-2). A serena proxy binds
	// BOTH an external pool port AND an internal upstream port
	// (ExternalPort + NativeHTTPInternalPortOffset). The overlap check AND the
	// --fix-ephemeral-range MOVE must account for the upstream band too — otherwise
	// computeEphemeralRangeFix would pick a new dynamic range that starts just above
	// the external pools yet SITS OVER the upstream band (~19150-19205), handing the
	// OS a serena UPSTREAM port to steal → an unhealable theft on the serena
	// backend's internal port (its failures are unclassified — no self-heal). The
	// remedy the L3 event recommends would then create a NEW theft class.
	//
	// FIX-5 P2-2 P3 residual (low-pri, note-only): this passes serenaPortPool(nil)
	// — the BUILT-IN default pool — while the runtime reallocation resolves the
	// serena pool from the LOADED manifest (reallocPoolForDescriptor →
	// EffectiveSerenaPortPool(m)). An operator who customizes daemon_template.port_pool
	// therefore gets a setup overlap-check / --fix-ephemeral-range MOVE computed
	// against the default band, not their custom band. The default is the shipped
	// case, so this only under/over-shoots the fix window for a customized pool; it
	// never mis-heals at runtime (that path uses the manifest). Resolving via the
	// manifest here for full symmetry is deferred as low-priority.
	if p, err := serenaPortPool(nil); err == nil && p.End >= p.Start {
		pools = append(pools, tcpRange{start: p.Start, end: p.End})
		off := config.NativeHTTPInternalPortOffset
		pools = append(pools, tcpRange{start: p.Start + off, end: p.End + off})
	}
	// LSP (mcp-language-server) manifest pool. LSP proxies carry no runtime_spec /
	// upstream port (their backend is stdio, not a TCP upstream), so there is no
	// upstream band to add here.
	if p, ok := api.WorkspaceLSPManifestPool(); ok && p.End >= p.Start {
		pools = append(pools, tcpRange{start: p.Start, end: p.End})
	}
	return pools
}

// tcpRange is a small [start,end] pool range used for the setup overlap report.
type tcpRange struct {
	start int
	end   int
}

func (r tcpRange) overlaps(e ephemeralRange) bool {
	if e.count <= 0 || r.end < r.start {
		return false
	}
	return r.start <= e.end() && e.start <= r.end
}

// trustedNetshPath returns the absolute system netsh.exe path without consulting
// PATH or the current directory. The --fix-ephemeral-range path may run from an
// elevated shell, so resolving netsh by name would allow executable search-path
// hijacking in that elevated context.
func trustedNetshPath() (string, error) {
	// Resolve System32 via the kernel-authoritative GetSystemDirectoryW syscall,
	// NOT the SystemRoot/windir ENVIRONMENT. --fix-ephemeral-range may run from an
	// elevated shell whose inherited environment is attacker-influenced; a
	// SystemRoot pointing at e.g. C:\Users\Public\fake (with fake\System32\netsh.exe)
	// would pass an absolute+stat check yet execute a hijacked binary as admin. The
	// syscall returns the real Windows system directory and cannot be redirected via
	// the environment or PATH.
	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return "", fmt.Errorf("resolve Windows system directory for netsh.exe: %w", err)
	}
	if !filepath.IsAbs(system32) {
		return "", fmt.Errorf("Windows system directory %q is not absolute", system32)
	}
	netsh := filepath.Join(system32, "netsh.exe")
	info, err := os.Stat(netsh)
	if err != nil {
		return "", fmt.Errorf("stat trusted netsh.exe %q: %w", netsh, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("trusted netsh.exe path %q is a directory", netsh)
	}
	return netsh, nil
}

// setEphemeralTCPRange is the netsh mutation seam (tests swap it). Requires an
// elevated shell.
var setEphemeralTCPRange = func(start, num int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	netsh, err := trustedNetshPath()
	if err != nil {
		return nil, err
	}
	cmd := newNetshCommandContext(ctx, netsh, "int", "ipv4", "set", "dynamicport", "tcp",
		"start="+strconv.Itoa(start), "num="+strconv.Itoa(num))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("netsh int ipv4 set dynamicport tcp start=%d num=%d: %w: %s", start, num, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func newNetshCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	process.NoConsole(cmd)
	return cmd
}

// computeEphemeralRangeFix picks a new dynamic range that clears every mcphub
// pool: start just above the highest pool end (rounded up to a clean 100
// boundary), num = every port from there to 65535. This is the "protect the
// pool, keep a wide window" MOVE (decision doc's targeted variant) — it never
// touches excludedportrange, and the new window stays large for WSL2/Hyper-V
// headroom while sitting entirely above the pools.
func computeEphemeralRangeFix(pools []tcpRange) (newStart, newNum int) {
	highest := 0
	for _, p := range pools {
		if p.end > highest {
			highest = p.end
		}
	}
	newStart = highest + 1
	if rem := newStart % 100; rem != 0 {
		newStart += 100 - rem
	}
	if newStart < 1024 || newStart > 60000 {
		// Defensive: no sane pool bound → fall back to the Windows default start.
		newStart = 49152
	}
	newNum = 65535 - newStart + 1
	return newStart, newNum
}

// runSetupEphemeralRangeStep is the L2 detect+warn (and opt-in fix) setup step.
// Non-mutating by default: it probes the OS dynamic range (no admin), computes
// the pool overlap, and warns naming the overlap + consequence + remedy. With
// fix=true AND an elevated shell it MOVES the range above the pools via netsh
// (printing before/after). Every failure path is NON-FATAL (warns + continues)
// so a netsh quirk never blocks `mcphub setup`.
func runSetupEphemeralRangeStep(cmd *cobra.Command, fix bool) {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	r, ok := osEphemeralTCPRange()
	if !ok {
		// Could not probe/parse the dynamic range — say nothing (a warn here
		// would be noise on a host where netsh is unavailable/localized oddly).
		return
	}
	pools := mcphubEffectivePools()
	var overlapping []tcpRange
	for _, p := range pools {
		if p.overlaps(r) {
			overlapping = append(overlapping, p)
		}
	}
	if len(overlapping) == 0 {
		return // ephemeral range is clear of every pool — nothing to warn/fix
	}

	fmt.Fprintf(errOut, "warning: the Windows TCP ephemeral (dynamic) port range %d-%d overlaps mcphub daemon pools (%s).\n",
		r.start, r.end(), formatTCPRanges(overlapping))
	fmt.Fprintf(errOut, "  Consequence: the OS can hand a pool port to a foreign process, so a daemon's bind is refused (WSAEACCES/WSAEADDRINUSE). mcphub self-heals dynamic-pool daemons by moving them to a fresh port, but a FIXED-port global daemon (9121-9149) cannot move and will crash-loop until the port frees.\n")

	if !fix {
		fmt.Fprintf(errOut, "  Remedy: re-run `mcphub setup --fix-ephemeral-range` in an ELEVATED shell to move the dynamic range above the pools, or run manually (admin): netsh int ipv4 set dynamicport tcp start=<above-pools> num=<n>. Do NOT `netsh add excludedportrange` on the pool (that would make mcphub's allocator treat the pool as unusable).\n")
		return
	}

	elevated, elevErr := setupIsElevated()
	if elevErr != nil {
		fmt.Fprintf(errOut, "  --fix-ephemeral-range: could not determine elevation (%v); NOT mutating the host. Run the netsh command manually in an elevated shell.\n", elevErr)
		return
	}
	if !elevated {
		fmt.Fprintf(errOut, "  --fix-ephemeral-range requires an ELEVATED shell; NOT mutating the host. Re-run from an Administrator prompt, or run manually: netsh int ipv4 set dynamicport tcp start=<above-pools> num=<n>.\n")
		return
	}

	newStart, newNum := computeEphemeralRangeFix(pools)
	fmt.Fprintf(out, "--fix-ephemeral-range: BEFORE: dynamic range %d-%d (start=%d num=%d)\n", r.start, r.end(), r.start, r.count)
	if mutOut, err := setEphemeralTCPRange(newStart, newNum); err != nil {
		fmt.Fprintf(errOut, "  --fix-ephemeral-range: netsh set failed: %v\n", err)
		return
	} else if trimmed := strings.TrimSpace(string(mutOut)); trimmed != "" {
		fmt.Fprintf(out, "  netsh: %s\n", trimmed)
	}
	fmt.Fprintf(out, "--fix-ephemeral-range: AFTER: dynamic range %d-%d (start=%d num=%d). The pools %s are now OUTSIDE the ephemeral range. (Not reverted on uninstall.)\n",
		newStart, newStart+newNum-1, newStart, newNum, formatTCPRanges(pools))
}

// formatTCPRanges renders pool ranges compactly for the operator message.
func formatTCPRanges(ranges []tcpRange) string {
	parts := make([]string, 0, len(ranges))
	for _, r := range ranges {
		parts = append(parts, fmt.Sprintf("%d-%d", r.start, r.end))
	}
	return strings.Join(parts, ", ")
}

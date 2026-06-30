//go:build linux

package process

import (
	"encoding/binary"
	"math/bits"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// ClockTicksPerSecond returns the kernel's CLK_TCK (clock-ticks-per-second)
// value, used to convert /proc/<pid>/stat jiffy fields to wall-clock
// durations. It is the SINGLE owner of this derivation (architecture-layering
// "one owner per cross-cutting invariant") — internal/process's own
// ProcessStartTime (the supervisor identity-gate's start-time proof) and
// internal/gui's probe (the --force --kill identity gate) both need the same
// CLK_TCK value, and a wrong value silently misclassifies a legitimate
// process as PID-recycled. Prior to this unification the two call sites had
// independently re-derived the value: this package estimated it at runtime
// via unix.Times + unix.Sysinfo (a rounded uptime/ticks ratio with sub-1%
// drift on a freshly-booted host), while internal/gui read the
// kernel-published value directly from /proc/self/auxv's AT_CLKTCK entry —
// an exact, zero-drift source already hardened across three rounds of bot
// review (32-bit auxv layout, native endianness, jiffy-precision loss). This
// unified owner uses the auxv reader as the primary source AND keeps the old
// runtime estimate as a secondary fallback (see the resolution-order list
// below); internal/gui now calls this function instead of keeping its own
// copy.
//
// Reads /proc/self/auxv once per process and caches the result, since
// CLK_TCK is a kernel build-time constant that never changes for the life of
// the process. Resolution order, best-to-worst (bot PR #474 P2 — do NOT
// blindly trust the 100 default on an auxv-unavailable host):
//
//  1. AT_CLKTCK from /proc/self/auxv — exact, kernel-published, zero drift.
//  2. A unix.Times/unix.Sysinfo runtime ESTIMATE (ticks-since-boot ÷ uptime)
//     when auxv is unreadable or lacks AT_CLKTCK. This is the value the
//     pre-unification internal/process derived; restored here as the SECONDARY
//     fallback because on a kernel built with HZ=250 or HZ=1000 (where auxv is
//     hidden, e.g. some hardened/minimal containers) a blind 100 would make
//     ProcessStartTime off by 2.5x/10x and mis-judge the supervisor identity
//     gate. The estimate has sub-1% drift on a freshly-booted host — far
//     closer to a non-100 true HZ than the 100 default is.
//  3. 100 (the most common kernel default) ONLY when BOTH the auxv read and
//     the estimate fail, so probe/start-time logic stays usable on minimal
//     containers that hide both surfaces.
//
// Each auxv entry is two C unsigned longs — 16 bytes on 64-bit builds
// (amd64/arm64), 8 bytes on 32-bit builds (386/arm). The list terminates with
// type AT_NULL = 0. Word size is constant per Go build, so the right decoder
// is picked at compile time via math/bits.UintSize. Originally Codex bot
// review on PR #23 P2 (auxv 32-bit parsing).
var (
	clkTckOnce  sync.Once
	clkTckValue int64
)

const (
	atClkTckType = 17                // AT_CLKTCK in <elf.h>
	atNullType   = 0                 // AT_NULL terminator
	auxvWordSize = bits.UintSize / 8 // 4 on 32-bit, 8 on 64-bit Go builds
	auxvEntryLen = 2 * auxvWordSize  // 8 on 32-bit, 16 on 64-bit
)

// readAuxvWord decodes one auxv field (sized to the native pointer) at
// offset i in data as a uint64. Uses binary.NativeEndian because
// /proc/self/auxv is emitted in native-endian unsigned long words — a prior
// LittleEndian decoder produced wrong AT_CLKTCK values on big-endian Linux
// targets (mips, ppc, s390x), silently falling back to the 100-default and
// breaking start-time reconstruction. Originally Codex bot review on PR #23
// P2 (native endianness). Compile-time constant branching on word size —
// wrong-arch arm is dead-code-eliminated.
func readAuxvWord(data []byte, i int) uint64 {
	if auxvWordSize == 8 {
		return binary.NativeEndian.Uint64(data[i : i+8])
	}
	return uint64(binary.NativeEndian.Uint32(data[i : i+4]))
}

// ClockTicksPerSecond returns the cached CLK_TCK value, reading
// /proc/self/auxv on first call. See the package-level doc above for the
// three-tier resolution order (auxv → estimate → 100).
func ClockTicksPerSecond() int64 {
	clkTckOnce.Do(func() {
		// Tier 1 (primary): exact value from /proc/self/auxv AT_CLKTCK.
		if hz, ok := clkTckFromAuxv(); ok {
			clkTckValue = hz
			return
		}
		// Tier 2 (fallback): runtime estimate from unix.Times/unix.Sysinfo —
		// better than a blind 100 on a HZ=250/1000 kernel that hides auxv.
		if hz, ok := clkTckFromUptimeEstimate(); ok {
			clkTckValue = hz
			return
		}
		// Tier 3 (last resort): the most common kernel default.
		clkTckValue = 100
	})
	return clkTckValue
}

// clkTckFromAuxv reads the kernel-published AT_CLKTCK entry from
// /proc/self/auxv. Returns (0, false) if the auxv is unreadable or the entry
// is absent. This is the exact, zero-drift primary source.
func clkTckFromAuxv() (int64, bool) {
	data, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 0, false
	}
	for i := 0; i+auxvEntryLen <= len(data); i += auxvEntryLen {
		atype := readAuxvWord(data, i)
		avalue := readAuxvWord(data, i+auxvWordSize)
		if atype == atNullType {
			break
		}
		if atype == atClkTckType && avalue > 0 {
			return int64(avalue), true
		}
	}
	return 0, false
}

// clkTckFromUptimeEstimate derives CLK_TCK as ticks-since-boot ÷ uptime via
// unix.Times + unix.Sysinfo (rounded). This is the pre-unification estimate
// internal/process used; it is the SECONDARY fallback (better than a blind
// 100 on a non-100-HZ kernel that hides auxv — bot PR #474 P2). Returns
// (0, false) if either syscall fails or yields a non-positive denominator.
func clkTckFromUptimeEstimate() (int64, bool) {
	var tms unix.Tms
	ticks, err := unix.Times(&tms)
	if err != nil || ticks == 0 {
		return 0, false
	}
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil || info.Uptime <= 0 {
		return 0, false
	}
	hz := int64((ticks + uintptr(info.Uptime/2)) / uintptr(info.Uptime))
	if hz <= 0 {
		return 0, false
	}
	return hz, true
}

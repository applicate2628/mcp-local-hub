//go:build linux

package process

import (
	"encoding/binary"
	"math/bits"
	"os"
	"sync"
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
// review (32-bit auxv layout, native endianness, jiffy-precision loss). The
// auxv reader is migrated here verbatim as the shared, more correct
// implementation; internal/gui now calls this function instead of keeping
// its own copy.
//
// Reads /proc/self/auxv once per process and caches the result, since
// CLK_TCK is a kernel build-time constant that never changes for the life of
// the process. Falls back to 100 (the most common kernel default) if the
// auxv is unreadable or the entry is missing, so probe/start-time logic
// stays usable on minimal containers that hide /proc/self/auxv.
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
// /proc/self/auxv on first call. See the package-level doc above.
func ClockTicksPerSecond() int64 {
	clkTckOnce.Do(func() {
		clkTckValue = 100 // safe default if auxv is unreadable
		data, err := os.ReadFile("/proc/self/auxv")
		if err != nil {
			return
		}
		for i := 0; i+auxvEntryLen <= len(data); i += auxvEntryLen {
			atype := readAuxvWord(data, i)
			avalue := readAuxvWord(data, i+auxvWordSize)
			if atype == atNullType {
				break
			}
			if atype == atClkTckType && avalue > 0 {
				clkTckValue = int64(avalue)
				return
			}
		}
	})
	return clkTckValue
}

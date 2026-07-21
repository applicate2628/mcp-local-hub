package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// callerStartTimeChildStateDirEnv, when set, makes this test binary act as a
// child that emits ONE intent-audit row into the given state dir after a
// deliberate delay, then exits. Consumed by the package TestMain
// (main_test.go) before m.Run().
const callerStartTimeChildStateDirEnv = "MCPHUB_TEST_CALLER_START_TIME_CHILD_STATEDIR"

// callerStartTimeChildDelayEnv is the delay (in milliseconds) the child waits
// between process start and emitting. The delay is the load-bearing part of
// the oracle — see the test doc.
const callerStartTimeChildDelayEnv = "MCPHUB_TEST_CALLER_START_TIME_CHILD_DELAY_MS"

// runCallerStartTimeOracleChild is the child body invoked from TestMain.
// Never returns.
func runCallerStartTimeOracleChild(stateDir string) {
	delayMS, _ := strconv.Atoi(os.Getenv(callerStartTimeChildDelayEnv))

	// Redirect state resolution to the parent-chosen dir so the child writes
	// its audit row where the parent can read it.
	daemonStateRootOverride = stateDir

	// Wait BEFORE emitting. This is what separates "process start time" from
	// "time at emit" — without it the two are indistinguishable in a freshly
	// spawned process and the oracle would be vacuous.
	time.Sleep(time.Duration(delayMS) * time.Millisecond)

	if err := NewAPI().AppendIntentAudit(NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask(`\mcp-local-hub-oracle-probe`),
	)); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

// TestIntentAudit_CallerStartTimeAgainstIndependentOracle verifies
// caller_start_time against a source that is NOT the code under test.
//
// WHY THIS EXISTS. The sibling assertion in
// TestIntentAudit_CallerFieldsPopulated compares the emitted value with
// CallerStartTime() — the very helper that produced it. That is a consistency
// check with a blind spot: if CallerStartTime itself regressed (a bad
// process-time conversion, or one of its documented fallbacks returning
// time.Now()), both sides agree and the test passes while the audit log
// carries a wrong process start time. Adversarial review confirmed the hole by
// mutating CallerStartTime to time.Now() and watching an isolated run SURVIVE.
//
// THE ORACLE: the PARENT's wall clock, sampled immediately before spawning a
// child. The child's true start time is necessarily within a few milliseconds
// of that sample, and the parent's clock is entirely independent of the
// process-time API under test.
//
// THE DELAY IS LOAD-BEARING. A fresh child would have start-time close to now,
// so a time.Now() regression would still fall inside any spawn-window bound
// and survive. The child therefore SLEEPS before emitting: a correct
// implementation still reports approximately the spawn time, while a
// time.Now() fallback reports approximately spawn+delay. The upper bound sits
// well below the delay, so the two are decisively separated.
//
// DURATION-INDEPENDENT BY CONSTRUCTION. The window is anchored to this test's
// own spawn instant, never to wall-clock-now at assert time, so it cannot
// regress into the suite-runtime budget that made the original plus/minus-2min
// -of-now assertion a time bomb.
func TestIntentAudit_CallerStartTimeAgainstIndependentOracle(t *testing.T) {
	const childDelay = 3 * time.Second

	stateDir := t.TempDir()

	// The oracle sample. Taken as late as possible before spawn.
	before := time.Now().UTC()

	cmd := exec.Command(os.Args[0], "-test.run=TestIntentAudit_CallerStartTimeAgainstIndependentOracle")
	cmd.Env = append(os.Environ(),
		callerStartTimeChildStateDirEnv+"="+stateDir,
		callerStartTimeChildDelayEnv+"="+strconv.Itoa(int(childDelay/time.Millisecond)),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("oracle child failed: %v\noutput:\n%s", err, out)
	}
	after := time.Now().UTC()

	if elapsed := after.Sub(before); elapsed < childDelay {
		t.Fatalf("child returned in %v, less than its %v delay - the delay did not take effect, so the oracle would be vacuous", elapsed, childDelay)
	}

	raw, err := os.ReadFile(filepath.Join(stateDir, intentAuditFileLeaf))
	if err != nil {
		t.Fatalf("read child audit log: %v\nchild output:\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal(firstJSONLine(t, raw), &got); err != nil {
		t.Fatalf("json: %v", err)
	}

	tsRaw, _ := got["caller_start_time"].(string)
	if tsRaw == "" {
		t.Fatalf("child emitted no caller_start_time; body = %v", got)
	}
	parsed, err := time.Parse(time.RFC3339Nano, tsRaw)
	if err != nil {
		t.Fatalf("caller_start_time not RFC3339Nano: %v (raw=%q)", err, tsRaw)
	}

	// Lower bound: generous, absorbing per-OS whole-second truncation of the
	// start time (which can round DOWN below the sample).
	lower := before.Add(-3 * time.Second)
	// Upper bound: comfortably above real spawn latency, and decisively BELOW
	// before+childDelay so a time.Now()-at-emit regression cannot pass.
	upper := before.Add(childDelay / 2)

	if parsed.Before(lower) || parsed.After(upper) {
		t.Errorf("caller_start_time = %v is outside the independent window [%v, %v].\n"+
			"The child was spawned at %v and emitted %v later. A value near %v indicates "+
			"CallerStartTime returned the time AT EMIT (a time.Now() fallback) rather than "+
			"the process START time.",
			parsed, lower, upper, before, childDelay, before.Add(childDelay))
	}
}

// firstJSONLine returns the first non-empty line of a JSON-Lines file.
func firstJSONLine(t *testing.T, raw []byte) []byte {
	t.Helper()
	for start := 0; start < len(raw); {
		end := start
		for end < len(raw) && raw[end] != '\n' {
			end++
		}
		line := raw[start:end]
		if len(line) > 0 {
			return line
		}
		start = end + 1
	}
	t.Fatalf("no JSON line found in %d bytes", len(raw))
	return nil
}

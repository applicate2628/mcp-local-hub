package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const portProbeFixtureDirEnv = "MCPHUB_TEST_PORT_PROBE_DIR"

type portProbeFixtureReply struct {
	Output string
	Exit   int
}

// runPortProbeTestChild is called before TestMain's normal suite setup. Only
// copies explicitly named netstat/wmic, with a parent-issued fixture directory,
// enter this branch. No host process table, listener, identity or kill is used.
func runPortProbeTestChild() {
	dir := os.Getenv(portProbeFixtureDirEnv)
	command := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if dir == "" || (command != "netstat" && command != "wmic") {
		return
	}
	// Witness every admitted child before reply/argument validation. In
	// particular, an unexpected WMIC that fails must not vanish from the oracle.
	f, err := os.OpenFile(filepath.Join(dir, "calls.txt"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(94)
	}
	_, writeErr := fmt.Fprintln(f, command)
	if errors.Join(writeErr, f.Close()) != nil {
		os.Exit(95)
	}
	data, err := os.ReadFile(filepath.Join(dir, "replies.json"))
	if err != nil {
		os.Exit(90)
	}
	var replies map[string]portProbeFixtureReply
	if json.Unmarshal(data, &replies) != nil {
		os.Exit(91)
	}
	reply, ok := replies[command]
	if !ok {
		os.Exit(92)
	}
	want := []string{"-ano"}
	if command == "wmic" {
		want = []string{"process", "where", "ProcessId=424242", "get", "WorkingSetSize,CreationDate", "/format:csv"}
	}
	if !reflect.DeepEqual(os.Args[1:], want) {
		os.Exit(93)
	}
	if _, err := fmt.Fprint(os.Stdout, reply.Output); err != nil {
		os.Exit(96)
	}
	os.Exit(reply.Exit)
}

func portProbeFixtureBinaries(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"netstat", "wmic"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		in, err := os.Open(exe)
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(filepath.Join(bin, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if err != nil {
			_ = in.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(out, in)
		if err := errors.Join(copyErr, out.Close(), in.Close()); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

func usePortProbeFixture(t *testing.T, bin string, replies map[string]portProbeFixtureReply) func() []string {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(replies)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "replies.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(portProbeFixtureDirEnv, dir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "calls.txt"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(data))
	}
}

func TestQueryLoopbackPortPIDUsesBoundedCollectorWithoutMetrics(t *testing.T) {
	bin := portProbeFixtureBinaries(t)
	const output = "TCP 0.0.0.0:33840 0.0.0.0:0 LISTENING 999\n" +
		"TCP [::1]:33840 [::]:0 LISTENING 998\n" +
		"TCP 127.0.0.1:33840 0.0.0.0:0 LISTENING 424242\n"
	cases := []struct {
		name  string
		port  int
		reply portProbeFixtureReply
		pid   int
		found bool
		fail  bool
	}{
		{name: "exact listener", port: 33840, reply: portProbeFixtureReply{Output: output}, pid: 424242, found: true},
		{name: "successful absence", port: 33841, reply: portProbeFixtureReply{Output: output}},
		{name: "command failure", port: 33840, reply: portProbeFixtureReply{Exit: 23}, fail: true},
		{name: "partial output then failure", port: 33840, reply: portProbeFixtureReply{Output: output, Exit: 23}, fail: true},
		{name: "zero port", port: 0, fail: true},
		{name: "negative port", port: -1, fail: true},
		{name: "out of range port", port: 65536, fail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := usePortProbeFixture(t, bin, map[string]portProbeFixtureReply{"netstat": tc.reply})
			pid, found, err := queryLoopbackPortPID(tc.port)
			if pid != tc.pid || found != tc.found || (err != nil) != tc.fail {
				t.Fatalf("observation = (%d, %t, %v), want (%d, %t, failure=%t)", pid, found, err, tc.pid, tc.found, tc.fail)
			}
			var wantCalls []string
			if tc.port > 0 && tc.port <= 65535 {
				wantCalls = []string{"netstat"}
			}
			if got := calls(); !reflect.DeepEqual(got, wantCalls) {
				t.Fatalf("child observations = %v, want %v; PID-only lookup must not acquire metrics", got, wantCalls)
			}
			if tc.reply.Exit != 0 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.reply.Exit {
					t.Fatalf("lost the original child failure: %v", err)
				}
			}
		})
	}
}

func TestQueryPortProcessInfoPreservesOptionalMetrics(t *testing.T) {
	bin := portProbeFixtureBinaries(t)
	created := time.Now().UTC().Add(-90 * time.Second).Truncate(time.Second)
	const ram = uint64(7 * 1024 * 1024)
	for _, failedMetrics := range []bool{false, true} {
		t.Run(fmt.Sprintf("metrics_failure_%t", failedMetrics), func(t *testing.T) {
			metricReply := portProbeFixtureReply{Output: fmt.Sprintf("Node,CreationDate,WorkingSetSize\nFIXTURE,%s,%d\n", created.Format("20060102150405.000000+000"), ram)}
			if failedMetrics {
				metricReply.Exit = 24
			}
			calls := usePortProbeFixture(t, bin, map[string]portProbeFixtureReply{
				"netstat": {Output: "TCP 127.0.0.1:33840 0.0.0.0:0 LISTENING 424242\n"},
				"wmic":    metricReply,
			})
			before := int64(time.Since(created).Seconds())
			pid, gotRAM, uptime, found := queryPortProcessInfo(33840)
			after := int64(time.Since(created).Seconds())
			if pid != 424242 || !found {
				t.Fatalf("diagnostic listener = (%d, %t), want (424242, true)", pid, found)
			}
			if failedMetrics {
				if gotRAM != 0 || uptime != 0 {
					t.Fatalf("failed metrics must remain unknown, got (%d, %d)", gotRAM, uptime)
				}
			} else if gotRAM != ram || uptime < before || uptime > after {
				t.Fatalf("metrics = (%d, %d), want RAM=%d and uptime in [%d, %d]", gotRAM, uptime, ram, before, after)
			}
			if got := calls(); !reflect.DeepEqual(got, []string{"netstat", "wmic"}) {
				t.Fatalf("diagnostic child observations = %v", got)
			}
		})
	}
}

func TestPortPIDCollectorFailureReachesKillPolicy(t *testing.T) {
	isolatePortPIDPolicySeams(t)
	bin := portProbeFixtureBinaries(t)
	calls := usePortProbeFixture(t, bin, map[string]portProbeFixtureReply{"netstat": {Exit: 23}})
	lookupPortPID = queryLoopbackPortPID
	outcome, err := killDaemonByPortOutcome(33840, time.Second)
	var exitErr *exec.ExitError
	if outcome != portKillProbeUnknown || !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("collector failure became a stop result (%v, %v)", outcome, err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{"netstat"}) {
		t.Fatalf("failed PID lookup must run only its observer, got %v", got)
	}
}

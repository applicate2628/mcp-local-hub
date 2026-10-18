package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func frozenPortSnapshot(t *testing.T, rows []SupervisorDaemon) []byte {
	t.Helper()
	raw, err := json.Marshal(SupervisorIntentFile{Version: 1, Daemons: rows})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func frozenPortManifestFixtures(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	writeManifest(t, dir, "capture-managed", "name: capture-managed\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9403\n")
	cwd, err := json.Marshal(filepath.ToSlash(dir))
	if err != nil {
		t.Fatal(err)
	}
	writeManifest(t, dir, "capture-companion", "name: capture-companion\nkind: companion\ntransport: process\ncommand: echo\ndaemons:\n  - name: default\n    port: 0\n    cwd: "+string(cwd)+"\n")
	writeManifest(t, dir, "capture-broken", "name: [unterminated\n")
}

func TestCaptureSupervisorFrozenPorts_ResolutionAndWire(t *testing.T) {
	frozenPortManifestFixtures(t)
	rows := []SupervisorDaemon{
		{TaskName: "z-stamped", Command: "synthetic-command", Port: 9400, Server: "missing", Daemon: "default"},
		{TaskName: "a-proxy", Command: "synthetic-command", Args: []string{"daemon", "workspace-proxy", "--port", "9401"}},
		{TaskName: "b-serena", Command: "synthetic-command", Args: []string{"daemon", "serena-proxy", "--port", "9402"}},
		{TaskName: "c-manifest", Command: "synthetic-command", Server: "capture-managed", Daemon: "default"},
	}
	got, err := CaptureSupervisorFrozenPorts(frozenPortSnapshot(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	// Independent fixed JSON argv vectors bind the established digest rule.
	argvJSON := []string{
		`["synthetic-command"]`,
		`["synthetic-command","daemon","workspace-proxy","--port","9401"]`,
		`["synthetic-command","daemon","serena-proxy","--port","9402"]`,
		`["synthetic-command"]`,
	}
	want := make([]SupervisorFrozenPort, len(rows))
	for i, row := range rows {
		port := 9400 + i
		if oldPort, ok := EffectiveDaemonPort(row); !ok || oldPort != port {
			t.Fatalf("legacy lookup %d = (%d,%v), want (%d,true)", i, oldPort, ok, port)
		}
		digest := sha256.Sum256([]byte(argvJSON[i]))
		want[i] = SupervisorFrozenPort{TaskName: row.TaskName, Port: port, ArgvSHA256: hex.EncodeToString(digest[:])}
	}
	want = []SupervisorFrozenPort{want[1], want[2], want[3], want[0]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cohort = %#v, want %#v", got, want)
	}
	first, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	wire := fmt.Sprintf(`{"task_name":"a-proxy","port":9401,"argv_sha256":"%s"}`, want[0].ArgvSHA256)
	if string(first) != wire {
		t.Fatalf("row wire = %s, want %s", first, wire)
	}
	// Old callers still select the first positive argv value, including their
	// established permissive range; stricter capture does not change that API.
	legacy := SupervisorDaemon{Args: []string{"daemon", "workspace-proxy", "--port", "bad", "--port", "70000", "--port", "9401"}}
	if port, ok := EffectiveDaemonPort(legacy); !ok || port != 70000 {
		t.Fatalf("legacy first-positive behavior = (%d,%v)", port, ok)
	}
}

func TestCaptureSupervisorFrozenPorts_PositiveEmpty(t *testing.T) {
	frozenPortManifestFixtures(t)
	companion := SupervisorDaemon{TaskName: "companion", Command: "synthetic-command", Server: "capture-companion", Daemon: "default"}
	for _, rows := range [][]SupervisorDaemon{{}, {companion}} {
		got, err := CaptureSupervisorFrozenPorts(frozenPortSnapshot(t, rows))
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("positive empty = (%#v,%v)", got, err)
		}
	}
	reads := 0
	got, err := captureSupervisorFrozenPorts(frozenPortSnapshot(t, []SupervisorDaemon{companion}), func(server, daemon string) (daemonPortResolution, error) {
		reads++
		return resolveManifestDaemonPort(server, daemon)
	})
	if err != nil || got == nil || reads != 1 {
		t.Fatalf("single companion resolution = (%#v,%v), reads=%d", got, err, reads)
	}
	if port, ok := EffectiveDaemonPort(companion); ok || port != 0 {
		t.Fatalf("legacy companion = (%d,%v)", port, ok)
	}
}

func TestCaptureSupervisorFrozenPorts_UnresolvedRefuses(t *testing.T) {
	frozenPortManifestFixtures(t)
	for _, raw := range [][]byte{nil, {}, []byte(`{`), []byte(`null`), []byte(`{"version":1}`), []byte(`{"version":1,"daemons":null}`), []byte(`{"version":1,"daemons":{}}`), []byte(`{"version":1,"daemons":[null]}`), []byte(`{"version":1,"daemons":[{}]}`), []byte(`{"version":0,"daemons":[]}`)} {
		if got, err := CaptureSupervisorFrozenPorts(raw); err == nil || got != nil {
			t.Fatalf("invalid input %q = (%#v,%v)", raw, got, err)
		}
	}
	base := SupervisorDaemon{TaskName: "unresolved", Command: "synthetic-command"}
	cases := []SupervisorDaemon{
		base,
		{TaskName: "negative", Command: base.Command, Port: -1},
		{TaskName: "large", Command: base.Command, Port: 65536},
		{Command: base.Command, Port: 9401},
		{TaskName: base.TaskName, Port: 9401},
		{TaskName: base.TaskName, Command: base.Command, Server: "capture-missing", Daemon: "default"},
		{TaskName: base.TaskName, Command: base.Command, Server: "capture-broken", Daemon: "default"},
		{TaskName: base.TaskName, Command: base.Command, Server: "capture-managed", Daemon: "absent"},
		{TaskName: base.TaskName, Command: base.Command, Server: "capture-managed", Daemon: "default", Args: []string{"daemon", "--server", "other", "--daemon", "default"}},
	}
	for _, args := range [][]string{
		{"daemon", "workspace-proxy"},
		{"daemon", "workspace-proxy", "--port"},
		{"daemon", "workspace-proxy", "--port", "bad"},
		{"daemon", "workspace-proxy", "--port", "0"},
		{"daemon", "workspace-proxy", "--port", "65536"},
		{"daemon", "workspace-proxy", "--port", "9401", "--port", "9401"},
		{"daemon", "workspace-proxy", "--port=9401"},
	} {
		row := base
		row.Args = args
		cases = append(cases, row)
	}
	for i, row := range cases {
		// A valid earlier row must never leak as partial success.
		raw := frozenPortSnapshot(t, []SupervisorDaemon{{TaskName: "valid", Command: base.Command, Port: 9400}, row})
		if got, err := CaptureSupervisorFrozenPorts(raw); err == nil || got != nil {
			t.Fatalf("unresolved row %d = (%#v,%v)", i, got, err)
		}
	}
	duplicate := SupervisorDaemon{TaskName: "duplicate", Command: base.Command, Port: 9400}
	if got, err := CaptureSupervisorFrozenPorts(frozenPortSnapshot(t, []SupervisorDaemon{duplicate, duplicate})); err == nil || got != nil {
		t.Fatalf("duplicate tuple = (%#v,%v)", got, err)
	}
	// A found numeric zero without positive companion semantics is unresolved.
	unknownZero := SupervisorDaemon{TaskName: "unknown-zero", Command: base.Command, Server: "synthetic", Daemon: "default"}
	if got, err := captureSupervisorFrozenPorts(frozenPortSnapshot(t, []SupervisorDaemon{unknownZero}), func(string, string) (daemonPortResolution, error) {
		return daemonPortResolution{}, nil
	}); err == nil || got != nil {
		t.Fatalf("unqualified manifest zero = (%#v,%v)", got, err)
	}
}

func TestCaptureSupervisorFrozenPorts_OriginalLegacyOperand(t *testing.T) {
	row := SupervisorDaemon{TaskName: "legacy", Command: "synthetic-command", Args: []string{"watchdog", "--once", "extra"}, Port: 9401}
	raw := frozenPortSnapshot(t, []SupervisorDaemon{row})
	normal, _, err := decodeSupervisorIntentFile("synthetic", raw)
	if err != nil || len(normal.Daemons) != 0 {
		t.Fatalf("normal legacy filter = (%#v,%v)", normal, err)
	}
	got, err := CaptureSupervisorFrozenPorts(raw)
	if err != nil || len(got) != 1 || got[0].TaskName != row.TaskName || got[0].Port != row.Port {
		t.Fatalf("original legacy port = (%#v,%v)", got, err)
	}
	row.Port = 0
	if got, err := CaptureSupervisorFrozenPorts(frozenPortSnapshot(t, []SupervisorDaemon{row})); err == nil || got != nil {
		t.Fatalf("unqualified legacy omission = (%#v,%v)", got, err)
	}
}

func TestCaptureSupervisorFrozenPorts_SnapshotOwnership(t *testing.T) {
	raw := []byte(`{"version":1,"intent_generation":7,"strict_mode":true,"daemons":[{"task_name":"owned","command":"synthetic-command","args":["a\\b","quoted\"arg"],"port":9401}],"maintenance_timers":[{"name":"timer","command":"synthetic-timer","args":[],"enabled":true}],"stops":{}}`)
	before := bytes.Clone(raw)
	got, err := CaptureSupervisorFrozenPorts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TaskName != "owned" || got[0].Port != 9401 {
		t.Fatalf("owned cohort = %#v", got)
	}
	if !bytes.Equal(raw, before) {
		t.Fatal("capture changed snapshot bytes or other fields")
	}
	want := append([]SupervisorFrozenPort(nil), got...)
	for i := range raw {
		raw[i] = 'x'
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("caller byte mutation changed returned rows")
	}
}

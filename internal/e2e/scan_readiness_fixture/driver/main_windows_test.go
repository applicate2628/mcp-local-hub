package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const failedStatus = `[{"server":"scan-readiness-fixture","daemon":"default","task_name":"\\mcp-local-hub-scan-readiness-fixture-default","state":"Running","service_state":"Degraded","readiness_stage":"mcp_initialize","readiness_settled":true,"readiness_failure":{"stage":"mcp_initialize","failure_id":"MCP_READINESS_HOST_UNAVAILABLE","retryable":true},"port":18080,"pid":4123}]`
const failedScan = `{"at":"2026-09-27T00:00:00Z","entries":[{"name":"scan-readiness-fixture","manifest_exists":true,"materialization_state":"degraded","readiness":{"settled":true,"materialization_state":"degraded","daemons":[{"task_name":"\\mcp-local-hub-scan-readiness-fixture-default","port":18080,"pid":4123,"pid_generation":7,"service_state":"Degraded","readiness_stage":"mcp_initialize","readiness_settled":true,"failure":{"stage":"mcp_initialize","failure_id":"MCP_READINESS_HOST_UNAVAILABLE","retryable":true}}],"primary_failure":{"stage":"mcp_initialize","failure_id":"MCP_READINESS_HOST_UNAVAILABLE","retryable":true}}}]}`
const readyStatus = `[{"server":"scan-readiness-fixture","daemon":"default","task_name":"\\mcp-local-hub-scan-readiness-fixture-default","state":"Running","service_state":"Running","readiness_stage":"complete","readiness_settled":true,"port":18080,"pid":4123}]`
const readyScan = `{"entries":[{"name":"scan-readiness-fixture","manifest_exists":true,"materialization_state":"ready","readiness":{"settled":true,"materialization_state":"ready","daemons":[{"task_name":"\\mcp-local-hub-scan-readiness-fixture-default","port":18080,"pid":4123,"pid_generation":7,"service_state":"Running","readiness_stage":"complete","readiness_settled":true}]}}]}`

func TestPlainPairsRequireSettledFailureThenSameGenerationRecovery(t *testing.T) {
	failed, err := parsePair([]byte(failedStatus), []byte(failedScan), false)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := parsePair([]byte(readyStatus), []byte(readyScan), true)
	if err != nil {
		t.Fatal(err)
	}
	failed.StartID, ready.StartID = "2026-09-27T00:00:00Z", "2026-09-27T00:00:00Z"
	if err := sameGeneration(failed, ready); err != nil {
		t.Fatal(err)
	}
	if failed.PID != 4123 || failed.Generation != 7 || ready.Port != 18080 {
		t.Fatalf("wrong identity: %+v %+v", failed, ready)
	}
}

func TestPairRejectsHealthyOnlyWrongIdentityAndMissingFailure(t *testing.T) {
	if _, err := parsePair([]byte(readyStatus), []byte(readyScan), false); err == nil {
		t.Fatal("healthy-only accepted as failed state")
	}
	if _, err := parsePair([]byte(failedStatus), []byte(readyScan), false); err == nil {
		t.Fatal("mismatched status/scan accepted")
	}
	if _, err := parsePair([]byte(failedStatus), []byte(`{"entries":[]}`), false); err == nil {
		t.Fatal("missing fixture entry accepted")
	}
	failed, _ := parsePair([]byte(failedStatus), []byte(failedScan), false)
	other := failed
	other.Generation++
	if err := sameGeneration(failed, other); err == nil {
		t.Fatal("generation drift accepted")
	}
	other = failed
	other.StartID = "other"
	failed.StartID = "original"
	if err := sameGeneration(failed, other); err == nil {
		t.Fatal("OS start drift accepted")
	}
}

func TestFocusOnlyFixtureRows(t *testing.T) {
	status := append([]byte(`[{"server":"foreign","task_name":"secret"},`), []byte(failedStatus[1:])...)
	focused, err := focusStatus(status)
	if err != nil {
		t.Fatal(err)
	}
	if string(focused) == "" || string(focused) == string(status) {
		t.Fatal("status not reduced to fixture")
	}
	if _, err := parsePair(append([]byte("["), append(focused, ']')...), []byte(failedScan), false); err != nil {
		t.Fatal(err)
	}
	scan := []byte(`{"entries":[{"name":"foreign","raw":"secret"},` + failedScan[len(`{"at":"2026-09-27T00:00:00Z","entries":[`):])
	entry, err := focusScan(scan)
	if err != nil {
		t.Fatal(err)
	}
	if string(entry) == "" || string(entry) == string(scan) {
		t.Fatal("scan not reduced to fixture")
	}
}

func TestFocusedReceiptExcludesUnrelatedDiagnosticAndClientRawData(t *testing.T) {
	status := strings.Replace(failedStatus, `"port":18080`, `"last_error":"secret","port":18080`, 1)
	row, err := focusStatus([]byte(status))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(row), "secret") {
		t.Fatal("status diagnostic escaped fixture receipt")
	}
	scan := strings.Replace(failedScan, `"manifest_exists":true`, `"client_presence":{"codex-cli":{"raw":{"token":"secret"}}},"manifest_exists":true`, 1)
	entry, err := focusScan([]byte(scan))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(entry), "secret") {
		t.Fatal("client raw config escaped fixture receipt")
	}
}

func TestFocusedPlainStatusDoesNotInventScanOnlyGeneration(t *testing.T) {
	row, err := focusStatus([]byte(failedStatus))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["pid_generation"]; exists {
		t.Fatalf("plain status receipt invented scan-only pid_generation: %s", row)
	}
	if string(fields["pid"]) != "4123" || string(fields["port"]) != "18080" {
		t.Fatalf("plain status identity lost: %s", row)
	}
}

func TestClosedWitnessIsDurableAndRedactedBeforeRelease(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if port <= 10000 {
		t.Fatalf("unexpected test port %d", port)
	}
	var g guestRun
	witness := []byte(`{"released":false,"held":1,"canceled":1,"events":[{"kind":"initialize_held"},{"kind":"initialize_canceled"}],"secret":"must-not-export"}`)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fixture/release" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		retained, readErr := os.ReadFile(filepath.Join(g.receiptDir, "closed-events.json"))
		if readErr != nil || bytes.Contains(retained, []byte("must-not-export")) || !bytes.Contains(retained, []byte(`"released":false`)) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	g = guestRun{receiptDir: t.TempDir(), port: port - 10000, client: &http.Client{Timeout: time.Second}}
	if err := g.releaseAfterWitness(context.Background(), witness); err != nil {
		t.Fatal(err)
	}
	if g.written != 1 {
		t.Fatalf("receipt files=%d want 1", g.written)
	}
}

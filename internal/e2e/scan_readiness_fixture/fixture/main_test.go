package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func rpc(t *testing.T, client *http.Client, url, method, sid string) *http.Response {
	t.Helper()
	var body string
	switch method {
	case "initialize":
		body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`
	case "tools/list":
		body = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	default:
		t.Fatalf("unknown method %q", method)
	}
	req, err := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestClosedInitializeCancellationAndOneWayRelease(t *testing.T) {
	f := newFixture()
	s := httptest.NewServer(f)
	defer s.Close()
	client := &http.Client{Timeout: time.Second}
	get, err := client.Get(s.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if get.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp=%d", get.StatusCode)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	done := make(chan error, 1)
	go func() {
		response, err := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	deadline := time.After(time.Second)
	for f.snapshot().Held == 0 {
		select {
		case <-deadline:
			t.Fatal("initialize was not held")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("CLOSED initialize returned before cancel: %v", err)
	default:
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled initialize completed without error")
	}
	deadline = time.After(time.Second)
	for f.snapshot().Canceled == 0 {
		select {
		case <-deadline:
			t.Fatal("fixture did not record cancellation")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if f.snapshot().Released {
		t.Fatal("cancellation opened latch")
	}
	bad := httptest.NewRecorder()
	f.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/fixture/release", nil))
	if bad.Code != http.StatusForbidden {
		t.Fatalf("external release=%d", bad.Code)
	}
	wrong, _ := client.Post(s.URL+"/fixture/other", "application/json", nil)
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong path=%d", wrong.StatusCode)
	}
	release, _ := client.Post(s.URL+"/fixture/release", "application/json", nil)
	release.Body.Close()
	if release.StatusCode != http.StatusNoContent {
		t.Fatalf("release=%d", release.StatusCode)
	}
	release, _ = client.Post(s.URL+"/fixture/release", "application/json", nil)
	release.Body.Close()
	if release.StatusCode != http.StatusConflict {
		t.Fatalf("repeat release=%d", release.StatusCode)
	}
	if f.snapshot().Releases != 1 {
		t.Fatalf("releases=%d", f.snapshot().Releases)
	}
}

func TestOpenSessionLifecycleAndBoundedEvents(t *testing.T) {
	f := newFixture()
	s := httptest.NewServer(f)
	defer s.Close()
	client := &http.Client{Timeout: time.Second}
	release, err := client.Post(s.URL+"/fixture/release", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	release.Body.Close()
	init := rpc(t, client, s.URL, "initialize", "")
	if init.StatusCode != http.StatusOK || init.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("initialize status/content=%d/%q", init.StatusCode, init.Header.Get("Content-Type"))
	}
	sid := init.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize did not mint session")
	}
	var envelope struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools json.RawMessage `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.NewDecoder(init.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	init.Body.Close()
	if envelope.Result.ProtocolVersion != "2025-03-26" || envelope.Result.Capabilities.Tools == nil || envelope.Result.ServerInfo.Name == "" {
		t.Fatalf("bad initialize: %+v", envelope)
	}
	list := rpc(t, client, s.URL, "tools/list", sid)
	var tools struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(list.Body).Decode(&tools); err != nil {
		t.Fatal(err)
	}
	list.Body.Close()
	if list.StatusCode != http.StatusOK || len(tools.Result.Tools) != 1 || tools.Result.Tools[0].Name != "fixture_ping" {
		t.Fatalf("list=%d %+v", list.StatusCode, tools)
	}
	del, _ := http.NewRequest(http.MethodDelete, s.URL+"/mcp", nil)
	del.Header.Set("Mcp-Session-Id", sid)
	terminated, err := client.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, terminated.Body)
	terminated.Body.Close()
	if terminated.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE=%d", terminated.StatusCode)
	}
	if got := f.snapshot(); got.Outstanding != 0 || got.Deletes != 1 || got.Lists != 1 {
		t.Fatalf("snapshot=%+v", got)
	}
}

func TestEventLogRetainsNewest64RecordedLifecycleEvents(t *testing.T) {
	f := newFixture()
	s := httptest.NewServer(f)
	defer s.Close()
	client := &http.Client{Timeout: time.Second}
	release, err := client.Post(s.URL+"/fixture/release", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	release.Body.Close()
	var sessions []string
	for i := 0; i < 70; i++ {
		init := rpc(t, client, s.URL, "initialize", "")
		sid := init.Header.Get("Mcp-Session-Id")
		init.Body.Close()
		if sid == "" {
			t.Fatalf("initialize %d has no session", i)
		}
		sessions = append(sessions, sid)
		list := rpc(t, client, s.URL, "tools/list", sid)
		list.Body.Close()
		if list.StatusCode != http.StatusOK {
			t.Fatalf("tools/list %d: %d", i, list.StatusCode)
		}
		del, err := http.NewRequest(http.MethodDelete, s.URL+"/mcp", nil)
		if err != nil {
			t.Fatal(err)
		}
		del.Header.Set("Mcp-Session-Id", sid)
		terminated, err := client.Do(del)
		if err != nil {
			t.Fatal(err)
		}
		terminated.Body.Close()
		if terminated.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE %d: %d", i, terminated.StatusCode)
		}
	}
	response, err := client.Get(s.URL + "/fixture/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var got fixtureSnapshot
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 64 || got.Dropped != 147 || got.Lists != 70 || got.Deletes != 70 || got.Outstanding != 0 || got.Releases != 1 {
		t.Fatalf("event retention/counters: %+v", got)
	}
	if got.Events[0].Kind != "deleted" || got.Events[0].Session != sessions[48] {
		t.Fatalf("oldest retained event is not session 49 deletion: %+v", got.Events[0])
	}
	for i := 49; i < 70; i++ {
		base := 1 + (i-49)*3
		for offset, kind := range []string{"initialized", "tools_list", "deleted"} {
			if event := got.Events[base+offset]; event.Kind != kind || event.Session != sessions[i] {
				t.Fatalf("retained event %d=%+v, want %s for session %d", base+offset, event, kind, i+1)
			}
		}
	}
}

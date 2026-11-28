package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"mcp-local-hub/internal/config"
)

func ordinaryTestManifest(t *testing.T, server, state string, port int, client bool) config.ServerManifest {
	t.Helper()
	raw := fmt.Sprintf("name: %s\nkind: global\ntransport: stdio-bridge\ncommand: fixture-child\ndaemons:\n  - name: default\n    port: %d\n    mcp_stdio_state: %s\n", server, port, state)
	if client {
		raw += "client_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n"
	}
	m, err := config.ParseManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

func TestHubOrdinaryAdmissionBeforeSession(t *testing.T) {
	h := newTestHandler(t)
	before, transport := LoadResolverSnapshot(), http.DefaultTransport
	t.Cleanup(func() {
		PublishResolverSnapshot(before)
		http.DefaultTransport = transport
	})
	local := ordinaryTestManifest(t, "local-fixture", "session-local", 19334, true)
	alias := ordinaryTestManifest(t, "alias-fixture", "shared", 19334, false)
	ordinary := ordinaryTestManifest(t, "ordinary-fixture", "shared", 19335, false)
	groups := []Group{
		{Name: "Alias", Servers: []string{alias.Name}},
		{Name: "Mixed", Servers: []string{local.Name, ordinary.Name}},
		{Name: "Ordinary", Servers: []string{ordinary.Name}},
		{Name: "Empty"},
		{Name: "Unresolved", Servers: []string{"unknown-fixture"}},
	}
	PublishResolverSnapshot(BuildResolverSnapshotFromManifestsAndGroups([]config.ServerManifest{local, alias, ordinary, ordinary}, groups))
	tokens := map[string]string{"claude-code": realToken}
	for _, group := range groups {
		tokens[GroupScopeKey(group.Name)] = realToken
	}
	publishTokenTable(HubTokenTable{Tokens: tokens})
	var outbound []string
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		method := peekMethod(mustReadDaemonRequest(req))
		outbound = append(outbound, method+"@"+req.URL.Port())
		return ordinaryHubTestResponse(method), nil
	})

	for _, row := range []struct {
		path     string
		status   int
		dispatch bool
	}{
		{"/clients/claude-code/mcp", http.StatusServiceUnavailable, false},
		{"/g/Alias/mcp", http.StatusServiceUnavailable, false},
		// A protected session-local member needs its received owner before any group session exists.
		{"/g/Mixed/mcp", http.StatusServiceUnavailable, false},
		{"/g/Ordinary/mcp", http.StatusOK, true},
		{"/g/Empty/mcp", http.StatusOK, false},
		{"/g/Unresolved/mcp", http.StatusOK, false},
	} {
		start := len(outbound)
		req := authedRequest(t, http.MethodPost, row.path, []byte(`{"jsonrpc":"2.0","id":17,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`))
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		id := res.Header().Get("Mcp-Session-Id")
		if res.Code != row.status || (id != "") != (row.status == http.StatusOK) {
			t.Fatalf("initialize %s = %d session=%t", row.path, res.Code, id != "")
		}
		if row.status == http.StatusServiceUnavailable {
			var wire struct {
				ID    int `json:"id"`
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &wire); err != nil || wire.ID != 17 || wire.Error.Code != -32000 || wire.Error.Message != errHubDaemonAdmission.Error() {
				t.Fatalf("unexpected generic denial for %s: %s", row.path, res.Body)
			}
			h.sessions.mu.RLock()
			allocated := len(h.sessions.sessions)
			h.sessions.mu.RUnlock()
			if allocated != 0 {
				t.Fatalf("initialize %s retained denied session state", row.path)
			}
		}
		if id != "" {
			h.sessions.Delete(id)
		}
		got := strings.Join(outbound[start:], ",")
		want := ""
		if row.dispatch {
			want = "initialize@19335,notifications/initialized@19335"
		}
		if got != want {
			t.Fatalf("initialize %s dispatches %q, want %q", row.path, got, want)
		}
	}
}

func mustReadDaemonRequest(req *http.Request) []byte {
	if req.Body == nil {
		return nil
	}
	body, _ := readAllBody(req)
	return body
}

func ordinaryHubTestResponse(method string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	if method == "initialize" {
		header.Set("Mcp-Session-Id", "fixture-daemon-sid")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`))}
	}
	return &http.Response{StatusCode: http.StatusAccepted, Header: header, Body: http.NoBody}
}

func TestHubOrdinaryAdmissionRechecksEffects(t *testing.T) {
	before, transport := LoadResolverSnapshot(), http.DefaultTransport
	t.Cleanup(func() {
		PublishResolverSnapshot(before)
		http.DefaultTransport = transport
	})
	const scope = "claude-code"
	manifest := ordinaryTestManifest(t, "ordinary-fixture", "shared", 19335, true)
	ref := canonicalDaemonRef{Server: manifest.Name, Daemon: manifest.Daemons[0].Name, Port: manifest.Daemons[0].Port}
	active := BuildResolverSnapshotFromManifests([]config.ServerManifest{manifest})
	manifest.ClientBindings = nil
	unbound := BuildResolverSnapshotFromManifests([]config.ServerManifest{manifest})
	foreign := ordinaryTestManifest(t, "different-fixture", "shared", ref.Port, true)
	reassigned := BuildResolverSnapshotFromManifests([]config.ServerManifest{foreign})
	target := hubDaemonRequest{ref: ref, scope: scope}
	sess := &hubSession{ScopeKey: scope, ProtocolVersion: "2025-11-25", InFlightRequests: make(map[requestIDKey]inflightEntry)}
	ctx := t.Context()
	rpcID := json.RawMessage(`1`)
	proto := "2025-11-25"

	t.Run("each list page", func(t *testing.T) {
		PublishResolverSnapshot(active)
		trips := 0
		http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			trips++
			_ = mustReadDaemonRequest(req)
			PublishResolverSnapshot(reassigned)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":2,"result":{"tools":[],"nextCursor":"next"}}`))}, nil
		})
		if _, err := postToolsList(ctx, target, "prior", proto); !errors.Is(err, errHubDaemonAdmission) || trips != 1 {
			t.Fatalf("pagination used stale endpoint: trips=%d error=%v", trips, err)
		}
	})

	t.Run("stale list result", func(t *testing.T) {
		PublishResolverSnapshot(active)
		sess.SnapshotAtInit = active
		sess.IntendedParticipants = []canonicalDaemonRef{ref}
		sess.InitSuccesses = map[canonicalDaemonRef]string{ref: "prior"}
		http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			_ = mustReadDaemonRequest(req)
			PublishResolverSnapshot(reassigned)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"stale","description":"old response"}]}}`))}, nil
		})
		body, err := AggregateToolsList(ctx, sess, rpcID, "instance")
		routes := sess.RouteMap.Load()
		if err != nil || strings.Contains(string(body), "old response") || (routes != nil && len(*routes) != 0) {
			t.Fatalf("stale list published a route: %s %v", body, err)
		}
	})

	t.Run("call, cancellation and cleanup", func(t *testing.T) {
		PublishResolverSnapshot(reassigned)
		var methods []string
		http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			methods = append(methods, req.Method+"@"+req.URL.Port())
			_ = mustReadDaemonRequest(req)
			return ordinaryHubTestResponse(""), nil
		})
		if _, err := postToolsCall(ctx, target, "prior", proto, rpcID, json.RawMessage(`{"name":"fixture"}`)); !errors.Is(err, errHubDaemonAdmission) || isRetriableTransportFailure(err) {
			t.Fatalf("ordinary call should be terminally denied: %v", err)
		}
		key, _ := newRequestIDKey(rpcID)
		sess.InsertInFlight(key, inflightEntry{DaemonRef: ref, DaemonSessionID: "prior", DaemonRequestID: rpcID})
		ForwardCancellation(ctx, sess, rpcID)
		if _, ok := sess.LookupInFlight(key); !ok {
			t.Fatal("denied cancel destroyed the call-owned settlement row")
		}
		if err := bestEffortDeleteDaemonSession(ctx, ref, "prior", proto); !errors.Is(err, errHubDaemonAdmission) || len(methods) != 0 {
			t.Fatalf("captured cancellation or cleanup reached the new port owner: %v %v", methods, err)
		}
		PublishResolverSnapshot(unbound)
		if err := bestEffortDeleteDaemonSession(ctx, ref, "prior", proto); err != nil || strings.Join(methods, ",") != "DELETE@"+strconv.Itoa(ref.Port) {
			t.Fatalf("scope removal blocked the still-owned ordinary cleanup: %v %v", methods, err)
		}
		sess.RemoveInFlight(key)
	})

	t.Run("DELETE settles the front session", func(t *testing.T) {
		h := newTestHandler(t)
		for _, row := range []struct {
			snapshot *ResolverSnapshot
			effects  int
		}{{unbound, 1}, {reassigned, 0}} {
			PublishResolverSnapshot(active)
			front, err := h.sessions.Create(scope, proto, active)
			if err != nil {
				t.Fatal(err)
			}
			front.InitSuccesses[ref] = "prior"
			front.DaemonProtoVer[ref] = proto
			PublishResolverSnapshot(row.snapshot)
			var effects int
			http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				effects++
				if req.Method != http.MethodDelete || req.URL.Port() != strconv.Itoa(ref.Port) || req.Header.Get("Mcp-Session-Id") != "prior" {
					return nil, errors.New("cleanup did not address the captured daemon session")
				}
				return ordinaryHubTestResponse(""), nil
			})
			req := authedRequest(t, http.MethodDelete, "/clients/claude-code/mcp", nil)
			req.Header.Set("Mcp-Session-Id", front.ClientSessionID)
			req.Header.Set("MCP-Protocol-Version", proto)
			resp := httptest.NewRecorder()
			h.ServeHTTP(resp, req)
			if _, exists := h.sessions.Get(front.ClientSessionID); resp.Code != http.StatusNoContent || exists || effects != row.effects {
				t.Fatalf("DELETE local settlement: code=%d retained=%v effects=%d", resp.Code, exists, effects)
			}
		}
	})

	t.Run("reinit cache", func(t *testing.T) {
		PublishResolverSnapshot(active)
		s := &hubSession{ScopeKey: scope, ProtocolVersion: proto}
		cleaned := make(chan string, 1)
		http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodDelete {
				cleaned <- req.Header.Get("Mcp-Session-Id")
				return ordinaryHubTestResponse(""), nil
			}
			method := peekMethod(mustReadDaemonRequest(req))
			if method == "notifications/initialized" {
				PublishResolverSnapshot(unbound)
			}
			return ordinaryHubTestResponse(method), nil
		})
		if _, err := s.reinitializeDaemonBinding(ctx, ref); !errors.Is(err, errHubDaemonAdmission) || len(s.InitSuccesses) != 0 {
			t.Fatalf("stale reinit published session: %v", err)
		}
		select {
		case id := <-cleaned:
			if id != "fixture-daemon-sid" {
				t.Fatal("retirement did not use its captured daemon session")
			}
		case <-time.After(PerDaemonInitTimeout):
			t.Fatal("uncached reinit did not settle its still-ordinary session")
		}
	})

	t.Run("redirect admission", func(t *testing.T) {
		for _, row := range []struct {
			destination string
			retire      bool
			trips       int
		}{
			{"/follow", false, 2},
			{"/follow", true, 1},
			{"http://127.0.0.1:19334/follow", false, 1},
		} {
			PublishResolverSnapshot(active)
			trips := 0
			http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				trips++
				_ = mustReadDaemonRequest(req)
				if trips == 1 {
					if row.retire {
						PublishResolverSnapshot(reassigned)
					}
					return &http.Response{StatusCode: 307, Header: http.Header{"Location": {row.destination}}, Body: http.NoBody}, nil
				}
				return ordinaryHubTestResponse(""), nil
			})
			_, _, err := doDaemonPost(ctx, target, []byte("{}"), "prior", proto, PerDaemonInitTimeout, false)
			if trips != row.trips || (err == nil) != (row.trips == 2) || (err != nil && !errors.Is(err, errHubDaemonAdmission)) {
				t.Fatalf("redirect %q revalidated=%t trips=%d error=%v", row.destination, row.retire, trips, err)
			}
		}
	})
}

func TestHubToolsCallStopsAfterProactiveAdmissionRefusal(t *testing.T) {
	before, transport := LoadResolverSnapshot(), http.DefaultTransport
	t.Cleanup(func() {
		PublishResolverSnapshot(before)
		http.DefaultTransport = transport
	})
	const scope, proto = "claude-code", "2025-11-25"
	original := ordinaryTestManifest(t, "ordinary-fixture", "shared", 19335, true)
	replacement := ordinaryTestManifest(t, original.Name, "shared", 19336, true)
	old := canonicalDaemonRef{Server: original.Name, Daemon: "default", Port: 19335}
	originalSnapshot := BuildResolverSnapshotFromManifests([]config.ServerManifest{original})
	sess := &hubSession{
		ScopeKey: scope, ProtocolVersion: proto, SnapshotAtInit: originalSnapshot,
		IntendedParticipants: []canonicalDaemonRef{old},
		InitSuccesses:        map[canonicalDaemonRef]string{old: "prior-sid"},
		InFlightRequests:     make(map[requestIDKey]inflightEntry),
	}
	if !sess.markStalePort(old.Port) {
		t.Fatal("fixture did not mark the original endpoint stale")
	}
	PublishResolverSnapshot(BuildResolverSnapshotFromManifests([]config.ServerManifest{replacement}))
	var effects []string
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		method := peekMethod(mustReadDaemonRequest(req))
		effects = append(effects, method+"@"+req.URL.Port())
		if method == "initialize" && req.URL.Port() == "19336" {
			// The replacement handoff is refused. The old port is ordinary
			// again, so a fallback to its old SID would have a visible effect.
			PublishResolverSnapshot(originalSnapshot)
			return nil, errHubDaemonAdmission
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))}, nil
	})
	route := canonicalToolRef{Server: old.Server, Daemon: old.Daemon, Port: old.Port, RawName: "echo"}
	body, err := dispatchToolsCall(t.Context(), sess, json.RawMessage(`17`), json.RawMessage(`{"name":"ordinary-fixture__echo"}`), route, "prior-sid", proto)
	if err != nil || strings.Join(effects, ",") != "initialize@19336" || sess.InFlightCount() != 0 {
		t.Fatalf("terminal proactive refusal allowed another effect: calls=%v in-flight=%d error=%v", effects, sess.InFlightCount(), err)
	}
	code, message := decodeRPCError(t, body)
	if code != -32000 || message != "tools/call failed: "+errHubDaemonAdmission.Error() || !strings.Contains(string(body), `"id":17`) {
		t.Fatalf("unexpected proactive refusal wire response: %s", body)
	}
}

func TestHubToolsCallBlockedRedirectHidesTarget(t *testing.T) {
	before, transport := LoadResolverSnapshot(), http.DefaultTransport
	t.Cleanup(func() {
		PublishResolverSnapshot(before)
		http.DefaultTransport = transport
	})
	const scope, proto = "claude-code", "2025-11-25"
	manifest := ordinaryTestManifest(t, "ordinary-fixture", "shared", 19335, true)
	PublishResolverSnapshot(BuildResolverSnapshotFromManifests([]config.ServerManifest{manifest}))
	sess := &hubSession{ScopeKey: scope, ProtocolVersion: proto, InFlightRequests: make(map[requestIDKey]inflightEntry)}
	const marker = "r252-inert-marker"
	trips := 0
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		trips++
		_ = mustReadDaemonRequest(req)
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{
			"Location": {"http://127.0.0.1:19334/" + marker + "?fixture=" + marker},
		}, Body: http.NoBody}, nil
	})
	ref := canonicalToolRef{Server: manifest.Name, Daemon: "default", Port: 19335, RawName: "echo"}
	body, err := dispatchToolsCall(t.Context(), sess, json.RawMessage(`23`), json.RawMessage(`{"name":"ordinary-fixture__echo"}`), ref, "prior-sid", proto)
	if err != nil || trips != 1 || sess.InFlightCount() != 0 {
		t.Fatalf("blocked redirect did not settle: trips=%d in-flight=%d err=%v", trips, sess.InFlightCount(), err)
	}
	code, message := decodeRPCError(t, body)
	if code != -32000 || strings.Contains(string(body), marker) || message != "tools/call failed: "+errHubDaemonAdmission.Error() {
		t.Fatalf("unexpected redirect refusal wire response: %s", body)
	}
}

func TestHubToolsCallDoesNotReplayAfterRedirectDialFailure(t *testing.T) {
	before, transport := LoadResolverSnapshot(), http.DefaultTransport
	t.Cleanup(func() {
		PublishResolverSnapshot(before)
		http.DefaultTransport = transport
	})
	const scope, proto = "claude-code", "2025-11-25"
	manifest := ordinaryTestManifest(t, "ordinary-fixture", "shared", 19335, true)
	PublishResolverSnapshot(BuildResolverSnapshotFromManifests([]config.ServerManifest{manifest}))
	sess := &hubSession{ScopeKey: scope, ProtocolVersion: proto, InFlightRequests: make(map[requestIDKey]inflightEntry)}
	var calls []string
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		method := peekMethod(mustReadDaemonRequest(req))
		calls = append(calls, method+" "+req.URL.Path)
		if len(calls) == 1 {
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": {"/fixture-follow"}},
				Body:       http.NoBody,
			}, nil
		}
		if req.URL.Path == "/fixture-follow" {
			return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
		}
		return ordinaryHubTestResponse(method), nil
	})
	ref := canonicalToolRef{Server: manifest.Name, Daemon: "default", Port: 19335, RawName: "echo"}
	body, err := dispatchToolsCall(t.Context(), sess, json.RawMessage(`29`), json.RawMessage(`{"name":"ordinary-fixture__echo"}`), ref, "prior-sid", proto)
	if err != nil || strings.Join(calls, ",") != "tools/call /mcp,tools/call /fixture-follow" || sess.InFlightCount() != 0 {
		t.Fatalf("later-hop dial was treated as a first-hop failure: calls=%v in-flight=%d err=%v", calls, sess.InFlightCount(), err)
	}
	code, message := decodeRPCError(t, body)
	if code != -32000 || message != "tools/call failed: daemon redirect failed" || strings.Contains(string(body), "fixture-follow") {
		t.Fatalf("later-hop failure exposed redirect data: %s", body)
	}
}

type admissionDeadlineBody struct{ ctx context.Context }

func (*admissionDeadlineBody) Read([]byte) (int, error) { return 0, io.EOF }

func (b *admissionDeadlineBody) Close() error {
	// The transport refused before it closed the body. Advance the test clock
	// past Client.Timeout by waiting for this request's later bound deadline.
	<-b.ctx.Done()
	return nil
}

func TestHubAdmissionRefusalSurvivesClientTimeout(t *testing.T) {
	before, transport := LoadResolverSnapshot(), http.DefaultTransport
	t.Cleanup(func() {
		PublishResolverSnapshot(before)
		http.DefaultTransport = transport
	})
	PublishResolverSnapshot(nil)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		trips := 0
		http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			trips++
			return nil, errors.New("refused request entered test transport")
		})
		ref := canonicalDaemonRef{Server: "ordinary-fixture", Daemon: "default", Port: 19335}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:19335/mcp?r255-inert-timeout", &admissionDeadlineBody{ctx: ctx})
		if err != nil {
			t.Fatal(err)
		}
		_, err = doHubDaemonRequest(req, hubDaemonRequest{ref: ref, scope: "claude-code"}, time.Second)
		if err != errHubDaemonAdmission || trips != 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("actual admission refusal lost: trips=%d ctx=%v err=%v", trips, ctx.Err(), err)
		}
	})
}

func TestHubToolsCallRedirectFailuresHideLocation(t *testing.T) {
	before, transport := LoadResolverSnapshot(), http.DefaultTransport
	t.Cleanup(func() {
		PublishResolverSnapshot(before)
		http.DefaultTransport = transport
	})
	const scope, proto = "claude-code", "2025-11-25"
	const marker = "r255-inert-location"
	manifest := ordinaryTestManifest(t, "ordinary-fixture", "shared", 19335, true)
	PublishResolverSnapshot(BuildResolverSnapshotFromManifests([]config.ServerManifest{manifest}))
	ref := canonicalToolRef{Server: manifest.Name, Daemon: "default", Port: 19335, RawName: "echo"}
	for _, row := range []struct {
		name, location string
		wantTrips      int
	}{
		{"default cap", "/" + marker + "?fixture=" + marker, 10},
		{"malformed Location", "http://[" + marker + "?fixture=" + marker, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			trips := 0
			http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				trips++
				_ = mustReadDaemonRequest(req)
				return &http.Response{
					StatusCode: http.StatusTemporaryRedirect,
					Header:     http.Header{"Location": {row.location}},
					Body:       http.NoBody,
				}, nil
			})
			sess := &hubSession{ScopeKey: scope, ProtocolVersion: proto, InFlightRequests: make(map[requestIDKey]inflightEntry)}
			body, err := dispatchToolsCall(t.Context(), sess, json.RawMessage(`41`), json.RawMessage(`{"name":"ordinary-fixture__echo"}`), ref, "prior", proto)
			if err != nil || trips != row.wantTrips || sess.InFlightCount() != 0 {
				t.Fatalf("redirect handling changed: trips=%d in-flight=%d err=%v", trips, sess.InFlightCount(), err)
			}
			code, message := decodeRPCError(t, body)
			if code != -32000 || message != "tools/call failed: daemon redirect failed" || strings.Contains(string(body), marker) {
				t.Fatalf("redirect handling exposed its Location: %s", body)
			}
		})
	}
}

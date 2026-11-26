package gui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"mcp-local-hub/internal/api"
)

func TestStreamLogsInvalidFileMatchesSnapshotError(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	dir := api.DefaultLogDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "directory-default.log")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(dir, "linked-dir-default.log")
	if err := os.Symlink(target, linkedDir); err != nil {
		t.Run("symlink setup", func(t *testing.T) { t.Skipf("directory symlink unavailable: %v", err) })
	}
	for _, name := range []string{"directory", "linked-dir"} {
		t.Run(name, func(t *testing.T) {
			if _, err := os.Lstat(filepath.Join(dir, name+"-default.log")); errors.Is(err, os.ErrNotExist) {
				t.Skip("symlink fixture unavailable")
			}
			provider := realLogs{}
			_, wantErr := provider.Logs(name, "", 0)
			if wantErr == nil {
				t.Fatal("snapshot accepted a directory")
			}
			if _, err := provider.PollDaemonLog(name, "", 0, true); err == nil || err.Error() != wantErr.Error() {
				t.Fatalf("prime error %v, want %v", err, wantErr)
			}
			state := &Server{logs: provider}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/logs/"+name+"/stream", nil)
			ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
			defer cancel()
			streamLogs(state, name, "", rec, req.WithContext(ctx))
			want := "event: error\ndata: " + wantErr.Error() + "\n\n"
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Body.String() != want {
				t.Fatalf("status=%d headers=%v body=%q want=%q", rec.Code, rec.Header(), rec.Body.String(), want)
			}
		})
	}

	t.Run("regular leaf symlink", func(t *testing.T) {
		regular := filepath.Join(dir, "regular.log")
		if err := os.WriteFile(regular, []byte("line\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(regular, filepath.Join(dir, "linked-leaf-default.log")); err != nil {
			t.Skipf("regular leaf symlink unavailable: %v", err)
		}
		poll, err := (realLogs{}).PollDaemonLog("linked-leaf", "", 0, false)
		if err != nil || poll != (api.DaemonLogPoll{Offset: 5, Suffix: "line\n"}) {
			t.Fatalf("poll got %+v, %v", poll, err)
		}
	})
}

func TestStreamLogsFramesAtPollCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		initial := int64(70)
		delta := "\nready\n\n" + strings.Repeat("x", maxPendingLineBytes+20)
		fl := &scriptedLogs{seq: []api.DaemonLogPoll{
			{Offset: initial},
			{Offset: initial + 3, Suffix: "old"},
			{Offset: initial + 3}, // absence leaves the pending fragment alone
			{Offset: 3},           // shrink discards it
			{Offset: 3 + int64(len(delta)), Suffix: delta},
			{Offset: 3 + int64(len(delta))}, // quiet tick before completion
			{Offset: 9 + int64(len(delta)), Suffix: " tail\n"},
		}}
		rec := newSSERecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/logs/example/stream?daemon=worker", nil)
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			streamLogs(&Server{logs: fl}, "example", "worker", rec, req.WithContext(ctx))
			close(done)
		}()
		synctest.Wait()
		time.Sleep(499 * time.Millisecond)
		synctest.Wait()
		calls, _, _ := fl.observation()
		if calls != 1 || rec.body() != "" {
			t.Fatalf("before first tick: calls=%d body length=%d", calls, len(rec.body()))
		}
		time.Sleep(1501 * time.Millisecond)
		synctest.Wait()
		want := "event: log-line\ndata: ready\n\n" +
			"event: log-line\ndata: " + strings.Repeat("x", maxPendingLineBytes) + " [truncated unterminated line]\n\n"
		calls, _, _ = fl.observation()
		if calls != 5 || rec.body() != want {
			t.Fatalf("overflow: calls=%d body length=%d want=%d", calls, len(rec.body()), len(want))
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		calls, _, _ = fl.observation()
		if calls != 6 || rec.body() != want {
			t.Fatalf("quiet tick: calls=%d body length=%d", calls, len(rec.body()))
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		want += "event: log-line\ndata: " + strings.Repeat("x", maxPendingLineBytes) + " tail\n\n"
		calls, server, daemon := fl.observation()
		if calls != 7 || rec.body() != want || server != "example" || daemon != "worker" {
			t.Fatalf("complete: calls=%d body length=%d want=%d server=%q daemon=%q", calls, len(rec.body()), len(want), server, daemon)
		}
		cancel()
		<-done
		time.Sleep(time.Second)
		calls, _, _ = fl.observation()
		if calls != 7 {
			t.Fatalf("poll after cancellation: calls=%d", calls)
		}
	})
}

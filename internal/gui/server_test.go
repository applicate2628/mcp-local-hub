package gui

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestServer_StartAndShutdown(t *testing.T) {
	s := NewServer(Config{Port: 0}) // 0 = OS picks free port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start(ctx, ready)
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server never signaled ready")
	}
	if s.Port() == 0 {
		t.Fatal("Port() returned 0 after ready")
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(s.Port()) + "/api/ping")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ping status %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

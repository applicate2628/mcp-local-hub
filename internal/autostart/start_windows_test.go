//go:build windows

package autostart

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

type guardedStartScheduler struct {
	fakeScheduler
	calls []scheduler.OwnedRunRequest
}

func (s *guardedStartScheduler) RunOwned(_ context.Context, request scheduler.OwnedRunRequest) error {
	s.calls = append(s.calls, request)
	return nil
}

func TestStartOwnerForExpectedProductAdmission(t *testing.T) {
	product := filepath.Join(t.TempDir(), "mcphub.exe")
	if err := os.WriteFile(product, []byte("synthetic product"), 0600); err != nil {
		t.Fatal(err)
	}
	old := schedulerFactoryFn
	t.Cleanup(func() { schedulerFactoryFn = old })
	s := &guardedStartScheduler{}
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return s, nil }
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		product string
	}{
		{"nil-context", nil, product}, {"canceled", canceled, product},
		{"empty", context.Background(), ""}, {"relative", context.Background(), "mcphub.exe"},
		{"missing", context.Background(), filepath.Join(t.TempDir(), "missing.exe")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := StartOwnerForExpectedProduct(tc.ctx, tc.product); err == nil {
				t.Fatal("unsafe admission succeeded")
			}
			if len(s.calls) != 0 {
				t.Fatal("guarded native run entered")
			}
		})
	}
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return &fakeScheduler{}, nil }
	if err := StartOwnerForExpectedProduct(context.Background(), product); err == nil {
		t.Fatal("unsupported capability succeeded")
	}
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return s, nil }
	if err := StartOwnerForExpectedProduct(context.Background(), product); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 1 || s.calls[0].Name != WindowsTaskName || s.calls[0].ExpectedCommand != product || s.calls[0].ExpectedRole != "gui" || s.calls[0].ExpectedOwner == "" {
		t.Fatalf("request=%+v", s.calls)
	}
}

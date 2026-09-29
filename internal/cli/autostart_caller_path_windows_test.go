//go:build windows

package cli

import (
	"errors"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/autostart"
)

func TestWindowsAutostartCanonicalOwnerIgnoresCallerExecutable(t *testing.T) {
	canonical := `C:\\synthetic\\.local\\bin\\mcphub.exe`
	for _, tc := range []struct {
		name       string
		opts       autostart.Options
		resolveErr error
		wantErr    bool
	}{
		{name: "default_enable", opts: autostart.Options{}},
		{name: "explicit_override", opts: autostart.Options{MCPHubPath: `D:\\portable\\mcphub.exe`}},
		{name: "missing_canonical", resolveErr: errors.New("missing canonical"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBackend{statusReturn: autostart.StateEnabledStopped}
			backend := newCanonicalAutostartBackend(fb, func() (string, error) { return canonical, tc.resolveErr })
			err := backend.Enable(tc.opts)
			if tc.wantErr {
				if err == nil || len(fb.enableCalls) != 0 {
					t.Fatalf("error=%v calls=%v", err, fb.enableCalls)
				}
				return
			}
			if err != nil || len(fb.enableCalls) != 1 {
				t.Fatalf("enable error=%v calls=%v", err, fb.enableCalls)
			}
			want := canonical
			if tc.opts.MCPHubPath != "" {
				want = tc.opts.MCPHubPath
			}
			if fb.enableCalls[0].MCPHubPath != want {
				t.Fatalf("path=%q want=%q", fb.enableCalls[0].MCPHubPath, want)
			}
			if _, err := backend.Status(tc.opts); err != nil {
				t.Fatal(err)
			}
			if _, err := backend.StatusSnapshot(tc.opts); err != nil {
				t.Fatal(err)
			}
		})
	}

	fb := &fakeBackend{}
	resolverCalls := 0
	backend := newCanonicalAutostartBackend(fb, func() (string, error) { resolverCalls++; return canonical, nil })
	if err := backend.Disable(); err != nil || resolverCalls != 0 || fb.disableCalls != 1 {
		t.Fatalf("disable err=%v resolver=%d calls=%d", err, resolverCalls, fb.disableCalls)
	}

	failingFake := &fakeBackend{}
	failing := newCanonicalAutostartBackend(failingFake, func() (string, error) { return "", errors.New("missing") })
	if _, err := failing.Status(autostart.Options{}); err == nil {
		t.Fatal("Status accepted missing canonical")
	}
	if len(failingFake.statusCalls) != 0 {
		t.Fatalf("Status resolver failure reached backend: %v", failingFake.statusCalls)
	}
	if _, err := failing.StatusSnapshot(autostart.Options{}); err == nil {
		t.Fatal("StatusSnapshot accepted missing canonical")
	}
	if len(failingFake.statusCalls) != 0 {
		t.Fatalf("StatusSnapshot resolver failure reached backend: %v", failingFake.statusCalls)
	}
}

func TestDefaultStrictModeDepsUsesAutostartFactory(t *testing.T) {
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	fb := &fakeBackend{}
	prev := autostartBackendFactoryFn
	calls := 0
	autostartBackendFactoryFn = func() (autostart.Backend, error) { calls++; return fb, nil }
	t.Cleanup(func() { autostartBackendFactoryFn = prev })
	deps, err := defaultStrictModeDeps()
	if err != nil || calls != 1 || deps.AutostartBackend != fb {
		t.Fatalf("deps=%+v err=%v calls=%d", deps, err, calls)
	}
}

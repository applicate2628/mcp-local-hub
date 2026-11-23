package api

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHubRouteFixedBindNative(t *testing.T) {
	for _, scenario := range []string{"missing qualification", "changed protected endpoint", "busy socket", "reload present", "late reload collision", "retire exact", "preserve replacement", "published witness with failed start"} {
		t.Run(scenario, func(t *testing.T) {
			dir := isolateStateDir(t)
			if _, err := EnsureHubTokens([]string{"claude-code"}); err != nil {
				t.Fatal(err)
			}
			holder, err := NewListenerWithSOExclusiveContext(t.Context(), "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			address := holder.Addr().String()
			if _, err := EnsureHubEndpoint(holder.Addr().(*net.TCPAddr).Port, os.Getpid()); err != nil {
				t.Fatal(err)
			}
			if scenario != "busy socket" {
				if err := holder.Close(); err != nil {
					t.Fatal(err)
				}
			}
			id, err := ObserveHubRouteStateDirectory(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			fixture := hubRouteFixture{dir: dir, id: id}
			originalClient := fixture.bytes(t, hubMcpTokensFileLeaf)
			path := filepath.Join(dir, hubMcpControlTokenFileLeaf)
			canary := []byte("fixture foreign content\n")
			var foreign hubRouteLeaf
			var result *HubMcpBindResult
			var qualification HubRouteQualification
			var originalEndpoint hubRouteLeaf
			late := errors.New("native endpoint rename completed before fixture error")
			defer func() {
				if result != nil {
					_ = result.CloseUnserved(t.Context())
				}
			}()
			fixture.with(t, func(h *HubRouteState) error {
				endpoint, err := h.ReadEndpoint()
				if err != nil {
					return err
				}
				originalEndpoint = endpoint.leaf
				qualification = testHubRouteQualification(endpoint, "a")
				if scenario != "missing qualification" {
					if err := h.StageQualification("", endpoint, qualification); err != nil {
						return err
					}
				}
				createForeign := func() error {
					if err := createFixedBindFixtureLeaf(h, canary); err != nil {
						return err
					}
					foreign, err = h.core.native.read(hubMcpControlTokenFileLeaf, hubRouteReloadLimit)
					return err
				}
				switch scenario {
				case "changed protected endpoint":
					if err := publishFixtureEndpoint(h, endpoint, qualification); err != nil {
						return err
					}
					changed, err := h.ReadEndpoint()
					if err != nil {
						return err
					}
					originalEndpoint = changed.leaf
				case "reload present":
					if err := createForeign(); err != nil {
						return err
					}
				case "late reload collision":
					h.core.native.beforeRename = createForeign
				case "published witness with failed start":
					renames := 0
					h.core.native.afterRename = func() error {
						renames++
						if renames == 2 {
							return late
						}
						return nil
					}
				}
				result, err = h.BindExistingHubMcpListener(endpoint, qualification)
				if scenario == "retire exact" || scenario == "preserve replacement" {
					if err != nil || result == nil {
						return errors.Join(errors.New("qualified bind did not return the socket and reload handler"), err)
					}
					if result.Listener.Addr().String() != address {
						return errors.New("fixed bind returned a different socket")
					}
					contender, bindErr := NewListenerWithSOExclusiveContext(t.Context(), address)
					if bindErr == nil {
						return errors.Join(errors.New("fixed listener did not hold the exclusive socket"), contender.Close())
					}
					return result.Reload.VerifyOriginalReload(h)
				}
				if result != nil || err == nil {
					return errors.New("refusal returned effect authority")
				}
				if scenario == "published witness with failed start" {
					var outcome *HubRoutePublicationError
					if !errors.Is(err, late) || !errors.As(err, &outcome) || outcome.Outcome != HubRoutePublicationApplied {
						return errors.Join(errors.New("endpoint publication outcome lost"), err)
					}
				}
				return nil
			})

			fixture.with(t, func(h *HubRouteState) error {
				current, err := h.ReadEndpoint()
				if err != nil {
					return err
				}
				reload, err := h.core.native.read(hubMcpControlTokenFileLeaf, hubRouteReloadLimit)
				if err != nil {
					return err
				}
				if result != nil || scenario == "published witness with failed start" {
					if current.Witness() != qualification.TransactionID || current.Endpoint().PID != os.Getpid() {
						return errors.New("missing current endpoint witness")
					}
				} else if !current.leaf.matches(originalEndpoint) {
					return errors.New("failure changed the endpoint preimage")
				}
				if foreign.present && !reload.matches(foreign) {
					return errors.New("native collision changed the foreign reload object")
				}
				if result != nil {
					if result.Listener.Addr().String() != address {
						return errors.New("fixed bind returned a different socket")
					}
					contender, bindErr := NewListenerWithSOExclusiveContext(t.Context(), address)
					if bindErr == nil {
						return errors.Join(errors.New("fixed listener did not hold the exclusive socket"), contender.Close())
					}
					return result.Reload.VerifyOriginalReload(h)
				}
				if !foreign.present && reload.present {
					return errors.New("failure left an owned reload credential")
				}
				return nil
			})
			if !bytes.Equal(originalClient, fixture.bytes(t, hubMcpTokensFileLeaf)) {
				t.Fatal("client credentials changed")
			}

			if scenario == "preserve replacement" {
				old := result.Reload.original.leaf
				if err := os.Rename(path, path+".fixture-prior"); err != nil {
					t.Fatal(err)
				}
				fixture.with(t, func(h *HubRouteState) error { return createFixedBindFixtureLeaf(h, old.data) })
				if err := result.CloseUnserved(t.Context()); !errors.Is(err, errHubRouteStateChanged) {
					t.Fatalf("retirement did not refuse an equal-byte foreign object: %v", err)
				}
				fixture.with(t, func(h *HubRouteState) error {
					current, err := h.core.native.read(hubMcpControlTokenFileLeaf, hubRouteReloadLimit)
					if err != nil || !current.present || current.identity == old.identity || !bytes.Equal(current.data, old.data) {
						return errors.Join(errors.New("foreign replacement was lost"), err)
					}
					return nil
				})
				result = nil
			} else if result != nil {
				if err := result.CloseUnserved(t.Context()); err != nil {
					t.Fatal(err)
				}
				result = nil
				fixture.with(t, func(h *HubRouteState) error { return h.CheckReloadTokenAbsent() })
			}
			if scenario != "busy socket" {
				rebound, err := NewListenerWithSOExclusiveContext(t.Context(), address)
				if err != nil {
					t.Fatalf("fixture socket did not become vacant: %v", err)
				}
				if err := rebound.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func createFixedBindFixtureLeaf(h *HubRouteState, payload []byte) error {
	sd, err := buildRestrictiveSecurityDescriptor()
	if err != nil {
		return err
	}
	handle, err := ntCreateRelative(h.core.native.parent, hubMcpControlTokenFileLeaf, windows.GENERIC_WRITE|windows.SYNCHRONIZE,
		windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, sd)
	if err != nil {
		return err
	}
	return errors.Join(windowsWriteAll(handle, payload), windows.CloseHandle(handle))
}

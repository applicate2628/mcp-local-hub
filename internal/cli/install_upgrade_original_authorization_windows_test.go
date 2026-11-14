//go:build windows

package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

type authorizationHeldSupervisor struct{ pid, terminated, closed int }

func (h *authorizationHeldSupervisor) PID() int { return h.pid }
func (h *authorizationHeldSupervisor) VerifyIdentity(p process.PIDIdentityProof) error {
	if p.PID != h.pid || p.ExecutablePath != `C:\Hub\mcphub.exe` {
		return fmt.Errorf("wrong original subject")
	}
	return nil
}
func (h *authorizationHeldSupervisor) Terminate() (bool, error) { h.terminated++; return true, nil }
func (h *authorizationHeldSupervisor) Close() error             { h.closed++; return nil }

func TestOriginalUpgradeStopRetainsExactRoleAndCurrentUserAuthorization(t *testing.T) {
	oldLookup, oldSID, oldPIDKill := processLookupIdentityFn, reapOwnerSIDMatchesCurrentFn, killPIDViaTaskkillFn
	t.Cleanup(func() {
		processLookupIdentityFn, reapOwnerSIDMatchesCurrentFn, killPIDViaTaskkillFn = oldLookup, oldSID, oldPIDKill
	})
	legacyPIDKills := 0
	killPIDViaTaskkillFn = func(int) error { legacyPIDKills++; return nil }
	owner := api.SupervisorLockOwner{PID: 71, StartedAt: time.Unix(1700000000, 0).UTC().Format(time.RFC3339Nano)}
	for _, tc := range []struct {
		role     string
		sameUser bool
	}{{"gui", true}, {"tray", true}, {"supervise", false}, {"supervise", true}} {
		processLookupIdentityFn = func(pid int) (process.ProcessIdentity, error) {
			return process.ProcessIdentity{PID: pid, Basename: "mcphub.exe", ExecutablePath: `C:\Hub\mcphub.exe`, CommandLine: `"C:\Hub\mcphub.exe" ` + tc.role, CreationDateUnix: 1700000000}, nil
		}
		reapOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return tc.sameUser, nil }
		if tc.role != "supervise" || !tc.sameUser {
			// A zero original view can grant no native identity. The canonical
			// role/SID refusal must happen before that branch is even entered.
			d := &v5UpgradeDeps{exePath: `C:\Hub\mcphub.exe`, readSupervisorOwner: func(string) (api.SupervisorLockOwner, error) { return owner, nil }, originalRuntime: &binaryadmission.WindowsCanonicalEntryPriorRuntimeV1{}, originalContext: context.Background()}
			err := d.forceKillSupervisorForOwner(owner)
			if err == nil || !strings.Contains(err.Error(), "refusing PID") || legacyPIDKills != 0 {
				t.Fatalf("role=%s sameUser=%t reached original/PID effect: %v", tc.role, tc.sameUser, err)
			}
			continue
		}
		accepted, err := supervisorPIDIsLiveMcphubSupervisor(owner.PID, owner.StartedAt, `C:\Hub`)
		if err != nil || !accepted {
			t.Fatalf("genuine owned supervise rejected: %v", err)
		}
		held := &authorizationHeldSupervisor{pid: owner.PID}
		holds, proofs := 0, 0
		err = terminateOriginalUpgradeSupervisor(context.Background(), owner, `C:\Hub\mcphub.exe`, func(pid int) (process.HeldPIDGeneration, error) {
			holds++
			if pid != owner.PID {
				t.Fatal("wrong PID used to acquire the one original owner")
			}
			return held, nil
		}, func(_ context.Context, original process.HeldPIDGeneration) error {
			proofs++
			if original != held {
				t.Fatal("native proof/effect subjects differ")
			}
			return nil
		})
		if err != nil || holds != 1 || proofs != 1 || held.terminated != 1 || held.closed != 1 || legacyPIDKills != 0 {
			t.Fatalf("held-only effect contract changed: hold=%d proof=%d terminate=%d close=%d err=%v", holds, proofs, held.terminated, held.closed, err)
		}
	}
}

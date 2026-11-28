package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestHubRouteQualificationNative(t *testing.T) {
	t.Run("codec", func(t *testing.T) {
		item := HubRouteQualification{TransactionID: strings.Repeat("a", 32), SuccessorSHA256: strings.Repeat("b", 64), ScopeSHA256: strings.Repeat("0", 64), EndpointAuthoritySHA256: strings.Repeat("1", 64), EndpointProvenanceSHA256: strings.Repeat("2", 64)}
		second := item
		second.TransactionID = strings.Repeat("b", 32)
		valid, err := json.Marshal(hubRouteQualificationRecord{Schema: hubRouteQualificationSchema, Qualified: []HubRouteQualification{item, second}})
		if err != nil || len(valid) != hubRouteQualificationLimit {
			t.Fatalf("canonical limit mismatch: bytes=%d err=%v", len(valid), err)
		}
		if _, err := decodeHubRouteQualification(valid); err != nil {
			t.Fatal(err)
		}
		invalid := []string{
			strings.Replace(string(valid), `"schema":`, `"schema":null,"schema":`, 1),
			strings.Replace(string(valid), `"transaction_id":`, `"transaction_id":null,"transaction_id":`, 1),
			strings.Replace(string(valid), `"scope_sha256":`, `"unrecognised":`, 1),
			strings.Replace(string(valid), `"transaction_id":"`+strings.Repeat("b", 32)+`"`, `"transaction_id":"`+strings.Repeat("a", 32)+`"`, 1),
			strings.Replace(string(valid), strings.Repeat("a", 32), strings.Repeat("A", 32), 1),
			`{"schema":"hub-mcp-route-qualification-v1","qualified":null}`,
			`{"schema":"hub-mcp-route-qualification-v1","qualified":[]}`,
			string(valid) + `{}`,
		}
		for i, data := range invalid {
			if _, err := decodeHubRouteQualification([]byte(data)); err == nil {
				t.Errorf("accepted invalid shape %d", i)
			}
		}
	})
	t.Run("restored_prior_and_reconcile_witness", func(t *testing.T) {
		f := newHubRouteFixture(t)
		f.with(t, func(s *HubRouteState) error {
			ep, err := s.ReadEndpoint()
			if err != nil {
				return err
			}
			prior := testHubRouteQualification(ep, "a")
			inactive := testHubRouteQualification(ep, "b")
			next := testHubRouteQualification(ep, "c")
			if err := s.StageQualification("", ep, prior); err != nil {
				return err
			}
			if err := s.StageQualification(prior.TransactionID, ep, inactive); err != nil {
				return err
			}
			if err := s.StageQualification(prior.TransactionID, ep, next); err != nil {
				return err
			}
			for _, want := range []HubRouteQualification{prior, next} {
				row, found, err := s.ReadQualification(want.TransactionID)
				if err != nil || !found || row != want {
					return fmt.Errorf("restored slot result %t: %w", found, err)
				}
			}
			if _, found, err := s.ReadQualification(inactive.TransactionID); err != nil || found {
				return fmt.Errorf("abandoned ID present=%t: %w", found, err)
			}
			if err := publishFixtureEndpoint(s, ep, next); err != nil {
				return err
			}
			qualified, err := s.ReadEndpoint()
			if err != nil {
				return err
			}
			if qualified.Witness() != next.TransactionID || qualified.AuthoritySHA256() != ep.AuthoritySHA256() || qualified.ProvenanceSHA256() == ep.ProvenanceSHA256() || !qualified.Endpoint().ReconcilePending {
				return errors.New("qualified endpoint did not preserve authority and add the selected witness")
			}
			return nil
		})
		before := f.bytes(t, hubMcpEndpointFileLeaf)
		lk, err := AcquireHubMcpLock()
		if err != nil {
			t.Fatal(err)
		}
		legacyErr := ClearHubReconcilePendingLocked()
		unlockErr := lk.Unlock()
		if !errors.Is(legacyErr, errHubRouteQualifiedAck) || unlockErr != nil || !bytes.Equal(before, f.bytes(t, hubMcpEndpointFileLeaf)) {
			t.Fatalf("legacy ACK changed qualified endpoint: %v %v", legacyErr, unlockErr)
		}
		var old HubRouteEndpointSelection
		f.with(t, func(s *HubRouteState) error {
			current, err := s.ReadEndpoint()
			if err != nil {
				return err
			}
			old = current
			if err := ClearHubReconcilePendingLocked(current); err != nil {
				return err
			}
			changed, err := s.ReadEndpoint()
			if err != nil {
				return err
			}
			if changed.Endpoint().ReconcilePending || changed.Witness() != current.Witness() || changed.ProvenanceSHA256() != current.ProvenanceSHA256() || changed.leaf.identity == current.leaf.identity {
				return errors.New("ACK did not confirm unique replacement preserving the witness and provenance")
			}
			if err := ClearHubReconcilePendingLocked(current); !errors.Is(err, errHubRouteStateChanged) {
				return fmt.Errorf("old endpoint accepted for ACK: %v", err)
			}
			return ClearHubReconcilePendingLocked(changed)
		})
		if err := ClearHubReconcilePendingLocked(old); !errors.Is(err, errHubRouteStateUnavailable) {
			t.Fatalf("released ACK scope accepted: %v", err)
		}
	})
	t.Run("legacy_ack", func(t *testing.T) {
		f := newHubRouteFixture(t)
		lk, err := AcquireHubMcpLock()
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(ClearHubReconcilePendingLocked(), lk.Unlock()); err != nil {
			t.Fatal(err)
		}
		raw := f.bytes(t, hubMcpEndpointFileLeaf)
		ep, err := decodeHubRouteEndpoint(raw)
		if err != nil || ep.ReconcilePending || ep.TransactionID != "" || bytes.Contains(raw, []byte(hubRouteWitnessKey)) {
			t.Fatalf("legacy bytes changed shape: %s %v", raw, err)
		}
	})
	t.Run("strict_parent_and_bound_names", func(t *testing.T) {
		for _, ace := range []struct {
			name string
			mask string
		}{{"read-only", "0x1"}, {"create-or-write", "0x2"}, {"delete-child", "0x40"}, {"write-dacl", "0x40000"}, {"take-owner", "0x80000"}} {
			t.Run(ace.name, func(t *testing.T) {
				f := newHubRouteFixture(t)
				canary := f.bytes(t, hubMcpEndpointFileLeaf)
				user, err := windows.GetCurrentProcessToken().GetTokenUser()
				if err != nil {
					t.Fatal(err)
				}
				sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + user.User.Sid.String() + ")(A;;" + ace.mask + ";;;WD)")
				if err != nil {
					t.Fatal(err)
				}
				acl, _, err := sd.DACL()
				if err != nil {
					t.Fatal(err)
				}
				if err := windows.SetNamedSecurityInfo(f.dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
					t.Fatal(err)
				}
				err = WithHubRouteState(t.Context(), f.dir, f.id, func(*HubRouteState) error { t.Fatal("unsafe parent reached callback"); return nil })
				if err == nil || !bytes.Equal(canary, f.bytes(t, hubMcpEndpointFileLeaf)) {
					t.Fatalf("strict parent refusal lost: %v", err)
				}
				if _, err := os.Stat(filepath.Join(f.dir, hubRouteQualificationLeaf)); !os.IsNotExist(err) {
					t.Fatalf("rejected parent gained a qualification: %v", err)
				}
			})
		}
		f := newHubRouteFixture(t)
		f.with(t, func(*HubRouteState) error {
			if err := os.Rename(f.dir, f.dir+"-new"); err == nil {
				return errors.New("native lease did not pin parent name")
			}
			if err := os.Rename(filepath.Dir(f.dir), filepath.Dir(f.dir)+"-new"); err == nil {
				return errors.New("native lease did not pin ancestor name")
			}
			return nil
		})
		foreign := hardenedTempDir(t)
		link := filepath.Join(t.TempDir(), "junction")
		if err := createJunctionForTest(link, foreign); err != nil {
			t.Fatal(err)
		}
		if _, err := ObserveHubRouteStateDirectory(t.Context(), link); err == nil {
			t.Fatal("accepted a directory junction")
		}
		if err := WithHubRouteState(t.Context(), foreign, f.id, func(*HubRouteState) error { t.Fatal("different physical parent reached callback"); return nil }); !errors.Is(err, errHubRouteStateChanged) {
			t.Fatalf("physical parent refusal: %v", err)
		}
	})
	t.Run("no_replace_collision", func(t *testing.T) {
		f := newHubRouteFixture(t)
		canary := []byte("fixture foreign qualification")
		f.with(t, func(s *HubRouteState) error {
			ep, err := s.ReadEndpoint()
			if err != nil {
				return err
			}
			s.core.native.beforeRename = func() error { return os.WriteFile(filepath.Join(f.dir, hubRouteQualificationLeaf), canary, 0o600) }
			result := s.StageQualification("", ep, testHubRouteQualification(ep, "a"))
			if !errors.Is(result, windows.STATUS_OBJECT_NAME_COLLISION) && !errors.Is(result, windows.ERROR_ALREADY_EXISTS) {
				return fmt.Errorf("native collision was hidden: %w", result)
			}
			if !bytes.Equal(canary, f.bytes(t, hubRouteQualificationLeaf)) {
				return errors.New("collision changed foreign bytes")
			}
			s.core.native.beforeRename = nil
			if err := s.StageQualification("", ep, testHubRouteQualification(ep, "b")); err == nil {
				return errors.New("invalid existing qualification was replaced")
			}
			entries, err := os.ReadDir(f.dir)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "."+hubRouteQualificationLeaf+".tmp.") {
					return errors.New("unpublished staging file remained")
				}
			}
			return nil
		})
	})
	t.Run("publication_outcomes", func(t *testing.T) {
		for _, outcome := range []HubRoutePublicationOutcome{HubRoutePublicationPreimageConfirmed, HubRoutePublicationApplied, HubRoutePublicationUnknown} {
			t.Run(string(outcome), func(t *testing.T) {
				f := newHubRouteFixture(t)
				f.with(t, func(s *HubRouteState) error {
					ep, err := s.ReadEndpoint()
					if err != nil {
						return err
					}
					qual := testHubRouteQualification(ep, "a")
					if err := s.StageQualification("", ep, qual); err != nil {
						return err
					}
					if err := publishFixtureEndpoint(s, ep, qual); err != nil {
						return err
					}
					ep, err = s.ReadEndpoint()
					if err != nil {
						return err
					}
					var blocked windows.Handle
					fault := errors.New("fixture native boundary fault")
					switch outcome {
					case HubRoutePublicationPreimageConfirmed:
						s.core.native.beforeRename = func() error { return fault }
					case HubRoutePublicationApplied:
						s.core.native.afterReadback = func() error { fault = windows.CloseHandle(0); return fault }
					case HubRoutePublicationUnknown:
						s.core.native.afterRename = func() error {
							var err error
							blocked, err = ntOpenRelativeWithShareAccess(s.core.native.parent, hubMcpEndpointFileLeaf, windows.DELETE, windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
							return err
						}
					}
					ack := ClearHubReconcilePendingLocked(ep)
					if blocked != 0 && blocked != windows.InvalidHandle {
						if err := windows.CloseHandle(blocked); err != nil {
							return err
						}
					}
					var result *HubRoutePublicationError
					if !errors.As(ack, &result) || result.Outcome != outcome {
						return fmt.Errorf("wanted publication outcome %s: %w", outcome, ack)
					}
					if outcome == HubRoutePublicationUnknown {
						fault = windows.STATUS_SHARING_VIOLATION
					}
					if !errors.Is(ack, fault) {
						return fmt.Errorf("original publication error missing: %w", ack)
					}
					persisted, err := s.ReadEndpoint()
					if err != nil {
						return err
					}
					wantPreimage := outcome == HubRoutePublicationPreimageConfirmed
					if persisted.Endpoint().ReconcilePending != wantPreimage || persisted.Witness() != ep.Witness() || persisted.ProvenanceSHA256() != ep.ProvenanceSHA256() || (persisted.leaf.identity == ep.leaf.identity) != wantPreimage {
						return fmt.Errorf("%s did not preserve the actual preimage or postimage", outcome)
					}
					return nil
				})
			})
		}
	})
	t.Run("same_legacy_lock", func(t *testing.T) {
		f := newHubRouteFixture(t)
		lk, err := AcquireHubMcpLock()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		err = WithHubRouteState(ctx, f.dir, f.id, func(*HubRouteState) error { t.Fatal("native scope bypassed the existing held flock"); return nil })
		unlockErr := lk.Unlock()
		if !errors.Is(err, context.DeadlineExceeded) || unlockErr != nil {
			t.Fatalf("same legacy lock not observed: %v %v", err, unlockErr)
		}
		f.with(t, func(s *HubRouteState) error { _, err := s.ReadEndpoint(); return err })
	})
}

type hubRouteFixture struct {
	dir string
	id  ConfigParentDirectoryIdentityV1
}

func newHubRouteFixture(t *testing.T) hubRouteFixture {
	t.Helper()
	dir := isolateStateDir(t)
	if _, err := EnsureHubEndpoint(48125, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateHubInstanceID(); err != nil {
		t.Fatal(err)
	}
	identity, err := ObserveHubRouteStateDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return hubRouteFixture{dir: dir, id: identity}
}
func (f hubRouteFixture) with(t *testing.T, run func(*HubRouteState) error) {
	t.Helper()
	if err := WithHubRouteState(t.Context(), f.dir, f.id, run); err != nil {
		t.Fatal(err)
	}
}
func (f hubRouteFixture) bytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func testHubRouteQualification(ep HubRouteEndpointSelection, digit string) HubRouteQualification {
	return HubRouteQualification{TransactionID: strings.Repeat(digit, 32), SuccessorSHA256: strings.Repeat("1", 64), ScopeSHA256: strings.Repeat("2", 64), EndpointAuthoritySHA256: ep.AuthoritySHA256(), EndpointProvenanceSHA256: ep.ProvenanceSHA256()}
}

func TestHubRouteQualificationLockACL(t *testing.T) {
	f := newHubRouteFixture(t)
	f.with(t, func(*HubRouteState) error { return nil }) // Actual default legacy marker.
	actor, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	groups, err := windows.GetCurrentProcessToken().GetTokenGroups()
	if err != nil {
		t.Fatal(err)
	}
	var logon *windows.SID
	for _, g := range groups.AllGroups() {
		if g.Attributes&windows.SE_GROUP_LOGON_ID == windows.SE_GROUP_LOGON_ID {
			logon = g.Sid
			break
		}
	}
	if logon == nil || !logon.IsValid() {
		t.Fatal("normal fixture token has no reported logon SID")
	}
	const unrelated = "S-1-5-21-111111111-222222222-333333333-4444"
	for _, test := range []struct {
		name, sid, mask string
		admit           bool
	}{
		{"current_logon_read_execute", logon.String(), "0x29", true},
		{"unrelated_read_execute", unrelated, "0x29", false},
		{"unrelated_read_attributes", unrelated, "0x80", false},
		{"current_logon_write", logon.String(), "0x02", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			aclText := "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + actor.User.Sid.String() + ")(A;;" + test.mask + ";;;" + test.sid + ")"
			path := filepath.Join(f.dir, hubMcpLockFileLeaf)
			sd, err := windows.SecurityDescriptorFromString(aclText)
			if err != nil {
				t.Fatal(err)
			}
			acl, _, err := sd.DACL()
			if err != nil {
				t.Fatal(err)
			}
			if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
				t.Fatal(err)
			}
			original, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			originalACL, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal(err)
			}
			entered := false
			err = WithHubRouteState(t.Context(), f.dir, f.id, func(*HubRouteState) error { entered = true; return nil })
			if test.admit != (err == nil) || test.admit != entered {
				t.Fatalf("marker ACL admitted=%t, want %t: %v", entered, test.admit, err)
			}
			current, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(original, current) || current.Size() != 0 || descriptor.String() != originalACL.String() {
				t.Fatal("marker check altered original object or ACL")
			}
		})
	}
}

func TestHubRouteReconcileScopeFailure(t *testing.T) {
	for _, test := range []struct {
		name            string
		callback, close bool
	}{{"callback", true, false}, {"lease_close", false, true}, {"callback_and_lease_close", true, true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newHubRouteFixture(t)
			later := errors.New("fixture downstream callback after confirmed ACK")
			var confirmed HubRouteEndpointSelection
			err := WithHubRouteState(t.Context(), f.dir, f.id, func(s *HubRouteState) error {
				ep, err := s.ReadEndpoint()
				if err != nil {
					return err
				}
				q := testHubRouteQualification(ep, "a")
				if err := s.StageQualification("", ep, q); err != nil {
					return err
				}
				if err := publishFixtureEndpoint(s, ep, q); err != nil {
					return err
				}
				ep, err = s.ReadEndpoint()
				if err != nil {
					return err
				}
				if !ep.Endpoint().ReconcilePending || ep.Witness() != q.TransactionID {
					return errors.New("fixture witness or pending state missing")
				}
				if err := ClearHubReconcilePendingLocked(ep); err != nil {
					return err
				}
				confirmed, err = s.ReadEndpoint()
				if err != nil {
					return err
				}
				if confirmed.Endpoint().ReconcilePending || confirmed.Witness() != q.TransactionID || confirmed.leaf.identity == ep.leaf.identity {
					return errors.New("native ACK not confirmed")
				}
				if test.close {
					s.core.native.guards = append(s.core.native.guards, 0)
				} // Native CloseHandle(0) fails during outer lease cleanup.
				if test.callback {
					return later
				}
				return nil
			})
			if confirmed.owner == nil {
				t.Fatalf("fixture did not reach a confirmed ACK: %v", err)
			}
			var result *HubRoutePublicationError
			if !errors.As(err, &result) || result.Outcome != HubRoutePublicationApplied || errors.Is(err, later) != test.callback || errors.Is(err, windows.ERROR_INVALID_HANDLE) != test.close {
				t.Errorf("outer scope lost confirmed ACK or later cause: %v", err)
			}
			f.with(t, func(s *HubRouteState) error {
				persisted, err := s.ReadEndpoint()
				if err != nil {
					return err
				}
				if persisted.Endpoint().ReconcilePending || persisted.Witness() != confirmed.Witness() || !persisted.leaf.matches(confirmed.leaf) {
					return errors.New("scope unwind rewrote or lost the native ACK")
				}
				return nil
			})
		})
	}
}

// The existing H codec/ACK fixtures need a qualified preimage without starting a socket.
func publishFixtureEndpoint(s *HubRouteState, selected HubRouteEndpointSelection, row HubRouteQualification) error {
	return s.use(func(c *hubRouteStateCore) error {
		if err := c.matchQualification(selected, row); err != nil {
			return err
		}
		_, err := c.publishEndpoint(selected, row.TransactionID)
		return err
	})
}

func TestHubRouteEndpointOriginalSHA256(t *testing.T) {
	f := newHubRouteFixture(t)
	read := func(t *testing.T) (ep HubRouteEndpointSelection) {
		t.Helper()
		f.with(t, func(h *HubRouteState) error {
			var err error
			ep, err = h.ReadEndpoint()
			return err
		})
		return ep
	}
	original := read(t)
	raw := f.bytes(t, hubMcpEndpointFileLeaf)
	if original.OriginalSHA256() != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("selection did not retain the digest of the original file bytes")
	}
	toggled := original.endpoint
	toggled.ReconcilePending = !toggled.ReconcilePending
	body, err := json.Marshal(toggled)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		body []byte
		want hubRouteEndpoint
	}{
		{"formatting", append(bytes.Clone(raw), '\n'), original.endpoint},
		{"reconcile_pending", body, toggled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Truncate and write the existing leaf, retaining its native file ID.
			if err := os.WriteFile(filepath.Join(f.dir, hubMcpEndpointFileLeaf), tc.body, 0600); err != nil {
				t.Fatal(err)
			}
			changed := read(t)
			if changed.OriginalIdentity() != original.OriginalIdentity() || changed.endpoint != tc.want ||
				changed.AuthoritySHA256() != original.AuthoritySHA256() || changed.ProvenanceSHA256() != original.ProvenanceSHA256() {
				t.Fatal("rewrite did not preserve the native file and semantic projections")
			}
			if changed.OriginalSHA256() == original.OriginalSHA256() || changed.OriginalSHA256() != fmt.Sprintf("%x", sha256.Sum256(tc.body)) {
				t.Fatal("original endpoint digest failed to distinguish the rewritten bytes")
			}
		})
	}
}

func TestHubRouteServingOriginalReloadObservation(t *testing.T) {
	f := newHubRouteFixture(t)
	path := filepath.Join(f.dir, hubMcpControlTokenFileLeaf)
	var selected HubRouteReloadSelection
	f.with(t, func(h *HubRouteState) error {
		if _, err := h.ObserveReloadToken(); err == nil {
			t.Fatal("absent reload object accepted as a serving original")
		}
		return h.CheckReloadTokenAbsent()
	})
	payload := []byte(strings.Repeat("c", 64) + "\n")
	if created, err := SecureCreateOwnerOnlyFile(path, payload); err != nil || !created {
		t.Fatal("create protected reload original", err)
	}
	f.with(t, func(h *HubRouteState) error {
		var err error
		selected, err = h.ObserveReloadToken()
		if err != nil {
			return err
		}
		again, err := h.ObserveReloadToken()
		if err != nil || !selected.Matches(again) || h.CheckReloadTokenAbsent() == nil {
			t.Fatalf("same occupied reload object lost: %v", err)
		}
		return nil
	})
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if created, err := SecureCreateOwnerOnlyFile(path, payload); err != nil || !created {
		t.Fatal("create protected reload replacement", err)
	}
	f.with(t, func(h *HubRouteState) error {
		changed, err := h.ObserveReloadToken()
		if err != nil {
			return err
		}
		if selected.Matches(changed) {
			t.Fatal("same bytes on a substituted native object accepted")
		}
		return nil
	})
}

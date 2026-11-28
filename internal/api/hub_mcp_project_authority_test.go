package api

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectAuthorityIsNeverPortable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	dir, err := dataDirForBundle()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gui-preferences.yaml")
	portable := []byte("# keep ordinary bytes\nappearance.theme: light\nunknown.example: retained\n")
	readExport := func(input []byte) []byte {
		t.Helper()
		seedSettingsFile(t, path, input)
		var out bytes.Buffer
		if err := WriteConfigBundle(&out); err != nil {
			t.Fatal(err)
		}
		archive, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
		if err != nil {
			t.Fatal(err)
		}
		entry, err := archive.Open("gui-preferences.yaml")
		if err != nil {
			t.Fatal(err)
		}
		exported, err := io.ReadAll(entry)
		if err := errors.Join(err, entry.Close()); err != nil {
			t.Fatal(err)
		}
		return exported
	}
	if !bytes.Equal(readExport(portable), portable) {
		t.Fatal("ordinary settings bytes changed during export")
	}
	protected := append(append([]byte{}, portable...), []byte(hubProjectAuthoritySettingsKey+": synthetic-private-value\n")...)
	exported := readExport(protected)
	if bytes.Contains(exported, []byte(hubProjectAuthoritySettingsKey)) || bytes.Contains(exported, []byte("synthetic-private-value")) {
		t.Fatal("project authority appeared in portable settings")
	}
	if !bytes.Contains(exported, []byte("appearance.theme: light")) || !bytes.Contains(exported, []byte("unknown.example: retained")) {
		t.Fatal("authority omission removed ordinary settings")
	}
	seedSettingsFile(t, path, append(bytes.Clone(protected), "---\nappearance.theme: [\n"...))
	var rejected bytes.Buffer
	if err := WriteConfigBundle(&rejected); !errors.Is(err, errInvalidSettingsDocument) {
		t.Fatalf("invalid settings export error = %v", err)
	}
	archive, err := zip.NewReader(bytes.NewReader(rejected.Bytes()), int64(rejected.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range archive.File {
		if entry.Name == "gui-preferences.yaml" {
			t.Fatal("rejected export included the invalid settings document")
		}
	}
}

func TestHubProjectTokenRequiresCurrentCommittedEnrollment(t *testing.T) {
	root := hubMcpStateTestHelper(t)
	t.Setenv("LOCALAPPDATA", root)
	prior := liveTokenTable.Load()
	t.Cleanup(func() { liveTokenTable.Store(prior) })
	liveTokenTable.Store(nil)
	ctx := context.Background()
	epoch := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 7, Port: 9555}
	seedSettingsFile(t, SettingsPath(), []byte("mcp_front.routing_target: front\nmcp_front.routing_generation: '7'\nmcp_front.routing_admitted_port: '9555'\n"))
	group := "Qualified"
	scope := GroupScopeKey(group)
	ordinary := GroupScopeKey("ordinary")
	if err := WriteGroups(GroupsConfig{Groups: []Group{{Name: group}, {Name: "ordinary"}}}); err != nil {
		t.Fatal(err)
	}
	initial, err := EnsureGroupTokens([]string{scope, ordinary})
	if err != nil {
		t.Fatal(err)
	}
	legacy := initial.Tokens[scope]
	control := initial.Tokens[ordinary]
	if ConstantTimeCompareToken(scope, legacy) != 1 || ConstantTimeCompareToken(ordinary, control) != 1 {
		t.Fatal("ordinary credential fixture is not active")
	}
	projectA := strings.Repeat("a", 64)
	projectB := strings.Repeat("b", 64)
	stage := func(project, previous string) (hubProjectEnrollment, string, hubProjectRebindReceiving) {
		t.Helper()
		e, token, err := stageHubProjectRebind(ctx, hubProjectRebind{
			Group: group, Project: project, PriorRevision: previous,
			AttachmentPlan: strings.Repeat("c", 64), Epoch: epoch,
		})
		if err != nil {
			t.Fatal(err)
		}
		return e, token, hubProjectRebindReceiving{Project: project, Revision: e.Revision,
			AttachmentPlan: e.AttachmentPlan, AttachmentReceipt: strings.Repeat("d", 64), Epoch: epoch}
	}
	check := func(token string, receiving hubProjectRebindReceiving, want bool) {
		t.Helper()
		if _, active := currentHubProjectEnrollment(scope, token, receiving); active != want {
			t.Fatalf("current project credential admitted = %t, want %t", active, want)
		}
	}
	commit := func(staged hubProjectEnrollment, receiving hubProjectRebindReceiving) {
		t.Helper()
		if err := commitHubProjectRebind(ctx, staged, receiving); err != nil {
			t.Fatal(err)
		}
	}

	a, tokenA, receivingA := stage(projectA, "")
	check(legacy, receivingA, false)
	check(tokenA, receivingA, false)
	if _, visible := CurrentTokenTable().Tokens[scope]; visible || ConstantTimeCompareToken(scope, legacy) != 0 {
		t.Fatal("quarantine exposed the preexisting group token")
	}
	wrong := receivingA
	wrong.Project = projectB
	if err := commitHubProjectRebind(ctx, a, wrong); !errors.Is(err, errHubProjectAuthority) {
		t.Fatalf("mismatched attachment receipt result = %v", err)
	}
	commit(a, receivingA)
	check(tokenA, receivingA, true)
	check(legacy, receivingA, false)
	oldTable, err := readHubMcpStateFile(hubMcpTokensFileLeaf)
	if err != nil {
		t.Fatal(err)
	}

	b, tokenB, receivingB := stage(projectB, a.Revision)
	check(tokenA, receivingA, false)
	check(tokenB, receivingB, false)
	newTable, err := readHubMcpStateFile(hubMcpTokensFileLeaf)
	if err != nil {
		t.Fatal(err)
	}
	liveTokenTable.Store(nil)
	public, err := EnsureGroupTokens([]string{scope, ordinary})
	if err != nil {
		t.Fatal(err)
	}
	if _, visible := public.Tokens[scope]; visible || public.Tokens[ordinary] != control {
		t.Fatal("generic cold ensure exposed the pending scope or changed the ordinary token")
	}
	check(tokenB, receivingB, false)
	if _, err := RotateHubToken(scope); !errors.Is(err, errHubProjectAuthority) {
		t.Fatalf("ordinary group-token rotation result for a guarded scope = %v", err)
	}
	if err := WriteGroups(GroupsConfig{Groups: []Group{{Name: "ordinary"}}}); !errors.Is(err, errHubProjectAuthority) {
		t.Fatalf("ordinary group removal result for a guarded scope = %v", err)
	}

	commit(b, receivingB)
	check(tokenB, receivingB, true)
	check(tokenA, receivingB, false)
	if err := writeHubMcpStateFile(hubMcpTokensFileLeaf, oldTable); err != nil {
		t.Fatal(err)
	}
	if _, err := ReloadHubTokens(); err != nil {
		t.Fatal(err)
	}
	check(tokenA, receivingA, false)
	check(tokenA, receivingB, false)
	check(tokenB, receivingB, false)
	if err := writeHubMcpStateFile(hubMcpTokensFileLeaf, newTable); err != nil {
		t.Fatal(err)
	}
	if _, err := ReloadHubTokens(); err != nil {
		t.Fatal(err)
	}
	rollback, tokenRollback, receivingRollback := stage(projectA, b.Revision)
	commit(rollback, receivingRollback)
	check(tokenRollback, receivingRollback, true)
	check(tokenA, receivingRollback, false)
	check(tokenB, receivingRollback, false)
	if tokenA == legacy || tokenB == tokenA || tokenRollback == tokenA || tokenRollback == tokenB ||
		ConstantTimeCompareToken(ordinary, control) != 1 {
		t.Fatal("rotation reused a retired token or changed an unrelated ordinary group")
	}

	path := SettingsPath()
	committedSettings, err := readStateFileInodeAnchored(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		suffix string
	}{
		{name: "second document", suffix: "---\nappearance.theme: light\n"},
		{name: "malformed trailing document", suffix: "---\nappearance.theme: [\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := append(bytes.Clone(committedSettings), tc.suffix...)
			seedSettingsFile(t, path, invalid)
			_, reloadErr := ReloadHubTokens()
			if _, active := currentHubProjectEnrollment(scope, tokenRollback, receivingRollback); active {
				t.Fatal("invalid settings stream admitted the previously committed credential")
			}
			if !errors.Is(reloadErr, errHubProjectAuthority) {
				t.Fatalf("invalid settings reload error = %v", reloadErr)
			}
			if ConstantTimeCompareToken(ordinary, control) != 1 || ConstantTimeCompareToken(scope, tokenRollback) != 0 {
				t.Fatal("failed reload changed the verified unrelated token or revealed the guarded token")
			}
			_, _, err := stageHubProjectRebind(ctx, hubProjectRebind{Group: group, Project: projectB,
				PriorRevision: rollback.Revision, AttachmentPlan: rollback.AttachmentPlan, Epoch: epoch})
			if !errors.Is(err, errHubProjectAuthority) {
				t.Fatalf("invalid settings mutation error = %v", err)
			}
			current, err := readStateFileInodeAnchored(path)
			if err != nil || !bytes.Equal(current, invalid) {
				t.Fatal("protected mutation rewrote invalid settings")
			}
			liveTokenTable.Store(nil)
			if _, err := ReloadHubTokens(); !errors.Is(err, errHubProjectAuthority) {
				t.Fatalf("cold invalid settings reload error = %v", err)
			}
			if ConstantTimeCompareToken(ordinary, control) != 0 || ConstantTimeCompareToken(scope, tokenRollback) != 0 {
				t.Fatal("cold reload with invalid settings established group authority")
			}
			seedSettingsFile(t, path, committedSettings)
			if _, err := ReloadHubTokens(); err != nil {
				t.Fatal(err)
			}
			if _, active := currentHubProjectEnrollment(scope, tokenRollback, receivingRollback); !active {
				t.Fatal("exact valid settings did not restore the independently received enrollment")
			}
		})
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReloadHubTokens(); err != nil {
		t.Fatal(err)
	}
	check(tokenRollback, receivingRollback, false)
	if ConstantTimeCompareToken(ordinary, control) != 1 || ConstantTimeCompareToken(scope, tokenRollback) != 0 {
		t.Fatal("missing settings exposed a known guarded scope or changed the unrelated group token")
	}
}

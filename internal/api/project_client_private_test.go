package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

func TestProjectPrivateIssuanceRejectsReplayMembershipDriftAndOldRollback(t *testing.T) {
	root := hubMcpStateTestHelper(t)
	t.Setenv("LOCALAPPDATA", root)
	prior := liveTokenTable.Load()
	t.Cleanup(func() { liveTokenTable.Store(prior) })
	liveTokenTable.Store(nil)
	seedSettingsFile(t, SettingsPath(), []byte("mcp_front.routing_target: front\nmcp_front.routing_generation: '7'\nmcp_front.routing_admitted_port: '9555'\n"))
	group := Group{Name: "Reasoning", Servers: []string{"private-fixture"}}
	if err := WriteGroups(GroupsConfig{Groups: []Group{group}}); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGroupTokens([]string{GroupScopeKey(group.Name)}); err != nil {
		t.Fatal(err)
	}
	epoch := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 7, Port: 9555}
	groupHash, err := projectClientValueHash(group)
	if err != nil {
		t.Fatal(err)
	}
	input := hubProjectRebind{Group: group.Name, Project: strings.Repeat("a", 64), AttachmentPlan: strings.Repeat("b", 64), Epoch: epoch}
	issued := hubProjectIssuance{Revision: strings.Repeat("c", 64), Token: strings.Repeat("d", 64), ProjectRoot: t.TempDir(), CapsuleLeaf: "config.toml.bak-mcp-local-hub-20261125-120000.project-client-private-v3.json", GroupSHA256: groupHash}
	ctx := context.Background()
	a, tokenA, err := stageHubProjectRebind(ctx, input, issued)
	if err != nil || tokenA != issued.Token || a.Revision != issued.Revision {
		t.Fatalf("transaction issuance lost identity: %+v %v", a, err)
	}
	if _, _, err := stageHubProjectRebind(ctx, input, issued); !errors.Is(err, errHubProjectAuthority) {
		t.Fatalf("replay minted new authority: %v", err)
	}
	receivingA := hubProjectRebindReceiving{Project: a.Project, Revision: a.Revision, AttachmentPlan: a.AttachmentPlan, AttachmentReceipt: strings.Repeat("e", 64), Epoch: epoch}
	if _, ok := currentHubProjectEnrollment(GroupScopeKey(group.Name), tokenA, receivingA); ok {
		t.Fatal("pending issuance was usable")
	}
	if err := commitHubProjectRebind(ctx, a, receivingA); err != nil {
		t.Fatal(err)
	}
	// This lower-boundary test supplies a receiving fixture. The production
	// front additionally requires its physical project capsule and backend.
	input.Project = strings.Repeat("f", 64)
	input.PriorRevision = a.Revision
	issued.Revision = strings.Repeat("1", 64)
	issued.Token = strings.Repeat("2", 64)
	issued.GroupSHA256 = strings.Repeat("3", 64)
	if _, _, err := stageHubProjectRebind(ctx, input, issued); !errors.Is(err, errHubProjectAuthority) {
		t.Fatalf("changed membership accepted: %v", err)
	}
	if _, ok := currentHubProjectEnrollment(GroupScopeKey(group.Name), tokenA, receivingA); !ok {
		t.Fatal("preflight refusal revoked old owner")
	}
	issued.GroupSHA256 = groupHash
	b, tokenB, err := stageHubProjectRebind(ctx, input, issued)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := currentHubProjectEnrollment(GroupScopeKey(group.Name), tokenA, receivingA); ok {
		t.Fatal("old project survived rebind quarantine")
	}
	if err := revokeHubProjectEnrollment(ctx, a, epoch); !errors.Is(err, errHubProjectAuthority) {
		t.Fatalf("old rollback touched successor: %v", err)
	}
	receivingB := hubProjectRebindReceiving{Project: b.Project, Revision: b.Revision, AttachmentPlan: b.AttachmentPlan, AttachmentReceipt: strings.Repeat("4", 64), Epoch: epoch}
	if err := commitHubProjectRebind(ctx, b, receivingB); err != nil {
		t.Fatal(err)
	}
	if _, ok := currentHubProjectEnrollment(GroupScopeKey(group.Name), tokenB, receivingB); !ok {
		t.Fatal("old rollback damaged successor credential")
	}
	if err := revokeHubProjectEnrollment(ctx, b, epoch); err != nil {
		t.Fatal(err)
	}
	if _, ok := currentHubProjectEnrollment(GroupScopeKey(group.Name), tokenB, receivingB); ok {
		t.Fatal("original rollback left its credential active")
	}
	if ConstantTimeCompareToken(GroupScopeKey(group.Name), tokenB) != 0 {
		t.Fatal("revocation exposed guarded credential to ordinary auth")
	}
}

type pendingProjectScopeTransport struct {
	sessionLocalAdmissionTransport
	original context.Context
	pending  bool
	checks   int
}

func (p *pendingProjectScopeTransport) AwaitRevokedSessions(_ context.Context, revoked func(context.Context) (bool, error)) error {
	selected, err := revoked(p.original)
	if err != nil {
		return err
	}
	if !selected || p.original.Err() == nil {
		return errors.New("original session was not revoked")
	}
	p.checks++
	if p.pending {
		return errors.New("original receipt pending")
	}
	return nil
}

func TestProjectScopeSettlementRetryKeepsOriginalReceiptAfterSIDDeletion(t *testing.T) {
	root := hubMcpStateTestHelper(t)
	t.Setenv("LOCALAPPDATA", root)
	prior := liveTokenTable.Load()
	t.Cleanup(func() { liveTokenTable.Store(prior) })
	liveTokenTable.Store(nil)
	seedSettingsFile(t, SettingsPath(), []byte("mcp_front.routing_target: front\nmcp_front.routing_generation: '7'\nmcp_front.routing_admitted_port: '9555'\n"))
	group := Group{Name: "Reasoning", Servers: []string{"private-fixture"}}
	if err := WriteGroups(GroupsConfig{Groups: []Group{group}}); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGroupTokens([]string{GroupScopeKey(group.Name)}); err != nil {
		t.Fatal(err)
	}
	old := liveTokenTable.Load().tokens.Tokens[GroupScopeKey(group.Name)]
	transport := &pendingProjectScopeTransport{pending: true}
	store := NewHubSessionStore(SessionStoreOpts{StdioSessions: transport})
	defer store.Close()
	session, err := store.Create(GroupScopeKey(group.Name), hubProtocolVersionFallback, nil)
	if err != nil {
		t.Fatal(err)
	}
	session.stdioCredential = &hubStdioCredential{digest: sha256.Sum256([]byte(old))}
	transport.original = session.stdioLifetime
	epoch := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 7, Port: 9555}
	e, _, err := stageHubProjectRebind(context.Background(), hubProjectRebind{Group: group.Name, Project: strings.Repeat("a", 64), AttachmentPlan: strings.Repeat("b", 64), Epoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHubMcpHandler(store)
	request := HubProjectScopeSettlementRequest{e.Group, e.Project, e.Revision, e.AttachmentPlan}
	for attempt := 0; attempt < 2; attempt++ {
		if err := h.ReceiveProjectScopeSettlement(context.Background(), request); err == nil {
			t.Fatal("pending receipt acknowledged")
		}
		if _, found := store.Get(session.ClientSessionID); found {
			t.Fatal("revoked public SID remained usable")
		}
	}
	if transport.checks != 2 {
		t.Fatal("retry lost original receipt after public SID deletion")
	}
	transport.pending = false
	if err := h.ReceiveProjectScopeSettlement(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Revision = strings.Repeat("f", 64)
	if err := h.ReceiveProjectScopeSettlement(context.Background(), request); err == nil {
		t.Fatal("caller revision substituted current authority")
	}
}

// This exercises the actual project config/paired-entry owner. The route
// selection is a planning fixture; it is not evidence of a native backend.
func TestProjectPrivatePlanIssuanceUsesPairedOwnerAndRejectsReplay(t *testing.T) {
	root := hubMcpStateTestHelper(t)
	t.Setenv("LOCALAPPDATA", root)
	seedSettingsFile(t, SettingsPath(), []byte("mcp_front.routing_target: front\nmcp_front.routing_generation: '4'\nmcp_front.routing_admitted_port: '9192'\n"))
	m, target := sessionLocalPlanFixture(t)
	m.ClientBindings = []config.ClientBinding{{Client: "codex-cli", Daemon: "default", URLPath: "/mcp"}}
	target.Tokens.Tokens["codex-cli"] = strings.Repeat("e", 64)
	if err := WriteGroups(GroupsConfig{Groups: []Group{{Name: "Reasoning", Servers: []string{m.Name}}}}); err != nil {
		t.Fatal(err)
	}
	projectRoot := hardenedTempDir(t)
	parent := filepath.Join(projectRoot, ".codex")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	before := []byte("# original project policy\nmodel = 'fixture'\n")
	if err := os.WriteFile(filepath.Join(parent, "config.toml"), before, 0600); err != nil {
		t.Fatal(err)
	}
	project, identity, err := clients.NewCodexCLIForProject(projectRoot, "reasoning", NewCodexProjectConfigMutationOwner)
	if err != nil {
		t.Fatal(err)
	}
	create := &InstallProjectClientCreate{Group: "Reasoning", ProjectRoot: projectRoot, LogicalEntryName: "reasoning", PolicySource: "native-defaults"}
	opts := InstallOpts{Server: m.Name, DaemonFilter: "default", ClientsInclude: []string{"codex-cli"}, DryRun: true, ProjectClientCreate: create}
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{DaemonFilter: "default", ClientsInclude: []string{"codex-cli"}, ProjectClientCreate: create, sessionLocal: target})
	if err != nil {
		t.Fatal(err)
	}
	release := managedRegistrationReleaseIdentity{Version: "fixture", Commit: strings.Repeat("1", 40)}
	p, err := prepareProjectClientAttachmentPlan(opts, strings.Repeat("d", 64), plan, project, identity, []byte{}, release)
	if err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(p.public)
	if err != nil || bytes.Contains(public, []byte(projectRoot)) || bytes.Contains(public, []byte(target.Tokens.Tokens["codex-cli"])) {
		t.Fatal("preview leaked private material")
	}
	if p.private.token != "" || p.public.Schema != projectPrivatePlanSchema {
		t.Fatal("preview issued a credential")
	}
	create.ApplyCreate, create.ExpectedPlanSHA256, opts.DryRun = true, p.public.PlanSHA256, false
	p, err = prepareProjectClientAttachmentPlan(opts, strings.Repeat("d", 64), plan, project, identity, []byte{}, release)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindProjectPrivateIssuance(p, plan, opts); err != nil {
		t.Fatal(err)
	}
	if !isLowerHex64(p.private.token) || plan.ClientUpdates[0].Headers["X-Mcphub-Hub-Token"] != p.private.token {
		t.Fatal("original transaction did not own actual credential")
	}
	owner := project.(clients.ProjectHTTPAttachmentClient)
	created, err := owner.CreateProjectHTTPAttachment(p.preparation, []byte{})
	if err != nil || !created.Committed {
		t.Fatalf("paired create: %v", err)
	}
	if err := owner.VerifyProjectHTTPAttachment(p.preparation); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareProjectClientAttachmentPlan(opts, strings.Repeat("d", 64), plan, project, identity, []byte{}, release); err == nil {
		t.Fatal("old plan reapplied over committed pair")
	}
	restored, err := owner.RestoreCreatedProjectHTTPAttachment(p.preparation, before)
	if err != nil || !restored.Committed {
		t.Fatalf("original pair restore: %v", err)
	}
	actual, err := os.ReadFile(filepath.Join(parent, "config.toml"))
	if err != nil || !bytes.Equal(actual, before) {
		t.Fatal("paired inverse changed unrelated policy")
	}
	if err := os.WriteFile(filepath.Join(parent, "config.toml"), append(before, []byte("# changed after preview\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := bindProjectPrivateIssuance(p, plan, opts); !errors.Is(err, clients.ErrProjectClientPlanStale) {
		t.Fatalf("beforeimage change issued authority: %v", err)
	}
}

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// A missing or accidentally mutating preview branch must not report success.
func TestAttachClientPreviewIsReadOnlyByDefault(t *testing.T) {
	var got api.AttachClientRequest
	builds := 0
	cmd := newAttachClientCmdWithPlanBuilder(func(_ context.Context, req api.AttachClientRequest) (*api.AttachClientPlan, error) {
		builds++
		got = req
		return &api.AttachClientPlan{
			Server: "fixture", Daemon: "default", Client: "mimocode", EntryKey: "fixture",
			Endpoint: "http://127.0.0.1:9300/mcp", BeforeTransport: "direct", AfterTransport: "http",
			TargetBytesSHA256: strings.Repeat("a", 64), EntryTokenSHA256: strings.Repeat("b", 64),
		}, nil
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"fixture", "--daemon", "default", "--client", "mimocode"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if builds != 1 || got != (api.AttachClientRequest{Server: "fixture", Daemon: "default", Client: "mimocode"}) {
		t.Fatalf("plan calls=%d selection=%+v", builds, got)
	}
	if text := out.String(); !strings.Contains(text, "state=would-attach") || !strings.Contains(text, "operation=attach") || !strings.Contains(text, `entry="fixture"`) {
		t.Fatalf("missing redacted dry-run summary: %q", text)
	}
}

func TestAttachClientMissingManifestPreviewKeepsSourcePrivate(t *testing.T) {
	const source = "private-source.yaml"
	var got api.AttachClientRequest
	cmd := newAttachClientCmdWithPlanBuilder(func(_ context.Context, req api.AttachClientRequest) (*api.AttachClientPlan, error) {
		got = req
		return &api.AttachClientPlan{State: "would-restore-and-attach", Server: "fixture", Daemon: "default", Client: "mimocode", EntryKey: "fixture",
			ManifestSHA256: strings.Repeat("c", 64), ManifestProvenance: "explicit-file",
			Endpoint: "http://127.0.0.1:9300/mcp", BeforeTransport: "direct", AfterTransport: "http",
			TargetBytesSHA256: strings.Repeat("a", 64), EntryTokenSHA256: strings.Repeat("b", 64)}, nil
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"fixture", "--daemon", "default", "--client", "mimocode", "--manifest-source", source})
	if err := cmd.Execute(); err != nil || got.ManifestSource != source || !strings.Contains(out.String(), "state=would-restore-and-attach") ||
		!strings.Contains(out.String(), "manifest_sha256="+strings.Repeat("c", 64)) || !strings.Contains(out.String(), "manifest_provenance=explicit-file") ||
		!strings.Contains(out.String(), "legacy_lease_namespace_may_migrate=true") || strings.Contains(out.String(), source) {
		t.Fatalf("source preview: request=%+v err=%v output=%q", got, err, out.String())
	}
}

func TestAttachClientPreviewRejectsUsageWithoutBuildingPlan(t *testing.T) {
	for _, args := range [][]string{
		{"fixture", "--daemon", "default"},
		{"fixture", "--client", "mimocode"},
		{"--daemon", "default", "--client", "mimocode"},
		{"fixture", "--daemon", "default", "--client", "mimocode", "--apply=false"},
		{"fixture", "--daemon", "default", "--client", "mimocode", "--rollback-receipt="},
		{"fixture", "--daemon", "default", "--client", "mimocode", "--rollback-receipt=latest"},
		{"fixture", "--daemon", "default", "--client", "mimocode", "--rollback-receipt=LATEST"},
		{"fixture", "--daemon", "default", "--client", "mimocode", "--rollback-receipt=all"},
		{"fixture", "--daemon", "default", "--client", "mimocode", "--yes"},
	} {
		calls := 0
		cmd := newAttachClientCmdWithPlanBuilder(func(context.Context, api.AttachClientRequest) (*api.AttachClientPlan, error) {
			calls++
			return nil, nil
		})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "ATTACH_USAGE stage=usage") || calls != 0 || out.Len() != 0 {
			t.Errorf("args=%q error=%v plan calls=%d output=%q", args, err, calls, out.String())
		}
	}
}

func TestAttachClientPreviewRedactsPlanFailure(t *testing.T) {
	const marker = "nonpublic-plan-failure-marker"
	cmd := newAttachClientCmdWithPlanBuilder(func(context.Context, api.AttachClientRequest) (*api.AttachClientPlan, error) {
		return nil, errors.Join(&api.AttachPlanError{Code: "manifest-missing"}, errors.New(marker))
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"fixture", "--daemon", "default", "--client", "mimocode"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "ATTACH_MANIFEST_MISSING stage=plan") || strings.Contains(err.Error(), marker) || out.Len() != 0 {
		t.Fatalf("error=%v output=%q", err, out.String())
	}
}

func TestAttachClientPreviewRejectsInvalidPlanWithoutOutput(t *testing.T) {
	cmd := newAttachClientCmdWithPlanBuilder(func(context.Context, api.AttachClientRequest) (*api.AttachClientPlan, error) {
		return &api.AttachClientPlan{Server: "other", Daemon: "default", Client: "mimocode"}, nil
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"fixture", "--daemon", "default", "--client", "mimocode"})
	err := cmd.Execute()
	if err == nil || err.Error() != "ATTACH_PLAN_INVALID stage=plan" || out.Len() != 0 {
		t.Fatalf("error=%v output=%q", err, out.String())
	}
}

func TestAttachClientPreviewIsRegisteredInServersGroup(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"attach-client"})
	if err != nil || cmd == nil || cmd.Name() != "attach-client" || cmd.GroupID != groupServers {
		t.Fatalf("command=%v error=%v", cmd, err)
	}
}

type attachPreviewFailingWriter struct{}

func (attachPreviewFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("private-output-destination")
}

func TestAttachClientPreviewRedactsOutputFailure(t *testing.T) {
	cmd := newAttachClientCmdWithPlanBuilder(func(context.Context, api.AttachClientRequest) (*api.AttachClientPlan, error) {
		return &api.AttachClientPlan{
			Server: "fixture", Daemon: "default", Client: "mimocode", EntryKey: "fixture",
			BeforeTransport: "direct", AfterTransport: "http",
			TargetBytesSHA256: strings.Repeat("a", 64), EntryTokenSHA256: strings.Repeat("b", 64),
		}, nil
	})
	cmd.SetOut(attachPreviewFailingWriter{})
	cmd.SetArgs([]string{"fixture", "--daemon", "default", "--client", "mimocode"})
	if err := cmd.Execute(); err == nil || err.Error() != "ATTACH_OUTPUT_FAILED stage=output" {
		t.Fatalf("output error=%v", err)
	}
}

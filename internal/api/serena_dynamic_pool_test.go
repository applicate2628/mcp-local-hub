// Package api — tests for the Phase 2 shared serena dynamic-pool
// builder/service (internal/api/serena_dynamic_pool.go).
package api

import (
	"testing"

	"mcp-local-hub/internal/config"
)

// globalEmbedSerenaManifest returns a manifest in the CURRENT embedded shape:
// kind: global, native-http, no daemon_template (the legacy 2-daemon
// claude/codex catalog). A consumer that demanded daemon_template.port_pool
// from this would fail closed — the service must NOT.
func globalEmbedSerenaManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "serena",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		BaseArgs:  []string{"--from", "git+https://example/serena", "serena", "start-mcp-server", "--transport", "streamable-http"},
		Env:       map[string]string{"PYTHONUNBUFFERED": "1"},
		Daemons: []config.DaemonSpec{
			{Name: "claude", Context: "claude-code", Port: 9121, ExtraArgs: []string{"--context", "claude-code"}},
			{Name: "codex", Context: "codex", Port: 9122, ExtraArgs: []string{"--context", "codex"}},
		},
	}
}

// TestSerenaDynamicPool_EffectiveTemplate_FromBuiltinDefault_WhenEmbedIsGlobal
// asserts that with the current embedded `kind: global` serena manifest, the
// service returns the built-in default template (no fail-closed). This is the
// finding #3 cycle-break.
func TestSerenaDynamicPool_EffectiveTemplate_FromBuiltinDefault_WhenEmbedIsGlobal(t *testing.T) {
	m := globalEmbedSerenaManifest()

	tpl := EffectiveSerenaDaemonTemplate(m)

	if tpl.Context != serenaDefaultContext {
		t.Errorf("Context = %q, want built-in default %q", tpl.Context, serenaDefaultContext)
	}
	if tpl.Context == "" {
		t.Errorf("built-in default Context must be non-empty (else --context %q at spawn)", tpl.Context)
	}
	if tpl.PortPool == nil {
		t.Fatalf("PortPool = nil; built-in default must supply a pool (no fail-closed)")
	}
	if tpl.PortPool.Start != serenaDefaultPortPoolStart || tpl.PortPool.End != serenaDefaultPortPoolEnd {
		t.Errorf("PortPool = {%d,%d}, want built-in default {%d,%d}",
			tpl.PortPool.Start, tpl.PortPool.End, serenaDefaultPortPoolStart, serenaDefaultPortPoolEnd)
	}
	wantArgs := []string{"--project", config.WorkspacePathToken}
	if !equalStringSliceDP(tpl.ExtraArgsTemplate, wantArgs) {
		t.Errorf("ExtraArgsTemplate = %v, want %v", tpl.ExtraArgsTemplate, wantArgs)
	}
	// The default's extra_args_template must NOT carry a --context token — the
	// context lives solely in DaemonTemplate.Context and is appended at spawn.
	if config.ArgsContainContextFlag(tpl.ExtraArgsTemplate) {
		t.Errorf("ExtraArgsTemplate must not contain --context; got %v", tpl.ExtraArgsTemplate)
	}
	// And it must carry the ${workspace.path} token (so the synthesized manifest
	// passes Validate's D.1 token requirement).
	if !containsWorkspacePathTokenDP(tpl.ExtraArgsTemplate) {
		t.Errorf("ExtraArgsTemplate must contain %q; got %v", config.WorkspacePathToken, tpl.ExtraArgsTemplate)
	}

	// Defensive-copy contract: mutating the returned slice/pool must not affect
	// a subsequent call's defaults.
	tpl.PortPool.Start = -1
	tpl.ExtraArgsTemplate[0] = "MUTATED"
	tpl2 := EffectiveSerenaDaemonTemplate(m)
	if tpl2.PortPool.Start != serenaDefaultPortPoolStart {
		t.Errorf("second call PortPool.Start = %d, want %d — defaults leaked through a shared pointer",
			tpl2.PortPool.Start, serenaDefaultPortPoolStart)
	}
	if tpl2.ExtraArgsTemplate[0] != "--project" {
		t.Errorf("second call ExtraArgsTemplate[0] = %q, want %q — defaults leaked through a shared slice",
			tpl2.ExtraArgsTemplate[0], "--project")
	}
}

// TestSerenaDynamicPool_EffectiveTemplate_PrefersEmbedDaemonTemplate_WhenPresent
// injects a manifest that DOES declare a daemon_template and asserts the
// service returns it verbatim (the embed has migrated to the dynamic-pool
// shape — the built-in default is not consulted).
func TestSerenaDynamicPool_EffectiveTemplate_PrefersEmbedDaemonTemplate_WhenPresent(t *testing.T) {
	m := &config.ServerManifest{
		Name:      "serena",
		Kind:      config.KindWorkspaceScoped,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		DaemonTemplate: &config.DaemonTemplate{
			Context:           "ide-assistant",
			PortPool:          &config.PortPool{Start: 9400, End: 9499},
			ExtraArgsTemplate: []string{"--project", config.WorkspacePathToken},
		},
	}

	tpl := EffectiveSerenaDaemonTemplate(m)

	if tpl.Context != "ide-assistant" {
		t.Errorf("Context = %q, want embed value %q (built-in default %q must NOT win)",
			tpl.Context, "ide-assistant", serenaDefaultContext)
	}
	if tpl.PortPool == nil || tpl.PortPool.Start != 9400 || tpl.PortPool.End != 9499 {
		t.Errorf("PortPool = %+v, want embed {9400,9499}", tpl.PortPool)
	}
	wantArgs := []string{"--project", config.WorkspacePathToken}
	if !equalStringSliceDP(tpl.ExtraArgsTemplate, wantArgs) {
		t.Errorf("ExtraArgsTemplate = %v, want embed %v", tpl.ExtraArgsTemplate, wantArgs)
	}

	// The returned pool must be a defensive copy — mutating it must not corrupt
	// the embed's struct.
	tpl.PortPool.Start = -1
	if m.DaemonTemplate.PortPool.Start != 9400 {
		t.Errorf("embed PortPool.Start mutated to %d via the returned copy — want immutable 9400",
			m.DaemonTemplate.PortPool.Start)
	}
}

// TestSerenaDynamicPool_BuildInMemoryManifest_PassesValidate_NativeHTTP asserts
// the in-memory dynamic-pool manifest passes Validate() AND that it is
// transport=native-http + kind=workspace-scoped.
func TestSerenaDynamicPool_BuildInMemoryManifest_PassesValidate_NativeHTTP(t *testing.T) {
	// Build from the legacy global embed — exercises the built-in-default path
	// AND the base_args reuse (the legacy --context lives in daemons[].extra_args,
	// NOT base_args, so reuse must stay --context-free).
	m := globalEmbedSerenaManifest()

	out, err := BuildInMemorySerenaDynamicPoolManifest(m)
	if err != nil {
		t.Fatalf("BuildInMemorySerenaDynamicPoolManifest: %v", err)
	}

	if err := out.Validate(); err != nil {
		t.Fatalf("synthesized manifest failed Validate(): %v", err)
	}
	if out.Transport != config.TransportNativeHTTP {
		t.Errorf("Transport = %q, want %q", out.Transport, config.TransportNativeHTTP)
	}
	if out.Kind != config.KindWorkspaceScoped {
		t.Errorf("Kind = %q, want %q", out.Kind, config.KindWorkspaceScoped)
	}
	if out.DaemonTemplate == nil {
		t.Fatalf("DaemonTemplate = nil; dynamic-pool manifest must carry one")
	}
	if out.DaemonTemplate.Context != serenaDefaultContext {
		t.Errorf("DaemonTemplate.Context = %q, want built-in default %q", out.DaemonTemplate.Context, serenaDefaultContext)
	}
	if out.Command != m.Command {
		t.Errorf("Command = %q, want embed command %q (the synthesized manifest must spawn the serena child)", out.Command, m.Command)
	}
	// No top-level port_pool / languages / daemons (the workspace-scoped
	// daemon_template branch of Validate rejects all three; Validate already
	// passed, but assert explicitly so a future Validate relaxation can't hide
	// a regression).
	if out.PortPool != nil {
		t.Errorf("top-level PortPool = %+v, want nil (moved into daemon_template)", out.PortPool)
	}
	if len(out.Languages) != 0 {
		t.Errorf("Languages = %v, want empty (dynamic-pool serena is multi-language per .serena/project.yml)", out.Languages)
	}
	if len(out.Daemons) != 0 {
		t.Errorf("Daemons = %v, want empty (mutually exclusive with daemon_template)", out.Daemons)
	}
	// The base_args reuse must not have smuggled a --context flag in.
	if config.ArgsContainContextFlag(out.BaseArgs) {
		t.Errorf("BaseArgs carries --context %v — context must come solely from daemon_template.context", out.BaseArgs)
	}
}

// TestSerenaDynamicPool_BuildInMemoryManifest_RejectsNilOrCommandless guards
// the build error paths so a malformed catalog input fails loud at build time
// rather than producing an empty-command manifest.
func TestSerenaDynamicPool_BuildInMemoryManifest_RejectsNilOrCommandless(t *testing.T) {
	if _, err := BuildInMemorySerenaDynamicPoolManifest(nil); err == nil {
		t.Errorf("nil embed: want error, got nil")
	}
	commandless := globalEmbedSerenaManifest()
	commandless.Command = ""
	if _, err := BuildInMemorySerenaDynamicPoolManifest(commandless); err == nil {
		t.Errorf("command-less embed: want error, got nil")
	}
}

// equalStringSliceDP is a small slice-equality helper local to the
// dynamic-pool tests (the package-level test file already defines several
// equal-* helpers; the DP suffix avoids a redeclaration collision).
func equalStringSliceDP(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// containsWorkspacePathTokenDP reports whether any arg contains the
// ${workspace.path} token.
func containsWorkspacePathTokenDP(args []string) bool {
	for _, a := range args {
		if a == config.WorkspacePathToken {
			return true
		}
	}
	return false
}

// Package api — area-4 router-native catalog flip guards.
//
// The shipped servers/serena/manifest.yaml was flipped from the
// unified-intermediate shape (kind: global, daemons[unified@9121], no
// daemon_template) to the dynamic-pool shape (kind: workspace-scoped,
// daemon_template, no daemons). These tests pin the two properties the flip
// must preserve:
//
//   - SYNTHESIZER IDENTITY: BuildInMemorySerenaDynamicPoolManifest of the
//     flipped (embed-carries-template) catalog produces a daemon_template +
//     command/base_args/env byte-identical to the prior (built-in-default)
//     projection, because the embed's explicit template MUST match
//     EffectiveSerenaDaemonTemplate's built-in default exactly.
//   - §7.1 GATE INERTNESS on a fresh (zero-workspace) install: the daemon plan
//     for the dynamic-pool catalog with no workspaces materializes ZERO
//     runtime_spec rows, so the spec-bearing-write gate
//     (desiredIntent.HasRuntimeSpecRow()) never fires — no INTRODUCE, no
//     cutover required on a brand-new host.
//
// Design ref: area-4 REVISE (architect a5920370 — accepted REVISE-to-narrower).
package api

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// loadShippedSerenaManifest parses the actual shipped catalog from disk. The
// test runs from internal/api so the source tree is reachable via ../../servers.
func loadShippedSerenaManifest(t *testing.T) *config.ServerManifest {
	t.Helper()
	f, err := os.Open("../../servers/serena/manifest.yaml")
	if err != nil {
		t.Fatalf("open shipped serena manifest: %v", err)
	}
	defer f.Close()
	m, err := config.ParseManifest(f)
	if err != nil {
		t.Fatalf("parse shipped serena manifest: %v", err)
	}
	return m
}

// priorUnifiedIntermediateSerenaManifest returns the serena catalog in the
// PRE-FLIP unified-intermediate shape (kind: global, one static `unified`
// daemon at 9121, NO daemon_template). The command/base_args/env mirror the
// shipped catalog so the synthesizer's reuse path is exercised identically; the
// daemon_template the synthesizer derives comes from EffectiveSerenaDaemonTemplate's
// BUILT-IN DEFAULT (the embed declares none). This is the reference output the
// flipped catalog must reproduce byte-for-byte.
func priorUnifiedIntermediateSerenaManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "serena",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		BaseArgs: []string{
			"--from",
			"git+https://github.com/oraios/serena@f0a3a279b7c48d28b9e7e4aea1ed9caed846906b",
			"serena",
			"start-mcp-server",
			"--transport",
			"streamable-http",
		},
		Env: map[string]string{"PYTHONUNBUFFERED": "1"},
		Daemons: []config.DaemonSpec{
			{Name: "unified", Context: "codex", Port: 9121, ExtraArgs: []string{"--context", "codex"}},
		},
	}
}

// TestSerenaRouterNativeFlip_ShippedCatalogTemplateMatchesBuiltinDefault pins the
// LOAD-BEARING precondition of the synthesizer-identity property: the flipped
// embed's daemon_template MUST equal EffectiveSerenaDaemonTemplate's built-in
// default EXACTLY. If they ever drift, the embed-wins branch would emit a
// different template than the prior built-in-default projection and the identity
// property below would silently break for every consumer.
func TestSerenaRouterNativeFlip_ShippedCatalogTemplateMatchesBuiltinDefault(t *testing.T) {
	shipped := loadShippedSerenaManifest(t)

	// Sanity: the shipped catalog is the dynamic-pool shape now.
	if shipped.Kind != config.KindWorkspaceScoped {
		t.Fatalf("shipped serena Kind = %q, want %q (router-native flip)", shipped.Kind, config.KindWorkspaceScoped)
	}
	if shipped.DaemonTemplate == nil {
		t.Fatalf("shipped serena has no daemon_template; the flip must add one")
	}
	if len(shipped.Daemons) != 0 {
		t.Fatalf("shipped serena still declares daemons[]=%v; the flip must remove them", shipped.Daemons)
	}

	// The embed-wins effective template (reads shipped.DaemonTemplate verbatim).
	embedWins := EffectiveSerenaDaemonTemplate(shipped)
	// The built-in default (what a no-daemon_template embed would yield).
	builtinDefault := EffectiveSerenaDaemonTemplate(priorUnifiedIntermediateSerenaManifest())

	if !reflect.DeepEqual(embedWins, builtinDefault) {
		t.Errorf("shipped embed daemon_template != built-in default:\n embed-wins = %+v (pool %+v)\n built-in   = %+v (pool %+v)",
			embedWins, embedWins.PortPool, builtinDefault, builtinDefault.PortPool)
	}
	// Spell out the exact values too so a drift report is readable.
	if embedWins.Context != serenaDefaultContext {
		t.Errorf("embed context = %q, want built-in default %q", embedWins.Context, serenaDefaultContext)
	}
	if embedWins.PortPool == nil || embedWins.PortPool.Start != serenaDefaultPortPoolStart || embedWins.PortPool.End != serenaDefaultPortPoolEnd {
		t.Errorf("embed port_pool = %+v, want built-in default {%d,%d}", embedWins.PortPool, serenaDefaultPortPoolStart, serenaDefaultPortPoolEnd)
	}
	wantArgs := []string{"--project", config.WorkspacePathToken}
	if !equalStringSliceDP(embedWins.ExtraArgsTemplate, wantArgs) {
		t.Errorf("embed extra_args_template = %v, want built-in default %v", embedWins.ExtraArgsTemplate, wantArgs)
	}
}

// TestSerenaRouterNativeFlip_SynthesizerIsIdentityProjection asserts that
// BuildInMemorySerenaDynamicPoolManifest(flipped-catalog) produces a manifest
// byte-identical (in every field the synthesizer copies) to
// BuildInMemorySerenaDynamicPoolManifest(prior-unified-intermediate-catalog).
// Because the flipped embed's explicit template equals the prior built-in
// default, the synthesizer becomes an IDENTITY projection — the migrate / E.2
// auto-register paths see the SAME in-memory manifest before and after the flip.
func TestSerenaRouterNativeFlip_SynthesizerIsIdentityProjection(t *testing.T) {
	flipped := loadShippedSerenaManifest(t)
	prior := priorUnifiedIntermediateSerenaManifest()

	outFlipped, err := BuildInMemorySerenaDynamicPoolManifest(flipped)
	if err != nil {
		t.Fatalf("build from flipped catalog: %v", err)
	}
	outPrior, err := BuildInMemorySerenaDynamicPoolManifest(prior)
	if err != nil {
		t.Fatalf("build from prior unified-intermediate catalog: %v", err)
	}

	// Both must validate.
	if err := outFlipped.Validate(); err != nil {
		t.Fatalf("flipped synthesized manifest failed Validate(): %v", err)
	}
	if err := outPrior.Validate(); err != nil {
		t.Fatalf("prior synthesized manifest failed Validate(): %v", err)
	}

	// daemon_template byte-identical.
	if !reflect.DeepEqual(outFlipped.DaemonTemplate, outPrior.DaemonTemplate) {
		t.Errorf("synthesized daemon_template differs after the flip:\n flipped = %+v (pool %+v)\n prior   = %+v (pool %+v)",
			outFlipped.DaemonTemplate, outFlipped.DaemonTemplate.PortPool,
			outPrior.DaemonTemplate, outPrior.DaemonTemplate.PortPool)
	}
	// command / base_args / env byte-identical (the spawn inputs).
	if outFlipped.Command != outPrior.Command {
		t.Errorf("synthesized Command differs: flipped=%q prior=%q", outFlipped.Command, outPrior.Command)
	}
	if !reflect.DeepEqual(outFlipped.BaseArgs, outPrior.BaseArgs) {
		t.Errorf("synthesized BaseArgs differ:\n flipped = %v\n prior   = %v", outFlipped.BaseArgs, outPrior.BaseArgs)
	}
	if !reflect.DeepEqual(outFlipped.Env, outPrior.Env) {
		t.Errorf("synthesized Env differs:\n flipped = %v\n prior   = %v", outFlipped.Env, outPrior.Env)
	}
	// kind + transport identical.
	if outFlipped.Kind != outPrior.Kind || outFlipped.Transport != outPrior.Transport {
		t.Errorf("synthesized kind/transport differ: flipped=(%q,%q) prior=(%q,%q)",
			outFlipped.Kind, outFlipped.Transport, outPrior.Kind, outPrior.Transport)
	}
}

// TestSerenaRouterNativeFlip_FreshInstall_ZeroWorkspaces_NoRuntimeSpec_GateInert
// asserts the §7.1 gate is INERT on a brand-new host installing the dynamic-pool
// catalog with zero registered workspaces: the daemon plan materializes ZERO
// rows (so HasRuntimeSpecRow() over the resulting intent is false), which means
// the spec-bearing-write gate (desiredIntent.HasRuntimeSpecRow() &&
// !priorIntent.HasRuntimeSpecRow(), install_parsed_manifest.go) cannot fire —
// no INTRODUCE, no cutover needed on a fresh install.
func TestSerenaRouterNativeFlip_FreshInstall_ZeroWorkspaces_NoRuntimeSpec_GateInert(t *testing.T) {
	flipped := loadShippedSerenaManifest(t)
	dyn, err := BuildInMemorySerenaDynamicPoolManifest(flipped)
	if err != nil {
		t.Fatalf("build dynamic-pool manifest from shipped catalog: %v", err)
	}

	// The guard the gate keys on: m.DaemonTemplate != nil is TRUE, but with zero
	// workspaces the plan is empty.
	if dyn.DaemonTemplate == nil {
		t.Fatalf("dynamic-pool manifest must carry a daemon_template")
	}

	// Zero registered workspaces (a fresh host).
	plan := BuildSupervisorDaemonsForSerena(dyn, []WorkspaceEntry{}, "deadbeef", "mcphub")
	if len(plan) != 0 {
		t.Fatalf("zero-workspace serena plan must be empty (no runtime_spec rows); got %d rows: %+v", len(plan), plan)
	}

	// Build an intent from the (empty) plan and confirm the gate's predicate is
	// false — the strongest direct proof the §7.1 gate stays inert.
	intent := &SupervisorIntentFile{Version: 1, Daemons: plan}
	if intent.HasRuntimeSpecRow() {
		t.Errorf("a zero-workspace dynamic-pool install must produce no runtime_spec rows; HasRuntimeSpecRow() = true")
	}

	// Cross-check: with ONE workspace the plan DOES materialize a runtime_spec
	// row, proving the zero-result above is the workspace-count guard firing, not
	// a manifest defect that would make EVERY install inert (non-vacuous).
	ws := WorkspaceEntry{
		WorkspaceKey:  WorkspaceKey("/tmp/ws-alpha"),
		WorkspacePath: "/tmp/ws-alpha",
		Language:      SerenaLanguageSentinel,
		Port:          9150,
	}
	planOne := BuildSupervisorDaemonsForSerena(dyn, []WorkspaceEntry{ws}, "deadbeef", "mcphub")
	if len(planOne) != 1 || planOne[0].RuntimeSpec == nil {
		t.Fatalf("one-workspace plan must materialize exactly one runtime_spec row; got %d rows (%+v) — the zero-result test would be vacuous otherwise", len(planOne), planOne)
	}
	if !strings.EqualFold(planOne[0].Server, "serena") {
		t.Errorf("materialized row Server = %q, want serena", planOne[0].Server)
	}
}

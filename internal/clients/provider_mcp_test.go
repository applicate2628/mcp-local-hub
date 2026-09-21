package clients

import (
	"context"
	"os"
	"testing"
	"time"
)

type providerSourceForWrapperTest struct {
	*codexCLI
	entries []ProviderMCPEntryV1
}

func (p *providerSourceForWrapperTest) ListProviderMCPEntries(context.Context) ([]ProviderMCPEntryV1, error) {
	return p.entries, nil
}

func (p *providerSourceForWrapperTest) CompareAndSetProviderMCPActivation(context.Context, ProviderMCPActivationCASV1) (ProviderMCPActivationResultV1, error) {
	return ProviderMCPActivationResultV1{}, nil
}

func TestLockingClientForwardsProviderMCPSource(t *testing.T) {
	base := &providerSourceForWrapperTest{codexCLI: &codexCLI{path: setupCodexConfig(t, "[plugins]\n")}, entries: []ProviderMCPEntryV1{{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: "reader", Args: []string{"--stdio"}, Env: map[string]string{"MODE": "read"}}}}
	wrapped := newLockingClient(base)
	source, ok := wrapped.(ProviderMCPSourceV1)
	if !ok {
		t.Fatal("locking client does not implement ProviderMCPSourceV1")
	}
	entries, err := source.ListProviderMCPEntries(context.Background())
	if err != nil || len(entries) != 1 || entries[0].ServerName != "reader" {
		t.Fatalf("ListProviderMCPEntries = %#v, %v", entries, err)
	}
	entries[0].Args[0] = "mutated"
	entries[0].Env["MODE"] = "mutated"
	if base.entries[0].Args[0] != "--stdio" || base.entries[0].Env["MODE"] != "read" {
		t.Fatal("locking forwarder leaked provider entry backing storage")
	}
	if _, err := source.CompareAndSetProviderMCPActivation(context.Background(), ProviderMCPActivationCASV1{}); err != nil {
		t.Fatalf("CAS forwarder error = %v", err)
	}
}

func TestCodexProviderMCPActivationCASRequiresFingerprints(t *testing.T) {
	c := &codexCLI{path: setupCodexConfig(t, "[plugins]\n")}
	_, err := c.CompareAndSetProviderMCPActivation(context.Background(), ProviderMCPActivationCASV1{PluginRef: "alpha@catalog", ServerName: "reader", DesiredEnabledPresent: true, DesiredEnabled: false})
	if err == nil {
		t.Fatal("CAS accepted missing fingerprints")
	}
}

func TestCodexProviderMCPActivationCASAlreadyDesiredDoesNotRewrite(t *testing.T) {
	path := setupCodexConfig(t, `[plugins."alpha@catalog".mcp_servers.reader]
enabled = true
scope = "user"
`)
	c := &codexCLI{path: path}
	doc, err := c.readTOML()
	if err != nil {
		t.Fatal(err)
	}
	server := doc["plugins"].(map[string]any)["alpha@catalog"].(map[string]any)["mcp_servers"].(map[string]any)["reader"].(map[string]any)
	activation, err := providerActivationFingerprint(server)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := providerPolicyFingerprint(server)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	result, err := c.CompareAndSetProviderMCPActivation(context.Background(), ProviderMCPActivationCASV1{
		PluginRef: "alpha@catalog", ServerName: "reader",
		ExpectedActivationFingerprint: activation, ExpectedPolicyFingerprint: policy,
		DesiredEnabledPresent: true, DesiredEnabled: true,
	})
	if err != nil || !result.PriorEnabledPresent || !result.PriorEnabled || result.ActivationFingerprint != activation {
		t.Fatalf("same-state CAS result=%+v err=%v", result, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(fixed) {
		t.Fatalf("same-state CAS rewrote config: modtime=%s want=%s", info.ModTime(), fixed)
	}
}

func TestCodexProviderMCPActivationCASRoundTripsAbsentAndPreservesSiblings(t *testing.T) {
	c := &codexCLI{path: setupCodexConfig(t, `[plugins."alpha@catalog".mcp_servers.reader]
scope = "user"

[plugins."alpha@catalog".mcp_servers.sibling]
enabled = true
policy = "keep"
`)}
	doc, err := c.readTOML()
	if err != nil {
		t.Fatal(err)
	}
	server := doc["plugins"].(map[string]any)["alpha@catalog"].(map[string]any)["mcp_servers"].(map[string]any)["reader"].(map[string]any)
	activation, err := providerActivationFingerprint(server)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := providerPolicyFingerprint(server)
	if err != nil {
		t.Fatal(err)
	}
	req := ProviderMCPActivationCASV1{PluginRef: "alpha@catalog", ServerName: "reader", ExpectedActivationFingerprint: activation, ExpectedPolicyFingerprint: policy, DesiredEnabledPresent: true, DesiredEnabled: false}
	result, err := c.CompareAndSetProviderMCPActivation(context.Background(), req)
	if err != nil || result.PriorEnabledPresent || result.PriorEnabled {
		t.Fatalf("disable result=%+v err=%v", result, err)
	}
	doc, err = c.readTOML()
	if err != nil {
		t.Fatal(err)
	}
	server = doc["plugins"].(map[string]any)["alpha@catalog"].(map[string]any)["mcp_servers"].(map[string]any)["reader"].(map[string]any)
	if got, ok := server["enabled"].(bool); !ok || got {
		t.Fatalf("enabled=%#v, want false", server["enabled"])
	}
	if got := doc["plugins"].(map[string]any)["alpha@catalog"].(map[string]any)["mcp_servers"].(map[string]any)["sibling"].(map[string]any)["policy"]; got != "keep" {
		t.Fatalf("sibling policy=%#v", got)
	}
	activation, err = providerActivationFingerprint(server)
	if err != nil {
		t.Fatal(err)
	}
	policy, err = providerPolicyFingerprint(server)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CompareAndSetProviderMCPActivation(context.Background(), ProviderMCPActivationCASV1{PluginRef: req.PluginRef, ServerName: req.ServerName, ExpectedActivationFingerprint: activation, ExpectedPolicyFingerprint: policy, DesiredEnabledPresent: false})
	if err != nil {
		t.Fatalf("restore absent: %v", err)
	}
	doc, err = c.readTOML()
	if err != nil {
		t.Fatal(err)
	}
	server = doc["plugins"].(map[string]any)["alpha@catalog"].(map[string]any)["mcp_servers"].(map[string]any)["reader"].(map[string]any)
	if _, present := server["enabled"]; present {
		t.Fatalf("restore left enabled override: %#v", server)
	}
}

func TestCodexProviderMCPActivationCASCreatesAbsentSelectedOverrideOnly(t *testing.T) {
	c := &codexCLI{path: setupCodexConfig(t, `[plugins."alpha@catalog"]
enabled = true

[plugins."alpha@catalog".sibling]
keep = "unchanged"
`)}
	empty := map[string]any{}
	activation, err := providerActivationFingerprint(empty)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := providerPolicyFingerprint(empty)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.CompareAndSetProviderMCPActivation(context.Background(), ProviderMCPActivationCASV1{PluginRef: "alpha@catalog", ServerName: "reader", ExpectedActivationFingerprint: activation, ExpectedPolicyFingerprint: policy, DesiredEnabledPresent: true, DesiredEnabled: false})
	if err != nil || result.PriorEnabledPresent || result.PriorEnabled {
		t.Fatalf("CAS result=%+v err=%v", result, err)
	}
	doc, err := c.readTOML()
	if err != nil {
		t.Fatal(err)
	}
	plugin := doc["plugins"].(map[string]any)["alpha@catalog"].(map[string]any)
	server := plugin["mcp_servers"].(map[string]any)["reader"].(map[string]any)
	if got, ok := server["enabled"].(bool); !ok || got {
		t.Fatalf("selected enabled=%#v", server["enabled"])
	}
	if plugin["enabled"] != true || plugin["sibling"].(map[string]any)["keep"] != "unchanged" {
		t.Fatalf("root/sibling changed: %#v", plugin)
	}
	activation, err = providerActivationFingerprint(server)
	if err != nil {
		t.Fatal(err)
	}
	policy, err = providerPolicyFingerprint(server)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CompareAndSetProviderMCPActivation(context.Background(), ProviderMCPActivationCASV1{PluginRef: "alpha@catalog", ServerName: "reader", ExpectedActivationFingerprint: activation, ExpectedPolicyFingerprint: policy, DesiredEnabledPresent: false}); err != nil {
		t.Fatalf("restore absent override: %v", err)
	}
	doc, err = c.readTOML()
	if err != nil {
		t.Fatal(err)
	}
	plugin = doc["plugins"].(map[string]any)["alpha@catalog"].(map[string]any)
	if _, present := plugin["mcp_servers"]; present {
		t.Fatalf("absent prior override left empty activation table: %#v", plugin)
	}
	if plugin["enabled"] != true || plugin["sibling"].(map[string]any)["keep"] != "unchanged" {
		t.Fatalf("root/sibling changed after restore: %#v", plugin)
	}
}

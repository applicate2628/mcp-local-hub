package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mcp-local-hub/internal/process"

	toml "github.com/pelletier/go-toml/v2"
)

var errCodexProviderInventoryUnavailable = errors.New("E_PROVIDER_INVENTORY_UNAVAILABLE")

const (
	codexProviderInventoryTimeout     = 120 * time.Second
	codexProviderInventoryStdoutLimit = 4 << 20
	codexProviderInventoryStderrLimit = 64 << 10
)

var codexProviderMCPInventoryRun = runCodexProviderMCPInventory

type codexPluginInventory struct {
	Installed []codexPluginInventoryRow `json:"installed"`
}

type codexPluginInventoryRow struct {
	PluginID        string                     `json:"pluginId"`
	Name            string                     `json:"name"`
	MarketplaceName string                     `json:"marketplaceName"`
	Version         string                     `json:"version"`
	Installed       bool                       `json:"installed"`
	Enabled         bool                       `json:"enabled"`
	Source          codexPluginInventorySource `json:"source"`
	InstallPolicy   any                        `json:"installPolicy"`
	AuthPolicy      any                        `json:"authPolicy"`
}

type codexPluginInventorySource struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	Path   string `json:"path"`
}

type codexPluginManifest struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	MCPServers string `json:"mcpServers"`
}

func (c *codexCLI) ListProviderMCPEntries(ctx context.Context) ([]ProviderMCPEntryV1, error) {
	raw, err := codexProviderMCPInventoryRun(ctx)
	if err != nil {
		return nil, providerInventoryUnavailable(err)
	}
	var inventory codexPluginInventory
	if err := json.Unmarshal(raw, &inventory); err != nil {
		return nil, providerInventoryUnavailable(err)
	}
	home, err := resolveCodexHome()
	if err != nil {
		return nil, providerInventoryUnavailable(err)
	}
	config, err := codexProviderConfig(c.path)
	if err != nil {
		return nil, providerInventoryUnavailable(err)
	}
	entries := make([]ProviderMCPEntryV1, 0)
	for _, row := range inventory.Installed {
		if !row.Installed {
			continue
		}
		if err := validateCodexPluginInventoryRow(row); err != nil {
			return nil, providerInventoryUnavailable(err)
		}
		receiptRoot, err := codexProviderReceiptRoot(home, row)
		if err != nil {
			return nil, providerInventoryUnavailable(err)
		}
		servers, receiptFingerprint, err := codexProviderReceiptServers(receiptRoot, row)
		if err != nil {
			return nil, providerInventoryUnavailable(err)
		}
		for serverName, rawServer := range servers {
			entry, err := normalizeCodexProviderEntry(row, receiptRoot, serverName, rawServer, config)
			if err != nil {
				return nil, providerInventoryUnavailable(err)
			}
			entry.ReceiptFingerprint = receiptFingerprint
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].PluginRef == entries[j].PluginRef {
			return entries[i].ServerName < entries[j].ServerName
		}
		return entries[i].PluginRef < entries[j].PluginRef
	})
	return entries, nil
}

func runCodexProviderMCPInventory(parent context.Context) ([]byte, error) {
	exe, err := exec.LookPath("codex")
	if err != nil {
		return nil, err
	}
	command, err := resolveCodexProviderInventoryCommand(exe)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, codexProviderInventoryTimeout)
	defer cancel()
	result, err := process.RunStrictlyContained(ctx, process.StrictRunInvocation{
		Command: command, Input: []byte{}, InputLimit: 1,
		StdoutLimit: codexProviderInventoryStdoutLimit, StderrLimit: codexProviderInventoryStderrLimit,
	})
	if err != nil || result.Stdout.Truncated || result.Stderr.Truncated {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("inventory output truncated")
	}
	return append([]byte(nil), result.Stdout.Prefix...), nil
}

// resolveCodexProviderInventoryCommand accepts either a direct executable or
// the standard npm Windows command shim. The shim is parsed as data and is
// never executed through cmd.exe: a sibling node.exe is preferred, with the
// standard npm native PATH-node fallback accepted only when it is native.
// One verified package-local JavaScript entry and fixed inventory arguments are
// admitted. Unknown batch files remain refused.
func resolveCodexProviderInventoryCommand(exe string) (*exec.Cmd, error) {
	return resolveCodexProviderInventoryCommandWithLookup(exe, exec.LookPath)
}

func resolveCodexProviderInventoryCommandWithLookup(exe string, lookup func(string) (string, error)) (*exec.Cmd, error) {
	ext := strings.ToLower(filepath.Ext(exe))
	if ext == ".bat" {
		return nil, errors.New("unsupported codex cmd shim")
	}
	if ext != ".cmd" {
		return exec.Command(exe, "plugin", "list", "--json"), nil
	}
	shimFile, err := os.Open(exe)
	if err != nil {
		return nil, errors.New("unsupported codex cmd shim")
	}
	defer shimFile.Close()
	shimRaw, err := io.ReadAll(io.LimitReader(shimFile, 64<<10+1))
	if err != nil || len(shimRaw) > 64<<10 {
		return nil, errors.New("unsupported codex cmd shim")
	}
	shim := strings.ReplaceAll(string(shimRaw), "\r\n", "\n")
	if !strings.Contains(shim, "CALL :find_dp0") || !strings.Contains(shim, "SETLOCAL") || !strings.Contains(shim, "IF EXIST \"%dp0%\\node.exe\"") || !strings.Contains(shim, "\"%_prog%\"") || !strings.Contains(shim, "%*") {
		return nil, errors.New("unsupported codex cmd shim")
	}
	const marker = `"%dp0%\`
	var rel string
	for remaining := shim; ; {
		start := strings.Index(remaining, marker)
		if start < 0 {
			break
		}
		remaining = remaining[start+len(marker):]
		end := strings.Index(remaining, `"`)
		if end < 0 {
			return nil, errors.New("unsupported codex cmd shim")
		}
		candidate := remaining[:end]
		remaining = remaining[end+1:]
		if strings.HasSuffix(strings.ToLower(candidate), ".js") {
			if rel != "" || !safeNPMShimRelativePath(candidate) || !strings.EqualFold(strings.ReplaceAll(candidate, "\\", "/"), "node_modules/@openai/codex/bin/codex.js") {
				return nil, errors.New("unsupported codex cmd shim")
			}
			rel = candidate
		}
	}
	if rel == "" {
		return nil, errors.New("unsupported codex cmd shim")
	}
	shimDir := filepath.Dir(exe)
	node, err := resolveCodexProviderNode(shimDir, lookup)
	if err != nil {
		return nil, err
	}
	script := filepath.Clean(filepath.Join(shimDir, filepath.FromSlash(strings.ReplaceAll(rel, "\\", "/"))))
	contained, err := filepath.Rel(shimDir, script)
	if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
		return nil, errors.New("unsupported codex cmd shim")
	}
	if info, err := os.Stat(script); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("unsupported codex cmd shim")
	}
	return exec.Command(node, script, "plugin", "list", "--json"), nil
}

func resolveCodexProviderNode(shimDir string, lookup func(string) (string, error)) (string, error) {
	sibling := filepath.Join(shimDir, "node.exe")
	if info, err := os.Stat(sibling); err == nil && info.Mode().IsRegular() {
		return sibling, nil
	}
	node, err := lookup("node")
	if err != nil || !filepath.IsAbs(node) {
		return "", errors.New("unsupported codex cmd shim")
	}
	ext := strings.ToLower(filepath.Ext(node))
	if ext != ".exe" && ext != ".com" {
		return "", errors.New("unsupported codex cmd shim")
	}
	if info, err := os.Stat(node); err != nil || !info.Mode().IsRegular() {
		return "", errors.New("unsupported codex cmd shim")
	}
	return node, nil
}

func safeNPMShimRelativePath(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '\\' || r == '/' }) {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return !strings.ContainsAny(path, `:%*"`)
}

func providerInventoryUnavailable(cause error) error {
	return fmt.Errorf("%w: %v", errCodexProviderInventoryUnavailable, cause)
}

func validateCodexPluginInventoryRow(row codexPluginInventoryRow) error {
	if row.PluginID == "" || row.Name == "" || row.MarketplaceName == "" || row.Version == "" || row.Source.Source == "" || row.InstallPolicy == nil || row.AuthPolicy == nil {
		return errors.New("inventory row missing required field")
	}
	if row.Source.Source == "local" {
		if row.Source.Path == "" || !filepath.IsAbs(row.Source.Path) {
			return errors.New("inventory local source path invalid")
		}
		return nil
	}
	if row.Source.ID == "" {
		return errors.New("inventory source missing id")
	}
	return nil
}

func codexProviderReceiptRoot(home string, row codexPluginInventoryRow) (string, error) {
	if row.Source.Source == "local" {
		receipt := filepath.Clean(row.Source.Path)
		info, err := os.Stat(receipt)
		if err != nil || !info.IsDir() {
			return "", errors.New("local receipt root unavailable")
		}
		return receipt, nil
	}
	for _, value := range []string{row.MarketplaceName, row.Name, row.Version} {
		if !singleProviderPathComponent(value) {
			return "", errors.New("receipt tuple contains unsafe component")
		}
	}
	root := filepath.Join(home, "plugins", "cache")
	receipt := filepath.Join(root, row.MarketplaceName, row.Name, row.Version)
	rel, err := filepath.Rel(root, receipt)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("receipt tuple escapes cache root")
	}
	info, err := os.Stat(receipt)
	if err != nil || !info.IsDir() {
		return "", errors.New("receipt root unavailable")
	}
	return receipt, nil
}

func singleProviderPathComponent(value string) bool {
	return value != "" && value != "." && value != ".." && !filepath.IsAbs(value) && !strings.ContainsAny(value, `\/`)
}

func codexProviderReceiptServers(receiptRoot string, row codexPluginInventoryRow) (map[string]map[string]any, string, error) {
	pluginPath := filepath.Join(receiptRoot, ".codex-plugin", "plugin.json")
	pluginRaw, err := os.ReadFile(pluginPath)
	if err != nil {
		return nil, "", err
	}
	var manifest codexPluginManifest
	if err := json.Unmarshal(pluginRaw, &manifest); err != nil || manifest.Name != row.Name || manifest.Version != row.Version || manifest.MCPServers == "" {
		return nil, "", errors.New("receipt manifest identity or mcp pointer invalid")
	}
	if filepath.IsAbs(manifest.MCPServers) {
		return nil, "", errors.New("receipt mcp pointer is absolute")
	}
	mcpPath := filepath.Clean(filepath.Join(receiptRoot, manifest.MCPServers))
	rel, err := filepath.Rel(receiptRoot, mcpPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", errors.New("receipt mcp pointer escapes root")
	}
	mcpRaw, err := os.ReadFile(mcpPath)
	if err != nil {
		return nil, "", err
	}
	var raw map[string]any
	if err := json.Unmarshal(mcpRaw, &raw); err != nil {
		return nil, "", err
	}
	standard, hasStandard := raw["mcpServers"]
	legacy, hasLegacy := raw["mcp_servers"]
	if hasStandard && hasLegacy {
		return nil, "", errors.New("receipt mcp server wrapper ambiguous")
	}
	serversMap := raw
	selected := standard
	if hasLegacy {
		selected = legacy
	}
	if hasStandard || hasLegacy {
		var ok bool
		serversMap, ok = selected.(map[string]any)
		if !ok {
			return nil, "", errors.New("receipt mcp server wrapper invalid")
		}
	}
	servers := make(map[string]map[string]any, len(serversMap))
	for name, value := range serversMap {
		server, ok := value.(map[string]any)
		if !ok || name == "" {
			return nil, "", errors.New("receipt server entry invalid")
		}
		servers[name] = cloneProviderMap(server)
	}
	fingerprint, err := providerFingerprint(struct {
		Inventory codexPluginInventoryRow `json:"inventory"`
		Plugin    json.RawMessage         `json:"plugin"`
		MCP       json.RawMessage         `json:"mcp"`
	}{row, pluginRaw, mcpRaw})
	return servers, fingerprint, err
}

func codexProviderConfig(path string) (map[string]any, error) {
	var out map[string]any
	err := withConfigReadLock(path, func() error {
		raw, present, err := readCodexConfigNoReparse(path)
		if err != nil {
			return err
		}
		if !present {
			out = map[string]any{}
			return nil
		}
		if err := toml.Unmarshal(raw, &out); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func normalizeCodexProviderEntry(row codexPluginInventoryRow, receiptRoot, serverName string, raw map[string]any, config map[string]any) (ProviderMCPEntryV1, error) {
	entry := ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: row.PluginID, ServerName: serverName, Scope: ProviderMCPScopeUser, PolicyState: ProviderMCPPolicyNone}
	if url, ok := raw["url"].(string); ok && url != "" {
		entry.Transport = ProviderMCPTransportHTTP
	} else {
		entry.Transport = ProviderMCPTransportStdio
		command, ok := raw["command"].(string)
		if !ok || command == "" {
			return entry, errors.New("receipt command missing")
		}
		entry.Command = command
	}
	args, err := providerStringSlice(raw["args"])
	if err != nil {
		return entry, err
	}
	entry.Args = args
	if providerHasVariable(entry.Command) || providerStringsHaveVariable(args) {
		return entry, errors.New("receipt variable expansion unsupported")
	}
	cwd, present := raw["cwd"]
	if !present {
		return entry, errors.New("receipt cwd missing")
	}
	cwdText, ok := cwd.(string)
	if !ok {
		return entry, errors.New("receipt cwd invalid")
	}
	if providerHasVariable(cwdText) {
		return entry, errors.New("receipt variable expansion unsupported")
	}
	if cwdText == "" {
		entry.WorkingDir = new(string)
	} else {
		resolved := filepath.Clean(filepath.Join(receiptRoot, cwdText))
		if !filepath.IsAbs(resolved) {
			return entry, errors.New("receipt cwd unresolved")
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return entry, errors.New("receipt cwd unavailable")
		}
		entry.WorkingDir = &resolved
	}
	env, err := providerStringMap(raw["env"])
	if err != nil {
		return entry, err
	}
	for _, value := range env {
		if providerHasVariable(value) {
			return entry, errors.New("receipt variable expansion unsupported")
		}
	}
	entry.Env = env
	forward, err := providerLocalEnvNames(raw["env_vars"])
	if err != nil {
		return entry, err
	}
	entry.EnvForwardLocal = forward
	if timeout, ok := raw["tool_timeout_sec"]; ok {
		number, ok := timeout.(float64)
		if !ok || number < 0 || number != float64(int(number)) {
			return entry, errors.New("receipt timeout invalid")
		}
		entry.ToolTimeoutSec = int(number)
	}
	activation, policy := codexProviderServerConfig(config, row.PluginID, serverName)
	activationValue, activationPresent := activation["enabled"]
	activationEnabled, _ := activationValue.(bool)
	entry.ActivationEnabledPresent = activationPresent
	entry.ActivationEnabled = activationEnabled
	entry.Enabled = row.Enabled && providerEnabled(activation)
	entry.ActivationFingerprint, err = providerActivationFingerprint(activation)
	if err != nil {
		return entry, err
	}
	disabledActivation := cloneProviderMap(activation)
	disabledActivation["enabled"] = false
	entry.DisabledActivationFingerprint, err = providerActivationFingerprint(disabledActivation)
	if err != nil {
		return entry, err
	}
	entry.PolicyFingerprint, err = providerPolicyFingerprint(policy)
	if err != nil {
		return entry, err
	}
	if providerPolicyPresent(policy) {
		entry.PolicyState = ProviderMCPPolicyUnrepresentable
	}
	return entry, nil
}

func codexProviderServerConfig(config map[string]any, pluginRef, server string) (map[string]any, map[string]any) {
	plugins, _ := config["plugins"].(map[string]any)
	plugin, _ := plugins[pluginRef].(map[string]any)
	servers, _ := plugin["mcp_servers"].(map[string]any)
	selected, _ := servers[server].(map[string]any)
	if selected == nil {
		selected = map[string]any{}
	}
	return cloneProviderMap(selected), cloneProviderMap(selected)
}

func providerEnabled(raw map[string]any) bool {
	value, present := raw["enabled"]
	if !present {
		return true
	}
	enabled, ok := value.(bool)
	return ok && enabled
}
func providerPolicyPresent(raw map[string]any) bool {
	for _, key := range []string{"default_tools_approval_mode", "enabled_tools", "disabled_tools", "tools"} {
		if _, ok := raw[key]; ok {
			return true
		}
	}
	return false
}
func providerStringSlice(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, errors.New("receipt args invalid")
	}
	out := make([]string, len(values))
	for i, v := range values {
		var ok bool
		out[i], ok = v.(string)
		if !ok {
			return nil, errors.New("receipt arg invalid")
		}
	}
	return out, nil
}
func providerStringMap(value any) (map[string]string, error) {
	out := map[string]string{}
	if value == nil {
		return out, nil
	}
	values, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("receipt env invalid")
	}
	for k, v := range values {
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("receipt env value invalid")
		}
		out[k] = s
	}
	return out, nil
}
func providerLocalEnvNames(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, errors.New("receipt env_vars invalid")
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		switch item := v.(type) {
		case string:
			if item == "" {
				return nil, errors.New("receipt env_var missing field")
			}
			out = append(out, item)
		case map[string]any:
			name, nok := item["name"].(string)
			source, sok := item["source"].(string)
			if !nok || name == "" || !sok {
				return nil, errors.New("receipt env_var missing field")
			}
			if source != "local" {
				return nil, errors.New("receipt remote env unsupported")
			}
			out = append(out, name)
		default:
			return nil, errors.New("receipt env_var invalid")
		}
	}
	sort.Strings(out)
	return out, nil
}
func providerHasVariable(value string) bool { return strings.Contains(value, "${") }
func providerStringsHaveVariable(values []string) bool {
	for _, value := range values {
		if providerHasVariable(value) {
			return true
		}
	}
	return false
}
func cloneProviderMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

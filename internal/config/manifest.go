package config

import (
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind enumerates daemon types. Only these two values are valid in manifest.kind.
const (
	KindGlobal          = "global"
	KindWorkspaceScoped = "workspace-scoped"
)

// Transport enumerates how the server speaks MCP. Only these are valid.
const (
	TransportNativeHTTP  = "native-http"
	TransportStdioBridge = "stdio-bridge"
)

// NativeHTTPInternalPortOffset is the fixed delta between a native-http
// daemon's external (client-facing) port and the internal port its
// upstream subprocess binds. Lives here so the two independent readers
// — api.Preflight (port-free check at install) and cli/daemon.go
// (subprocess --port flag at runtime) — share a single source of truth.
const NativeHTTPInternalPortOffset = 10000

// ServerManifest is the parsed form of a `servers/<name>/manifest.yaml` file.
type ServerManifest struct {
	Name             string            `yaml:"name"`
	Kind             string            `yaml:"kind"`
	Transport        string            `yaml:"transport"`
	Command          string            `yaml:"command"`
	BaseArgs         []string          `yaml:"base_args"`
	BaseArgsTemplate []string          `yaml:"base_args_template"`
	Env              map[string]string `yaml:"env"`
	Daemons          []DaemonSpec      `yaml:"daemons"`
	Languages        []LanguageSpec    `yaml:"languages"`
	PortPool         *PortPool         `yaml:"port_pool"`
	IdleTimeoutMin   int               `yaml:"idle_timeout_min"`
	ClientBindings   []ClientBinding   `yaml:"client_bindings"`
	WeeklyRefresh    bool              `yaml:"weekly_refresh"`
}

type DaemonSpec struct {
	Name      string   `yaml:"name"`
	Context   string   `yaml:"context"`
	Port      int      `yaml:"port"`
	ExtraArgs []string `yaml:"extra_args"`
}

type LanguageSpec struct {
	Name       string   `yaml:"name"`
	LspCommand string   `yaml:"lsp_command"`
	ExtraFlags []string `yaml:"extra_flags"`
}

type PortPool struct {
	Start int `yaml:"start"`
	End   int `yaml:"end"`
}

type ClientBinding struct {
	Client  string `yaml:"client"`
	Daemon  string `yaml:"daemon"`
	URLPath string `yaml:"url_path"`
}

// ParseManifest reads YAML from r and returns a validated ServerManifest.
// Returns an error if required fields are missing or kind/transport values
// are unknown.
//
// Environment expansion: ${USERPROFILE}, ${HOME}, and other ${ENV} tokens
// in BaseArgs and Env values are expanded against the host environment
// at parse time (via os.ExpandEnv). This keeps shipped manifests portable
// — the user's home path doesn't need to be hard-coded in the YAML.
func ParseManifest(r io.Reader) (*ServerManifest, error) {
	var m ServerManifest
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("yaml decode: %w", err)
	}
	var missing []string
	for i, a := range m.BaseArgs {
		expanded, miss := expandEnvCrossPlatform(a)
		m.BaseArgs[i] = expanded
		missing = append(missing, miss...)
	}
	for k, v := range m.Env {
		expanded, miss := expandEnvCrossPlatform(v)
		m.Env[k] = expanded
		for _, name := range miss {
			missing = append(missing, k+":"+name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("manifest references unresolved environment variable(s): %s (set them before invoking mcphub, or remove the ${...} reference from the manifest)",
			strings.Join(missing, ", "))
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// expandEnvCrossPlatform expands $VAR and ${VAR} tokens against the host
// environment. Returns the expanded string plus a list of variable
// names that were referenced but not set — callers can decide whether
// to treat empty expansion as an error or accept the empty value.
//
// Cross-platform niceness: ${HOME} on Windows (where HOME is typically
// unset) falls back to USERPROFILE, and vice-versa, so the same
// manifest works under bash, cmd.exe, and PowerShell without dual
// templating. Both unset → the name is reported as missing.
func expandEnvCrossPlatform(s string) (string, []string) {
	var missing []string
	expanded := os.Expand(s, func(name string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		if name == "HOME" {
			if v := os.Getenv("USERPROFILE"); v != "" {
				return v
			}
		}
		if name == "USERPROFILE" {
			if v := os.Getenv("HOME"); v != "" {
				return v
			}
		}
		missing = append(missing, name)
		return ""
	})
	return expanded, missing
}

// Validate checks required fields and enum values. Called automatically by ParseManifest.
func (m *ServerManifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("manifest: name is required")
	}
	if m.Kind != KindGlobal && m.Kind != KindWorkspaceScoped {
		return fmt.Errorf("manifest %s: kind must be %q or %q (got %q)", m.Name, KindGlobal, KindWorkspaceScoped, m.Kind)
	}
	if m.Transport != TransportNativeHTTP && m.Transport != TransportStdioBridge {
		return fmt.Errorf("manifest %s: transport must be %q or %q (got %q)", m.Name, TransportNativeHTTP, TransportStdioBridge, m.Transport)
	}
	if m.Command == "" {
		return fmt.Errorf("manifest %s: command is required", m.Name)
	}
	return nil
}

package config

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Kind enumerates daemon types. Only these two values are valid in manifest.kind.
const (
	KindGlobal           = "global"
	KindWorkspaceScoped  = "workspace-scoped"
)

// Transport enumerates how the server speaks MCP. Only these are valid.
const (
	TransportNativeHTTP  = "native-http"
	TransportStdioBridge = "stdio-bridge"
)

// ServerManifest is the parsed form of a `servers/<name>/manifest.yaml` file.
type ServerManifest struct {
	Name            string          `yaml:"name"`
	Kind            string          `yaml:"kind"`
	Transport       string          `yaml:"transport"`
	Command         string          `yaml:"command"`
	BaseArgs        []string        `yaml:"base_args"`
	BaseArgsTemplate []string       `yaml:"base_args_template"`
	Env             map[string]string `yaml:"env"`
	Daemons         []DaemonSpec    `yaml:"daemons"`
	Languages       []LanguageSpec  `yaml:"languages"`
	PortPool        *PortPool       `yaml:"port_pool"`
	IdleTimeoutMin  int             `yaml:"idle_timeout_min"`
	ClientBindings  []ClientBinding `yaml:"client_bindings"`
	WeeklyRefresh   bool            `yaml:"weekly_refresh"`
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
// Returns an error if required fields are missing or kind/transport values are unknown.
func ParseManifest(r io.Reader) (*ServerManifest, error) {
	var m ServerManifest
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("yaml decode: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
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

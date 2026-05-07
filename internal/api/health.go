package api

// HealthSnapshot is the canonical snapshot returned by GET /api/health.
// Owns the contract G3 (capability display) and G4 (Hub MCP routing)
// consume. Per spec docs/superpowers/specs/2026-05-07-g2-unified-health-endpoint-design.md.
//
// Probes and Capabilities are pointers so they're omitted from JSON when
// the caller didn't opt in via HealthOpts.IncludeProbes /
// IncludeCapabilities — keeps the cheap default response cheap.
type HealthSnapshot struct {
	SchemaVersion string               `json:"schema_version"`
	Hub           HubSection           `json:"hub"`
	Daemons       DaemonsSection       `json:"daemons"`
	Probes        *ProbesSection       `json:"probes,omitempty"`
	Capabilities  *CapabilitiesSection `json:"capabilities,omitempty"`
}

// HealthOpts toggles which expensive sections appear in the snapshot.
// IncludeCapabilities implies IncludeProbes (capability discovery
// requires a successful probe first).
type HealthOpts struct {
	IncludeProbes       bool
	IncludeCapabilities bool
	Refresh             bool // bust cache for included sections (rate-limited)
}

// HubSection describes hub-self info. Immutable per process lifetime;
// TTLMs is *int64 with nil meaning "never expires".
type HubSection struct {
	Version     string  `json:"version"`
	Commit      string  `json:"commit"`
	BuildDate   string  `json:"build_date"`
	StartedAt   string  `json:"started_at"`
	Lock        HubLock `json:"lock"`
	GeneratedAt int64   `json:"generated_at"`
	TTLMs       *int64  `json:"ttl_ms"`
}

type HubLock struct {
	PID  int `json:"pid"`
	Port int `json:"port"`
}

// DaemonsSection wraps the per-daemon process state.
type DaemonsSection struct {
	Items       []DaemonRow    `json:"items"`
	GeneratedAt int64          `json:"generated_at"`
	TTLMs       int64          `json:"ttl_ms"`
	Errors      []SectionError `json:"errors"`
}

type DaemonRow struct {
	Server        string  `json:"server"`
	Daemon        string  `json:"daemon"`
	PID           int     `json:"pid"`
	Port          int     `json:"port"`
	RAMBytes      uint64  `json:"ram_bytes"`
	UptimeSec     int64   `json:"uptime_sec"`
	State         string  `json:"state"` // "running" | "stopped" | "starting" | "failed"
	RestartCount  int     `json:"restart_count"`
	LastRestartAt *string `json:"last_restart_at"`
}

type ProbesSection struct {
	Items       []ProbeRow     `json:"items"`
	GeneratedAt int64          `json:"generated_at"`
	TTLMs       int64          `json:"ttl_ms"`
	Errors      []SectionError `json:"errors"`
}

type ProbeRow struct {
	Server    string `json:"server"`
	Daemon    string `json:"daemon"`
	OK        bool   `json:"ok"`
	ToolCount int    `json:"tool_count"`
	Err       string `json:"err"`
	Source    string `json:"source"` // "" | "proxy-synthetic"
}

type CapabilitiesSection struct {
	Items       []CapabilityRow `json:"items"`
	GeneratedAt int64           `json:"generated_at"`
	TTLMs       int64           `json:"ttl_ms"`
	Errors      []SectionError  `json:"errors"`
}

type CapabilityRow struct {
	Server    string               `json:"server"`
	Daemon    string               `json:"daemon"`
	Tools     CapabilitySubSection `json:"tools"`
	Prompts   CapabilitySubSection `json:"prompts"`
	Resources CapabilitySubSection `json:"resources"`
}

// CapabilitySubSection state vocabulary per spec:
//   - "ok"          — list returned, items populated
//   - "empty"       — list returned successfully, zero items
//   - "unsupported" — server responded method-not-found / capability not declared
//   - "error"       — request failed (timeout, parse, transport); Err populated
//   - "stale"       — last successful fetch older than 2× TTL, served best-effort
type CapabilitySubSection struct {
	State string           `json:"state"`
	Items []CapabilityItem `json:"items"`
	Err   string           `json:"err,omitempty"`
}

type CapabilityItem struct {
	Name      string `json:"name"`
	ID        string `json:"id"` // canonical: server/daemon/kind/name
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"` // tool|prompt|resource
}

// SectionError is one partial-failure entry. Scope is "wmic", "daemon:<name>",
// "probe:<server>/<daemon>", "tools:<server>/<daemon>", etc. — granular enough
// for the operator to locate the failure surface.
type SectionError struct {
	Scope string `json:"scope"`
	Err   string `json:"err"`
}

// capabilityID builds the canonical capability identifier per spec:
// {server}/{daemon}/{kind}/{name}. G4's Hub-MCP routing layer uses this
// exact form — do not introduce a parallel scheme.
func capabilityID(server, daemon, kind, name string) string {
	return server + "/" + daemon + "/" + kind + "/" + name
}

// HealthSnapshot returns the current snapshot. Phase 1 stub: returns
// SchemaVersion="1" + zero-valued Hub/Daemons + nil Probes/Capabilities
// regardless of opts. Wired in Phase 2+.
func (a *API) HealthSnapshot(opts HealthOpts) (HealthSnapshot, error) {
	return HealthSnapshot{
		SchemaVersion: "1",
		Hub:           HubSection{},
		Daemons:       DaemonsSection{Items: []DaemonRow{}, Errors: []SectionError{}},
	}, nil
}

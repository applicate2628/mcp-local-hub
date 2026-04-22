package config

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// PortRegistry is the parsed form of configs/ports.yaml — the central authority
// for which port each daemon uses. Conflicts between global ports and workspace
// pools are detected at parse time.
type PortRegistry struct {
	Global          []GlobalPortEntry    `yaml:"global"`
	WorkspaceScoped []WorkspacePoolEntry `yaml:"workspace_scoped"`
}

type GlobalPortEntry struct {
	Server string `yaml:"server"`
	Daemon string `yaml:"daemon"`
	Port   int    `yaml:"port"`
}

type WorkspacePoolEntry struct {
	Server    string `yaml:"server"`
	PoolStart int    `yaml:"pool_start"`
	PoolEnd   int    `yaml:"pool_end"`
}

// ParsePortRegistry reads YAML and returns a validated registry.
// Validation ensures no two global entries share a port and no global port falls
// inside any workspace pool range.
func ParsePortRegistry(r io.Reader) (*PortRegistry, error) {
	var reg PortRegistry
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&reg); err != nil {
		return nil, fmt.Errorf("port registry decode: %w", err)
	}
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	return &reg, nil
}

// Validate checks for conflicts:
// - two global entries with the same port
// - a global port that falls inside any workspace pool
// - overlapping workspace pools
func (r *PortRegistry) Validate() error {
	seen := map[int]string{}
	for _, g := range r.Global {
		if owner, ok := seen[g.Port]; ok {
			return fmt.Errorf("port %d conflict: %s and %s/%s", g.Port, owner, g.Server, g.Daemon)
		}
		seen[g.Port] = fmt.Sprintf("%s/%s", g.Server, g.Daemon)
	}
	for _, w := range r.WorkspaceScoped {
		if w.PoolStart > w.PoolEnd {
			return fmt.Errorf("server %s: pool_start %d > pool_end %d", w.Server, w.PoolStart, w.PoolEnd)
		}
		for _, g := range r.Global {
			if g.Port >= w.PoolStart && g.Port <= w.PoolEnd {
				return fmt.Errorf("global port %d (%s/%s) falls inside workspace pool %d-%d (%s)",
					g.Port, g.Server, g.Daemon, w.PoolStart, w.PoolEnd, w.Server)
			}
		}
	}
	for i := 0; i < len(r.WorkspaceScoped); i++ {
		a := r.WorkspaceScoped[i]
		for j := i + 1; j < len(r.WorkspaceScoped); j++ {
			b := r.WorkspaceScoped[j]
			if a.PoolStart <= b.PoolEnd && b.PoolStart <= a.PoolEnd {
				return fmt.Errorf("workspace pool %d-%d (%s) overlaps with %d-%d (%s)",
					a.PoolStart, a.PoolEnd, a.Server, b.PoolStart, b.PoolEnd, b.Server)
			}
		}
	}
	return nil
}

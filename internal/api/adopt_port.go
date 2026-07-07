package api

import (
	"bytes"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/servers"
)

const (
	adoptPortStart = 9300
	adoptPortEnd   = 9399
)

var collectEmbeddedManifestPortsFn = collectEmbeddedManifestPorts

func pickNextFreeAdoptPort() (int, error) {
	used := collectUsedAdoptPorts()
	for p := adoptPortStart; p <= adoptPortEnd; p++ {
		if used[p] {
			continue
		}
		if adoptPortBindable(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free adopted-server port in %d-%d range", adoptPortStart, adoptPortEnd)
}

func validateExplicitAdoptPort(port int) error {
	if port < adoptPortStart || port > adoptPortEnd {
		return fmt.Errorf("adopt port %d is outside the adopted-server port range %d-%d", port, adoptPortStart, adoptPortEnd)
	}
	if collectUsedAdoptPorts()[port] {
		return fmt.Errorf("adopt port %d is already in use by an existing manifest, manifest port pool, or supervisor intent", port)
	}
	if !adoptPortBindable(port) {
		return fmt.Errorf("adopt port %d is not bindable on 127.0.0.1; choose a free port in %d-%d", port, adoptPortStart, adoptPortEnd)
	}
	return nil
}

func collectUsedAdoptPorts() map[int]bool {
	used := map[int]bool{}
	collectDiskManifestPorts(defaultManifestDir(), used)
	collectEmbeddedManifestPortsFn(used)
	collectSupervisorIntentPorts(used)
	return used
}

func collectDiskManifestPorts(dir string, used map[int]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name(), "manifest.yaml"))
		if err != nil {
			continue
		}
		collectManifestPorts(data, used)
	}
}

func collectEmbeddedManifestPorts(used map[int]bool) {
	for _, name := range embeddedManifestNames() {
		data, err := fs.ReadFile(servers.Manifests, name+"/manifest.yaml")
		if err != nil {
			continue
		}
		collectManifestPorts(data, used)
	}
}

func collectSupervisorIntentPorts(used map[int]bool) {
	stateDir, err := daemonStateDirReadOnly()
	if err != nil {
		return
	}
	intent, err := ReadSupervisorIntent(joinStateFilePath(stateDir, supervisorIntentFileLeaf))
	if err != nil || intent == nil {
		return
	}
	for _, row := range intent.Daemons {
		if row.Port >= adoptPortStart && row.Port <= adoptPortEnd {
			used[row.Port] = true
		}
	}
}

func collectManifestPorts(data []byte, used map[int]bool) {
	m, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil {
		return
	}
	for _, daemon := range m.Daemons {
		if daemon.Port >= adoptPortStart && daemon.Port <= adoptPortEnd {
			used[daemon.Port] = true
		}
	}
	collectPortPool(m.PortPool, used)
	if m.DaemonTemplate != nil {
		collectPortPool(m.DaemonTemplate.PortPool, used)
	}
}

func collectPortPool(pool *config.PortPool, used map[int]bool) {
	if pool == nil {
		return
	}
	for p := adoptPortStart; p <= adoptPortEnd; p++ {
		if p >= pool.Start && p <= pool.End {
			used[p] = true
		}
	}
}

func adoptPortBindable(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

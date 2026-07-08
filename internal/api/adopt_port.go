package api

import (
	"bytes"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/servers"
)

const (
	adoptPortStart = 9300
	adoptPortEnd   = 9399
)

var (
	adoptManifestDirFn             = defaultManifestDir
	collectEmbeddedManifestPortsFn = collectEmbeddedManifestPorts
)

var (
	adoptPortLineRE      = regexp.MustCompile(`\bport:\s*([0-9]+)\b`)
	adoptPoolStartLineRE = regexp.MustCompile(`\bstart:\s*([0-9]+)\b`)
	adoptPoolEndLineRE   = regexp.MustCompile(`\bend:\s*([0-9]+)\b`)
)

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
	collectDiskManifestPorts(adoptManifestDirFn(), used)
	collectEmbeddedManifestPortsFn(used)
	collectSupervisorIntentPorts(used)
	collectConfiguredGUIPort(used)
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
			fmt.Fprintf(os.Stderr, "mcphub adopt port allocator: unreadable manifest %s: %v\n", filepath.Join(dir, entry.Name(), "manifest.yaml"), err)
			continue
		}
		collectManifestPorts(filepath.Join(dir, entry.Name(), "manifest.yaml"), data, used)
	}
}

func collectEmbeddedManifestPorts(used map[int]bool) {
	for _, name := range embeddedManifestNames() {
		data, err := fs.ReadFile(servers.Manifests, name+"/manifest.yaml")
		if err != nil {
			continue
		}
		collectManifestPorts(name+"/manifest.yaml", data, used)
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

func collectConfiguredGUIPort(used map[int]bool) {
	raw, err := NewAPI().SettingsGet("gui_server.port")
	if err != nil {
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return
	}
	markUsedAdoptPort(port, used)
}

func collectManifestPorts(source string, data []byte, used map[int]bool) {
	m, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil {
		count := collectManifestPortsTolerant(data, used)
		fmt.Fprintf(os.Stderr, "mcphub adopt port allocator: manifest %s did not parse (%v); reserved %d port(s) from raw scrape\n", source, err, count)
		return
	}
	for _, daemon := range m.Daemons {
		markUsedAdoptPort(daemon.Port, used)
	}
	collectPortPool(m.PortPool, used)
	if m.DaemonTemplate != nil {
		collectPortPool(m.DaemonTemplate.PortPool, used)
	}
}

func collectManifestPortsTolerant(data []byte, used map[int]bool) int {
	lines := strings.Split(string(data), "\n")
	count := 0
	for _, raw := range lines {
		line := stripAdoptPortComment(raw)
		if m := adoptPortLineRE.FindStringSubmatch(line); len(m) == 2 {
			if port, err := strconv.Atoi(m[1]); err == nil && markUsedAdoptPort(port, used) {
				count++
			}
		}
	}
	return count + collectManifestPortPoolsTolerant(lines, used)
}

func collectManifestPortPoolsTolerant(lines []string, used map[int]bool) int {
	count := 0
	inPool := false
	poolIndent := 0
	start, end := 0, 0
	flush := func() {
		if start > 0 && end > 0 {
			count += markUsedAdoptPortRange(start, end, used)
		}
		start, end = 0, 0
	}
	for _, raw := range lines {
		line := stripAdoptPortComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		trimmed := strings.TrimSpace(line)
		if inPool && indent <= poolIndent {
			flush()
			inPool = false
		}
		if strings.Contains(trimmed, "port_pool:") {
			if poolStart, poolEnd, ok := parseAdoptPortPoolBounds(trimmed); ok {
				count += markUsedAdoptPortRange(poolStart, poolEnd, used)
				continue
			}
			if strings.HasPrefix(trimmed, "port_pool:") {
				inPool = true
				poolIndent = indent
				start, end = 0, 0
			}
			continue
		}
		if !inPool {
			continue
		}
		if m := adoptPoolStartLineRE.FindStringSubmatch(trimmed); len(m) == 2 {
			start, _ = strconv.Atoi(m[1])
			continue
		}
		if m := adoptPoolEndLineRE.FindStringSubmatch(trimmed); len(m) == 2 {
			end, _ = strconv.Atoi(m[1])
		}
	}
	if inPool {
		flush()
	}
	return count
}

func parseAdoptPortPoolBounds(s string) (int, int, bool) {
	startMatch := adoptPoolStartLineRE.FindStringSubmatch(s)
	endMatch := adoptPoolEndLineRE.FindStringSubmatch(s)
	if len(startMatch) != 2 || len(endMatch) != 2 {
		return 0, 0, false
	}
	start, startErr := strconv.Atoi(startMatch[1])
	end, endErr := strconv.Atoi(endMatch[1])
	if startErr != nil || endErr != nil {
		return 0, 0, false
	}
	return start, end, true
}

func stripAdoptPortComment(line string) string {
	if before, _, ok := strings.Cut(line, "#"); ok {
		return before
	}
	return line
}

func markUsedAdoptPortRange(start, end int, used map[int]bool) int {
	if end < start {
		return 0
	}
	count := 0
	for p := start; p <= end; p++ {
		if markUsedAdoptPort(p, used) {
			count++
		}
	}
	return count
}

func markUsedAdoptPort(port int, used map[int]bool) bool {
	if port < adoptPortStart || port > adoptPortEnd {
		return false
	}
	if used[port] {
		return false
	}
	used[port] = true
	return true
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

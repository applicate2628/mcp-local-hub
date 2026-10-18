package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	adoptManifestDirFn             = CanonicalManifestDir
	collectEmbeddedManifestPortsFn = collectEmbeddedManifestPorts
)

var (
	adoptPortLineRE      = regexp.MustCompile(`\bport:\s*([0-9]+)\b`)
	adoptPoolStartLineRE = regexp.MustCompile(`\bstart:\s*([0-9]+)\b`)
	adoptPoolEndLineRE   = regexp.MustCompile(`\bend:\s*([0-9]+)\b`)
)

func pickNextFreeAdoptPort() (int, error) {
	used, err := collectUsedAdoptPortsResolved()
	if err != nil {
		return 0, err
	}
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
	used, err := collectUsedAdoptPortsResolved()
	if err != nil {
		return err
	}
	if used[port] {
		return fmt.Errorf("adopt port %d is already in use by an existing manifest, manifest port pool, or supervisor intent", port)
	}
	if !adoptPortBindable(port) {
		return fmt.Errorf("adopt port %d is not bindable on 127.0.0.1; choose a free port in %d-%d", port, adoptPortStart, adoptPortEnd)
	}
	return nil
}

func collectUsedAdoptPorts() map[int]bool {
	used, _ := collectUsedAdoptPortsResolved()
	return used
}

func collectUsedAdoptPortsResolved() (map[int]bool, error) {
	used := map[int]bool{}
	dir, err := adoptManifestDirFn()
	if err != nil {
		return nil, fmt.Errorf("adopt port allocation: %w", err)
	}
	collectDiskManifestPorts(dir, used)
	collectEmbeddedManifestPortsFn(used)
	collectSupervisorIntentPorts(used)
	collectConfiguredGUIPort(used)
	return used, nil
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

type adoptPortInventoryObservation struct {
	used    map[int]bool
	reads   []manifestSourceReadAttempt
	intent  *SupervisorIntentFile
	guiPort int
	cause   error
}

type adoptPortInventoryFailure struct {
	observation *adoptPortInventoryObservation
	cause       error
}

func (e *adoptPortInventoryFailure) Error() string { return "GRAPHIFY_PORT_INVENTORY_UNAVAILABLE" }
func (e *adoptPortInventoryFailure) Unwrap() error { return e.cause }

// Definition uses the same inventory and port-range owners with observed
// acquisition. Ordinary allocation retains its existing tolerant policy above.
func collectUsedAdoptPortsObserved() (observation *adoptPortInventoryObservation, resultErr error) {
	observation = &adoptPortInventoryObservation{used: map[int]bool{}}
	defer func() {
		if resultErr != nil {
			observation.cause = resultErr
			resultErr = &adoptPortInventoryFailure{observation: observation, cause: resultErr}
		}
	}()
	dir, err := adoptManifestDirFn()
	if err != nil {
		return observation, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return observation, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name(), "manifest.yaml")
		read := readManifestSourceAttempt("disk", path)
		observation.reads = append(observation.reads, read)
		if err := errors.Join(read.readError, read.closeError); err != nil {
			return observation, err
		}
		if err := collectManifestPortsObserved(read.bytes, observation.used); err != nil {
			return observation, err
		}
	}
	embeddedNames, err := embeddedManifestNamesObserved()
	if err != nil {
		return observation, err
	}
	for _, name := range embeddedNames {
		read := readManifestSourceAttempt("embedded", name+"/manifest.yaml")
		observation.reads = append(observation.reads, read)
		if err := errors.Join(read.readError, read.closeError); err != nil {
			return observation, err
		}
		if err := collectManifestPortsObserved(read.bytes, observation.used); err != nil {
			return observation, err
		}
	}
	stateDir, err := daemonStateDirReadOnly()
	if err != nil {
		return observation, err
	}
	intent, err := ReadSupervisorIntent(joinStateFilePath(stateDir, supervisorIntentFileLeaf))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return observation, err
	}
	observation.intent = intent
	if intent != nil {
		for _, row := range intent.Daemons {
			markUsedAdoptPort(row.Port, observation.used)
		}
	}
	rawPort, err := NewAPI().SettingsGet("gui_server.port")
	if err != nil {
		return observation, err
	}
	observation.guiPort, err = strconv.Atoi(strings.TrimSpace(rawPort))
	if err != nil || observation.guiPort < 0 || observation.guiPort > 65535 {
		return observation, errors.Join(err, errors.New("GRAPHIFY_PORT_SETTINGS_INVALID"))
	}
	markUsedAdoptPort(observation.guiPort, observation.used)
	return observation, nil
}

func collectManifestPortsObserved(data []byte, used map[int]bool) error {
	m, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil {
		return err
	}
	for _, daemon := range m.Daemons {
		markUsedAdoptPort(daemon.Port, used)
	}
	collectPortPool(m.PortPool, used)
	if m.DaemonTemplate != nil {
		collectPortPool(m.DaemonTemplate.PortPool, used)
	}
	return nil
}

func (o *adoptPortInventoryObservation) fingerprint() (string, error) {
	if o == nil || o.cause != nil || o.used == nil {
		return "", errors.New("GRAPHIFY_PORT_INVENTORY_UNQUALIFIED")
	}
	material, err := json.Marshal(struct {
		Reads []struct {
			Source  string
			Content string
		}
		Intent  *SupervisorIntentFile
		GUIPort int
	}{Reads: func() []struct {
		Source  string
		Content string
	} {
		var rows []struct {
			Source  string
			Content string
		}
		for _, read := range o.reads {
			rows = append(rows, struct {
				Source  string
				Content string
			}{read.path, ManifestHashContent(read.bytes)})
		}
		return rows
	}(), Intent: o.intent, GUIPort: o.guiPort})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(material)
	return hex.EncodeToString(digest[:]), nil
}

type adoptPortProbeObservation struct {
	port           int
	listener       net.Listener
	acquireError   error
	closeAttempted bool
	closeDefined   bool
	closeError     error
}

type adoptPortProbeFailure struct {
	observation adoptPortProbeObservation
	cause       error
}

func (e *adoptPortProbeFailure) Error() string { return "GRAPHIFY_PORT_PROBE_UNQUALIFIED" }
func (e *adoptPortProbeFailure) Unwrap() error { return e.cause }

func adoptPortBindableObserved(port int) (observation adoptPortProbeObservation, resultErr error) {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	return finishAdoptPortProbeObserved(port, listener, err)
}

func finishAdoptPortProbeObserved(port int, listener net.Listener, acquireErr error) (observation adoptPortProbeObservation, resultErr error) {
	observation.port, observation.acquireError = port, acquireErr
	if listener == nil {
		if acquireErr == nil {
			acquireErr = errors.New("port probe original listener unavailable")
		}
		return observation, &adoptPortProbeFailure{observation: observation, cause: acquireErr}
	}
	observation.listener = listener
	observation.closeAttempted = true
	observation.closeError = listener.Close()
	observation.closeDefined = true
	if cause := errors.Join(acquireErr, observation.closeError); cause != nil {
		return observation, &adoptPortProbeFailure{observation: observation, cause: cause}
	}
	return observation, nil
}

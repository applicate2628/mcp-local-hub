// hub_mcp_instance.go — Phase 2 Task 2.3 (G4 unified hub MCP).
//
// Persistent hub instance id + endpoint state file
// (<state-dir>/hub-mcp.endpoint.json). The instance_id is generated
// ONCE on first start (when the endpoint file is created) and
// persisted across hub restarts. Operators only re-install client
// configs on explicit rotation events:
//
//   - Token compromise → `mcphub hub-mcp regenerate-token --client X`
//     (Phase 5 CLI; rotates one client's token without touching
//     instance_id).
//   - Instance compromise → `mcphub hub-mcp regenerate-instance-id`
//     (Phase 5 CLI; invalidates ALL existing clients in one shot via
//     RotateHubInstanceID below).
//
// The auth gate (Phase 4 HTTP handler) still rejects requests carrying
// a mismatched X-Mcphub-Instance-Id, but the routine restart path
// doesn't require operator action because the instance_id survives the
// restart. This resolves the v2 contradiction between "install once"
// and "every restart needs reinstall" (codex r3 security F-S1 closure).
//
// `--reset-port` (ResetHubPort) clears Port WITHOUT touching
// instance_id. The recovery workflow for a pre-bind credential-leak
// is `--reset-port` + `regenerate-token` + `regenerate-instance-id`
// + `mcphub install <client>` per the spec's burn-down sequence.
//
// State-mutating paths serialize on hub-mcp.lock via acquireHubMcpLock.
//
// Spec: §"Hub instance ID — persistent across restarts".
// Plan: Task 2.3.

package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// HubEndpoint is the on-disk shape of hub-mcp.endpoint.json. Persistent
// across restarts; rotated only by explicit RotateHubInstanceID. Port
// rotates only via ResetHubPort (or via callers passing a new
// ephemeralPort to EnsureHubEndpoint after a listener allocation).
//
// StartedAt is an RFC3339Nano UTC timestamp written on every Ensure /
// Rotate call — it records when the CURRENT hub process started,
// independent of when the file was first created. The instance_id is
// the long-lived identity; StartedAt is operational metadata for
// `mcphub hub-mcp status`.
type HubEndpoint struct {
	Port       int    `json:"port"`
	InstanceID string `json:"instance_id"`
	PID        int    `json:"pid"`
	StartedAt  string `json:"started_at"` // RFC3339Nano UTC
}

// EnsureHubEndpoint loads or generates hub-mcp.endpoint.json under
// hub-mcp.lock. Returns the post-write struct.
//
// Caller convention:
//   - ephemeralPort=0 means "preserve the loaded Port (or leave at 0
//     if there isn't one yet)". Useful on the very first call before
//     the listener has bound, or after ResetHubPort cleared Port.
//   - ephemeralPort>0 means "overwrite Port with this value". Callers
//     pass listener.Addr().(*net.TCPAddr).Port AFTER the OS assigned
//     a port for an ephemeral bind.
//
// PID is always overwritten (the current process owns the file). The
// instance_id is generated only when the loaded record is empty (first
// start). Existing records preserve instance_id across every call.
func EnsureHubEndpoint(ephemeralPort, pid int) (HubEndpoint, error) {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return HubEndpoint{}, err
	}
	defer func() { _ = lk.Unlock() }()
	return ensureHubEndpointLocked(ephemeralPort, pid)
}

// ensureHubEndpointLocked is the in-flock half. Extracted so tests
// + the rotate path can compose without re-locking. The caller MUST
// already hold hub-mcp.lock.
func ensureHubEndpointLocked(ephemeralPort, pid int) (HubEndpoint, error) {
	ep, err := loadHubEndpointLocked()
	if err != nil && !isMissingEndpointErr(err) {
		// A parse / DACL failure on an EXISTING file is not silently
		// regenerated — the spec calls for refusing to proceed so an
		// operator can investigate (§"Bind ordering" step 4).
		return HubEndpoint{}, err
	}
	if ep.InstanceID == "" {
		fresh, gerr := generateHexToken()
		if gerr != nil {
			return HubEndpoint{}, gerr
		}
		ep.InstanceID = fresh
	}
	if ephemeralPort > 0 {
		ep.Port = ephemeralPort
	}
	ep.PID = pid
	ep.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)

	payload, perr := json.Marshal(ep)
	if perr != nil {
		return HubEndpoint{}, fmt.Errorf("marshal hub-mcp endpoint: %w", perr)
	}
	if werr := writeHubMcpStateFile(hubMcpEndpointFileLeaf, payload); werr != nil {
		return HubEndpoint{}, werr
	}
	return ep, nil
}

// LoadHubEndpoint reads <state-dir>/hub-mcp.endpoint.json WITHOUT
// mutating or generating. Used by `mcphub hub-mcp status` and the
// install reconciler (Phase 5). Returns an error if the file is
// absent OR fails the load-time DACL/owner/mode gate.
//
// LoadHubEndpoint does NOT acquire hub-mcp.lock. Callers that need
// strict read-modify-write semantics should call EnsureHubEndpoint
// (which holds the lock internally). For status / inspection it's
// safe to read without the lock; the worst case is a torn read across
// an atomic rename, and the unmarshal would surface that as a parse
// error which the CLI can report.
func LoadHubEndpoint() (HubEndpoint, error) {
	return loadHubEndpointLocked()
}

// loadHubEndpointLocked is the shared read helper. Reads + parses but
// does NOT acquire hub-mcp.lock — callers must lock when composing
// with a subsequent write. Returns a zero struct + error when the
// file is missing OR fails the verify; a successful read with an
// empty InstanceID is impossible because we only persist after
// generating one.
func loadHubEndpointLocked() (HubEndpoint, error) {
	raw, err := readHubMcpStateFile(hubMcpEndpointFileLeaf)
	if err != nil {
		return HubEndpoint{}, err
	}
	var ep HubEndpoint
	if uerr := json.Unmarshal(raw, &ep); uerr != nil {
		return HubEndpoint{}, fmt.Errorf("hub-mcp.endpoint.json corrupt: %w", uerr)
	}
	return ep, nil
}

// RotateHubInstanceID generates a fresh instance_id and rewrites the
// endpoint file under hub-mcp.lock. Triggered by
// `mcphub hub-mcp regenerate-instance-id` (Phase 5). Every client
// whose stored instance_id no longer matches gets 401 from the next
// bind and must be re-installed.
//
// PID is overwritten with the current process PID; StartedAt is
// refreshed to the rotate moment so `mcphub hub-mcp status` shows
// the burn-down timestamp.
func RotateHubInstanceID() (HubEndpoint, error) {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return HubEndpoint{}, err
	}
	defer func() { _ = lk.Unlock() }()

	ep, err := loadHubEndpointLocked()
	if err != nil && !isMissingEndpointErr(err) {
		return HubEndpoint{}, err
	}
	fresh, gerr := generateHexToken()
	if gerr != nil {
		return HubEndpoint{}, gerr
	}
	ep.InstanceID = fresh
	ep.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)

	payload, perr := json.Marshal(ep)
	if perr != nil {
		return HubEndpoint{}, fmt.Errorf("marshal rotated hub-mcp endpoint: %w", perr)
	}
	if werr := writeHubMcpStateFile(hubMcpEndpointFileLeaf, payload); werr != nil {
		return HubEndpoint{}, werr
	}
	return ep, nil
}

// ResetHubPort clears the persisted Port (sets it to 0) without
// touching InstanceID. Triggered by `mcphub gui --reset-port` (Phase 5
// CLI). The next listener-factory call will pass ephemeralPort=0 to
// the OS, get a fresh allocation, then call EnsureHubEndpoint with
// the new port to record it.
//
// Holds hub-mcp.lock for the read-modify-write so a concurrent
// EnsureHubEndpoint cannot overwrite the cleared Port mid-flight.
func ResetHubPort() error {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return err
	}
	defer func() { _ = lk.Unlock() }()

	ep, err := loadHubEndpointLocked()
	if err != nil {
		// If the file doesn't exist, there's nothing to reset. Return
		// the underlying error so the CLI can surface "hub has not
		// run yet" if appropriate.
		return err
	}
	ep.Port = 0
	ep.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)

	payload, perr := json.Marshal(ep)
	if perr != nil {
		return fmt.Errorf("marshal hub-mcp endpoint on reset: %w", perr)
	}
	return writeHubMcpStateFile(hubMcpEndpointFileLeaf, payload)
}

// generateHexToken returns a 64-lower-hex string from crypto/rand 32
// bytes. Shared by EnsureHubEndpoint (instance_id), RotateHubInstanceID,
// and EnsureHubTokens (per-client tokens, Task 2.4). One place to
// audit randomness sourcing.
func generateHexToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// isMissingEndpointErr decides whether a load failure is "the file
// hasn't been written yet" (acceptable; EnsureHubEndpoint generates a
// fresh record) versus "the file exists but is corrupt or has bad
// DACL" (refuse to silently regenerate per spec §"Bind ordering"
// step 4).
//
// Routing through isHubMcpStateMissingErr keeps the POSIX
// (os.ErrNotExist) + Windows (NTStatus / Win32 errno) detection logic
// in one place — see hub_mcp_state_{posix,windows}.go.
func isMissingEndpointErr(err error) bool {
	return isHubMcpStateMissingErr(err)
}

package api

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/process"
	"mcp-local-hub/servers"
)

// AttachClientRequest selects exactly one existing managed route and client.
type AttachClientRequest struct{ Server, Daemon, Client, ManifestSource string }

// AttachClientPlan is a read-only, redacted preview. F2 must reobserve its
// private identity under the selected client's existing config lock.
type AttachClientPlan struct {
	State              string `json:"state,omitempty"`
	ReceiptID          string `json:"receipt_id,omitempty"`
	Server             string `json:"server"`
	Daemon             string `json:"daemon"`
	Client             string `json:"client"`
	EntryKey           string `json:"entry_key"`
	Endpoint           string `json:"endpoint"`
	BeforeTransport    string `json:"before_transport"`
	AfterTransport     string `json:"after_transport"`
	TargetBytesSHA256  string `json:"target_bytes_sha256"`
	EntryTokenSHA256   string `json:"entry_token_sha256"`
	ManifestSHA256     string `json:"manifest_sha256,omitempty"`
	ManifestProvenance string `json:"manifest_provenance,omitempty"`
	frozen             attachPlanIdentity
	restore            *attachManifestCandidate
	restoreSource      string
}

// Formatting a private plan must not reveal its retained raw YAML or optional
// caller path through a diagnostic that prints the whole struct.
func (p *AttachClientPlan) String() string {
	if p == nil {
		return "<nil attach-client plan>"
	}
	return fmt.Sprintf("attach-client plan state=%q server=%q daemon=%q client=%q entry=%q", p.State, p.Server, p.Daemon, p.Client, p.EntryKey)
}

func (p *AttachClientPlan) GoString() string { return p.String() }

type attachPlanIdentity struct {
	manifestHash     string
	intentGeneration uint64
	owner            SupervisorLockOwner
	taskName         string
	command          string
	args             []string
	childPID         int
	childStartedAt   time.Time
	port             int
	targetPath       string
}

// AttachPlanError carries a stable no-write refusal code without echoing raw
// command lines, configuration paths, environment, or client secrets.
type AttachPlanError struct {
	Code string
}

func (e *AttachPlanError) Error() string {
	return "attach-client plan " + e.Code
}

type attachObserved struct {
	manifest       *config.ServerManifest
	manifestHash   string
	intent         *SupervisorIntentFile
	owner          SupervisorLockOwner
	rows           []DaemonStatus
	childStartedAt time.Time
	listenerPID    int
	readiness      MCPReadinessResult
	origin         clients.SelectedEntryOriginV1
}

// admitAttachObserved is one fail-closed target gate; no branch writes state.
func admitAttachObserved(req AttachClientRequest, o attachObserved) (*AttachClientPlan, error) {
	deny := func(code string) (*AttachClientPlan, error) { return nil, &AttachPlanError{Code: code} }
	if req.Server == "" || req.Daemon == "" || req.Client != "mimocode" {
		return deny("unsupported-selection")
	}
	if o.manifest == nil || o.manifest.Name != req.Server || o.manifestHash == "" {
		return deny("manifest-mismatch")
	}
	if o.manifest.Kind != config.KindGlobal || o.manifest.Transport == config.TransportRemoteHTTP {
		return deny("manifest-route-unsupported")
	}
	if o.intent == nil || o.owner.PID <= 0 || o.owner.StartedAt == "" {
		return deny("supervisor-unverified")
	}
	if _, err := time.Parse(time.RFC3339Nano, o.owner.StartedAt); err != nil {
		return deny("supervisor-unverified")
	}
	var manifestPort int
	for _, d := range o.manifest.Daemons {
		if d.Name == req.Daemon {
			if manifestPort != 0 || d.Port <= 0 {
				return deny("manifest-ambiguous")
			}
			manifestPort = d.Port
		}
	}
	if manifestPort <= 0 {
		return deny("manifest-route-unsupported")
	}
	selectedBindingCount := 0
	for _, b := range o.manifest.ClientBindings {
		if b.Client != req.Client {
			continue
		}
		selectedBindingCount++
		if b.Daemon != req.Daemon || (b.URLPath != "" && b.URLPath != "/mcp") {
			return deny("unsupported-route")
		}
	}
	if selectedBindingCount > 1 {
		return deny("manifest-ambiguous")
	}
	intent, intentCode := selectAttachIntent(o.intent, req)
	if intentCode != "" {
		return deny(intentCode)
	}
	if intent.ManifestHash != o.manifestHash || intent.Port != manifestPort {
		return deny("intent-mismatch")
	}
	// The route must be exactly the manifest-backed generic descriptor, not a
	// proxy, maintenance timer, inferred task-name row, or arbitrary busy port.
	if port, ok := EffectiveDaemonPort(*intent); !ok || port != manifestPort {
		return deny("intent-port-mismatch")
	}
	var row *DaemonStatus
	for i := range o.rows {
		r := &o.rows[i]
		if r.TaskName != intent.TaskName && !(r.Server == req.Server && r.Daemon == req.Daemon) {
			continue
		}
		if row != nil {
			return deny("status-duplicate")
		}
		row = r
	}
	if row == nil || row.IsMaintenance || row.Workspace != "" || row.State != "Running" || row.Server != req.Server || row.Daemon != req.Daemon || row.TaskName != intent.TaskName || row.CommandInternal != intent.Command || !reflect.DeepEqual(row.ArgsInternal, intent.Args) || row.Port != manifestPort || row.PID <= 0 {
		return deny("status-mismatch")
	}
	started, err := time.Parse(time.RFC3339Nano, row.StartedAtInternal)
	if err != nil || o.childStartedAt.IsZero() || !started.Equal(o.childStartedAt) {
		return deny("child-generation-mismatch")
	}
	if o.listenerPID <= 0 || o.listenerPID != row.PID {
		return deny("listener-owner-mismatch")
	}
	if o.readiness.State != MCPReadinessReady {
		return deny("mcp-unready")
	}
	if o.origin.Version != 1 || o.origin.Verdict != clients.SelectedEntryOriginTargetOwnedDirect || o.origin.ClientID != req.Client || o.origin.EntryName != req.Server || o.origin.Scope != "known-global" || o.origin.TargetPathInternal == "" || o.origin.TargetBytesSHA256 == "" || o.origin.EntryTokenSHA256 == "" {
		return deny("client-origin-" + string(o.origin.Verdict))
	}
	return &AttachClientPlan{
		Server: req.Server, Daemon: req.Daemon, Client: req.Client,
		EntryKey:        req.Server,
		Endpoint:        fmt.Sprintf("http://127.0.0.1:%d/mcp", manifestPort),
		BeforeTransport: "direct", AfterTransport: "http",
		TargetBytesSHA256: o.origin.TargetBytesSHA256, EntryTokenSHA256: o.origin.EntryTokenSHA256,
		frozen: attachPlanIdentity{manifestHash: o.manifestHash, intentGeneration: o.intent.IntentGeneration,
			owner: o.owner, taskName: intent.TaskName, command: intent.Command,
			args: append([]string(nil), intent.Args...), childPID: row.PID,
			childStartedAt: o.childStartedAt, port: manifestPort, targetPath: o.origin.TargetPathInternal},
	}, nil
}

// selectAttachIntent is the sole row selector for pre-probe image proof and
// final admission. Command is persisted launch truth, not an ambient PATH name.
func selectAttachIntent(file *SupervisorIntentFile, req AttachClientRequest) (*SupervisorDaemon, string) {
	if file == nil {
		return nil, "intent-unavailable"
	}
	var selected *SupervisorDaemon
	for i := range file.Daemons {
		d := &file.Daemons[i]
		server, daemon, ok := DescriptorServerDaemon(*d)
		if d.Server != req.Server && server != req.Server && d.TaskName != `\mcp-local-hub-`+req.Server+`-`+req.Daemon {
			continue
		}
		if !ok || server != req.Server || daemon != req.Daemon || !DescriptorHasGlobalDaemonArgv(*d) || !attachStrictArgv(d.Args, req) {
			return nil, "intent-corrupt"
		}
		if selected != nil {
			return nil, "intent-duplicate"
		}
		selected = d
	}
	if selected == nil || selected.TaskName == "" || !filepath.IsAbs(selected.Command) {
		return nil, "intent-mismatch"
	}
	return selected, ""
}

func attachStrictArgv(args []string, req AttachClientRequest) bool {
	if len(args) < 5 || args[0] != "daemon" {
		return false
	}
	serverCount, daemonCount := 0, 0
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--server":
			if i+1 >= len(args) || args[i+1] != req.Server {
				return false
			}
			serverCount++
			i++
		case "--daemon":
			if i+1 >= len(args) || args[i+1] != req.Daemon {
				return false
			}
			daemonCount++
			i++
		}
	}
	return serverCount == 1 && daemonCount == 1
}

// BuildAttachClientPlan is internal current-user dry-run admission. It neither
// obtains a mutation lease nor opens any writer; F2 must revalidate after its
// bounded config-lock wait and may not treat this point-in-time plan as authority.
func (a *API) BuildAttachClientPlan(ctx context.Context, req AttachClientRequest) (*AttachClientPlan, error) {
	return a.buildAttachClientPlan(ctx, req, nil)
}

// An expected origin is used only for internal final revalidation: F2 rechecks
// the selected direct source under its config lock; after writing HTTP, F2
// independently checks the physical postimage. The live daemon/route checks
// here still run in both cases.
func (a *API) buildAttachClientPlan(ctx context.Context, req AttachClientRequest, expectedOrigin *clients.SelectedEntryOriginV1) (*AttachClientPlan, error) {
	if ctx == nil {
		return nil, &AttachPlanError{Code: "missing-context"}
	}
	foreground, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if req.Server == "" || req.Daemon == "" || req.Client != "mimocode" {
		return nil, &AttachPlanError{Code: "unsupported-selection"}
	}
	var restore *attachManifestCandidate
	raw, manifest, hash, err := a.readAttachManifest(req.Server)
	if err != nil {
		var refusal *AttachPlanError
		if !errors.As(err, &refusal) || refusal.Code != "manifest-absent" {
			return nil, err
		}
		candidate, resolveErr := a.resolveMissingAttachManifest(req)
		if resolveErr != nil {
			return nil, resolveErr
		}
		raw, hash = candidate.raw, candidate.hash
		manifest, err = parseManifestForName(req.Server, []byte(raw))
		if err != nil {
			return nil, &AttachPlanError{Code: "manifest-candidate-mismatch"}
		}
		restore = candidate
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return nil, &AttachPlanError{Code: "state-unavailable"}
	}
	owner, err := ReadSupervisorLockOwner(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		return nil, &AttachPlanError{Code: "supervisor-unverified"}
	}
	intent, err := ReadSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		return nil, &AttachPlanError{Code: "intent-unavailable"}
	}
	selectedIntent, intentCode := selectAttachIntent(intent, req)
	if intentCode != "" {
		return nil, &AttachPlanError{Code: intentCode}
	}
	rows, err := dialSupervisorIPCStatusForOwner(foreground, stateDir, owner)
	if err != nil {
		return nil, &AttachPlanError{Code: "status-unverified"}
	}
	var childPID int
	var childStartedRaw string
	for _, row := range rows {
		if row.Server == req.Server && row.Daemon == req.Daemon {
			if childPID != 0 {
				return nil, &AttachPlanError{Code: "status-duplicate"}
			}
			childPID = row.PID
			childStartedRaw = row.StartedAtInternal
		}
	}
	if childPID <= 0 {
		return nil, &AttachPlanError{Code: "status-mismatch"}
	}
	childStart, err := time.Parse(time.RFC3339Nano, childStartedRaw)
	if err != nil || foreground.Err() != nil {
		return nil, &AttachPlanError{Code: "child-generation-unverified"}
	}
	// The process owner compares this persisted launch image and kernel start
	// through one Windows handle. A 1ns tolerance makes the RFC3339Nano status
	// timestamp an exact generation check, not the older liveness grace window.
	proof := process.PIDIdentityProof{PID: childPID, ExecutablePath: selectedIntent.Command,
		StartedAt: childStartedRaw, StartTolerance: time.Nanosecond}
	if err := process.VerifyPIDIdentity(proof); err != nil || foreground.Err() != nil {
		return nil, &AttachPlanError{Code: "child-identity-mismatch"}
	}
	var port int
	for _, d := range manifest.Daemons {
		if d.Name == req.Daemon {
			port = d.Port
			break
		}
	}
	if port <= 0 {
		return nil, &AttachPlanError{Code: "manifest-route-unsupported"}
	}
	listenerPID, found, err := LoopbackPortOwnerPIDContext(foreground, port)
	if err != nil || !found || listenerPID <= 0 || foreground.Err() != nil {
		return nil, &AttachPlanError{Code: "listener-owner-unverified"}
	}
	var reader clients.SelectedEntryOriginReader
	var origin clients.SelectedEntryOriginV1
	var historicalOrigin clients.SelectedEntryOriginV1
	var selectedClient clients.Client
	var committed *attachClientReceiptV1
	var receiptLeaf string
	if expectedOrigin == nil {
		client, err := clients.NewMimoCode()
		if err != nil {
			return nil, &AttachPlanError{Code: "client-origin-unavailable"}
		}
		selectedClient = client
		var ok bool
		reader, ok = client.(clients.SelectedEntryOriginReader)
		if !ok {
			return nil, &AttachPlanError{Code: "client-origin-unsupported"}
		}
		lookup := &AttachClientPlan{Server: req.Server, Daemon: req.Daemon, Client: req.Client, EntryKey: req.Server}
		lookup.frozen.targetPath = client.ConfigPath()
		var snapshotLeaf string
		receiptLeaf, snapshotLeaf = attachReceiptLeaves(lookup)
		committed, err = attachReadReceipt(receiptLeaf)
		if err != nil && !isHubMcpStateMissingErr(err) {
			return nil, &AttachPlanError{Code: "receipt-unverified"}
		}
		if committed != nil && committed.State != "rolled-back" {
			id, idErr := hex.DecodeString(committed.ID)
			if committed.State != "committed" {
				return nil, &AttachPlanError{Code: "receipt-recovery-required"}
			}
			if idErr != nil || len(id) != 16 || committed.Server != req.Server || committed.Daemon != req.Daemon ||
				committed.Client != req.Client || committed.EntryKey != req.Server || committed.TargetPath != client.ConfigPath() ||
				!attachSnapshotLeafMatches(committed.SnapshotLeaf, snapshotLeaf, committed.ID) ||
				committed.BeforeSHA256 == "" || committed.BeforeTokenSHA256 == "" ||
				committed.AfterSHA256 == "" || committed.AfterTokenSHA256 == "" ||
				committed.ManifestSHA256 != hash || committed.Owner != owner ||
				committed.TaskName != selectedIntent.TaskName || committed.ChildPID != childPID ||
				!committed.ChildStartedAt.Equal(childStart) || committed.Port != port ||
				committed.Endpoint != fmt.Sprintf("http://127.0.0.1:%d/mcp", port) {
				return nil, &AttachPlanError{Code: "receipt-identity-drift"}
			}
			snapshot, snapshotErr := readHubMcpStateFile(committed.SnapshotLeaf)
			if snapshotErr != nil || attachHash(snapshot) != committed.BeforeSHA256 {
				return nil, &AttachPlanError{Code: "receipt-snapshot-unverified"}
			}
			token, tokenErr := clients.SelectedEntryPhysicalTokenSHA256(client, snapshot, req.Server)
			if tokenErr != nil || token != committed.BeforeTokenSHA256 {
				return nil, &AttachPlanError{Code: "receipt-snapshot-unverified"}
			}
			historicalOrigin = clients.SelectedEntryOriginV1{Version: 1, Verdict: clients.SelectedEntryOriginTargetOwnedDirect,
				ClientID: req.Client, EntryName: req.Server, Scope: "known-global",
				TargetPathInternal: client.ConfigPath(), TargetBytesSHA256: committed.BeforeSHA256,
				EntryTokenSHA256: committed.BeforeTokenSHA256}
			origin, err = attachCommittedHTTPProof(foreground, client, reader, req, committed)
			if err != nil {
				return nil, err
			}
		} else {
			committed = nil
			origin = reader.ReadSelectedEntryOrigin(foreground, req.Server)
		}
	} else {
		origin = *expectedOrigin
	}
	if committed == nil {
		if code := attachOriginPlanCode(origin, false); code != "" {
			return nil, &AttachPlanError{Code: code}
		}
	}
	readiness := ProbeMCPReadinessContext(foreground, port)
	// The probe owns a separately bounded synchronous session DELETE. A caller
	// cancellation during that cleanup cannot turn its prior ready result into
	// successful dry-run admission.
	if foreground.Err() != nil {
		return nil, &AttachPlanError{Code: "admission-timeout"}
	}
	if readiness.State != MCPReadinessReady {
		return nil, &AttachPlanError{Code: "mcp-unready"}
	}
	lastOrigin := origin
	if committed != nil {
		lastOrigin, err = attachCommittedHTTPProof(foreground, selectedClient, reader, req, committed)
		if err != nil {
			return nil, err
		}
	} else if reader != nil {
		lastOrigin = reader.ReadSelectedEntryOrigin(foreground, req.Server)
	}
	if foreground.Err() != nil || lastOrigin.Verdict != origin.Verdict || lastOrigin.TargetPathInternal != origin.TargetPathInternal || lastOrigin.TargetBytesSHA256 != origin.TargetBytesSHA256 || lastOrigin.EntryTokenSHA256 != origin.EntryTokenSHA256 {
		return nil, &AttachPlanError{Code: "client-origin-drift"}
	}
	lastOwner, err := ReadSupervisorLockOwner(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil || lastOwner != owner {
		return nil, &AttachPlanError{Code: "supervisor-drift"}
	}
	lastIntent, err := ReadSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil || !reflect.DeepEqual(lastIntent, intent) {
		return nil, &AttachPlanError{Code: "intent-drift"}
	}
	var lastRaw, lastHash string
	if restore != nil {
		lastCandidate, resolveErr := a.resolveMissingAttachManifest(req)
		if resolveErr != nil || !sameAttachManifestCandidate(lastCandidate, restore) || lastCandidate.raw != raw || lastCandidate.hash != hash {
			return nil, &AttachPlanError{Code: "manifest-source-drift"}
		}
		lastRaw, lastHash = lastCandidate.raw, lastCandidate.hash
	} else {
		lastRaw, _, lastHash, err = a.readAttachManifest(req.Server)
	}
	if err != nil || lastHash != hash || lastRaw != raw {
		return nil, &AttachPlanError{Code: "manifest-drift"}
	}
	lastRows, err := dialSupervisorIPCStatusForOwner(foreground, stateDir, owner)
	if err != nil || !sameAttachRows(rows, lastRows) {
		return nil, &AttachPlanError{Code: "status-drift"}
	}
	if err := process.VerifyPIDIdentity(proof); err != nil || foreground.Err() != nil {
		return nil, &AttachPlanError{Code: "child-identity-drift"}
	}
	lastListenerPID, found, err := LoopbackPortOwnerPIDContext(foreground, port)
	if err != nil || !found || lastListenerPID != listenerPID || foreground.Err() != nil {
		return nil, &AttachPlanError{Code: "listener-owner-drift"}
	}
	admittedOrigin := lastOrigin
	if committed != nil {
		admittedOrigin = historicalOrigin
	}
	plan, err := admitAttachObserved(req, attachObserved{manifest: manifest, manifestHash: hash,
		intent: intent, owner: owner, rows: lastRows, childStartedAt: childStart,
		listenerPID: lastListenerPID, readiness: readiness, origin: admittedOrigin})
	if err != nil {
		return nil, err
	}
	if restore != nil {
		if committed != nil {
			return nil, &AttachPlanError{Code: "receipt-recovery-required"}
		}
		plan.State = "would-restore-and-attach"
		plan.ManifestSHA256, plan.ManifestProvenance = restore.hash, restore.provenance
		plan.restore, plan.restoreSource = restore, req.ManifestSource
	}
	if committed == nil {
		return plan, err
	}
	lastReceipt, err := attachReadReceipt(receiptLeaf)
	if err != nil || !reflect.DeepEqual(lastReceipt, committed) || foreground.Err() != nil {
		return nil, &AttachPlanError{Code: "receipt-drift"}
	}
	plan.State, plan.ReceiptID = "already-committed", committed.ID
	plan.BeforeTransport, plan.AfterTransport = "http", "http"
	plan.TargetBytesSHA256, plan.EntryTokenSHA256 = committed.AfterSHA256, committed.AfterTokenSHA256
	return plan, nil
}

// attachCommittedHTTPProof compares the adapter's current single-source HTTP
// proof with the receipt's selected postimage. Historical direct bytes do not
// authorize a current HTTP binding, even when a higher source has the same URL.
func attachCommittedHTTPProof(ctx context.Context, client clients.Client, reader clients.SelectedEntryOriginReader, req AttachClientRequest, receipt *attachClientReceiptV1) (clients.SelectedEntryOriginV1, error) {
	origin := reader.ReadSelectedEntryOrigin(ctx, req.Server)
	deny := func(code string) (clients.SelectedEntryOriginV1, error) {
		return clients.SelectedEntryOriginV1{}, &AttachPlanError{Code: code}
	}
	if code := attachOriginPlanCode(origin, true); code != "" {
		return deny(code)
	}
	if origin.Version != 1 || origin.ClientID != req.Client || origin.EntryName != req.Server || origin.Scope != "known-global" || origin.SourceKind != "write-target" || origin.TargetPathInternal != client.ConfigPath() || origin.TargetPathInternal != receipt.TargetPath || ctx.Err() != nil {
		return deny("client-origin-drift")
	}
	if origin.TargetBytesSHA256 != receipt.AfterSHA256 || origin.EntryTokenSHA256 != receipt.AfterTokenSHA256 {
		return deny("receipt-postimage-drift")
	}
	entry, err := client.GetEntry(req.Server)
	if err != nil || entry == nil || entry.Disabled || entry.SourceBelowWriteTarget || entry.URL != receipt.Endpoint || ctx.Err() != nil {
		return deny("receipt-effective-drift")
	}
	return origin, nil
}

// attachOriginPlanCode keeps source facts and observable refusal IDs separate.
// In particular, an unowned HTTP entry retains its pre-F4b plan error.
func attachOriginPlanCode(origin clients.SelectedEntryOriginV1, committed bool) string {
	if origin.Version != 1 {
		return "client-origin-unverified"
	}
	switch origin.Verdict {
	case clients.SelectedEntryOriginTargetOwnedDirect:
		if !committed {
			return ""
		}
	case clients.SelectedEntryOriginTargetOwnedHTTP:
		if committed {
			return ""
		}
		return "client-origin-disabled-or-unsupported-transport"
	case clients.SelectedEntryOriginHigherOrMixed, clients.SelectedEntryOriginBelowOrImport,
		clients.SelectedEntryOriginMissing, clients.SelectedEntryOriginUnsupported,
		clients.SelectedEntryOriginMalformed, clients.SelectedEntryOriginUnwritable,
		clients.SelectedEntryOriginAliasOrAmbiguous, clients.SelectedEntryOriginUnsupportedClient:
		return "client-origin-" + string(origin.Verdict)
	default:
		return "client-origin-unverified"
	}
	return "client-origin-" + string(origin.Verdict)
}

func sameAttachRows(before, after []DaemonStatus) bool {
	// Uptime and volatile presentation fields may move; only source wire fields
	// defining the selected generation are relevant to this admission.
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		a, b := before[i], after[i]
		if a.Server != b.Server || a.Daemon != b.Daemon || a.TaskName != b.TaskName || a.CommandInternal != b.CommandInternal || !reflect.DeepEqual(a.ArgsInternal, b.ArgsInternal) || a.Port != b.Port || a.PID != b.PID || a.StartedAtInternal != b.StartedAtInternal || a.State != b.State {
			return false
		}
	}
	return true
}

func (a *API) readAttachManifest(server string) (string, *config.ServerManifest, string, error) {
	if err := checkManifestName(server); err != nil {
		return "", nil, "", &AttachPlanError{Code: "invalid-server"}
	}
	// An embedded source shadowed by a disk manifest has no unique raw source.
	if manifestDirForTests() == "" {
		if _, err := fs.ReadFile(servers.Manifests, server+"/manifest.yaml"); err == nil {
			dir, err := CanonicalManifestDir()
			if err != nil {
				return "", nil, "", &AttachPlanError{Code: "manifest-source-unverified"}
			}
			_, err = os.Lstat(filepath.Join(dir, server, "manifest.yaml"))
			if err == nil {
				return "", nil, "", &AttachPlanError{Code: "manifest-source-ambiguous"}
			}
			if !os.IsNotExist(err) {
				return "", nil, "", &AttachPlanError{Code: "manifest-source-unverified"}
			}
		}
	}
	raw, err := a.ManifestGet(server)
	if err != nil {
		return "", nil, "", &AttachPlanError{Code: "manifest-absent"}
	}
	manifest, err := config.ParseManifest(strings.NewReader(raw))
	if err != nil || manifest.Name != server {
		return "", nil, "", &AttachPlanError{Code: "manifest-corrupt"}
	}
	return raw, manifest, ManifestHashContent([]byte(raw)), nil
}

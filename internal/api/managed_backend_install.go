package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// Only the materializer owns these fields. Serialization exports a private
// protected-capsule reference, never raw intent or caller PID authority.
type ManagedBackendInstallReceiptV1 struct{ reference, digest, stage, outcome string }
type managedBackendReceiptWire struct {
	CapsuleReference string `json:"capsule_reference"`
	CapsuleSHA256    string `json:"capsule_sha256"`
	Stage            string `json:"stage"`
	Outcome          string `json:"outcome"`
}

func (r ManagedBackendInstallReceiptV1) MarshalJSON() ([]byte, error) {
	return json.Marshal(managedBackendReceiptWire{r.reference, r.digest, r.stage, r.outcome})
}
func (r *ManagedBackendInstallReceiptV1) UnmarshalJSON(b []byte) error {
	var w managedBackendReceiptWire
	if err := decodeProjectAttachmentJSON(b, &w); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil || !projectClientExactJSONFields(fields, "capsule_reference", "capsule_sha256", "stage", "outcome") {
		return clients.ErrProjectClientInvalid
	}
	if !managedRegistrationSafeIdentifier(w.CapsuleReference) || !managedRegistrationBackupDigestValid(w.CapsuleSHA256) || w.Stage == "" || w.Outcome == "" {
		return clients.ErrProjectClientInvalid
	}
	*r = ManagedBackendInstallReceiptV1{w.CapsuleReference, w.CapsuleSHA256, w.Stage, w.Outcome}
	return nil
}

type ManagedBackendCompensationV1 struct {
	Outcome        string `json:"outcome"`
	IntentRestored bool   `json:"intent_restored"`
	NativeSettled  bool   `json:"native_settled"`
	FirstFailure   string `json:"first_failure"`
}

type managedBackendCompensationRecord struct {
	Schema            string                       `json:"schema"`
	CapsuleReference  string                       `json:"capsule_reference"`
	CapsuleSHA256     string                       `json:"capsule_sha256"`
	TargetGroupSHA256 string                       `json:"target_group_sha256"`
	Received          ManagedBackendCompensationV1 `json:"received"`
}

func prepareOwnedBackendCompensation(ctx context.Context, root string, ref *ManagedBackendInstallReceiptV1, target string) (managedBackendCompensationRecord, bool, error) {
	record := managedBackendCompensationRecord{Schema: "managed-backend-compensation-v1", CapsuleReference: ref.reference, CapsuleSHA256: ref.digest, TargetGroupSHA256: target, Received: ManagedBackendCompensationV1{Outcome: "pending"}}
	leaf := ref.reference + ".inverse.json"
	b, err := ReadClientConfigBackupBeneathRootNoFollow(ctx, root, []string{leaf}, "")
	if err == nil {
		var previous managedBackendCompensationRecord
		if err := decodeProjectAttachmentJSON(b, &previous); err != nil {
			return record, false, err
		}
		if previous.Schema != record.Schema || previous.CapsuleReference != record.CapsuleReference || previous.CapsuleSHA256 != record.CapsuleSHA256 || previous.TargetGroupSHA256 != target {
			return record, false, clients.ErrProjectClientRollbackIncomplete
		}
		if previous.Received.Outcome == "received" && previous.Received.IntentRestored {
			return previous, true, nil
		}
		return record, false, errors.New("managed backend inverse already attempted; preserve pending custody")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return record, false, err
	}
	b, err = json.Marshal(record)
	if err != nil {
		return record, false, err
	}
	_, err = (managedRegistrationBackupIO{}).Create(ctx, root, leaf, b, projectClientHash(b))
	return record, false, err
}

func receiveOwnedBackendCompensation(ctx context.Context, root string, before managedBackendCompensationRecord, result ManagedBackendCompensationV1) error {
	owner := managedRegistrationBackupIO{}
	leaf := before.CapsuleReference + ".inverse.json"
	original, err := json.Marshal(before)
	if err != nil {
		return err
	}
	if _, err := owner.Read(ctx, root, leaf, projectClientHash(original)); err != nil {
		return err
	}
	after := before
	after.Received = result
	b, err := json.Marshal(after)
	if err != nil {
		return err
	}
	if err := SecureWriteClientConfig(filepath.Join(root, leaf), b); err != nil {
		return err
	}
	_, err = owner.Read(ctx, root, leaf, projectClientHash(b))
	return err
}

type managedBackendScope struct {
	Rows       []SupervisorDaemon      `json:"rows"`
	Stops      map[string]DaemonIntent `json:"stops"`
	Watermarks map[string]DaemonIntent `json:"watermarks"`
}
type managedBackendActivation struct {
	Supervisor    SupervisorLockOwner `json:"supervisor"`
	State         string              `json:"state"`
	PIDGeneration uint64              `json:"pid_generation"`
}
type managedBackendInstallCapsule struct {
	Schema               string                              `json:"schema"`
	OperationID          string                              `json:"operation_id"`
	TargetGroupSHA256    string                              `json:"target_group_sha256"`
	ManifestSHA256       string                              `json:"manifest_sha256"`
	PlanSHA256           string                              `json:"plan_sha256"`
	NamespaceSHA256      string                              `json:"namespace_sha256"`
	PriorIntentDefined   bool                                `json:"prior_intent_defined"`
	PriorIntentSHA256    string                              `json:"prior_intent_sha256"`
	ReceivedIntentSHA256 string                              `json:"received_intent_sha256"`
	PriorScopeSHA256     string                              `json:"prior_scope_sha256"`
	ReceivedScopeSHA256  string                              `json:"received_scope_sha256"`
	InverseBackupID      clients.ManagedRegistrationBackupID `json:"inverse_backup_id"`
	Ownership            string                              `json:"ownership"`
	Publication          string                              `json:"publication"`
	Activation           managedBackendActivation            `json:"activation"`
}

func decodeManagedBackendInstallCapsule(b []byte) (managedBackendInstallCapsule, error) {
	var c managedBackendInstallCapsule
	if err := decodeProjectAttachmentJSON(b, &c); err != nil {
		return c, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil || !projectClientExactJSONFields(fields, "schema", "operation_id", "target_group_sha256", "manifest_sha256", "plan_sha256", "namespace_sha256", "prior_intent_defined", "prior_intent_sha256", "received_intent_sha256", "prior_scope_sha256", "received_scope_sha256", "inverse_backup_id", "ownership", "publication", "activation") {
		return c, clients.ErrProjectClientInvalid
	}
	if c.Schema != "managed-backend-install-v1" || !managedRegistrationSafeIdentifier(c.OperationID) || (c.Ownership != "created" && c.Ownership != "reused") || c.Activation.Supervisor.PID <= 0 {
		return c, clients.ErrProjectClientInvalid
	}
	for _, h := range []string{c.TargetGroupSHA256, c.ManifestSHA256, c.PlanSHA256, c.NamespaceSHA256, c.PriorIntentSHA256, c.ReceivedIntentSHA256, c.PriorScopeSHA256, c.ReceivedScopeSHA256} {
		if !managedRegistrationBackupDigestValid(h) {
			return c, clients.ErrProjectClientInvalid
		}
	}
	if _, err := clients.ParseManagedRegistrationBackupID(c.InverseBackupID.String()); err != nil {
		return c, err
	}
	if _, err := time.Parse(time.RFC3339Nano, c.Activation.Supervisor.StartedAt); err != nil {
		return c, err
	}
	switch c.Publication {
	case "prepared", "attempted", "received", "unknown", "rolled-back":
	default:
		return c, clients.ErrProjectClientInvalid
	}
	if c.Activation.State != "unreceived" && (c.Activation.State != "ready" || c.Activation.PIDGeneration == 0) {
		return c, clients.ErrProjectClientInvalid
	}
	return c, nil
}

type managedBackendInverseBackup struct {
	ProjectRoot         string                    `json:"project_root"`
	ProjectPlan         ProjectClientCreatePlanV1 `json:"project_plan"`
	ProjectBeforeBase64 string                    `json:"project_before_base64"`
	ProjectAfterSHA256  string                    `json:"project_after_sha256"`
	ProjectOwnedDelta   projectClientOwnedDelta   `json:"project_owned_delta"`
	IntentDefined       bool                      `json:"intent_defined"`
	IntentBytesBase64   string                    `json:"intent_bytes_base64"`
	PriorScope          managedBackendScope       `json:"prior_scope"`
	ReceivedScope       managedBackendScope       `json:"received_scope"`
	Server              string                    `json:"server"`
	Daemon              string                    `json:"daemon"`
}
type managedBackendInstallOperation struct {
	preparedInvocation  *PreparedSerenaConsumerInvocation
	heldIntent          *StateFileObservedLease
	preparedCommit      *SupervisorFileMutationObservation
	preparedCommitSHA   string
	preparedPriorSHA    string
	captureControl      func(context.Context, string) (SupervisorLockOwner, error)
	admitCreatedInverse func(context.Context, string) error
	attachment          *projectClientAttachmentPlan
	root, path          string
	owner               managedRegistrationBackupIO
	public              ProjectClientCreatePlanV1
	capsule             managedBackendInstallCapsule
	backup              managedBackendInverseBackup
	backupSHA           string
	priorRaw            []byte
	prior, desired      *SupervisorIntentFile
	receipt             *ManagedBackendInstallReceiptV1
	compensation        ManagedBackendCompensationV1
	attempted           bool
}

func selectedBackendScope(intent *SupervisorIntentFile, server, daemon string) (managedBackendScope, error) {
	s := managedBackendScope{Rows: []SupervisorDaemon{}, Stops: map[string]DaemonIntent{}, Watermarks: map[string]DaemonIntent{}}
	if intent == nil {
		return s, clients.ErrProjectClientInvalid
	}
	task := canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon(server, daemon))
	for _, row := range intent.Daemons {
		if canonicalIntentTaskKey(row.TaskName) == task {
			if row.Server != server || row.Daemon != daemon || len(s.Rows) != 0 {
				return s, clients.ErrProjectClientEntryConflict
			}
			s.Rows = append(s.Rows, row)
		}
	}
	for key, value := range intent.Stops {
		if canonicalIntentTaskKey(key) == task {
			s.Stops[key] = value
		}
	}
	for key, value := range intent.LegacyStopWatermarks {
		if canonicalIntentTaskKey(key) == task {
			s.Watermarks[key] = value
		}
	}
	return s, nil
}

func (op *managedBackendInstallOperation) beginLocked(ctx context.Context, prior, desired *SupervisorIntentFile, defined bool, server, daemon string) error {
	if op == nil || op.receipt != nil || ctx == nil || ctx.Err() != nil {
		return clients.ErrProjectClientInvalid
	}
	before, err := selectedBackendScope(prior, server, daemon)
	if err != nil {
		return err
	}
	after, err := selectedBackendScope(desired, server, daemon)
	if err != nil {
		return err
	}
	if len(after.Rows) != 1 {
		return clients.ErrProjectClientInvalid
	}
	ownership := "created"
	if len(before.Rows) == 1 {
		if !reflect.DeepEqual(before, after) {
			return clients.ErrProjectClientEntryConflict
		}
		ownership = "reused"
	} else if len(before.Stops) != 0 || len(before.Watermarks) != 0 {
		return clients.ErrProjectClientEntryConflict
	}
	if ownership == "created" {
		if op.admitCreatedInverse == nil {
			return ErrSupervisorCapabilityIncomplete
		}
		if err := op.admitCreatedInverse(ctx, op.root); err != nil {
			return err
		}
	}
	raw := []byte{}
	if defined {
		raw, err = readSupervisorIntentFileInodeAnchored(op.path)
		if err != nil {
			return err
		}
		if op.preparedInvocation != nil && projectClientHash(raw) != op.preparedPriorSHA {
			return clients.ErrProjectClientPlanStale
		}
		var decoded SupervisorIntentFile
		if json.Unmarshal(raw, &decoded) != nil || !reflect.DeepEqual(&decoded, prior) {
			return clients.ErrProjectClientPlanStale
		}
	}
	var control SupervisorLockOwner
	if op.captureControl != nil {
		control, err = op.captureControl(ctx, op.root)
	} else {
		control, err = ReadSupervisorLockOwner(filepath.Join(op.root, "supervisor.lock"))
		if err == nil {
			err = VerifySupervisorControlGenerationFromStateDir(ctx, op.root, control)
		}
	}
	if err != nil {
		return err
	}
	if control.PID <= 0 || control.StartedAt == "" {
		return clients.ErrProjectClientInvalid
	}
	op.priorRaw = append([]byte(nil), raw...)
	op.prior = cloneSupervisorIntentFile(prior)
	op.desired = cloneSupervisorIntentFile(desired)
	op.backup = managedBackendInverseBackup{IntentDefined: defined, IntentBytesBase64: base64.StdEncoding.EncodeToString(raw), PriorScope: before, ReceivedScope: after, Server: server, Daemon: daemon}
	if op.attachment == nil {
		return clients.ErrProjectClientInvalid
	}
	p := op.attachment
	logical, managed, mask, alias, separators := p.preparation.OwnedDelta()
	op.backup.ProjectRoot = p.identity.CanonicalRoot()
	op.backup.ProjectPlan = p.public
	op.backup.ProjectBeforeBase64 = base64.StdEncoding.EncodeToString(p.preparation.BeforeBytes())
	op.backup.ProjectAfterSHA256 = p.preparation.AfterSHA256()
	op.backup.ProjectOwnedDelta = projectClientOwnedDelta{logical, managed, base64.StdEncoding.EncodeToString(mask), base64.StdEncoding.EncodeToString(alias), base64.StdEncoding.EncodeToString(separators)}
	backupBytes, err := json.Marshal(op.backup)
	if err != nil {
		return err
	}
	op.backupSHA = projectClientHash(backupBytes)
	leaf := "supervisor-intent.json.bak-mcp-local-hub-" + time.Now().UTC().Format("20060102-150405")
	id, err := clients.ParseManagedRegistrationBackupID(leaf)
	if err != nil {
		return err
	}
	priorSHA, err := projectClientValueHash(before)
	if err != nil {
		return err
	}
	afterSHA, err := projectClientValueHash(after)
	if err != nil {
		return err
	}
	op.capsule = managedBackendInstallCapsule{Schema: "managed-backend-install-v1", OperationID: "backend-" + op.public.PlanSHA256, TargetGroupSHA256: op.public.TargetGroupSHA256, ManifestSHA256: op.public.ManifestSHA256, PlanSHA256: op.public.PlanSHA256, NamespaceSHA256: projectClientHash([]byte(op.root)), PriorIntentDefined: defined, PriorIntentSHA256: projectClientHash(raw), ReceivedIntentSHA256: projectClientHash(raw), PriorScopeSHA256: priorSHA, ReceivedScopeSHA256: afterSHA, InverseBackupID: id, Ownership: ownership, Publication: "prepared", Activation: managedBackendActivation{Supervisor: control, State: "unreceived"}}
	b, err := json.Marshal(op.capsule)
	if err != nil {
		return err
	}
	ref := op.capsule.OperationID + ".json"
	op.receipt = &ManagedBackendInstallReceiptV1{ref, projectClientHash(b), "prearm", "pending"}
	if _, err := op.owner.Create(ctx, op.root, leaf, backupBytes, op.backupSHA); err != nil {
		return err
	}
	if _, err := op.owner.Create(ctx, op.root, ref, b, projectClientHash(b)); err != nil {
		return err
	}
	readback, err := op.owner.Read(ctx, op.root, ref, projectClientHash(b))
	if err != nil || !bytes.Equal(b, readback) {
		return errors.Join(err, clients.ErrProjectClientPlanStale)
	}
	op.receipt.stage = "prepared"
	return nil
}

func (op *managedBackendInstallOperation) persist(ctx context.Context, stage, outcome string) error {
	if op == nil || op.receipt == nil {
		return clients.ErrProjectClientInvalid
	}
	old, err := op.owner.Read(ctx, op.root, op.receipt.reference, op.receipt.digest)
	if err != nil {
		return err
	}
	var prior managedBackendInstallCapsule
	if json.Unmarshal(old, &prior) != nil || prior.OperationID != op.capsule.OperationID {
		return clients.ErrProjectClientPlanStale
	}
	b, err := json.Marshal(op.capsule)
	if err != nil {
		return err
	}
	if err := SecureWriteClientConfig(filepath.Join(op.root, op.receipt.reference), b); err != nil {
		return err
	}
	readback, err := op.owner.Read(ctx, op.root, op.receipt.reference, projectClientHash(b))
	if err != nil || !bytes.Equal(b, readback) {
		return errors.Join(err, clients.ErrProjectClientPlanStale)
	}
	op.receipt.digest = projectClientHash(b)
	op.receipt.stage = stage
	op.receipt.outcome = outcome
	return nil
}

func (op *managedBackendInstallOperation) readIntentLocked(ctx context.Context) (*SupervisorIntentFile, bool, []byte, error) {
	if op.preparedInvocation != nil {
		current, defined, sha, err := op.preparedInvocation.readParsedIntentObserved(ctx, op.heldIntent)
		if err != nil || !defined {
			return current, defined, nil, err
		}
		raw, err := readSupervisorIntentFileInodeAnchored(op.path)
		if err != nil {
			return nil, false, nil, err
		}
		if projectClientHash(raw) != sha {
			return nil, false, nil, clients.ErrProjectClientPlanStale
		}
		return current, true, raw, nil
	}
	raw, err := readSupervisorIntentFileInodeAnchored(op.path)
	if err != nil {
		return nil, false, nil, err
	}
	current, err := ReadSupervisorIntent(op.path)
	return current, true, raw, err
}

func (op *managedBackendInstallOperation) receiveLocked(ctx context.Context) error {
	current, defined, raw, err := op.readIntentLocked(ctx)
	if err != nil {
		return err
	}
	if !defined {
		return clients.ErrProjectClientPlanStale
	}
	scope, err := selectedBackendScope(current, op.backup.Server, op.backup.Daemon)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(scope, op.backup.ReceivedScope) {
		op.capsule.Publication = "unknown"
		return errors.Join(clients.ErrProjectClientPlanStale, op.persist(ctx, "readback", "pending"))
	}
	op.capsule.ReceivedIntentSHA256 = projectClientHash(raw)
	op.capsule.Publication = "received"
	return op.persist(ctx, "intent-received", "pending")
}

func restoreBackendScope(current *SupervisorIntentFile, before, owned managedBackendScope, server, daemon string) (*SupervisorIntentFile, error) {
	actual, err := selectedBackendScope(current, server, daemon)
	if err != nil || !reflect.DeepEqual(actual, owned) {
		return nil, errors.Join(err, clients.ErrProjectClientRollbackIncomplete)
	}
	// Only the outer row list and owned stop maps are mutated. Keep every
	// foreign descriptor's typed shape (including empty versus nil args).
	out := cloneSupervisorIntentFile(current)
	task := canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon(server, daemon))
	rows := make([]SupervisorDaemon, 0, len(out.Daemons))
	for _, row := range out.Daemons {
		if canonicalIntentTaskKey(row.TaskName) != task {
			rows = append(rows, row)
		}
	}
	out.Daemons = append(rows, before.Rows...)
	if out.Stops == nil {
		out.Stops = map[string]DaemonIntent{}
	}
	if out.LegacyStopWatermarks == nil {
		out.LegacyStopWatermarks = map[string]DaemonIntent{}
	}
	for key := range out.Stops {
		if canonicalIntentTaskKey(key) == task {
			delete(out.Stops, key)
		}
	}
	for key := range out.LegacyStopWatermarks {
		if canonicalIntentTaskKey(key) == task {
			delete(out.LegacyStopWatermarks, key)
		}
	}
	for key, value := range before.Stops {
		out.Stops[key] = value
	}
	for key, value := range before.Watermarks {
		out.LegacyStopWatermarks[key] = value
	}
	return out, nil
}

// This is the single typed intermediate compensation owner. The legacy void
// callback merely invokes it; receipt and caller retain its actual outcome.
func (op *managedBackendInstallOperation) compensateLocked(ctx context.Context) (result ManagedBackendCompensationV1, err error) {
	result = ManagedBackendCompensationV1{Outcome: "pending"}
	defer func() {
		if err != nil {
			result.FirstFailure = "backend_intent_compensation_failed"
		}
		op.compensation = result
		if op.attempted && op.capsule.Ownership == "created" && !result.NativeSettled {
			result.Outcome = "pending"
			op.compensation = result
		}
	}()
	if op.attempted && op.capsule.Ownership == "created" {
		// Publication can race the existing intent watcher. A metadata undo
		// cannot prove the original native generation closed; retain the exact
		// descriptor for the received supervisor-owned inverse instead.
		return result, errors.New("managed backend native settlement pending before intent compensation")
	}
	current, defined, currentRaw, err := op.readIntentLocked(ctx)
	if err != nil {
		return result, err
	}
	if !defined {
		return result, clients.ErrProjectClientPlanStale
	}
	actual, e := selectedBackendScope(current, op.backup.Server, op.backup.Daemon)
	if e != nil {
		return result, e
	}
	if reflect.DeepEqual(actual, op.backup.PriorScope) {
		result.Outcome = "received"
		result.IntentRestored = true
		return result, nil
	}
	restored, err := restoreBackendScope(current, op.backup.PriorScope, op.backup.ReceivedScope, op.backup.Server, op.backup.Daemon)
	if err != nil {
		return result, err
	}
	var observed []scopedSupervisorIntentPublication
	if op.preparedInvocation != nil {
		observed = []scopedSupervisorIntentPublication{{ctx: ctx, prior: current, invocation: op.preparedInvocation, lease: op.heldIntent,
			beforeSHA: projectClientHash(currentRaw), freshCommit: op.preparedCommit, freshCommitSHA: op.preparedCommitSHA}}
	}
	if err = publishScopedSupervisorIntentRemovalLockHeld(op.path, restored, !op.backup.IntentDefined, observed...); err != nil {
		return result, err
	}
	readback, readbackDefined, _, err := op.readIntentLocked(ctx)
	if err != nil {
		return result, err
	}
	if !readbackDefined {
		if op.backup.IntentDefined {
			return result, clients.ErrProjectClientRollbackIncomplete
		}
		result.Outcome, result.IntentRestored = "received", true
		return result, nil
	}
	scope, err := selectedBackendScope(readback, op.backup.Server, op.backup.Daemon)
	if err != nil || !reflect.DeepEqual(scope, op.backup.PriorScope) {
		return result, errors.Join(err, clients.ErrProjectClientRollbackIncomplete)
	}
	result.Outcome = "received"
	result.IntentRestored = true
	return result, nil
}

func (op *managedBackendInstallOperation) sealReady(ctx context.Context, snapshot ReadinessSnapshotV1) error {
	if op == nil || op.receipt == nil || op.capsule.Publication != "received" {
		return clients.ErrProjectClientInvalid
	}
	if err := VerifySupervisorControlGenerationFromStateDir(ctx, op.root, op.capsule.Activation.Supervisor); err != nil {
		return err
	}
	if !snapshot.Settled {
		return clients.ErrProjectClientInvalid
	}
	hits := 0
	for _, d := range snapshot.Daemons {
		if canonicalIntentTaskKey(d.TaskName) == canonicalIntentTaskKey(op.backup.ReceivedScope.Rows[0].TaskName) {
			if !d.Settled || d.PIDGeneration == 0 {
				return clients.ErrProjectClientInvalid
			}
			op.capsule.Activation.PIDGeneration = d.PIDGeneration
			hits++
		}
	}
	if hits != 1 {
		return clients.ErrProjectClientInvalid
	}
	op.capsule.Activation.State = "ready"
	return op.persist(ctx, "ready", "sealed")
}

func (op *managedBackendInstallOperation) withIntentLockHeld(ctx context.Context, action func() error) (err error) {
	if op.preparedInvocation != nil {
		lease, acquireErr := AcquireStateFileObservedLease(ctx, op.path)
		if acquireErr != nil {
			if lease != nil {
				proof, closeErr := lease.CloseObserved()
				acquireErr = errors.Join(acquireErr, closeErr, proof.Validate(lease), op.preparedInvocation.ReceiveIntentClose(proof))
			}
			return acquireErr
		}
		defer func() {
			proof, closeErr := lease.CloseObserved()
			err = errors.Join(err, closeErr, proof.Validate(lease), op.preparedInvocation.ReceiveIntentClose(proof))
			op.heldIntent = nil
		}()
		return op.preparedInvocation.WithIntentLockHeld(ctx, lease, func() error {
			op.heldIntent = lease
			return action()
		})
	}
	release, err := lockSupervisorIntent(op.path)
	if err != nil {
		return err
	}
	defer releaseSupervisorIntentAndJoinApplied(&err, release, "backend install receive lock", false)
	return action()
}

func (op *managedBackendInstallOperation) receiveAfterMaterialization(ctx context.Context) error {
	return op.withIntentLockHeld(ctx, func() error { return op.receiveLocked(ctx) })
}

// Prospective attachment owns no Task/autostart migration. Existing positive
// owner state and absence of the selected legacy Task make these late actions
// unnecessary; otherwise refuse before definition publication.
func checkProjectBackendLateEffects(m *config.ServerManifest, daemon string, intent *SupervisorIntentFile) error {
	backend, err := installAutostartBackendFactoryFn()
	if err != nil {
		return err
	}
	entry, err := CanonicalAutostartEntrypoint()
	if err != nil {
		return err
	}
	return withInstallAutostartGUIControl(func(allow bool) error {
		if allow {
			state, err := backend.Status(autostart.Options{MCPHubPath: entry, StrictMode: intent.StrictMode, OwnerMode: intent.EffectiveOwnerMode()})
			if err != nil {
				return err
			}
			if state != autostart.StateEnabledRunning && state != autostart.StateEnabledStopped {
				return fmt.Errorf("managed backend autostart inverse unavailable for current state %s", state)
			}
		}
		sch, err := newScheduler()
		if err != nil {
			return err
		}
		name := supervisorTaskNameForManifestDaemon(m.Name, daemon)
		rows, err := sch.List(name)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if strings.TrimPrefix(row.Name, `\`) == strings.TrimPrefix(name, `\`) {
				return errors.New("managed backend legacy Task inverse unavailable")
			}
		}
		return nil
	})
}

// Reused inverse is a no-op. Created inverse is deliberately fail closed until
// project/projection restoration and full admitted-consumer acquisition have
// been received by the owning attachment composition.
func (a *API) RestoreOwnedBackendInstall(ctx context.Context, receipt *ManagedBackendInstallReceiptV1) (result ManagedBackendCompensationV1, resultErr error) {
	return a.restoreOwnedBackendInstallWithOperation(ctx, receipt, nil)
}

func (a *API) restoreOwnedBackendInstallWithOperation(ctx context.Context, receipt *ManagedBackendInstallReceiptV1, operation *managedBackendInstallOperation) (result ManagedBackendCompensationV1, resultErr error) {
	result = ManagedBackendCompensationV1{Outcome: "pending"}
	if receipt == nil || receipt.outcome != "sealed" {
		return result, clients.ErrProjectClientRollbackIncomplete
	}
	var root string
	var err error
	if operation != nil {
		if operation.receipt != receipt || operation.root == "" {
			return result, clients.ErrProjectClientRollbackIncomplete
		}
		if operation.preparedInvocation != nil && (operation.root != operation.preparedInvocation.io.Root || operation.path != operation.preparedInvocation.io.IntentPath) {
			return result, errors.New("SERENA_PARSED_INTENT_OWNER_MISSING")
		}
		root = operation.root
	} else {
		root, err = DaemonStateDir()
		if err != nil {
			return result, err
		}
	}
	owner := managedRegistrationBackupIO{}
	b, err := owner.Read(ctx, root, receipt.reference, receipt.digest)
	if err != nil {
		return result, err
	}
	c, err := decodeManagedBackendInstallCapsule(b)
	if err != nil {
		return result, err
	}
	if c.Schema != "managed-backend-install-v1" || c.NamespaceSHA256 != projectClientHash([]byte(root)) || c.Publication != "received" || c.Activation.State != "ready" {
		return result, clients.ErrProjectClientRollbackIncomplete
	}
	if c.Ownership != "created" && c.Ownership != "reused" {
		return result, clients.ErrProjectClientRollbackIncomplete
	}
	if c.Ownership == "created" {
		// V1 exposes stopped intent before queue admission. Never use it for
		// a receipt-owned inverse while the fenced V2 owner is unavailable.
		return result, ErrSupervisorCapabilityIncomplete
	}
	backupBytes, err := ReadClientConfigBackupBeneathRootNoFollow(ctx, root, []string{c.InverseBackupID.String()}, "")
	if err != nil {
		return result, err
	}
	var backup managedBackendInverseBackup
	if err := json.Unmarshal(backupBytes, &backup); err != nil {
		return result, err
	}
	priorBytes, err := base64.StdEncoding.Strict().DecodeString(backup.IntentBytesBase64)
	if err != nil || projectClientHash(priorBytes) != c.PriorIntentSHA256 || backup.IntentDefined != c.PriorIntentDefined {
		return result, clients.ErrProjectClientRollbackIncomplete
	}
	priorScopeSHA, _ := projectClientValueHash(backup.PriorScope)
	receivedScopeSHA, _ := projectClientValueHash(backup.ReceivedScope)
	if priorScopeSHA != c.PriorScopeSHA256 || receivedScopeSHA != c.ReceivedScopeSHA256 || len(backup.ReceivedScope.Rows) != 1 {
		return result, clients.ErrProjectClientRollbackIncomplete
	}
	manifestDir, err := CanonicalManifestDir()
	if err != nil {
		return result, err
	}
	manifestBytes, err := loadManifestYAMLEmbedFirstFromDir(backup.Server, manifestDir)
	if err != nil {
		return result, err
	}
	if ManifestHashContent(manifestBytes) != c.ManifestSHA256 || backup.ReceivedScope.Rows[0].ManifestHash != c.ManifestSHA256 {
		return result, clients.ErrProjectClientRollbackIncomplete
	}
	if err := backup.ProjectPlan.validate(); err != nil || backup.ProjectPlan.PlanSHA256 != c.PlanSHA256 {
		return result, errors.Join(err, clients.ErrProjectClientRollbackIncomplete)
	}
	project, identity, err := clients.NewCodexCLIForProject(backup.ProjectRoot, backup.ProjectOwnedDelta.LogicalKey, NewCodexProjectConfigMutationOwner)
	if err != nil {
		return result, err
	}
	if identity.ProjectIdentitySHA256() != backup.ProjectPlan.ProjectIdentitySHA256 {
		return result, clients.ErrProjectClientRollbackIncomplete
	}
	beforeProject, err := base64.StdEncoding.Strict().DecodeString(backup.ProjectBeforeBase64)
	if err != nil || projectClientHash(beforeProject) != backup.ProjectPlan.ConfigBeforeSHA256 {
		return result, clients.ErrProjectClientRollbackIncomplete
	}
	d := backup.ProjectOwnedDelta
	mask, err := base64.StdEncoding.Strict().DecodeString(d.MaskTokenBase64)
	if err != nil {
		return result, err
	}
	alias, err := base64.StdEncoding.Strict().DecodeString(d.AliasTokenBase64)
	if err != nil {
		return result, err
	}
	separators, err := base64.StdEncoding.Strict().DecodeString(d.SeparatorsBase64)
	if err != nil {
		return result, err
	}
	projectOwner := project.(clients.ProjectHTTPAttachmentClient)
	preparation, err := projectOwner.RecoverProjectHTTPAttachment(d.LogicalKey, backup.ProjectPlan.Operation, beforeProject, backup.ProjectAfterSHA256, mask, alias, separators)
	if err != nil {
		return result, err
	}
	if err := projectOwner.VerifyRestoredProjectHTTPAttachment(preparation); err != nil {
		return result, err
	}
	if c.Ownership == "reused" {
		record, terminal, err := prepareOwnedBackendCompensation(ctx, root, receipt, c.TargetGroupSHA256)
		if err != nil {
			return result, err
		}
		if terminal {
			return record.Received, nil
		}
		result = ManagedBackendCompensationV1{Outcome: "received", IntentRestored: true}
		return result, receiveOwnedBackendCompensation(ctx, root, record, result)
	}
	if err := acquireNoManagedBackendConsumers(ctx, root, backup.Server, c.TargetGroupSHA256, c.OperationID); err != nil {
		return result, err
	}
	var current *SupervisorIntentFile
	if operation != nil && operation.preparedInvocation != nil {
		current, err = ReadSupervisorIntent(operation.path)
	} else {
		current, err = loadSupervisorOwnedIntent()
	}
	if err != nil {
		return result, err
	}
	scope, err := selectedBackendScope(current, backup.Server, backup.Daemon)
	if err != nil || !reflect.DeepEqual(scope, backup.ReceivedScope) {
		return result, errors.Join(err, clients.ErrProjectClientRollbackIncomplete)
	}
	for _, row := range scope.Rows {
		binding, bindingErr := preparedSerenaDescriptorBinding(row)
		if bindingErr != nil {
			return result, bindingErr
		}
		if binding != nil && (operation == nil || operation.preparedInvocation == nil || operation.preparedCommit == nil) {
			return result, errors.New("SERENA_PARSED_REPLACEMENT_INPUT_MISSING")
		}
	}
	// The same scoped proposal owner supplies an early reference veto before
	// any stop action. It grants no native settlement; the original owner and
	// fresh held postimage/CAS still govern the later compensation below.
	prospective, err := restoreBackendScope(current, backup.PriorScope, scope, backup.Server, backup.Daemon)
	if err != nil {
		return result, err
	}
	if _, _, _, err := prepareScopedSupervisorIntentRemoval(current, prospective, !c.PriorIntentDefined); err != nil {
		return result, err
	}
	rows, err := dialSupervisorIPCStatusForOwner(ctx, root, c.Activation.Supervisor)
	if err != nil {
		return result, err
	}
	matches := 0
	for _, row := range rows {
		if canonicalIntentTaskKey(row.TaskName) == canonicalIntentTaskKey(backup.ReceivedScope.Rows[0].TaskName) {
			if row.ReadinessObservation == nil || row.ReadinessObservation.CurrentPIDGeneration != c.Activation.PIDGeneration {
				return result, clients.ErrProjectClientRollbackIncomplete
			}
			matches++
		}
	}
	if matches != 1 {
		return result, clients.ErrProjectClientRollbackIncomplete
	}
	record, terminal, err := prepareOwnedBackendCompensation(ctx, root, receipt, c.TargetGroupSHA256)
	if err != nil {
		return result, err
	}
	if terminal {
		return record.Received, nil
	}
	defer func() {
		if resultErr != nil {
			result.Outcome = "pending"
			result.FirstFailure = "backend_compensation_unreceived"
		}
		resultErr = errors.Join(resultErr, receiveOwnedBackendCompensation(ctx, root, record, result))
	}()
	settled, err := a.stopOwnedDescriptorSettled(ctx, backup.ReceivedScope.Rows[0], &c.Activation.Supervisor)
	if err != nil {
		return result, err
	}
	if settled.PIDGeneration <= 0 || uint64(settled.PIDGeneration) != c.Activation.PIDGeneration {
		return result, clients.ErrProjectClientRollbackIncomplete
	}
	result.NativeSettled = true
	path := filepath.Join(root, supervisorIntentFileLeaf)
	restoreLocked := func() (ManagedBackendCompensationV1, error) {
		var currentRaw []byte
		var currentDefined bool
		if operation != nil && operation.preparedInvocation != nil {
			current, currentDefined, currentRaw, err = operation.readIntentLocked(ctx)
			if err == nil && !currentDefined {
				err = clients.ErrProjectClientPlanStale
			}
		} else {
			current, err = ReadSupervisorIntent(path)
		}
		if err != nil {
			return result, err
		}
		stoppedScope, err := selectedBackendScope(current, backup.Server, backup.Daemon)
		if err != nil {
			return result, err
		}
		if !reflect.DeepEqual(stoppedScope.Rows, backup.ReceivedScope.Rows) || !reflect.DeepEqual(stoppedScope.Watermarks, backup.ReceivedScope.Watermarks) {
			return result, clients.ErrProjectClientRollbackIncomplete
		}
		for _, stop := range stoppedScope.Stops {
			if stop.Desired != IntentDesiredStopped || stop.Reason != IntentReasonUserStop {
				return result, clients.ErrProjectClientRollbackIncomplete
			}
		}
		compensated, err := restoreBackendScope(current, backup.PriorScope, stoppedScope, backup.Server, backup.Daemon)
		if err != nil {
			return result, err
		}
		var observed []scopedSupervisorIntentPublication
		if operation != nil && operation.preparedInvocation != nil {
			observed = []scopedSupervisorIntentPublication{{ctx: ctx, prior: current, invocation: operation.preparedInvocation, lease: operation.heldIntent,
				beforeSHA: projectClientHash(currentRaw), freshCommit: operation.preparedCommit, freshCommitSHA: operation.preparedCommitSHA}}
		}
		if err = publishScopedSupervisorIntentRemovalLockHeld(path, compensated, !c.PriorIntentDefined, observed...); err != nil {
			return result, err
		}
		var readback *SupervisorIntentFile
		if operation != nil && operation.preparedInvocation != nil {
			var defined bool
			readback, defined, _, err = operation.readIntentLocked(ctx)
			if err == nil && !defined && !c.PriorIntentDefined {
				result.IntentRestored, result.Outcome = true, "received"
				return result, nil
			}
			if err == nil && !defined {
				err = clients.ErrProjectClientPlanStale
			}
		} else {
			readback, err = ReadSupervisorIntent(path)
		}
		if !c.PriorIntentDefined && errors.Is(err, os.ErrNotExist) {
			result.IntentRestored = true
			result.Outcome = "received"
			return result, nil
		}
		if err != nil {
			return result, err
		}
		actual, err := selectedBackendScope(readback, backup.Server, backup.Daemon)
		if err != nil || !reflect.DeepEqual(actual, backup.PriorScope) {
			return result, errors.Join(err, clients.ErrProjectClientRollbackIncomplete)
		}
		result.IntentRestored = true
		result.Outcome = "received"
		return result, nil
	}
	if operation != nil && operation.preparedInvocation != nil {
		resultErr = operation.withIntentLockHeld(ctx, func() error {
			var restoreErr error
			result, restoreErr = restoreLocked()
			return restoreErr
		})
		return result, resultErr
	}
	release, err := lockSupervisorIntent(path)
	if err != nil {
		return result, err
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			result.Outcome = "pending"
			resultErr = errors.Join(resultErr, releaseErr)
		}
	}()
	return restoreLocked()
}

func acquireNoManagedBackendConsumers(ctx context.Context, root, server, targetGroup, ownOperation string) error {
	raw, err := ReadStateFileInodeAnchored(filepath.Join(root, managedEntriesFileLeaf))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		var state ManagedEntries
		if json.Unmarshal(raw, &state) != nil || state.Version != managedEntriesSchemaVersion {
			return clients.ErrProjectClientRollbackIncomplete
		}
		for _, entry := range state.Entries {
			if entry.Server == server {
				return errors.New("managed backend still has current admitted consumers")
			}
		}
	}
	journal, err := newManagedRegistrationJournalStore(root)
	if err != nil {
		return err
	}
	pending, err := journal.read(ctx)
	if err != nil {
		return err
	}
	for _, row := range pending.Rows {
		if row.Owner == server && row.Settlement != clients.ManagedRegistrationSettlementRolledBack {
			return errors.New("managed backend still has journal/rollback consumers")
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "backend-") || !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".inverse.json") || entry.Name() == ownOperation+".json" {
			continue
		}
		b, err := ReadClientConfigBackupBeneathRootNoFollow(ctx, root, []string{entry.Name()}, "")
		if err != nil {
			return err
		}
		other, decodeErr := decodeManagedBackendInstallCapsule(b)
		if decodeErr != nil || other.NamespaceSHA256 != projectClientHash([]byte(root)) {
			return clients.ErrProjectClientRollbackIncomplete
		}
		if other.TargetGroupSHA256 == targetGroup && other.Publication != "rolled-back" {
			inverseBytes, inverseErr := ReadClientConfigBackupBeneathRootNoFollow(ctx, root, []string{entry.Name() + ".inverse.json"}, "")
			if inverseErr == nil {
				var inverse managedBackendCompensationRecord
				if err := decodeProjectAttachmentJSON(inverseBytes, &inverse); err != nil {
					return err
				}
				if inverse.CapsuleReference != entry.Name() || inverse.CapsuleSHA256 != projectClientHash(b) || inverse.TargetGroupSHA256 != targetGroup {
					return clients.ErrProjectClientRollbackIncomplete
				}
				if inverse.Received.Outcome == "received" && inverse.Received.IntentRestored && (other.Ownership == "reused" || inverse.Received.NativeSettled) {
					continue
				}
			} else if !errors.Is(inverseErr, os.ErrNotExist) {
				return inverseErr
			}
			return errors.New("managed backend still has pending admitted attachment")
		}
	}
	return ctx.Err()
}

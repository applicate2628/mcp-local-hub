package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

const windowsCanonicalEntrySetSchemaV1 = binaryadmission.WindowsCanonicalEntrySetSchemaV1
const UpgradeAdmissionCanonicalEntryProductV1 = "canonical-entry-product-v1"
const UpgradeAdmissionCanonicalEntryProductV2 = "canonical-entry-product-v2"

// The current arm shares the existing outer record and entry_set key, while
// historical V1 values retain their original serializer and meaning.
type windowsCanonicalEntrySetJournalV2 struct {
	Schema             string                                             `json:"schema"`
	TransactionID      string                                             `json:"transaction_id"`
	CanonicalEntry     windowsProductRecoveryArtifactV3                   `json:"canonical_entry"`
	StagedEntry        windowsProductRecoveryStagedV3                     `json:"staged_entry"`
	Bundle             windowsCanonicalEntryBundleJournalV2               `json:"bundle"`
	Expected           binaryadmission.WindowsCanonicalEntrySetSnapshotV2 `json:"expected"`
	Moves              []windowsCanonicalEntryMoveRecordV2                `json:"moves"`
	Cleanup            []windowsCanonicalEntryCleanupV2                   `json:"cleanup"`
	PriorRuntime       *windowsCanonicalEntryRuntimePreimageV1            `json:"prior_runtime"`
	CleanArchiveSHA256 string                                             `json:"clean_archive_sha256,omitempty"`
}

type windowsCanonicalEntryBundleJournalV2 struct {
	RelativePath    string                                                           `json:"relative_path"`
	BeforeKind      binaryadmission.WindowsCanonicalEntryBeforeKindV2                `json:"before_kind"`
	BeforeDirectory *binaryadmission.WindowsCanonicalEntryPayloadDirectorySnapshotV1 `json:"before_directory"`
	BeforeInventory *binaryadmission.RuntimeInventoryV1                              `json:"before_inventory"`
	Retained        *windowsCanonicalEntryRetainedRuntimeV2                          `json:"retained,omitempty"`
	StagedDirectory *binaryadmission.WindowsCanonicalEntryPayloadDirectorySnapshotV1 `json:"staged_directory"`
	Disposition     windowsCanonicalEntryDispositionV2                               `json:"disposition"`
	Effect          string                                                           `json:"effect"`
	Members         []windowsCanonicalEntryMemberJournalV2                           `json:"members"`
	Directories     []windowsCanonicalEntryDirectoryJournalV2                        `json:"directories"`
}

// The native aggregate binds original file and parent identities and their
// content facts. This journal value alone does not supply native authority.
type windowsCanonicalEntryRetainedRuntimeV2 struct {
	MembersSHA256 string   `json:"members_sha256"`
	Singletons    []string `json:"singletons"`
}

func (e *windowsCanonicalEntrySetJournalV2) retainsSingleton(key string) bool {
	return e.Bundle.Retained != nil && slices.Contains(e.Bundle.Retained.Singletons, key)
}

type windowsCanonicalEntryMemberJournalV2 = binaryadmission.WindowsCanonicalEntryMemberRecoveryPlanV2

type windowsCanonicalEntryDirectoryJournalV2 = binaryadmission.WindowsCanonicalEntryMemberDirectoryRecordV2

type windowsCanonicalEntryDispositionV2 struct {
	State                string                    `json:"state"`
	Transferred          bool                      `json:"transferred"`
	OriginalOwnerPending bool                      `json:"original_owner_pending"`
	DispositionPending   bool                      `json:"disposition_pending"`
	CloseError           *string                   `json:"close_error"`
	CloseOwner           *windowsProductCloseOwner `json:"close_owner,omitempty"`
}

// A close fault can outlive its recovery command. This diagnostic identifies
// that command's native lifetime. It grants no file or process effect authority.
type windowsProductCloseOwner struct {
	PID       uint32    `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

type windowsCanonicalEntryMoveRecordV2 = binaryadmission.WindowsCanonicalEntryMemberMoveRecordV2

type windowsCanonicalEntryCleanupV2 = binaryadmission.WindowsCanonicalEntryMemberCleanupRecordV2

type windowsCanonicalEntrySetReceiptV2 windowsCanonicalEntrySetReceiptV1

func (j windowsProductRecoveryJournalV3) MarshalJSON() ([]byte, error) {
	type legacy windowsProductRecoveryJournalV3
	if j.EntryBundle == nil {
		return json.Marshal(legacy(j))
	}
	if j.EntrySet != nil {
		return nil, errors.New("legacy and current entry arms coexist")
	}
	e := j.EntryBundle
	launcher := e.retainsSingleton(e.Expected.CanonicalEntryRelativePath)
	product := e.retainsSingleton(e.Expected.ProductRelativePath)
	if !launcher && !product {
		return json.Marshal(struct {
			legacy
			EntrySet *windowsCanonicalEntrySetJournalV2 `json:"entry_set"`
		}{legacy(j), e})
	}
	var entry any = e
	if launcher {
		if e.StagedEntry != (windowsProductRecoveryStagedV3{}) {
			return nil, errors.New("retained Launcher cannot carry a stage")
		}
		type plain windowsCanonicalEntrySetJournalV2
		entry = struct {
			*plain
			StagedEntry *windowsProductRecoveryStagedV3 `json:"staged_entry"`
		}{(*plain)(e), nil}
	}
	if product {
		if j.Staged != (windowsProductRecoveryStagedV3{}) {
			return nil, errors.New("retained Product cannot carry a stage")
		}
		return json.Marshal(struct {
			legacy
			EntrySet any                             `json:"entry_set"`
			Staged   *windowsProductRecoveryStagedV3 `json:"staged"`
		}{legacy(j), entry, nil})
	}
	return json.Marshal(struct {
		legacy
		EntrySet any `json:"entry_set"`
	}{legacy(j), entry})
}

func (j *windowsProductRecoveryJournalV3) UnmarshalJSON(raw []byte) error {
	schema, err := canonicalEntryArmSchema(raw)
	if err != nil {
		return err
	}
	type legacy windowsProductRecoveryJournalV3
	var base legacy
	var wire struct {
		*legacy
		EntrySet json.RawMessage `json:"entry_set"`
	}
	wire.legacy = &base
	if err = decodeStrictCanonicalProductRecord(raw, &wire); err != nil {
		return err
	}
	switch schema {
	case "":
	case windowsCanonicalEntrySetSchemaV1:
		if err = decodeStrictCanonicalProductRecord(wire.EntrySet, &base.EntrySet); err != nil {
			return err
		}
	case binaryadmission.WindowsCanonicalEntrySetSchemaV2:
		if err = decodeStrictCanonicalProductRecord(wire.EntrySet, &base.EntryBundle); err != nil {
			return err
		}
	default:
		return errors.New("unsupported nested canonical entry journal schema")
	}
	if e := base.EntryBundle; e != nil && e.Bundle.Retained != nil {
		staged, err := uniqueCanonicalEntryRecordMember(raw, "staged")
		if err != nil {
			return err
		}
		null := bytes.Equal(bytes.TrimSpace(staged), []byte("null"))
		if null != e.retainsSingleton(e.Expected.ProductRelativePath) {
			return errors.New("selective Product stage disagrees with retention")
		}
		if !null {
			if _, err := canonicalCurrentObject(staged, "path", "sha256"); err != nil {
				return err
			}
		}
	}
	*j = windowsProductRecoveryJournalV3(base)
	return nil
}

func (r UpgradeReceiptV3) MarshalJSON() ([]byte, error) {
	type legacy UpgradeReceiptV3
	if r.EntryBundle == nil {
		return json.Marshal(legacy(r))
	}
	if r.EntrySet != nil {
		return nil, errors.New("legacy and current receipt arms coexist")
	}
	return json.Marshal(struct {
		legacy
		EntrySet *windowsCanonicalEntrySetReceiptV2 `json:"entry_set"`
	}{legacy(r), r.EntryBundle})
}

func (r *UpgradeReceiptV3) UnmarshalJSON(raw []byte) error {
	schema, err := canonicalEntryArmSchema(raw)
	if err != nil {
		return err
	}
	type legacy UpgradeReceiptV3
	var base legacy
	var wire struct {
		*legacy
		EntrySet json.RawMessage `json:"entry_set"`
	}
	wire.legacy = &base
	if err = decodeStrictCanonicalProductRecord(raw, &wire); err != nil {
		return err
	}
	switch schema {
	case "":
	case windowsCanonicalEntrySetSchemaV1:
		if err = decodeStrictCanonicalProductRecord(wire.EntrySet, &base.EntrySet); err != nil {
			return err
		}
	case binaryadmission.WindowsCanonicalEntrySetSchemaV2:
		if _, err = canonicalCurrentObject(wire.EntrySet, "schema", "transaction_id", "build_id", "registry_projection_sha256", "canonical_entry_path", "canonical_entry_sha256", "product_path", "identity_path", "identity_sha256", "machine"); err != nil {
			return err
		}
		if err = decodeStrictCanonicalProductRecord(wire.EntrySet, &base.EntryBundle); err != nil {
			return err
		}
	default:
		return errors.New("unsupported nested canonical entry receipt schema")
	}
	*r = UpgradeReceiptV3(base)
	return nil
}

func decodeStrictCanonicalProductRecord(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.Join(err, errors.New("trailing canonical Product record data"))
	}
	return nil
}

// Strict receiving belongs to the existing entry codec. Unlike encoding/json's
// default matching, required decoded keys are exact, unique and non-null.
func canonicalCurrentObject(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	return canonicalCurrentObjectWithOptional(raw, "", names...)
}

func canonicalCurrentObjectWithOptional(raw []byte, optional string, names ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.Join(err, errors.New("current entry evidence must be an object"))
	}
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[n] = true
	}
	fields := make(map[string]json.RawMessage, len(names))
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		name, ok := t.(string)
		if !ok || !wanted[name] && (optional == "" || name != optional) {
			return nil, errors.New("unknown or inexact current entry field")
		}
		if _, ok := fields[name]; ok {
			return nil, errors.New("duplicate current entry field")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			switch name {
			case "prior", "staged", "staged_entry", "prior_absence", "prior_aside_relative", "successor_aside_relative", "before_directory", "before_inventory", "staged_directory", "disposition", "error", "close_error", "prior_runtime":
			default:
				return nil, errors.New("required current entry field is null")
			}
		}
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.Join(err, errors.New("trailing current entry evidence"))
	}
	requiredCount := len(fields)
	if _, ok := fields[optional]; optional != "" && ok {
		requiredCount--
	}
	if requiredCount != len(wanted) {
		return nil, errors.New("required current entry field missing")
	}
	return fields, nil
}

func (e *windowsCanonicalEntrySetJournalV2) UnmarshalJSON(raw []byte) error {
	f, err := canonicalCurrentObjectWithOptional(raw, "clean_archive_sha256", "schema", "transaction_id", "canonical_entry", "staged_entry", "bundle", "expected", "moves", "cleanup", "prior_runtime")
	if err != nil {
		return err
	}
	if raw := f["clean_archive_sha256"]; raw != nil {
		var digest string
		if err := json.Unmarshal(raw, &digest); err != nil || !validLowerHexSHA256(digest) {
			return errors.Join(err, errors.New("invalid clean archive digest"))
		}
	}
	object := func(raw json.RawMessage, nullable bool, names ...string) (map[string]json.RawMessage, error) {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if nullable {
				return nil, nil
			}
			return nil, errors.New("required current entry object is null")
		}
		return canonicalCurrentObject(raw, names...)
	}
	for key, names := range map[string][]string{
		"canonical_entry": {"target", "prior_present", "prior_sha256", "new_sha256", "baseline_asides"},
		"prior_runtime":   {"path", "present", "sha256"},
	} {
		if _, err := object(f[key], key == "prior_runtime", names...); err != nil {
			return err
		}
	}
	b, err := canonicalCurrentObjectWithOptional(f["bundle"], "retained", "relative_path", "before_kind", "before_directory", "before_inventory", "staged_directory", "disposition", "effect", "members", "directories")
	if err != nil {
		return err
	}
	if retained := b["retained"]; retained != nil {
		if _, err := canonicalCurrentObject(retained, "members_sha256", "singletons"); err != nil {
			return err
		}
	}
	identity := func(raw json.RawMessage) error {
		_, err := object(raw, false, "volume_serial_number", "file_index_high", "file_index_low", "creation_time")
		return err
	}
	directory := func(raw json.RawMessage, nullable bool) error {
		v, err := object(raw, nullable, "schema", "root", "relative_path", "created", "parent_identity", "directory_identity")
		if err != nil || v == nil {
			return err
		}
		return errors.Join(identity(v["parent_identity"]), identity(v["directory_identity"]))
	}
	if err := errors.Join(directory(b["before_directory"], true), directory(b["staged_directory"], true)); err != nil {
		return err
	}
	if !bytes.Equal(bytes.TrimSpace(b["before_inventory"]), []byte("null")) {
		var compactInventory bytes.Buffer
		if err := json.Compact(&compactInventory, b["before_inventory"]); err != nil {
			return err
		}
		if _, err := binaryadmission.ParseRuntimeInventory(append(compactInventory.Bytes(), '\n')); err != nil {
			return err
		}
	}
	disposition, err := canonicalCurrentObjectWithOptional(b["disposition"], "close_owner", "state", "transferred", "original_owner_pending", "disposition_pending", "close_error")
	if err != nil {
		return err
	}
	if owner := disposition["close_owner"]; owner != nil {
		if _, err := canonicalCurrentObject(owner, "pid", "started_at"); err != nil {
			return err
		}
	}
	array := func(raw json.RawMessage) ([]json.RawMessage, error) {
		if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' {
			return nil, errors.New("required current entry array is null or missing")
		}
		var v []json.RawMessage
		err := json.Unmarshal(raw, &v)
		return v, err
	}
	rows, err := array(b["members"])
	if err != nil {
		return err
	}
	for _, raw := range rows {
		v, err := object(raw, false, "member", "prior", "prior_absence", "staged", "prior_aside_relative", "successor_aside_relative")
		if err != nil {
			return err
		}
		for _, side := range []string{"prior", "staged"} {
			m, err := object(v[side], true, "member", "file", "native_identity", "parent_identity", "size", "attributes")
			if err != nil {
				return err
			}
			if m == nil {
				continue
			}
			if _, err := object(m["file"], false, "path", "sha256"); err != nil {
				return err
			}
			if err := errors.Join(identity(m["native_identity"]), identity(m["parent_identity"])); err != nil {
				return err
			}
		}
		if a, err := object(v["prior_absence"], true, "parent_relative", "parent_identity", "missing_relative"); err != nil {
			return err
		} else if a != nil {
			if err := identity(a["parent_identity"]); err != nil {
				return err
			}
		}
	}
	rows, err = array(b["directories"])
	if err != nil {
		return err
	}
	nativeResultFields := []string{"member", "source_relative", "destination_relative", "attempted", "native_status_known", "native_status", "applied", "readback", "error", "close_error", "original_owner_pending"}
	moveFields := append([]string{"prior"}, nativeResultFields...)
	for _, raw := range rows {
		v, err := object(raw, false, "original", "state", "disposition")
		if err != nil {
			return err
		}
		if err := directory(v["original"], false); err != nil {
			return err
		}
		if _, err := object(v["disposition"], true, nativeResultFields...); err != nil {
			return err
		}
	}
	rows, err = array(f["moves"])
	if err != nil {
		return err
	}
	for _, raw := range rows {
		if _, err := object(raw, false, moveFields...); err != nil {
			return err
		}
	}
	rows, err = array(f["cleanup"])
	if err != nil {
		return err
	}
	for _, raw := range rows {
		if _, err := object(raw, false, "transaction_id", "side", "member", "source_relative", "expected_sha256", "state", "purpose"); err != nil {
			return err
		}
	}
	type plain windowsCanonicalEntrySetJournalV2
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	decoded := windowsCanonicalEntrySetJournalV2(value)
	staged := f["staged_entry"]
	null := bytes.Equal(bytes.TrimSpace(staged), []byte("null"))
	if null != decoded.retainsSingleton(decoded.Expected.CanonicalEntryRelativePath) {
		return errors.New("current Launcher stage disagrees with retention")
	}
	if !null {
		if _, err := object(staged, false, "path", "sha256"); err != nil {
			return err
		}
	}
	*e = decoded
	return nil
}

// Only an original binary-admission staging owner can supply this carrier.
// The detached snapshot is journal DATA, never a constructor for this owner.
type stagedWindowsCanonicalEntrySetV1 struct {
	original       *binaryadmission.StagedWindowsCanonicalEntrySetV1
	prior          *binaryadmission.WindowsCanonicalEntryPriorSetV1
	priorAttempted bool
	priorError     error
}

type stagedWindowsCanonicalEntrySetV2 struct {
	mu                 sync.Mutex
	closeRequested     bool
	closeError         error
	selective          bool
	stageOwnsPrior     bool
	original           *binaryadmission.StagedWindowsCanonicalEntrySetV2
	prior              *binaryadmission.WindowsCanonicalEntryPriorSetV2
	priorAttempted     bool
	priorError         error
	directorySnapshots map[*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1]binaryadmission.WindowsCanonicalEntryMemberDirectorySnapshotV2
}

// stageCurrentEntryBundleForUpgrade receives the same native stage and prior
// pair even when preparation is partial. The caller closes any returned stage.
func stageCurrentEntryBundleForUpgrade(ctx context.Context, source *binaryadmission.WindowsCanonicalEntrySetV2, target string) (*stagedWindowsCanonicalEntrySetV2, error) {
	stage, prior, err := source.StageForUpgrade(ctx, target)
	if stage == nil {
		return nil, errors.Join(err, errors.New("original upgrade stage unavailable"))
	}
	if err == nil && prior == nil {
		err = errors.New("original upgrade prior unavailable")
	}
	return &stagedWindowsCanonicalEntrySetV2{
		stageOwnsPrior: true,
		selective:      err == nil && stage.RetainsRuntime(),
		original:       stage,
		prior:          prior,
		priorAttempted: true,
		priorError:     err,
	}, err
}

func (s *stagedWindowsCanonicalEntrySetV2) acquirePrior(ctx context.Context) (*binaryadmission.WindowsCanonicalEntryPriorSetV2, error) {
	if s == nil || s.original == nil {
		return nil, errors.New("original staged current entry set required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeRequested {
		return s.prior, errors.Join(s.priorError, errors.New("staged current entry carrier is closing"))
	}
	if s.priorAttempted {
		return s.prior, s.priorError
	}
	s.priorAttempted = true
	s.prior, s.priorError = s.original.AcquireOriginalPriorSet(ctx)
	if s.prior == nil && s.priorError == nil {
		s.priorError = errors.New("original current prior acquisition returned no owner")
	}
	return s.prior, s.priorError
}

type windowsCanonicalEntryBundlePreparedV2 struct {
	source                          *stagedWindowsCanonicalEntrySetV2
	snapshot                        binaryadmission.WindowsCanonicalEntrySetSnapshotV2
	targets                         binaryadmission.WindowsCanonicalEntrySetTargetsV2
	observation                     binaryadmission.WindowsCanonicalEntryPriorObservationV2
	priorLocations, stagedLocations map[string]string
	nativeResults                   []binaryadmission.WindowsCanonicalEntryMoveResultV1
	nativeErrors                    []error
	firstJournalBytes               []byte
}

func currentPriorSnapshot(ctx context.Context, source *stagedWindowsCanonicalEntrySetV2, target string) (productPairFileSnapshot, error) {
	result := productPairFileSnapshot{path: target}
	prior, err := source.acquirePrior(ctx)
	if err != nil {
		return result, err
	}
	targets, err := source.original.DestinationTargets()
	if err != nil {
		return result, err
	}
	root := filepath.Dir(targets.CanonicalEntryPath)
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return result, err
	}
	relative = filepath.ToSlash(relative)
	present, body, _, err := prior.ReadOriginal(ctx, relative)
	result.present, result.body = present, body
	return result, err
}

func currentStageProductPath(stage *binaryadmission.StagedWindowsCanonicalEntrySetV2) (string, error) {
	if stage == nil {
		return "", errors.New("current original stage required")
	}
	snapshot, err := stage.Snapshot()
	if err != nil {
		return "", err
	}
	targets, err := stage.DestinationTargets()
	if err != nil {
		return "", err
	}
	members, err := stage.Members()
	if err != nil {
		return "", err
	}
	for _, member := range members {
		if member.Member == snapshot.ProductRelativePath {
			if !currentEntryRelative(member.File.Path) || path.Dir(member.File.Path) != "." || member.File.SHA256 != snapshot.Product.SHA256 {
				return "", errors.New("current Product has no valid issued flat stage")
			}
			return filepath.Join(filepath.Dir(targets.CanonicalEntryPath), filepath.FromSlash(member.File.Path)), nil
		}
	}
	if stage.RetainsRuntime() {
		// The native selector left Product in place. The exact witness and
		// held Product are checked with the original cohort before use.
		return "", nil
	}
	return "", errors.New("current original stage has no Product member")
}

// qualifyRetainedPlan pairs the Stage receipt with the existing V3 selection
// rule. Only handed-off prior evidence and actual staged rows enter this plan.
func (s *stagedWindowsCanonicalEntrySetV2) qualifyRetainedPlan(ctx context.Context, snapshot binaryadmission.WindowsCanonicalEntrySetSnapshotV2, before binaryadmission.WindowsCanonicalEntryPriorObservationV2, staged []binaryadmission.WindowsCanonicalEntryMemberEvidenceV2) (*windowsCanonicalEntryRetainedRuntimeV2, map[string]bool, error) {
	if !s.selective {
		return nil, nil, nil
	}
	witness, err := s.original.RetainedMembersWitness(ctx)
	if err != nil {
		return nil, nil, err
	}
	retained := &windowsCanonicalEntryRetainedRuntimeV2{MembersSHA256: witness.MembersSHA256, Singletons: slices.Clone(witness.Singletons)}
	e := windowsCanonicalEntrySetJournalV2{Expected: snapshot, Bundle: windowsCanonicalEntryBundleJournalV2{
		RelativePath: snapshot.BundleRelativePath, BeforeKind: before.BeforeKind,
		BeforeDirectory: before.BundleDirectory, BeforeInventory: before.Inventory, Retained: retained,
	}}
	for _, member := range before.Members {
		if member.Member == snapshot.CanonicalEntryRelativePath {
			e.CanonicalEntry.PriorPresent = true
			e.CanonicalEntry.PriorSHA256 = member.File.SHA256
			break
		}
	}
	omitted, _, err := currentEntryRetainedRuntime(&e)
	if err != nil {
		return nil, nil, err
	}
	evidence := make([]binaryadmission.WindowsCanonicalEntryMemberEvidenceV2, 0, len(omitted))
	for _, member := range before.Members {
		if omitted[member.Member] {
			evidence = append(evidence, member)
		}
	}
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Member < evidence[j].Member })
	raw, err := binaryadmission.CanonicalRuntimeJSON(evidence)
	if err != nil || len(evidence) != len(omitted) || binaryadmission.RuntimeDigest(raw) != witness.MembersSHA256 {
		return nil, nil, errors.Join(err, errors.New("retained plan differs from held original witness"))
	}

	pending := map[string]binaryadmission.RuntimeObjectV1{
		snapshot.CanonicalEntryRelativePath: {Kind: "file", SHA256: snapshot.CanonicalEntry.SHA256},
	}
	for _, object := range snapshot.BundleInventory {
		if object.Kind == "file" {
			pending[path.Join(snapshot.BundleRelativePath, object.Path)] = object
		}
	}
	for key := range omitted {
		delete(pending, key)
	}
	for _, member := range staged {
		plan, exists := pending[member.Member]
		if !exists || !currentEntryRelative(member.File.Path) || path.Dir(member.File.Path) != "." ||
			member.File.SHA256 != plan.SHA256 || plan.Size != nil && (member.Size != *plan.Size || member.Attributes != plan.Attributes) {
			return nil, nil, errors.New("selective stage has a foreign, retained or mismatched issued row")
		}
		delete(pending, member.Member)
	}
	if len(pending) != 0 {
		return nil, nil, errors.New("selective stage and retained witness do not cover the candidate")
	}
	return retained, omitted, ctx.Err()
}

func prepareCanonicalEntryBundle(o WindowsProductTxnOpts, id string) (*windowsCanonicalEntryBundlePreparedV2, *windowsCanonicalEntrySetJournalV2, error) {
	if o.EntryBundle == nil {
		return nil, nil, nil
	}
	if o.EntrySet != nil || o.Context == nil || o.EntryBundle.original == nil {
		return nil, nil, errors.New("one original current entry arm and context required")
	}
	source := o.EntryBundle
	if !source.selective {
		if err := source.original.VerifyUnchanged(o.Context); err != nil {
			return nil, nil, err
		}
	}
	snapshot, err := source.original.Snapshot()
	if err != nil {
		return nil, nil, err
	}
	targets, err := source.original.DestinationTargets()
	if err != nil {
		return nil, nil, err
	}
	if !sameWindowsProductPath(targets.CanonicalEntryPath, o.CanonicalEntryPath) || !sameWindowsProductPath(targets.ProductPath, o.ProductPath) || !sameWindowsProductPath(targets.IdentityPath, o.IdentityPath) {
		return nil, nil, errors.New("current stage disagrees with registry-derived transaction targets")
	}
	members, err := source.original.Members()
	if err != nil {
		return nil, nil, err
	}
	prior, err := source.acquirePrior(o.Context)
	if err != nil {
		return nil, nil, err
	}
	observation, err := prior.Observation(o.Context)
	if err != nil {
		return nil, nil, err
	}
	state := &windowsCanonicalEntryBundlePreparedV2{source: source, snapshot: snapshot, targets: targets, observation: observation, priorLocations: map[string]string{}, stagedLocations: map[string]string{}}
	arm := &windowsCanonicalEntrySetJournalV2{Schema: binaryadmission.WindowsCanonicalEntrySetSchemaV2, TransactionID: id, Expected: snapshot, Moves: []windowsCanonicalEntryMoveRecordV2{}, Cleanup: []windowsCanonicalEntryCleanupV2{}, Bundle: windowsCanonicalEntryBundleJournalV2{RelativePath: snapshot.BundleRelativePath, BeforeKind: observation.BeforeKind, BeforeDirectory: observation.BundleDirectory, BeforeInventory: observation.Inventory, Effect: "exact-members", Members: []windowsCanonicalEntryMemberJournalV2{}, Directories: []windowsCanonicalEntryDirectoryJournalV2{}, Disposition: windowsCanonicalEntryDispositionV2{State: "staged", OriginalOwnerPending: true, DispositionPending: true}}}
	retained, omitted, err := source.qualifyRetainedPlan(o.Context, snapshot, observation, members)
	if err != nil {
		return state, arm, err
	}
	arm.Bundle.Retained = retained
	rows := map[string]*windowsCanonicalEntryMemberJournalV2{}
	for i := range members {
		member := members[i]
		if _, duplicate := rows[member.Member]; duplicate {
			return state, arm, errors.New("duplicate issued current stage member")
		}
		rows[member.Member] = &windowsCanonicalEntryMemberJournalV2{Member: member.Member, Staged: &member}
		state.stagedLocations[member.Member] = member.File.Path
	}
	for i := range observation.Members {
		member := observation.Members[i]
		if omitted[member.Member] {
			continue
		}
		row := rows[member.Member]
		if row == nil {
			row = &windowsCanonicalEntryMemberJournalV2{Member: member.Member}
			rows[member.Member] = row
		}
		if row.Prior != nil {
			return state, arm, errors.New("duplicate original current prior member")
		}
		row.Prior = &member
		state.priorLocations[member.Member] = member.File.Path
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		row := rows[key]
		if row.Prior != nil {
			aside := fmt.Sprintf(".mcphub-%s-prior-%d.stage", id, i)
			row.PriorAsideRelative = &aside
		} else {
			absence, ok := observation.Absences[key]
			if !ok {
				return state, arm, errors.New("new-only current member has no original absence")
			}
			row.PriorAbsence = &absence
		}
		if row.Staged != nil {
			aside := fmt.Sprintf(".mcphub-%s-successor-%d.stage", id, i)
			row.SuccessorAsideRelative = &aside
		}
		arm.Bundle.Members = append(arm.Bundle.Members, *row)
	}
	entry := currentEntryMember(arm, snapshot.CanonicalEntryRelativePath)
	product := currentEntryMember(arm, snapshot.ProductRelativePath)
	launcherRetained := omitted[snapshot.CanonicalEntryRelativePath]
	productRetained := omitted[snapshot.ProductRelativePath]
	if !launcherRetained && (entry == nil || entry.Staged == nil) || !productRetained && (product == nil || product.Staged == nil) {
		return state, arm, errors.New("current stage lacks canonical file projections")
	}
	root := filepath.Dir(targets.CanonicalEntryPath)
	if productRetained {
		if o.StagedProduct != "" {
			return state, arm, errors.New("retained Product cannot carry a staged path")
		}
	} else if !sameWindowsProductPath(filepath.Join(root, filepath.FromSlash(product.Staged.File.Path)), o.StagedProduct) {
		return state, arm, errors.New("StagedProduct is not the issued original flat Product stage")
	}
	arm.CanonicalEntry = windowsProductRecoveryArtifactV3{Target: targets.CanonicalEntryPath, NewSHA256: snapshot.CanonicalEntry.SHA256, BaselineAsides: []windowsProductRecoveryAsideV3{}}
	if launcherRetained {
		arm.CanonicalEntry.PriorPresent = true
		arm.CanonicalEntry.PriorSHA256 = snapshot.CanonicalEntry.SHA256
	} else {
		arm.CanonicalEntry.NewSHA256 = entry.Staged.File.SHA256
		if entry.Prior != nil {
			arm.CanonicalEntry.PriorPresent = true
			arm.CanonicalEntry.PriorSHA256 = entry.Prior.File.SHA256
		}
		arm.StagedEntry = windowsProductRecoveryStagedV3{Path: filepath.Join(root, filepath.FromSlash(entry.Staged.File.Path)), SHA256: entry.Staged.File.SHA256}
	}
	if o.Mode == WindowsProductPairModeUpgrade {
		view, err := prior.RuntimeProduct(o.Context)
		if err != nil {
			return state, arm, err
		}
		arm.PriorRuntime = &windowsCanonicalEntryRuntimePreimageV1{Path: targets.CanonicalEntryPath}
		if view != nil {
			if err := view.ValidateOriginal(o.Context); err != nil {
				return state, arm, err
			}
			artifact := view.Artifact()
			arm.PriorRuntime = &windowsCanonicalEntryRuntimePreimageV1{Path: artifact.Path, Present: true, SHA256: artifact.SHA256}
		}
	}
	return state, arm, nil
}

func (t *WindowsProductTxn) receiveOriginalMemberDirectories() error {
	p := t.prepared
	if p.entryBundle == nil {
		return nil
	}
	s := p.entryBundle.source
	owners, ensureErr := s.original.EnsureOriginalMemberDirectories(t.Opts.Context)
	// Native bank retains every issued partial reference before errors; this
	// returned union supplies journal data, never the lifecycle ownership truth.
	if s.directorySnapshots == nil {
		s.directorySnapshots = map[*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1]binaryadmission.WindowsCanonicalEntryMemberDirectorySnapshotV2{}
	}
	byPath := map[string]binaryadmission.WindowsCanonicalEntryMemberDirectorySnapshotV2{}
	for _, d := range p.entryBundle.observation.Directories {
		byPath[d.RelativePath] = d
	}
	var receiveErrs []error
	for _, owner := range owners {
		if owner == nil {
			receiveErrs = append(receiveErrs, errors.New("nil original current directory owner"))
			continue
		}
		d, err := owner.SnapshotForEntryMembersV2()
		if err == nil {
			s.directorySnapshots[owner] = d
		}
		receiveErrs = append(receiveErrs, err)
		if err != nil {
			continue
		}
		if before, ok := byPath[d.RelativePath]; ok && before != d {
			receiveErrs = append(receiveErrs, errors.New("current ancestor original identities disagree"))
			continue
		}
		byPath[d.RelativePath] = d
	}
	keys := make([]string, 0, len(byPath))
	for key := range byPath {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := strings.Count(keys[i], "/"), strings.Count(keys[j], "/")
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	for _, key := range keys {
		d := byPath[key]
		state := "retained"
		if d.Created {
			state = "created_identity_received"
		}
		p.journal.EntryBundle.Bundle.Directories = append(p.journal.EntryBundle.Bundle.Directories, windowsCanonicalEntryDirectoryJournalV2{Original: d, State: state})
	}
	return errors.Join(append([]error{ensureErr}, receiveErrs...)...)
}

func currentBundleDisposition(e *windowsCanonicalEntrySetJournalV2) windowsProductTargetDispositionV1 {
	result := windowsProductTargetIdentical
	for _, row := range e.Bundle.Members {
		if row.Prior == nil {
			result = windowsProductTargetCreate
			continue
		}
		if e.Bundle.Retained != nil || row.Staged == nil || row.Prior.File.SHA256 != row.Staged.File.SHA256 {
			return windowsProductTargetReplace
		}
	}
	return result
}

func currentNativeResultRecord(result binaryadmission.WindowsCanonicalEntryMoveResultV1) binaryadmission.WindowsCanonicalEntryNativeResultRecordV2 {
	render := func(err error) *string {
		if err == nil {
			return nil
		}
		text := err.Error()
		return &text
	}
	return binaryadmission.WindowsCanonicalEntryNativeResultRecordV2{Member: result.Member, SourceRelative: result.SourceRelative, DestinationRelative: result.DestinationRelative, Attempted: result.Attempted, NativeStatusKnown: result.NativeStatusKnown, NativeStatus: result.NativeStatus, Applied: result.Applied, Readback: result.Readback, Error: render(result.Error), CloseError: render(result.CloseError), OriginalOwnerPending: result.OriginalOwnerPending}
}

func currentMoveRecord(result binaryadmission.WindowsCanonicalEntryMoveResultV1, prior bool, source, destination string) windowsCanonicalEntryMoveRecordV2 {
	v := currentNativeResultRecord(result)
	return windowsCanonicalEntryMoveRecordV2{Member: v.Member, Prior: prior, SourceRelative: source, DestinationRelative: destination, Applied: v.Applied, Readback: v.Readback, NativeStatusKnown: v.NativeStatusKnown, NativeStatus: v.NativeStatus, Attempted: v.Attempted, Error: v.Error, CloseError: v.CloseError, OriginalOwnerPending: v.OriginalOwnerPending}
}

func currentOriginalMemberObservation(ctx context.Context, source *stagedWindowsCanonicalEntrySetV2, prior bool, member string) (binaryadmission.WindowsCanonicalEntryMemberEvidenceV2, error) {
	if ctx == nil || source == nil || source.original == nil {
		return binaryadmission.WindowsCanonicalEntryMemberEvidenceV2{}, errors.New("current original observation owner and context required")
	}
	if prior {
		if source.prior == nil {
			return binaryadmission.WindowsCanonicalEntryMemberEvidenceV2{}, errors.New("current original prior owner required")
		}
		return source.prior.ObserveOriginalMember(ctx, member)
	}
	return source.original.ObserveOriginalMember(ctx, member)
}

func (t *WindowsProductTxn) moveCurrentEntryOriginal(ctx context.Context, prior bool, member, destination string) error {
	p := t.prepared
	e := p.journal.EntryBundle
	state := p.entryBundle
	locations := state.stagedLocations
	if prior {
		locations = state.priorLocations
	}
	observation, observeErr := currentOriginalMemberObservation(ctx, state.source, prior, member)
	if observeErr != nil {
		state.nativeErrors = append(state.nativeErrors, observeErr)
		return observeErr
	}
	row := currentEntryMember(e, member)
	if row == nil {
		return errors.New("current move has no issued original")
	}
	original := row.Staged
	if prior {
		original = row.Prior
	}
	if original == nil || observation.NativeIdentity != original.NativeIdentity || observation.File.SHA256 != original.File.SHA256 || observation.Size != original.Size || observation.Attributes != original.Attributes || !currentEntryRelative(observation.File.Path) {
		return errors.New("current native member observation differs from its original plan")
	}
	source := observation.File.Path
	locations[member] = source
	e.Moves = append(e.Moves, windowsCanonicalEntryMoveRecordV2{Member: member, Prior: prior, SourceRelative: source, DestinationRelative: destination})
	index := len(e.Moves) - 1
	if err := t.persistProductJournal(); err != nil {
		return err
	}
	var result binaryadmission.WindowsCanonicalEntryMoveResultV1
	var err error
	if prior {
		result, err = state.source.prior.MoveOriginal(ctx, member, destination)
	} else {
		result, err = state.source.original.MoveOriginal(ctx, member, destination)
	}
	state.nativeResults = append(state.nativeResults, result)
	state.nativeErrors = append(state.nativeErrors, err)
	record := currentMoveRecord(result, prior, source, destination)
	record.Member = member
	e.Moves[index] = record
	if result.Applied {
		locations[member] = destination
	}
	received := canonicalEntryMoveResultError(result, err, member, destination)
	if result.SourceRelative != "" && result.SourceRelative != source {
		received = errors.Join(received, errors.New("native current move source differs from persisted intent"))
	}
	return errors.Join(received, t.persistProductJournal())
}

func currentPromotionOrder(e *windowsCanonicalEntrySetJournalV2, prior bool) []string {
	keys := []string{}
	for _, row := range e.Bundle.Members {
		if prior && row.Prior != nil || !prior && row.Staged != nil {
			keys = append(keys, row.Member)
		}
	}
	rank := func(key string) int {
		switch key {
		case e.Expected.CanonicalEntryRelativePath:
			return 4
		case e.Expected.ProductRelativePath:
			return 3
		case e.Expected.IdentityRelativePath:
			return 2
		case e.Expected.RuntimePayload.Path:
			return 1
		default:
			return 0
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := rank(keys[i]), rank(keys[j])
		if a != b {
			if prior {
				return a > b
			}
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
}

func (t *WindowsProductTxn) promoteCurrentEntryBundle(ctx context.Context) error {
	e := t.prepared.journal.EntryBundle
	e.Bundle.Disposition.State = "apply-pending"
	for _, key := range currentPromotionOrder(e, true) {
		row := currentEntryMember(e, key)
		if err := t.moveCurrentEntryOriginal(ctx, true, key, *row.PriorAsideRelative); err != nil {
			return err
		}
	}
	for _, key := range currentPromotionOrder(e, false) {
		if err := t.moveCurrentEntryOriginal(ctx, false, key, key); err != nil {
			return err
		}
		if t.prepared.deps.Fault != nil {
			var stage WindowsProductStage
			switch key {
			case e.Expected.ProductRelativePath:
				stage = WindowsProductStageProductPromoted
			case e.Expected.IdentityRelativePath:
				stage = WindowsProductStageIdentityPromoted
			case e.Expected.CanonicalEntryRelativePath:
				stage = WindowsProductStageCanonicalEntryPromoted
			}
			if stage != "" {
				if err := t.prepared.deps.Fault(stage); err != nil {
					return err
				}
			}
		}
	}
	if err := t.Opts.EntryBundle.original.VerifyUnchanged(ctx); err != nil {
		return err
	}
	actual, err := t.Opts.EntryBundle.original.PublishedSnapshot(ctx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, e.Expected) {
		return errors.New("current complete cohort readback differs from original projection")
	}
	return nil
}

func readCanonicalEntryBundle(ctx context.Context, targets binaryadmission.WindowsCanonicalEntrySetTargetsV2, expected binaryadmission.WindowsCanonicalEntrySetSnapshotV2) (actual binaryadmission.WindowsCanonicalEntrySetSnapshotV2, retErr error) {
	owner, err := binaryadmission.AcquireWindowsCanonicalEntrySetV2(ctx, targets, expected)
	if owner != nil {
		defer func() { retErr = errors.Join(retErr, owner.Close()) }()
	}
	if err != nil {
		return actual, err
	}
	if owner == nil {
		return actual, errors.New("current cohort receiving returned no original owner")
	}
	return owner.Snapshot()
}

func inspectCurrentEntryPreflight(o WindowsProductTxnOpts) (result windowsProductPairPreflightV1, retErr error) {
	if o.Context == nil || o.EntryBundle == nil || o.EntryBundle.original == nil || o.EntrySet != nil {
		return result, errors.New("one current original entry stage and context required")
	}
	if !o.EntryBundle.selective {
		if err := o.EntryBundle.original.VerifyUnchanged(o.Context); err != nil {
			return result, err
		}
	}
	snapshot, err := o.EntryBundle.original.Snapshot()
	if err != nil {
		return result, err
	}
	targets, err := o.EntryBundle.original.DestinationTargets()
	if err != nil {
		return result, err
	}
	if !sameWindowsProductPath(targets.CanonicalEntryPath, o.CanonicalEntryPath) || !sameWindowsProductPath(targets.ProductPath, o.ProductPath) || !sameWindowsProductPath(targets.IdentityPath, o.IdentityPath) {
		return result, errors.New("current original target projection differs")
	}
	newMembers, err := o.EntryBundle.original.Members()
	if err != nil {
		return result, err
	}
	prior, err := o.EntryBundle.acquirePrior(o.Context)
	if err != nil {
		return result, err
	}
	before, err := prior.Observation(o.Context)
	if err != nil {
		return result, err
	}
	_, omitted, err := o.EntryBundle.qualifyRetainedPlan(o.Context, snapshot, before, newMembers)
	if err != nil {
		return result, err
	}
	old := map[string]string{}
	for _, member := range before.Members {
		if !omitted[member.Member] {
			old[member.Member] = member.File.SHA256
		}
	}
	disposition := windowsProductTargetIdentical
	productStage := ""
	for _, member := range newMembers {
		if member.Member == snapshot.ProductRelativePath {
			productStage = member.File.Path
		}
		digest, present := old[member.Member]
		if !present {
			if disposition != windowsProductTargetReplace {
				disposition = windowsProductTargetCreate
			}
		} else if o.EntryBundle.selective || digest != member.File.SHA256 {
			disposition = windowsProductTargetReplace
		}
		delete(old, member.Member)
	}
	if len(old) != 0 {
		disposition = windowsProductTargetReplace
	}
	if omitted[snapshot.ProductRelativePath] {
		if o.StagedProduct != "" {
			return result, errors.New("retained Product cannot carry a staged path")
		}
	} else if productStage == "" || !sameWindowsProductPath(filepath.Join(filepath.Dir(targets.CanonicalEntryPath), filepath.FromSlash(productStage)), o.StagedProduct) {
		return result, errors.New("Product stage is not the issued current original")
	}
	view, err := prior.RuntimeProduct(o.Context)
	if err != nil {
		return result, err
	}
	if view != nil {
		if err := view.ValidateOriginal(o.Context); err != nil {
			return result, err
		}
		artifact := view.Artifact()
		result.PriorCLI = windowsProductPreimageV1{Present: true, SHA256: artifact.SHA256}
	}
	legacy, err := snapshotProductPairFile(o.LegacyWindowlessPath)
	if err != nil {
		return result, err
	}
	result.PriorWindowless = windowsProductPreimage(legacy)
	result.Disposition = windowsProductPairDispositionV1{CLI: disposition, Windowless: windowsProductTargetIdentical}
	return result, o.Context.Err()
}

func cleanupCurrentEntryLegacyFiles(o WindowsProductTxnOpts, p *windowsProductPreparedTxn, forward bool) error {
	if p.legacyRetained != "" {
		if err := removeBoundWindowsProductFileIfPresent(p.legacyRetained, p.journal.LegacyWindowless.PriorSHA256, p.deps); err != nil {
			return err
		}
	}
	if len(p.journal.LegacyWindowless.BaselineAsides) != 0 {
		return errors.Join(ErrWindowsProductCleanupPending, errors.New("legacy original aside custody requires receiving"))
	}
	for _, target := range []string{o.LegacyWindowlessPath, filepath.Join(filepath.Dir(o.ProductPath), "mcphub-pe-admit.exe")} {
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(ErrWindowsProductCleanupPending, fmt.Errorf("retired original target absence unconfirmed: %v", err))
		}
	}
	return nil
}

func closeCurrentEntryFiles(source *stagedWindowsCanonicalEntrySetV2, beforeStageClose func()) error {
	if source == nil {
		return nil
	}
	if source.stageOwnsPrior {
		return source.original.CloseMembers()
	}
	var priorErr, stageErr error
	if source.prior != nil {
		// The unpublished BUNDLE stage uses the original guarded parent
		// path only after the prior borrower has closed successfully.
		priorErr = source.prior.Close()
	}
	if source.original != nil {
		if beforeStageClose != nil {
			beforeStageClose()
		}
		stageErr = source.original.Close()
	}
	return errors.Join(stageErr, priorErr)
}

func orderedOriginalMemberDirectories(input []*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1, snapshots map[*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1]binaryadmission.WindowsCanonicalEntryMemberDirectorySnapshotV2) []*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1 {
	owners := append([]*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1(nil), input...)
	for i, j := 0, len(owners)-1; i < j; i, j = i+1, j-1 {
		owners[i], owners[j] = owners[j], owners[i]
	}
	sort.SliceStable(owners, func(i, j int) bool {
		a, aok := snapshots[owners[i]]
		b, bok := snapshots[owners[j]]
		if aok != bok {
			return !aok
		}
		if !aok {
			return false
		}
		ad, bd := strings.Count(a.RelativePath, "/"), strings.Count(b.RelativePath, "/")
		if ad != bd {
			return ad > bd
		}
		return a.RelativePath > b.RelativePath
	})
	return owners
}

func orderedCurrentDirectories(source *stagedWindowsCanonicalEntrySetV2) []*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1 {
	if source == nil {
		return nil
	}
	if source.original == nil {
		return nil
	}
	return source.original.Directories()
}

// This sole carrier path receives all member borrowers before the native
// deepest-first lenders. Acquisition errors remain with their original caller;
// closure errors stay sticky while each repeated supported receive still runs.
func (source *stagedWindowsCanonicalEntrySetV2) Close() error {
	return source.closeWithStageObservation(nil)
}

func (source *stagedWindowsCanonicalEntrySetV2) closeWithStageObservation(beforeStageClose func()) error {
	if source == nil {
		return nil
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	source.closeRequested = true
	if source.stageOwnsPrior {
		if beforeStageClose != nil {
			beforeStageClose()
		}
		source.closeError = errors.Join(source.closeError, source.original.Close())
		return source.closeError
	}
	owners := orderedCurrentDirectories(source)
	err := settleCurrentEntryReferences(
		func() error { return closeCurrentEntryFiles(source, beforeStageClose) },
		owners,
		func(owner *binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1) error { return owner.Close() },
	)
	source.closeError = errors.Join(source.closeError, err)
	return source.closeError
}

// The order/error-continuation operation is shared by the carrier and its inert
// failure trace; it introduces no alternate native resource issuer or closer.
func settleCurrentEntryReferences(members func() error, owners []*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1, closeDirectory func(*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1) error) error {
	causes := []error{members()}
	for _, owner := range owners {
		if owner != nil {
			causes = append(causes, closeDirectory(owner))
		}
	}
	return errors.Join(causes...)
}

func closeCurrentEntryOwners(source *stagedWindowsCanonicalEntrySetV2) error { return source.Close() }

func (t *WindowsProductTxn) discardCurrentEntryOriginal(prior bool, member, location string, purpose binaryadmission.WindowsCanonicalEntryRecoveryPurposeV2, persist bool) error {
	p := t.prepared
	e := p.journal.EntryBundle
	state := p.entryBundle
	row := currentEntryMember(e, member)
	if row == nil {
		return errors.New("unissued current cleanup member")
	}
	original := row.Staged
	side := "unused-successor"
	if prior {
		original = row.Prior
		side = "prior"
	}
	if original == nil || location == member {
		return errors.New("current cleanup has no nonfinal original")
	}
	index := -1
	for i := range e.Cleanup {
		v := &e.Cleanup[i]
		if v.Side == side && v.Member == member {
			index = i
			if v.SourceRelative != location || v.Purpose != purpose {
				return errors.New("current cleanup intent changed")
			}
			if v.State == "absence_received" {
				path := filepath.Join(filepath.Dir(e.CanonicalEntry.Target), filepath.FromSlash(location))
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					return errors.Join(ErrWindowsProductCleanupPending, err, errors.New("received current cleanup absence is now occupied or unreadable"))
				}
				return nil
			}
			break
		}
	}
	if index < 0 {
		e.Cleanup = append(e.Cleanup, windowsCanonicalEntryCleanupV2{TransactionID: e.TransactionID, Side: side, Member: member, SourceRelative: location, ExpectedSHA256: original.File.SHA256, State: "retained", Purpose: purpose})
		index = len(e.Cleanup) - 1
	}
	e.Cleanup[index].State = "discard_intent"
	if persist {
		if err := t.persistProductJournal(); err != nil {
			return err
		}
	}
	ctx := context.WithoutCancel(t.Opts.Context)
	var actual binaryadmission.WindowsCanonicalEntryMoveResultV1
	var err error
	if prior {
		actual, err = state.source.prior.DiscardOriginal(ctx, member, location)
	} else {
		actual, err = state.source.original.DiscardOriginal(ctx, member, location)
	}
	state.nativeResults = append(state.nativeResults, actual)
	state.nativeErrors = append(state.nativeErrors, err)
	if actual.Applied && actual.Readback {
		e.Cleanup[index].State = "absence_received"
	}
	received := canonicalEntryDiscardResultError(actual, err, member, location)
	if persist {
		received = errors.Join(received, t.persistProductJournal())
	}
	return received
}

func (t *WindowsProductTxn) cleanupCurrentEntryFiles(committed, persist bool) error {
	p := t.prepared
	e := p.journal.EntryBundle
	purpose := binaryadmission.WindowsCanonicalRecoveryRollbackCleanupV2
	if committed {
		purpose = binaryadmission.WindowsCanonicalRecoveryCommittedForwardV2
	}
	for _, row := range e.Bundle.Members {
		if row.Staged != nil {
			location := p.entryBundle.stagedLocations[row.Member]
			if location != "" && location != row.Member {
				if err := t.discardCurrentEntryOriginal(false, row.Member, location, purpose, persist); err != nil {
					return err
				}
			}
		}
		if committed && row.Prior != nil {
			location := p.entryBundle.priorLocations[row.Member]
			if location == row.Member {
				return errors.New("committed prior original was not displaced")
			}
			if err := t.discardCurrentEntryOriginal(true, row.Member, location, purpose, persist); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *WindowsProductTxn) disposeCurrentEntryDirectories(persist bool) error {
	p := t.prepared
	e := p.journal.EntryBundle
	var causes []error
	effectsHeld := false
	var closeErrors []error
	for _, owner := range orderedCurrentDirectories(p.entryBundle.source) {
		if owner == nil {
			continue
		}
		original, known := p.entryBundle.source.directorySnapshots[owner]
		if !known {
			var err error
			original, err = owner.SnapshotForEntryMembersV2()
			if err != nil {
				causes = append(causes, err)
				effectsHeld = true
			} else {
				known = true
			}
		}
		if known && original.Created && !effectsHeld {
			index := -1
			for k := range e.Bundle.Directories {
				if e.Bundle.Directories[k].Original == original {
					index = k
					break
				}
			}
			if index < 0 {
				causes = append(causes, errors.New("created current directory lacks exact original evidence"))
				effectsHeld = true
			} else if e.Bundle.Directories[index].State == "absence_received" {
				if _, err := os.Lstat(filepath.Join(original.Root, filepath.FromSlash(original.RelativePath))); !errors.Is(err, os.ErrNotExist) {
					causes = append(causes, errors.Join(ErrWindowsProductCleanupPending, err))
					effectsHeld = true
				}
			} else {
				e.Bundle.Directories[index].State = "dispose_intent"
				var effectErr error
				if persist {
					effectErr = t.persistProductJournal()
				}
				if effectErr == nil {
					actual, err := owner.DisposeOriginalIfEmpty(context.WithoutCancel(t.Opts.Context))
					p.entryBundle.nativeResults = append(p.entryBundle.nativeResults, actual)
					p.entryBundle.nativeErrors = append(p.entryBundle.nativeErrors, err)
					record := currentNativeResultRecord(actual)
					e.Bundle.Directories[index].Disposition = &record
					if actual.Applied && actual.Readback {
						e.Bundle.Directories[index].State = "absence_received"
					}
					effectErr = canonicalEntryDiscardResultError(actual, err, actual.Member, original.RelativePath)
					if persist {
						effectErr = errors.Join(effectErr, t.persistProductJournal())
					}
				}
				if effectErr != nil {
					causes = append(causes, effectErr)
					effectsHeld = true
				}
			}
		}
		// A child lender is closed before its parent can be disposed. Failure
		// stops further effects, while every remaining resource Close still runs.
		if err := owner.Close(); err != nil {
			causes = append(causes, err)
			closeErrors = append(closeErrors, err)
			p.entryBundle.nativeErrors = append(p.entryBundle.nativeErrors, err)
			effectsHeld = true
		}
	}
	if err := errors.Join(closeErrors...); err != nil {
		rendered := err.Error()
		e.Bundle.Disposition.CloseError = &rendered
		e.Bundle.Disposition.OriginalOwnerPending = true
	}
	return errors.Join(causes...)
}

func (t *WindowsProductTxn) settleUnpublishedCurrentPreparation(transferred bool) error {
	p := t.prepared
	present, inspectErr := inspectWindowsProductRecoveryLeaves(t.Opts.StateDir)
	var cleanupErr error
	if !present && inspectErr == nil && transferred {
		p.journal.EntryBundle.Bundle.Disposition.State = "rolled-back"
		cleanupErr = t.cleanupCurrentEntryFiles(false, false)
		if cleanupErr == nil {
			cleanupErr = closeCurrentEntryFiles(t.Opts.EntryBundle, nil)
		}
		if cleanupErr == nil {
			cleanupErr = t.disposeCurrentEntryDirectories(false)
		}
	}
	// Once a writer was entered, a failed write never implies publication absence.
	// Positive same-fence absence above is the only unpublished cleanup path.
	if present || inspectErr != nil {
		p.journal.EntryBundle.Bundle.Disposition.State = "retained"
	}
	return errors.Join(inspectErr, cleanupErr, closeCurrentEntryOwners(t.Opts.EntryBundle))
}

// No move intent and no successor startup can precede a task rewrite for a
// nonempty changed cohort. An unreaped PREPARED transaction is also untouched.
func untouchedServingPreparation(j windowsProductRecoveryJournalV3) bool {
	return j.PriorServingAggregate != nil && j.Phase == WindowsProductRecoveryPrepared &&
		j.EntryBundle != nil && len(j.EntryBundle.Moves) == 0 &&
		(j.Quiescence == nil || len(j.EntryBundle.Bundle.Members) != 0)
}

func verifyServingRollbackReceipts(receipts windowsProductRecoveryReceiptsV3, read func(string) ([]byte, error)) error {
	for _, receipt := range []windowsProductPairRecoveryReceipt{receipts.V3, receipts.V2} {
		raw, err := read(receipt.Path)
		if !receipt.PriorPresent && windowsProductJournalCleanAbsence(err) {
			continue
		}
		if !receipt.PriorPresent || err != nil || !bytes.Equal(raw, receipt.PriorBytes) {
			return errors.Join(ErrWindowsProductRecoveryIdentity, errors.New("prior Product receipt changed before rollback"), err)
		}
	}
	return nil
}

func verifyUntouchedServingPreparation(j windowsProductRecoveryJournalV3, tasks WindowsProductTaskTxn, read func(string) ([]byte, error), hash func(string) (string, error)) error {
	if err := verifyServingRollbackReceipts(j.Receipts, read); err != nil {
		return err
	}
	verifier, ok := tasks.(interface{ VerifyRestored() error })
	if !ok {
		return errors.New("original prepared Task owner required")
	}
	if err := verifier.VerifyRestored(); err != nil {
		return err
	}
	legacy := j.LegacyWindowless
	digest, err := hash(legacy.Target)
	if !legacy.PriorPresent && windowsProductJournalCleanAbsence(err) {
		return nil
	}
	if !legacy.PriorPresent || err != nil || digest != legacy.PriorSHA256 {
		return errors.Join(ErrWindowsProductRecoveryIdentity, errors.New("prepared legacy preimage changed"), err)
	}
	return nil
}

func (t *WindowsProductTxn) rollbackCurrentEntryBundle(trigger error) error {
	o, p := t.Opts, t.prepared
	e := p.journal.EntryBundle
	ctx := context.WithoutCancel(o.Context)
	untouched := untouchedServingPreparation(p.journal)
	if untouched {
		if err := verifyUntouchedServingPreparation(p.journal, o.Tasks, p.deps.ReadReceipt, p.deps.HashFile); err != nil {
			return errors.Join(trigger, ErrWindowsProductRecoveryPending, err)
		}
	} else if p.journal.PriorServingAggregate != nil {
		if err := verifyServingRollbackReceipts(p.journal.Receipts, p.deps.ReadReceipt); err != nil {
			return errors.Join(trigger, ErrWindowsProductRecoveryPending, err)
		}
	}
	e.Bundle.Disposition.State = "rollback-pending"
	// The common rollback owner has settled the exact successor before entering.
	keys := currentPromotionOrder(e, false)
	for i := len(keys) - 1; i >= 0; i-- {
		key := keys[i]
		if p.entryBundle.stagedLocations[key] != key {
			continue
		}
		row := currentEntryMember(e, key)
		if err := t.moveCurrentEntryOriginal(ctx, false, key, *row.SuccessorAsideRelative); err != nil {
			return errors.Join(trigger, ErrWindowsProductRecoveryPending, err)
		}
	}
	keys = currentPromotionOrder(e, true)
	for i := len(keys) - 1; i >= 0; i-- {
		key := keys[i]
		if p.entryBundle.priorLocations[key] == key {
			continue
		}
		if err := t.moveCurrentEntryOriginal(ctx, true, key, key); err != nil {
			return errors.Join(trigger, ErrWindowsProductRecoveryPending, err)
		}
	}
	// Selected observations defer unrelated peer checks to settlement. Receive
	// both original banks before restoring Tasks, receipts or the prior runtime.
	if o.EntryBundle == nil || o.EntryBundle.prior == nil || o.EntryBundle.original == nil {
		return errors.Join(trigger, ErrWindowsProductRecoveryPending, errors.New("rollback original member banks required"))
	}
	if err := errors.Join(o.EntryBundle.prior.VerifyUnchanged(ctx), o.EntryBundle.original.VerifyUnchanged(ctx)); err != nil {
		return errors.Join(trigger, ErrWindowsProductRecoveryPending, err)
	}
	if !untouched {
		if err := o.Tasks.RestoreImport(); err != nil {
			return errors.Join(trigger, fmt.Errorf("%w: restore exact task XML: %w", ErrWindowsProductPairRecoveryRollback, err))
		}
		if err := restoreLegacyWindowlessPreimage(p, o.LegacyWindowlessPath); err != nil {
			return errors.Join(trigger, err)
		}
	}
	if p.journal.PriorServingAggregate == nil {
		for _, snapshot := range []productPairFileSnapshot{p.receiptV3Prior, p.receiptV2Prior} {
			if err := restoreProductPairFile(snapshot); err != nil {
				return errors.Join(trigger, err)
			}
		}
	}
	if e.PriorRuntime != nil {
		view, err := o.EntryBundle.prior.RuntimeProduct(ctx)
		if err != nil {
			return errors.Join(trigger, err)
		}
		if view != nil {
			if err := view.ValidateOriginal(ctx); err != nil {
				return errors.Join(trigger, err)
			}
		}
		if p.journal.PreSettlement != nil && p.journal.PreSettlement.GUIOwner == windowsProductGUIOwnerConfirmedPresent {
			if p.deps.RecoverPriorSettlement == nil {
				return errors.Join(trigger, ErrWindowsProductRecoveryPending)
			}
			if err := p.deps.RecoverPriorSettlement(p.journal); err != nil {
				return errors.Join(trigger, err)
			}
		} else if p.journal.PriorServingAggregate == nil && p.journal.PriorFleetReaped && view != nil && p.deps.RestartPrior != nil {
			artifact := view.Artifact()
			launch, err := p.deps.RestartPrior(artifact.Path)
			if err != nil {
				return errors.Join(trigger, err, abortStartedSupervisor(launch))
			}
			recoveryCtx, cancel := context.WithTimeout(context.Background(), defaultSupervisorLockReleaseTimeout)
			err = receiveStartedSupervisor(recoveryCtx, launch, supervisorExpectation(o.StateDir, artifact.Path, artifact.SHA256), func() error {
				if p.deps.WaitSupervisorReady != nil {
					return p.deps.WaitSupervisorReady(recoveryCtx, artifact.Path, artifact)
				}
				return nil
			})
			cancel()
			if err != nil {
				return errors.Join(trigger, err)
			}
		}
	}
	e.Bundle.Disposition.State = "rolled-back"
	if err := t.persistProductJournal(); err != nil {
		return errors.Join(trigger, err)
	}
	if err := t.cleanupCurrentEntryFiles(false, true); err != nil {
		return errors.Join(trigger, ErrWindowsProductCleanupPending, err)
	}
	if err := closeCurrentEntryFiles(o.EntryBundle, nil); err != nil {
		return errors.Join(trigger, ErrWindowsProductRecoveryPending, err)
	}
	if err := t.disposeCurrentEntryDirectories(true); err != nil {
		return errors.Join(trigger, ErrWindowsProductCleanupPending, err)
	}
	if err := closeCurrentEntryOwners(o.EntryBundle); err != nil {
		return errors.Join(trigger, ErrWindowsProductRecoveryPending, err)
	}
	e.Bundle.Disposition.State = "settled"
	e.Bundle.Disposition.DispositionPending = false
	e.Bundle.Disposition.OriginalOwnerPending = false
	p.journal.Phase = WindowsProductRecoverySettledRollback
	p.journal.TaskSetSHA256 = ""
	if err := t.persistProductJournal(); err != nil {
		return errors.Join(trigger, err)
	}
	if err := o.Tasks.DiscardRecoveryStore(); err != nil {
		return errors.Join(trigger, err)
	}
	if p.journal.PriorServingAggregate != nil {
		if t.transferAggregateRollback == nil {
			return errors.Join(trigger, ErrWindowsProductRecoveryPending)
		}
		if err := t.transferAggregateRollback(ctx, p.lastJournalBytes); err != nil {
			return errors.Join(trigger, ErrWindowsProductRecoveryPending, err)
		}
		return &productMaterialRollbackError{cause: trigger}
	}
	if err := p.deps.RemoveRecoveryJournal(filepath.Join(o.StateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
		return errors.Join(trigger, err)
	}
	return fmt.Errorf("%w; exact current Windows cohort rollback completed", trigger)
}

// The initiating error remains visible when material rollback succeeded. Any
// separate cleanup failure still inhibits post-release restoration this run.
type productMaterialRollbackError struct{ cause error }

func (e *productMaterialRollbackError) Error() string {
	return fmt.Sprintf("%v; Product material rollback completed", e.cause)
}
func (e *productMaterialRollbackError) Unwrap() error { return e.cause }
func soleMaterialRollbackError(err error) bool {
	switch e := err.(type) {
	case *productMaterialRollbackError:
		return true
	case interface{ Unwrap() []error }:
		causes := e.Unwrap()
		return len(causes) == 1 && soleMaterialRollbackError(causes[0])
	case interface{ Unwrap() error }:
		return soleMaterialRollbackError(e.Unwrap())
	default:
		return false
	}
}

func currentEntrySetReceipt(j windowsProductRecoveryJournalV3) *windowsCanonicalEntrySetReceiptV2 {
	e := j.EntryBundle
	if e == nil {
		return nil
	}
	s := e.Expected
	return &windowsCanonicalEntrySetReceiptV2{Schema: e.Schema, TransactionID: j.TransactionID, BuildID: s.BuildID, RegistryProjectionSHA256: s.RegistryProjectionSHA256, CanonicalEntryPath: e.CanonicalEntry.Target, CanonicalEntrySHA256: s.CanonicalEntry.SHA256, ProductPath: j.Product.Target, IdentityPath: s.Identity.Path, IdentitySHA256: s.Identity.SHA256, Machine: s.Machine}
}

func reconcileStagedCurrentEntryNoop(ctx context.Context, stateDir string, mode WindowsProductPairMode, source *stagedWindowsCanonicalEntrySetV2, reconcile func(UpgradeReceiptV3) error) (bool, error) {
	if ctx == nil || source == nil || source.original == nil || reconcile == nil {
		return false, errors.New("original current stage and receiving owner required")
	}
	if err := source.original.VerifyUnchanged(ctx); err != nil {
		return false, err
	}
	snapshot, err := source.original.Snapshot()
	if err != nil {
		return false, err
	}
	targets, err := source.original.DestinationTargets()
	if err != nil {
		return false, err
	}
	members, err := source.original.Members()
	if err != nil {
		return false, err
	}
	if source.selective && len(members) != 0 {
		return false, nil
	}
	prior, err := source.acquirePrior(ctx)
	if err != nil {
		return false, err
	}
	observation, err := prior.Observation(ctx)
	if err != nil {
		return false, err
	}
	if observation.BeforeKind != binaryadmission.WindowsCanonicalEntryBeforeBundleV2 {
		return false, nil
	}
	if source.selective {
		if _, _, err := source.qualifyRetainedPlan(ctx, snapshot, observation, members); err != nil {
			return false, err
		}
	} else {
		if len(members) != len(observation.Members) {
			return false, nil
		}
		old := make(map[string]string, len(observation.Members))
		for _, member := range observation.Members {
			old[member.Member] = member.File.SHA256
		}
		for _, member := range members {
			if old[member.Member] != member.File.SHA256 {
				return false, nil
			}
		}
	}
	if err := prior.VerifyUnchanged(ctx); err != nil {
		return false, err
	}
	var receiptMismatch error
	if source.selective {
		receiptMismatch = errors.New("zero-change current upgrade requires a matching V3 receipt")
	}
	raw, err := api.ReadStateFileInodeAnchored(filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, receiptMismatch
	}
	if err != nil {
		return false, err
	}
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil {
		return false, err
	}
	r := decoded.V3
	if r == nil || r.EntryBundle == nil || r.EntrySet != nil || r.Admission != UpgradeAdmissionCanonicalEntryProductV2 || !windowsProductPairReceiptModeMatches(r.Mode, mode) {
		return false, receiptMismatch
	}
	e := r.EntryBundle
	if r.Artifact.SHA256 != snapshot.Product.SHA256 || e.BuildID != snapshot.BuildID || e.RegistryProjectionSHA256 != snapshot.RegistryProjectionSHA256 || e.CanonicalEntrySHA256 != snapshot.CanonicalEntry.SHA256 || e.IdentitySHA256 != snapshot.Identity.SHA256 || !sameWindowsProductPath(e.CanonicalEntryPath, targets.CanonicalEntryPath) || !sameWindowsProductPath(e.ProductPath, targets.ProductPath) || !sameWindowsProductPath(e.IdentityPath, targets.IdentityPath) {
		return false, receiptMismatch
	}
	actual, err := source.original.PublishedSnapshot(ctx)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(actual, snapshot) {
		return false, errors.New("current complete no-op readback differs from original projection")
	}
	if err := reconcile(*r); err != nil {
		return false, err
	}
	return true, nil
}

func reconcileCurrentEntryCommitReceipt(ctx context.Context, stateDir string, j windowsProductRecoveryJournalV3, d WindowsProductTxnDeps) (bool, error) {
	if j.EntryBundle == nil || j.EntrySet != nil {
		return false, errors.New("one current entry journal arm required")
	}
	d = withWindowsProductDefaults(d)
	raw, err := d.ReadReceipt(filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil {
		return false, err
	}
	r := decoded.V3
	if r == nil || r.EntryBundle == nil || r.EntrySet != nil || r.Admission != UpgradeAdmissionCanonicalEntryProductV2 {
		return false, nil
	}
	if r.TransactionID != j.TransactionID || r.Artifact.SHA256 != j.Product.NewSHA256 || r.TaskSetSHA256 != j.TaskSetSHA256 || !reflect.DeepEqual(r.EntryBundle, currentEntrySetReceipt(j)) {
		return false, errors.New("current committed receipt differs from original journal")
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV2(j.EntryBundle.CanonicalEntry.Target)
	if err != nil {
		return false, err
	}
	if _, err := d.AdmitEntryBundle(ctx, targets, j.EntryBundle.Expected); err != nil {
		return false, err
	}
	if d.PublishCommitted == nil {
		return false, errors.New("current committed publisher unavailable")
	}
	_, err = d.PublishCommitted(WindowsProductCommitEvent{Schema: r.Schema, TransactionID: r.TransactionID, Mode: r.Mode, Artifact: r.Artifact, EntryBundle: r.EntryBundle, TaskSetSHA256: r.TaskSetSHA256, Retired: r.Retired, ReceiptSHA256: binaryadmission.RuntimeDigest(raw), InstalledAt: r.InstalledAt})
	return err == nil, err
}

func (s *stagedWindowsCanonicalEntrySetV1) acquirePrior(ctx context.Context) (*binaryadmission.WindowsCanonicalEntryPriorSetV1, error) {
	if s == nil || s.original == nil {
		return nil, errors.New("original staged entry set required")
	}
	if s.prior != nil || s.priorAttempted {
		return s.prior, s.priorError
	}
	s.priorAttempted = true
	s.prior, s.priorError = s.original.AcquireOriginalPriorSet(ctx)
	if s.prior == nil && s.priorError == nil {
		s.priorError = errors.New("original prior-set acquisition returned no owner")
	}
	return s.prior, s.priorError
}

type windowsCanonicalEntryDirectoryV1 struct {
	State           string                                                                     `json:"state,omitempty"`
	Absence         *binaryadmission.WindowsCanonicalEntryPayloadDirectoryAbsenceObservationV1 `json:"absence,omitempty"`
	CreationEntered bool                                                                       `json:"creation_entered,omitempty"`
	Original        *binaryadmission.WindowsCanonicalEntryPayloadDirectorySnapshotV1           `json:"original,omitempty"`
	Disposition     *windowsCanonicalEntryMoveRecordV1                                         `json:"disposition,omitempty"`
	Path            string                                                                     `json:"path"`
	PriorPresent    bool                                                                       `json:"prior_present"`
	Created         bool                                                                       `json:"created"`
}

type windowsCanonicalEntrySetJournalV1 struct {
	Cleanup          []windowsCanonicalEntryCleanupV1                   `json:"cleanup,omitempty"`
	Schema           string                                             `json:"schema"`
	TransactionID    string                                             `json:"transaction_id"`
	CanonicalEntry   windowsProductRecoveryArtifactV3                   `json:"canonical_entry"`
	Identity         windowsProductRecoveryArtifactV3                   `json:"identity"`
	StagedEntry      windowsProductRecoveryStagedV3                     `json:"staged_entry"`
	StagedIdentity   windowsProductRecoveryStagedV3                     `json:"staged_identity"`
	Expected         binaryadmission.WindowsCanonicalEntrySetSnapshotV1 `json:"expected"`
	PayloadDirectory windowsCanonicalEntryDirectoryV1                   `json:"payload_directory"`
	Moves            []windowsCanonicalEntryMoveRecordV1                `json:"moves,omitempty"`
	PriorRuntime     *windowsCanonicalEntryRuntimePreimageV1            `json:"prior_runtime,omitempty"`
}

type windowsCanonicalEntryCleanupV1 struct {
	TransactionID  string `json:"transaction_id"`
	Side           string `json:"side"`
	Member         string `json:"member"`
	SourceRelative string `json:"source_relative"`
	ExpectedSHA256 string `json:"expected_sha256"`
	State          string `json:"state"`
}
type windowsCanonicalEntryRuntimePreimageV1 struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	SHA256  string `json:"sha256"`
}

type windowsCanonicalEntrySetReceiptV1 struct {
	Schema                   string `json:"schema"`
	TransactionID            string `json:"transaction_id"`
	BuildID                  string `json:"build_id"`
	RegistryProjectionSHA256 string `json:"registry_projection_sha256"`
	CanonicalEntryPath       string `json:"canonical_entry_path"`
	CanonicalEntrySHA256     string `json:"canonical_entry_sha256"`
	ProductPath              string `json:"product_path"`
	IdentityPath             string `json:"identity_path"`
	IdentitySHA256           string `json:"identity_sha256"`
	Machine                  uint16 `json:"machine"`
}

type windowsCanonicalEntrySetPrepared struct {
	directory                         *binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1
	source                            *stagedWindowsCanonicalEntrySetV1
	snapshot                          binaryadmission.WindowsCanonicalEntrySetSnapshotV1
	stages                            binaryadmission.WindowsCanonicalEntrySetTargetsV1
	targets                           binaryadmission.WindowsCanonicalEntrySetTargetsV1
	entryPrior, identityPrior         productPairFileSnapshot
	entryPromotion, identityPromotion *WindowsProductPairPromotion
	entryWithdrawn                    string
	stagedLocations, priorLocations   map[string]string
	nativeResults                     []binaryadmission.WindowsCanonicalEntryMoveResultV1
	nativeErrors                      []error
}

func canonicalEntrySetJournalTargets(j windowsProductRecoveryJournalV3) (binaryadmission.WindowsCanonicalEntrySetTargetsV1, error) {
	if j.EntrySet == nil {
		return binaryadmission.WindowsCanonicalEntrySetTargetsV1{}, errors.New("canonical entry-set journal arm is required")
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV1(j.EntrySet.CanonicalEntry.Target)
	if err != nil {
		return targets, err
	}
	if !sameWindowsProductPath(targets.ProductPath, j.Product.Target) || !sameWindowsProductPath(targets.IdentityPath, j.EntrySet.Identity.Target) {
		return targets, errors.New("canonical entry-set journal targets disagree with registry")
	}
	return targets, nil
}

func validateCanonicalEntrySetJournal(j windowsProductRecoveryJournalV3) error {
	if j.EntryBundle != nil {
		if j.EntrySet != nil {
			return errors.New("legacy and current entry journal arms coexist")
		}
		return validateCanonicalEntryBundleJournal(j)
	}
	if err := validateOriginalPayloadDirectory(j); err != nil {
		return err
	}
	if err := validateCanonicalCleanup(j); err != nil {
		return err
	}
	e := j.EntrySet
	if e == nil {
		return nil
	}
	targets, err := canonicalEntrySetJournalTargets(j)
	if err != nil {
		return err
	}
	if j.Mode == WindowsProductPairModeUpgrade {
		r := e.PriorRuntime
		if r == nil {
			return errors.New("complete-set upgrade requires original prior runtime projection")
		}
		var prior windowsProductPreimageV1
		if sameWindowsProductPath(r.Path, e.CanonicalEntry.Target) {
			prior = windowsProductPreimageV1{Present: e.CanonicalEntry.PriorPresent, SHA256: e.CanonicalEntry.PriorSHA256}
		} else if sameWindowsProductPath(r.Path, j.Product.Target) {
			prior = windowsProductPreimageV1{Present: j.Product.PriorPresent, SHA256: j.Product.PriorSHA256}
		} else {
			return errors.New("prior runtime escaped exact entry-set targets")
		}
		if prior != (windowsProductPreimageV1{Present: r.Present, SHA256: r.SHA256}) {
			return errors.New("prior runtime does not match original member preimage")
		}
		if !r.Present && (e.CanonicalEntry.PriorPresent || j.Product.PriorPresent || e.Identity.PriorPresent) {
			return errors.New("partial entry set is not prior runtime absence")
		}
	}
	if e.Schema != windowsCanonicalEntrySetSchemaV1 || e.Expected.Schema != e.Schema || e.TransactionID != j.TransactionID || strings.TrimSpace(e.Expected.BuildID) == "" || !validLowerHexSHA256(e.Expected.RegistryProjectionSHA256) {
		return errors.New("invalid canonical entry-set journal identity")
	}
	for _, member := range []windowsProductRecoveryArtifactV3{e.CanonicalEntry, e.Identity} {
		if !validRecoveryPreimage(member.PriorPresent, member.PriorSHA256) || !validLowerHexSHA256(member.NewSHA256) {
			return errors.New("invalid canonical entry-set member preimage")
		}
		if err := validateWindowsProductRecoveryAsides(member.Target, member.BaselineAsides); err != nil {
			return err
		}
	}
	if !cleanAbsolutePath(e.StagedEntry.Path) || !cleanAbsolutePath(e.StagedIdentity.Path) || e.StagedEntry.SHA256 != e.CanonicalEntry.NewSHA256 || e.StagedIdentity.SHA256 != e.Identity.NewSHA256 {
		return errors.New("invalid canonical entry-set stages")
	}
	paths := []string{j.Staged.Path, e.StagedEntry.Path, e.StagedIdentity.Path, targets.CanonicalEntryPath, targets.ProductPath, targets.IdentityPath}
	for i, path := range paths {
		for k := 0; k < i; k++ {
			if sameWindowsProductPath(path, paths[k]) {
				return errors.New("canonical entry-set paths alias")
			}
		}
	}
	if e.Expected.ProductArtifact.Role != binaryadmission.WindowsArtifactRoleProduct || e.Expected.ProductArtifact.Subsystem != binaryadmission.WindowsGUISubsystem || e.Expected.ProductArtifact.Machine == 0 || e.Expected.ProductArtifact.SHA256 != j.Product.NewSHA256 || !sameWindowsProductPath(e.Expected.ProductArtifact.Path, j.Product.Target) {
		return errors.New("invalid canonical entry-set Product binding")
	}
	if fmt.Sprintf("%x", sha256.Sum256(e.Expected.IdentityBytes)) != e.Identity.NewSHA256 {
		return errors.New("canonical entry-set identity bytes differ")
	}
	if e.Expected.CanonicalEntry.SHA256 != e.CanonicalEntry.NewSHA256 || e.Expected.Product.SHA256 != j.Product.NewSHA256 || e.Expected.Identity.SHA256 != e.Identity.NewSHA256 || e.Expected.Machine != e.Expected.ProductArtifact.Machine {
		return errors.New("canonical entry-set expected member identities differ")
	}
	if !sameWindowsProductPath(e.PayloadDirectory.Path, filepath.Dir(targets.ProductPath)) || (e.PayloadDirectory.PriorPresent && e.PayloadDirectory.Created) {
		return errors.New("invalid canonical payload directory preimage")
	}
	return nil
}

func canonicalEntrySnapshotFromJournal(j windowsProductRecoveryJournalV3) binaryadmission.WindowsCanonicalEntrySetSnapshotV1 {
	s := j.EntrySet.Expected
	s.CanonicalEntry.Path = j.EntrySet.CanonicalEntry.Target
	s.Product.Path = j.Product.Target
	s.Identity.Path = j.EntrySet.Identity.Target
	s.ProductArtifact.Path = j.Product.Target
	s.IdentityBytes = append([]byte(nil), s.IdentityBytes...)
	return s
}
func readCanonicalEntrySet(ctx context.Context, targets binaryadmission.WindowsCanonicalEntrySetTargetsV1, expected binaryadmission.WindowsCanonicalEntrySetSnapshotV1) (actual binaryadmission.WindowsCanonicalEntrySetSnapshotV1, retErr error) {
	owner, err := binaryadmission.AcquireWindowsCanonicalEntrySetV1(ctx, targets, expected)
	if owner != nil {
		defer func() { retErr = errors.Join(retErr, owner.Close()) }()
	}
	if err != nil {
		return actual, err
	}
	if owner == nil {
		return actual, errors.New("canonical entry-set reader returned no original owner")
	}
	return owner.Snapshot()
}

func canonicalEntrySetReceipt(j windowsProductRecoveryJournalV3) *windowsCanonicalEntrySetReceiptV1 {
	if j.EntrySet == nil {
		return nil
	}
	e := j.EntrySet
	return &windowsCanonicalEntrySetReceiptV1{Schema: e.Schema, TransactionID: j.TransactionID, BuildID: e.Expected.BuildID, RegistryProjectionSHA256: e.Expected.RegistryProjectionSHA256, CanonicalEntryPath: e.CanonicalEntry.Target, CanonicalEntrySHA256: e.CanonicalEntry.NewSHA256, ProductPath: j.Product.Target, IdentityPath: e.Identity.Target, IdentitySHA256: e.Identity.NewSHA256, Machine: e.Expected.ProductArtifact.Machine}
}

func validateCanonicalEntrySetReceipt(receipt UpgradeReceiptV3) error {
	if receipt.EntryBundle != nil {
		if receipt.EntrySet != nil || receipt.Admission != UpgradeAdmissionCanonicalEntryProductV2 {
			return errors.New("current entry receipt and admission disagree")
		}
		e := receipt.EntryBundle
		if _, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(e.BuildID, e.CanonicalEntrySHA256, binaryadmission.WindowsCanonicalEntryReverseProjectionV1); err != nil {
			return err
		}
		targets, err := binaryadmission.CanonicalEntryTargetsV2(e.CanonicalEntryPath)
		if err != nil {
			return err
		}
		projection, err := binaryadmission.WindowsCanonicalEntryRegistryProjectionSHA256V2()
		if err != nil {
			return fmt.Errorf("current canonical entry registry projection: %w", err)
		}
		if e.Schema != binaryadmission.WindowsCanonicalEntrySetSchemaV2 || e.TransactionID != receipt.TransactionID || e.RegistryProjectionSHA256 != projection || !validLowerHexSHA256(e.CanonicalEntrySHA256) || !validLowerHexSHA256(e.IdentitySHA256) || e.Machine != receipt.Artifact.Machine || !sameWindowsProductPath(targets.ProductPath, e.ProductPath) || !sameWindowsProductPath(targets.IdentityPath, e.IdentityPath) {
			return errors.New("invalid current canonical entry receipt")
		}
		return nil
	}
	e := receipt.EntrySet
	if receipt.Admission == UpgradeAdmissionLocalProductV3 {
		if e != nil {
			return errors.New("legacy product receipt cannot certify an entry set")
		}
		return nil
	}
	if receipt.Admission != UpgradeAdmissionCanonicalEntryProductV1 || e == nil {
		return errors.New("canonical entry-set admission and arm disagree")
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV1(e.CanonicalEntryPath)
	if err != nil {
		return err
	}
	if e.Schema != windowsCanonicalEntrySetSchemaV1 || e.TransactionID != receipt.TransactionID || strings.TrimSpace(e.BuildID) == "" || !validLowerHexSHA256(e.RegistryProjectionSHA256) || !validLowerHexSHA256(e.CanonicalEntrySHA256) || !validLowerHexSHA256(e.IdentitySHA256) || e.Machine != receipt.Artifact.Machine || !sameWindowsProductPath(targets.ProductPath, e.ProductPath) || !sameWindowsProductPath(targets.IdentityPath, e.IdentityPath) {
		return errors.New("invalid canonical entry-set receipt")
	}
	return nil
}

func prepareCanonicalEntrySet(o WindowsProductTxnOpts, d WindowsProductTxnDeps, id string) (*windowsCanonicalEntrySetPrepared, *windowsCanonicalEntrySetJournalV1, error) {
	if o.EntrySet == nil {
		return nil, nil, nil
	}
	if o.Context == nil || o.EntrySet.original == nil {
		return nil, nil, errors.New("original staged canonical entry set and context are required")
	}
	if err := o.EntrySet.original.VerifyUnchanged(); err != nil {
		return nil, nil, err
	}
	stages, err := o.EntrySet.original.Targets()
	if err != nil {
		return nil, nil, err
	}
	targets, err := o.EntrySet.original.DestinationTargets()
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := o.EntrySet.original.Snapshot()
	if err != nil {
		return nil, nil, err
	}
	if !sameWindowsProductPath(targets.CanonicalEntryPath, o.CanonicalEntryPath) || !sameWindowsProductPath(targets.ProductPath, o.ProductPath) || !sameWindowsProductPath(targets.IdentityPath, o.IdentityPath) || !sameWindowsProductPath(stages.ProductPath, o.StagedProduct) {
		return nil, nil, errors.New("staged canonical entry set disagrees with transaction paths")
	}
	entryPrior, err := canonicalPriorSnapshot(o.EntrySet.prior, "launcher", targets.CanonicalEntryPath)
	if err != nil {
		return nil, nil, err
	}
	identityPrior, err := canonicalPriorSnapshot(o.EntrySet.prior, "identity", targets.IdentityPath)
	if err != nil {
		return nil, nil, err
	}
	payloadParentAbsent, err := o.EntrySet.prior.ObservePayloadParentAbsent(o.Context)
	if err != nil {
		return nil, nil, fmt.Errorf("observe original canonical payload parent: %w", err)
	}
	dir := windowsCanonicalEntryDirectoryV1{Path: filepath.Dir(targets.ProductPath), PriorPresent: !payloadParentAbsent}
	entryAsides, err := snapshotWindowsProductAsides(targets.CanonicalEntryPath, d)
	if err != nil {
		return nil, nil, err
	}
	identityAsides, err := snapshotWindowsProductAsides(targets.IdentityPath, d)
	if err != nil {
		return nil, nil, err
	}
	expected := snapshot
	expected.CanonicalEntry.Path = targets.CanonicalEntryPath
	expected.Product.Path = targets.ProductPath
	expected.ProductArtifact.Path = targets.ProductPath
	expected.Identity.Path = targets.IdentityPath
	state := &windowsCanonicalEntrySetPrepared{source: o.EntrySet, snapshot: snapshot, stages: stages, targets: targets, entryPrior: entryPrior, identityPrior: identityPrior}
	arm := &windowsCanonicalEntrySetJournalV1{Schema: windowsCanonicalEntrySetSchemaV1, TransactionID: id, Expected: expected,
		CanonicalEntry: windowsProductRecoveryArtifactV3{Target: targets.CanonicalEntryPath, PriorPresent: entryPrior.present, PriorSHA256: windowsProductPreimage(entryPrior).SHA256, NewSHA256: snapshot.CanonicalEntry.SHA256, BaselineAsides: entryAsides},
		Identity:       windowsProductRecoveryArtifactV3{Target: targets.IdentityPath, PriorPresent: identityPrior.present, PriorSHA256: windowsProductPreimage(identityPrior).SHA256, NewSHA256: snapshot.Identity.SHA256, BaselineAsides: identityAsides},
		StagedEntry:    windowsProductRecoveryStagedV3{Path: stages.CanonicalEntryPath, SHA256: snapshot.CanonicalEntry.SHA256}, StagedIdentity: windowsProductRecoveryStagedV3{Path: stages.IdentityPath, SHA256: snapshot.Identity.SHA256}, PayloadDirectory: dir}
	if o.Mode == WindowsProductPairModeUpgrade {
		view, err := o.EntrySet.prior.RuntimeProduct(o.Context)
		if err != nil {
			return nil, nil, err
		}
		runtime := &windowsCanonicalEntryRuntimePreimageV1{Path: targets.CanonicalEntryPath}
		if view != nil {
			if err := view.ValidateOriginal(o.Context); err != nil {
				return nil, nil, err
			}
			artifact := view.Artifact()
			runtime.Path, runtime.Present, runtime.SHA256 = artifact.Path, true, artifact.SHA256
		}
		arm.PriorRuntime = runtime
	}
	return state, arm, nil
}

func (t *WindowsProductTxn) receiveOriginalPayloadDirectory() error {
	p := t.prepared
	if p.entrySet == nil {
		return nil
	}
	owner, ensureErr := t.Opts.EntrySet.original.EnsureOriginalPayloadDirectory(t.Opts.Context)
	p.entrySet.directory = owner
	if owner == nil {
		return errors.Join(ensureErr, errors.New("original payload directory owner missing"))
	}
	observation, readErr := owner.Snapshot()
	if ensureErr != nil || readErr != nil {
		return errors.Join(ensureErr, readErr)
	}
	record := &p.journal.EntrySet.PayloadDirectory
	if observation.Schema != binaryadmission.WindowsCanonicalEntryPayloadDirectorySchemaV1 || !sameWindowsProductPath(observation.Root, filepath.Dir(t.Opts.CanonicalEntryPath)) || observation.RelativePath != filepath.ToSlash(filepath.Dir(binaryadmission.WindowsCanonicalProductRelativePathV1)) || observation.Created == record.PriorPresent {
		return errors.New("payload directory original creation/preimage disagrees")
	}
	record.Created = observation.Created
	record.Original = &observation
	if observation.Created {
		record.State = "created_identity_received"
	}
	return nil // Prepare publishes this evidence in its first durable checkpoint.
}

// No final file or task has changed during preparation. Clean only unpublished
// originals; a present or unverifiable journal retains every recovery input.
func (t *WindowsProductTxn) settleUnpublishedCanonicalPreparation(transferred bool) error {
	p := t.prepared
	present, inspectErr := inspectWindowsProductRecoveryLeaves(t.Opts.StateDir)
	var cleanupErr error
	if !present && inspectErr == nil && transferred {
		cleanupErr = t.cleanupCanonicalOwnedFiles(false)
	}
	// Untaken stages dispose their own files here. Taken stages only release
	// handles, preserving any bytes needed by an uncertain journal publication.
	closeErr := closeCanonicalEntryFileOwners(t.Opts, p)
	directory := p.entrySet.directory
	if directory == nil {
		return errors.Join(inspectErr, cleanupErr, closeErr)
	}
	record := p.journal.EntrySet.PayloadDirectory
	if !present && inspectErr == nil && closeErr == nil && record.Original != nil && record.Created {
		result, err := directory.DisposeOriginalIfEmpty(context.WithoutCancel(t.Opts.Context))
		p.entrySet.nativeResults = append(p.entrySet.nativeResults, result)
		p.entrySet.nativeErrors = append(p.entrySet.nativeErrors, err)
		cleanupErr = errors.Join(cleanupErr, canonicalEntryDiscardResultError(result, err, "payload-directory", record.Original.RelativePath))
	}
	return errors.Join(inspectErr, cleanupErr, closeErr, directory.Close())
}

func (t *WindowsProductTxn) ensureCanonicalPayloadDirectory() error {
	p := t.prepared
	if p.entrySet == nil {
		return nil
	}
	if p.entrySet.directory == nil || p.journal.EntrySet.PayloadDirectory.Original == nil {
		return errors.New("payload directory native custody was not durably received")
	}
	observation, err := p.entrySet.directory.Snapshot()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(observation, *p.journal.EntrySet.PayloadDirectory.Original) {
		return errors.New("payload directory native observation changed")
	}
	return nil
}

type windowsCanonicalEntryMoveRecordV1 struct {
	Member              string `json:"member"`
	Prior               bool   `json:"prior"`
	SourceRelative      string `json:"source_relative"`
	DestinationRelative string `json:"destination_relative"`
	Applied             bool   `json:"applied"`
	Readback            bool   `json:"readback"`
	NativeStatusKnown   bool   `json:"native_status_known"`
	NativeStatus        uint32 `json:"native_status"`
}

func canonicalPriorSnapshot(owner *binaryadmission.WindowsCanonicalEntryPriorSetV1, member, path string) (productPairFileSnapshot, error) {
	if owner == nil {
		return productPairFileSnapshot{}, errors.New("original canonical prior owner required")
	}
	present, body, digest, err := owner.ReadOriginal(member)
	if err != nil {
		return productPairFileSnapshot{}, err
	}
	if present && fmt.Sprintf("%x", sha256.Sum256(body)) != digest {
		return productPairFileSnapshot{}, errors.New("original canonical prior readback differs")
	}
	return productPairFileSnapshot{path: path, present: present, body: append([]byte(nil), body...)}, nil
}

func (t *WindowsProductTxn) entryRelative(path string) (string, error) {
	relative, err := filepath.Rel(filepath.Dir(t.Opts.CanonicalEntryPath), path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("canonical member target escaped original root")
	}
	return filepath.ToSlash(relative), nil
}

func (t *WindowsProductTxn) moveEntryOriginal(ctx context.Context, prior bool, member, target string) error {
	p := t.prepared
	e := p.entrySet
	relative, err := t.entryRelative(target)
	if err != nil {
		return err
	}
	p.journal.EntrySet.Moves = append(p.journal.EntrySet.Moves, windowsCanonicalEntryMoveRecordV1{Member: member, Prior: prior, DestinationRelative: relative})
	index := len(p.journal.EntrySet.Moves) - 1
	if err := t.persistProductJournal(); err != nil {
		return err
	}
	var result binaryadmission.WindowsCanonicalEntryMoveResultV1
	if prior {
		result, err = t.Opts.EntrySet.prior.MoveOriginal(ctx, member, relative)
	} else {
		result, err = t.Opts.EntrySet.original.MoveOriginal(ctx, member, relative)
	}
	e.nativeResults = append(e.nativeResults, result)
	e.nativeErrors = append(e.nativeErrors, err)
	record := &p.journal.EntrySet.Moves[index]
	record.SourceRelative = result.SourceRelative
	record.Applied = result.Applied
	record.Readback = result.Readback
	record.NativeStatusKnown = result.NativeStatusKnown
	record.NativeStatus = result.NativeStatus
	if result.Applied {
		if prior {
			if e.priorLocations == nil {
				e.priorLocations = make(map[string]string)
			}
			e.priorLocations[member] = target
		} else {
			if e.stagedLocations == nil {
				e.stagedLocations = make(map[string]string)
			}
			e.stagedLocations[member] = target
		}
	}
	received := canonicalEntryMoveResultError(result, err, member, relative)
	return errors.Join(received, t.persistProductJournal())
}

func (t *WindowsProductTxn) canonicalAside(target string) string {
	return target + ".old-" + t.prepared.deps.Now().UTC().Add(time.Duration(len(t.prepared.journal.EntrySet.Moves))*time.Second).Format("20060102T150405Z")
}

func (t *WindowsProductTxn) promoteCanonicalMember(ctx context.Context, member, target string, prior productPairFileSnapshot, newSHA string) (WindowsProductPairPromotion, error) {
	result := WindowsProductPairPromotion{Target: target, PriorPresent: prior.present, NewSHA256: newSHA}
	if prior.present {
		result.RetainedPrior = t.canonicalAside(target)
		if err := t.moveEntryOriginal(ctx, true, member, result.RetainedPrior); err != nil {
			return result, err
		}
	}
	return result, t.moveEntryOriginal(ctx, false, member, target)
}

func (t *WindowsProductTxn) promoteProductOriginal(ctx context.Context) (WindowsProductPairPromotion, error) {
	p := t.prepared
	if p.entrySet == nil {
		return p.deps.Promote(t.Opts.StagedProduct, t.Opts.ProductPath, p.staged.SHA256)
	}
	return t.promoteCanonicalMember(ctx, "product", t.Opts.ProductPath, p.productPrior, p.staged.SHA256)
}

func (t *WindowsProductTxn) promoteCanonicalEntryCompanions(ctx context.Context) error {
	p := t.prepared
	if p.entrySet == nil {
		return nil
	}
	e := p.entrySet
	j := p.journal.EntrySet
	for _, member := range []struct {
		name, target, sha string
		prior             productPairFileSnapshot
		promotion         **WindowsProductPairPromotion
		fault             WindowsProductStage
	}{
		{"identity", e.targets.IdentityPath, j.Identity.NewSHA256, e.identityPrior, &e.identityPromotion, WindowsProductStageIdentityPromoted},
		{"launcher", e.targets.CanonicalEntryPath, j.CanonicalEntry.NewSHA256, e.entryPrior, &e.entryPromotion, WindowsProductStageCanonicalEntryPromoted},
	} {
		if windowsProductTargetDisposition(member.prior, member.sha) != windowsProductTargetIdentical {
			promotion, err := t.promoteCanonicalMember(ctx, member.name, member.target, member.prior, member.sha)
			*member.promotion = rollbackProductPairPromotion(promotion, member.prior)
			if err != nil {
				return err
			}
		}
		if p.deps.Fault != nil {
			if err := p.deps.Fault(member.fault); err != nil {
				return err
			}
		}
	}
	return t.verifyLiveCanonicalEntrySet()
}

func (t *WindowsProductTxn) verifyLiveCanonicalEntrySet() error {
	p := t.prepared
	e := p.entrySet
	if err := t.Opts.EntrySet.original.VerifyUnchanged(); err != nil {
		return err
	}
	current, err := t.Opts.EntrySet.original.Targets()
	if err != nil {
		return err
	}
	for _, member := range []struct{ name, current, target, want string }{
		{"launcher", current.CanonicalEntryPath, e.targets.CanonicalEntryPath, e.snapshot.CanonicalEntry.SHA256},
		{"product", current.ProductPath, e.targets.ProductPath, e.snapshot.Product.SHA256},
		{"identity", current.IdentityPath, e.targets.IdentityPath, e.snapshot.Identity.SHA256},
	} {
		if location := e.stagedLocations[member.name]; location != "" {
			if !sameWindowsProductPath(location, member.target) || !sameWindowsProductPath(member.current, member.target) {
				return errors.New("canonical original member name differs after promotion")
			}
		} else {
			if e.priorLocations[member.name] != "" {
				return errors.New("identical canonical member prior moved unexpectedly")
			}
			present, _, digest, err := t.Opts.EntrySet.prior.ReadOriginal(member.name)
			if err != nil || !present || digest != member.want {
				return errors.Join(err, errors.New("identical canonical member no longer matches original"))
			}
		}
	}
	return nil
}

func (t *WindowsProductTxn) withdrawCanonicalSuccessorEntry() error {
	p := t.prepared
	if p.entrySet == nil || p.entrySet.stagedLocations["launcher"] == "" {
		return nil
	}
	e := p.entrySet
	target := e.targets.CanonicalEntryPath
	if !sameWindowsProductPath(e.stagedLocations["launcher"], target) {
		return nil
	}
	e.entryWithdrawn = t.canonicalAside(target)
	return t.moveEntryOriginal(context.WithoutCancel(t.Opts.Context), false, "launcher", e.entryWithdrawn)
}

func (t *WindowsProductTxn) restoreCanonicalMember(member, target string, prior productPairFileSnapshot) error {
	e := t.prepared.entrySet
	if location := e.stagedLocations[member]; location != "" && sameWindowsProductPath(location, target) {
		if err := t.moveEntryOriginal(context.WithoutCancel(t.Opts.Context), false, member, t.canonicalAside(target)); err != nil {
			return err
		}
	}
	if location := e.priorLocations[member]; location != "" {
		if !prior.present {
			return errors.New("prior-absent canonical member unexpectedly acquired an original")
		}
		return t.moveEntryOriginal(context.WithoutCancel(t.Opts.Context), true, member, target)
	}
	return nil
}

func (t *WindowsProductTxn) restoreCanonicalIdentity() error {
	if t.prepared.entrySet == nil {
		return nil
	}
	return t.restoreCanonicalMember("identity", t.Opts.IdentityPath, t.prepared.entrySet.identityPrior)
}

func (t *WindowsProductTxn) restoreCanonicalPublicEntryLast() error {
	if t.prepared.entrySet == nil {
		return nil
	}
	return t.restoreCanonicalMember("launcher", t.Opts.CanonicalEntryPath, t.prepared.entrySet.entryPrior)
}

func (t *WindowsProductTxn) discardEntryOriginal(ctx context.Context, prior bool, member, path string) error {
	relative, err := t.entryRelative(path)
	if err != nil {
		return err
	}
	var result binaryadmission.WindowsCanonicalEntryMoveResultV1
	if prior {
		result, err = t.Opts.EntrySet.prior.DiscardOriginal(ctx, member, relative)
	} else {
		result, err = t.Opts.EntrySet.original.DiscardOriginal(ctx, member, relative)
	}
	t.prepared.entrySet.nativeResults = append(t.prepared.entrySet.nativeResults, result)
	t.prepared.entrySet.nativeErrors = append(t.prepared.entrySet.nativeErrors, err)
	return canonicalEntryDiscardResultError(result, err, member, relative)
}

func (t *WindowsProductTxn) cleanupCanonicalOwnedFiles(committed bool) error {
	p := t.prepared
	if p.entrySet == nil {
		return nil
	}
	e := p.entrySet
	for _, member := range []struct{ name, stage, target string }{
		{"launcher", e.stages.CanonicalEntryPath, e.targets.CanonicalEntryPath}, {"product", e.stages.ProductPath, e.targets.ProductPath}, {"identity", e.stages.IdentityPath, e.targets.IdentityPath},
	} {
		location := e.stagedLocations[member.name]
		if location == "" {
			location = member.stage
		}
		if !sameWindowsProductPath(location, member.target) {
			var disposeErr error
			if committed {
				disposeErr = t.receiveCommittedCanonicalDiscard(context.WithoutCancel(t.Opts.Context), "unused-successor", member.name, location)
			} else {
				disposeErr = t.discardEntryOriginal(context.WithoutCancel(t.Opts.Context), false, member.name, location)
			}
			if err := disposeErr; err != nil {
				return err
			}
		}
		if committed {
			if prior := e.priorLocations[member.name]; prior != "" && !sameWindowsProductPath(prior, member.target) {
				if err := t.receiveCommittedCanonicalDiscard(context.WithoutCancel(t.Opts.Context), "prior", member.name, prior); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (t *WindowsProductTxn) removeCreatedPayloadDirectoryAfterInverse() error {
	p := t.prepared
	if p.entrySet == nil {
		return nil
	}
	record := &p.journal.EntrySet.PayloadDirectory
	if p.entrySet.directory == nil {
		return errors.New("payload directory original owner unavailable")
	}
	if record.PriorPresent {
		return p.entrySet.directory.Close()
	}
	if !record.Created || record.Original == nil {
		return errors.New("payload directory original creation identity is unconfirmed")
	}
	record.State = "dispose_intent"
	if err := t.persistProductJournal(); err != nil {
		return err
	}
	result, err := p.entrySet.directory.DisposeOriginalIfEmpty(context.WithoutCancel(t.Opts.Context))
	p.entrySet.nativeResults = append(p.entrySet.nativeResults, result)
	p.entrySet.nativeErrors = append(p.entrySet.nativeErrors, err)
	record.Disposition = &windowsCanonicalEntryMoveRecordV1{Member: result.Member, SourceRelative: result.SourceRelative, Applied: result.Applied, Readback: result.Readback, NativeStatusKnown: result.NativeStatusKnown, NativeStatus: result.NativeStatus}
	if received := canonicalEntryDiscardResultError(result, err, "payload-directory", record.Original.RelativePath); received != nil {
		return errors.Join(received, ErrWindowsProductRecoveryPending)
	}
	record.State = "absence_received"
	return errors.Join(t.persistProductJournal(), p.entrySet.directory.Close())
}
func windowsCanonicalSetDisposition(productPrior productPairFileSnapshot, e *windowsCanonicalEntrySetPrepared, productSHA string) windowsProductTargetDispositionV1 {
	values := []windowsProductTargetDispositionV1{windowsProductTargetDisposition(productPrior, productSHA), windowsProductTargetDisposition(e.entryPrior, e.snapshot.CanonicalEntry.SHA256), windowsProductTargetDisposition(e.identityPrior, e.snapshot.Identity.SHA256)}
	result := windowsProductTargetIdentical
	for _, value := range values {
		if value == windowsProductTargetReplace {
			return value
		}
		if value == windowsProductTargetCreate {
			result = value
		}
	}
	return result
}

func validateWindowsProductTxnReplacementAuthority(o WindowsProductTxnOpts, p *windowsProductPreparedTxn) error {
	if p.entryBundle != nil {
		if o.Mode != WindowsProductPairModeUpgrade {
			return validateWindowsSingleProductReplacementAuthority(o, p.disposition, p.productPrior, p.legacyPrior)
		}
		runtime := p.journal.EntryBundle.PriorRuntime
		if runtime == nil {
			return ErrWindowsProductPairQuiescenceRequired
		}
		prior, err := currentPriorSnapshot(o.Context, o.EntryBundle, runtime.Path)
		if err != nil {
			return err
		}
		if windowsProductPreimage(prior) != (windowsProductPreimageV1{Present: runtime.Present, SHA256: runtime.SHA256}) {
			return errors.New("current original prior runtime changed")
		}
		projected := o
		projected.EntryBundle = nil
		projected.ProductPath = runtime.Path
		return validateWindowsSingleProductReplacementAuthority(projected, p.disposition, prior, p.legacyPrior)
	}
	if p.entrySet == nil {
		return validateWindowsSingleProductReplacementAuthority(o, p.disposition, p.productPrior, p.legacyPrior)
	}
	if o.Mode != WindowsProductPairModeUpgrade {
		return validateWindowsSingleProductReplacementAuthority(o, p.disposition, p.entrySet.entryPrior, p.legacyPrior)
	}
	runtime := p.journal.EntrySet.PriorRuntime
	if runtime == nil {
		return ErrWindowsProductPairQuiescenceRequired
	}
	prior := p.entrySet.entryPrior
	if sameWindowsProductPath(runtime.Path, o.ProductPath) {
		prior = p.productPrior
	} else if !sameWindowsProductPath(runtime.Path, o.CanonicalEntryPath) {
		return errors.New("prior runtime is outside original complete-set targets")
	}
	if windowsProductPreimage(prior) != (windowsProductPreimageV1{Present: runtime.Present, SHA256: runtime.SHA256}) {
		return errors.New("original prior runtime preimage changed")
	}
	projected := o
	// Existing legacy authority names the stopped runtime as CLI; the new public
	// entry and GUI Product remain distinct on the actual transaction options.
	projected.EntrySet = nil
	projected.ProductPath = runtime.Path
	return validateWindowsSingleProductReplacementAuthority(projected, p.disposition, prior, p.legacyPrior)
}
func verifyLegacyProductPromotion(p *windowsProductPreparedTxn, promotion WindowsProductPairPromotion) error {
	if p.entrySet != nil {
		return nil
	}
	return verifyProductPairPromotion(promotion, p.productPrior)
}

// Select the exact decoded arm before encoding/json can case-fold a reserved
// key or let a later duplicate change its typed schema.
func canonicalEntryArmSchema(raw []byte) (string, error) {
	entry, err := uniqueCanonicalEntryRecordMember(raw, "entry_set")
	if err != nil || len(entry) == 0 {
		return "", err
	}
	if bytes.Equal(bytes.TrimSpace(entry), []byte("null")) {
		return "", errors.New("entry_set must be absent for legacy or a complete object")
	}
	schemaRaw, err := uniqueCanonicalEntryRecordMember(entry, "schema")
	if err != nil {
		return "", err
	}
	var schema string
	if len(schemaRaw) == 0 {
		return "", errors.New("nested canonical entry schema required")
	}
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		return "", err
	}
	if schema != windowsCanonicalEntrySetSchemaV1 && schema != binaryadmission.WindowsCanonicalEntrySetSchemaV2 {
		return "", errors.New("unsupported nested canonical entry schema")
	}
	return schema, nil
}

func uniqueCanonicalEntryRecordMember(raw []byte, member string) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if start != json.Delim('{') {
		return nil, errors.New("product record must be a JSON object")
	}
	var selected json.RawMessage
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("product record member is not a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if !strings.EqualFold(key, member) {
			continue
		}
		if key != member {
			return nil, fmt.Errorf("reserved %s member spelling is not exact", member)
		}
		if selected != nil {
			return nil, fmt.Errorf("duplicate decoded %s member", member)
		}
		selected = value
	}
	end, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if end != json.Delim('}') {
		return nil, errors.New("product record object is incomplete")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.Join(err, errors.New("product record has trailing JSON"))
	}
	return selected, nil
}

// OriginalOwnerPending describes retained custody, not completion of this
// particular native operation. Final owner Close remains a separate phase.
func canonicalEntryMoveResultError(result binaryadmission.WindowsCanonicalEntryMoveResultV1, callErr error, member, destination string) error {
	err := errors.Join(callErr, result.Error, result.CloseError)
	if !result.Applied || !result.Readback || !result.NativeStatusKnown || result.Member != member || result.DestinationRelative != destination {
		err = errors.Join(err, errors.New("canonical original move not fully received"))
	}
	return err
}

func canonicalEntryDiscardResultError(result binaryadmission.WindowsCanonicalEntryMoveResultV1, callErr error, member, source string) error {
	err := errors.Join(callErr, result.Error, result.CloseError)
	if !result.Applied || !result.Readback || !result.NativeStatusKnown || result.Member != member || result.SourceRelative != source {
		err = errors.Join(err, errors.New("canonical original discard or final absence unreceived"))
	}
	return err
}

func validateGUIOwnedStopPreimage(journal windowsProductRecoveryJournalV3, receipt windowsProductQuiescenceReceiptV1) error {
	if hasCanonicalEntryArm(journal) && canonicalJournalPriorRuntime(journal) == nil {
		return errors.New("complete-set prior runtime projection is missing")
	}
	runtime := canonicalRecoveryRuntimeArtifact(journal)
	if !sameWindowsProductPath(receipt.CanonicalCLIPath, runtime.Target) ||
		!sameWindowsProductPath(receipt.CanonicalWindowlessPath, journal.LegacyWindowless.Target) ||
		receipt.PriorCLI != (windowsProductPreimageV1{Present: runtime.PriorPresent, SHA256: runtime.PriorSHA256}) ||
		receipt.PriorWindowless != (windowsProductPreimageV1{Present: journal.LegacyWindowless.PriorPresent, SHA256: journal.LegacyWindowless.PriorSHA256}) {
		return errors.New("GUI-owned settlement changed original runtime preimage")
	}
	return nil
}

func cleanupCanonicalEntryLegacyFiles(o WindowsProductTxnOpts, p *windowsProductPreparedTxn, forward bool) error {
	if p.entrySet == nil {
		for _, stage := range []string{p.journal.Staged.Path, p.journal.EntrySet.StagedEntry.Path, p.journal.EntrySet.StagedIdentity.Path} {
			if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: cold original stage absence unconfirmed", ErrWindowsProductCleanupPending)
			}
		}
		for _, target := range []string{o.ProductPath, p.journal.EntrySet.CanonicalEntry.Target, p.journal.EntrySet.Identity.Target} {
			paths, err := p.deps.ListGeneratedAsides(target)
			if err != nil || len(paths) != 0 {
				return fmt.Errorf("%w: cold original asides require receiving", ErrWindowsProductCleanupPending)
			}
		}
	}
	if p.legacyRetained != "" {
		if err := removeBoundWindowsProductFileIfPresent(p.legacyRetained, p.journal.LegacyWindowless.PriorSHA256, p.deps); err != nil {
			return err
		}
	}
	if len(p.journal.LegacyWindowless.BaselineAsides) != 0 {
		return fmt.Errorf("%w: legacy original aside custody requires receiving", ErrWindowsProductCleanupPending)
	}
	if _, err := os.Lstat(o.LegacyWindowlessPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: legacy windowless remains", ErrWindowsProductCleanupPending)
	}
	helper := filepath.Join(filepath.Dir(o.ProductPath), "mcphub-pe-admit.exe")
	if _, err := os.Lstat(helper); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: helper absence unconfirmed", ErrWindowsProductCleanupPending)
	}
	return nil
}

func reconcileCanonicalEntryCommitReceipt(ctx context.Context, stateDir string, j windowsProductRecoveryJournalV3, d WindowsProductTxnDeps) (bool, error) {
	if j.EntrySet == nil {
		return false, errors.New("canonical entry-set journal arm required")
	}
	d = withWindowsProductDefaults(d)
	raw, err := d.ReadReceipt(filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil {
		return false, err
	}
	r := decoded.V3
	if r == nil || r.EntrySet == nil || r.Admission != UpgradeAdmissionCanonicalEntryProductV1 {
		return false, nil
	}
	expected := canonicalEntrySetReceipt(j)
	if r.TransactionID != j.TransactionID || r.Artifact.SHA256 != j.Product.NewSHA256 || r.TaskSetSHA256 != j.TaskSetSHA256 || !reflect.DeepEqual(r.EntrySet, expected) {
		return false, errors.New("canonical entry-set committed receipt differs from original journal")
	}
	targets, err := canonicalEntrySetJournalTargets(j)
	if err != nil {
		return false, err
	}
	if _, err := d.AdmitEntrySet(ctx, targets, canonicalEntrySnapshotFromJournal(j)); err != nil {
		return false, err
	}
	receiptSHA := fmt.Sprintf("%x", sha256.Sum256(raw))
	if d.PublishCommitted == nil {
		return false, errors.New("canonical entry-set committed publisher missing")
	}
	_, err = d.PublishCommitted(WindowsProductCommitEvent{Schema: r.Schema, TransactionID: r.TransactionID, Mode: r.Mode, Artifact: r.Artifact, EntrySet: r.EntrySet, TaskSetSHA256: r.TaskSetSHA256, Retired: r.Retired, ReceiptSHA256: receiptSHA, InstalledAt: r.InstalledAt})
	return err == nil, err
}

func canonicalRecoveryRuntimeArtifact(j windowsProductRecoveryJournalV3) windowsProductPairRecoveryArtifact {
	if j.EntryBundle != nil && j.EntryBundle.PriorRuntime != nil {
		r := j.EntryBundle.PriorRuntime
		return windowsProductPairRecoveryArtifact{Target: r.Path, PriorPresent: r.Present, PriorSHA256: r.SHA256, NewSHA256: j.Product.NewSHA256}
	}
	if j.EntrySet != nil && j.EntrySet.PriorRuntime != nil {
		r := j.EntrySet.PriorRuntime
		return windowsProductPairRecoveryArtifact{Target: r.Path, PriorPresent: r.Present, PriorSHA256: r.SHA256, NewSHA256: j.Product.NewSHA256}
	}
	return windowsProductPairRecoveryArtifact{Target: j.Product.Target, PriorPresent: j.Product.PriorPresent, PriorSHA256: j.Product.PriorSHA256, NewSHA256: j.Product.NewSHA256}
}

func hasCanonicalEntryArm(j windowsProductRecoveryJournalV3) bool {
	return j.EntrySet != nil || j.EntryBundle != nil
}

func canonicalJournalEntryPath(j windowsProductRecoveryJournalV3) string {
	if j.EntryBundle != nil {
		if j.EntrySet != nil {
			return ""
		}
		return j.EntryBundle.CanonicalEntry.Target
	}
	if j.EntrySet != nil {
		return j.EntrySet.CanonicalEntry.Target
	}
	return j.Product.Target
}

func canonicalJournalPriorRuntime(j windowsProductRecoveryJournalV3) *windowsCanonicalEntryRuntimePreimageV1 {
	if j.EntryBundle != nil {
		if j.EntrySet != nil {
			return nil
		}
		return j.EntryBundle.PriorRuntime
	}
	if j.EntrySet != nil {
		return j.EntrySet.PriorRuntime
	}
	return nil
}

func reconcileStagedCanonicalEntryNoop(ctx context.Context, stateDir string, mode WindowsProductPairMode, source *stagedWindowsCanonicalEntrySetV1, reconcile func(UpgradeReceiptV3) error) (bool, error) {
	if source == nil || source.original == nil || reconcile == nil {
		return false, errors.New("original set and committed receiving owner required")
	}
	if err := source.original.VerifyUnchanged(); err != nil {
		return false, err
	}
	snapshot, err := source.original.Snapshot()
	if err != nil {
		return false, err
	}
	targets, err := source.original.DestinationTargets()
	if err != nil {
		return false, err
	}
	prior, err := source.acquirePrior(ctx)
	if err != nil {
		return false, err
	}
	for _, member := range []struct{ name, sha string }{{"launcher", snapshot.CanonicalEntry.SHA256}, {"product", snapshot.Product.SHA256}, {"identity", snapshot.Identity.SHA256}} {
		present, _, digest, err := prior.ReadOriginal(member.name)
		if err != nil {
			return false, err
		}
		if !present || digest != member.sha {
			return false, nil
		}
	}
	d := withWindowsProductDefaults(WindowsProductTxnDeps{})
	raw, err := d.ReadReceipt(filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil {
		return false, err
	}
	r := decoded.V3
	if r == nil || r.EntrySet == nil || r.Admission != UpgradeAdmissionCanonicalEntryProductV1 {
		return false, nil
	}
	e := r.EntrySet
	if !windowsProductPairReceiptModeMatches(r.Mode, mode) || r.Artifact.SHA256 != snapshot.Product.SHA256 || e.BuildID != snapshot.BuildID || e.RegistryProjectionSHA256 != snapshot.RegistryProjectionSHA256 || e.CanonicalEntrySHA256 != snapshot.CanonicalEntry.SHA256 || e.IdentitySHA256 != snapshot.Identity.SHA256 || !sameWindowsProductPath(e.CanonicalEntryPath, targets.CanonicalEntryPath) || !sameWindowsProductPath(e.ProductPath, targets.ProductPath) || !sameWindowsProductPath(e.IdentityPath, targets.IdentityPath) {
		return false, nil
	}
	if err := reconcile(*r); err != nil {
		return false, err
	}
	return true, nil
}

func canonicalRollbackRuntime(o WindowsProductTxnOpts, p *windowsProductPreparedTxn) (string, productPairFileSnapshot) {
	if p.journal.EntrySet != nil && p.journal.EntrySet.PriorRuntime != nil {
		path := p.journal.EntrySet.PriorRuntime.Path
		if sameWindowsProductPath(path, o.CanonicalEntryPath) {
			return path, p.entrySet.entryPrior
		}
	}
	return o.ProductPath, p.productPrior
}

func canonicalCleanupMember(j windowsProductRecoveryJournalV3, side, member string) (target, stage, expected string, err error) {
	if j.EntrySet == nil {
		return "", "", "", errors.New("cleanup entry-set arm missing")
	}
	var artifact windowsProductRecoveryArtifactV3
	switch member {
	case "launcher":
		artifact = j.EntrySet.CanonicalEntry
		stage = j.EntrySet.StagedEntry.Path
	case "product":
		artifact = j.Product
		stage = j.Staged.Path
	case "identity":
		artifact = j.EntrySet.Identity
		stage = j.EntrySet.StagedIdentity.Path
	default:
		return "", "", "", errors.New("unknown cleanup member")
	}
	switch side {
	case "prior":
		if !artifact.PriorPresent {
			return "", "", "", errors.New("absent prior cannot be cleanup original")
		}
		expected = artifact.PriorSHA256
	case "unused-successor":
		expected = artifact.NewSHA256
	default:
		return "", "", "", errors.New("unknown cleanup side")
	}
	return artifact.Target, stage, expected, nil
}

func validateCanonicalCleanup(j windowsProductRecoveryJournalV3) error {
	if j.EntrySet == nil || len(j.EntrySet.Cleanup) == 0 {
		return nil
	}
	if j.Phase != WindowsProductRecoveryCommitDecided && j.Phase != WindowsProductRecoverySettledCommit {
		return errors.New("irreversible cleanup records require committed-forward decision")
	}
	seen := make(map[string]bool)
	root := filepath.Dir(j.EntrySet.CanonicalEntry.Target)
	for _, row := range j.EntrySet.Cleanup {
		target, stage, expected, err := canonicalCleanupMember(j, row.Side, row.Member)
		if err != nil {
			return err
		}
		key := row.Side + "\x00" + row.Member
		if seen[key] || row.TransactionID != j.TransactionID || row.ExpectedSHA256 != expected || !validLowerHexSHA256(expected) {
			return errors.New("cleanup record disagrees with original transaction/member")
		}
		seen[key] = true
		if row.State != "retained" && row.State != "discard_intent" && row.State != "absence_received" {
			return errors.New("unknown cleanup receiving state")
		}
		source := filepath.Join(root, filepath.FromSlash(row.SourceRelative))
		relative, err := filepath.Rel(root, source)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(row.SourceRelative) || filepath.ToSlash(relative) != row.SourceRelative {
			return errors.New("cleanup source escaped original root")
		}
		for _, final := range []string{j.EntrySet.CanonicalEntry.Target, j.Product.Target, j.EntrySet.Identity.Target} {
			if sameWindowsProductPath(source, final) {
				return errors.New("canonical successor is never disposable cleanup")
			}
		}
		if !api.IsGeneratedRenameAsideForTarget(target, source) && (row.Side != "unused-successor" || !sameWindowsProductPath(source, stage)) {
			return errors.New("cleanup location is not exact original stage/aside")
		}
	}
	return nil
}

func canonicalCleanupRow(j windowsProductRecoveryJournalV3, side, member string) (int, *windowsCanonicalEntryCleanupV1) {
	if j.EntrySet == nil {
		return -1, nil
	}
	for i := range j.EntrySet.Cleanup {
		row := &j.EntrySet.Cleanup[i]
		if row.Side == side && row.Member == member {
			return i, row
		}
	}
	return -1, nil
}

func cleanupAbsenceAuthorized(row *windowsCanonicalEntryCleanupV1) bool {
	return row != nil && (row.State == "discard_intent" || row.State == "absence_received")
}

func (t *WindowsProductTxn) receiveCommittedCanonicalDiscard(ctx context.Context, side, member, path string) error {
	j := &t.prepared.journal
	if j.Phase != WindowsProductRecoveryCommitDecided && j.Phase != WindowsProductRecoverySettledCommit {
		return errors.New("original committed cleanup decision missing")
	}
	relative, err := t.entryRelative(path)
	if err != nil {
		return err
	}
	_, _, expected, err := canonicalCleanupMember(*j, side, member)
	if err != nil {
		return err
	}
	index, row := canonicalCleanupRow(*j, side, member)
	if row == nil {
		j.EntrySet.Cleanup = append(j.EntrySet.Cleanup, windowsCanonicalEntryCleanupV1{TransactionID: j.TransactionID, Side: side, Member: member, SourceRelative: relative, ExpectedSHA256: expected, State: "retained"})
		index = len(j.EntrySet.Cleanup) - 1
		row = &j.EntrySet.Cleanup[index]
	} else if row.SourceRelative != relative || row.ExpectedSHA256 != expected {
		return errors.New("cleanup original location changed")
	}
	if row.State == "absence_received" {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, ErrWindowsProductCleanupPending)
		}
		return nil
	}
	if row.State == "discard_intent" {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			j.EntrySet.Cleanup[index].State = "absence_received"
			return t.persistProductJournal()
		} else if err != nil {
			return err
		}
	}
	row.State = "discard_intent"
	if err := t.persistProductJournal(); err != nil {
		return err
	}
	if err := t.discardEntryOriginal(ctx, side == "prior", member, path); err != nil {
		return err
	}
	j.EntrySet.Cleanup[index].State = "absence_received"
	return t.persistProductJournal()
}

func closeCanonicalEntryFileOwners(o WindowsProductTxnOpts, p *windowsProductPreparedTxn) error {
	var causes []error
	if o.EntrySet != nil {
		if o.EntrySet.original != nil {
			causes = append(causes, o.EntrySet.original.Close())
		}
		if o.EntrySet.prior != nil {
			causes = append(causes, o.EntrySet.prior.Close())
		}
	}
	return errors.Join(causes...)
}

func validateOriginalPayloadDirectory(j windowsProductRecoveryJournalV3) error {
	if j.EntrySet == nil {
		return nil
	}
	d := j.EntrySet.PayloadDirectory
	if d.Created && d.Original == nil {
		return errors.New("payload directory Created flag has no native creation identity")
	}
	if d.State != "" && d.State != "created_identity_received" && d.State != "dispose_intent" && d.State != "absence_received" {
		return errors.New("unknown payload directory disposition state")
	}
	if (d.State == "dispose_intent" || d.State == "absence_received") && (d.PriorPresent || !d.Created || d.Original == nil || j.Phase == WindowsProductRecoveryCommitDecided || j.Phase == WindowsProductRecoverySettledCommit) {
		return errors.New("payload directory disposal is not original-created rollback")
	}
	if d.Absence != nil && (d.State != "absence_received" || !d.Absence.Absent || d.Original == nil || d.Absence.ParentIdentity != d.Original.ParentIdentity || !sameWindowsProductPath(d.Absence.Root, d.Original.Root) || d.Absence.RelativePath != d.Original.RelativePath) {
		return errors.New("payload directory absence observation disagrees")
	}
	if d.Original == nil {
		if j.Phase != WindowsProductRecoveryPrepared && j.Phase != WindowsProductRecoveryGUIOwnedStopPending {
			return errors.New("payload directory original receiving is incomplete")
		}
		return nil
	}
	expected := d.Original
	if !d.CreationEntered || expected.Schema != binaryadmission.WindowsCanonicalEntryPayloadDirectorySchemaV1 || expected.Created != d.Created || expected.Created == d.PriorPresent || !sameWindowsProductPath(expected.Root, filepath.Dir(j.EntrySet.CanonicalEntry.Target)) || expected.RelativePath != filepath.ToSlash(filepath.Dir(binaryadmission.WindowsCanonicalProductRelativePathV1)) {
		return errors.New("payload directory native snapshot disagrees with original transaction")
	}
	return nil
}

func currentEntryRelative(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "\\:\x00") && !strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "../") && path.Clean(value) == value
}

func currentEntryMember(e *windowsCanonicalEntrySetJournalV2, member string) *windowsCanonicalEntryMemberJournalV2 {
	i := sort.Search(len(e.Bundle.Members), func(i int) bool { return e.Bundle.Members[i].Member >= member })
	if i < len(e.Bundle.Members) && e.Bundle.Members[i].Member == member {
		return &e.Bundle.Members[i]
	}
	return nil
}

func validateCanonicalEntryBundleJournal(j windowsProductRecoveryJournalV3) error {
	e := j.EntryBundle
	if e == nil || e.Schema != binaryadmission.WindowsCanonicalEntrySetSchemaV2 || e.TransactionID != j.TransactionID || e.Expected.Schema != e.Schema || e.Bundle.Effect != "exact-members" || e.Bundle.StagedDirectory != nil || e.Bundle.RelativePath != e.Expected.BundleRelativePath {
		return errors.New("invalid current exact-member journal arm")
	}
	if err := binaryadmission.ValidateWindowsCanonicalEntrySetSnapshotV2(e.Expected); err != nil {
		return err
	}
	if d := e.Bundle.Disposition; d.CloseOwner != nil {
		if !d.OriginalOwnerPending || d.CloseError == nil || *d.CloseError == "" || !servingAggregateRollbackJournal(j) ||
			d.CloseOwner.PID == 0 || d.CloseOwner.StartedAt.IsZero() || d.CloseOwner.StartedAt.Nanosecond()%100 != 0 {
			return errors.New("invalid serving rollback close-owner diagnostic")
		}
	}
	if !e.Bundle.Disposition.Transferred || e.Bundle.Disposition.State == "staged" {
		return errors.New("current published journal has no transferred disposition")
	}
	if e.CleanArchiveSHA256 != "" {
		if !validLowerHexSHA256(e.CleanArchiveSHA256) || j.Mode != WindowsProductPairModeUpgrade ||
			j.PreSettlement == nil || e.PriorRuntime == nil || !e.PriorRuntime.Present {
			return errors.New("clean archive binding lacks exact restored Product evidence")
		}
		switch j.Phase {
		case WindowsProductRecoveryPrepared, WindowsProductRecoveryGUIOwnedStopPending, WindowsProductRecoverySuccessorStarting:
			if e.Bundle.Disposition.State != "rolled-back" {
				return errors.New("clean archive binding requires the rolled-back disposition")
			}
		case WindowsProductRecoverySettledRollback:
			if e.Bundle.Disposition.State != "settled" {
				return errors.New("clean archive binding requires the settled rollback disposition")
			}
		default:
			return errors.New("clean archive binding is outside rollback cleanup")
		}
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV2(e.CanonicalEntry.Target)
	if err != nil {
		return err
	}
	root := filepath.Dir(targets.CanonicalEntryPath)
	if !sameWindowsProductPath(targets.ProductPath, j.Product.Target) || e.Expected.Product.SHA256 != j.Product.NewSHA256 || e.Expected.CanonicalEntry.SHA256 != e.CanonicalEntry.NewSHA256 || !sameWindowsProductPath(e.Expected.ProductArtifact.Path, targets.ProductPath) || !sameWindowsProductPath(e.Expected.CanonicalEntry.Path, targets.CanonicalEntryPath) || !sameWindowsProductPath(e.Expected.Identity.Path, targets.IdentityPath) || !validRecoveryPreimage(e.CanonicalEntry.PriorPresent, e.CanonicalEntry.PriorSHA256) {
		return errors.New("current journal file projections disagree")
	}
	if e.retainsSingleton(e.Expected.CanonicalEntryRelativePath) {
		if e.StagedEntry != (windowsProductRecoveryStagedV3{}) {
			return errors.New("retained Launcher cannot carry a stage")
		}
	} else if e.StagedEntry.SHA256 != e.CanonicalEntry.NewSHA256 {
		return errors.New("current journal file projections disagree")
	}
	newFiles := map[string]binaryadmission.RuntimeObjectV1{e.Expected.CanonicalEntryRelativePath: {Path: e.Expected.CanonicalEntryRelativePath, Kind: "file", SHA256: e.Expected.CanonicalEntry.SHA256}}
	for _, object := range e.Expected.BundleInventory {
		if object.Kind == "file" {
			newFiles[path.Join(e.Bundle.RelativePath, object.Path)] = object
		}
	}
	priorFiles := map[string]string{}
	switch e.Bundle.BeforeKind {
	case binaryadmission.WindowsCanonicalEntryBeforeAbsentV2:
	case binaryadmission.WindowsCanonicalEntryBeforeRootProductV2:
		priorFiles[e.Expected.CanonicalEntryRelativePath] = e.CanonicalEntry.PriorSHA256
	case binaryadmission.WindowsCanonicalEntryBeforeLegacyV2:
		legacy, err := binaryadmission.CanonicalEntryTargetsV1(targets.CanonicalEntryPath)
		if err != nil {
			return err
		}
		identity, err := filepath.Rel(root, legacy.IdentityPath)
		if err != nil {
			return err
		}
		priorFiles[e.Expected.CanonicalEntryRelativePath], priorFiles[e.Expected.ProductRelativePath], priorFiles[filepath.ToSlash(identity)] = e.CanonicalEntry.PriorSHA256, j.Product.PriorSHA256, ""
	case binaryadmission.WindowsCanonicalEntryBeforeBundleV2:
		if e.Bundle.BeforeInventory == nil {
			return errors.New("prior current membership inventory missing")
		}
		raw, err := binaryadmission.CanonicalRuntimeJSON(*e.Bundle.BeforeInventory)
		if err != nil {
			return err
		}
		if _, err := binaryadmission.ParseRuntimeInventory(raw); err != nil {
			return err
		}
		priorFiles[e.Expected.CanonicalEntryRelativePath] = e.CanonicalEntry.PriorSHA256
		for _, object := range e.Bundle.BeforeInventory.Objects {
			if object.Kind == "file" {
				priorFiles[path.Join(e.Bundle.RelativePath, object.Path)] = object.SHA256
			}
		}
	default:
		return errors.New("unknown selected prior kind")
	}
	if e.Bundle.BeforeKind != binaryadmission.WindowsCanonicalEntryBeforeBundleV2 && e.Bundle.BeforeInventory != nil {
		return errors.New("noncurrent prior cannot carry bundle inventory")
	}
	if e.Bundle.Members == nil || len(e.Bundle.Members) == 0 && e.Bundle.Retained == nil ||
		e.Moves == nil || e.Cleanup == nil || e.Bundle.Directories == nil {
		return errors.New("current member evidence arrays missing")
	}
	var retainedFiles map[string]bool
	var retainedDirectories map[string]string
	if e.Bundle.Retained != nil {
		retainedFiles, retainedDirectories, err = currentEntryRetainedRuntime(e)
		if err != nil {
			return err
		}
		if retainedFiles[e.Expected.ProductRelativePath] &&
			(!j.Product.PriorPresent || j.Product.PriorSHA256 != j.Product.NewSHA256 || j.Staged != (windowsProductRecoveryStagedV3{})) {
			return errors.New("retained Product preimage or absent stage disagrees")
		}
		for key := range retainedFiles {
			delete(newFiles, key)
			delete(priorFiles, key)
		}
	}
	seenPaths := map[string]string{}
	memberKeys := map[string]bool{}
	for key := range retainedFiles {
		memberKeys[strings.ToLower(key)] = true
	}
	identities := map[binaryadmission.WindowsCanonicalEntryFileIdentityV2]bool{}
	for _, row := range e.Bundle.Members {
		key := strings.ToLower(row.Member)
		if memberKeys[key] {
			return errors.New("case-aliased current member keys")
		}
		memberKeys[key] = true
	}
	last := ""
	for i := range e.Bundle.Members {
		row := &e.Bundle.Members[i]
		if !currentEntryRelative(row.Member) || row.Member <= last {
			return errors.New("current member keys are not canonical and unique")
		}
		last = row.Member
		newObject, isNew := newFiles[row.Member]
		oldDigest, isPrior := priorFiles[row.Member]
		if isNew != (row.Staged != nil) || isPrior != (row.Prior != nil) || (!isNew && !isPrior) {
			return errors.New("current member set differs from selected new/prior membership")
		}
		for _, side := range []struct {
			value *binaryadmission.WindowsCanonicalEntryMemberEvidenceV2
			prior bool
		}{{row.Prior, true}, {row.Staged, false}} {
			v := side.value
			if v == nil {
				continue
			}
			if v.Member != row.Member || !currentEntryRelative(v.File.Path) || !validLowerHexSHA256(v.File.SHA256) || v.Size < 0 || v.NativeIdentity.CreationTime == 0 || v.ParentIdentity.CreationTime == 0 {
				return errors.New("invalid original current member evidence")
			}
			key := strings.ToLower(v.File.Path)
			if _, ok := seenPaths[key]; ok {
				return errors.New("original current member locations alias")
			}
			if identities[v.NativeIdentity] {
				return errors.New("original current file identities alias")
			}
			identities[v.NativeIdentity] = true
			if !side.prior && memberKeys[key] {
				return errors.New("current stage aliases a selected final name")
			}
			seenPaths[key] = row.Member
			if side.prior {
				if v.File.Path != row.Member || (oldDigest != "" && oldDigest != v.File.SHA256) {
					return errors.New("prior current file preimage disagrees")
				}
			} else if v.File.SHA256 != newObject.SHA256 || (newObject.Size != nil && v.Size != *newObject.Size) || path.Dir(v.File.Path) != "." || v.File.Path == row.Member {
				return errors.New("issued current stage disagrees with authenticated file plan")
			}
		}
		if row.Prior != nil {
			if row.PriorAbsence != nil || row.PriorAsideRelative == nil {
				return errors.New("prior member has invalid absence/aside evidence")
			}
		} else {
			if row.PriorAbsence == nil || row.PriorAsideRelative != nil || row.PriorAbsence.ParentIdentity.CreationTime == 0 || !currentEntryRelative(row.PriorAbsence.MissingRelative) || (row.PriorAbsence.ParentRelative != "" && !currentEntryRelative(row.PriorAbsence.ParentRelative)) || (row.Member != row.PriorAbsence.MissingRelative && !strings.HasPrefix(row.Member, row.PriorAbsence.MissingRelative+"/")) {
				return errors.New("new-only member lacks original absence observation")
			}
		}
		if (row.Staged != nil) != (row.SuccessorAsideRelative != nil) {
			return errors.New("successor aside selection disagrees")
		}
		for _, aside := range []*string{row.PriorAsideRelative, row.SuccessorAsideRelative} {
			if aside == nil {
				continue
			}
			if !currentEntryRelative(*aside) || path.Dir(*aside) != "." || memberKeys[strings.ToLower(*aside)] {
				return errors.New("issued current aside escaped flat original root")
			}
			key := strings.ToLower(*aside)
			if _, ok := seenPaths[key]; ok {
				return errors.New("current aside aliases original path")
			}
			seenPaths[key] = row.Member
		}
		delete(newFiles, row.Member)
		delete(priorFiles, row.Member)
	}
	if len(newFiles) != 0 || len(priorFiles) != 0 {
		return errors.New("selected current membership incomplete")
	}
	locations, err := currentEntryMoveLocations(e)
	if err != nil {
		return err
	}
	directories := map[string]binaryadmission.WindowsCanonicalEntryDirectoryIdentityV1{}
	for _, row := range e.Bundle.Directories {
		d := row.Original
		if d.Schema != binaryadmission.WindowsCanonicalEntryMemberDirectorySchemaV2 || !sameWindowsProductPath(d.Root, root) || !currentEntryRelative(d.RelativePath) || d.DirectoryIdentity.CreationTime == 0 || d.ParentIdentity.CreationTime == 0 {
			return errors.New("invalid current original directory evidence")
		}
		if _, duplicate := directories[strings.ToLower(d.RelativePath)]; duplicate {
			return errors.New("duplicate current original directory")
		}
		if parent := path.Dir(d.RelativePath); parent != "." {
			if id, ok := directories[strings.ToLower(parent)]; !ok || id != d.ParentIdentity {
				return errors.New("current directory evidence is not an original parent chain")
			}
		}
		key := strings.ToLower(d.RelativePath)
		if retained, ok := retainedDirectories[key]; ok {
			if d.RelativePath != retained || d.Created || row.State != "retained" || row.Disposition != nil {
				return errors.New("current retained runtime directory has changed or disposal evidence")
			}
			if d.RelativePath == e.Bundle.RelativePath &&
				(d.DirectoryIdentity != e.Bundle.BeforeDirectory.DirectoryIdentity || d.ParentIdentity != e.Bundle.BeforeDirectory.ParentIdentity) {
				return errors.New("retained runtime is not bound to the original bundle directory")
			}
		}
		directories[key] = d.DirectoryIdentity
		switch row.State {
		case "retained":
			if d.Created {
				return errors.New("created directory has no creation receiving")
			}
		case "created_identity_received":
			if !d.Created {
				return errors.New("prior directory is marked created")
			}
		case "dispose_intent", "absence_received":
			if !d.Created || j.Phase == WindowsProductRecoveryCommitDecided || j.Phase == WindowsProductRecoverySettledCommit {
				return errors.New("required/prior current directory cannot be disposed")
			}
		default:
			return errors.New("unknown current directory state")
		}
	}
	for key := range retainedDirectories {
		if _, ok := directories[key]; !ok {
			return errors.New("current retained runtime directory or parent evidence missing")
		}
	}
	if e.Bundle.BeforeDirectory != nil {
		d := e.Bundle.BeforeDirectory
		if d.Created || !sameWindowsProductPath(d.Root, root) || d.RelativePath != e.Bundle.RelativePath {
			return errors.New("prior bundle container context disagrees")
		}

	}
	if e.PriorRuntime != nil {
		r := e.PriorRuntime
		member, err := filepath.Rel(root, r.Path)
		if err != nil {
			return err
		}
		member = filepath.ToSlash(member)
		row := currentEntryMember(e, member)
		if r.Present {
			if member == e.Expected.ProductRelativePath && retainedFiles[member] {
				if r.SHA256 != j.Product.PriorSHA256 {
					return errors.New("prior runtime differs from the retained original Product")
				}
			} else if row == nil || row.Prior == nil || row.Prior.File.SHA256 != r.SHA256 {
				return errors.New("prior runtime is not an actual original member")
			}
		} else if r.SHA256 != "" || e.Bundle.BeforeKind != binaryadmission.WindowsCanonicalEntryBeforeAbsentV2 {
			return errors.New("prior runtime absence disagrees")
		}
	} else if j.Mode == WindowsProductPairModeUpgrade {
		return errors.New("current upgrade prior runtime projection missing")
	}
	cleanupKeys := map[string]bool{}
	for _, row := range e.Cleanup {
		key := row.Side + "\x00" + row.Member
		if cleanupKeys[key] {
			return errors.New("duplicate current cleanup original")
		}
		cleanupKeys[key] = true
		member := currentEntryMember(e, row.Member)
		if member == nil || row.TransactionID != j.TransactionID || !validLowerHexSHA256(row.ExpectedSHA256) {
			return errors.New("invalid current cleanup member")
		}
		prior := row.Side == "prior"
		if row.Side != "prior" && row.Side != "unused-successor" {
			return errors.New("unknown current cleanup side")
		}
		original := member.Staged
		key = "successor\x00" + row.Member
		if prior {
			original = member.Prior
			key = "prior\x00" + row.Member
		}
		if original == nil || original.File.SHA256 != row.ExpectedSHA256 || !locations[key][row.SourceRelative] || row.SourceRelative == row.Member {
			return errors.New("current cleanup escaped nonfinal original lineage")
		}
		switch row.Purpose {
		case "committed-forward":
			if j.Phase != WindowsProductRecoveryCommitDecided && j.Phase != WindowsProductRecoverySettledCommit {
				return errors.New("current committed cleanup decision missing")
			}
		case "rollback-cleanup":
			if prior || e.Bundle.Disposition.State != "rolled-back" && e.Bundle.Disposition.State != "cleanup-pending" && e.Bundle.Disposition.State != "settled" || j.Phase == WindowsProductRecoveryCommitDecided || j.Phase == WindowsProductRecoverySettledCommit {
				return errors.New("current rollback cleanup decision missing")
			}
		default:
			return errors.New("unknown current cleanup purpose")
		}
		switch row.State {
		case "retained", "discard_intent", "absence_received":
		default:
			return errors.New("unknown current cleanup state")
		}
	}
	switch e.Bundle.Disposition.State {
	case "staged", "transferred", "apply-pending", "committed", "rollback-pending", "rolled-back", "cleanup-pending", "retained", "settled":
	default:
		return errors.New("unknown current disposition state")
	}
	return nil
}

var errCurrentEntryRuntimeRetentionUnavailable = errors.New("native current runtime retention witness unavailable")

func requireCurrentEntryNativeRetention(e *windowsCanonicalEntrySetJournalV2) error {
	if e != nil && e.Bundle.Retained != nil {
		// This generic writer has neither the original selective stage nor a
		// sealed current recovery source.
		return errors.Join(ErrWindowsProductRecoveryPending, errCurrentEntryRuntimeRetentionUnavailable)
	}
	return nil
}

func currentEntryRetainedRuntime(e *windowsCanonicalEntrySetJournalV2) (files map[string]bool, directories map[string]string, err error) {
	witness := e.Bundle.Retained
	if e.Bundle.BeforeKind != binaryadmission.WindowsCanonicalEntryBeforeBundleV2 ||
		e.Bundle.BeforeInventory == nil || e.Bundle.BeforeDirectory == nil ||
		witness == nil || !validLowerHexSHA256(witness.MembersSHA256) || witness.Singletons == nil {
		return nil, nil, errors.New("retained runtime requires original bundle and native witness")
	}
	index, err := binaryadmission.ParseRuntimePayload(e.Expected.RuntimePayloadBytes)
	if err != nil {
		return nil, nil, err
	}
	relativeIndex, ok := strings.CutPrefix(e.Expected.RuntimePayload.Path, e.Bundle.RelativePath+"/")
	if !ok || !currentEntryRelative(relativeIndex) {
		return nil, nil, errors.New("retained runtime index is outside its original bundle")
	}
	prior := make(map[string]binaryadmission.RuntimeObjectV1, len(e.Bundle.BeforeInventory.Objects))
	for _, object := range e.Bundle.BeforeInventory.Objects {
		prior[object.Path] = object
	}
	oldIndex := prior[relativeIndex]
	if oldIndex.Kind != "file" || oldIndex.Size == nil || *oldIndex.Size != int64(len(e.Expected.RuntimePayloadBytes)) ||
		oldIndex.SHA256 != binaryadmission.RuntimeDigest(e.Expected.RuntimePayloadBytes) {
		return nil, nil, errors.New("retained runtime index differs from original bundle inventory")
	}
	next := make(map[string]binaryadmission.RuntimeObjectV1, len(e.Expected.BundleInventory))
	for _, object := range e.Expected.BundleInventory {
		next[object.Path] = object
	}

	files, directories = make(map[string]bool), make(map[string]string)
	retain := func(object binaryadmission.RuntimeObjectV1) {
		relative := path.Join(e.Bundle.RelativePath, object.Path)
		if object.Kind == "file" {
			files[relative] = true
			relative = path.Dir(relative)
		}
		for ; relative != "."; relative = path.Dir(relative) {
			directories[strings.ToLower(relative)] = relative
		}
	}
	for _, component := range index.Components {
		// The generation parent includes the descriptor and inventory that
		// are siblings of the component's declared runtime root.
		root := path.Dir(component.Root)
		prefix := root + "/"
		if next[root].Kind != "directory" {
			return nil, nil, errors.New("retained runtime generation directory missing")
		}
		for key, object := range next {
			if key == root || strings.HasPrefix(key, prefix) {
				if !reflect.DeepEqual(prior[key], object) {
					return nil, nil, errors.New("retained runtime generation inventory changed")
				}
				retain(object)
			}
		}
		for key := range prior {
			if (key == root || strings.HasPrefix(key, prefix)) && next[key].Kind == "" {
				return nil, nil, errors.New("foreign object in original retained runtime generation")
			}
		}
	}
	last := ""
	for _, key := range witness.Singletons {
		if key <= last {
			return nil, nil, errors.New("invalid retained runtime singleton selection")
		}
		last = key
		switch key {
		case e.Expected.CanonicalEntryRelativePath:
			// Launcher is outside the bundle inventories. Its original
			// digest is recorded separately; native attributes and extent
			// remain part of the required held witness.
			if !e.CanonicalEntry.PriorPresent || e.CanonicalEntry.PriorSHA256 != e.Expected.CanonicalEntry.SHA256 {
				return nil, nil, errors.New("retained Launcher differs from its original preimage")
			}
			files[key] = true
			continue
		case e.Expected.ProductRelativePath, e.Expected.RuntimePayload.Path, e.Expected.IdentityRelativePath:
		default:
			return nil, nil, errors.New("invalid retained runtime singleton selection")
		}
		relative, _ := strings.CutPrefix(key, e.Bundle.RelativePath+"/")
		object := next[relative]
		if object.Kind != "file" || !reflect.DeepEqual(prior[relative], object) {
			return nil, nil, errors.New("retained runtime singleton differs from original bundle inventory")
		}
		retain(object)
	}
	return files, directories, nil
}

func currentEntryMoveLocations(e *windowsCanonicalEntrySetJournalV2) (map[string]map[string]bool, error) {
	locations := map[string]map[string]bool{}
	for _, row := range e.Bundle.Members {
		if row.Prior != nil {
			locations["prior\x00"+row.Member] = map[string]bool{row.Prior.File.Path: true}
		}
		if row.Staged != nil {
			locations["successor\x00"+row.Member] = map[string]bool{row.Staged.File.Path: true}
		}
	}
	for _, move := range e.Moves {
		row := currentEntryMember(e, move.Member)
		if row == nil {
			return nil, errors.New("move names an unissued current member")
		}
		side := "successor"
		if move.Prior {
			side = "prior"
		}
		key := side + "\x00" + move.Member
		if !currentEntryRelative(move.SourceRelative) || !locations[key][move.SourceRelative] || !currentEntryRelative(move.DestinationRelative) {
			return nil, errors.New("move lineage disagrees with legal original locations")
		}
		aside := row.SuccessorAsideRelative
		if move.Prior {
			aside = row.PriorAsideRelative
		}
		if move.DestinationRelative != row.Member && (aside == nil || move.DestinationRelative != *aside) {
			return nil, errors.New("move destination is not issued final/aside")
		}
		if move.Readback && !move.Applied || move.Applied && (!move.Attempted || !move.NativeStatusKnown) || move.NativeStatusKnown && !move.Attempted {
			return nil, errors.New("impossible current native move outcome")
		}
		switch {
		case move.Applied:
			locations[key] = map[string]bool{move.DestinationRelative: true}
		case move.NativeStatusKnown:
			locations[key] = map[string]bool{move.SourceRelative: true}
		default:
			locations[key] = map[string]bool{move.SourceRelative: true, move.DestinationRelative: true}
		}
	}
	return locations, nil
}

package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gofrs/flock"
)

type recoveryTarget struct {
	SelectedPath     string `json:"selected_path"`
	AbsolutePath     string `json:"absolute_path"`
	Outcome          string `json:"outcome"`
	RemoveErrorClass string `json:"remove_error_class"`
}
type recoveryRecord struct {
	AttemptID     string         `json:"attempt_id"`
	State         string         `json:"state"`
	ResolutionCWD string         `json:"resolution_cwd"`
	Key           recoveryTarget `json:"key"`
	Vault         recoveryTarget `json:"vault"`
}
type recoveryCapture struct {
	cwd        string
	key, vault recoveryTarget
}
type recoveryStage struct {
	phase              string
	record             recoveryRecord
	id, digest, reason string
}
type recoveryReservation struct {
	owner    *secretsInitRecoveryOwner
	stage    recoveryStage
	prepared []byte
	settled  bool
}
type secretsInitRecoveryOwner struct {
	root  string
	write func(string, []byte) error
}

// PublicRecoveryReference conveys evidence availability only, never an operation capability.
type PublicRecoveryReference struct {
	ID     string `json:"recovery_id,omitempty"`
	Status string `json:"recovery_status"`
}
type SecretsInitRecoverySummary struct {
	AttemptID string `json:"attempt_id"`
	State     string `json:"state"`
}
type SecretsInitRecoveryReport struct {
	AttemptID    string         `json:"attempt_id"`
	State        string         `json:"state"`
	Key          recoveryTarget `json:"key"`
	Vault        recoveryTarget `json:"vault"`
	Authority    string         `json:"authority"`
	CurrentState string         `json:"current_state"`
}
type SecretsRecoveryPersistenceError struct {
	PersistenceCause error
	InitError        error
	reference        PublicRecoveryReference
}

func (e *SecretsRecoveryPersistenceError) Error() string {
	return "secrets initialization recovery evidence could not be verified"
}
func (e *SecretsRecoveryPersistenceError) Unwrap() error {
	if e.InitError != nil {
		return e.InitError
	}
	return e.PersistenceCause
}
func (e *SecretsRecoveryPersistenceError) RecoveryReference() PublicRecoveryReference {
	return e.reference
}

func captureSecretsInitRecoveryTargets(key, vault, cwd string) (recoveryCapture, error) {
	k, v := key, vault
	if !filepath.IsAbs(k) {
		k = filepath.Join(cwd, k)
	}
	if !filepath.IsAbs(v) {
		v = filepath.Join(cwd, v)
	}
	k = filepath.Clean(k)
	v = filepath.Clean(v)
	c := recoveryCapture{cwd: cwd, key: recoveryTarget{key, k, "not_attempted", "none"}, vault: recoveryTarget{vault, v, "not_attempted", "none"}}
	return c, validateRecoveryCapture(c)
}
func validateRecoveryCapture(c recoveryCapture) error {
	for _, s := range []string{c.cwd, c.key.SelectedPath, c.key.AbsolutePath, c.vault.SelectedPath, c.vault.AbsolutePath} {
		if s == "" || !utf8.ValidString(s) {
			return errors.New("invalid recovery capture")
		}
		raw, err := json.Marshal(s)
		if err != nil {
			return err
		}
		var restored string
		if err = json.Unmarshal(raw, &restored); err != nil || restored != s {
			return errors.New("lossy recovery capture")
		}
	}
	if !filepath.IsAbs(c.cwd) || !filepath.IsAbs(c.key.AbsolutePath) || !filepath.IsAbs(c.vault.AbsolutePath) || c.key.AbsolutePath == c.vault.AbsolutePath {
		return errors.New("invalid recovery operation paths")
	}
	return nil
}
func newSecretsInitRecoveryOwner() (*secretsInitRecoveryOwner, error) {
	root, err := DaemonStateDir()
	if err != nil {
		return nil, err
	}
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("invalid recovery state root")
	}
	return &secretsInitRecoveryOwner{root: root, write: SecureWriteClientConfig}, nil
}

const recoveryPrefix = "secrets-init-recovery."

func recoveryHex(s string, n int) bool {
	if len(s) != n || s == strings.Repeat("0", n) {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func recoveryDigest(raw []byte) string { d := sha256.Sum256(raw); return hex.EncodeToString(d[:]) }
func recoveryPrepared(r recoveryRecord) recoveryStage {
	r.State = "prepared"
	r.Key.Outcome = "not_attempted"
	r.Key.RemoveErrorClass = "none"
	r.Vault.Outcome = "not_attempted"
	r.Vault.RemoveErrorClass = "none"
	return recoveryStage{phase: "prepared", record: r}
}
func recoveryCaptureOf(r recoveryRecord) recoveryCapture {
	return recoveryCapture{r.ResolutionCWD, r.Key, r.Vault}
}
func validateRecoveryStage(s recoveryStage) error {
	if s.phase == "retired" {
		if !recoveryHex(s.id, 32) || !recoveryHex(s.digest, 64) || s.reason != "init_succeeded" {
			return errors.New("invalid recovery retirement")
		}
		return nil
	}
	if s.phase != "prepared" && s.phase != "complete" {
		return errors.New("invalid recovery phase")
	}
	r := s.record
	if !recoveryHex(r.AttemptID, 32) {
		return errors.New("invalid recovery attempt ID")
	}
	if err := validateRecoveryCapture(recoveryCaptureOf(r)); err != nil {
		return err
	}
	if s.phase == "prepared" && r.State != "prepared" || s.phase == "complete" && (r.State != "init_failed" && r.State != "init_refused") {
		return errors.New("invalid recovery state")
	}
	if s.phase == "complete" && !recoveryHex(s.digest, 64) {
		return errors.New("invalid recovery binding")
	}
	for _, t := range []recoveryTarget{r.Key, r.Vault} {
		if r.State == "init_failed" {
			if !(t.Outcome == "removed" && t.RemoveErrorClass == "none" || t.Outcome == "absent" && t.RemoveErrorClass == "not_exist" || t.Outcome == "remove_failed" && t.RemoveErrorClass == "other") {
				return errors.New("invalid recovery cleanup facts")
			}
		} else if t.Outcome != "not_attempted" || t.RemoveErrorClass != "none" {
			return errors.New("invalid pending recovery facts")
		}
	}
	return nil
}
func encodeRecoveryStage(s recoveryStage) ([]byte, error) {
	if err := validateRecoveryStage(s); err != nil {
		return nil, err
	}
	var raw []byte
	var err error
	switch s.phase {
	case "prepared":
		raw, err = json.Marshal(struct {
			Version int            `json:"schema_version"`
			Phase   string         `json:"phase"`
			Record  recoveryRecord `json:"record"`
		}{1, s.phase, s.record})
	case "complete":
		raw, err = json.Marshal(struct {
			Version int            `json:"schema_version"`
			Phase   string         `json:"phase"`
			Digest  string         `json:"prepared_sha256"`
			Record  recoveryRecord `json:"record"`
		}{1, s.phase, s.digest, s.record})
	case "retired":
		raw, err = json.Marshal(struct {
			Version int    `json:"schema_version"`
			Phase   string `json:"phase"`
			ID      string `json:"attempt_id"`
			Digest  string `json:"prepared_sha256"`
			Reason  string `json:"reason"`
		}{1, s.phase, s.id, s.digest, s.reason})
	}
	if len(raw) > maxStateFileBytes {
		return nil, errors.New("recovery stage exceeds capacity")
	}
	return raw, err
}

// Validate the three fixed v1 object shapes before any struct decoding. Token
// traversal rejects duplicates, aliases, null and arrays that ordinary struct
// unmarshalling would otherwise merge, replace or default.
func recoveryObjectTokens(d *json.Decoder, shape string) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return errors.New("recovery object required")
	}
	seen := map[string]bool{}
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return err
		}
		name, ok := tok.(string)
		if !ok || seen[name] {
			return errors.New("duplicate recovery member")
		}
		seen[name] = true
		child := ""
		allowed := false
		switch shape {
		case "stage":
			switch name {
			case "schema_version", "phase", "prepared_sha256", "attempt_id", "reason":
				allowed = true
			case "record":
				allowed = true
				child = "record"
			}
		case "record":
			switch name {
			case "attempt_id", "state", "resolution_cwd":
				allowed = true
			case "key", "vault":
				allowed = true
				child = "target"
			}
		case "target":
			switch name {
			case "selected_path", "absolute_path", "outcome", "remove_error_class":
				allowed = true
			}
		}
		if !allowed {
			return errors.New("unknown recovery member")
		}
		if child != "" {
			if err = recoveryObjectTokens(d, child); err != nil {
				return err
			}
			continue
		}
		value, err := d.Token()
		if err != nil {
			return err
		}
		if shape == "stage" && name == "schema_version" {
			n, ok := value.(json.Number)
			if !ok || n.String() != "1" {
				return errors.New("unsupported recovery schema")
			}
		} else {
			if _, ok := value.(string); !ok {
				return errors.New("recovery string required")
			}
		}
	}
	tok, err = d.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('}') {
		return errors.New("invalid recovery object end")
	}
	var required []string
	switch shape {
	case "stage":
		required = []string{"schema_version", "phase"}
	case "record":
		required = []string{"attempt_id", "state", "resolution_cwd", "key", "vault"}
	case "target":
		required = []string{"selected_path", "absolute_path", "outcome", "remove_error_class"}
	}
	for _, name := range required {
		if !seen[name] {
			return errors.New("missing recovery member")
		}
	}
	return nil
}
func decodeRecoveryStage(raw []byte) (recoveryStage, error) {
	var zero recoveryStage
	if len(raw) == 0 || len(raw) > maxStateFileBytes || !utf8.Valid(raw) {
		return zero, errors.New("invalid recovery stage bytes")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := recoveryObjectTokens(d, "stage"); err != nil {
		return zero, err
	}
	if _, err := d.Token(); err != io.EOF {
		return zero, errors.New("trailing recovery value")
	}
	var wire struct {
		Version int            `json:"schema_version"`
		Phase   string         `json:"phase"`
		Record  recoveryRecord `json:"record"`
		ID      string         `json:"attempt_id"`
		Digest  string         `json:"prepared_sha256"`
		Reason  string         `json:"reason"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return zero, err
	}
	s := recoveryStage{wire.Phase, wire.Record, wire.ID, wire.Digest, wire.Reason}
	canonical, err := encodeRecoveryStage(s)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(canonical, raw) {
		return zero, errors.New("noncanonical recovery stage")
	}
	return s, nil
}

func (o *secretsInitRecoveryOwner) leaf(id, phase string) string {
	return filepath.Join(o.root, recoveryPrefix+id+"."+phase+".json")
}
func (o *secretsInitRecoveryOwner) readStage(id, phase string) (recoveryStage, []byte, error) {
	raw, err := readStateFileInodeAnchoredWithStrictPolicyNoAudit(o.leaf(id, phase), func() bool { return true })
	if err != nil {
		return recoveryStage{}, nil, err
	}
	s, err := decodeRecoveryStage(raw)
	if err != nil {
		return recoveryStage{}, nil, err
	}
	got := s.record.AttemptID
	if phase == "retired" {
		got = s.id
	}
	if s.phase != phase || got != id {
		return recoveryStage{}, nil, errors.New("recovery filename binding mismatch")
	}
	return s, raw, nil
}
func (o *secretsInitRecoveryOwner) readStages(id string) (recoveryRecord, []byte, int, error) {
	if !recoveryHex(id, 32) {
		return recoveryRecord{}, nil, 0, errors.New("invalid recovery attempt ID")
	}
	p, raw, err := o.readStage(id, "prepared")
	if err != nil {
		return recoveryRecord{}, nil, 0, err
	}
	record := p.record
	actual := len(raw)
	terminals := 0
	for _, phase := range []string{"complete", "retired"} {
		s, b, e := o.readStage(id, phase)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return recoveryRecord{}, nil, 0, e
		}
		terminals++
		if terminals > 1 || s.digest != recoveryDigest(raw) {
			return recoveryRecord{}, nil, 0, errors.New("conflicting recovery terminal")
		}
		actual += len(b)
		if phase == "complete" {
			canonical, e := encodeRecoveryStage(recoveryPrepared(s.record))
			if e != nil || !bytes.Equal(canonical, raw) {
				return recoveryRecord{}, nil, 0, errors.New("changed recovery capture")
			}
			record = s.record
		} else {
			record.State = "retired"
		}
	}
	return record, raw, actual, nil
}
func (o *secretsInitRecoveryOwner) read(id string) (recoveryRecord, error) {
	r, _, _, err := o.readStages(id)
	return r, err
}
func (o *secretsInitRecoveryOwner) candidateIDs() ([]string, error) {
	entries, err := os.ReadDir(o.root)
	if err != nil {
		return nil, err
	}
	return recoveryCandidateIDs(entries)
}

func recoveryCandidateIDs(entries []os.DirEntry) ([]string, error) {
	ids := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if name == "secrets-init-recovery.lock" {
			continue
		}
		if !strings.HasPrefix(name, "secrets-init-recovery") {
			continue
		}
		parts := strings.Split(name, ".")
		if len(parts) != 4 || parts[0] != "secrets-init-recovery" || !recoveryHex(parts[1], 32) || parts[3] != "json" || (parts[2] != "prepared" && parts[2] != "complete" && parts[2] != "retired") || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("invalid recovery namespace candidate")
		}
		ids[parts[1]] = true
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// Reserve the worst finite terminal size for this exact captured string pair.
// Existing allocations, including logical retirements, never expire.
func recoveryAllocation(prepared []byte) (int, error) {
	p, err := decodeRecoveryStage(prepared)
	if err != nil {
		return 0, err
	}
	maxTerminal := 0
	retired := recoveryStage{phase: "retired", id: p.record.AttemptID, digest: recoveryDigest(prepared), reason: "init_succeeded"}
	b, err := encodeRecoveryStage(retired)
	if err != nil {
		return 0, err
	}
	maxTerminal = len(b)
	variants := []struct{ outcome, class string }{{"removed", "none"}, {"absent", "not_exist"}, {"remove_failed", "other"}}
	for _, k := range variants {
		for _, v := range variants {
			r := p.record
			r.State = "init_failed"
			r.Key.Outcome = k.outcome
			r.Key.RemoveErrorClass = k.class
			r.Vault.Outcome = v.outcome
			r.Vault.RemoveErrorClass = v.class
			b, err = encodeRecoveryStage(recoveryStage{phase: "complete", record: r, digest: recoveryDigest(prepared)})
			if err != nil {
				return 0, err
			}
			if len(b) > maxTerminal {
				maxTerminal = len(b)
			}
		}
	}
	r := p.record
	r.State = "init_refused"
	b, err = encodeRecoveryStage(recoveryStage{phase: "complete", record: r, digest: recoveryDigest(prepared)})
	if err != nil {
		return 0, err
	}
	if len(b) > maxTerminal {
		maxTerminal = len(b)
	}
	if len(prepared) > maxStateFileBytes-maxTerminal {
		return 0, errors.New("recovery allocation exceeds capacity")
	}
	return len(prepared) + maxTerminal, nil
}
func (o *secretsInitRecoveryOwner) capacity(extra []byte) error {
	ids, err := o.candidateIDs()
	if err != nil {
		return err
	}
	total := 0
	for _, id := range ids {
		_, raw, actual, err := o.readStages(id)
		if err != nil {
			return err
		}
		allocation, err := recoveryAllocation(raw)
		if err != nil {
			return err
		}
		if actual > allocation {
			allocation = actual
		}
		if allocation > maxStateFileBytes-total {
			return errors.New("recovery capacity exhausted")
		}
		total += allocation
	}
	allocation, err := recoveryAllocation(extra)
	if err != nil {
		return err
	}
	if allocation > maxStateFileBytes-total {
		return errors.New("recovery capacity exhausted")
	}
	return nil
}
func (o *secretsInitRecoveryOwner) locked(fn func() error) error {
	if o == nil || o.root == "" || !filepath.IsAbs(o.root) || o.write == nil {
		return errors.New("invalid recovery owner")
	}
	lock := flock.New(filepath.Join(o.root, "secrets-init-recovery.lock"))
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	return fn()
}
func (o *secretsInitRecoveryOwner) absent(id string, phases ...string) error {
	for _, phase := range phases {
		_, err := os.Lstat(o.leaf(id, phase))
		if !os.IsNotExist(err) {
			if err != nil {
				return err
			}
			return errors.New("recovery destination already exists")
		}
	}
	return nil
}
func (o *secretsInitRecoveryOwner) publish(s recoveryStage) ([]byte, error) {
	raw, err := encodeRecoveryStage(s)
	if err != nil {
		return nil, err
	}
	id := s.record.AttemptID
	if s.phase == "retired" {
		id = s.id
	}
	if err = o.write(o.leaf(id, s.phase), raw); err != nil {
		return nil, err
	}
	_, back, err := o.readStage(id, s.phase)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, back) {
		return nil, errors.New("recovery protected readback mismatch")
	}
	return raw, nil
}
func (o *secretsInitRecoveryOwner) Reserve(c recoveryCapture) (*recoveryReservation, error) {
	if err := validateRecoveryCapture(c); err != nil {
		return nil, err
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(idBytes[:])
	s := recoveryStage{phase: "prepared", record: recoveryRecord{id, "prepared", c.cwd, c.key, c.vault}}
	raw, err := encodeRecoveryStage(s)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeRecoveryStage(raw)
	if err != nil || decoded.record != s.record {
		return nil, errors.New("invalid recovery prepared roundtrip")
	}
	var reservation *recoveryReservation
	err = o.locked(func() error {
		if err := o.capacity(raw); err != nil {
			return err
		}
		if err := o.absent(id, "prepared", "complete", "retired"); err != nil {
			return err
		}
		if _, err := o.publish(s); err != nil {
			return err
		}
		reservation = &recoveryReservation{owner: o, stage: s, prepared: raw}
		return nil
	})
	return reservation, err
}
func (o *secretsInitRecoveryOwner) terminal(r *recoveryReservation, s recoveryStage) (PublicRecoveryReference, error) {
	ref := PublicRecoveryReference{Status: "unavailable"}
	if r != nil && recoveryHex(r.stage.record.AttemptID, 32) {
		ref = PublicRecoveryReference{r.stage.record.AttemptID, "incomplete"}
	}
	if _, err := encodeRecoveryStage(s); err != nil {
		return ref, err
	}
	err := o.locked(func() error {
		if r == nil || r.owner != o || r.settled {
			return errors.New("recovery reservation not live")
		}
		p, raw, err := o.readStage(r.stage.record.AttemptID, "prepared")
		if err != nil {
			return err
		}
		if p.record != r.stage.record || !bytes.Equal(raw, r.prepared) {
			return errors.New("recovery reservation changed")
		}
		if err = o.absent(p.record.AttemptID, "complete", "retired"); err != nil {
			return err
		}
		r.settled = true
		if _, err = o.publish(s); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return ref, err
	}
	ref.Status = "ready"
	return ref, nil
}
func (o *secretsInitRecoveryOwner) Complete(r *recoveryReservation, state string, targets *secretsInitCleanupTargets) (PublicRecoveryReference, error) {
	if r == nil {
		return PublicRecoveryReference{Status: "unavailable"}, errors.New("missing recovery reservation")
	}
	record := r.stage.record
	record.State = state
	if state == "init_failed" {
		if targets == nil {
			return PublicRecoveryReference{record.AttemptID, "incomplete"}, errors.New("missing recovery cleanup facts")
		}
		for _, pair := range []struct {
			dst *recoveryTarget
			src secretsInitTargetOutcome
		}{{&record.Key, targets.Key}, {&record.Vault, targets.Vault}} {
			if pair.src.Path != pair.dst.AbsolutePath {
				return PublicRecoveryReference{record.AttemptID, "incomplete"}, errors.New("recovery cleanup target changed")
			}
			pair.dst.Outcome = string(pair.src.Outcome)
			switch {
			case pair.src.RemoveError == nil:
				pair.dst.RemoveErrorClass = "none"
			case os.IsNotExist(pair.src.RemoveError):
				pair.dst.RemoveErrorClass = "not_exist"
			default:
				pair.dst.RemoveErrorClass = "other"
			}
		}
	} else if state != "init_refused" || targets != nil {
		return PublicRecoveryReference{record.AttemptID, "incomplete"}, errors.New("invalid recovery disposition")
	}
	return o.terminal(r, recoveryStage{phase: "complete", record: record, digest: recoveryDigest(r.prepared)})
}
func (o *secretsInitRecoveryOwner) retire(r *recoveryReservation) (PublicRecoveryReference, error) {
	if r == nil {
		return PublicRecoveryReference{Status: "unavailable"}, errors.New("missing recovery reservation")
	}
	return o.terminal(r, recoveryStage{phase: "retired", id: r.stage.record.AttemptID, digest: recoveryDigest(r.prepared), reason: "init_succeeded"})
}
func readOnlySecretsInitRecoveryOwner() (*secretsInitRecoveryOwner, error) {
	root, err := DaemonStateDirReadOnly()
	if err != nil {
		return nil, err
	}
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("invalid recovery state root")
	}
	return &secretsInitRecoveryOwner{root: root}, nil
}
func (a *API) ListSecretsInitRecovery() ([]SecretsInitRecoverySummary, error) {
	o, err := readOnlySecretsInitRecoveryOwner()
	if err != nil {
		return nil, err
	}
	entries, err := readSecretsInitRecoveryDirectory(o.root)
	if err != nil {
		return nil, err
	}
	ids, err := recoveryCandidateIDs(entries)
	if err != nil {
		return nil, err
	}
	result := make([]SecretsInitRecoverySummary, 0, len(ids))
	for _, id := range ids {
		r, err := o.read(id)
		if err != nil {
			return nil, err
		}
		result = append(result, SecretsInitRecoverySummary{id, r.State})
	}
	return result, nil
}
func (a *API) ReadSecretsInitRecovery(id string) (SecretsInitRecoveryReport, error) {
	if !recoveryHex(id, 32) {
		return SecretsInitRecoveryReport{}, errors.New("invalid recovery attempt ID")
	}
	o, err := readOnlySecretsInitRecoveryOwner()
	if err != nil {
		return SecretsInitRecoveryReport{}, err
	}
	r, err := o.read(id)
	if err != nil {
		return SecretsInitRecoveryReport{}, err
	}
	return SecretsInitRecoveryReport{r.AttemptID, r.State, r.Key, r.Vault, "historical_attempt_only", "not_inspected"}, nil
}
func recoveryPersistenceFailure(err, initErr error, ref PublicRecoveryReference) error {
	if err == nil {
		return nil
	}
	if ref.Status == "" {
		ref.Status = "unavailable"
	}
	return &SecretsRecoveryPersistenceError{PersistenceCause: fmt.Errorf("recovery persistence: %w", err), InitError: initErr, reference: ref}
}

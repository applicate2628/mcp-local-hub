package clients

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const managedRegistrationBackupMaxBytes int64 = 16 << 20

// ErrManagedRegistrationBackupExists reports that a request-scoped backup
// creator found the final backup leaf already present. The caller must not
// treat an existing leaf as evidence for a new managed mutation.
var ErrManagedRegistrationBackupExists = errors.New("managed registration backup already exists")

type ManagedRegistrationBackupCreateDisposition string

const (
	ManagedRegistrationBackupNotCreated      ManagedRegistrationBackupCreateDisposition = "not-created"
	ManagedRegistrationBackupCommitted       ManagedRegistrationBackupCreateDisposition = "committed"
	ManagedRegistrationBackupCommitUncertain ManagedRegistrationBackupCreateDisposition = "commit-uncertain"
)

type ManagedRegistrationBackupCleanupDisposition string

const (
	ManagedRegistrationBackupCleanupNotRequired ManagedRegistrationBackupCleanupDisposition = "not-required"
	ManagedRegistrationBackupCleanupUnconfirmed ManagedRegistrationBackupCleanupDisposition = "unconfirmed"
)

type ManagedRegistrationBackupCreateReceipt struct {
	Disposition        ManagedRegistrationBackupCreateDisposition
	CleanupDisposition ManagedRegistrationBackupCleanupDisposition
}

// ManagedRegistrationBackupIO is a per-operation bridge to the API-owned
// owner-only create and no-follow read primitives. Implementations must not
// retain contents, and Read returns a caller-owned byte slice.
type ManagedRegistrationBackupIO interface {
	Create(ctx context.Context, parentDir string, leaf string, contents []byte, expectedSHA256 string) (ManagedRegistrationBackupCreateReceipt, error)
	Read(ctx context.Context, parentDir string, leaf string, expectedSHA256 string) ([]byte, error)
}

// ManagedRegistrationEnabledState is the normalized enable polarity of one
// physical client-config entry.
type ManagedRegistrationEnabledState string

const (
	ManagedRegistrationEnabledAbsent    ManagedRegistrationEnabledState = "absent"
	ManagedRegistrationEnabled          ManagedRegistrationEnabledState = "enabled"
	ManagedRegistrationDisabled         ManagedRegistrationEnabledState = "disabled"
	ManagedRegistrationEnabledAmbiguous ManagedRegistrationEnabledState = "ambiguous"
)

// ManagedRegistrationTransport is a fixed, persistence-safe transport class.
type ManagedRegistrationTransport string

const (
	ManagedRegistrationTransportAbsent      ManagedRegistrationTransport = "absent"
	ManagedRegistrationTransportHTTP        ManagedRegistrationTransport = "http"
	ManagedRegistrationTransportStdioRelay  ManagedRegistrationTransport = "stdio-relay"
	ManagedRegistrationTransportStdioDirect ManagedRegistrationTransport = "stdio-direct"
	ManagedRegistrationTransportExternal    ManagedRegistrationTransport = "external"
	ManagedRegistrationTransportUnknown     ManagedRegistrationTransport = "unknown"
)

// ManagedRegistrationIdentityState is a fixed, persistence-safe identity
// verdict. It never carries the identity itself.
type ManagedRegistrationIdentityState string

const (
	ManagedRegistrationIdentityAbsent      ManagedRegistrationIdentityState = "absent"
	ManagedRegistrationIdentityManagedHTTP ManagedRegistrationIdentityState = "managed-http"
	ManagedRegistrationIdentityRelay       ManagedRegistrationIdentityState = "relay"
	ManagedRegistrationIdentityExternal    ManagedRegistrationIdentityState = "external"
	ManagedRegistrationIdentityOpaque      ManagedRegistrationIdentityState = "opaque"
	ManagedRegistrationIdentityUnavailable ManagedRegistrationIdentityState = "identity-unavailable"
)

// ManagedRegistrationSettlement is the durable client-side outcome spelling.
type ManagedRegistrationSettlement string

const (
	ManagedRegistrationSettlementCaptured         ManagedRegistrationSettlement = "captured"
	ManagedRegistrationSettlementPrepared         ManagedRegistrationSettlement = "prepared"
	ManagedRegistrationSettlementRollbackPrepared ManagedRegistrationSettlement = "rollback-prepared"
	ManagedRegistrationSettlementWriteUncertain   ManagedRegistrationSettlement = "write-uncertain"
	ManagedRegistrationSettlementApplied          ManagedRegistrationSettlement = "applied"
	ManagedRegistrationSettlementVerified         ManagedRegistrationSettlement = "verified"
	ManagedRegistrationSettlementRestartRequired  ManagedRegistrationSettlement = "restart-required"
	ManagedRegistrationSettlementSettled          ManagedRegistrationSettlement = "settled"
	ManagedRegistrationSettlementConflict         ManagedRegistrationSettlement = "conflict"
	ManagedRegistrationSettlementRolledBack       ManagedRegistrationSettlement = "rolled-back"
)

// ManagedRegistrationPhase is a fixed safe phase identifier.
type ManagedRegistrationPhase string

const (
	ManagedRegistrationPhaseCapture     ManagedRegistrationPhase = "capture"
	ManagedRegistrationPhaseValidation  ManagedRegistrationPhase = "validation"
	ManagedRegistrationPhaseCompare     ManagedRegistrationPhase = "compare"
	ManagedRegistrationPhaseBackup      ManagedRegistrationPhase = "backup"
	ManagedRegistrationPhaseBeforeWrite ManagedRegistrationPhase = "before-write"
	ManagedRegistrationPhaseMutation    ManagedRegistrationPhase = "mutation"
	ManagedRegistrationPhaseReadback    ManagedRegistrationPhase = "readback"
	ManagedRegistrationPhaseRelease     ManagedRegistrationPhase = "release"
	ManagedRegistrationPhaseRollback    ManagedRegistrationPhase = "rollback"
)

// ManagedRegistrationFailureClass is a fixed safe failure identifier. Raw
// adapter, callback, lock, and panic values never cross the client boundary.
type ManagedRegistrationFailureClass string

const (
	ManagedRegistrationFailureNone                   ManagedRegistrationFailureClass = "none"
	ManagedRegistrationFailureSafeProjection         ManagedRegistrationFailureClass = "safe-projection-failed"
	ManagedRegistrationFailureIdentityUnavailable    ManagedRegistrationFailureClass = "identity-unavailable"
	ManagedRegistrationFailureFingerprintInvalid     ManagedRegistrationFailureClass = "fingerprint-invalid"
	ManagedRegistrationFailurePreconditionConflict   ManagedRegistrationFailureClass = "precondition-conflict"
	ManagedRegistrationFailureEnabledStateInvalid    ManagedRegistrationFailureClass = "enabled-state-invalid"
	ManagedRegistrationFailureDesiredEntryInvalid    ManagedRegistrationFailureClass = "desired-entry-invalid"
	ManagedRegistrationFailureBackup                 ManagedRegistrationFailureClass = "backup-failed"
	ManagedRegistrationFailureBeforeWrite            ManagedRegistrationFailureClass = "before-write-failed"
	ManagedRegistrationFailureAdapterFailed          ManagedRegistrationFailureClass = "adapter-failed"
	ManagedRegistrationFailureReadbackMismatch       ManagedRegistrationFailureClass = "readback-mismatch"
	ManagedRegistrationFailureLockReleaseUnconfirmed ManagedRegistrationFailureClass = "lock-release-unconfirmed"
	ManagedRegistrationFailureBackupIDInvalid        ManagedRegistrationFailureClass = "managed-registration-backup-id-invalid"
	ManagedRegistrationFailureRestoreRequestInvalid  ManagedRegistrationFailureClass = "mcp-registration-restore-request-invalid"
	ManagedRegistrationFailureBackupReferenceInvalid ManagedRegistrationFailureClass = "mcp-registration-backup-reference-invalid"
	ManagedRegistrationFailureTargetGroupMismatch    ManagedRegistrationFailureClass = "target-group-mismatch"
)

// ManagedRegistrationFingerprint is a structurally opaque full SHA-256 value.
// The unexported representation prevents callers from storing arbitrary text in
// a field that is allowed onto persistence/diagnostic surfaces.
type ManagedRegistrationFingerprint struct {
	digest [sha256.Size]byte
	valid  bool
}

func (f ManagedRegistrationFingerprint) String() string {
	if !f.valid {
		return ""
	}
	return hex.EncodeToString(f.digest[:])
}

func (f ManagedRegistrationFingerprint) MarshalJSON() ([]byte, error) {
	return json.Marshal(f.String())
}

func (f *ManagedRegistrationFingerprint) UnmarshalJSON(data []byte) error {
	var raw string
	if f == nil {
		return managedRegistrationError(ManagedRegistrationFailureFingerprintInvalid)
	}
	*f = ManagedRegistrationFingerprint{}
	if json.Unmarshal(data, &raw) != nil || !managedRegistrationFingerprintTextValid(raw) {
		return managedRegistrationError(ManagedRegistrationFailureFingerprintInvalid)
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return managedRegistrationError(ManagedRegistrationFailureFingerprintInvalid)
	}
	copy(f.digest[:], decoded)
	f.valid = true
	return nil
}

// ManagedRegistrationEntrySnapshot is an allowlist-by-construction,
// persistence-safe data-transfer object. Raw identities and MCPEntry cannot be
// represented by this type.
type ManagedRegistrationEntrySnapshot struct {
	ClientID                string
	Exists                  bool
	EnabledState            ManagedRegistrationEnabledState
	Transport               ManagedRegistrationTransport
	IdentityState           ManagedRegistrationIdentityState
	ConfigSHA256            ManagedRegistrationFingerprint
	EntrySHA256             ManagedRegistrationFingerprint
	DesiredProjectionSHA256 ManagedRegistrationFingerprint
	ConfigPathSHA256        ManagedRegistrationFingerprint
	EntryKeySHA256          ManagedRegistrationFingerprint
	TargetIdentitySHA256    ManagedRegistrationFingerprint
}

// ManagedRegistrationBackupID is an opaque, sibling-relative pinned-backup
// identity. Its constructor is private and validates the concrete backup path.
type ManagedRegistrationBackupID struct {
	value string
}

func (id ManagedRegistrationBackupID) String() string {
	return id.value
}

func (id ManagedRegistrationBackupID) MarshalJSON() ([]byte, error) {
	return json.Marshal(id.value)
}

// ParseManagedRegistrationBackupID constructs one opaque implicit-v1 backup
// identity from a complete timestamped backup leaf. Parsing is syntax-only: it
// never resolves, stats, opens, or joins a filesystem path.
func ParseManagedRegistrationBackupID(raw string) (ManagedRegistrationBackupID, error) {
	if !managedRegistrationBackupIDGrammarValid(raw) {
		return ManagedRegistrationBackupID{}, managedRegistrationError(ManagedRegistrationFailureBackupIDInvalid)
	}
	return ManagedRegistrationBackupID{value: raw}, nil
}

// UnmarshalJSON restores an opaque backup identity through the single parser.
// Zero/absence is represented only by null or the empty JSON string.
func (id *ManagedRegistrationBackupID) UnmarshalJSON(data []byte) error {
	if id == nil {
		return managedRegistrationError(ManagedRegistrationFailureBackupIDInvalid)
	}
	*id = ManagedRegistrationBackupID{}
	trimmed := bytes.TrimSpace(data)
	if !utf8.Valid(trimmed) {
		return managedRegistrationError(ManagedRegistrationFailureBackupIDInvalid)
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if !managedRegistrationBackupIDJSONSurrogatesValid(trimmed) {
		return managedRegistrationError(ManagedRegistrationFailureBackupIDInvalid)
	}
	var raw string
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return managedRegistrationError(ManagedRegistrationFailureBackupIDInvalid)
	}
	if raw == "" {
		return nil
	}
	parsed, err := ParseManagedRegistrationBackupID(raw)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// managedRegistrationBackupIDJSONSurrogatesValid rejects unpaired UTF-16
// surrogate escapes before encoding/json can replace them with U+FFFD. It is
// deliberately not a second JSON parser: non-string syntax, ordinary escapes,
// and malformed non-surrogate JSON remain owned by encoding/json.
func managedRegistrationBackupIDJSONSurrogatesValid(data []byte) bool {
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return true
	}
	end := len(data) - 1
	for i := 1; i < end; i++ {
		if data[i] != '\\' || i+1 >= end {
			continue
		}
		if data[i+1] != 'u' {
			i++ // Skip the escaped byte, including an escaped backslash.
			continue
		}
		if i+5 >= end {
			return true // The standard decoder owns truncated JSON escapes.
		}
		code, ok := managedRegistrationJSONHex16(data[i+2 : i+6])
		if !ok {
			return true // The standard decoder owns non-hex JSON escapes.
		}
		switch {
		case code >= 0xD800 && code <= 0xDBFF:
			next := i + 6
			if next+5 >= end || data[next] != '\\' || data[next+1] != 'u' {
				return false
			}
			low, lowOK := managedRegistrationJSONHex16(data[next+2 : next+6])
			if !lowOK || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i = next + 5
		case code >= 0xDC00 && code <= 0xDFFF:
			return false
		default:
			i += 5
		}
	}
	return true
}

func managedRegistrationJSONHex16(raw []byte) (uint16, bool) {
	if len(raw) != 4 {
		return 0, false
	}
	var value uint16
	for _, char := range raw {
		value <<= 4
		switch {
		case char >= '0' && char <= '9':
			value |= uint16(char - '0')
		case char >= 'a' && char <= 'f':
			value |= uint16(char-'a') + 10
		case char >= 'A' && char <= 'F':
			value |= uint16(char-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func managedRegistrationBackupIDGrammarValid(raw string) bool {
	if raw == "" || raw == "." || raw == ".." || !utf8.ValidString(raw) || filepath.IsAbs(raw) || filepath.VolumeName(raw) != "" || filepath.Base(raw) != raw || strings.ContainsAny(raw, `/\:`) {
		return false
	}
	for _, char := range raw {
		if unicode.IsControl(char) {
			return false
		}
	}
	marker := strings.LastIndex(raw, backupSuffixPrefix)
	if marker <= 0 {
		return false
	}
	liveBase := raw[:marker]
	if liveBase == "" || liveBase == "." || liveBase == ".." {
		return false
	}
	tail := raw[marker+len(backupSuffixPrefix):]
	if len(tail) != len("20060102-150405") && len(tail) != len("20060102-150405-000000002") {
		return false
	}
	stamp := tail[:len("20060102-150405")]
	parsedTime, err := time.Parse("20060102-150405", stamp)
	if err != nil || parsedTime.Format("20060102-150405") != stamp {
		return false
	}
	if len(tail) == len("20060102-150405") {
		return true
	}
	if tail[len("20060102-150405")] != '-' {
		return false
	}
	collision := tail[len("20060102-150405-"):]
	if len(collision) != 9 {
		return false
	}
	for _, digit := range collision {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	value, err := strconv.ParseUint(collision, 10, 32)
	return err == nil && value >= 2
}

// ManagedRegistrationMutationPreparation is the only callback payload.
type ManagedRegistrationMutationPreparation struct {
	Before                  ManagedRegistrationEntrySnapshot
	BackupID                ManagedRegistrationBackupID
	DesiredProjectionSHA256 ManagedRegistrationFingerprint
	TargetIdentitySHA256    ManagedRegistrationFingerprint
}

// ManagedRegistrationConditionalBackupRestoreRequest is transient rollback
// input. Raw backup bytes, paths, and original MCPEntry never cross this seam.
type ManagedRegistrationConditionalBackupRestoreRequest struct {
	EntryName                 string
	BackupID                  ManagedRegistrationBackupID
	BackupIO                  ManagedRegistrationBackupIO
	CodexInverse              *CodexHTTPInverseRelocation
	ExpectedApplied           ManagedRegistrationEntrySnapshot
	ExpectedBaseline          ManagedRegistrationEntrySnapshot
	ExpectedTargetGroupSHA256 ManagedRegistrationFingerprint
	BeforeWrite               func(ManagedRegistrationBackupRestorePreparation) error
}

// ManagedRegistrationBackupRestorePreparation is the safe callback payload
// used to durably arm rollback before the first adapter write.
type ManagedRegistrationBackupRestorePreparation struct {
	Applied  ManagedRegistrationEntrySnapshot
	Baseline ManagedRegistrationEntrySnapshot
	BackupID ManagedRegistrationBackupID
}

// ManagedRegistrationMutationRequest is transient call input. EntryName and
// Desired are consumed only inside ConditionalManagedRegistrationMutation and
// are never copied into a returned safe type.
type ManagedRegistrationMutationRequest struct {
	EntryName                string
	ExpectedConfigSHA256     ManagedRegistrationFingerprint
	ExpectedEntrySHA256      ManagedRegistrationFingerprint
	ExpectedConfigPathSHA256 ManagedRegistrationFingerprint
	ExpectedEntryKeySHA256   ManagedRegistrationFingerprint
	ExpectedBaseline         ManagedRegistrationEntrySnapshot
	ExpectedTargetSHA256     ManagedRegistrationFingerprint
	PreparedBackupID         ManagedRegistrationBackupID
	Desired                  MCPEntry
	BackupKeepN              int
	BackupIO                 ManagedRegistrationBackupIO
	CodexRelocation          *ManagedRegistrationCodexRelocation
	BeforeWrite              func(ManagedRegistrationMutationPreparation) error
}

// ManagedRegistrationCodexRelocation is the raw-free frozen relocation proof.
// The locking client reconstructs SourceSnapshot only from its pinned backup.
type ManagedRegistrationCodexRelocation struct {
	SourceEntryName string
	TargetEntryName string
	ExpectedSource  CodexTransport
}

// ManagedRegistrationMutationObserved contains safe snapshots and fixed
// identifiers only. It cannot carry a path, raw error, panic, or MCPEntry.
type ManagedRegistrationMutationObserved struct {
	Invoked              bool
	Before               ManagedRegistrationEntrySnapshot
	After                ManagedRegistrationEntrySnapshot
	PreconditionConflict bool
	Phase                ManagedRegistrationPhase
	Failure              ManagedRegistrationFailureClass
	Settlement           ManagedRegistrationSettlement
}

// ManagedRegistrationClient is an optional capability owned only by the
// locking wrapper. Concrete syntax adapters intentionally do not implement it.
type ManagedRegistrationClient interface {
	SnapshotManagedRegistration(context.Context, string) (ManagedRegistrationEntrySnapshot, error)
	ManagedRegistrationDesiredMatchesSnapshot(context.Context, string, MCPEntry, ManagedRegistrationEntrySnapshot) (bool, error)
	ResolveManagedRegistrationEntryName(context.Context, []string, ManagedRegistrationEntrySnapshot) (string, error)
	ConditionalManagedRegistrationMutation(context.Context, ManagedRegistrationMutationRequest) ManagedRegistrationMutationObserved
	ConditionalManagedRegistrationExactCreate(context.Context, ManagedRegistrationMutationRequest) ManagedRegistrationMutationObserved
	ConditionalManagedRegistrationBackupRestore(context.Context, ManagedRegistrationConditionalBackupRestoreRequest) ManagedRegistrationMutationObserved
}

// ResolveManagedRegistrationEntryName re-resolves the transient adapter key
// while holding the same config read lock used by snapshot capture. Only an
// exact, unique safe-snapshot tuple is admitted; the raw key never leaves the
// immediate reconciler call.
func (l *lockingClient) ResolveManagedRegistrationEntryName(ctx context.Context, candidates []string, expected ManagedRegistrationEntrySnapshot) (name string, err error) {
	if ctx == nil || ctx.Err() != nil {
		return "", managedRegistrationError(ManagedRegistrationFailureIdentityUnavailable)
	}
	if l == nil || l.Client == nil || expected.ClientID == "" || expected.ClientID != l.Client.Name() {
		return "", managedRegistrationError(ManagedRegistrationFailureIdentityUnavailable)
	}
	lockErr := withConfigReadLock(l.Client.ConfigPath(), func() error {
		seen := make(map[string]struct{}, len(candidates))
		for _, candidate := range candidates {
			if candidate == "" {
				return managedRegistrationError(ManagedRegistrationFailureIdentityUnavailable)
			}
			if _, duplicate := seen[candidate]; duplicate {
				return managedRegistrationError(ManagedRegistrationFailureIdentityUnavailable)
			}
			seen[candidate] = struct{}{}
			_, snapshot, captureErr := l.captureManagedRegistrationLocked(candidate)
			if captureErr != nil {
				return captureErr
			}
			if snapshot.ClientID != expected.ClientID || snapshot.Exists != expected.Exists || snapshot.EnabledState != expected.EnabledState || snapshot.Transport != expected.Transport || snapshot.IdentityState != expected.IdentityState || snapshot.ConfigSHA256 != expected.ConfigSHA256 || snapshot.EntrySHA256 != expected.EntrySHA256 || snapshot.DesiredProjectionSHA256 != expected.DesiredProjectionSHA256 || snapshot.ConfigPathSHA256 != expected.ConfigPathSHA256 || snapshot.EntryKeySHA256 != expected.EntryKeySHA256 || snapshot.TargetIdentitySHA256 != expected.TargetIdentitySHA256 {
				continue
			}
			if name != "" {
				return managedRegistrationError(ManagedRegistrationFailureIdentityUnavailable)
			}
			name = candidate
		}
		if name == "" {
			return managedRegistrationError(ManagedRegistrationFailureIdentityUnavailable)
		}
		return nil
	})
	if lockErr != nil {
		return "", lockErr
	}
	return name, nil
}

var _ ManagedRegistrationClient = (*lockingClient)(nil)

// AsManagedRegistrationClient admits only the existing config-lock wrapper.
func AsManagedRegistrationClient(client Client) (ManagedRegistrationClient, bool) {
	locking, ok := client.(*lockingClient)
	if !ok || locking == nil {
		return nil, false
	}
	return locking, true
}

type managedRegistrationSafeError struct {
	failure ManagedRegistrationFailureClass
}

func (e *managedRegistrationSafeError) Error() string {
	if e == nil || e.failure == ManagedRegistrationFailureNone {
		return "managed registration failed"
	}
	return "managed registration: " + string(e.failure)
}

func managedRegistrationError(failure ManagedRegistrationFailureClass) error {
	return &managedRegistrationSafeError{failure: failure}
}

// ManagedRegistrationFailureClassOf extracts the fixed class from a safe
// boundary error without exposing an underlying cause.
func ManagedRegistrationFailureClassOf(err error) ManagedRegistrationFailureClass {
	if err == nil {
		return ManagedRegistrationFailureNone
	}
	var safe *managedRegistrationSafeError
	if errors.As(err, &safe) {
		return safe.failure
	}
	return ManagedRegistrationFailureAdapterFailed
}

type managedRegistrationRawState struct {
	clientID   string
	configPath string
	entryName  string
	config     []byte
	rawEntry   any
	entry      *MCPEntry
	exists     bool
	rawExact   bool
}

type managedRegistrationEntryRawExtractor interface {
	EntryRawSubtree(configBytes []byte, name string) (subtree any, present bool, err error)
}

type managedRegistrationDesiredState struct {
	transport            ManagedRegistrationTransport
	http                 managedHTTPIdentity
	relay                managedRelayIdentity
	projectionSHA256     ManagedRegistrationFingerprint
	targetIdentitySHA256 ManagedRegistrationFingerprint
	disabled             bool
	toolTimeoutSec       int
}

func (l *lockingClient) SnapshotManagedRegistration(ctx context.Context, entryName string) (snapshot ManagedRegistrationEntrySnapshot, err error) {
	if ctx == nil || ctx.Err() != nil || entryName == "" {
		return snapshot, managedRegistrationError(ManagedRegistrationFailureSafeProjection)
	}
	var captureErr error
	lockErr := withConfigReadLock(l.Client.ConfigPath(), func() error {
		_, snapshot, captureErr = l.captureManagedRegistrationLocked(entryName)
		return nil
	})
	if captureErr != nil {
		return ManagedRegistrationEntrySnapshot{}, captureErr
	}
	if lockErr != nil {
		failure := ManagedRegistrationFailureAdapterFailed
		if errors.Is(lockErr, ErrConfigLockReleaseUnconfirmed) {
			failure = ManagedRegistrationFailureLockReleaseUnconfirmed
		}
		return ManagedRegistrationEntrySnapshot{}, managedRegistrationError(failure)
	}
	return snapshot, nil
}

// ManagedRegistrationDesiredMatchesSnapshot proves that one exact physical
// entry still equals a caller's frozen desired value while holding the same
// read lock used by snapshot capture. The client owner retains all adapter-
// specific presentation comparisons, including Codex ToolTimeoutSec.
func (l *lockingClient) ManagedRegistrationDesiredMatchesSnapshot(ctx context.Context, entryName string, desired MCPEntry, expected ManagedRegistrationEntrySnapshot) (matched bool, err error) {
	if ctx == nil || ctx.Err() != nil || entryName == "" || desired.Name != entryName || expected.ClientID != l.Client.Name() {
		return false, managedRegistrationError(ManagedRegistrationFailureDesiredEntryInvalid)
	}
	lockErr := withConfigReadLock(l.Client.ConfigPath(), func() error {
		raw, current, captureErr := l.captureManagedRegistrationLocked(entryName)
		if captureErr != nil {
			return captureErr
		}
		if current != expected {
			return managedRegistrationError(ManagedRegistrationFailurePreconditionConflict)
		}
		validated, failure := l.validateManagedRegistrationDesired(entryName, desired)
		if failure != ManagedRegistrationFailureNone {
			return managedRegistrationError(failure)
		}
		matchesReadback, _ := managedRegistrationDesiredMatchesReadback(validated, raw)
		matched = matchesReadback &&
			current.DesiredProjectionSHA256 == validated.projectionSHA256 &&
			current.TargetIdentitySHA256 == validated.targetIdentitySHA256
		return nil
	})
	if lockErr != nil {
		return false, lockErr
	}
	if err := ctx.Err(); err != nil {
		return false, managedRegistrationError(ManagedRegistrationFailurePreconditionConflict)
	}
	return matched, nil
}

type managedRegistrationMutationOperation uint8

const (
	managedRegistrationMutationExisting managedRegistrationMutationOperation = iota + 1
	managedRegistrationMutationExactCreate
)

func (l *lockingClient) ConditionalManagedRegistrationMutation(ctx context.Context, request ManagedRegistrationMutationRequest) ManagedRegistrationMutationObserved {
	return l.conditionalManagedRegistrationMutation(ctx, request, managedRegistrationMutationExisting)
}

func (l *lockingClient) ConditionalManagedRegistrationExactCreate(ctx context.Context, request ManagedRegistrationMutationRequest) ManagedRegistrationMutationObserved {
	return l.conditionalManagedRegistrationMutation(ctx, request, managedRegistrationMutationExactCreate)
}

func (l *lockingClient) conditionalManagedRegistrationMutation(ctx context.Context, request ManagedRegistrationMutationRequest, operation managedRegistrationMutationOperation) (observed ManagedRegistrationMutationObserved) {
	observed.Phase = ManagedRegistrationPhaseValidation
	observed.Failure = ManagedRegistrationFailureNone
	observed.Settlement = ManagedRegistrationSettlementCaptured
	if ctx == nil || (operation != managedRegistrationMutationExisting && operation != managedRegistrationMutationExactCreate) || request.EntryName == "" || request.Desired.Name != request.EntryName || request.BackupKeepN < 0 || request.BeforeWrite == nil {
		observed.Failure = ManagedRegistrationFailureDesiredEntryInvalid
		return observed
	}
	if request.BackupIO == nil {
		observed.Phase = ManagedRegistrationPhaseBackup
		observed.Failure = ManagedRegistrationFailureBackup
		return observed
	}
	if ctx.Err() != nil {
		observed.Phase = ManagedRegistrationPhaseBackup
		observed.Failure = ManagedRegistrationFailureBackup
		return observed
	}
	for _, fingerprint := range []ManagedRegistrationFingerprint{
		request.ExpectedConfigSHA256,
		request.ExpectedEntrySHA256,
		request.ExpectedConfigPathSHA256,
		request.ExpectedEntryKeySHA256,
		request.ExpectedTargetSHA256,
	} {
		if !fingerprint.valid {
			observed.Failure = ManagedRegistrationFailureFingerprintInvalid
			return observed
		}
	}

	execution, _ := l.withManagedRegistrationMutationLock(ctx, func() error {
		rawBefore, before, captureErr := l.captureManagedRegistrationLocked(request.EntryName)
		observed.Before = before
		if captureErr != nil {
			observed.Phase = ManagedRegistrationPhaseCapture
			observed.Failure = ManagedRegistrationFailureClassOf(captureErr)
			return nil
		}

		desired, desiredFailure := l.validateManagedRegistrationDesired(request.EntryName, request.Desired)
		if desiredFailure != ManagedRegistrationFailureNone {
			observed.Phase = ManagedRegistrationPhaseValidation
			observed.Failure = desiredFailure
			return nil
		}
		if desired.targetIdentitySHA256 != request.ExpectedTargetSHA256 {
			observed.Phase = ManagedRegistrationPhaseValidation
			observed.Failure = ManagedRegistrationFailureTargetGroupMismatch
			return nil
		}
		if rawBefore.config == nil ||
			before.ConfigSHA256 != request.ExpectedConfigSHA256 ||
			before.EntrySHA256 != request.ExpectedEntrySHA256 ||
			before.ConfigPathSHA256 != request.ExpectedConfigPathSHA256 ||
			before.EntryKeySHA256 != request.ExpectedEntryKeySHA256 {
			observed.Phase = ManagedRegistrationPhaseCompare
			observed.Failure = ManagedRegistrationFailurePreconditionConflict
			observed.PreconditionConflict = true
			observed.Settlement = ManagedRegistrationSettlementConflict
			return nil
		}
		if request.ExpectedBaseline.ClientID == "" || before != request.ExpectedBaseline {
			observed.Phase = ManagedRegistrationPhaseCompare
			observed.Failure = ManagedRegistrationFailurePreconditionConflict
			observed.PreconditionConflict = true
			observed.Settlement = ManagedRegistrationSettlementConflict
			return nil
		}
		if before.IdentityState == ManagedRegistrationIdentityUnavailable || before.IdentityState == ManagedRegistrationIdentityOpaque {
			observed.Phase = ManagedRegistrationPhaseValidation
			observed.Failure = ManagedRegistrationFailureIdentityUnavailable
			return nil
		}
		if before.EnabledState == ManagedRegistrationEnabledAmbiguous {
			observed.Phase = ManagedRegistrationPhaseCompare
			observed.Failure = ManagedRegistrationFailureEnabledStateInvalid
			return nil
		}
		if !before.Exists {
			if operation != managedRegistrationMutationExactCreate {
				observed.Phase = ManagedRegistrationPhaseCompare
				observed.Failure = ManagedRegistrationFailurePreconditionConflict
				observed.PreconditionConflict = true
				observed.Settlement = ManagedRegistrationSettlementConflict
				return nil
			}
			if !rawBefore.rawExact || rawBefore.rawEntry != nil || rawBefore.entry != nil || rawBefore.exists || before.EnabledState != ManagedRegistrationEnabledAbsent || before.Transport != ManagedRegistrationTransportAbsent || before.IdentityState != ManagedRegistrationIdentityAbsent || before.TargetIdentitySHA256.String() != "" || before.DesiredProjectionSHA256.String() != "" {
				observed.Phase = ManagedRegistrationPhaseValidation
				observed.Failure = ManagedRegistrationFailureIdentityUnavailable
				return nil
			}
		} else if operation == managedRegistrationMutationExactCreate {
			observed.Phase = ManagedRegistrationPhaseCompare
			observed.Failure = ManagedRegistrationFailurePreconditionConflict
			observed.PreconditionConflict = true
			observed.Settlement = ManagedRegistrationSettlementConflict
			return nil
		}

		var expectedCodexBytes []byte
		if l.Client.Name() == "codex-cli" && (before.Exists || request.CodexRelocation != nil) {
			sourceName := request.EntryName
			if request.CodexRelocation != nil {
				sourceName = request.CodexRelocation.SourceEntryName
			}
			var transformErr error
			expectedCodexBytes, transformErr = replaceCodexTransportBytes(rawBefore.config, sourceName, request.EntryName, request.Desired)
			if transformErr != nil {
				observed.Failure = ManagedRegistrationFailureDesiredEntryInvalid
				return nil
			}
			document, decodeErr := decodeCodexTOMLBytes(expectedCodexBytes)
			if decodeErr != nil {
				observed.Failure = ManagedRegistrationFailureDesiredEntryInvalid
				return nil
			}
			preserved := document["mcp_servers"].(map[string]any)[request.EntryName].(map[string]any)
			if managedRegistrationEnabledState(preserved, true) == ManagedRegistrationEnabledAmbiguous {
				observed.Failure = ManagedRegistrationFailureEnabledStateInvalid
				return nil
			}
			preservedDesired := request.Desired
			preservedDesired.Disabled = mcpEntryDisabled(preserved)
			preservedDesired.ToolTimeoutSec, transformErr = codexToolTimeoutSec(preserved)
			if transformErr != nil {
				observed.Failure = ManagedRegistrationFailureDesiredEntryInvalid
				return nil
			}
			desired, desiredFailure = l.validateManagedRegistrationDesired(request.EntryName, preservedDesired)
			if desiredFailure != ManagedRegistrationFailureNone {
				observed.Failure = desiredFailure
				return nil
			}
		}

		backupID := request.PreparedBackupID
		var backupBytes []byte
		defer clear(backupBytes)
		if backupID.String() != "" {
			parsed, err := ParseManagedRegistrationBackupID(backupID.String())
			var readErr error
			backupBytes, readErr = readManagedRegistrationBackup(ctx, request.BackupIO, rawBefore.configPath, backupID, before.ConfigSHA256.String())
			baseline, baselineErr := l.managedRegistrationSnapshotFromBytes(request.EntryName, backupBytes)
			if err != nil || parsed != backupID || readErr != nil || baselineErr != nil || before.Exists || !managedRegistrationBaselineSnapshotMatches(baseline, before) {
				observed.Phase = ManagedRegistrationPhaseBackup
				observed.Failure = ManagedRegistrationFailureBackupReferenceInvalid
				return nil
			}
		} else {
			created, createErr := l.createManagedRegistrationBackup(ctx, request.BackupIO, rawBefore.configPath, rawBefore.config, before.ConfigSHA256.String())
			if createErr != nil {
				observed.Phase = ManagedRegistrationPhaseBackup
				observed.Failure = ManagedRegistrationFailureBackup
				return nil
			}
			backupID = created
			parentDir, leaf, locationOK := managedRegistrationBackupLocation(rawBefore.configPath, backupID)
			if !locationOK {
				observed.Phase = ManagedRegistrationPhaseBackup
				observed.Failure = ManagedRegistrationFailureBackup
				return nil
			}
			var readErr error
			backupBytes, readErr = request.BackupIO.Read(ctx, parentDir, leaf, before.ConfigSHA256.String())
			if readErr != nil || managedRegistrationBytesSHA256(backupBytes) != before.ConfigSHA256 {
				observed.Phase = ManagedRegistrationPhaseBackup
				observed.Failure = ManagedRegistrationFailureBackup
				return nil
			}
		}
		_, afterBackup, afterBackupErr := l.captureManagedRegistrationLocked(request.EntryName)
		if afterBackupErr != nil {
			observed.Phase = ManagedRegistrationPhaseCapture
			observed.Failure = ManagedRegistrationFailureClassOf(afterBackupErr)
			return nil
		}
		if afterBackup != before {
			observed.Phase = ManagedRegistrationPhaseCompare
			observed.Failure = ManagedRegistrationFailurePreconditionConflict
			observed.PreconditionConflict = true
			observed.Settlement = ManagedRegistrationSettlementConflict
			return nil
		}

		if invokeManagedRegistrationBeforeWrite(request.BeforeWrite, ManagedRegistrationMutationPreparation{
			Before: before, BackupID: backupID, DesiredProjectionSHA256: desired.projectionSHA256, TargetIdentitySHA256: desired.targetIdentitySHA256,
		}) {
			observed.Phase = ManagedRegistrationPhaseBeforeWrite
			observed.Failure = ManagedRegistrationFailureBeforeWrite
			return nil
		}
		if request.BackupKeepN > 0 {
			pruneOldTimestamped(l.Client.ConfigPath(), request.BackupKeepN)
		}
		if ctx.Err() != nil {
			observed.Phase = ManagedRegistrationPhaseBackup
			observed.Failure = ManagedRegistrationFailureBackup
			return nil
		}
		observed.Settlement = ManagedRegistrationSettlementPrepared
		_, beforeInvoke, beforeInvokeErr := l.captureManagedRegistrationLocked(request.EntryName)
		if beforeInvokeErr != nil {
			observed.Phase = ManagedRegistrationPhaseCapture
			observed.Failure = ManagedRegistrationFailureClassOf(beforeInvokeErr)
			return nil
		}
		if beforeInvoke != before {
			observed.Phase = ManagedRegistrationPhaseCompare
			observed.Failure = ManagedRegistrationFailurePreconditionConflict
			observed.PreconditionConflict = true
			observed.Settlement = ManagedRegistrationSettlementConflict
			return nil
		}
		observed.Invoked = true
		mutationFailed := invokeManagedRegistrationMutation(func() error {
			if request.CodexRelocation == nil {
				return l.Client.AddEntry(request.Desired)
			}
			relocation := *request.CodexRelocation
			if relocation.SourceEntryName == "" || relocation.TargetEntryName != request.EntryName {
				return managedRegistrationError(ManagedRegistrationFailureDesiredEntryInvalid)
			}
			extractor, ok := l.Client.(managedRegistrationEntryRawExtractor)
			if !ok {
				return managedRegistrationError(ManagedRegistrationFailureDesiredEntryInvalid)
			}
			rawSource, present, extractErr := extractor.EntryRawSubtree(backupBytes, relocation.SourceEntryName)
			sourceSnapshot, sourceOK := rawSource.(map[string]any)
			if extractErr != nil || !present || !sourceOK || len(sourceSnapshot) == 0 {
				return managedRegistrationError(ManagedRegistrationFailureBackupReferenceInvalid)
			}
			codex, ok := l.Client.(interface {
				RelocateHTTPEntry(CodexHTTPRelocation) (CodexHTTPRelocationResult, error)
			})
			if !ok {
				return managedRegistrationError(ManagedRegistrationFailureDesiredEntryInvalid)
			}
			_, err := codex.RelocateHTTPEntry(CodexHTTPRelocation{
				SourceEntryName:      relocation.SourceEntryName,
				TargetEntryName:      relocation.TargetEntryName,
				Entry:                request.Desired,
				ExpectedSource:       relocation.ExpectedSource,
				ExpectedConfigSHA256: before.ConfigSHA256.String(),
				SourceSnapshot:       sourceSnapshot,
			})
			return err
		})

		rawAfter, after, readbackErr := l.captureManagedRegistrationLocked(request.EntryName)
		observed.After = after
		observed.After.TargetIdentitySHA256 = desired.targetIdentitySHA256
		if mutationFailed {
			observed.Phase = ManagedRegistrationPhaseMutation
			observed.Failure = ManagedRegistrationFailureAdapterFailed
			observed.Settlement = ManagedRegistrationSettlementWriteUncertain
			return nil
		}
		if readbackErr != nil {
			observed.Phase = ManagedRegistrationPhaseReadback
			observed.Failure = ManagedRegistrationFailureClassOf(readbackErr)
			observed.Settlement = ManagedRegistrationSettlementWriteUncertain
			return nil
		}
		match, matchFailure := managedRegistrationDesiredMatchesReadback(desired, rawAfter)
		if expectedCodexBytes != nil && !bytes.Equal(expectedCodexBytes, rawAfter.config) {
			match, matchFailure = false, ManagedRegistrationFailureReadbackMismatch
		}
		if !match {
			observed.Phase = ManagedRegistrationPhaseReadback
			observed.Failure = matchFailure
			observed.Settlement = ManagedRegistrationSettlementWriteUncertain
			return nil
		}
		observed.Phase = ManagedRegistrationPhaseReadback
		observed.Failure = ManagedRegistrationFailureNone
		observed.Settlement = ManagedRegistrationSettlementVerified
		return nil
	})

	if execution.releaseErr != nil {
		observed.Phase = ManagedRegistrationPhaseRelease
		observed.Failure = ManagedRegistrationFailureLockReleaseUnconfirmed
		if observed.Invoked {
			observed.Settlement = ManagedRegistrationSettlementWriteUncertain
		}
	}
	if !execution.bodyEntered && observed.Failure == ManagedRegistrationFailureNone {
		observed.Phase = ManagedRegistrationPhaseCapture
		observed.Failure = ManagedRegistrationFailureAdapterFailed
	}
	return observed
}

// ManagedRegistrationTargetFingerprint returns the exact client-owned target
// identity used during locked desired validation. The API runtime uses it to
// bind its target-group proof to the same identity the write/readback owner
// will observe.
func ManagedRegistrationTargetFingerprint(entry MCPEntry, relay bool) (ManagedRegistrationFingerprint, error) {
	if relay {
		identity, failure := managedRegistrationRelayIdentityFromDesired(entry)
		if failure != ManagedRegistrationFailureNone {
			return ManagedRegistrationFingerprint{}, managedRegistrationError(failure)
		}
		return managedRegistrationRelayIdentitySHA256(identity), nil
	}
	identity, failure := managedRegistrationHTTPIdentity(entry.URL)
	if failure != ManagedRegistrationFailureNone {
		return ManagedRegistrationFingerprint{}, managedRegistrationError(failure)
	}
	return managedRegistrationHTTPIdentitySHA256(identity), nil
}

type managedRegistrationBackupBytesRestorer interface {
	restoreEntryFromBytes([]byte, string, bool, WriteConfigFileFunc) error
}

func (l *lockingClient) ConditionalManagedRegistrationBackupRestore(ctx context.Context, request ManagedRegistrationConditionalBackupRestoreRequest) (observed ManagedRegistrationMutationObserved) {
	observed.Phase = ManagedRegistrationPhaseRollback
	observed.Failure = ManagedRegistrationFailureNone
	observed.Settlement = ManagedRegistrationSettlementCaptured
	if ctx == nil || !l.managedRegistrationRestoreRequestValid(request) {
		observed.Failure = ManagedRegistrationFailureRestoreRequestInvalid
		return observed
	}

	execution, _ := l.withManagedRegistrationMutationLock(ctx, func() error {
		_, current, captureErr := l.captureManagedRegistrationLocked(request.EntryName)
		observed.Before = current
		if captureErr != nil {
			observed.Failure = ManagedRegistrationFailureClassOf(captureErr)
			return nil
		}
		if !managedRegistrationAppliedSnapshotMatches(current, request.ExpectedApplied) {
			observed.PreconditionConflict = true
			observed.Failure = ManagedRegistrationFailurePreconditionConflict
			observed.Settlement = ManagedRegistrationSettlementConflict
			return nil
		}

		backupBytes, backupErr := readManagedRegistrationBackup(ctx, request.BackupIO, l.Client.ConfigPath(), request.BackupID, request.ExpectedBaseline.ConfigSHA256.String())
		defer clear(backupBytes)
		if backupErr != nil {
			observed.Failure = ManagedRegistrationFailureBackupReferenceInvalid
			return nil
		}
		baseline, baselineErr := l.managedRegistrationSnapshotFromBytes(request.EntryName, backupBytes)
		if baselineErr != nil || !managedRegistrationBaselineSnapshotMatches(baseline, request.ExpectedBaseline) {
			observed.Failure = ManagedRegistrationFailureBackupReferenceInvalid
			return nil
		}
		if invokeManagedRegistrationBackupRestoreBeforeWrite(request.BeforeWrite, ManagedRegistrationBackupRestorePreparation{
			Applied: request.ExpectedApplied, Baseline: request.ExpectedBaseline, BackupID: request.BackupID,
		}) {
			observed.Failure = ManagedRegistrationFailureBeforeWrite
			return nil
		}

		restorer, ok := l.Client.(managedRegistrationBackupBytesRestorer)
		if !ok {
			observed.Failure = ManagedRegistrationFailureRestoreRequestInvalid
			return nil
		}
		observed.Settlement = ManagedRegistrationSettlementRollbackPrepared
		observed.Invoked = true
		if invokeManagedRegistrationMutation(func() error {
			if request.CodexInverse == nil {
				return restorer.restoreEntryFromBytes(backupBytes, request.EntryName, true, nil)
			}
			inverse := *request.CodexInverse
			if inverse.exactInverse == nil {
				codex, ok := l.Client.(*codexCLI)
				if !ok {
					return managedRegistrationError(ManagedRegistrationFailureRestoreRequestInvalid)
				}
				appliedBytes, err := replaceCodexTransportBytes(backupBytes, inverse.SourceEntryName, inverse.TargetEntryName, inverse.Target)
				if err != nil || managedRegistrationBytesSHA256(appliedBytes) != request.ExpectedApplied.ConfigSHA256 {
					return managedRegistrationError(ManagedRegistrationFailureBackupReferenceInvalid)
				}
				inverse.exactInverse = cloneCodexHTTPInverseCapsule(&codexHTTPInverseCapsule{
					configPath: codex.path, source: inverse.SourceEntryName, target: inverse.TargetEntryName,
					entry: inverse.Target, before: backupBytes, applied: appliedBytes,
				})
			}
			codex, ok := l.Client.(interface {
				RestoreRelocatedHTTPEntry(CodexHTTPInverseRelocation) (CodexHTTPInverseResult, error)
			})
			if !ok {
				return managedRegistrationError(ManagedRegistrationFailureRestoreRequestInvalid)
			}
			_, err := codex.RestoreRelocatedHTTPEntry(inverse)
			return err
		}) {
			observed.Failure = ManagedRegistrationFailureAdapterFailed
			observed.Settlement = ManagedRegistrationSettlementWriteUncertain
			return nil
		}

		_, after, readbackErr := l.captureManagedRegistrationLocked(request.EntryName)
		observed.After = after
		if readbackErr != nil {
			observed.Failure = ManagedRegistrationFailureClassOf(readbackErr)
			observed.Settlement = ManagedRegistrationSettlementWriteUncertain
			return nil
		}
		if !managedRegistrationRestoredSnapshotMatches(after, request.ExpectedBaseline) {
			observed.Failure = ManagedRegistrationFailureReadbackMismatch
			observed.Settlement = ManagedRegistrationSettlementWriteUncertain
			return nil
		}
		observed.Failure = ManagedRegistrationFailureNone
		observed.Settlement = ManagedRegistrationSettlementVerified
		return nil
	})

	if execution.releaseErr != nil {
		observed.Phase = ManagedRegistrationPhaseRollback
		observed.Failure = ManagedRegistrationFailureLockReleaseUnconfirmed
		if observed.Invoked {
			observed.Settlement = ManagedRegistrationSettlementWriteUncertain
		}
	}
	if !execution.bodyEntered && observed.Failure == ManagedRegistrationFailureNone {
		observed.Failure = ManagedRegistrationFailureAdapterFailed
	}
	return observed
}

func (l *lockingClient) managedRegistrationRestoreRequestValid(request ManagedRegistrationConditionalBackupRestoreRequest) bool {
	if request.EntryName == "" || request.BackupIO == nil || request.BeforeWrite == nil || !request.ExpectedTargetGroupSHA256.valid {
		return false
	}
	parsedBackupID, err := ParseManagedRegistrationBackupID(request.BackupID.String())
	if err != nil || parsedBackupID != request.BackupID {
		return false
	}
	applied := request.ExpectedApplied
	baseline := request.ExpectedBaseline
	if applied.ClientID == "" || applied.ClientID != l.Client.Name() || baseline.ClientID != applied.ClientID ||
		!applied.ConfigSHA256.valid || !applied.EntrySHA256.valid || !applied.DesiredProjectionSHA256.valid ||
		!applied.ConfigPathSHA256.valid || !applied.EntryKeySHA256.valid || !applied.TargetIdentitySHA256.valid ||
		!baseline.ConfigSHA256.valid || !baseline.EntrySHA256.valid || !baseline.ConfigPathSHA256.valid || !baseline.EntryKeySHA256.valid ||
		applied.ConfigPathSHA256 != baseline.ConfigPathSHA256 || applied.EntryKeySHA256 != baseline.EntryKeySHA256 {
		return false
	}
	wantEntryKey := managedRegistrationDomainSHA256("entry-key/v1", applied.ClientID, request.EntryName)
	return wantEntryKey == applied.EntryKeySHA256
}

func managedRegistrationAppliedSnapshotMatches(current, expected ManagedRegistrationEntrySnapshot) bool {
	return current.ClientID == expected.ClientID &&
		current.Exists == expected.Exists &&
		current.EnabledState == expected.EnabledState &&
		current.Transport == expected.Transport &&
		current.IdentityState == expected.IdentityState &&
		current.ConfigSHA256 == expected.ConfigSHA256 &&
		current.EntrySHA256 == expected.EntrySHA256 &&
		current.ConfigPathSHA256 == expected.ConfigPathSHA256 &&
		current.EntryKeySHA256 == expected.EntryKeySHA256 &&
		current.TargetIdentitySHA256 == expected.TargetIdentitySHA256 &&
		current.DesiredProjectionSHA256 == expected.DesiredProjectionSHA256 &&
		expected.DesiredProjectionSHA256.valid
}

func managedRegistrationBaselineSnapshotMatches(actual, expected ManagedRegistrationEntrySnapshot) bool {
	return actual.ClientID == expected.ClientID &&
		actual.Exists == expected.Exists &&
		actual.EnabledState == expected.EnabledState &&
		actual.Transport == expected.Transport &&
		actual.IdentityState == expected.IdentityState &&
		actual.ConfigSHA256 == expected.ConfigSHA256 &&
		actual.EntrySHA256 == expected.EntrySHA256 &&
		actual.ConfigPathSHA256 == expected.ConfigPathSHA256 &&
		actual.EntryKeySHA256 == expected.EntryKeySHA256
}

func managedRegistrationRestoredSnapshotMatches(actual, baseline ManagedRegistrationEntrySnapshot) bool {
	return actual.ClientID == baseline.ClientID &&
		actual.Exists == baseline.Exists &&
		actual.EnabledState == baseline.EnabledState &&
		actual.Transport == baseline.Transport &&
		actual.IdentityState == baseline.IdentityState &&
		actual.EntrySHA256 == baseline.EntrySHA256 &&
		actual.ConfigPathSHA256 == baseline.ConfigPathSHA256 &&
		actual.EntryKeySHA256 == baseline.EntryKeySHA256
}

func (l *lockingClient) managedRegistrationSnapshotFromBytes(entryName string, configBytes []byte) (snapshot ManagedRegistrationEntrySnapshot, err error) {
	defer func() {
		if recover() != nil {
			snapshot = ManagedRegistrationEntrySnapshot{}
			err = managedRegistrationError(ManagedRegistrationFailureBackupReferenceInvalid)
		}
	}()
	extractor, ok := l.Client.(managedRegistrationEntryRawExtractor)
	if !ok {
		return ManagedRegistrationEntrySnapshot{}, managedRegistrationError(ManagedRegistrationFailureBackupReferenceInvalid)
	}
	rawEntry, exists, extractErr := extractor.EntryRawSubtree(configBytes, entryName)
	if extractErr != nil {
		return ManagedRegistrationEntrySnapshot{}, managedRegistrationError(ManagedRegistrationFailureBackupReferenceInvalid)
	}
	raw := managedRegistrationRawState{
		clientID: l.Client.Name(), configPath: l.Client.ConfigPath(), entryName: entryName,
		config: configBytes, rawEntry: rawEntry, exists: exists,
	}
	return managedRegistrationSafeSnapshot(raw)
}

func readManagedRegistrationBackup(ctx context.Context, backupIO ManagedRegistrationBackupIO, configPath string, id ManagedRegistrationBackupID, expectedSHA256 string) ([]byte, error) {
	if ctx == nil || backupIO == nil || expectedSHA256 == "" || ctx.Err() != nil {
		return nil, managedRegistrationError(ManagedRegistrationFailureBackupReferenceInvalid)
	}
	parentDir, leaf, ok := managedRegistrationBackupLocation(configPath, id)
	if !ok {
		return nil, managedRegistrationError(ManagedRegistrationFailureBackupReferenceInvalid)
	}
	contents, err := backupIO.Read(ctx, parentDir, leaf, expectedSHA256)
	if err != nil || managedRegistrationBytesSHA256(contents).String() != expectedSHA256 {
		clear(contents)
		return nil, managedRegistrationError(ManagedRegistrationFailureBackupReferenceInvalid)
	}
	return contents, nil
}

// createManagedRegistrationBackup allocates the bounded timestamped backup
// identity inside the existing config-lock transaction. The API-owned port is
// the only component that publishes or cleans up the final file.
func (l *lockingClient) createManagedRegistrationBackup(ctx context.Context, backupIO ManagedRegistrationBackupIO, configPath string, contents []byte, expectedSHA256 string) (ManagedRegistrationBackupID, error) {
	if ctx == nil || backupIO == nil || expectedSHA256 == "" || ctx.Err() != nil {
		return ManagedRegistrationBackupID{}, managedRegistrationError(ManagedRegistrationFailureBackup)
	}
	configAbs, err := filepath.Abs(configPath)
	if err != nil {
		return ManagedRegistrationBackupID{}, managedRegistrationError(ManagedRegistrationFailureBackup)
	}
	parentDir := filepath.Dir(filepath.Clean(configAbs))
	base := filepath.Base(configAbs)
	timestamp := time.Now().UTC().Format(backupTimestampLayout)
	for collision := 0; collision < 1024; collision++ {
		leaf := base + backupSuffixPrefix + timestamp
		if collision > 0 {
			leaf += fmt.Sprintf("-%09d", collision+1)
		}
		id, parseErr := ParseManagedRegistrationBackupID(leaf)
		if parseErr != nil {
			return ManagedRegistrationBackupID{}, managedRegistrationError(ManagedRegistrationFailureBackup)
		}
		receipt, createErr := backupIO.Create(ctx, parentDir, leaf, contents, expectedSHA256)
		if createErr == nil && receipt.Disposition == ManagedRegistrationBackupCommitted && receipt.CleanupDisposition == ManagedRegistrationBackupCleanupNotRequired && ctx.Err() == nil {
			return id, nil
		}
		if errors.Is(createErr, ErrManagedRegistrationBackupExists) && receipt.Disposition == ManagedRegistrationBackupNotCreated && receipt.CleanupDisposition == ManagedRegistrationBackupCleanupNotRequired {
			continue
		}
		return ManagedRegistrationBackupID{}, managedRegistrationError(ManagedRegistrationFailureBackup)
	}
	return ManagedRegistrationBackupID{}, managedRegistrationError(ManagedRegistrationFailureBackup)
}

func invokeManagedRegistrationBackupRestoreBeforeWrite(callback func(ManagedRegistrationBackupRestorePreparation) error, preparation ManagedRegistrationBackupRestorePreparation) (failed bool) {
	defer func() {
		if recover() != nil {
			failed = true
		}
	}()
	return callback(preparation) != nil
}

func (l *lockingClient) captureManagedRegistrationLocked(entryName string) (raw managedRegistrationRawState, snapshot ManagedRegistrationEntrySnapshot, err error) {
	defer func() {
		if recover() != nil {
			raw = managedRegistrationRawState{}
			snapshot = ManagedRegistrationEntrySnapshot{}
			err = managedRegistrationError(ManagedRegistrationFailureAdapterFailed)
		}
	}()
	configBytes, readErr := readRawConfig(l.Client.ConfigPath())
	if readErr != nil {
		return raw, snapshot, managedRegistrationError(ManagedRegistrationFailureAdapterFailed)
	}
	entry, entryErr := l.Client.GetEntry(entryName)
	if entryErr != nil {
		return raw, snapshot, managedRegistrationError(ManagedRegistrationFailureAdapterFailed)
	}
	confirmedConfigBytes, confirmErr := readRawConfig(l.Client.ConfigPath())
	if confirmErr != nil || !bytes.Equal(configBytes, confirmedConfigBytes) {
		return raw, snapshot, managedRegistrationError(ManagedRegistrationFailureSafeProjection)
	}

	raw = managedRegistrationRawState{
		clientID:   l.Client.Name(),
		configPath: l.Client.ConfigPath(),
		entryName:  entryName,
		config:     configBytes,
		entry:      entry,
		exists:     entry != nil,
	}
	if extractor, ok := l.Client.(managedRegistrationEntryRawExtractor); ok {
		parsedExists := entry != nil
		raw.rawEntry, raw.exists, entryErr = extractor.EntryRawSubtree(configBytes, entryName)
		if entryErr != nil {
			return managedRegistrationRawState{}, ManagedRegistrationEntrySnapshot{}, managedRegistrationError(ManagedRegistrationFailureSafeProjection)
		}
		if raw.exists != parsedExists {
			return managedRegistrationRawState{}, ManagedRegistrationEntrySnapshot{}, managedRegistrationError(ManagedRegistrationFailureIdentityUnavailable)
		}
		raw.rawExact = true
	} else if entry != nil {
		raw.rawEntry = entry
	}
	snapshot, err = managedRegistrationSafeSnapshot(raw)
	if err != nil {
		return managedRegistrationRawState{}, ManagedRegistrationEntrySnapshot{}, err
	}
	return raw, snapshot, nil
}

func managedRegistrationSafeSnapshot(raw managedRegistrationRawState) (ManagedRegistrationEntrySnapshot, error) {
	pathIdentity, err := filepath.Abs(raw.configPath)
	if err != nil {
		return ManagedRegistrationEntrySnapshot{}, managedRegistrationError(ManagedRegistrationFailureSafeProjection)
	}
	entrySHA, err := managedRegistrationValueSHA256(raw.rawEntry)
	if err != nil {
		return ManagedRegistrationEntrySnapshot{}, managedRegistrationError(ManagedRegistrationFailureSafeProjection)
	}
	transport, identityState, targetSHA := managedRegistrationClassifyRawIdentity(raw)
	desiredProjectionSHA := managedRegistrationLiveProjectionSHA256(raw, transport, identityState)
	return ManagedRegistrationEntrySnapshot{
		ClientID:                raw.clientID,
		Exists:                  raw.exists,
		EnabledState:            managedRegistrationEnabledState(raw.rawEntry, raw.exists),
		Transport:               transport,
		IdentityState:           identityState,
		ConfigSHA256:            managedRegistrationBytesSHA256(raw.config),
		EntrySHA256:             entrySHA,
		DesiredProjectionSHA256: desiredProjectionSHA,
		ConfigPathSHA256:        managedRegistrationDomainSHA256("config-path/v1", filepath.Clean(pathIdentity)),
		EntryKeySHA256:          managedRegistrationDomainSHA256("entry-key/v1", raw.clientID, raw.entryName),
		TargetIdentitySHA256:    targetSHA,
	}, nil
}

func managedRegistrationLiveProjectionSHA256(
	raw managedRegistrationRawState,
	transport ManagedRegistrationTransport,
	identityState ManagedRegistrationIdentityState,
) ManagedRegistrationFingerprint {
	enabled := managedRegistrationEnabledState(raw.rawEntry, raw.exists)
	if enabled != ManagedRegistrationEnabled && enabled != ManagedRegistrationDisabled {
		return ManagedRegistrationFingerprint{}
	}
	disabled := enabled == ManagedRegistrationDisabled
	switch {
	case transport == ManagedRegistrationTransportHTTP && identityState == ManagedRegistrationIdentityManagedHTTP:
		entry, ok := raw.rawEntry.(map[string]any)
		if !ok {
			return ManagedRegistrationFingerprint{}
		}
		rawURL, ok := entry["url"].(string)
		if !ok {
			return ManagedRegistrationFingerprint{}
		}
		identity, failure := managedRegistrationHTTPIdentity(rawURL)
		if failure != ManagedRegistrationFailureNone {
			return ManagedRegistrationFingerprint{}
		}
		return managedRegistrationDomainSHA256(
			"desired-http/v1",
			raw.clientID,
			identity.scheme,
			identity.host,
			identity.port,
			identity.escapedPath,
			strconv.FormatBool(disabled),
			strconv.Itoa(raw.entry.ToolTimeoutSec),
		)
	case transport == ManagedRegistrationTransportStdioRelay && identityState == ManagedRegistrationIdentityRelay:
		identity, failure := managedRegistrationRelayIdentityFromRaw(raw.rawEntry)
		if failure != ManagedRegistrationFailureNone {
			return ManagedRegistrationFingerprint{}
		}
		parts := []string{raw.clientID, identity.executable, strconv.FormatBool(disabled)}
		parts = append(parts, identity.args...)
		return managedRegistrationDomainSHA256("desired-relay/v1", parts...)
	default:
		return ManagedRegistrationFingerprint{}
	}
}

func (l *lockingClient) validateManagedRegistrationDesired(entryName string, desired MCPEntry) (managedRegistrationDesiredState, ManagedRegistrationFailureClass) {
	if desired.Name != entryName {
		return managedRegistrationDesiredState{}, ManagedRegistrationFailureDesiredEntryInvalid
	}
	if l.Client.IsRelayStdio() {
		relay, failure := managedRegistrationRelayIdentityFromDesired(desired)
		if failure != ManagedRegistrationFailureNone {
			return managedRegistrationDesiredState{}, ManagedRegistrationFailureDesiredEntryInvalid
		}
		projectionParts := []string{l.Client.Name(), relay.executable, strconv.FormatBool(desired.Disabled)}
		projectionParts = append(projectionParts, relay.args...)
		projection := managedRegistrationDomainSHA256("desired-relay/v1", projectionParts...)
		return managedRegistrationDesiredState{
			transport:            ManagedRegistrationTransportStdioRelay,
			relay:                relay,
			projectionSHA256:     projection,
			targetIdentitySHA256: managedRegistrationRelayIdentitySHA256(relay),
			disabled:             desired.Disabled,
		}, ManagedRegistrationFailureNone
	}
	if desired.ToolTimeoutSec < 0 || len(desired.Headers) != 0 || len(desired.Env) != 0 || desired.Raw != nil || desired.SourceBelowWriteTarget || desired.RelayURL != "" {
		return managedRegistrationDesiredState{}, ManagedRegistrationFailureDesiredEntryInvalid
	}
	httpIdentity, failure := managedRegistrationHTTPIdentity(desired.URL)
	if failure != ManagedRegistrationFailureNone {
		return managedRegistrationDesiredState{}, ManagedRegistrationFailureIdentityUnavailable
	}
	projection := managedRegistrationDomainSHA256(
		"desired-http/v1", l.Client.Name(), httpIdentity.scheme, httpIdentity.host, httpIdentity.port, httpIdentity.escapedPath, strconv.FormatBool(desired.Disabled), strconv.Itoa(desired.ToolTimeoutSec),
	)
	return managedRegistrationDesiredState{
		transport:            ManagedRegistrationTransportHTTP,
		http:                 httpIdentity,
		projectionSHA256:     projection,
		targetIdentitySHA256: managedRegistrationHTTPIdentitySHA256(httpIdentity),
		disabled:             desired.Disabled,
		toolTimeoutSec:       desired.ToolTimeoutSec,
	}, ManagedRegistrationFailureNone
}

func managedRegistrationDesiredMatchesReadback(desired managedRegistrationDesiredState, raw managedRegistrationRawState) (bool, ManagedRegistrationFailureClass) {
	if managedRegistrationEnabledState(raw.rawEntry, raw.exists) == ManagedRegistrationEnabledAmbiguous || !raw.exists {
		return false, ManagedRegistrationFailureReadbackMismatch
	}
	wantEnabled := ManagedRegistrationEnabled
	if desired.disabled {
		wantEnabled = ManagedRegistrationDisabled
	}
	if managedRegistrationEnabledState(raw.rawEntry, raw.exists) != wantEnabled {
		return false, ManagedRegistrationFailureReadbackMismatch
	}
	switch desired.transport {
	case ManagedRegistrationTransportHTTP:
		m, ok := raw.rawEntry.(map[string]any)
		if !ok || m["command"] != nil || m["args"] != nil {
			return false, ManagedRegistrationFailureReadbackMismatch
		}
		rawURL, ok := m["url"].(string)
		if !ok {
			return false, ManagedRegistrationFailureIdentityUnavailable
		}
		readback, failure := managedRegistrationHTTPIdentity(rawURL)
		if failure != ManagedRegistrationFailureNone {
			return false, ManagedRegistrationFailureIdentityUnavailable
		}
		if raw.entry == nil || raw.entry.ToolTimeoutSec != desired.toolTimeoutSec {
			return false, ManagedRegistrationFailureReadbackMismatch
		}
		return managedRegistrationHTTPIdentityEqual(desired.http, readback), ManagedRegistrationFailureReadbackMismatch
	case ManagedRegistrationTransportStdioRelay:
		readback, failure := managedRegistrationRelayIdentityFromRaw(raw.rawEntry)
		if failure != ManagedRegistrationFailureNone {
			return false, ManagedRegistrationFailureIdentityUnavailable
		}
		return managedRegistrationRelayIdentityEqual(desired.relay, readback), ManagedRegistrationFailureReadbackMismatch
	default:
		return false, ManagedRegistrationFailureReadbackMismatch
	}
}

func managedRegistrationEnabledState(raw any, exists bool) ManagedRegistrationEnabledState {
	if !exists {
		return ManagedRegistrationEnabledAbsent
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return ManagedRegistrationEnabledAmbiguous
	}
	disabledRaw, hasDisabled := m["disabled"]
	enabledRaw, hasEnabled := m["enabled"]
	if !hasDisabled && !hasEnabled {
		return ManagedRegistrationEnabled
	}
	states := make([]ManagedRegistrationEnabledState, 0, 2)
	if hasDisabled {
		disabled, ok := disabledRaw.(bool)
		if !ok {
			return ManagedRegistrationEnabledAmbiguous
		}
		if disabled {
			states = append(states, ManagedRegistrationDisabled)
		} else {
			states = append(states, ManagedRegistrationEnabled)
		}
	}
	if hasEnabled {
		enabled, ok := enabledRaw.(bool)
		if !ok {
			return ManagedRegistrationEnabledAmbiguous
		}
		if enabled {
			states = append(states, ManagedRegistrationEnabled)
		} else {
			states = append(states, ManagedRegistrationDisabled)
		}
	}
	if len(states) == 2 && states[0] != states[1] {
		return ManagedRegistrationEnabledAmbiguous
	}
	return states[0]
}

type managedHTTPIdentity struct {
	scheme      string
	host        string
	port        string
	escapedPath string
}

func managedRegistrationHTTPIdentity(raw string) (managedHTTPIdentity, ManagedRegistrationFailureClass) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Scheme != "http" {
		return managedHTTPIdentity{}, ManagedRegistrationFailureIdentityUnavailable
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if !managedRegistrationLoopbackHost(host) {
		return managedHTTPIdentity{}, ManagedRegistrationFailureIdentityUnavailable
	}
	port := u.Port()
	portNumber, portErr := strconv.Atoi(port)
	if portErr != nil || portNumber < 1 || portNumber > 65535 {
		return managedHTTPIdentity{}, ManagedRegistrationFailureIdentityUnavailable
	}
	escapedPath := u.EscapedPath()
	if escapedPath == "" {
		escapedPath = "/"
	}
	if u.RawPath != "" {
		unescaped, unescapeErr := url.PathUnescape(u.RawPath)
		if unescapeErr != nil || unescaped != u.Path || escapedPath != u.RawPath {
			return managedHTTPIdentity{}, ManagedRegistrationFailureIdentityUnavailable
		}
	}
	return managedHTTPIdentity{scheme: "http", host: "loopback", port: port, escapedPath: escapedPath}, ManagedRegistrationFailureNone
}

func managedRegistrationLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func managedRegistrationHTTPIdentityEqual(left, right managedHTTPIdentity) bool {
	return left == right
}

func managedRegistrationHTTPIdentitySHA256(identity managedHTTPIdentity) ManagedRegistrationFingerprint {
	return managedRegistrationDomainSHA256("target-http/v1", identity.scheme, identity.host, identity.port, identity.escapedPath)
}

type managedRelayIdentity struct {
	executable string
	args       []string
}

func managedRegistrationRelayIdentityFromDesired(entry MCPEntry) (managedRelayIdentity, ManagedRegistrationFailureClass) {
	if entry.URL != "" || len(entry.Headers) != 0 || len(entry.Env) != 0 || entry.Raw != nil || entry.SourceBelowWriteTarget {
		return managedRelayIdentity{}, ManagedRegistrationFailureDesiredEntryInvalid
	}
	if !filepath.IsAbs(entry.RelayExePath) || filepath.Clean(entry.RelayExePath) != entry.RelayExePath {
		return managedRelayIdentity{}, ManagedRegistrationFailureDesiredEntryInvalid
	}
	var args []string
	if entry.RelayURL != "" {
		if entry.RelayServer != "" || entry.RelayDaemon != "" {
			return managedRelayIdentity{}, ManagedRegistrationFailureDesiredEntryInvalid
		}
		if _, failure := managedRegistrationHTTPIdentity(entry.RelayURL); failure != ManagedRegistrationFailureNone {
			return managedRelayIdentity{}, ManagedRegistrationFailureDesiredEntryInvalid
		}
		args = []string{"relay", "--url", entry.RelayURL}
	} else {
		if entry.RelayServer == "" || entry.RelayDaemon == "" {
			return managedRelayIdentity{}, ManagedRegistrationFailureDesiredEntryInvalid
		}
		args = []string{"relay", "--server", entry.RelayServer, "--daemon", entry.RelayDaemon}
	}
	return managedRelayIdentity{executable: entry.RelayExePath, args: args}, ManagedRegistrationFailureNone
}

func managedRegistrationRelayIdentityFromRaw(raw any) (managedRelayIdentity, ManagedRegistrationFailureClass) {
	m, ok := raw.(map[string]any)
	if !ok {
		return managedRelayIdentity{}, ManagedRegistrationFailureIdentityUnavailable
	}
	executable, ok := m["command"].(string)
	if !ok || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return managedRelayIdentity{}, ManagedRegistrationFailureIdentityUnavailable
	}
	rawArgs, ok := m["args"].([]any)
	if !ok {
		return managedRelayIdentity{}, ManagedRegistrationFailureIdentityUnavailable
	}
	args := make([]string, len(rawArgs))
	for i, value := range rawArgs {
		arg, ok := value.(string)
		if !ok {
			return managedRelayIdentity{}, ManagedRegistrationFailureIdentityUnavailable
		}
		args[i] = arg
	}
	return managedRelayIdentity{executable: executable, args: args}, ManagedRegistrationFailureNone
}

func managedRegistrationRelayIdentityEqual(left, right managedRelayIdentity) bool {
	return left.executable == right.executable && slices.Equal(left.args, right.args)
}

func managedRegistrationRelayIdentitySHA256(identity managedRelayIdentity) ManagedRegistrationFingerprint {
	parts := append([]string{identity.executable}, identity.args...)
	return managedRegistrationDomainSHA256("target-relay/v1", parts...)
}

func managedRegistrationClassifyRawIdentity(raw managedRegistrationRawState) (ManagedRegistrationTransport, ManagedRegistrationIdentityState, ManagedRegistrationFingerprint) {
	if !raw.exists {
		return ManagedRegistrationTransportAbsent, ManagedRegistrationIdentityAbsent, ManagedRegistrationFingerprint{}
	}
	m, ok := raw.rawEntry.(map[string]any)
	if !ok {
		return ManagedRegistrationTransportUnknown, ManagedRegistrationIdentityUnavailable, ManagedRegistrationFingerprint{}
	}
	if rawURL, ok := m["url"].(string); ok && rawURL != "" {
		parsed, parseErr := url.Parse(rawURL)
		if parseErr != nil {
			return ManagedRegistrationTransportHTTP, ManagedRegistrationIdentityUnavailable, ManagedRegistrationFingerprint{}
		}
		if parsed.Opaque != "" {
			return ManagedRegistrationTransportExternal, ManagedRegistrationIdentityOpaque, ManagedRegistrationFingerprint{}
		}
		if identity, failure := managedRegistrationHTTPIdentity(rawURL); failure == ManagedRegistrationFailureNone {
			return ManagedRegistrationTransportHTTP, ManagedRegistrationIdentityManagedHTTP, managedRegistrationHTTPIdentitySHA256(identity)
		}
		if parsed.Scheme == "http" || parsed.Scheme == "https" {
			return ManagedRegistrationTransportExternal, ManagedRegistrationIdentityExternal, ManagedRegistrationFingerprint{}
		}
		return ManagedRegistrationTransportExternal, ManagedRegistrationIdentityUnavailable, ManagedRegistrationFingerprint{}
	}
	if _, hasCommand := m["command"]; hasCommand {
		if rawArgs, ok := m["args"].([]any); ok && len(rawArgs) >= 3 {
			arg0, _ := rawArgs[0].(string)
			arg1, _ := rawArgs[1].(string)
			rawURL, _ := rawArgs[2].(string)
			if arg0 == "relay" && arg1 == "--url" {
				parsed, parseErr := url.Parse(rawURL)
				if parseErr != nil {
					return ManagedRegistrationTransportStdioRelay, ManagedRegistrationIdentityUnavailable, ManagedRegistrationFingerprint{}
				}
				if parsed.Opaque != "" {
					return ManagedRegistrationTransportStdioRelay, ManagedRegistrationIdentityOpaque, ManagedRegistrationFingerprint{}
				}
			}
		}
		if identity, failure := managedRegistrationRelayIdentityFromRaw(raw.rawEntry); failure == ManagedRegistrationFailureNone && len(identity.args) > 0 && identity.args[0] == "relay" {
			if len(identity.args) >= 3 && identity.args[1] == "--url" {
				parsed, parseErr := url.Parse(identity.args[2])
				if parseErr != nil {
					return ManagedRegistrationTransportStdioRelay, ManagedRegistrationIdentityUnavailable, ManagedRegistrationFingerprint{}
				}
				if parsed.Opaque != "" {
					return ManagedRegistrationTransportStdioRelay, ManagedRegistrationIdentityOpaque, ManagedRegistrationFingerprint{}
				}
			}
			return ManagedRegistrationTransportStdioRelay, ManagedRegistrationIdentityRelay, managedRegistrationRelayIdentitySHA256(identity)
		}
		return ManagedRegistrationTransportStdioDirect, ManagedRegistrationIdentityExternal, ManagedRegistrationFingerprint{}
	}
	return ManagedRegistrationTransportUnknown, ManagedRegistrationIdentityUnavailable, ManagedRegistrationFingerprint{}
}

func managedRegistrationBackupID(configPath, backupPath string) (ManagedRegistrationBackupID, bool) {
	configAbs, configErr := filepath.Abs(configPath)
	backupAbs, backupErr := filepath.Abs(backupPath)
	if configErr != nil || backupErr != nil || filepath.Dir(filepath.Clean(configAbs)) != filepath.Dir(filepath.Clean(backupAbs)) {
		return ManagedRegistrationBackupID{}, false
	}
	id, parseErr := ParseManagedRegistrationBackupID(filepath.Base(backupAbs))
	if parseErr != nil {
		return ManagedRegistrationBackupID{}, false
	}
	resolved, resolveErr := resolveManagedRegistrationBackupID(configAbs, id)
	if resolveErr != nil || filepath.Clean(resolved) != filepath.Clean(backupAbs) {
		return ManagedRegistrationBackupID{}, false
	}
	return id, true
}

// managedRegistrationBackupLocation derives the only backup directory and
// leaf accepted for an opaque backup identity. It performs no filesystem
// lookup: the request-scoped BackupIO owns no-follow object validation.
func managedRegistrationBackupLocation(configPath string, id ManagedRegistrationBackupID) (parentDir string, leaf string, ok bool) {
	parsed, err := ParseManagedRegistrationBackupID(id.String())
	if err != nil || parsed != id {
		return "", "", false
	}
	configAbs, err := filepath.Abs(configPath)
	if err != nil {
		return "", "", false
	}
	configAbs = filepath.Clean(configAbs)
	leaf = id.String()
	if !strings.HasPrefix(leaf, filepath.Base(configAbs)+backupSuffixPrefix) {
		return "", "", false
	}
	return filepath.Dir(configAbs), leaf, true
}

// resolveManagedRegistrationBackupID is deliberately unexported and is the
// client-owned filesystem boundary. External callers can carry the opaque ID
// but cannot turn it into a path.
func resolveManagedRegistrationBackupID(configPath string, id ManagedRegistrationBackupID) (string, error) {
	parsed, err := ParseManagedRegistrationBackupID(id.String())
	if err != nil || parsed != id {
		return "", managedRegistrationError(ManagedRegistrationFailureBackupIDInvalid)
	}
	configAbs, err := filepath.Abs(configPath)
	if err != nil {
		return "", managedRegistrationError(ManagedRegistrationFailureBackupIDInvalid)
	}
	configAbs = filepath.Clean(configAbs)
	if !strings.HasPrefix(id.String(), filepath.Base(configAbs)+backupSuffixPrefix) {
		return "", managedRegistrationError(ManagedRegistrationFailureBackupIDInvalid)
	}
	parent := filepath.Dir(configAbs)
	candidate := filepath.Join(parent, id.String())
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil || filepath.Dir(filepath.Clean(candidateAbs)) != parent || filepath.Base(candidateAbs) != id.String() {
		return "", managedRegistrationError(ManagedRegistrationFailureBackupIDInvalid)
	}
	info, err := os.Lstat(candidateAbs)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", managedRegistrationError(ManagedRegistrationFailureBackupIDInvalid)
	}
	return filepath.Clean(candidateAbs), nil
}

func managedRegistrationFingerprintTextValid(fingerprint string) bool {
	if len(fingerprint) != sha256.Size*2 {
		return false
	}
	for _, char := range fingerprint {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func managedRegistrationBytesSHA256(value []byte) ManagedRegistrationFingerprint {
	sum := sha256.Sum256(value)
	return ManagedRegistrationFingerprint{digest: sum, valid: true}
}

func managedRegistrationValueSHA256(value any) (ManagedRegistrationFingerprint, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return ManagedRegistrationFingerprint{}, err
	}
	return managedRegistrationBytesSHA256(raw), nil
}

func managedRegistrationDomainSHA256(domain string, parts ...string) ManagedRegistrationFingerprint {
	hash := sha256.New()
	writePart := func(value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(value))
	}
	writePart("mcp-local-hub/managed-registration/" + domain)
	for _, part := range parts {
		writePart(part)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return ManagedRegistrationFingerprint{digest: digest, valid: true}
}

func invokeManagedRegistrationBackup(callback func() (string, error)) (path string, failed bool) {
	defer func() {
		if recover() != nil {
			path = ""
			failed = true
		}
	}()
	path, err := callback()
	return path, err != nil
}

func invokeManagedRegistrationBeforeWrite(callback func(ManagedRegistrationMutationPreparation) error, preparation ManagedRegistrationMutationPreparation) (failed bool) {
	defer func() {
		if recover() != nil {
			failed = true
		}
	}()
	return callback(preparation) != nil
}

func invokeManagedRegistrationMutation(callback func() error) (failed bool) {
	defer func() {
		if recover() != nil {
			failed = true
		}
	}()
	return callback() != nil
}

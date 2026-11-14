package binaryadmission

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	WindowsCanonicalEntrySetSchemaV1         = "windows-canonical-entry-set-v1"
	WindowsCanonicalEntryRelativePathV1      = "mcphub.exe"
	WindowsCanonicalProductRelativePathV1    = "mcphub-product/mcphub.exe"
	WindowsCanonicalIdentityRelativePathV1   = "windows-product-identity.json"
	WindowsCanonicalEntryReverseProjectionV1 = "../mcphub.exe"
)

// BuildBinding is immutable input from the executing Product's composition root.
// It is public correlation data, not a capability or an assertion of admission.
type WindowsCanonicalEntryBuildBindingV1 struct {
	buildID                      string
	expectedCanonicalEntrySHA256 string
	reverseProjection            string
}

func NewWindowsCanonicalEntryBuildBindingV1(buildID, expectedCanonicalEntrySHA256, reverseProjection string) (WindowsCanonicalEntryBuildBindingV1, error) {
	if !validLowerHex(buildID, 32, 32) || !validLowerHex(expectedCanonicalEntrySHA256, 64, 64) || reverseProjection != WindowsCanonicalEntryReverseProjectionV1 {
		return WindowsCanonicalEntryBuildBindingV1{}, errors.New("canonical entry build binding is missing or invalid")
	}
	return WindowsCanonicalEntryBuildBindingV1{buildID, expectedCanonicalEntrySHA256, reverseProjection}, nil
}

func (b WindowsCanonicalEntryBuildBindingV1) BuildID() string { return b.buildID }
func (b WindowsCanonicalEntryBuildBindingV1) ExpectedCanonicalEntrySHA256() string {
	return b.expectedCanonicalEntrySHA256
}
func (b WindowsCanonicalEntryBuildBindingV1) ReverseProjection() string { return b.reverseProjection }

const canonicalEntryCommentsNamespace = "MCPHUB-CANONICAL-ENTRY-"
const canonicalEntryCommentsMarkerV1 = canonicalEntryCommentsNamespace + "V1="

var ErrWindowsCanonicalEntryBindingMissing = errors.New("legacy Product has no canonical entry build binding")

type windowsCanonicalEntryCommentsRecordV1 struct {
	BuildID           string `json:"build_id"`
	LauncherSHA256    string `json:"launcher_sha256"`
	ReverseProjection string `json:"reverse_projection"`
}

func EncodeWindowsCanonicalEntryCommentsV1(existing string, binding WindowsCanonicalEntryBuildBindingV1) (string, error) {
	if strings.Contains(existing, canonicalEntryCommentsNamespace) {
		return "", errors.New("existing Comments contains reserved canonical entry marker")
	}
	if _, err := NewWindowsCanonicalEntryBuildBindingV1(binding.BuildID(), binding.ExpectedCanonicalEntrySHA256(), binding.ReverseProjection()); err != nil {
		return "", err
	}
	raw, err := json.Marshal(windowsCanonicalEntryCommentsRecordV1{binding.BuildID(), binding.ExpectedCanonicalEntrySHA256(), binding.ReverseProjection()})
	if err != nil {
		return "", err
	}
	return existing + "\n" + canonicalEntryCommentsMarkerV1 + string(raw), nil
}

// The final record has canonical compact JSON, exact keys/order, and no suffix.
// Removing just the inserted LF preserves all original Comments bytes.
func ParseWindowsCanonicalEntryCommentsV1(comments string) (string, WindowsCanonicalEntryBuildBindingV1, error) {
	count := strings.Count(comments, canonicalEntryCommentsMarkerV1)
	if count == 0 && !strings.Contains(comments, canonicalEntryCommentsNamespace) {
		return "", WindowsCanonicalEntryBuildBindingV1{}, ErrWindowsCanonicalEntryBindingMissing
	}
	if count != 1 || strings.Count(comments, canonicalEntryCommentsNamespace) != 1 {
		return "", WindowsCanonicalEntryBuildBindingV1{}, errors.New("unknown or duplicate canonical entry Comments marker")
	}
	i := strings.Index(comments, canonicalEntryCommentsMarkerV1)
	if i == 0 || comments[i-1] != '\n' {
		return "", WindowsCanonicalEntryBuildBindingV1{}, errors.New("canonical entry Comments marker must start final inserted line")
	}
	raw := []byte(comments[i+len(canonicalEntryCommentsMarkerV1):])
	var record windowsCanonicalEntryCommentsRecordV1
	if err := decodeCanonicalEntryJSON(raw, &record); err != nil {
		return "", WindowsCanonicalEntryBuildBindingV1{}, err
	}
	canonical, _ := json.Marshal(record)
	if !bytes.Equal(raw, canonical) {
		return "", WindowsCanonicalEntryBuildBindingV1{}, errors.New("noncanonical canonical entry Comments record")
	}
	binding, err := NewWindowsCanonicalEntryBuildBindingV1(record.BuildID, record.LauncherSHA256, record.ReverseProjection)
	if err != nil {
		return "", WindowsCanonicalEntryBuildBindingV1{}, err
	}
	return comments[:i-1], binding, nil
}

type WindowsCanonicalEntryFileV1 struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Snapshot is detached, serializable evidence. A decoded snapshot never grants
// native admission: readers must freshly acquire and validate the original set.
type WindowsCanonicalEntrySetSnapshotV1 struct {
	Schema                     string                      `json:"schema"`
	BuildID                    string                      `json:"build_id"`
	RegistryProjectionSHA256   string                      `json:"registry_projection_sha256"`
	CanonicalEntryRelativePath string                      `json:"canonical_entry_relative_path"`
	ProductRelativePath        string                      `json:"product_relative_path"`
	IdentityRelativePath       string                      `json:"identity_relative_path"`
	ReverseProjection          string                      `json:"reverse_projection"`
	CanonicalEntry             WindowsCanonicalEntryFileV1 `json:"canonical_entry"`
	Product                    WindowsCanonicalEntryFileV1 `json:"product"`
	Identity                   WindowsCanonicalEntryFileV1 `json:"identity"`
	Machine                    uint16                      `json:"machine"`
	ProductArtifact            WindowsArtifact             `json:"product_artifact"`
	IdentityBytes              []byte                      `json:"identity_bytes"`
}

// The legacy WindowsArtifact wire shape omits native fields. The complete-set
// journal explicitly retains them without changing that legacy serializer.
type canonicalEntryProductArtifactWireV1 struct {
	Path      string              `json:"path"`
	Role      WindowsArtifactRole `json:"role"`
	Version   string              `json:"version"`
	Commit    string              `json:"commit"`
	BuildDate string              `json:"build_date"`
	SHA256    string              `json:"sha256"`
	Machine   uint16              `json:"machine"`
	Subsystem uint16              `json:"subsystem"`
}
type canonicalEntrySnapshotAliasV1 WindowsCanonicalEntrySetSnapshotV1
type canonicalEntrySnapshotWireV1 struct {
	canonicalEntrySnapshotAliasV1
	ProductArtifact canonicalEntryProductArtifactWireV1 `json:"product_artifact"`
}

func (v WindowsCanonicalEntrySetSnapshotV1) MarshalJSON() ([]byte, error) {
	p := v.ProductArtifact
	return json.Marshal(canonicalEntrySnapshotWireV1{canonicalEntrySnapshotAliasV1(v), canonicalEntryProductArtifactWireV1{p.Path, p.Role, p.Version, p.Commit, p.BuildDate, p.SHA256, p.Machine, p.Subsystem}})
}
func (v *WindowsCanonicalEntrySetSnapshotV1) UnmarshalJSON(raw []byte) error {
	var wire canonicalEntrySnapshotWireV1
	if err := decodeCanonicalEntryJSON(raw, &wire); err != nil {
		return err
	}
	if err := canonicalEntryExactJSONKeys(raw, "schema", "build_id", "registry_projection_sha256", "canonical_entry_relative_path", "product_relative_path", "identity_relative_path", "reverse_projection", "canonical_entry", "product", "identity", "machine", "product_artifact", "identity_bytes"); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, name := range []string{"canonical_entry", "product", "identity"} {
		if err := canonicalEntryExactJSONKeys(fields[name], "path", "sha256"); err != nil {
			return err
		}
	}
	if err := canonicalEntryExactJSONKeys(fields["product_artifact"], "path", "role", "version", "commit", "build_date", "sha256", "machine", "subsystem"); err != nil {
		return err
	}
	result := WindowsCanonicalEntrySetSnapshotV1(wire.canonicalEntrySnapshotAliasV1)
	p := wire.ProductArtifact
	result.ProductArtifact = WindowsArtifact{Path: p.Path, Role: p.Role, Version: p.Version, Commit: p.Commit, BuildDate: p.BuildDate, SHA256: p.SHA256, Machine: p.Machine, Subsystem: p.Subsystem}
	*v = result
	return nil
}

type WindowsCanonicalEntrySetTargetsV1 struct {
	CanonicalEntryPath string `json:"canonical_entry_path"`
	ProductPath        string `json:"product_path"`
	IdentityPath       string `json:"identity_path"`
}

// MoveResult is diagnostic DATA, never a serialized native capability. A nil
// native return records status zero; precondition failures have no status.
type WindowsCanonicalEntryMoveResultV1 struct {
	Member               string
	SourceRelative       string
	DestinationRelative  string
	Attempted            bool
	NativeStatusKnown    bool
	NativeStatus         uint32
	Applied              bool
	Readback             bool
	Error                error
	CloseError           error
	OriginalOwnerPending bool
}

type canonicalEntryNativeIdentity struct{ volume, high, low uint32 }
type canonicalEntryNativeFile struct {
	file          *os.File
	readOnlyGuard *os.File
	identity      canonicalEntryNativeIdentity
	path          string
}

// Owner types are host-neutral so legacy transaction declarations remain
// source-compatible. Only the Windows implementation can acquire them.
type WindowsCanonicalEntrySetV1 struct {
	mu                       sync.Mutex
	files                    []*os.File
	entry, product, identity *canonicalEntryNativeFile
	snapshot                 WindowsCanonicalEntrySetSnapshotV1
	ready                    bool
	closed                   bool
	closeError               error
	payloadDirectoryLink     *canonicalEntryPayloadDirectoryLinkV1
	payloadDirectoryBorrowed bool
}

type StagedWindowsCanonicalEntrySetV1 struct {
	mu             sync.Mutex
	root           *WindowsCanonicalEntrySetV1
	parent         *canonicalEntryNativeFile
	members        [3]*canonicalEntryNativeFile
	cleanupSHA256  [3]string
	discardResults [3]*WindowsCanonicalEntryMoveResultV1
	destinations   WindowsCanonicalEntrySetTargetsV1
	snapshot       WindowsCanonicalEntrySetSnapshotV1
	ready          bool
	taken          bool
	closed         bool
	closeError     error
}

// PriorSet retains original destination preimages, not assertions supplied by
// the caller. It never deletes files merely because Close was called.
type WindowsCanonicalEntryPriorSetV1 struct {
	mu                sync.Mutex
	root              *WindowsCanonicalEntrySetV1
	parent            *canonicalEntryNativeFile
	payloadParent     *canonicalEntryNativeFile
	members           [3]*canonicalEntryNativeFile
	present           [3]bool
	missingComponents [3]string
	sha256            [3]string
	destinations      WindowsCanonicalEntrySetTargetsV1
	discardResults    [3]*WindowsCanonicalEntryMoveResultV1
	ready             bool
	closed            bool
	closeError        error
}

func CanonicalEntryTargetsV1(entryPath string) (WindowsCanonicalEntrySetTargetsV1, error) {
	if !filepath.IsAbs(entryPath) || filepath.Clean(entryPath) != entryPath || filepath.Base(entryPath) != WindowsCanonicalEntryRelativePathV1 {
		return WindowsCanonicalEntrySetTargetsV1{}, errors.New("canonical entry target must be an exact absolute mcphub.exe path")
	}
	root := filepath.Dir(entryPath)
	return WindowsCanonicalEntrySetTargetsV1{entryPath, filepath.Join(root, filepath.FromSlash(WindowsCanonicalProductRelativePathV1)), filepath.Join(root, WindowsCanonicalIdentityRelativePathV1)}, nil
}

// The V1 codec deliberately has exactly nine fields; extensions require another
// admitted schema. The exact original JSON bytes are retained separately.
type WindowsProductIdentityV1 struct {
	Schema    string              `json:"schema"`
	Role      WindowsArtifactRole `json:"role"`
	Filename  string              `json:"filename"`
	Subsystem uint16              `json:"subsystem"`
	Machine   uint16              `json:"machine"`
	Version   string              `json:"version"`
	Commit    string              `json:"commit"`
	BuildDate string              `json:"build_date"`
	SHA256    string              `json:"sha256"`
}

// Nine metadata fields need only a few hundred bytes. Keep formatting headroom
// while bounding the original bytes retained and copied on every admission.
const windowsProductIdentityMaxBytes = 64 << 10

func validateWindowsProductIdentitySize(size int64) error {
	if size < 0 || size > windowsProductIdentityMaxBytes {
		return fmt.Errorf("Windows Product identity-v1 size outside 0..%d bytes", windowsProductIdentityMaxBytes)
	}
	return nil
}

func decodeCanonicalEntryJSON(raw []byte, v any) error {
	if err := uniqueSourceJSON(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("canonical entry JSON trailing data")
	}
	return nil
}

func canonicalEntryExactJSONKeys(raw []byte, keys ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if len(fields) != len(keys) {
		return errors.New("canonical entry JSON missing or unknown field")
	}
	for _, key := range keys {
		value, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("canonical entry JSON missing or null exact key %q", key)
		}
	}
	return nil
}

func ParseWindowsProductIdentity(raw []byte) (v WindowsProductIdentityV1, err error) {
	if err = validateWindowsProductIdentitySize(int64(len(raw))); err != nil {
		return v, err
	}
	if err = decodeCanonicalEntryJSON(raw, &v); err != nil {
		return v, err
	}
	if err = canonicalEntryExactJSONKeys(raw, "schema", "role", "filename", "subsystem", "machine", "version", "commit", "build_date", "sha256"); err != nil {
		return v, err
	}
	if v.Schema != "windows-product-identity-v1" || v.Role != WindowsArtifactRoleProduct || v.Filename != WindowsCanonicalEntryRelativePathV1 || v.Subsystem != WindowsGUISubsystem || (v.Machine != 0x8664 && v.Machine != 0xaa64) || !validProductSemVer(v.Version) || !validLowerHex(v.Commit, 40, 40) || !validUTCSeconds(v.BuildDate) || !validLowerHex(v.SHA256, 64, 64) {
		return v, errors.New("invalid Windows Product identity-v1")
	}
	return v, nil
}

type WindowsProductBuildArtifact struct {
	Role            string `json:"role"`
	Filename        string `json:"filename"`
	Source          string `json:"source"`
	Subsystem       uint16 `json:"subsystem"`
	ProductMetadata bool   `json:"productMetadata"`
	NPMPayload      bool   `json:"npmPayload"`
	NPMBin          bool   `json:"npmBin"`
}

type WindowsProductRegistry struct {
	SchemaVersion    int `json:"schemaVersion"`
	IdentityManifest struct {
		Schema   string `json:"schema"`
		Filename string `json:"filename"`
	} `json:"identityManifest"`
	Artifacts []WindowsProductBuildArtifact `json:"artifacts"`
}

// canonicalEntryRegistryV1 is the single protocol projection shared by runtime
// and build readers. Input must match it; formatting is not identity.
func canonicalEntryRegistryV1() WindowsProductRegistry {
	v := WindowsProductRegistry{SchemaVersion: 3}
	v.IdentityManifest.Schema = "windows-product-identity-v1"
	v.IdentityManifest.Filename = WindowsCanonicalIdentityRelativePathV1
	v.Artifacts = []WindowsProductBuildArtifact{
		{"product", WindowsCanonicalProductRelativePathV1, "./cmd/mcphub", 2, true, true, false},
		{"launcher", WindowsCanonicalEntryRelativePathV1, "./cmd/mcphub-launcher", 3, false, true, false},
	}
	return v
}

func ParseWindowsProductRegistry(raw []byte) (v WindowsProductRegistry, err error) {
	if err = decodeCanonicalEntryJSON(raw, &v); err != nil {
		return v, err
	}
	if err = canonicalEntryExactJSONKeys(raw, "schemaVersion", "identityManifest", "artifacts"); err != nil {
		return v, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return v, err
	}
	if err = canonicalEntryExactJSONKeys(fields["identityManifest"], "schema", "filename"); err != nil {
		return v, err
	}
	var artifacts []json.RawMessage
	if err = json.Unmarshal(fields["artifacts"], &artifacts); err != nil {
		return v, err
	}
	for _, artifact := range artifacts {
		if err = canonicalEntryExactJSONKeys(artifact, "role", "filename", "source", "subsystem", "productMetadata", "npmPayload", "npmBin"); err != nil {
			return v, err
		}
	}
	want, _ := json.Marshal(canonicalEntryRegistryV1())
	actual, _ := json.Marshal(v)
	if !bytes.Equal(actual, want) {
		return v, errors.New("invalid Windows Product registry schema3 projection")
	}
	return v, nil
}

func WindowsCanonicalEntryRegistryProjectionSHA256V1() string {
	raw, _ := json.Marshal(canonicalEntryRegistryV1())
	return canonicalEntrySHA256(raw)
}

func canonicalEntrySHA256(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func validateCanonicalEntrySnapshotV1(v WindowsCanonicalEntrySetSnapshotV1) error {
	if v.Schema != WindowsCanonicalEntrySetSchemaV1 || !validLowerHex(v.BuildID, 32, 32) || v.RegistryProjectionSHA256 != WindowsCanonicalEntryRegistryProjectionSHA256V1() || v.CanonicalEntryRelativePath != WindowsCanonicalEntryRelativePathV1 || v.ProductRelativePath != WindowsCanonicalProductRelativePathV1 || v.IdentityRelativePath != WindowsCanonicalIdentityRelativePathV1 || v.ReverseProjection != WindowsCanonicalEntryReverseProjectionV1 {
		return errors.New("invalid canonical entry snapshot projection")
	}
	for _, f := range []WindowsCanonicalEntryFileV1{v.CanonicalEntry, v.Product, v.Identity} {
		if f.Path == "" || !validLowerHex(f.SHA256, 64, 64) {
			return errors.New("incomplete canonical entry snapshot file")
		}
	}
	i, err := ParseWindowsProductIdentity(v.IdentityBytes)
	if err != nil {
		return err
	}
	p := v.ProductArtifact
	if canonicalEntrySHA256(v.IdentityBytes) != v.Identity.SHA256 || i.SHA256 != v.Product.SHA256 || p.SHA256 != i.SHA256 || p.Role != i.Role || p.Version != i.Version || p.Commit != i.Commit || p.BuildDate != i.BuildDate || p.Machine != i.Machine || p.Subsystem != i.Subsystem || v.Machine != i.Machine {
		return fmt.Errorf("canonical entry snapshot Product/identity disagreement")
	}
	return nil
}

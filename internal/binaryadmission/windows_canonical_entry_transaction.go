package binaryadmission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path"
	"path/filepath"
	"slices"
	"sync"
)

const WindowsCanonicalEntrySetSchemaV2 = "windows-canonical-entry-set-v2"

// This is detached evidence. Only fresh native acquisition issues an owner.
type WindowsCanonicalEntrySetSnapshotV2 struct {
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
	BundleRelativePath         string                      `json:"bundle_relative_path"`
	BundleInventory            []RuntimeObjectV1           `json:"bundle_inventory"`
	BundleTreeSHA256           string                      `json:"bundle_tree_sha256"`
	RuntimePayload             WindowsCanonicalEntryFileV1 `json:"runtime_payload"`
	RuntimePayloadBytes        []byte                      `json:"runtime_payload_bytes"`
}

type canonicalEntrySnapshotAliasV2 WindowsCanonicalEntrySetSnapshotV2
type canonicalEntrySnapshotWireV2 struct {
	canonicalEntrySnapshotAliasV2
	ProductArtifact canonicalEntryProductArtifactWireV1 `json:"product_artifact"`
}

func (v WindowsCanonicalEntrySetSnapshotV2) MarshalJSON() ([]byte, error) {
	p := v.ProductArtifact
	return json.Marshal(canonicalEntrySnapshotWireV2{canonicalEntrySnapshotAliasV2(v), canonicalEntryProductArtifactWireV1{p.Path, p.Role, p.Version, p.Commit, p.BuildDate, p.SHA256, p.Machine, p.Subsystem}})
}

func (v *WindowsCanonicalEntrySetSnapshotV2) UnmarshalJSON(raw []byte) error {
	var wire canonicalEntrySnapshotWireV2
	if err := decodeCanonicalEntryJSON(raw, &wire); err != nil {
		return err
	}
	if err := canonicalEntryExactJSONKeys(raw, "schema", "build_id", "registry_projection_sha256", "canonical_entry_relative_path", "product_relative_path", "identity_relative_path", "reverse_projection", "canonical_entry", "product", "identity", "machine", "product_artifact", "identity_bytes", "bundle_relative_path", "bundle_inventory", "bundle_tree_sha256", "runtime_payload", "runtime_payload_bytes"); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, name := range []string{"canonical_entry", "product", "identity", "runtime_payload"} {
		if err := canonicalEntryExactJSONKeys(fields[name], "path", "sha256"); err != nil {
			return err
		}
	}
	if err := canonicalEntryExactJSONKeys(fields["product_artifact"], "path", "role", "version", "commit", "build_date", "sha256", "machine", "subsystem"); err != nil {
		return err
	}
	if wire.Schema != WindowsCanonicalEntrySetSchemaV2 {
		return errors.New("unsupported current canonical entry snapshot schema")
	}
	result := WindowsCanonicalEntrySetSnapshotV2(wire.canonicalEntrySnapshotAliasV2)
	p := wire.ProductArtifact
	result.ProductArtifact = WindowsArtifact{Path: p.Path, Role: p.Role, Version: p.Version, Commit: p.Commit, BuildDate: p.BuildDate, SHA256: p.SHA256, Machine: p.Machine, Subsystem: p.Subsystem}
	if err := ValidateWindowsCanonicalEntrySetSnapshotV2(result); err != nil {
		return err
	}
	*v = result
	return nil
}

type WindowsCanonicalEntrySetTargetsV2 struct {
	CanonicalEntryPath string `json:"canonical_entry_path"`
	ProductPath        string `json:"product_path"`
	IdentityPath       string `json:"identity_path"`
	BundlePath         string `json:"bundle_path"`
}

func (t WindowsCanonicalEntrySetTargetsV2) entryTargets() WindowsCanonicalEntrySetTargetsV1 {
	return WindowsCanonicalEntrySetTargetsV1{t.CanonicalEntryPath, t.ProductPath, t.IdentityPath}
}

func CanonicalEntryTargetsV2(entryPath string) (WindowsCanonicalEntrySetTargetsV2, error) {
	r, err := CompiledWindowsProductRegistry()
	if err != nil {
		return WindowsCanonicalEntrySetTargetsV2{}, err
	}
	identity, err := r.IdentityRelativePath()
	if err != nil {
		return WindowsCanonicalEntrySetTargetsV2{}, err
	}
	if !filepath.IsAbs(entryPath) || filepath.Clean(entryPath) != entryPath || filepath.Base(entryPath) != r.Artifacts[1].Filename {
		return WindowsCanonicalEntrySetTargetsV2{}, errors.New("current canonical entry target must be exact absolute registry launcher path")
	}
	root := filepath.Dir(entryPath)
	return WindowsCanonicalEntrySetTargetsV2{entryPath, filepath.Join(root, filepath.FromSlash(r.Artifacts[0].Filename)), filepath.Join(root, filepath.FromSlash(identity)), filepath.Join(root, filepath.FromSlash(path.Dir(r.Artifacts[0].Filename)))}, nil
}

// The current registry is a required compiler input. Preserve its actual
// failure rather than comparing a missing projection with a default value.
func WindowsCanonicalEntryRegistryProjectionSHA256V2() (string, error) {
	r, err := CompiledWindowsProductRegistry()
	if err != nil {
		return "", err
	}
	return r.ProjectionSHA256()
}

// The complete-cohort owner is distinct from the read-only dispatch owner.
// Its original guards, partial acquisitions and cached Close belong to this set.
type WindowsCanonicalEntrySetV2 struct {
	mu                sync.Mutex
	guard             *WindowsCanonicalEntrySetV1
	parent, bundle    *canonicalEntryNativeFile
	objects           map[string]*canonicalEntryNativeFile
	ctx               context.Context
	snapshot          WindowsCanonicalEntrySetSnapshotV2
	directorySnapshot WindowsCanonicalEntryPayloadDirectorySnapshotV1
	ready, closed     bool
	closeError        error
}

func cloneCanonicalEntrySnapshotV2(v WindowsCanonicalEntrySetSnapshotV2) WindowsCanonicalEntrySetSnapshotV2 {
	v.IdentityBytes = slices.Clone(v.IdentityBytes)
	v.RuntimePayloadBytes = slices.Clone(v.RuntimePayloadBytes)
	v.BundleInventory = slices.Clone(v.BundleInventory)
	for i := range v.BundleInventory {
		if v.BundleInventory[i].Size != nil {
			n := *v.BundleInventory[i].Size
			v.BundleInventory[i].Size = &n
		}
	}
	return v
}

func ValidateWindowsCanonicalEntrySetSnapshotV2(v WindowsCanonicalEntrySetSnapshotV2) error {
	r, err := CompiledWindowsProductRegistry()
	if err != nil {
		return err
	}
	projection, err := r.ProjectionSHA256()
	if err != nil {
		return err
	}
	identityRelative, err := r.IdentityRelativePath()
	if err != nil {
		return err
	}
	if v.Schema != WindowsCanonicalEntrySetSchemaV2 || !validLowerHex(v.BuildID, 32, 32) || v.RegistryProjectionSHA256 != projection || v.CanonicalEntryRelativePath != r.Artifacts[1].Filename || v.ProductRelativePath != r.Artifacts[0].Filename || v.IdentityRelativePath != identityRelative || v.BundleRelativePath != path.Dir(r.Artifacts[0].Filename) || v.ReverseProjection != WindowsCanonicalEntryReverseProjectionV1 {
		return errors.New("invalid current canonical entry snapshot projection")
	}
	targets, err := CanonicalEntryTargetsV2(v.CanonicalEntry.Path)
	if err != nil {
		return err
	}
	if v.Product.Path != targets.ProductPath || v.Identity.Path != targets.IdentityPath || v.ProductArtifact.Path != v.Product.Path {
		return errors.New("current snapshot final paths disagree with registry")
	}
	for _, f := range []WindowsCanonicalEntryFileV1{v.CanonicalEntry, v.Product, v.Identity, v.RuntimePayload} {
		if f.Path == "" || !validLowerHex(f.SHA256, 64, 64) {
			return errors.New("incomplete current snapshot file")
		}
	}
	i, err := ParseWindowsProductIdentityV2(v.IdentityBytes)
	if err != nil {
		return err
	}
	p := v.ProductArtifact
	if canonicalEntrySHA256(v.IdentityBytes) != v.Identity.SHA256 || i.SHA256 != v.Product.SHA256 || i.LauncherSHA256 != v.CanonicalEntry.SHA256 || p.SHA256 != i.SHA256 || p.Role != i.Role || p.Version != i.Version || p.Commit != i.Commit || p.BuildDate != i.BuildDate || p.Machine != i.Machine || p.Subsystem != i.Subsystem || v.Machine != i.Machine || v.Machine != 0x8664 {
		return errors.New("current snapshot Product/identity disagreement")
	}
	payload, err := ParseRuntimePayload(v.RuntimePayloadBytes)
	if err != nil {
		return err
	}
	if v.RuntimePayload.Path != r.RuntimePayload.Filename || v.RuntimePayload.Path != path.Join(v.BundleRelativePath, i.RuntimePayloadPath) || v.RuntimePayload.SHA256 != i.RuntimePayloadSHA256 || RuntimeDigest(v.RuntimePayloadBytes) != i.RuntimePayloadSHA256 || payload.Target != "win32-x64" {
		return errors.New("current snapshot runtime binding disagreement")
	}
	wrapper := RuntimeInventoryV1{Schema: RuntimeInventorySchemaV1, Target: payload.Target, Objects: v.BundleInventory}
	raw, err := CanonicalRuntimeJSON(wrapper)
	if err != nil {
		return err
	}
	if _, err = ParseRuntimeInventory(raw); err != nil {
		return err
	}
	if !validLowerHex(v.BundleTreeSHA256, 64, 64) || RuntimeDigest(raw) != v.BundleTreeSHA256 {
		return errors.New("current snapshot canonical bundle digest disagreement")
	}
	objects := make(map[string]RuntimeObjectV1, len(v.BundleInventory))
	for _, o := range v.BundleInventory {
		objects[o.Path] = o
	}
	for name, hash := range map[string]string{path.Base(v.ProductRelativePath): v.Product.SHA256, path.Base(v.IdentityRelativePath): v.Identity.SHA256, i.RuntimePayloadPath: i.RuntimePayloadSHA256} {
		o, ok := objects[name]
		if !ok || o.Kind != "file" || o.SHA256 != hash {
			return errors.New("current snapshot required bundle file missing or mismatched")
		}
	}
	for _, c := range payload.Components {
		if objects[c.Root].Kind != "directory" || objects[c.Inventory].Kind != "file" || objects[c.Inventory].SHA256 != c.TreeSHA256 || objects[c.DescriptorPath].Kind != "file" || objects[c.DescriptorPath].SHA256 != c.DescriptorSHA256 {
			return errors.New("current snapshot authenticated component metadata missing or mismatched")
		}
	}
	return nil
}

// compareSnapshotV2 compares the complete detached projection, including every
// inventory field and exact acquired manifest/identity byte sequence.
func compareSnapshotV2(a, b WindowsCanonicalEntrySetSnapshotV2) bool {
	x, e := json.Marshal(a)
	if e != nil {
		return false
	}
	y, e := json.Marshal(b)
	return e == nil && bytes.Equal(x, y)
}

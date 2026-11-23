package binaryadmission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
)

const runtimePayloadIndexRelativeV4 = "runtime-payload.json"

// WindowsProductRegistryV4 explicitly binds the current runtime-index domain.
// The strict legacy wire type and its projection remain unchanged.
type WindowsProductRegistryV4 struct {
	WindowsProductRegistry
	RuntimePayload *struct {
		Schema             string   `json:"schema"`
		ReleaseDescriptors []string `json:"releaseDescriptors"`
		Filename           string   `json:"filename"`
	} `json:"runtimePayload"`
}

func windowsProductRegistryV4ReleaseDescriptors() [2]string {
	return [2]string{"runtimes/python/release.yaml", "runtimes/uv/release.yaml"}
}

func validateWindowsProductRegistryV4(v WindowsProductRegistryV4) error {
	if len(v.Artifacts) != 2 || v.IdentityManifest.Filename != WindowsCanonicalIdentityRelativePathV1 {
		return errors.New("PRODUCT_REGISTRY_INVALID")
	}
	want := canonicalEntryRegistryV1()
	descriptors := windowsProductRegistryV4ReleaseDescriptors()
	if v.Artifacts[0] != want.Artifacts[0] || v.Artifacts[1] != want.Artifacts[1] {
		return errors.New("PRODUCT_REGISTRY_EXECUTABLE_CONTRACT_INVALID")
	}
	if v.SchemaVersion != 4 || v.IdentityManifest.Schema != "windows-product-identity-v2" ||
		v.RuntimePayload == nil || v.RuntimePayload.Schema != RuntimePayloadSchemaV1 ||
		v.RuntimePayload.Filename != path.Join(path.Dir(want.Artifacts[0].Filename), runtimePayloadIndexRelativeV4) ||
		!slices.Equal(v.RuntimePayload.ReleaseDescriptors, descriptors[:]) {
		return errors.New("PRODUCT_REGISTRY_BUNDLE_INVALID")
	}
	return nil
}

// ParseWindowsProductRegistryV4 accepts the exact current producer wire shape.
func ParseWindowsProductRegistryV4(raw []byte) (v WindowsProductRegistryV4, err error) {
	if err = decodeCanonicalEntryJSON(raw, &v); err != nil {
		return v, err
	}
	if err = canonicalEntryExactJSONKeys(raw, "schemaVersion", "identityManifest", "artifacts", "runtimePayload"); err != nil {
		return v, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return v, err
	}
	if err = canonicalEntryExactJSONKeys(fields["identityManifest"], "schema", "filename"); err != nil {
		return v, err
	}
	if err = canonicalEntryExactJSONKeys(fields["runtimePayload"], "schema", "releaseDescriptors", "filename"); err != nil {
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
	return v, validateWindowsProductRegistryV4(v)
}

// IdentityRelativePath derives placement before acquisition and performs no
// path probing or fallback.
func (r WindowsProductRegistryV4) IdentityRelativePath() (string, error) {
	if err := validateWindowsProductRegistryV4(r); err != nil {
		return "", err
	}
	return path.Join(path.Dir(r.Artifacts[0].Filename), r.IdentityManifest.Filename), nil
}

func (r WindowsProductRegistryV4) ProjectionSHA256() (string, error) {
	if err := validateWindowsProductRegistryV4(r); err != nil {
		return "", err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return canonicalEntrySHA256(raw), nil
}

// ReadWindowsProductSourceV4 captures the current registry, its exact release
// descriptors and any requested pack metadata in one source snapshot read.
func ReadWindowsProductSourceV4(ctx context.Context, sourceRoot, snapshotPath string, additional ...string) (PublisherSourceInput, WindowsProductRegistryV4, map[string][]byte, error) {
	const registryPath = "windows-product-artifacts.json"
	descriptors := windowsProductRegistryV4ReleaseDescriptors()
	names := append([]string{registryPath, descriptors[0], descriptors[1]}, additional...)
	source, files, err := ReadPublisherSourceSnapshotFiles(ctx, sourceRoot, snapshotPath, names)
	if err != nil {
		return source, WindowsProductRegistryV4{}, nil, err
	}
	registry, err := ParseWindowsProductRegistryV4(files[registryPath])
	if err != nil {
		return source, WindowsProductRegistryV4{}, nil, err
	}
	return source, registry, files, nil
}

// ValidateWindowsProductRuntimeSourceV4 binds a previously inspected runtime
// payload to the descriptor bytes captured with that same registry.
func ValidateWindowsProductRuntimeSourceV4(payload RuntimePayloadV1, registry WindowsProductRegistryV4, files map[string][]byte) error {
	if err := validateWindowsProductRegistryV4(registry); err != nil {
		return err
	}
	ids := runtimePayloadComponentIDsV1()
	if len(payload.Components) != len(ids) {
		return errors.New("RUNTIME_BUILD_SOURCE_DESCRIPTOR_SET_MISMATCH")
	}
	for i, sourcePath := range registry.RuntimePayload.ReleaseDescriptors {
		component := payload.Components[i]
		raw, captured := files[sourcePath]
		if !captured || component.ID != ids[i] || RuntimeDigest(raw) != component.DescriptorSHA256 {
			return fmt.Errorf("RUNTIME_BUILD_SOURCE_DESCRIPTOR_MISMATCH: %s", sourcePath)
		}
	}
	return nil
}

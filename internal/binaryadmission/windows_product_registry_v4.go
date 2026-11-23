package binaryadmission

import (
	"encoding/json"
	"errors"
	"path"
)

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

func validateWindowsProductRegistryV4(v WindowsProductRegistryV4) error {
	if len(v.Artifacts) != 2 || v.IdentityManifest.Filename != WindowsCanonicalIdentityRelativePathV1 {
		return errors.New("PRODUCT_REGISTRY_INVALID")
	}
	want := canonicalEntryRegistryV1()
	if v.Artifacts[0] != want.Artifacts[0] || v.Artifacts[1] != want.Artifacts[1] {
		return errors.New("PRODUCT_REGISTRY_EXECUTABLE_CONTRACT_INVALID")
	}
	if v.SchemaVersion != 4 || v.IdentityManifest.Schema != "windows-product-identity-v2" ||
		v.RuntimePayload == nil || v.RuntimePayload.Schema != RuntimePayloadSchemaV1 ||
		v.RuntimePayload.Filename != "mcphub-product/runtime-payload.json" ||
		len(v.RuntimePayload.ReleaseDescriptors) != 2 ||
		v.RuntimePayload.ReleaseDescriptors[0] != "runtimes/python/release.yaml" ||
		v.RuntimePayload.ReleaseDescriptors[1] != "runtimes/uv/release.yaml" {
		return errors.New("PRODUCT_REGISTRY_BUNDLE_INVALID")
	}
	return nil
}

func parseWindowsProductRegistryV4(raw []byte) (v WindowsProductRegistryV4, err error) {
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

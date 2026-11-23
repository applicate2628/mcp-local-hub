package binaryadmission

import "errors"

// WindowsProductIdentityV2 preserves the V1 fields/order. The four added
// fields are absent for V1 and mandatory for V2; no legacy meaning is changed.
type WindowsProductIdentityV2 struct {
	Schema               string              `json:"schema"`
	Role                 WindowsArtifactRole `json:"role"`
	Filename             string              `json:"filename"`
	Subsystem            uint16              `json:"subsystem"`
	Machine              uint16              `json:"machine"`
	Version              string              `json:"version"`
	Commit               string              `json:"commit"`
	BuildDate            string              `json:"build_date"`
	SHA256               string              `json:"sha256"`
	SourceEpoch          string              `json:"source_epoch,omitempty"`
	LauncherSHA256       string              `json:"launcher_sha256,omitempty"`
	RuntimePayloadPath   string              `json:"runtime_payload_path,omitempty"`
	RuntimePayloadSHA256 string              `json:"runtime_payload_sha256,omitempty"`
}

func validWindowsProductIdentityMetadata(v WindowsProductIdentityV1) bool {
	return v.Role == WindowsArtifactRoleProduct && v.Filename == WindowsCanonicalEntryRelativePathV1 &&
		v.Subsystem == WindowsGUISubsystem && (v.Machine == 0x8664 || v.Machine == 0xaa64) &&
		validProductSemVer(v.Version) && validLowerHex(v.Commit, 40, 40) &&
		validUTCSeconds(v.BuildDate) && validLowerHex(v.SHA256, 64, 64)
}

func (v WindowsProductIdentityV2) legacyMetadata() WindowsProductIdentityV1 {
	return WindowsProductIdentityV1{v.Schema, v.Role, v.Filename, v.Subsystem, v.Machine, v.Version, v.Commit, v.BuildDate, v.SHA256}
}

func ParseWindowsProductIdentityV2(raw []byte) (v WindowsProductIdentityV2, err error) {
	if err = validateWindowsProductIdentitySize(int64(len(raw))); err != nil {
		return v, err
	}
	if err = decodeCanonicalEntryJSON(raw, &v); err != nil {
		return v, err
	}
	if v.Schema != "windows-product-identity-v2" {
		return v, errors.New("PRODUCT_IDENTITY_CURRENT_REQUIRED")
	}
	if err = canonicalEntryExactJSONKeys(raw, "schema", "role", "filename", "subsystem", "machine", "version", "commit", "build_date", "sha256", "source_epoch", "launcher_sha256", "runtime_payload_path", "runtime_payload_sha256"); err != nil {
		return v, err
	}
	if !validWindowsProductIdentityMetadata(v.legacyMetadata()) ||
		!validLowerHex(v.SourceEpoch, 64, 64) || !validLowerHex(v.LauncherSHA256, 64, 64) ||
		!RuntimeRelativePath(v.RuntimePayloadPath) || !validLowerHex(v.RuntimePayloadSHA256, 64, 64) {
		return v, errors.New("PRODUCT_IDENTITY_BUNDLE_INVALID")
	}
	return v, nil
}

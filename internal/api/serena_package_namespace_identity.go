package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type serenaNamespaceObjectID struct {
	VolumeID  string `json:"volume_id"`
	FileID128 string `json:"file_id128"`
}

func serenaNamespaceObject(value SerenaTreeObject) (serenaNamespaceObjectID, error) {
	if !value.Directory || len(value.Device) != 16 || len(value.FileID) != 32 || value.Device != strings.ToLower(value.Device) || value.FileID != strings.ToLower(value.FileID) {
		return serenaNamespaceObjectID{}, errors.New("SERENA_NAMESPACE_FULL_ID_UNDEFINED")
	}
	if _, err := hex.DecodeString(value.Device); err != nil {
		return serenaNamespaceObjectID{}, err
	}
	if _, err := hex.DecodeString(value.FileID); err != nil {
		return serenaNamespaceObjectID{}, err
	}
	return serenaNamespaceObjectID{value.Device, value.FileID}, nil
}
func sameSerenaNamespaceID(a, b SerenaTreeObject) bool {
	return a.Directory && b.Directory && a.Device == b.Device && a.FileID == b.FileID
}
func serenaPackageNamespaceDigest(key string, identity serenaPackageNamespaceIdentity) (string, error) {
	if !preparedSerenaDigest(key) {
		return "", errors.New("SERENA_PACKAGE_NAMESPACE_KEY_INVALID")
	}
	runtimeID, err := serenaNamespaceObject(identity.runtimeRoot)
	if err != nil {
		return "", err
	}
	packageID, err := serenaNamespaceObject(identity.packageRoot)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(struct {
		Schema      string                  `json:"schema"`
		Version     int                     `json:"version"`
		PackageKey  string                  `json:"package_key"`
		RuntimeRoot serenaNamespaceObjectID `json:"runtime_root"`
		PackageRoot serenaNamespaceObjectID `json:"package_root"`
	}{SerenaPackageNamespaceSchema, 1, key, runtimeID, packageID})
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(body), nil
}
func serenaRecordLeafParentDigest(common, role, name string, parent SerenaTreeObject) (string, error) {
	if !preparedSerenaDigest(common) || (role != "prepared-ready" && role != "prepared-operation") || (role == "prepared-ready" && name != "ready.json") || (role == "prepared-operation" && (!strings.HasSuffix(name, ".json") || !preparedSerenaID(strings.TrimSuffix(name, ".json")))) {
		return "", errors.New("SERENA_RECORD_LEAF_BINDING_INVALID")
	}
	parentID, err := serenaNamespaceObject(parent)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(struct {
		Schema           string                  `json:"schema"`
		Version          int                     `json:"version"`
		PackageNamespace string                  `json:"package_namespace_sha256"`
		LeafRole         string                  `json:"leaf_role"`
		Parent           serenaNamespaceObjectID `json:"parent"`
		LeafName         string                  `json:"leaf_name"`
	}{"serena-record-leaf-parent-v1", 1, common, role, parentID, name})
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(body), nil
}

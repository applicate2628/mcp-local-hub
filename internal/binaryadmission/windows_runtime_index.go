package binaryadmission

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
)

const RuntimePayloadSchemaV1 = "hub-runtime-payload-v1"
const runtimePayloadTargetV1 = "win32-x64"

type RuntimeComponentV1 struct {
	ID               string `json:"id"`
	DescriptorPath   string `json:"descriptor_path"`
	DescriptorSHA256 string `json:"descriptor_sha256"`
	Root             string `json:"root"`
	Inventory        string `json:"inventory"`
	TreeSHA256       string `json:"tree_sha256"`
}
type RuntimePayloadV1 struct {
	Schema     string               `json:"schema"`
	Target     string               `json:"target"`
	Components []RuntimeComponentV1 `json:"components"`
}

func CanonicalRuntimeJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func RuntimeDigest(raw []byte) string {
	return canonicalEntrySHA256(raw)
}

func RuntimeRelativePath(name string) bool {
	if name == "" || !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:\x00<>\"|?*") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.TrimRight(part, ". ") != part {
			return false
		}
		for _, c := range part {
			if c < 32 {
				return false
			}
		}
		stem := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
			return false
		}
	}
	return true
}

func decodeCanonicalRuntime(raw []byte, result any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(result); err != nil {
		return err
	}
	canonical, err := CanonicalRuntimeJSON(result)
	if err != nil || !bytes.Equal(raw, canonical) {
		return errors.New("RUNTIME_NONCANONICAL_JSON")
	}
	return nil
}

func RuntimeComponentContentID(target, descriptorSHA, treeSHA string) (string, error) {
	if target != runtimePayloadTargetV1 || !validLowerHex(descriptorSHA, 64, 64) || !validLowerHex(treeSHA, 64, 64) {
		return "", errors.New("RUNTIME_COMPONENT_ID_INPUT_INVALID")
	}
	raw, err := CanonicalRuntimeJSON(struct {
		Target           string `json:"target"`
		DescriptorSHA256 string `json:"descriptor_sha256"`
		TreeSHA256       string `json:"tree_sha256"`
	}{target, descriptorSHA, treeSHA})
	return RuntimeDigest(raw), err
}

func runtimePayloadComponentIDsV1() [2]string {
	return [2]string{"python", "uv"}
}

func runtimePayloadComponentV1(target, id, descriptorSHA, treeSHA string) (RuntimeComponentV1, error) {
	contentID, err := RuntimeComponentContentID(target, descriptorSHA, treeSHA)
	if err != nil {
		return RuntimeComponentV1{}, err
	}
	prefix := "runtimes/" + id + "/" + contentID + "/"
	return RuntimeComponentV1{
		ID: id, DescriptorPath: prefix + "release.yaml", DescriptorSHA256: descriptorSHA,
		Root: prefix + "root", Inventory: prefix + "inventory.json", TreeSHA256: treeSHA,
	}, nil
}

// The canonical encoder fixes the structure and escaping. Every admitted
// digest and content ID has the same lower-case SHA-256 width, and the two
// admitted component IDs and locators come from the parser's own schema.
func canonicalRuntimePayloadExtent() (int64, error) {
	sampleDigest := RuntimeDigest(nil)
	payload := RuntimePayloadV1{Schema: RuntimePayloadSchemaV1, Target: runtimePayloadTargetV1}
	for _, id := range runtimePayloadComponentIDsV1() {
		component, err := runtimePayloadComponentV1(payload.Target, id, sampleDigest, sampleDigest)
		if err != nil {
			return 0, err
		}
		payload.Components = append(payload.Components, component)
	}
	raw, err := CanonicalRuntimeJSON(payload)
	return int64(len(raw)), err
}

func ParseRuntimePayload(raw []byte) (v RuntimePayloadV1, err error) {
	if err = decodeCanonicalRuntime(raw, &v); err != nil {
		return v, err
	}
	ids := runtimePayloadComponentIDsV1()
	if v.Schema != RuntimePayloadSchemaV1 || v.Target != runtimePayloadTargetV1 || len(v.Components) != len(ids) {
		return v, errors.New("RUNTIME_PAYLOAD_INVALID")
	}
	for i, c := range v.Components {
		if c.ID != ids[i] || !validLowerHex(c.DescriptorSHA256, 64, 64) || !validLowerHex(c.TreeSHA256, 64, 64) {
			return v, errors.New("RUNTIME_COMPONENT_INVALID")
		}
		expected, err := runtimePayloadComponentV1(v.Target, c.ID, c.DescriptorSHA256, c.TreeSHA256)
		if err != nil {
			return v, err
		}
		if c.Root != expected.Root || c.Inventory != expected.Inventory || c.DescriptorPath != expected.DescriptorPath {
			return v, errors.New("RUNTIME_COMPONENT_LOCATOR_INVALID")
		}
	}
	return v, nil
}

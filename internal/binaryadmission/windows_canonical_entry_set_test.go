package binaryadmission

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func canonicalEntryTestBinding(t *testing.T) WindowsCanonicalEntryBuildBindingV1 {
	t.Helper()
	b, err := NewWindowsCanonicalEntryBuildBindingV1(strings.Repeat("a", 32), strings.Repeat("b", 64), WindowsCanonicalEntryReverseProjectionV1)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCanonicalEntryCommentsPreservesExactPriorAndSourceEpoch(t *testing.T) {
	b := canonicalEntryTestBinding(t)
	existing := "original\r\nComments\r\n\nmcphub-source-v1;sha256=" + strings.Repeat("c", 64)
	encoded, err := EncodeWindowsCanonicalEntryCommentsV1(existing, b)
	if err != nil {
		t.Fatal(err)
	}
	prior, parsed, err := ParseWindowsCanonicalEntryCommentsV1(encoded)
	if err != nil || prior != existing || parsed != b {
		t.Fatalf("prior=%q binding=%+v error=%v", prior, parsed, err)
	}
	epoch, err := parsePublisherSourceComments(encoded)
	if err != nil || epoch != strings.Repeat("c", 64) {
		t.Fatalf("epoch=%q error=%v", epoch, err)
	}
	legacyEpoch, err := parsePublisherSourceComments(existing)
	if err != nil || legacyEpoch != epoch {
		t.Fatalf("legacy epoch=%q error=%v", legacyEpoch, err)
	}
	if _, _, err := ParseWindowsCanonicalEntryCommentsV1(existing); !errors.Is(err, ErrWindowsCanonicalEntryBindingMissing) {
		t.Fatalf("legacy error=%v", err)
	}
	if _, err := EncodeWindowsCanonicalEntryCommentsV1(encoded, b); err == nil {
		t.Fatal("reserved marker accepted in prior Comments")
	}
}

func TestCanonicalEntryCommentsRejectsNoncanonicalAndAmbiguousRecords(t *testing.T) {
	b := canonicalEntryTestBinding(t)
	good, err := EncodeWindowsCanonicalEntryCommentsV1("old\r\n", b)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"missing inserted LF":   strings.TrimPrefix(strings.TrimPrefix(good, "old\r\n"), "\n"),
		"duplicate marker":      good + "\n" + canonicalEntryCommentsMarkerV1 + "{}",
		"unknown version":       strings.Replace(good, "ENTRY-V1=", "ENTRY-V2=", 1),
		"unknown key":           strings.Replace(good, `"build_id":`, `"extra":"x","build_id":`, 1),
		"duplicate decoded key": strings.Replace(good, `"build_id":`, `"build\u005fid":"x","build_id":`, 1),
		"case variant":          strings.Replace(good, `"build_id":`, `"BUILD_ID":`, 1),
		"missing digest":        strings.Replace(good, `"launcher_sha256":"`+b.ExpectedCanonicalEntrySHA256()+`",`, "", 1),
		"uppercase digest":      strings.Replace(good, b.ExpectedCanonicalEntrySHA256(), strings.ToUpper(b.ExpectedCanonicalEntrySHA256()), 1),
		"whitespace":            strings.Replace(good, `{"build_id":`, `{ "build_id":`, 1),
		"trailing newline":      good + "\n",
		"trailing document":     good + "{}",
		"wrong reverse":         strings.Replace(good, "../mcphub.exe", "../../mcphub.exe", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseWindowsCanonicalEntryCommentsV1(raw); err == nil {
				t.Fatal("accepted malformed record")
			}
			if _, err := parsePublisherSourceComments(raw); err == nil {
				t.Fatal("epoch reader accepted malformed reserved record")
			}
		})
	}
	if _, err := EncodeWindowsCanonicalEntryCommentsV1("reserved "+canonicalEntryCommentsNamespace+"V99=", b); err == nil {
		t.Fatal("unknown namespace collision accepted")
	}
	for _, input := range [][3]string{{"", b.ExpectedCanonicalEntrySHA256(), b.ReverseProjection()}, {b.BuildID(), "", b.ReverseProjection()}, {b.BuildID(), b.ExpectedCanonicalEntrySHA256(), ""}} {
		if _, err := NewWindowsCanonicalEntryBuildBindingV1(input[0], input[1], input[2]); err == nil {
			t.Fatal("missing build binding accepted")
		}
	}
}

func TestCanonicalEntryIdentityV1RejectsExtensionsDuplicatesAndDefaultInputs(t *testing.T) {
	i := WindowsProductIdentityV1{"windows-product-identity-v1", WindowsArtifactRoleProduct, "mcphub.exe", WindowsGUISubsystem, 0x8664, "0.4.41", strings.Repeat("d", 40), "2026-11-10T12:34:56Z", strings.Repeat("e", 64)}
	good, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	if actual, err := ParseWindowsProductIdentity(good); err != nil || actual != i {
		t.Fatalf("identity=%+v err=%v", actual, err)
	}
	for _, raw := range [][]byte{
		[]byte(`{}`), []byte(`null`), append(append([]byte(nil), good...), []byte(`{}`)...),
		bytes.Replace(good, []byte(`"schema":`), []byte(`"Schema":`), 1),
		bytes.Replace(good, []byte(`"role":`), []byte(`"role":"product","role":`), 1),
		bytes.Replace(good, []byte(`"sha256":`), []byte(`"source_epoch":"x","sha256":`), 1),
		bytes.Replace(good, []byte(`"subsystem":2`), []byte(`"subsystem":0`), 1),
		bytes.Replace(good, []byte(`"machine":34404`), []byte(`"machine":0`), 1),
	} {
		if _, err := ParseWindowsProductIdentity(raw); err == nil {
			t.Fatalf("accepted invalid identity %s", raw)
		}
	}
}

func TestCanonicalEntryRegistry3IsClosedAndRetainsFalseFields(t *testing.T) {
	good, err := json.Marshal(canonicalEntryRegistryV1())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseWindowsProductRegistry(good); err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{
		bytes.Replace(good, []byte(`"schemaVersion":3`), []byte(`"schemaVersion":4`), 1),
		bytes.Replace(good, []byte(`"npmBin":false`), []byte(`"npmBin":true`), 1),
		bytes.Replace(good, []byte(`,"npmBin":false`), nil, 1),
		bytes.Replace(good, []byte(`"productMetadata":false`), []byte(`"ProductMetadata":false`), 1),
		bytes.Replace(good, []byte(`"artifacts":`), []byte(`"runtimePayload":{},"artifacts":`), 1),
	} {
		if _, err := ParseWindowsProductRegistry(raw); err == nil {
			t.Fatalf("accepted invalid registry %s", raw)
		}
	}
	if h := WindowsCanonicalEntryRegistryProjectionSHA256V1(); !validLowerHex(h, 64, 64) {
		t.Fatalf("projection hash %q", h)
	}
}

func TestCanonicalEntrySnapshotJSONPreservesNativeArtifactFields(t *testing.T) {
	s := WindowsCanonicalEntrySetSnapshotV1{Schema: WindowsCanonicalEntrySetSchemaV1, Machine: 0x8664, ProductArtifact: WindowsArtifact{Path: "product", Role: WindowsArtifactRoleProduct, Machine: 0x8664, Subsystem: 2}, IdentityBytes: []byte("exact\r\nbytes")}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var actual WindowsCanonicalEntrySetSnapshotV1
	if err = json.Unmarshal(raw, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.ProductArtifact != s.ProductArtifact || actual.Machine != s.Machine || !bytes.Equal(actual.IdentityBytes, s.IdentityBytes) {
		t.Fatalf("snapshot lost native evidence: %s -> %+v", raw, actual)
	}
	if _, err = json.Marshal(s.ProductArtifact); err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(s.ProductArtifact)
	if bytes.Contains(legacy, []byte(`"machine"`)) || bytes.Contains(legacy, []byte(`"path"`)) {
		t.Fatalf("legacy WindowsArtifact wire shape changed: %s", legacy)
	}
}

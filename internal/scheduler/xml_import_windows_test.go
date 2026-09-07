//go:build windows

package scheduler

import (
	"bytes"
	"testing"
	"unicode/utf16"
)

func TestPrepareTaskXMLForImportEncodesUnicodeUTF8AsUTF16LEBOM(t *testing.T) {
	const source = `<?xml version="1.0"?><Task><RegistrationInfo><Description>plan §2531 Привет 😀</Description></RegistrationInfo></Task>`
	got, err := prepareTaskXMLForImport([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	want := EncodeXMLUTF16LEBOM(source)
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded bytes=%x want=%x", got, want)
	}
	if len(got) < 2 || got[0] != 0xFF || got[1] != 0xFE {
		t.Fatalf("missing UTF-16LE BOM: %x", got)
	}
	units := make([]uint16, 0, (len(got)-2)/2)
	for i := 2; i < len(got); i += 2 {
		units = append(units, uint16(got[i])|uint16(got[i+1])<<8)
	}
	if decoded := string(utf16.Decode(units)); decoded != source {
		t.Fatalf("decoded=%q want=%q", decoded, source)
	}
}

func TestPrepareTaskXMLForImportPreservesUTF16LEBOMBytes(t *testing.T) {
	liveness := EncodeXMLUTF16LEBOM(BuildLivenessXML(`C:\bin\mcphub.exe`, `C:\bin`, "§-Привет"))
	got, err := prepareTaskXMLForImport(liveness)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, liveness) {
		t.Fatalf("pre-encoded liveness XML changed: got=%x want=%x", got, liveness)
	}
}

func TestPrepareTaskXMLForImportRejectsInvalidBytes(t *testing.T) {
	for _, raw := range [][]byte{
		{0xFF},
		{0xFF, 0xFE, 0x00},
		{0xFF, 0xFE, 0x00, 0xD8},
		{0xFE, 0xFF, 0x00, 0x3C},
	} {
		if _, err := prepareTaskXMLForImport(raw); err == nil {
			t.Fatalf("invalid XML bytes accepted: %x", raw)
		}
	}
}

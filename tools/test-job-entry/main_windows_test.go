//go:build windows

package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixturePEAdmissionRequiresGUISubsystem(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		subsystem                      uint16
		optional, malformed, wantError bool
	}{
		{"gui", pe.IMAGE_SUBSYSTEM_WINDOWS_GUI, true, false, false},
		{"console", pe.IMAGE_SUBSYSTEM_WINDOWS_CUI, true, false, true},
		{"native", pe.IMAGE_SUBSYSTEM_NATIVE, true, false, true},
		{"missing-optional-header", 0, false, false, true},
		{"malformed", 0, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Metadata parser fixture only; no synthetic image is executed or
			// accepted as evidence of native Job/process ownership.
			var raw bytes.Buffer
			dos := make([]byte, 128)
			copy(dos, "MZ")
			binary.LittleEndian.PutUint32(dos[0x3c:], 128)
			raw.Write(dos)
			raw.WriteString("PE\x00\x00")
			optional := pe.OptionalHeader64{Magic: 0x20b, Subsystem: tc.subsystem, NumberOfRvaAndSizes: 16}
			coff := pe.FileHeader{Machine: pe.IMAGE_FILE_MACHINE_AMD64, Characteristics: pe.IMAGE_FILE_EXECUTABLE_IMAGE}
			if tc.optional {
				coff.SizeOfOptionalHeader = uint16(binary.Size(optional))
			}
			if err := binary.Write(&raw, binary.LittleEndian, coff); err != nil {
				t.Fatal(err)
			}
			if tc.optional {
				if err := binary.Write(&raw, binary.LittleEndian, optional); err != nil {
					t.Fatal(err)
				}
			}
			data := raw.Bytes()
			if tc.malformed {
				data = []byte("not PE")
			}
			path := filepath.Join(t.TempDir(), "fixture.exe")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := requireGUIFixtureImage(path); (err != nil) != tc.wantError {
				t.Fatalf("GUI image admission: %v", err)
			}
		})
	}
}

func protocolReady() entryReady {
	return entryReady{1, "ready", entryBind{"run", strings.Repeat("a", 32), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64), strings.Repeat("e", 64)}, strings.Repeat("f", 32), 17, "1234567"}
}

func TestCanonicalReceiverFramesRejectAmbiguity(t *testing.T) {
	ready := protocolReady()
	raw, err := json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}
	var actual entryReady
	if err := decodeCanonical(raw, &actual); err != nil || actual != ready {
		t.Fatalf("valid READY: %+v %v", actual, err)
	}
	text := string(raw)
	for name, invalid := range map[string]string{
		"duplicate":        strings.Replace(text, `"v":1`, `"v":1,"v":1`, 1),
		"duplicate-nested": strings.Replace(text, `"run":"run"`, `"run":"run","run":"run"`, 1),
		"missing":          strings.Replace(text, `"v":1,`, "", 1),
		"null":             strings.Replace(text, `"pid":17`, `"pid":null`, 1),
		"case":             strings.Replace(text, `"pid":17`, `"PID":17`, 1),
		"float":            strings.Replace(text, `"pid":17`, `"pid":17.0`, 1),
		"overflow":         strings.Replace(text, `"pid":17`, `"pid":4294967296`, 1),
		"extra":            strings.TrimSuffix(text, "}") + `,"extra":true}`,
		"trailing":         text + `{}`,
		"whitespace":       text + "\r",
		"nonascii":         strings.Replace(text, `"run":"run"`, `"run":"р"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			var got entryReady
			if err := decodeCanonical([]byte(invalid), &got); err == nil {
				t.Fatal("ambiguous receiver frame accepted")
			}
		})
	}
}

func TestReadyAndAckRequireExactRunAndGrant(t *testing.T) {
	ready := protocolReady()
	if err := validateReady(ready, ready.Bind); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*entryReady){
		"pid":                     func(v *entryReady) { v.PID = 0 },
		"generation-leading-zero": func(v *entryReady) { v.Created100ns = "01234567" },
		"generation-zero":         func(v *entryReady) { v.Created100ns = "0" },
		"challenge":               func(v *entryReady) { v.Challenge = strings.Repeat("F", 32) },
		"run":                     func(v *entryReady) { v.Bind.Run += "-other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := ready
			change(&changed)
			if err := validateReady(changed, ready.Bind); err == nil {
				t.Fatal("uncorrelated READY accepted")
			}
		})
	}
	grant := entryGrant{V: 1, Kind: "grant", Bind: ready.Bind, Challenge: ready.Challenge, PID: ready.PID, Created100ns: ready.Created100ns, OwnerGeneration: strings.Repeat("1", 32), Confirm: true}
	raw, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	ack := entryAck{1, "ack", ready.Bind.Nonce, ready.Challenge, grant.OwnerGeneration, digest(raw), true}
	if err := validateAck(ack, ready, grant, raw); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*entryAck){
		"nonce":        func(v *entryAck) { v.Nonce = strings.Repeat("2", 32) },
		"challenge":    func(v *entryAck) { v.Challenge = strings.Repeat("2", 32) },
		"owner":        func(v *entryAck) { v.OwnerGeneration = strings.Repeat("2", 32) },
		"payload":      func(v *entryAck) { v.GrantSHA256 = digest(append(raw, '\n')) },
		"confirmation": func(v *entryAck) { v.Confirm = false },
	} {
		t.Run(name, func(t *testing.T) {
			changed := ack
			change(&changed)
			if err := validateAck(changed, ready, grant, raw); err == nil {
				t.Fatal("uncorrelated ACK accepted")
			}
		})
	}
}

func TestCaptureRequiresBoundedCompleteControlFrames(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		wantError  bool
		frames     int
	}{
		{"complete", "ordinary output\n" + entryPrefix + "{}\n" + entryPrefix + "{}\n", false, 2},
		{"truncated", entryPrefix + "{}", true, 0},
		{"extra", strings.Repeat(entryPrefix+"{}\n", 3), true, 2},
		{"oversized-frame", entryPrefix + strings.Repeat("x", frameLimit) + "\n", true, 0},
		{"oversized-line", strings.Repeat("x", 64<<10) + "\n", true, 0},
		{"oversized-stream", strings.Repeat("line\n", streamLimit/5+1), true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			frames := make(chan []byte, 2)
			got := captureStream(strings.NewReader(tc.text), &log, true, frames)
			if (got.err != nil) != tc.wantError || got.frames != tc.frames || got.eof == tc.wantError {
				t.Fatalf("capture: %+v", got)
			}
		})
	}
}

func TestFixtureEnvironmentExcludesInheritedHelperModes(t *testing.T) {
	got := fixtureEnvironment([]string{"Path=path", "=C:=C:\\checkout", "MCPHUB_JOB_PROOF_CHILD=1", "mcphub_job_fixture_entry_config=stale", "MCPHUB_CANONICAL_COMMAND_ACTOR=private"}, "C:\\private\\entry-config.json")
	want := []string{"Path=path", "=C:=C:\\checkout", entryConfigEnv + "=C:\\private\\entry-config.json"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("environment: %q", got)
	}
}

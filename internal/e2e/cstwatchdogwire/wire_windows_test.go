//go:build windows && cst_vptr_probe

package cstwatchdogwire

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"reflect"
	"strings"
	"testing"
)

func TestCSTF4CellLayoutAndRoundTrip(t *testing.T) {
	var nonce, token [16]byte
	for i := range nonce {
		nonce[i] = byte(i)
		token[i] = byte(0xf0 + i)
	}
	binding := NewBinding(nonce, "silence", token)
	frame := Frame{Binding: binding, Sequence: 1, Phase: Ready}
	raw, err := Encode(frame)
	if err != nil {
		t.Fatal(err)
	}
	cellDigest := sha256.Sum256([]byte("silence"))
	rootDigest := sha256.Sum256(token[:])
	if string(raw[:8]) != "CSTF4W01" || string(raw[8:24]) != string(nonce[:]) || string(raw[24:40]) != string(cellDigest[:16]) || string(raw[40:56]) != string(rootDigest[:16]) {
		t.Fatal("frozen F4 header offsets changed")
	}
	if binary.LittleEndian.Uint64(raw[56:64]) != 1 || binary.LittleEndian.Uint32(raw[64:68]) != 1 || binary.LittleEndian.Uint32(raw[68:72]) != 0 || binary.LittleEndian.Uint64(raw[72:80]) != 0 || binary.LittleEndian.Uint32(raw[80:84]) != 0 {
		t.Fatal("frozen F4 sequence/phase/flag/handle/cause offsets changed")
	}
	for _, value := range raw[84:92] {
		if value != 0 {
			t.Fatal("frozen reserved bytes changed")
		}
	}
	if binary.LittleEndian.Uint32(raw[92:96]) != crc32.Checksum(raw[:92], crc32.MakeTable(crc32.Castagnoli)) {
		t.Fatal("frozen CRC position or polynomial changed")
	}
	got, err := Decode(raw[:], raw[:], binding)
	if err != nil || got != frame {
		t.Fatalf("valid specimen rejected or changed: got=%+v err=%v", got, err)
	}
	for _, legal := range []Frame{
		{Binding: binding, Sequence: 2, Phase: Started, Flags: Attempted},
		{Binding: binding, Sequence: 3, Phase: Barrier, Flags: Attempted | BarrierSeen, CustodianHandle: 7},
		{Binding: binding, Sequence: 4, Phase: Fired, Flags: Attempted | BarrierSeen | InjectionFired},
		{Binding: binding, Sequence: 5, Phase: Finished, Flags: Attempted | BarrierSeen | InjectionFired | FinishedSeen, Cause: CauseSilent},
		{Binding: binding, Sequence: 3, Phase: Fired, Flags: Attempted | InjectionFired},
		{Binding: binding, Sequence: 4, Phase: Finished, Flags: Attempted | InjectionFired | FinishedSeen, Cause: CauseLaunchPrecreate},
		{Binding: binding, Sequence: 2, Phase: Fault, Flags: Attempted, Cause: CauseRootUnbound},
	} {
		encoded, err := Encode(legal)
		if err != nil {
			t.Fatalf("legal phase %d rejected: %v", legal.Phase, err)
		}
		decoded, err := Decode(encoded[:], encoded[:], binding)
		if err != nil || decoded != legal {
			t.Fatalf("legal phase %d failed round trip: got=%+v err=%v", legal.Phase, decoded, err)
		}
	}
}

func TestCSTF4CellRejectsMalformedAndWrongBinding(t *testing.T) {
	var nonce, token [16]byte
	nonce[0], token[0] = 1, 2
	binding := NewBinding(nonce, "silence", token)
	base, err := Encode(Frame{Binding: binding, Sequence: 1, Phase: Ready})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		change     func(*[Size]byte)
		keepBadCRC bool
	}{
		{"magic", func(raw *[Size]byte) { raw[0] ^= 1 }, false},
		{"nonce", func(raw *[Size]byte) { raw[8] ^= 1 }, false},
		{"cell", func(raw *[Size]byte) { raw[24] ^= 1 }, false},
		{"root", func(raw *[Size]byte) { raw[40] ^= 1 }, false},
		{"zero_sequence", func(raw *[Size]byte) { binary.LittleEndian.PutUint64(raw[56:64], 0) }, false},
		{"phase", func(raw *[Size]byte) { binary.LittleEndian.PutUint32(raw[64:68], 7) }, false},
		{"flags", func(raw *[Size]byte) { binary.LittleEndian.PutUint32(raw[68:72], Attempted) }, false},
		{"handle", func(raw *[Size]byte) { binary.LittleEndian.PutUint64(raw[72:80], 9) }, false},
		{"cause", func(raw *[Size]byte) { binary.LittleEndian.PutUint32(raw[80:84], CauseSilent) }, false},
		{"reserved", func(raw *[Size]byte) { raw[84] = 1 }, false},
		{"crc", func(raw *[Size]byte) { raw[92] ^= 1 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := base
			tc.change(&bad)
			if !tc.keepBadCRC {
				binary.LittleEndian.PutUint32(bad[92:96], crc32.Checksum(bad[:92], crc32.MakeTable(crc32.Castagnoli)))
			}
			if _, err := Decode(bad[:], bad[:], binding); err == nil {
				t.Fatal("malformed or valid-CRC wrong-meaning cell accepted")
			}
		})
	}
	torn := base
	torn[0] ^= 1
	if _, err := Decode(base[:], torn[:], binding); err == nil {
		t.Fatal("torn two-read cell accepted")
	}
	if _, err := Decode(base[:95], base[:], binding); err == nil {
		t.Fatal("short cell accepted")
	}
	wrong := NewBinding(nonce, "other-cell", token)
	if _, err := Decode(base[:], base[:], wrong); err == nil {
		t.Fatal("wrong selected cell accepted")
	}
	wrongToken := token
	wrongToken[0] ^= 1
	wrong = NewBinding(nonce, "silence", wrongToken)
	if _, err := Decode(base[:], base[:], wrong); err == nil {
		t.Fatal("wrong root token accepted")
	}
}

func TestCSTF4CellRejectsImpossibleFaultFlags(t *testing.T) {
	var nonce, token [16]byte
	nonce[0], token[0] = 1, 2
	binding := NewBinding(nonce, "silence", token)
	valid, err := Encode(Frame{Binding: binding, Sequence: 1, Phase: Fault, Flags: Attempted, Cause: CauseRootUnbound})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		flags uint32
	}{
		{"barrier_without_attempt", BarrierSeen},
		{"fired_without_attempt", InjectionFired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Encode(Frame{Binding: binding, Sequence: 1, Phase: Fault, Flags: tc.flags, Cause: CauseRootUnbound}); err == nil {
				t.Fatal("Encode accepted impossible cumulative FAULT flags")
			}
			bad := valid
			binary.LittleEndian.PutUint32(bad[68:72], tc.flags)
			binary.LittleEndian.PutUint32(bad[92:96], crc32.Checksum(bad[:92], crc32.MakeTable(crc32.Castagnoli)))
			if _, err := Decode(bad[:], bad[:], binding); err == nil {
				t.Fatal("Decode accepted CRC-valid impossible cumulative FAULT flags")
			}
		})
	}
}

func TestCSTF4EvidenceV1B0CompatibilityAndTask2Strictness(t *testing.T) {
	token := strings.Repeat("0", 32)
	b0 := `{"version":"CSTF4WatchdogEvidenceV1","cell":"precreate","rootToken":"` + token +
		`","attempted":1,"barrier":0,"fired":1,"finished":1,"cause":"launch_precreate_failed"}`
	encoded, err := json.Marshal(Evidence{Version: EvidenceVersion, Cell: "precreate", RootToken: token,
		Attempted: 1, Fired: 1, Finished: 1, Cause: "launch_precreate_failed"})
	if err != nil || string(encoded) != b0 {
		t.Fatalf("B0 evidence bytes changed: %s err=%v", encoded, err)
	}
	gotB0, err := DecodeEvidence([]byte(b0))
	if err != nil || gotB0.Task2 != nil {
		t.Fatalf("B0 evidence was not accepted without Task 2: got=%+v err=%v", gotB0, err)
	}
	valid := fmt.Sprintf(`{"version":"CSTF4WatchdogEvidenceV1","cell":"b1_crash","rootToken":%q,"attempted":1,"barrier":1,"fired":1,"finished":1,"cause":"custodian_lost","task2":{"barrier":"B1","observerState":"none","f3NonceDigest":%q,"f3Sequence":2,"slotSha256":%q,"target":{"pid":101,"creationTicks":1,"imageSha256":%q,"originalRetained":true,"startClosed":true,"innerMember":true},"settlement":{"targetJobClose":"unknown","childWait":"unknown","stdinJoin":"unknown","stdoutEOF":"unknown","stderrEOF":"unknown","settledExit":"unknown"}}}`,
		token, strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64))
	gotB1, err := DecodeEvidence([]byte(valid))
	if err != nil || gotB1.Task2 == nil || gotB1.Task2.Target == nil || gotB1.Task2.Target.PID != 101 {
		t.Fatalf("complete B1 evidence rejected or changed: got=%+v err=%v", gotB1, err)
	}
	for _, tc := range []struct{ name, raw string }{
		{"unknown_nested", strings.Replace(valid, `"barrier":"B1"`, `"barrier":"B1","extra":1`, 1)},
		{"missing_original_retained", strings.Replace(valid, `,"originalRetained":true`, "", 1)},
		{"wrong_boolean_type", strings.Replace(valid, `"startClosed":true`, `"startClosed":"true"`, 1)},
		{"zero_f3_sequence", strings.Replace(valid, `"f3Sequence":2`, `"f3Sequence":0`, 1)},
		{"invalid_observer_state", strings.Replace(valid, `"observerState":"none"`, `"observerState":"later"`, 1)},
		{"missing_settlement_result", strings.Replace(valid, `"settledExit":"unknown"`, `"settledExit":""`, 1)},
		{"zero_target_pid", strings.Replace(valid, `"pid":101`, `"pid":0`, 1)},
		{"trailing_value", valid + `{}`},
		{"null_task2", strings.TrimSuffix(b0, `}`) + `,"task2":null}`},
		{"missing_finished", strings.Replace(b0, `,"finished":1`, "", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeEvidence([]byte(tc.raw)); err == nil {
				t.Fatal("malformed or incomplete Evidence V1 accepted")
			}
		})
	}
}

func TestTask2ObservationSlotV1RoundTrip(t *testing.T) {
	var nonce [16]byte
	copy(nonce[:], []byte("task2-nonce-1234"))
	owned := true
	observation := Task2Evidence{
		Barrier: "B1", ObserverState: "none",
		Target: &Task2Target{PID: 101, CreationTicks: 1, ImageSHA256: strings.Repeat("c", 64),
			OriginalRetained: &owned, StartClosed: &owned, InnerMember: &owned},
		Settlement: &Task2Settlement{TargetJobClose: "unknown", ChildWait: "unknown", StdinJoin: "unknown",
			StdoutEOF: "unknown", StderrEOF: "unknown", SettledExit: "unknown"},
	}
	raw, err := EncodeTask2Slot(nonce, 2, observation)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 4096 || string(raw[:4]) != "CST2" || binary.LittleEndian.Uint16(raw[4:6]) != 1 ||
		string(raw[8:24]) != string(nonce[:]) || binary.LittleEndian.Uint64(raw[24:32]) != 2 {
		t.Fatal("Task 2 slot header/size mismatch")
	}
	length := binary.LittleEndian.Uint16(raw[6:8])
	if length == 0 || length > 4032 || sha256.Sum256(raw[64:64+length]) != [32]byte(raw[32:64]) {
		t.Fatal("Task 2 payload length or hash mismatch")
	}
	for _, value := range raw[64+length:] {
		if value != 0 {
			t.Fatal("Task 2 slot tail is not zero")
		}
	}
	got, err := DecodeTask2Slot(raw[:], raw[:], nonce, 2)
	if err != nil || !reflect.DeepEqual(got, observation) {
		t.Fatalf("Task 2 slot round trip changed observation: got=%+v err=%v", got, err)
	}
}

func TestTask2ObservationSlotV1RejectsMalformed(t *testing.T) {
	var nonce [16]byte
	copy(nonce[:], []byte("task2-nonce-1234"))
	owned := true
	base, err := EncodeTask2Slot(nonce, 2, Task2Evidence{
		Barrier: "B1", ObserverState: "none",
		Target: &Task2Target{PID: 101, CreationTicks: 1, ImageSHA256: strings.Repeat("c", 64),
			OriginalRetained: &owned, StartClosed: &owned, InnerMember: &owned},
		Settlement: &Task2Settlement{TargetJobClose: "unknown", ChildWait: "unknown", StdinJoin: "unknown",
			StdoutEOF: "unknown", StderrEOF: "unknown", SettledExit: "unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*[Task2SlotSize]byte)
	}{
		{"magic", func(raw *[Task2SlotSize]byte) { raw[0] ^= 1 }},
		{"version", func(raw *[Task2SlotSize]byte) { binary.LittleEndian.PutUint16(raw[4:6], 2) }},
		{"zero_length", func(raw *[Task2SlotSize]byte) { binary.LittleEndian.PutUint16(raw[6:8], 0) }},
		{"oversize_length", func(raw *[Task2SlotSize]byte) { binary.LittleEndian.PutUint16(raw[6:8], 4033) }},
		{"nonce", func(raw *[Task2SlotSize]byte) { raw[8] ^= 1 }},
		{"sequence", func(raw *[Task2SlotSize]byte) { binary.LittleEndian.PutUint64(raw[24:32], 3) }},
		{"payload_hash", func(raw *[Task2SlotSize]byte) { raw[32] ^= 1 }},
		{"nonzero_tail", func(raw *[Task2SlotSize]byte) {
			length := int(binary.LittleEndian.Uint16(raw[6:8]))
			raw[64+length] = 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := base
			tc.change(&bad)
			if _, err := DecodeTask2Slot(bad[:], bad[:], nonce, 2); err == nil {
				t.Fatal("malformed Task 2 slot admitted")
			}
		})
	}
	torn := base
	torn[64] ^= 1
	if _, err := DecodeTask2Slot(base[:], torn[:], nonce, 2); err == nil {
		t.Fatal("torn two-read Task 2 slot admitted")
	}
	if _, err := DecodeTask2Slot(base[:4095], base[:], nonce, 2); err == nil {
		t.Fatal("short Task 2 slot admitted")
	}
	for _, tc := range []struct{ name, added string }{
		{"unknown_payload_field", `,"extra":1`},
		{"self_referential_slot_hash", `,"slotSha256":"` + strings.Repeat("d", 64) + `"`},
		{"child_authored_f3_sequence", `,"f3Sequence":2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			length := int(binary.LittleEndian.Uint16(base[6:8]))
			payload := strings.TrimSuffix(string(base[64:64+length]), "}") + tc.added + "}"
			if len(payload) > 4032 {
				t.Fatal("fixture exceeds slot bound")
			}
			var bad [Task2SlotSize]byte
			copy(bad[:64], base[:64])
			binary.LittleEndian.PutUint16(bad[6:8], uint16(len(payload)))
			copy(bad[64:], payload)
			hash := sha256.Sum256([]byte(payload))
			copy(bad[32:64], hash[:])
			if _, err := DecodeTask2Slot(bad[:], bad[:], nonce, 2); err == nil {
				t.Fatal("hash-valid wrong-meaning Task 2 slot admitted")
			}
		})
	}
}

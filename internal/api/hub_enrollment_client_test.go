package api

import (
	"encoding/json"
	"testing"
)

func TestEnrollmentClientClosedFrames(t *testing.T) {
	enroll := HubEnrollmentEnrollV1{
		Version: 1, Op: "enroll", Challenge: "challenge", Correlation: "correlation",
		Task: "cst", Generation: 3, CapabilitySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	raw, err := json.Marshal(enroll)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"op":"enroll","challenge":"challenge","correlation":"correlation","task":"cst","generation":3,"capability_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	if string(raw) != want {
		t.Fatalf("wire=%s want=%s", raw, want)
	}
	if len(raw) > HubEnrollmentMaxFrameBytes {
		t.Fatalf("frame exceeds %d", HubEnrollmentMaxFrameBytes)
	}
	if err := ValidateHubEnrollmentEnrollV1(enroll); err != nil {
		t.Fatalf("valid enroll rejected: %v", err)
	}
	var withUnknown HubEnrollmentEnrollV1
	if err := DecodeHubEnrollmentFrameV1([]byte(`{"version":1,"op":"enroll","challenge":"challenge","correlation":"correlation","task":"cst","generation":3,"capability_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","extra":true}`), &withUnknown); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestEnrollmentClientReceiptStateGate(t *testing.T) {
	good := HubEnrollmentReceiptV1{Version: 1, Correlation: "c", State: HubEnrollmentStateEnrolled, ChannelSettled: true}
	if err := ValidateHubEnrollmentReceiptV1(good, "c", HubEnrollmentStateEnrolled); err != nil {
		t.Fatalf("good receipt rejected: %v", err)
	}
	bad := []HubEnrollmentReceiptV1{
		{Version: 1, Correlation: "wrong", State: HubEnrollmentStateEnrolled, ChannelSettled: true},
		{Version: 1, Correlation: "c", State: HubEnrollmentStateConsumed, ChannelSettled: true},
		{Version: 1, Correlation: "c", State: HubEnrollmentStateEnrolled},
	}
	for _, receipt := range bad {
		if err := ValidateHubEnrollmentReceiptV1(receipt, "c", HubEnrollmentStateEnrolled); err == nil {
			t.Fatalf("bad receipt accepted: %#v", receipt)
		}
	}
}

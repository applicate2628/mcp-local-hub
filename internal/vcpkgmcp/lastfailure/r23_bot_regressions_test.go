package lastfailure

import "testing"

func TestR23WrapperDoesNotPairEarlierTargetWithLaterCommandTriplet(t *testing.T) {
	data := []byte("command: vcpkg install somelib\n" +
		"command: vcpkg install otherlib --triplet=cl\n")
	info, ok, err := ParseWrapperContent(data)
	if err != nil || !ok {
		t.Fatalf("ParseWrapperContent() = ok=%v err=%v", ok, err)
	}
	if info.RequestedTargetWasAttempted("somelib", "cl") {
		t.Fatal("target from an earlier invocation was paired with the later invocation triplet")
	}
	if !info.RequestedTargetWasAttempted("otherlib", "cl") {
		t.Fatal("latest invocation target/triplet pair was not retained")
	}
}

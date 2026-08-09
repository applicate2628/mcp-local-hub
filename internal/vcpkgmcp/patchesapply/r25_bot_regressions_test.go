package patchesapply

import "testing"

func TestR25ZeroValueTripletSetRemovesFact(t *testing.T) {
	facts := parseTripletFacts("set(VAR old)\nset(VAR)\n", "", "demo", "")
	if _, ok := facts["VAR"]; ok {
		t.Fatalf("facts=%v, zero-value set must remove VAR", facts)
	}
}

func TestR25QuotedEmptyTripletSetRetainsEmptyFact(t *testing.T) {
	facts := parseTripletFacts("set(VAR old)\nset(VAR \"\")\n", "", "demo", "")
	if value, ok := facts["VAR"]; !ok || value != "" {
		t.Fatalf("facts=%v, quoted empty value is a defined empty fact", facts)
	}
}

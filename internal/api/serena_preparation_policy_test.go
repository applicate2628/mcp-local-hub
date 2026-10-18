package api

import "testing"

func TestSerenaPreparationPolicySelection(t *testing.T) {
	policy, err := resolveSerenaPreparationPolicy(nil)
	if err != nil || policy != DefaultSerenaPreparationPolicy() {
		t.Fatal("missing override did not select the single owned default", err)
	}
	zero := SerenaPreparationPolicyV1{}
	if _, err = resolveSerenaPreparationPolicy(&zero); err == nil {
		t.Fatal("present zero override silently fell back")
	}
	baseline, _ := policy.IdentitySHA256()
	for _, selector := range []string{"", "python", "3.10.9", "3.15.0", "03.13.11", "3"} {
		changed := policy
		changed.InterpreterSelector = selector
		if changed.Validate() == nil {
			t.Fatalf("invalid selector admitted: %q", selector)
		}
	}
	policy.InterpreterSelector = "3.14.7"
	changed, err := policy.IdentitySHA256()
	if err != nil || changed == baseline {
		t.Fatal("supported explicit interpreter change failed to split identity", err)
	}
}

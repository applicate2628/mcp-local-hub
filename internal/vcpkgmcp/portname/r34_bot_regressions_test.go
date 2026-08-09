package portname

import (
	"errors"
	"testing"
)

func TestR34ConsecutiveHyphensAreRejected(t *testing.T) {
	for _, raw := range []string{"a--b", "x---y"} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) unexpectedly succeeded", raw)
		} else {
			var invalid *InvalidNameError
			if !errors.As(err, &invalid) {
				t.Fatalf("Parse(%q) error=%T %v, want InvalidNameError", raw, err, err)
			}
		}
	}
}

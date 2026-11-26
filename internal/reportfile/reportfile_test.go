package reportfile

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReadPrefixWithStaleSize(t *testing.T) {
	const capBytes = 8
	r := strings.NewReader("report body with a tail")
	prefix, err := readPrefix(r, 3, capBytes)
	if err != nil || string(prefix) != "report bo" || r.Len() != 0 {
		t.Fatalf("prefix=%q, unread bytes=%d, error=%v", prefix, r.Len(), err)
	}
}

func TestReadPrefixDiscardsPrefixAfterTailError(t *testing.T) {
	const capBytes = 8
	failure := errors.New("injected read error")
	r := io.MultiReader(strings.NewReader("report body with a tail"), iotest.ErrReader(failure))
	prefix, err := readPrefix(r, 24, capBytes)
	if prefix != nil || !errors.Is(err, failure) {
		t.Fatalf("prefix=%q, error=%v, want injected error with no prefix", prefix, err)
	}
}

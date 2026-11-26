package branding

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestPackPNGICO(t *testing.T) {
	for _, tc := range []struct {
		name    string
		images  []PNGIcon
		wantHex string
	}{
		{"single image payload begins at byte 22", []PNGIcon{{Width: 16, Height: 16, PNG: []byte{1, 2}}}, "000001000100101000000100200002000000160000000102"},
		{"multiple images, wide and tall", []PNGIcon{{Width: 16, Height: 32, PNG: []byte{1, 2}}, {Width: 256, Height: 256, PNG: []byte{3}}}, "0000010002001020000001002000020000002600000000000000010020000100000028000000010203"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := hex.DecodeString(tc.wantHex)
			if err != nil {
				t.Fatal(err)
			}
			if got := PackPNGICO(tc.images...); !bytes.Equal(got, want) {
				t.Fatalf("ICO=%x, want %x", got, want)
			}
		})
	}
}

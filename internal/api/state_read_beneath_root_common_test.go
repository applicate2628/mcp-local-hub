package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadClientConfigBackupBeneathRootNoFollowPolicy(t *testing.T) {
	t.Run("exact-cap-capture-and-verified-rollback", func(t *testing.T) {
		root := t.TempDir()
		payload := bytes.Repeat([]byte("x"), MaxClientConfigBackupBytes)
		if err := os.WriteFile(filepath.Join(root, "backup.json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		captured, err := ReadClientConfigBackupBeneathRootNoFollow(context.Background(), root, []string{"backup.json"}, "")
		if err != nil {
			t.Fatalf("capture at exact cap: %v", err)
		}
		if !bytes.Equal(captured, payload) {
			t.Fatal("capture returned bytes different from the retained final handle")
		}
		verified, err := ReadClientConfigBackupBeneathRootNoFollow(context.Background(), root, []string{"backup.json"}, stateReadDigest(payload))
		if err != nil {
			t.Fatalf("rollback verification at exact cap: %v", err)
		}
		if !bytes.Equal(verified, payload) {
			t.Fatal("verified rollback returned bytes different from the retained final handle")
		}
	})

	t.Run("cap-plus-one", func(t *testing.T) {
		root := t.TempDir()
		payload := bytes.Repeat([]byte("x"), MaxClientConfigBackupBytes+1)
		if err := os.WriteFile(filepath.Join(root, "backup.json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadClientConfigBackupBeneathRootNoFollow(context.Background(), root, []string{"backup.json"}, "")
		requireStateReadCategory(t, err, StateFileReadErrorTooLarge)
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := ReadClientConfigBackupBeneathRootNoFollow(ctx, t.TempDir(), []string{"backup.json"}, "")
		requireStateReadCategory(t, err, StateFileReadErrorCanceled)
	})

	t.Run("checksum-mismatch", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "backup.json"), []byte("actual"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadClientConfigBackupBeneathRootNoFollow(context.Background(), root, []string{"backup.json"}, stateReadDigest([]byte("expected")))
		requireStateReadCategory(t, err, StateFileReadErrorChecksumMismatch)
	})

	t.Run("non-regular-final-object", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "backup.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := ReadClientConfigBackupBeneathRootNoFollow(context.Background(), root, []string{"backup.json"}, "")
		requireStateReadCategory(t, err, StateFileReadErrorUnsafeObjectOrIO)
	})
}

func stateReadDigest(bytes []byte) string {
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:])
}

func requireStateReadCategory(t *testing.T, err error, want StateFileReadErrorCategory) {
	t.Helper()
	var readErr *StateFileReadError
	if !errors.As(err, &readErr) {
		t.Fatalf("error %T %v does not retain StateFileReadError", err, err)
	}
	if readErr.Category != want {
		t.Fatalf("category=%q, want %q; error=%v", readErr.Category, want, err)
	}
}

func TestStateFileReadErrorCategorySurvivesWrappingAndProseChange(t *testing.T) {
	sentinel := errors.New("underlying cause")
	for _, category := range []StateFileReadErrorCategory{
		StateFileReadErrorInvalidInput,
		StateFileReadErrorCanceled,
		StateFileReadErrorUnsafeObjectOrIO,
		StateFileReadErrorTooLarge,
		StateFileReadErrorChecksumMismatch,
	} {
		t.Run(string(category), func(t *testing.T) {
			base := newStateFileReadError(category, "test", "baseline.json", sentinel)
			wrapped := fmt.Errorf("new caller prose: %w", base)
			requireStateReadCategory(t, wrapped, category)
			if !errors.Is(wrapped, sentinel) {
				t.Fatalf("wrapped %q category lost its underlying cause", category)
			}
		})
	}
}

func TestValidateStateReadBeneathRootInputReturnsOneTypedTaxonomy(t *testing.T) {
	root := t.TempDir()
	digest := strings.Repeat("0", sha256.Size*2)
	tests := []struct {
		name      string
		root      string
		parts     []string
		digest    string
		component string
		wantHex   bool
	}{
		{name: "relative-root", root: "relative", parts: []string{"pin.json"}, digest: digest},
		{name: "no-components", root: root, digest: digest},
		{name: "short-digest", root: root, parts: []string{"pin.json"}, digest: strings.Repeat("0", sha256.Size*2-1)},
		{name: "non-hex-digest", root: root, parts: []string{"pin.json"}, digest: strings.Repeat("g", sha256.Size*2), wantHex: true},
		{name: "empty-component", root: root, parts: []string{""}, digest: digest, component: ""},
		{name: "dot-component", root: root, parts: []string{"."}, digest: digest, component: "."},
		{name: "dot-dot-component", root: root, parts: []string{".."}, digest: digest, component: ".."},
		{name: "absolute-component", root: root, parts: []string{filepath.Join(root, "absolute")}, digest: digest, component: filepath.Join(root, "absolute")},
		{name: "slash-component", root: root, parts: []string{"dir/file"}, digest: digest, component: "dir/file"},
		{name: "backslash-component", root: root, parts: []string{"dir\\file"}, digest: digest, component: "dir\\file"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateStateReadBeneathRootInput(test.root, test.parts, test.digest)
			if err == nil {
				t.Fatal("validation unexpectedly succeeded")
			}
			requireStateReadCategory(t, err, StateFileReadErrorInvalidInput)
			if err.Operation != "validate" || err.Component != test.component {
				t.Fatalf("error=%+v, want validate component %q", err, test.component)
			}
			if test.wantHex {
				var decodeErr hex.InvalidByteError
				if !errors.As(err, &decodeErr) {
					t.Fatalf("non-hex input lost the original decode error: %v", err)
				}
			}
		})
	}
}

func TestStateReadDynamicLimitIsRemainingPlusOne(t *testing.T) {
	tests := []struct {
		name         string
		bufferLength int
		want         int
	}{
		{name: "remaining-zero", bufferLength: maxStateFileBytes, want: 1},
		{name: "remaining-one", bufferLength: maxStateFileBytes - 1, want: 2},
		{name: "remaining-chunk-minus-one", bufferLength: maxStateFileBytes - (stateReadChunkSize - 1), want: stateReadChunkSize},
		{name: "remaining-chunk", bufferLength: maxStateFileBytes - stateReadChunkSize, want: stateReadChunkSize},
		{name: "remaining-cap", bufferLength: 0, want: stateReadChunkSize},
		{name: "over-cap", bufferLength: maxStateFileBytes + 1, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := stateReadRequestLimit(test.bufferLength, stateReadChunkSize); got != test.want {
				t.Fatalf("request=%d, want %d", got, test.want)
			}
		})
	}
	if got := stateReadRequestLimit(0, 0); got != 0 {
		t.Fatalf("zero chunk request=%d, want 0", got)
	}
}

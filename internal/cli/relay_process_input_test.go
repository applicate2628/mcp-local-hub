package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestBindProcessRelayInputScopesGrant(t *testing.T) {
	root := &cobra.Command{Use: "owned-test"}
	original := strings.NewReader("borrowed-root-input")
	root.SetIn(original)
	relay, other := newRelayCmdReal(), &cobra.Command{Use: "other"}
	root.AddCommand(relay, other)
	path := filepath.Join(t.TempDir(), "owned-input.txt")
	if err := os.WriteFile(path, []byte("owned-relay-input"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := BindProcessRelayInput(root, input); err != nil {
		t.Fatal(err)
	}
	if root.InOrStdin() != original || other.InOrStdin() != original {
		t.Fatal("relay grant changed unrelated input owners")
	}
	if offset, err := input.Seek(0, io.SeekCurrent); err != nil || offset != 0 {
		t.Fatalf("binding consumed/closed input: offset=%d err=%v", offset, err)
	}
	got, err := io.ReadAll(relay.InOrStdin())
	if err != nil || string(got) != "owned-relay-input" {
		t.Fatalf("relay did not receive the explicit input: %q, %v", got, err)
	}
	// An embedding caller can replace the grant with an ordinary borrowed
	// reader before execution; no old callback is stored elsewhere in root.
	replacement := strings.NewReader("replacement")
	relay.SetIn(replacement)
	if relay.InOrStdin() != replacement {
		t.Fatal("input override retained old process grant")
	}
}

func TestBindProcessRelayInputRejectsInvalidComposition(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "owned")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, tc := range []struct {
		name  string
		root  *cobra.Command
		input *os.File
	}{
		{"nil_root", nil, file},
		{"nil_input", &cobra.Command{Use: "test"}, nil},
		{"no_relay", &cobra.Command{Use: "test"}, file},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := BindProcessRelayInput(tc.root, tc.input); err == nil {
				t.Fatal("invalid process composition accepted")
			}
		})
	}
}

func TestBindProcessRelayInputDoesNotProbeBeforeCommand(t *testing.T) {
	root := &cobra.Command{Use: "owned-test"}
	root.AddCommand(newRelayCmdReal())
	file, err := os.CreateTemp(t.TempDir(), "closed-input")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := BindProcessRelayInput(root, file); err != nil {
		t.Fatalf("binding probed native input even though relay has not run: %v", err)
	}
	// --help, --version and unrelated commands must remain independent of
	// whether the process inherited a usable protocol input descriptor.
}

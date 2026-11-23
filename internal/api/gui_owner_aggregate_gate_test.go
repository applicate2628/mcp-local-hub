package api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSavedHubEndpointGateBorrow(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		active, invalid bool
	}{
		{"default_off", "appearance.theme: dark\n", false, false},
		{"saved_on", "gui_server.hub_endpoint_enabled: true\n", true, false},
		{"malformed_value", "gui_server.hub_endpoint_enabled: yes\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(hardenedTempDir(t), "gui-preferences.yaml")
			if err := WriteStateFileBytesAtomic(path, []byte(tc.body)); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			called := false
			callbackCause := errors.New("read-only gate callback")
			err = NewAPI().WithSavedHubEndpointGateIn(t.Context(), path, func(active bool) error {
				called = true
				if active != tc.active {
					t.Fatalf("observed saved gate %t, want %t", active, tc.active)
				}
				return callbackCause
			})
			if called == tc.invalid || err == nil || errors.Is(err, callbackCause) == tc.invalid {
				t.Fatalf("typed gate observation called=%t error=%v", called, err)
			}
			actual, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(actual, []byte(tc.body)) {
				t.Fatalf("gate borrow changed bytes: %s %v", actual, err)
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("gate borrow replaced settings file: %v", err)
			}
		})
	}
}

func TestSavedHubEndpointGateRejectsTrailingDocument(t *testing.T) {
	for _, tc := range []struct{ name, tail string }{
		{"second_document", "gui_server.hub_endpoint_enabled: false\n"},
		{"malformed_tail", "gui_server.hub_endpoint_enabled: [unterminated\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(hardenedTempDir(t), "gui-preferences.yaml")
			expected := []byte("gui_server.hub_endpoint_enabled: true\n---\n" + tc.tail)
			if err := WriteStateFileBytesAtomic(path, expected); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			api := NewAPI()
			value, err := api.SettingsGetIn(path, "gui_server.hub_endpoint_enabled")
			if err != nil || value != "true" {
				t.Fatalf("legacy settings interpretation changed: %q, %v", value, err)
			}
			legacy, err := api.ObserveRequiredGUIOwnerControlContextIn(t.Context(), path)
			if err != nil || legacy.Control == nil || !legacy.LeafPresent {
				t.Fatalf("legacy GUI observation changed: %v", err)
			}
			called := false
			err = api.WithSavedHubEndpointGateIn(t.Context(), path, func(bool) error { called = true; return nil })
			if called || !errors.Is(err, errInvalidSettingsDocument) {
				t.Errorf("protected H gate accepted trailing settings: callback=%t, error=%v", called, err)
			}
			raw, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, expected) {
				t.Fatalf("protected rejection changed settings bytes: %v", err)
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("protected rejection replaced the settings owner: %v", err)
			}
		})
	}
}

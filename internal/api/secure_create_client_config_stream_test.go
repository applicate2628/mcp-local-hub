//go:build windows

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSecureCreateOwnerOnlyFileStreamPublishesAndRevokes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive")
	payload := []byte("first and second part")
	var saved io.Writer
	created, err := SecureCreateOwnerOnlyFileStream(t.Context(), path, func(w io.Writer) error {
		saved = w
		if _, err := w.Write(payload[:5]); err != nil {
			return err
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial archive exposed during creation: %v", err)
		}
		_, err := w.Write(payload[5:])
		return err
	})
	if !created || err != nil {
		t.Fatalf("archive create: %v, %v", created, err)
	}
	if n, err := io.WriteString(saved, "late bytes"); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("retained writer: %d, %v", n, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, payload) {
		t.Fatalf("archive readback differs: %v", err)
	}
	created, err = SecureCreateOwnerOnlyFile(path, []byte("replacement"))
	if created || err != nil {
		t.Fatalf("existing byte API create: %v, %v", created, err)
	}
	created, err = SecureCreateOwnerOnlyFileStream(t.Context(), path, func(io.Writer) error {
		t.Fatal("occupied slot invoked the write callback")
		return nil
	})
	if created || err != nil {
		t.Fatalf("existing stream create: %v, %v", created, err)
	}
	raw, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, payload) {
		t.Fatalf("archive was overwritten: %v", err)
	}
}

func TestSecureCreateOwnerOnlyFileStreamRefusesPartialPublication(t *testing.T) {
	callbackErr := errors.New("fixture callback stopped")
	for _, tc := range []struct {
		name string
		want error
	}{
		{"callback failure", callbackErr},
		{"canceled after writing", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			dir := t.TempDir()
			path := filepath.Join(dir, "archive")
			var saved io.Writer
			created, err := SecureCreateOwnerOnlyFileStream(ctx, path, func(w io.Writer) error {
				saved = w
				if _, err := io.WriteString(w, "partial private bytes"); err != nil {
					return err
				}
				if tc.want == context.Canceled {
					cancel()
					return nil
				}
				return callbackErr
			})
			if created || !errors.Is(err, tc.want) {
				t.Fatalf("failed archive reported publication: %v, %v", created, err)
			}
			if n, err := io.WriteString(saved, "late bytes"); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("retained failed writer: %d, %v", n, err)
			}
			rows, err := os.ReadDir(dir)
			if err != nil || len(rows) != 0 {
				t.Fatalf("partial archive or temp retained: %d, %v", len(rows), err)
			}
		})
	}
}

func TestSecureCreateOwnerOnlyFileStreamRacingDestinationSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive")
	created, err := SecureCreateOwnerOnlyFileStream(t.Context(), path, func(w io.Writer) error {
		if _, err := io.WriteString(w, "proposed archive"); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("race winner"), 0o600)
	})
	if created || err != nil {
		t.Fatalf("occupied destination during publish: %v, %v", created, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "race winner" {
		t.Fatalf("race winner was overwritten: %v", err)
	}
	rows, err := os.ReadDir(dir)
	if err != nil || len(rows) != 1 || rows[0].Name() != "archive" {
		t.Fatalf("race left extra files: %d, %v", len(rows), err)
	}
}

type secureCreateTestShortWriter struct{}

func (secureCreateTestShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestSecureCreateStreamRemembersIgnoredShortWrite(t *testing.T) {
	err := writeSecureCreateBody(t.Context(), secureCreateTestShortWriter{}, func(w io.Writer) error {
		_, _ = io.WriteString(w, "intentionally ignored short write")
		return nil
	})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("callback hid the write failure: %v", err)
	}
}

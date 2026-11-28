//go:build windows

package api

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
)

// This fixture owns a real file writer, lease, reader and namespace. Its
// identifiers grant no installation, MCP or complete publication authority.
func preparedSerenaReadyNativeWriter(t *testing.T, ctx context.Context, size int) (*StateFilePublicationObservation, []byte, string) {
	t.Helper()
	parent := hardenedTempDir(t)
	recordIO, err := NewPreparedSerenaRecordIO(parent, filepath.Join(parent, "fixture-intent.json"), "readback-fixture", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("b", 64)
	namespace, err := recordIO.AcquireRecordNamespaceObserved(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var writer *StateFilePublicationObservation
	t.Cleanup(func() {
		proof, closeErr := namespace.CloseObserved()
		if writer != nil && writer.fresh != nil {
			closeErr = errors.Join(closeErr, writer.ReceiveFreshNamespaceClose(proof))
		}
		if closeErr != nil {
			t.Errorf("close original fixture namespace: %v", closeErr)
		}
	})
	path, err := recordIO.recordPath(key)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := NewStateFileFreshCreationEvidence(ctx, namespace, path, "prepared-ready")
	if err != nil {
		t.Fatal(err)
	}
	const prefix, suffix = "{\"fixture\":\"", "\"}"
	raw := []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
	request := clients.ConfigWriteRequest{Context: ctx, Path: path, Contents: raw, FreshCreationEvidence: fresh}
	writer, err = WriteStateFileBytesAtomicObserved(request)
	if err != nil || writer == nil || writer.readback == nil {
		t.Fatalf("original observed writer unavailable: %v", err)
	}
	if err := writer.readback.Validate(ctx, path, int64(size), raw, preparedSerenaHash(raw)); err != nil {
		t.Fatalf("original reader did not bind its actual write-size cap: %v", err)
	}
	if writer.Complete() || writer.ResourcesClosed() || writer.ReceiptSHA256() != "" {
		t.Fatal("writer admitted its outstanding original namespace")
	}
	emptyOwner := NewPreparedSerenaPublicationOwner()
	if err := emptyOwner.readyWritten(ctx, "readback-fixture", writer, writer.readback, raw); err == nil || err.Error() != "SERENA_PREPARED_PUBLICATION_WRITER_UNSETTLED" {
		t.Fatalf("unknown namespace closure admitted: %v", err)
	}
	closeProof, err := namespace.CloseObserved()
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.ReceiveFreshNamespaceClose(closeProof); err != nil {
		t.Fatal(err)
	}
	if !writer.Complete() || !writer.ResourcesClosed() || !preparedSerenaDigest(writer.ReceiptSHA256()) || !bytes.Equal(writer.readbackRaw, raw) {
		t.Fatal("original writer resources or receipt remain unsettled")
	}
	t.Logf("original writer complete; bytes=%d readback_cap=%d receipt=%s", len(raw), writer.readback.maxBytes, writer.ReceiptSHA256())
	return writer, raw, parent
}

func TestPreparedSerenaReadyPublicationOriginalReadback(t *testing.T) {
	for _, sample := range []struct {
		name string
		size int
		want string
	}{
		{"observed-1700-bytes", 1700, "SERENA_PREPARED_PUBLICATION_PREREQUISITES_MISSING"},
		{"exact-one-mib", 1 << 20, "SERENA_PREPARED_PUBLICATION_PREREQUISITES_MISSING"},
		{"over-one-mib", (1 << 20) + 1, "SERENA_PREPARED_PUBLICATION_WRITER_UNSETTLED"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			writer, raw, _ := preparedSerenaReadyNativeWriter(t, ctx, sample.size)
			// Reaching the NEXT guard proves original writer/readback receiving.
			// The owner has no fabricated installation/qualification entries.
			owner := NewPreparedSerenaPublicationOwner()
			err := owner.readyWritten(ctx, "readback-fixture", writer, writer.readback, raw)
			if err == nil || err.Error() != sample.want || len(owner.operations) != 0 {
				t.Fatalf("ready receiving error=%v, want %s with empty owner", err, sample.want)
			}
		})
	}
}

func TestPreparedSerenaReadyPublicationReadbackRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	writer, raw, parent := preparedSerenaReadyNativeWriter(t, ctx, 1700)
	wideRaw, wideReader, err := ReadStateFileInodeAnchoredObserved(ctx, writer.path, 1<<20)
	if err != nil || wideReader == nil || !bytes.Equal(wideRaw, raw) {
		t.Fatal("independent wider-cap reader unavailable", err)
	}
	otherPath := filepath.Join(parent, "different-path.json")
	if err = WriteStateFileBytesAtomic(otherPath, raw); err != nil {
		t.Fatal(err)
	}
	otherRaw, otherReader, err := ReadStateFileInodeAnchoredObserved(ctx, otherPath, int64(len(raw)))
	if err != nil || otherReader == nil || !bytes.Equal(otherRaw, raw) {
		t.Fatal("independent other-path reader unavailable", err)
	}
	changed := append([]byte(nil), raw...)
	changed[0] = '['
	renewed, cancelRenewed := context.WithTimeout(context.Background(), time.Minute)
	defer cancelRenewed()
	for _, sample := range []struct {
		name   string
		ctx    context.Context
		writer *StateFilePublicationObservation
		reader *StateFileReadObservation
		raw    []byte
	}{
		{"different-read-budget", ctx, writer, wideReader, raw},
		{"different-read-path", ctx, writer, otherReader, raw},
		{"different-data-and-digest", ctx, writer, writer.readback, changed},
		{"missing-reader", ctx, writer, nil, raw},
		{"unknown-writer", ctx, nil, writer.readback, raw},
		{"empty-data", ctx, writer, writer.readback, nil},
		{"renewed-receiving-deadline", renewed, writer, writer.readback, raw},
	} {
		t.Run(sample.name, func(t *testing.T) {
			owner := NewPreparedSerenaPublicationOwner()
			err := owner.readyWritten(sample.ctx, "readback-fixture", sample.writer, sample.reader, sample.raw)
			if err == nil || err.Error() != "SERENA_PREPARED_PUBLICATION_WRITER_UNSETTLED" || len(owner.operations) != 0 {
				t.Fatalf("foreign or incomplete readback admitted: %v", err)
			}
		})
	}
}

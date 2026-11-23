//go:build windows

package api

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestRoutePublicationReadbackRetainsOriginalRelaxAudit(t *testing.T) {
	for _, closed := range []bool{true, false} {
		name := "unclosed original audit"
		if closed {
			name = "settled original audit"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(`C:\`, "fixture", "report.json")
			data := []byte("synthetic journal")
			req := clients.ConfigWriteRequest{Context: ctx, Path: path, Contents: data}
			audit := &HubMcpLogObservation{finalized: true, view: HubMcpLogSnapshotV1{FileAcquired: true},
				fileCloseAttempted: true, fileCloseReturned: closed}
			audit.original = audit
			primary := errors.New("fixture original readback failed")
			builder := newStateFileReadBuilder(ctx, path, int64(len(data)))
			builder.audits = append(builder.audits, audit)
			_, original, readErr := builder.finish(nil, primary)
			if original.ResourcesFinalized() != closed {
				t.Fatal("original audit disposition did not reach read finalization")
			}

			publication := &StateFilePublicationObservation{sha256: preparedSerenaHash(data)}
			var calls int
			err := receiveStateFilePublicationReadback(publication, req, func(got context.Context, gotPath string, cap int64,
				capture func(uintptr, []byte) error) ([]byte, *StateFileReadObservation, error) {
				calls++
				if got != ctx || gotPath != path || cap != int64(len(data)) || capture != nil {
					t.Fatal("private replacement selected a different readback")
				}
				return nil, original, readErr
			})
			if calls != 1 || !errors.Is(err, primary) || publication.readback != original ||
				!publication.readbackAttempted || publication.readbackQualified ||
				len(publication.readback.audits) != 1 || publication.readback.audits[0] != audit ||
				publication.readback.ResourcesFinalized() != closed {
				t.Fatalf("publication replaced the original nested readback or audit receipt: %v", err)
			}
		})
	}
}

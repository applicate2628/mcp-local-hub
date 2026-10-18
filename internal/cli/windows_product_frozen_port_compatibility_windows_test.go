//go:build windows

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// Exercise the real V3 codec with the same journal shape as the existing
// productRecoveryV3Fixture, without invoking product or recovery operations.
func TestWindowsProductFrozenPortCompatibility(t *testing.T) {
	dir := t.TempDir()
	digest := strings.Repeat("a", sha256.Size*2)
	base := windowsProductRecoveryJournalV3{
		Schema: WindowsProductRecoverySchemaV3, TransactionID: strings.Repeat("a", 32),
		Phase: WindowsProductRecoveryPrepared, Mode: WindowsProductPairModeUpgrade,
		Product: windowsProductRecoveryArtifactV3{
			Target: filepath.Join(dir, "mcphub.exe"), NewSHA256: digest,
			BaselineAsides: []windowsProductRecoveryAsideV3{},
		},
		LegacyWindowless: windowsProductRecoveryLegacyWindowlessV3{
			Target: filepath.Join(dir, "mcphub-windowless.exe"), BaselineAsides: []windowsProductRecoveryAsideV3{},
		},
		Receipts: windowsProductRecoveryReceiptsV3{
			V3: windowsProductPairRecoveryReceipt{Path: filepath.Join(dir, UpgradeReceiptSchemaV3+".json")},
			V2: windowsProductPairRecoveryReceipt{Path: filepath.Join(dir, UpgradeReceiptSchemaV2+".json")},
		},
		Tasks:  WindowsProductPairTaskRecovery{StoreRoot: filepath.Join(dir, "task-store"), Backups: []WindowsProductPairTaskBackup{}},
		Staged: windowsProductRecoveryStagedV3{Path: filepath.Join(dir, "mcphub.exe.stage"), SHA256: digest},
		PreSettlement: &windowsProductPreSettlementV3{
			Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorKnownAbsent},
			Ports:      []windowsProductPortSettlementV1{}, GUIOwner: windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true,
		},
	}
	row := api.SupervisorFrozenPort{TaskName: "synthetic-task", Port: 9137, ArgvSHA256: digest}
	for _, tc := range []struct {
		name    string
		mode    WindowsProductPairMode
		rows    []api.SupervisorFrozenPort
		wantErr string
	}{
		{name: "upgrade-null", mode: WindowsProductPairModeUpgrade, wantErr: "upgrade V3 recovery requires a non-null frozen port set"},
		{name: "upgrade-empty", mode: WindowsProductPairModeUpgrade, rows: []api.SupervisorFrozenPort{}},
		{name: "upgrade-rows", mode: WindowsProductPairModeUpgrade, rows: []api.SupervisorFrozenPort{row}},
		{name: "canonicalize-null", mode: WindowsProductPairModeCanonicalize},
		{name: "setup-empty", mode: WindowsProductPairModeSetup, rows: []api.SupervisorFrozenPort{}},
		{name: "invalid-row", mode: WindowsProductPairModeUpgrade, rows: []api.SupervisorFrozenPort{{TaskName: row.TaskName, Port: 0, ArgvSHA256: digest}}, wantErr: "invalid V3 frozen port"},
		{name: "duplicate-row", mode: WindowsProductPairModeUpgrade, rows: []api.SupervisorFrozenPort{row, row}, wantErr: "unsorted or duplicate V3 frozen port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := base
			journal.Mode, journal.FrozenPorts = tc.mode, tc.rows
			if tc.mode != WindowsProductPairModeUpgrade {
				journal.PreSettlement = nil
			} else if tc.name == "upgrade-rows" {
				pre := *base.PreSettlement
				pre.Ports = []windowsProductPortSettlementV1{{TaskName: row.TaskName, Port: row.Port, PreState: windowsProductPortUnbound, PostUnbound: true}}
				journal.PreSettlement = &pre
			}
			var captured []byte
			writeCalls, readCalls := 0, 0
			write := func(_ string, data []byte) error {
				writeCalls++
				captured = append([]byte(nil), data...)
				return nil
			}
			read := func(string) ([]byte, error) {
				readCalls++
				return append([]byte(nil), captured...), nil
			}
			journalPath := filepath.Join(dir, WindowsProductRecoveryFileLeafV3)
			err := persistWindowsProductRecoveryJournalV3(journalPath, journal, write, read)
			raw := append([]byte(nil), captured...)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr || raw != nil || writeCalls != 0 || readCalls != 0 {
					t.Fatalf("encode raw=%q err=%v, want %q", raw, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeWindowsProductRecoveryJournalV3(raw)
			if err != nil {
				t.Fatal(err)
			}
			if (decoded.FrozenPorts == nil) != (tc.rows == nil) {
				t.Fatal("nil/empty port presence changed")
			}
			err = persistWindowsProductRecoveryJournalV3(journalPath, decoded, write, read)
			again := captured
			if err != nil || !bytes.Equal(raw, again) {
				t.Fatalf("codec round trip bytes differ: %v", err)
			}
			// Only the test-owned temporary directory varies across baseline/delta
			// runs; emit every normalized byte for external before/after receiving.
			prefix, err := json.Marshal(dir)
			if err != nil {
				t.Fatal(err)
			}
			normalized := bytes.ReplaceAll(raw, prefix[1:len(prefix)-1], []byte("synthetic-root"))
			t.Logf("FROZEN_PORT_PAYLOAD %s %x", tc.name, normalized)
		})
	}
}

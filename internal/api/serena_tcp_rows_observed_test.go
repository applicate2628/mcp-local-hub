package api

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestOriginalTCPRowsKeepsFullPeerAndRejectsDegradation(t *testing.T) {
	header := "Active Connections\n\nProto Local Address Foreign Address State PID\n"
	valid := header + "TCP 127.0.0.1:1234 0.0.0.0:0 LISTENING 7\nTCP 127.0.0.1:1234 127.0.0.1:5678 ESTABLISHED 7\nTCP [::ffff:127.0.0.1]:9999 [::]:0 LISTENING 8\n"
	rows, err := parseOriginalLoopbackTCPRows([]byte(valid))
	if err != nil || len(rows) != 2 || rows[1].RemotePort != 5678 || rows[1].PID != 7 {
		t.Fatal(rows, err)
	}
	for _, bad := range []string{
		"", "garbage", strings.Replace(valid, " 7", " 0", 1),
		header + "TCP 127.0.0.1:1234 127.0.0.1:5678 ESTABLISHED x\n",
		header + "TCP 127.0.0.1:1234 127.0.0.1:5678 UNKNOWN 7\n",
		header + "TCP 127.0.0.1:1234 invalid ESTABLISHED 7\n",
		valid + "TCP 127.0.0.1:1234 0.0.0.0:0 LISTENING 8\n",
	} {
		if _, err := parseOriginalLoopbackTCPRows([]byte(bad)); err == nil {
			t.Fatal("degraded frame admitted")
		}
	}
	if rows, err := parseOriginalLoopbackTCPRows([]byte(header)); err != nil || rows == nil || len(rows) != 0 {
		t.Fatal("completed empty frame lost", err)
	}
}

func TestOriginalTCPRowsRecognizedHeaderFrames(t *testing.T) {
	// Actual Windows netstat -ano OEM-866 preamble, without network rows.
	// SHA-256: 59b7529b5bfd36321c5b182763b1dc4e1fa07091b84751e119601d9de6f05f9d.
	// This fixture is independent of the parser's recognized header constants.
	captured, err := hex.DecodeString("0d0a80aae2a8a2adeba520afaea4aaabeee7a5ada8ef0d0a0d0a202088acef202020208baeaaa0abecadeba920a0a4e0a5e1202020202020202082ada5e8ada8a920a0a4e0a5e12020202020202020202091aee1e2aeefada8a5202020202020205049440d0a")
	if err != nil {
		t.Fatal(err)
	}
	if preparedSerenaHash(captured) != "59b7529b5bfd36321c5b182763b1dc4e1fa07091b84751e119601d9de6f05f9d" {
		t.Fatal("captured OEM header fixture changed")
	}
	oemHeader := string(captured)
	lines := strings.Split(oemHeader, "\r\n")
	oemTitle, oemColumns := lines[1], lines[3]
	const englishTitle = "Active Connections"
	const englishColumns = "Proto Local Address Foreign Address State PID"
	const data = "TCP 127.0.0.1:1234 0.0.0.0:0 LISTENING 7\r\n" +
		"TCP 127.0.0.1:1234 127.0.0.1:5678 ESTABLISHED 7\r\n" +
		"TCP 0.0.0.0:3456 0.0.0.0:0 LISTENING 8\r\n" +
		"UDP 127.0.0.1:6789 *:* 9\r\n"
	wantRows := []OriginalLoopbackTCPRowV1{
		{"LISTENING", "127.0.0.1", 1234, "0.0.0.0", 0, 7},
		{"ESTABLISHED", "127.0.0.1", 1234, "127.0.0.1", 5678, 7},
		{"LISTENING", "0.0.0.0", 3456, "0.0.0.0", 0, 8},
	}
	for _, tc := range []struct {
		name, frame string
		empty       bool
	}{
		{"english-title", englishTitle + "\n" + englishColumns + "\n" + data, false},
		{"english-header-only", englishColumns + "\n" + data, false},
		{"russian-oem-title", oemHeader + data, false},
		{"russian-oem-header-only", oemColumns + "\r\n" + data, false},
		{"english-empty", englishTitle + "\n" + englishColumns + "\n", true},
		{"english-empty-header-only", englishColumns + "\n", true},
		{"russian-oem-empty", oemHeader, true},
		{"russian-oem-empty-header-only", oemColumns + "\r\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := parseOriginalLoopbackTCPRows([]byte(tc.frame))
			if err != nil || rows == nil {
				t.Fatalf("recognized complete frame: rows=%v err=%v", rows, err)
			}
			want := wantRows
			if tc.empty {
				want = nil
			}
			if len(rows) != len(want) {
				t.Fatalf("row count=%d, want %d", len(rows), len(want))
			}
			for i := range want {
				if rows[i] != want[i] {
					t.Fatalf("row[%d]=%+v, want %+v", i, rows[i], want[i])
				}
			}
		})
	}

	for _, tc := range []struct{ name, frame string }{
		{"missing-header", oemTitle + "\r\n" + data},
		{"title-only", oemTitle + "\r\n"},
		{"unknown-preamble", "unknown preamble\n" + oemHeader + data},
		{"mixed-english-title", englishTitle + "\n" + oemColumns + "\n" + data},
		{"mixed-oem-title", oemTitle + "\n" + englishColumns + "\n" + data},
		{"duplicate-english-title", englishTitle + "\n" + englishTitle + "\n" + englishColumns + "\n" + data},
		{"duplicate-oem-title", oemTitle + "\n" + oemHeader + data},
		{"duplicate-english-header", englishColumns + "\n" + englishColumns + "\n" + data},
		{"duplicate-oem-header", oemHeader + oemColumns + "\n" + data},
		{"missing-pid-column", strings.Replace(oemHeader, "PID", "", 1) + data},
		{"extra-column", strings.Replace(oemHeader, "PID", "PID EXTRA", 1) + data},
		{"malformed-columns", strings.Replace(oemHeader, "PID", "OWNER", 1) + data},
		{"midstream-english-header", englishColumns + "\n" + data + englishColumns + "\n"},
		{"midstream-oem-header", oemHeader + data + oemColumns + "\n"},
		{"midstream-english-title", englishColumns + "\n" + data + englishTitle + "\n"},
		{"midstream-oem-title", oemHeader + data + oemTitle + "\n"},
		{"unknown-trailing-line", oemHeader + data + "unknown trailing line\n"},
		{"invalid-row-pid", oemHeader + "TCP 127.0.0.1:1234 0.0.0.0:0 LISTENING x\n"},
		{"extra-row-field", oemHeader + "TCP 127.0.0.1:1234 0.0.0.0:0 LISTENING 7 EXTRA\n"},
		{"damaged-oem-bytes", strings.ToValidUTF8(oemHeader, "\uFFFD") + data},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rows, err := parseOriginalLoopbackTCPRows([]byte(tc.frame)); err == nil || rows != nil {
				t.Fatalf("malformed frame admitted: rows=%v err=%v", rows, err)
			}
		})
	}
}

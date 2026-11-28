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

// These independent numeric-address fixtures establish parser semantics.
// They do not reconstruct the missing frame from the failed Serena run.
func TestOriginalTCPEndpointScopedIPv6Grammar(t *testing.T) {
	for _, tc := range []struct {
		name, input, host string
		port              uint16
	}{
		{"ipv4-control", "127.0.0.1:1234", "127.0.0.1", 1234},
		{"scope-zero", "[fe80::1%0]:0", "fe80::1%0", 0},
		{"scope-one-expanded", "[FE80:0:0:0:0:0:0:1%1]:65535", "FE80:0:0:0:0:0:0:1%1", 65535},
		{"scope-max", "[fe80::1%4294967295]:1234", "fe80::1%4294967295", 1234},
		{"scope-leading-zero", "[fe80::1%0000000001]:1234", "fe80::1%0000000001", 1234},
		{"ipv6-loopback-zone", "[::1%1]:1234", "::1%1", 1234},
		{"mapped-dotted", "[::ffff:127.0.0.1]:1234", "::ffff:127.0.0.1", 1234},
		{"mapped-hex", "[::ffff:7f00:1]:1234", "::ffff:7f00:1", 1234},
		{"mapped-dotted-zone", "[::ffff:127.0.0.1%1]:1234", "::ffff:127.0.0.1%1", 1234},
		{"mapped-hex-zone", "[::ffff:7f00:1%1]:1234", "::ffff:7f00:1%1", 1234},
		{"unspecified-expanded", "[0:0:0:0:0:0:0:0]:1234", "::", 1234},
		{"unspecified-zone-zero", "[::%0]:1234", "::", 1234},
		{"unspecified-expanded-zone-max", "[0:0:0:0:0:0:0:0%4294967295]:1234", "::", 1234},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port, err := parseOriginalTCPEndpoint(tc.input)
			if err != nil || host != tc.host || port != tc.port {
				t.Fatalf("endpoint=%q: host=%q port=%d err=%v; want host=%q port=%d", tc.input, host, port, err, tc.host, tc.port)
			}
		})
	}
	const addressError = "ORIGINAL_TCP_ROW_ADDRESS_INVALID"
	const portError = "ORIGINAL_TCP_ROW_PORT_INVALID"
	for _, tc := range []struct{ name, input, category string }{
		{"empty-zone", "[fe80::1%]:1234", addressError},
		{"repeated-zone", "[fe80::1%1%2]:1234", addressError},
		{"plus-zone", "[fe80::1%+1]:1234", addressError},
		{"minus-zone", "[fe80::1%-1]:1234", addressError},
		{"named-zone", "[fe80::1%eth0]:1234", addressError},
		{"unicode-zone", "[fe80::1%\u0661]:1234", addressError},
		{"overlong-zero-zone", "[fe80::1%00000000000]:1234", addressError},
		{"overflow-zone", "[fe80::1%4294967296]:1234", addressError},
		{"ipv4-zone", "127.0.0.1%1:1234", addressError},
		{"invalid-ipv6", "[fe80:::1%1]:1234", addressError},
		{"missing-bracket", "[fe80::1%1:1234", addressError},
		{"unbracketed-ipv6", "fe80::1%1:1234", addressError},
		{"double-bracket", "[[fe80::1%1]]:1234", addressError},
		{"port-overflow", "[fe80::1%1]:65536", portError},
		{"port-negative", "[fe80::1%1]:-1", portError},
		{"port-text", "[fe80::1%1]:bad", portError},
		{"port-empty", "[fe80::1%1]:", portError},
		{"port-underscore", "[fe80::1%1]:1_2", portError},
		{"zone-error-precedes-port-error", "[fe80::1%eth0]:bad", addressError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port, err := parseOriginalTCPEndpoint(tc.input)
			if err == nil || err.Error() != tc.category || host != "" || port != 0 {
				t.Fatalf("invalid endpoint=%q: host=%q port=%d err=%v; want %s", tc.input, host, port, err, tc.category)
			}
		})
	}
}

func TestOriginalTCPRowsScopedIPv6OwnershipBoundary(t *testing.T) {
	const prefix = "Active Connections\nProto Local Address Foreign Address State PID\n" +
		"TCP 127.0.0.1:1234 0.0.0.0:0 LISTENING 7\n"
	for _, tc := range []struct {
		name, line string
		extra      *OriginalLoopbackTCPRowV1
	}{
		{"local-zone", "TCP [fe80::1%0]:1234 [fe80::2]:5678 ESTABLISHED 8", nil},
		{"remote-zone", "TCP [fe80::1]:1234 [fe80::2%4294967295]:5678 ESTABLISHED 8", nil},
		{"both-zones", "TCP [fe80::1%1]:1234 [fe80::2%1]:5678 ESTABLISHED 8", nil},
		{"ipv6-loopback-zone", "TCP [::1%1]:1234 [::1%1]:5678 ESTABLISHED 8", nil},
		{"mapped-dotted-zone", "TCP [::ffff:127.0.0.1%1]:1234 [::]:0 LISTENING 8", nil},
		{"mapped-hex-zone", "TCP [::ffff:7f00:1%1]:1234 [::]:0 LISTENING 8", nil},
		{"mapped-zero-zone", "TCP [::ffff:0.0.0.0%1]:1234 [::]:0 LISTENING 8", nil},
		{"expanded-wildcard", "TCP [0:0:0:0:0:0:0:0]:1234 [::]:0 LISTENING 8",
			&OriginalLoopbackTCPRowV1{"LISTENING", "::", 1234, "::", 0, 8}},
		{"scoped-wildcard", "TCP [::%0]:1234 [::%4294967295]:0 LISTENING 8",
			&OriginalLoopbackTCPRowV1{"LISTENING", "::", 1234, "::", 0, 8}},
		{"expanded-scoped-wildcard", "TCP [0:0:0:0:0:0:0:0%1]:1234 [::]:0 LISTENING 8",
			&OriginalLoopbackTCPRowV1{"LISTENING", "::", 1234, "::", 0, 8}},
		// Parsing retains the full peer; it does not supply native ownership.
		{"scoped-peer-on-ipv4-row", "TCP 127.0.0.1:1234 [FE80:0:0:0:0:0:0:2%1]:5678 ESTABLISHED 7",
			&OriginalLoopbackTCPRowV1{"ESTABLISHED", "127.0.0.1", 1234, "FE80:0:0:0:0:0:0:2%1", 5678, 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := parseOriginalLoopbackTCPRows([]byte(prefix + tc.line + "\n"))
			want := []OriginalLoopbackTCPRowV1{{"LISTENING", "127.0.0.1", 1234, "0.0.0.0", 0, 7}}
			if tc.extra != nil {
				want = append(want, *tc.extra)
			}
			if err != nil || rows == nil || len(rows) != len(want) {
				t.Fatalf("rows=%+v err=%v; want %+v", rows, err, want)
			}
			for i := range want {
				if rows[i] != want[i] {
					t.Fatalf("row[%d]=%+v, want %+v", i, rows[i], want[i])
				}
			}
		})
	}
}

func TestOriginalTCPRowsScopedIPv6RejectsDegradation(t *testing.T) {
	const prefix = "Active Connections\nProto Local Address Foreign Address State PID\n" +
		"TCP 127.0.0.1:1234 0.0.0.0:0 LISTENING 7\n"
	const scopedWildcard = "TCP [::%1]:1234 [::%1]:0 LISTENING 8\n"
	for _, tc := range []struct{ name, lines, category string }{
		{"local-zone-before-ignored-state", "TCP [fe80::1%]:1234 [fe80::2]:5678 TIME_WAIT 8\n", "ORIGINAL_TCP_ROW_ADDRESS_INVALID"},
		{"remote-zone-before-ignored-state", "TCP [fe80::1]:1234 [fe80::2%]:5678 TIME_WAIT 8\n", "ORIGINAL_TCP_ROW_ADDRESS_INVALID"},
		{"invalid-remote-address", "TCP [fe80::1%1]:1234 [fe80:::2%1]:5678 TIME_WAIT 8\n", "ORIGINAL_TCP_ROW_ADDRESS_INVALID"},
		{"port-before-ignored-state", "TCP [fe80::1%1]:65536 [fe80::2%1]:5678 TIME_WAIT 8\n", "ORIGINAL_TCP_ROW_PORT_INVALID"},
		{"unsupported-state", "TCP [fe80::1%1]:1234 [fe80::2%1]:5678 UNKNOWN 8\n", "ORIGINAL_TCP_ROW_STATE_UNSUPPORTED"},
		{"invalid-pid", "TCP [fe80::1%1]:1234 [fe80::2%1]:5678 ESTABLISHED x\n", "ORIGINAL_TCP_ROW_PID_INVALID"},
		{"overflow-pid", "TCP [fe80::1%1]:1234 [fe80::2%1]:5678 ESTABLISHED 4294967296\n", "ORIGINAL_TCP_ROW_PID_INVALID"},
		{"wildcard-zero-pid", "TCP [::%1]:1234 [::]:0 LISTENING 0\n", "ORIGINAL_TCP_ROW_ENDPOINT_INVALID"},
		{"wildcard-zero-port", "TCP [::%1]:0 [::]:0 LISTENING 8\n", "ORIGINAL_TCP_ROW_ENDPOINT_INVALID"},
		{"ipv4-row-zero-peer-port", "TCP 127.0.0.1:1234 [fe80::2%1]:0 ESTABLISHED 7\n", "ORIGINAL_TCP_ROW_ENDPOINT_INVALID"},
		{"duplicate-wildcard", scopedWildcard + scopedWildcard, "ORIGINAL_TCP_ROW_DUPLICATE_OR_CONFLICT"},
		{"conflicting-wildcard-owner", scopedWildcard + "TCP [::%1]:1234 [::%1]:0 LISTENING 9\n", "ORIGINAL_TCP_ROW_DUPLICATE_OR_CONFLICT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := parseOriginalLoopbackTCPRows([]byte(prefix + tc.lines))
			if rows != nil || err == nil || !strings.Contains(err.Error(), tc.category) {
				t.Fatalf("partial or degraded frame admitted: rows=%+v err=%v; want %s", rows, err, tc.category)
			}
		})
	}
}

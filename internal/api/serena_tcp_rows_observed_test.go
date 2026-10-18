package api

import (
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

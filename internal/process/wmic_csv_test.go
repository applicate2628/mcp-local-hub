package process

import (
	"strings"
	"testing"
)

func TestParseWmicProcessCSVRecord_ReorderedHeaderWithCommaFields(t *testing.T) {
	header, ok := ParseWmicCSVHeader(
		"Node,ExecutablePath,CreationDate,CommandLine,Name",
		"Node", "CommandLine", "CreationDate", "ExecutablePath", "Name",
	)
	if !ok {
		t.Fatal("expected reordered WMIC header to be recognized")
	}

	row, ok := ParseWmicProcessCSVRecord(
		`HOST,C:\Users\Doe, Jane\bin\mcphub.exe,20260707090000.000000+000,"mcphub.exe daemon --server S,with-comma",mcphub.exe`,
		header,
	)
	if !ok {
		t.Fatal("expected reordered WMIC row to parse with header-derived positions")
	}
	if row["CommandLine"] != `mcphub.exe daemon --server S,with-comma` {
		t.Fatalf("CommandLine = %q", row["CommandLine"])
	}
	if row["ExecutablePath"] != `C:\Users\Doe, Jane\bin\mcphub.exe` {
		t.Fatalf("ExecutablePath = %q", row["ExecutablePath"])
	}
	if row["CreationDate"] != "20260707090000.000000+000" {
		t.Fatalf("CreationDate = %q", row["CreationDate"])
	}
	if row["Name"] != "mcphub.exe" {
		t.Fatalf("Name = %q", row["Name"])
	}
}

func TestReadWmicCSVRecords_MultilineQuotedCommandLine(t *testing.T) {
	sample := "Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize\n" +
		"HOST,\"bash -c \"\"printf one\nprintf two\"\"\",20260707090000.000000+000,C:\\Tools\\bash.exe,7000,8123,4096\n"

	records, err := ReadWmicCSVRecords(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("ReadWmicCSVRecords: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want header + one data record: %#v", len(records), records)
	}
	if !strings.Contains(records[1], "printf one\nprintf two") {
		t.Fatalf("multiline command line was not preserved in one record: %q", records[1])
	}
}

func TestReadWmicCSVRecords_UnmatchedQuoteDoesNotFoldFollowingRows(t *testing.T) {
	const row2 = `HOST,node.exe C:\srv\one.js,20260707090000.000000+000,C:\Tools\node.exe,7000,8123,4096`
	const row3 = `HOST,node.exe C:\srv\two.js,20260707090100.000000+000,C:\Tools\node.exe,7000,8124,4096`
	sample := "Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize\n" +
		`HOST,"node.exe C:\srv\broken.js,20260707085900.000000+000,C:\Tools\node.exe,7000,8122,4096` + "\n" +
		row2 + "\n" +
		row3 + "\n"

	records, err := ReadWmicCSVRecords(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("ReadWmicCSVRecords: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("records = %d, want header + three data records: %#v", len(records), records)
	}
	if records[2] != row2 {
		t.Fatalf("second normal row folded or changed: got %q want %q", records[2], row2)
	}
	if records[3] != row3 {
		t.Fatalf("third normal row folded or changed: got %q want %q", records[3], row3)
	}
}
